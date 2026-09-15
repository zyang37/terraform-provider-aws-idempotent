// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package idempotency

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/hashicorp/terraform-plugin-go/tfprotov5"

	"github.com/hashicorp/terraform-provider-aws/internal/idempotency/journal"
)

// WrapProviderServer wraps a tfprotov5.ProviderServer so that every
// ApplyResourceChange call carries a CallScope (see scope.go), and so that
// journal entries created by AWS SDK calls made during that call are
// promoted from responded to completed once Terraform has (as best this
// RPC boundary can tell) received the result, or released back to pending
// if it has not.
//
// This is the "wrapper A" component in docs/DEV_PLAN.md section 4.2, and
// it is load-bearing, not an optimization: without it, an entry a
// successful call marks responded (see serializeMiddleware.record) is
// never advanced to completed, and would incorrectly look reclaimable to a
// later, unrelated call that happens to hash to the same key -- e.g.
// tainting and recreating the same resource, or destroying it and creating
// a differently-configured one that happens to serialize identically. See
// TestWrapProviderServer_PromotesOnSuccess and
// TestWrapProviderServer_ReleasesOnCancellation.
//
// Every other RPC (ReadResource, PlanResourceChange, GetProviderSchema,
// ...) passes straight through unmodified via ProviderServer's embedding:
// only AWS calls made from inside a resource's Create/Update/Delete method
// use the journal at all (see Middleware), and those all happen during
// ApplyResourceChange.
func WrapProviderServer(inner tfprotov5.ProviderServer, store *journal.Store) tfprotov5.ProviderServer {
	return &wrappedServer{ProviderServer: inner, store: store}
}

type wrappedServer struct {
	tfprotov5.ProviderServer
	store *journal.Store
}

func (w *wrappedServer) ApplyResourceChange(ctx context.Context, req *tfprotov5.ApplyResourceChangeRequest) (*tfprotov5.ApplyResourceChangeResponse, error) {
	callID, err := newCallID()
	if err != nil {
		// Cannot generate a call ID: proceed without journal integration for
		// this call rather than failing the apply outright over what is, at
		// this point, a source-of-randomness failure serious enough that
		// the provider has bigger problems -- but an apply that otherwise
		// would have succeeded shouldn't be blocked by it.
		return w.ProviderServer.ApplyResourceChange(ctx, req)
	}

	scope := CallScope{CallID: callID, ResourceType: req.TypeName}
	resp, callErr := w.ProviderServer.ApplyResourceChange(WithCallScope(ctx, scope), req)

	// Use a fresh, short-lived, non-cancellable context for the journal
	// bookkeeping below: ctx may already be cancelled (that is exactly the
	// "release" branch's trigger), and a cancelled context would make the
	// database update itself fail immediately, which is the one thing we
	// must not let happen here -- an entry left in "responded" forever
	// (never promoted, never released) is the stale-reuse bug this wrapper
	// exists to prevent. 5s is generous for a local SQLite write.
	bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if ctx.Err() != nil {
		// Terraform cancelled the request (interrupted, or the plugin is
		// being torn down mid-call). We cannot know whether our own
		// ApplyResourceChange handler's response, if any, ever reached
		// Terraform -- treat every entry from this call as undelivered.
		_ = w.store.ReleaseCall(bgCtx, callID) //nolint:errcheck // best-effort; see doc comment on GlobalStore
		return resp, callErr
	}

	// ctx is still live: this RPC is returning normally (with or without
	// an application-level error in resp.Diagnostics -- either way,
	// Terraform is about to receive this response and, on success, persist
	// the resulting state). Promote every entry this call marked
	// responded to completed.
	_ = w.store.PromoteCall(bgCtx, callID) //nolint:errcheck
	return resp, callErr
}

func newCallID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
