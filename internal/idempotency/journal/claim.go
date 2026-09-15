// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package journal

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// maxClaimAttempts bounds the retry loop in ClaimOrCreate against the
// (self-resolving, but in principle unbounded) race where two processes
// insert a new sequence number for the same key at the same instant; see
// the loop's comment below.
const maxClaimAttempts = 8

// ClaimOrCreate returns the entry this process should use for key: either
// an existing pending/responded entry abandoned by a now-dead process (a
// crash-recovery reclaim), or a freshly created one if every existing
// entry for this key is completed, failed, or actively owned by a still
// -running process.
//
// tokenFor is called at most once, with the sequence number of a newly
// created entry, to render its token; it is not called when reclaiming an
// existing entry, whose stored token is reused unchanged (see
// docs/DEV_PLAN.md section 4: the token is a function of key and seq, so a
// reclaim would derive the same value anyway, but reading the stored value
// avoids recomputing it and stays correct even if the derivation ever
// changes).
func (s *Store) ClaimOrCreate(ctx context.Context, key string, meta Meta, tokenFor func(seq int64) string) (Entry, error) {
	var lastErr error
	for attempt := 0; attempt < maxClaimAttempts; attempt++ {
		var e Entry
		var created bool
		err := withBusyRetry(func() error {
			var innerErr error
			e, created, innerErr = s.tryClaim(ctx, key, meta, tokenFor)
			return innerErr
		})
		if err == nil {
			return e, nil
		}
		lastErr = err
		if created && isUniqueConstraintErr(err) {
			time.Sleep(jitter(5 * time.Millisecond << attempt))
			continue // another process inserted the same (key, seq) first; retry and pick the next seq
		}
		return Entry{}, err
	}
	return Entry{}, fmt.Errorf("idempotency: could not claim an entry for key %q after %d attempts (concurrent claimants): %w", key, maxClaimAttempts, lastErr)
}

func (s *Store) tryClaim(ctx context.Context, key string, meta Meta, tokenFor func(seq int64) string) (_ Entry, attemptedInsert bool, _ error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Entry{}, false, fmt.Errorf("idempotency: begin claim: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	rows, err := tx.QueryContext(ctx,
		`SELECT seq, token, state, claimed_by FROM entries
		 WHERE key = ? AND state IN ('pending','responded')
		 ORDER BY seq ASC`, key)
	if err != nil {
		return Entry{}, false, fmt.Errorf("idempotency: query candidates: %w", err)
	}
	type candidate struct {
		seq       int64
		token     string
		state     State
		claimedBy string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		var state string
		if err := rows.Scan(&c.seq, &c.token, &state, &c.claimedBy); err != nil {
			rows.Close() //nolint:errcheck
			return Entry{}, false, fmt.Errorf("idempotency: scan candidate: %w", err)
		}
		c.state = State(state)
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return Entry{}, false, fmt.Errorf("idempotency: iterate candidates: %w", err)
	}
	if err := rows.Close(); err != nil {
		return Entry{}, false, fmt.Errorf("idempotency: close candidate rows: %w", err)
	}

	for _, c := range candidates {
		if s.live.isLive(c.claimedBy) {
			continue // another process is (or was, until it cleanly finished) actively using this entry
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE entries SET state = 'pending', claimed_by = ?, call_id = '', updated_at = ?
			 WHERE key = ? AND seq = ? AND claimed_by = ?`,
			string(s.live.self), now(), key, c.seq, c.claimedBy)
		if err != nil {
			return Entry{}, false, fmt.Errorf("idempotency: reclaim entry: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return Entry{}, false, fmt.Errorf("idempotency: reclaim rows affected: %w", err)
		}
		if n == 0 {
			// Another process reclaimed it between our SELECT and this
			// UPDATE. Not an error: just move on to the next candidate
			// (or fall through to inserting a new one).
			continue
		}
		if err := tx.Commit(); err != nil {
			return Entry{}, false, fmt.Errorf("idempotency: commit reclaim: %w", err)
		}
		return Entry{
			Key: key, Seq: c.seq, Token: c.token,
			Service: meta.Service, Operation: meta.Operation, ResourceType: meta.ResourceType,
			State: StatePending, ClaimedBy: string(s.live.self),
		}, false, nil
	}

	// Nothing reclaimable: every candidate (if any) is owned by a live
	// process, and every other entry for this key is completed or failed.
	// Mint a new sequence number.
	var maxSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM entries WHERE key = ?`, key).Scan(&maxSeq); err != nil {
		return Entry{}, false, fmt.Errorf("idempotency: read max seq: %w", err)
	}
	seq := int64(0)
	if maxSeq.Valid {
		seq = maxSeq.Int64 + 1
	}
	token := tokenFor(seq)
	ts := now()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO entries (key, seq, token, service, operation, resource_type, state, claimed_by, call_id, summary, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, 'pending', ?, '', '', ?, ?)`,
		key, seq, token, meta.Service, meta.Operation, meta.ResourceType, string(s.live.self), ts, ts); err != nil {
		return Entry{}, true, fmt.Errorf("idempotency: insert entry: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Entry{}, true, fmt.Errorf("idempotency: commit insert: %w", err)
	}
	return Entry{
		Key: key, Seq: seq, Token: token,
		Service: meta.Service, Operation: meta.Operation, ResourceType: meta.ResourceType,
		State: StatePending, ClaimedBy: string(s.live.self),
		CreatedAt: unixNanoToTime(ts), UpdatedAt: unixNanoToTime(ts),
	}, false, nil
}

// MarkResponded transitions a pending entry to responded once AWS has
// returned a result. callID identifies the in-flight ApplyResourceChange
// call (see idempotency.CallScope); summary is a short human-readable
// description of the result (e.g. a resource ID or ARN) used only for
// orphan reports and the `journal ls` CLI.
//
// It returns an error if the entry was not in the pending state -- which
// should not happen in normal operation (this process just claimed it) and
// indicates either a bug or an unexpected concurrent modification, but
// callers should log and continue rather than fail the already-succeeded
// AWS call over a bookkeeping discrepancy.
func (s *Store) MarkResponded(ctx context.Context, key string, seq int64, callID, summary string) error {
	return s.transition(ctx, key, seq, StatePending, StateResponded, callID, summary)
}

// MarkFailed transitions a pending entry to failed after AWS rejected the
// request (see the classification in idempotency's middleware.go). A
// failed entry's token is never reused: replaying a request AWS has
// already told us is invalid would just fail again, and a corrected retry
// should be free to pick a fresh token rather than possibly colliding with
// diagnostic state left over from the rejected attempt.
func (s *Store) MarkFailed(ctx context.Context, key string, seq int64) error {
	return s.transition(ctx, key, seq, StatePending, StateFailed, "", "")
}

func (s *Store) transition(ctx context.Context, key string, seq int64, from, to State, callID, summary string) error {
	return withBusyRetry(func() error {
		res, err := s.db.ExecContext(ctx,
			`UPDATE entries SET state = ?, call_id = ?, summary = ?, updated_at = ?
			 WHERE key = ? AND seq = ? AND state = ?`,
			string(to), callID, summary, now(), key, seq, string(from))
		if err != nil {
			return fmt.Errorf("idempotency: transition %s->%s: %w", from, to, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("idempotency: transition %s->%s rows affected: %w", from, to, err)
		}
		if n == 0 {
			return fmt.Errorf("idempotency: entry %s#%d was not in state %s (already transitioned by a concurrent process, or never claimed)", key, seq, from)
		}
		return nil
	})
}

// PromoteCall transitions every responded entry belonging to callID to
// completed: Terraform received ApplyResourceChange's response for this
// call, meaning (module the caveat in docs/DEV_PLAN.md section 7.2 about
// state persistence happening after this RPC returns) the result was
// delivered. Called by the gRPC wrapper in server.go after a handler
// returns with ctx still live.
func (s *Store) PromoteCall(ctx context.Context, callID string) error {
	return withBusyRetry(func() error {
		_, err := s.db.ExecContext(ctx,
			`UPDATE entries SET state = 'completed', updated_at = ? WHERE call_id = ? AND state = 'responded'`,
			now(), callID)
		if err != nil {
			return fmt.Errorf("idempotency: promote call %s: %w", callID, err)
		}
		return nil
	})
}

// ReleaseCall demotes every responded entry belonging to callID back to
// pending, clearing call_id: the ApplyResourceChange call whose context
// this belonged to was cancelled or the process is shutting down before
// confirming delivery, so the entry must be treated as "might not have
// reached Terraform" and stay reusable on the next apply. Called by the
// gRPC wrapper in server.go when ctx.Err() != nil after a handler returns.
func (s *Store) ReleaseCall(ctx context.Context, callID string) error {
	return withBusyRetry(func() error {
		_, err := s.db.ExecContext(ctx,
			`UPDATE entries SET state = 'pending', call_id = '', updated_at = ? WHERE call_id = ? AND state = 'responded'`,
			now(), callID)
		if err != nil {
			return fmt.Errorf("idempotency: release call %s: %w", callID, err)
		}
		return nil
	})
}

func isUniqueConstraintErr(err error) bool {
	// modernc.org/sqlite wraps the driver error; matching on message
	// substring is what its own tests do too, since it does not export a
	// typed sentinel for SQLITE_CONSTRAINT_PRIMARYKEY.
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "constraint failed: primary key")
}
