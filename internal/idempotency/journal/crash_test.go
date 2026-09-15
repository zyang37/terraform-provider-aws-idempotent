// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package journal

// This file is the closest thing to an end-to-end test the journal package
// can run on its own, without the rest of the provider: it proves the
// crash-recovery property the whole design exists for (see
// docs/DEV_PLAN.md section 8.1's "Crash durability" bullet) using a real
// separate OS process that is genuinely SIGKILLed, not a simulation within
// one process. A same-process test cannot exercise this: liveness.isLive's
// whole mechanism is "does the OS still hold this file lock", which only a
// real process boundary can meaningfully test -- an in-process fake would
// just be testing the mock, not the flock behavior itself.
//
// It works via the standard Go trick of re-executing the test binary
// itself as a subprocess with an env var selecting a different entry
// point (see TestMain): `go test` builds this package's tests into
// os.Args[0], and TestCrashHelperClaim, run via that re-exec with
// GO_TEST_CRASH_HELPER=claim, does nothing but open the journal, claim one
// entry, print its key/seq, and then block forever (so the parent can
// SIGKILL it mid-"request").

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

const crashHelperEnv = "GO_TEST_CRASH_HELPER"

// TestCrashHelperClaim is not a real test: it is the subprocess entry point
// used by TestReclaim_AfterProcessIsSIGKILLed. It is skipped under normal
// `go test` runs and only does anything when re-invoked with
// GO_TEST_CRASH_HELPER=claim set (see runCrashHelper).
func TestCrashHelperClaim(t *testing.T) {
	if os.Getenv(crashHelperEnv) != "claim" {
		t.Skip("only runs as a re-exec'd subprocess; see TestReclaim_AfterProcessIsSIGKILLed")
	}
	path := os.Getenv("GO_TEST_JOURNAL_PATH")
	key := os.Getenv("GO_TEST_JOURNAL_KEY")

	s, err := Open(context.Background(), path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: Open: %v\n", err) //nolint:forbidigo
		os.Exit(1)
	}
	e, err := s.ClaimOrCreate(context.Background(), key, Meta{Service: "EC2", Operation: "RunInstances"}, func(int64) string { return "helper-token" })
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: ClaimOrCreate: %v\n", err) //nolint:forbidigo
		os.Exit(1)
	}
	// Signal the parent (via stdout, which it reads) that the claim
	// happened -- this is the moment standing in for "the SDK request has
	// been sent to AWS, but the response has not come back yet". The
	// parent SIGKILLs us right after seeing this line, deliberately
	// *without* ever calling MarkResponded/PromoteCall/Close, so we die
	// exactly as if the whole `terraform apply` process had been killed
	// mid-request.
	fmt.Printf("CLAIMED seq=%d token=%s\n", e.Seq, e.Token) //nolint:forbidigo
	select {}                                               // block until SIGKILLed
}

func TestReclaim_AfterProcessIsSIGKILLed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGKILL semantics don't apply on Windows; the lease/lock mechanism itself is exercised by the in-process journal tests")
	}

	dir := t.TempDir()
	path := dir + "/journal.db"
	const key = "crash-test-key"

	cmd := exec.Command(os.Args[0], "-test.run", "^TestCrashHelperClaim$", "-test.v")
	cmd.Env = append(os.Environ(),
		crashHelperEnv+"=claim",
		"GO_TEST_JOURNAL_PATH="+path,
		"GO_TEST_JOURNAL_KEY="+key,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start helper: %v", err)
	}

	claimed := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		var acc strings.Builder
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				acc.Write(buf[:n])
				if strings.Contains(acc.String(), "CLAIMED") {
					claimed <- acc.String()
					return
				}
			}
			if err != nil {
				claimed <- acc.String() // helper exited/errored before claiming; surface whatever it printed
				return
			}
		}
	}()

	select {
	case out := <-claimed:
		if !strings.Contains(out, "CLAIMED") {
			t.Fatalf("helper did not report a successful claim before exiting; output: %q", out)
		}
	case <-time.After(10 * time.Second):
		cmd.Process.Kill() //nolint:errcheck
		t.Fatal("timed out waiting for helper subprocess to claim an entry")
	}

	// The helper has claimed the entry and is now blocked in select{},
	// standing in for "request sent, response not yet received". Kill it
	// the way an OOM killer or a user's Ctrl-C-then-kill would: SIGKILL,
	// no cleanup handlers run, Store.Close is never called.
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL helper: %v", err)
	}
	_ = cmd.Wait() // expected to report the kill signal as the exit reason; not itself the assertion

	// Immediately (no sleep, no timeout) reopen the journal as a fresh
	// process would on the very next `terraform apply` and reclaim.
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen after crash: %v", err)
	}
	defer s.Close() //nolint:errcheck

	reclaimed, err := s.ClaimOrCreate(context.Background(), key, Meta{}, func(int64) string {
		t.Fatal("tokenFor should not be called: the crashed helper's entry must be reclaimed, not superseded by a new one")
		return ""
	})
	if err != nil {
		t.Fatalf("ClaimOrCreate after crash: %v", err)
	}
	if reclaimed.Seq != 0 || reclaimed.Token != "helper-token" {
		t.Fatalf("got %+v, want the crashed helper's original seq=0 token=helper-token reused", reclaimed)
	}
	if reclaimed.State != StatePending {
		t.Fatalf("got state %s, want pending", reclaimed.State)
	}
}
