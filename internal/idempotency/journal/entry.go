// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package journal

import "time"

// State is an entry's position in the lifecycle described in
// docs/DEV_PLAN.md section 4.4.
type State string

const (
	// StatePending: a token was claimed and the request may have been sent
	// to AWS, but no result has been recorded as delivered to Terraform.
	// This is the crash-recovery state: a pending entry's token is reused
	// by the next call that hashes to the same key.
	StatePending State = "pending"

	// StateResponded: AWS returned a result, but the call has not yet been
	// confirmed delivered to Terraform (state write not yet observed to
	// complete). See Store.PromoteCall / Store.ReleaseCall.
	StateResponded State = "responded"

	// StateCompleted: the result was delivered to Terraform. The entry's
	// token is retired; a future call with the same key gets the next
	// sequence number.
	StateCompleted State = "completed"

	// StateFailed: AWS rejected the request (a client error such as
	// validation failure). The token is retired the same as StateCompleted
	// -- reusing it would replay a request AWS has already told us is
	// invalid.
	StateFailed State = "failed"
)

// Entry is one row of the journal.
type Entry struct {
	Key          string
	Seq          int64
	Token        string
	Service      string
	Operation    string
	ResourceType string
	State        State
	ClaimedBy    string // this process's lock-file ID; see liveness.go
	CallID       string // set once Responded, used by PromoteCall/ReleaseCall
	Summary      string // human-readable response identifier(s), for orphan reports
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Meta identifies the AWS operation an entry is for. It is informational
// only (used for orphan reports and the `journal ls` CLI); it does not
// participate in the claim key, which is computed by the caller (see
// idempotency.Hash256) and already includes the operation.
type Meta struct {
	Service      string
	Operation    string
	ResourceType string
}
