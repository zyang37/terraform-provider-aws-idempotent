// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package idempotency

import "context"

// ctxKey namespaces this package's context values.
type ctxKey int

const (
	ctxKeyAuto ctxKey = iota
	ctxKeyCall
)

// markAuto records, on ctx, that the current operation's token field was
// empty or held the AutoToken sentinel (see initializeMiddleware) and
// should be resolved deterministically by serializeMiddleware. A
// caller-supplied real token leaves ctx unmarked, so serializeMiddleware
// passes the request through untouched.
func markAuto(ctx context.Context, spec opSpec) context.Context {
	return context.WithValue(ctx, ctxKeyAuto, spec)
}

func autoSpec(ctx context.Context) (opSpec, bool) {
	spec, ok := ctx.Value(ctxKeyAuto).(opSpec)
	return spec, ok
}

// CallScope identifies one Terraform ApplyResourceChange RPC, so that every
// journal entry a resource's Create/Update/Delete handler causes during
// that one RPC can be promoted (delivered) or released (not confirmed
// delivered) together when the RPC returns. See server.go.
type CallScope struct {
	// CallID is a value unique to this RPC invocation, used as
	// journal.Entry.CallID.
	CallID string

	// ResourceType is the Terraform resource type being applied (e.g.
	// "aws_instance"), recorded on new journal entries for
	// human-readable `journal ls` / orphan-report output. Best-effort:
	// left empty if the caller cannot determine it.
	ResourceType string
}

// WithCallScope attaches scope to ctx. The gRPC wrapper in server.go calls
// this once per ApplyResourceChange invocation, before delegating to the
// wrapped provider server; every AWS SDK call made while handling that
// request inherits it through the request's context.Context, which
// Terraform's plugin protocol and every SDK call in the codebase already
// thread through unbroken (a prerequisite this design relies on: an SDK
// call made with context.Background() instead of the request's context,
// which does happen in a small number of places -- see docs/DEV_PLAN.md's
// limitations -- will not be associated with a call scope, and its journal
// entries will simply never be promoted/released by server.go; they still
// get correct crash-recovery semantics from ClaimOrCreate's liveness
// check, just not the "confirmed delivered" -> completed fast path).
func WithCallScope(ctx context.Context, scope CallScope) context.Context {
	return context.WithValue(ctx, ctxKeyCall, scope)
}

// callScope returns the CallScope attached by WithCallScope, if any.
func callScope(ctx context.Context) (CallScope, bool) {
	scope, ok := ctx.Value(ctxKeyCall).(CallScope)
	return scope, ok
}
