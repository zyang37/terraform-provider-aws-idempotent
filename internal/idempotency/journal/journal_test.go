// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package journal

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func TestOpen_CreatesSchemaAndSalt(t *testing.T) {
	s := open(t)
	salt := s.Salt()
	var zero [32]byte
	if salt == zero {
		t.Fatal("Salt() returned all-zero bytes; expected random salt")
	}
}

func TestOpen_SaltPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.db")

	s1, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	salt1 := s1.Salt()
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close() //nolint:errcheck
	if s2.Salt() != salt1 {
		t.Fatal("salt changed across reopen of the same journal file")
	}
}

func TestClaimOrCreate_NewKeyGetsSeqZero(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	e, err := s.ClaimOrCreate(ctx, "k1", Meta{Service: "EC2", Operation: "RunInstances"}, func(seq int64) string {
		if seq != 0 {
			t.Errorf("tokenFor called with seq=%d, want 0", seq)
		}
		return "token-0"
	})
	if err != nil {
		t.Fatalf("ClaimOrCreate: %v", err)
	}
	if e.Seq != 0 || e.Token != "token-0" || e.State != StatePending {
		t.Fatalf("got %+v", e)
	}
}

func TestClaimOrCreate_SameProcessStillPendingGetsNewSeq(t *testing.T) {
	// A pending entry claimed by this same process and not yet resolved
	// (no MarkResponded/MarkFailed) must NOT be reclaimed by a second
	// ClaimOrCreate for the same key: isLive(self) is always true, so the
	// entry looks "actively in use" and a new sequence number is minted
	// instead. This is what makes `count = 2` with identical arguments
	// (two concurrent, still in-flight calls hashing to the same key, both
	// legitimately wanting a resource of their own) produce two tokens
	// instead of one -- see TestClaimOrCreate_MultipleClaimsForSameKeyGetDistinctSeqs.
	// Only a *dead* process's pending entry (a real crash) is reclaimable.
	s := open(t)
	ctx := context.Background()

	e1, err := s.ClaimOrCreate(ctx, "k1", Meta{}, func(int64) string { return "tok-0" })
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}

	e2, err := s.ClaimOrCreate(ctx, "k1", Meta{}, func(int64) string { return "tok-1" })
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if e2.Seq == e1.Seq {
		t.Fatalf("expected a distinct seq for a second still-pending claim, got e1=%+v e2=%+v", e1, e2)
	}
}

func TestClaimOrCreate_CompletedEntryGetsNewSeq(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	e1, err := s.ClaimOrCreate(ctx, "k1", Meta{}, func(int64) string { return "tok-0" })
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := s.MarkResponded(ctx, e1.Key, e1.Seq, "call-1", "arn:...:i-1"); err != nil {
		t.Fatalf("MarkResponded: %v", err)
	}
	if err := s.PromoteCall(ctx, "call-1"); err != nil {
		t.Fatalf("PromoteCall: %v", err)
	}

	e2, err := s.ClaimOrCreate(ctx, "k1", Meta{}, func(seq int64) string {
		if seq != 1 {
			t.Errorf("tokenFor called with seq=%d, want 1", seq)
		}
		return "tok-1"
	})
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if e2.Seq != 1 {
		t.Fatalf("expected a fresh seq after completion, got %+v", e2)
	}
}

func TestClaimOrCreate_FailedEntryGetsNewSeq(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	e1, err := s.ClaimOrCreate(ctx, "k1", Meta{}, func(int64) string { return "tok-0" })
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := s.MarkFailed(ctx, e1.Key, e1.Seq); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	e2, err := s.ClaimOrCreate(ctx, "k1", Meta{}, func(seq int64) string { return "tok-1" })
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if e2.Seq != 1 {
		t.Fatalf("expected a fresh seq after failure, got %+v", e2)
	}
}

func TestClaimOrCreate_MultipleClaimsForSameKeyGetDistinctSeqs(t *testing.T) {
	// Models `count = N` with identical arguments: N calls hashing to the
	// same key, all still in flight (nothing marked responded/failed yet),
	// must each get a distinct sequence number/token, not pile up on one.
	s := open(t)
	ctx := context.Background()

	seen := map[int64]bool{}
	for i := 0; i < 5; i++ {
		e, err := s.ClaimOrCreate(ctx, "same-key", Meta{}, func(seq int64) string {
			return "tok"
		})
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if seen[e.Seq] {
			t.Fatalf("claim %d reused seq %d", i, e.Seq)
		}
		seen[e.Seq] = true
		// Mark it responded+completed immediately is wrong for this test:
		// we want them all simultaneously "in flight" (pending, live), to
		// prove concurrent live claims fan out rather than collide. Do
		// nothing further.
	}
	if len(seen) != 5 {
		t.Fatalf("got %d distinct seqs, want 5: %v", len(seen), seen)
	}
}

func TestMarkResponded_WrongStateErrors(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	// No entry exists yet for this key/seq at all.
	if err := s.MarkResponded(ctx, "nope", 0, "call", "summary"); err == nil {
		t.Fatal("expected an error transitioning a nonexistent entry")
	}
}

func TestReleaseCall_DemotesRespondedToPending(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	e, err := s.ClaimOrCreate(ctx, "k1", Meta{}, func(int64) string { return "tok" })
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.MarkResponded(ctx, e.Key, e.Seq, "call-1", "summary"); err != nil {
		t.Fatalf("MarkResponded: %v", err)
	}
	if err := s.ReleaseCall(ctx, "call-1"); err != nil {
		t.Fatalf("ReleaseCall: %v", err)
	}

	list, err := s.List(ctx, "k1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].State != StatePending || list[0].CallID != "" {
		t.Fatalf("got %+v, want one pending entry with cleared call_id", list)
	}
}

func TestGC_DeletesOldCompletedAndFailedOnly(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	completed, err := s.ClaimOrCreate(ctx, "completed-key", Meta{}, func(int64) string { return "t1" })
	if err != nil {
		t.Fatal(err)
	}
	must(t, s.MarkResponded(ctx, completed.Key, completed.Seq, "c1", ""))
	must(t, s.PromoteCall(ctx, "c1"))

	failed, err := s.ClaimOrCreate(ctx, "failed-key", Meta{}, func(int64) string { return "t2" })
	if err != nil {
		t.Fatal(err)
	}
	must(t, s.MarkFailed(ctx, failed.Key, failed.Seq))

	pending, err := s.ClaimOrCreate(ctx, "pending-key", Meta{}, func(int64) string { return "t3" })
	if err != nil {
		t.Fatal(err)
	}
	_ = pending

	// Force-age everything by GC'ing with a negative retention (everything
	// updated "before now + 1h" i.e. everything so far).
	n, err := s.GC(ctx, -time.Hour)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if n != 2 {
		t.Fatalf("GC deleted %d rows, want 2 (completed+failed, not pending)", n)
	}

	all, err := s.All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Key != "pending-key" {
		t.Fatalf("got %+v, want only the pending entry to survive GC", all)
	}
}

func TestOrphans_ReportsOldPendingEntries(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	if _, err := s.ClaimOrCreate(ctx, "stuck", Meta{Service: "EC2", Operation: "RunInstances"}, func(int64) string { return "t" }); err != nil {
		t.Fatal(err)
	}

	orphans, err := s.Orphans(ctx, -time.Hour) // "older than negative an hour" == everything
	if err != nil {
		t.Fatalf("Orphans: %v", err)
	}
	if len(orphans) != 1 || orphans[0].Key != "stuck" {
		t.Fatalf("got %+v", orphans)
	}

	none, err := s.Orphans(ctx, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("expected no orphans older than 24h for a just-created entry, got %+v", none)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
