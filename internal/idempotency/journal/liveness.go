// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package journal

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
)

// selfID is the current process's lock-file ID, used as entries.claimed_by.
// It has no meaning beyond identifying "which lock file corresponds to this
// process instance"; it is not a PID (PIDs are reused across reboots and
// would make a stale claimed_by ambiguous).
type selfID string

// liveness answers "is the process that claimed this entry still running?"
// using OS file locks (via github.com/gofrs/flock, which wraps flock(2) on
// Unix and LockFileEx on Windows) as a crash-safe heartbeat:
//
//   - On Open, this process creates its own lock file under lockDir and
//     holds an exclusive lock on it for as long as the process lives. The
//     OS releases that lock automatically when the process exits, by
//     *any* means -- a clean return, os.Exit, or SIGKILL -- because the
//     lock is scoped to the open file descriptor, not to any code this
//     process runs.
//   - To check whether some other selfID's process is still alive, we try
//     to acquire *its* lock file non-blockingly. If we succeed, no process
//     holds it: it is dead (and we immediately release and delete the
//     file). If acquisition would block, a live process holds it.
//
// This gives an immediate, crash-safe liveness answer with no polling and
// no arbitrary timeout: a `terraform apply` re-run one second after a
// SIGKILL correctly reclaims the killed process's pending journal entries,
// which is the entire point of this package (see docs/DEV_PLAN.md section
// 4.4 and the crash matrix in section 8.4).
type liveness struct {
	dir  string
	self selfID
	lock *flock.Flock // held for the lifetime of the process; never Unlock'd except by Store.Close
}

func newLiveness(journalDir string) (*liveness, error) {
	dir := filepath.Join(journalDir, "locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("idempotency: create lock dir: %w", err)
	}

	id, err := randomID()
	if err != nil {
		return nil, err
	}

	l := flock.New(filepath.Join(dir, string(id)+".lock"))
	locked, err := l.TryLock()
	if err != nil {
		return nil, fmt.Errorf("idempotency: lock %s: %w", l.Path(), err)
	}
	if !locked {
		// A freshly-generated random ID should never collide with an
		// existing lock file; if it somehow does, treat it as a hard
		// error rather than silently picking a new ID and risking a
		// (however unlikely) duplicate claimed_by.
		return nil, fmt.Errorf("idempotency: could not acquire freshly created lock file %s", l.Path())
	}

	return &liveness{dir: dir, self: id, lock: l}, nil
}

func randomID() (selfID, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("idempotency: generate process id: %w", err)
	}
	return selfID(hex.EncodeToString(b[:])), nil
}

func (l *liveness) path(id string) string {
	return filepath.Join(l.dir, id+".lock")
}

// isLive reports whether the process identified by id (an entries.claimed_by
// value) is still running. An empty id (never claimed) is never live.
func (l *liveness) isLive(id string) bool {
	if id == "" || id == string(l.self) {
		return id == string(l.self) // we are always live to ourselves
	}
	p := l.path(id)
	if _, err := os.Stat(p); err != nil {
		// No lock file: either it was never created (shouldn't happen for
		// a real claimed_by value) or its owner cleanly removed it on
		// Close. Either way, nothing holds it.
		return false
	}
	other := flock.New(p)
	locked, err := other.TryLock()
	if err != nil {
		// Filesystem error (permissions, I/O): fail safe by assuming the
		// process is still live, so we don't reclaim its entries out from
		// under it based on an inconclusive check.
		return true
	}
	if !locked {
		return true // another process holds the lock: it is running
	}
	// We just proved nobody holds it. Clean up: unlock and remove the
	// file so it doesn't accumulate forever, and so a *future* isLive
	// check for this same id short-circuits on the Stat above rather than
	// re-doing a TryLock.
	_ = other.Unlock()
	_ = os.Remove(p)
	return false
}

// close releases and removes this process's own lock file. Called from
// Store.Close on a clean shutdown; deliberately NOT deferred/finalizer-based,
// so that a crash (which is exactly the scenario this package exists to
// detect) leaves the lock file in place for isLive to find.
func (l *liveness) close() error {
	if err := l.lock.Unlock(); err != nil {
		return err
	}
	return os.Remove(l.path(string(l.self)))
}
