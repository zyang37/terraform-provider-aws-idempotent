// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package journal

// TestConcurrentProcesses_DistinctClaimsNoDuplicates spawns several real OS
// processes (not goroutines: the point is to exercise cross-process SQLite
// contention and the flock-based liveness check under genuine concurrency,
// not just Go's in-process scheduler) that all claim against the *same*
// key at once, each immediately marking its own claim responded+completed
// (a normal, non-crashing run). It asserts every claim got a distinct
// sequence number and token, and none were skipped or duplicated -- the
// property that makes `count = N` with identical arguments produce N
// resources rather than colliding them onto one token.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestCrashHelperClaimAndComplete(t *testing.T) {
	if os.Getenv(crashHelperEnv) != "claim-and-complete" {
		t.Skip("only runs as a re-exec'd subprocess; see TestConcurrentProcesses_DistinctClaimsNoDuplicates")
	}
	path := os.Getenv("GO_TEST_JOURNAL_PATH")
	key := os.Getenv("GO_TEST_JOURNAL_KEY")
	id := os.Getenv("GO_TEST_HELPER_ID")

	ctx := context.Background()
	s, err := Open(ctx, path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper %s: Open: %v\n", id, err) //nolint:forbidigo
		os.Exit(1)
	}
	defer s.Close() //nolint:errcheck

	e, err := s.ClaimOrCreate(ctx, key, Meta{Service: "EC2", Operation: "RunInstances"}, func(int64) string {
		return "token-" + id
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper %s: ClaimOrCreate: %v\n", id, err) //nolint:forbidigo
		os.Exit(1)
	}
	callID := "call-" + id
	if err := s.MarkResponded(ctx, e.Key, e.Seq, callID, "resource-"+id); err != nil {
		fmt.Fprintf(os.Stderr, "helper %s: MarkResponded: %v\n", id, err) //nolint:forbidigo
		os.Exit(1)
	}
	if err := s.PromoteCall(ctx, callID); err != nil {
		fmt.Fprintf(os.Stderr, "helper %s: PromoteCall: %v\n", id, err) //nolint:forbidigo
		os.Exit(1)
	}
	fmt.Printf("RESULT seq=%d token=%s\n", e.Seq, e.Token) //nolint:forbidigo
}

func TestConcurrentProcesses_DistinctClaimsNoDuplicates(t *testing.T) {
	const n = 8
	dir := t.TempDir()
	path := dir + "/journal.db"
	const key = "concurrent-key"

	type result struct {
		id     int
		seq    string
		token  string
		errOut string
		err    error
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run", "^TestCrashHelperClaimAndComplete$", "-test.v")
			cmd.Env = append(os.Environ(),
				crashHelperEnv+"=claim-and-complete",
				"GO_TEST_JOURNAL_PATH="+path,
				"GO_TEST_JOURNAL_KEY="+key,
				"GO_TEST_HELPER_ID="+strconv.Itoa(i),
			)
			out, err := cmd.CombinedOutput()
			r := result{id: i, err: err, errOut: string(out)}
			for _, line := range strings.Split(string(out), "\n") {
				if strings.HasPrefix(line, "RESULT ") {
					fields := strings.Fields(line)
					for _, f := range fields[1:] {
						kv := strings.SplitN(f, "=", 2)
						if len(kv) != 2 {
							continue
						}
						switch kv[0] {
						case "seq":
							r.seq = kv[1]
						case "token":
							r.token = kv[1]
						}
					}
				}
			}
			results[i] = r
		}(i)
	}
	wg.Wait()

	seqs := map[string]int{}
	tokens := map[string]int{}
	for _, r := range results {
		if r.err != nil {
			t.Errorf("helper %d failed: %v; output: %s", r.id, r.err, r.errOut)
			continue
		}
		if r.seq == "" || r.token == "" {
			t.Errorf("helper %d produced no RESULT line; output: %s", r.id, r.errOut)
			continue
		}
		seqs[r.seq]++
		tokens[r.token]++
	}
	for seq, count := range seqs {
		if count != 1 {
			t.Errorf("seq %s claimed by %d helpers, want exactly 1", seq, count)
		}
	}
	for tok, count := range tokens {
		if count != 1 {
			t.Errorf("token %s issued to %d helpers, want exactly 1", tok, count)
		}
	}
	if len(seqs) != n {
		t.Errorf("got %d distinct seqs, want %d", len(seqs), n)
	}
}
