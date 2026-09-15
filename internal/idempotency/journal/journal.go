// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

// Package journal is the local, crash-durable record of which idempotency
// tokens have been issued for which requests, and whether their responses
// have been delivered to Terraform. See docs/DEV_PLAN.md section 4.4 for
// the design, and entry.go for the state machine.
package journal

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver: registers "sqlite"
)

// Store is a journal backed by a SQLite database file. A Store is safe for
// concurrent use by multiple goroutines within one process, and the
// database file is safe for concurrent use by multiple provider processes
// (multiple `terraform apply` runs, or multiple provider aliases): SQLite's
// WAL mode plus a busy-timeout handle cross-process writer contention, and
// liveness.go's flock-based check handles cross-process claim reclaiming.
type Store struct {
	db   *sql.DB
	live *liveness
	salt [32]byte
}

// Open opens (creating if necessary) the journal database at path, along
// with its sibling lock directory. The returned Store must be closed with
// Close when the provider process shuts down cleanly; if the process is
// killed instead, that is the exact scenario this package is designed to
// recover from on the next Open (see liveness.go).
func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("idempotency: create journal dir: %w", err)
	}

	// _pragma sets these on every new connection the pool opens, which
	// matters because database/sql may open more than one connection to
	// the file; foreign_keys and busy_timeout must be per-connection.
	//
	// _txlock=immediate makes every BeginTx acquire SQLite's write lock
	// up front instead of lazily at the first write statement. Without
	// it, two connections (in our case: two separate `terraform apply`
	// processes) that both start a deferred transaction and both later
	// try to upgrade to a write lock at nearly the same moment can get an
	// immediate SQLITE_BUSY on the upgrade rather than being queued and
	// retried by the busy_timeout handler -- busy_timeout governs waiting
	// for a lock, not resolving two transactions racing to acquire one at
	// the same instant. Acquiring it immediately serializes that race
	// through the normal, busy_timeout-respecting lock-wait path instead.
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("idempotency: open journal: %w", err)
	}
	// A single writer connection avoids SQLITE_BUSY entirely for our own
	// in-process concurrency (multiple goroutines handling concurrent SDK
	// calls within one provider process); cross-process contention is
	// still handled by the busy_timeout pragma above.
	db.SetMaxOpenConns(1)

	live, err := newLiveness(filepath.Dir(path))
	if err != nil {
		db.Close()
		return nil, err
	}

	s := &Store{db: db, live: live}
	// migrate and loadOrCreateSalt each do a read-then-maybe-write that is
	// not itself wrapped in one transaction (a fresh file's schema
	// creation and salt generation are naturally racy the first time
	// several processes open the same brand-new journal at once); wrap
	// them in the busy-retry helper rather than relying solely on the
	// busy_timeout pragma, which in practice does not reliably cover a
	// herd of processes all racing to create the same file's schema
	// simultaneously (see withBusyRetry's doc comment).
	if err := withBusyRetry(func() error { return s.migrate(ctx) }); err != nil {
		live.close() //nolint:errcheck // best-effort cleanup on the error path
		db.Close()
		return nil, err
	}
	if err := withBusyRetry(func() error { return s.loadOrCreateSalt(ctx) }); err != nil {
		live.close() //nolint:errcheck
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle and this process's liveness lock
// file. See liveness.close for why this matters: it is what lets a
// *cleanly*-exited process's claims be distinguished, instantly, from a
// crashed one's on the next Open (both eventually make isLive return
// false, but the clean path also deletes the row-independent lock file
// immediately rather than leaving it for the next Open's isLive check to
// clean up).
func (s *Store) Close() error {
	liveErr := s.live.close()
	dbErr := s.db.Close()
	return errors.Join(liveErr, dbErr)
}

const schemaVersion = 1

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("idempotency: read schema version: %w", err)
	}
	if version >= schemaVersion {
		return nil
	}
	if version != 0 {
		return fmt.Errorf("idempotency: journal schema version %d is newer than this build supports (%d); "+
			"upgrade the provider or delete the journal file to start fresh", version, schemaVersion)
	}

	const ddl = `
CREATE TABLE IF NOT EXISTS meta (
	k TEXT PRIMARY KEY,
	v BLOB NOT NULL
);

CREATE TABLE IF NOT EXISTS entries (
	key           TEXT    NOT NULL,
	seq           INTEGER NOT NULL,
	token         TEXT    NOT NULL,
	service       TEXT    NOT NULL,
	operation     TEXT    NOT NULL,
	resource_type TEXT    NOT NULL DEFAULT '',
	state         TEXT    NOT NULL CHECK (state IN ('pending','responded','completed','failed')),
	claimed_by    TEXT    NOT NULL DEFAULT '',
	call_id       TEXT    NOT NULL DEFAULT '',
	summary       TEXT    NOT NULL DEFAULT '',
	created_at    INTEGER NOT NULL,
	updated_at    INTEGER NOT NULL,
	PRIMARY KEY (key, seq)
);

CREATE INDEX IF NOT EXISTS entries_state_idx ON entries (state);
CREATE INDEX IF NOT EXISTS entries_call_id_idx ON entries (call_id) WHERE call_id != '';
`
	if _, err := s.db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("idempotency: create schema: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("idempotency: set schema version: %w", err)
	}
	return nil
}

func (s *Store) loadOrCreateSalt(ctx context.Context) error {
	row := s.db.QueryRowContext(ctx, "SELECT v FROM meta WHERE k = 'workspace_salt'")
	var v []byte
	switch err := row.Scan(&v); {
	case err == nil:
		if len(v) != len(s.salt) {
			return fmt.Errorf("idempotency: stored workspace_salt has %d bytes, want %d", len(v), len(s.salt))
		}
		copy(s.salt[:], v)
		return nil
	case errors.Is(err, sql.ErrNoRows):
		// fall through to create
	default:
		return fmt.Errorf("idempotency: read workspace_salt: %w", err)
	}

	if _, err := rand.Read(s.salt[:]); err != nil {
		return fmt.Errorf("idempotency: generate workspace_salt: %w", err)
	}
	// INSERT OR IGNORE: if two processes race to create the very first
	// journal file's salt, the loser's generated bytes are discarded and
	// it re-reads whatever the winner wrote, so every process ends up
	// agreeing on one salt regardless of who created it.
	if _, err := s.db.ExecContext(ctx, "INSERT OR IGNORE INTO meta (k, v) VALUES ('workspace_salt', ?)", s.salt[:]); err != nil {
		return fmt.Errorf("idempotency: store workspace_salt: %w", err)
	}
	row = s.db.QueryRowContext(ctx, "SELECT v FROM meta WHERE k = 'workspace_salt'")
	if err := row.Scan(&v); err != nil {
		return fmt.Errorf("idempotency: read workspace_salt after insert: %w", err)
	}
	copy(s.salt[:], v)
	return nil
}

// Salt returns this journal's workspace salt: 32 random bytes generated
// once, the first time any process opens this journal file, and persisted
// thereafter. It is mixed into every derived token (see
// idempotency.resolveToken) specifically so that two workspaces holding
// identical configuration against the same AWS account -- and therefore
// computing identical request hashes -- cannot derive each other's tokens
// and silently adopt each other's resources.
func (s *Store) Salt() [32]byte { return s.salt }

func now() int64 { return time.Now().UTC().UnixNano() }

func unixNanoToTime(ns int64) time.Time { return time.Unix(0, ns).UTC() }
