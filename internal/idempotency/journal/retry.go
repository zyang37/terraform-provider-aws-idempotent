// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package journal

import (
	"math/rand/v2"
	"strings"
	"time"
)

// withBusyRetry retries fn while it returns a SQLite "database is locked"
// (SQLITE_BUSY) error, with jittered exponential backoff.
//
// The connection-level `busy_timeout` pragma (set in Open's DSN) is meant
// to make SQLite itself block-and-retry internally before ever returning
// SQLITE_BUSY to the caller, and does handle plain lock contention. It
// does not reliably cover every source of SQLITE_BUSY this package can hit
// in practice, in particular several separate OS processes racing to
// create and migrate the *same brand-new* journal file at once (the exact
// situation the first apply in a workspace, or the multi-process test in
// multiprocess_test.go, creates): the CREATE TABLE / PRAGMA user_version
// sequence in migrate() is not itself one atomic transaction, so two
// processes can each observe "no schema yet" and both attempt to create
// it, and depending on timing that collision can surface as an immediate
// SQLITE_BUSY rather than one busy_timeout would have waited out. Retrying
// the whole operation at this level is simple, correct (every retried
// operation here is either read-only or safely re-runnable: CREATE TABLE
// IF NOT EXISTS, INSERT OR IGNORE, or a full claim/transition attempt that
// starts a fresh transaction), and independent of exactly which SQLite
// build/driver quirk produced the busy error.
func withBusyRetry(fn func() error) error {
	const maxElapsed = 10 * time.Second
	backoff := 5 * time.Millisecond
	const maxBackoff = 250 * time.Millisecond

	deadline := time.Now().Add(maxElapsed)
	for {
		err := fn()
		if err == nil || !isBusyErr(err) {
			return err
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(jitter(backoff))
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func jitter(d time.Duration) time.Duration {
	// Full jitter (AWS architecture blog's recommended strategy): a
	// uniformly random duration between 0 and d, so that several
	// processes backing off from the same collision don't retry in
	// lockstep.
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d))) //nolint:gosec // jitter, not a security use
}

func isBusyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") || strings.Contains(msg, "database is locked")
}
