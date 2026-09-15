// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package journal

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// GC deletes completed and failed entries older than retention. It returns
// the number of rows deleted. Pending and responded entries are never
// deleted by GC regardless of age -- an old pending entry might look
// abandoned, but "old" is not the same as "provably dead"; that
// determination is liveness.isLive's job, exercised by ClaimOrCreate, not
// GC's. An old pending entry is instead surfaced by Orphans, for a human to
// look at.
func (s *Store) GC(ctx context.Context, retention time.Duration) (int64, error) {
	cutoff := time.Now().Add(-retention).UnixNano()
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM entries WHERE state IN ('completed','failed') AND updated_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("idempotency: gc: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("idempotency: gc rows affected: %w", err)
	}
	return n, nil
}

// Orphans returns pending or responded entries last updated more than
// olderThan ago, regardless of whether their claimed_by process is
// currently live. A live-owned entry that is still this old usually means
// a very long-running apply (large resource, heavy retries) and is not
// actually a problem; a dead-owned one usually means exactly the crash
// scenario this package protects against, already handled correctly by
// ClaimOrCreate on the next apply -- but it is also consistent with a
// duplicate resource AWS created and nobody ever cleaned up (e.g. the
// config was edited before the retry, so the hash no longer matches; see
// docs/DEV_PLAN.md section 5's "edit config after crash" row). Orphans
// does not distinguish these cases; it is a prompt for a human to check
// with `journal ls` / the AWS console, not an automated action.
func (s *Store) Orphans(ctx context.Context, olderThan time.Duration) ([]Entry, error) {
	cutoff := time.Now().Add(-olderThan).UnixNano()
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, seq, token, service, operation, resource_type, state, claimed_by, call_id, summary, created_at, updated_at
		 FROM entries
		 WHERE state IN ('pending','responded') AND updated_at < ?
		 ORDER BY updated_at ASC`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("idempotency: query orphans: %w", err)
	}
	defer rows.Close()
	return scanEntries(rows)
}

// List returns every entry for key, ordered by sequence number. Used by
// the `journal ls` CLI.
func (s *Store) List(ctx context.Context, key string) ([]Entry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, seq, token, service, operation, resource_type, state, claimed_by, call_id, summary, created_at, updated_at
		 FROM entries WHERE key = ? ORDER BY seq ASC`, key)
	if err != nil {
		return nil, fmt.Errorf("idempotency: list %s: %w", key, err)
	}
	defer rows.Close()
	return scanEntries(rows)
}

// All returns every entry in the journal, ordered by last update. Used by
// the `journal ls` CLI when no key is given.
func (s *Store) All(ctx context.Context) ([]Entry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, seq, token, service, operation, resource_type, state, claimed_by, call_id, summary, created_at, updated_at
		 FROM entries ORDER BY updated_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("idempotency: list all: %w", err)
	}
	defer rows.Close()
	return scanEntries(rows)
}

func scanEntries(rows *sql.Rows) ([]Entry, error) {
	var out []Entry
	for rows.Next() {
		var e Entry
		var state string
		var created, updated int64
		if err := rows.Scan(&e.Key, &e.Seq, &e.Token, &e.Service, &e.Operation, &e.ResourceType,
			&state, &e.ClaimedBy, &e.CallID, &e.Summary, &created, &updated); err != nil {
			return nil, fmt.Errorf("idempotency: scan entry: %w", err)
		}
		e.State = State(state)
		e.CreatedAt = unixNanoToTime(created)
		e.UpdatedAt = unixNanoToTime(updated)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("idempotency: iterate entries: %w", err)
	}
	return out, nil
}
