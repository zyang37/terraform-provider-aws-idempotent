// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package idempotency

import (
	"context"
	"errors"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov5"

	"github.com/hashicorp/terraform-provider-aws/internal/idempotency/journal"
)

var errNoCallScope = errors.New("no CallScope on context: WrapProviderServer did not attach one")

// fakeInnerServer stands in for the real muxed provider server: it
// implements just enough of tfprotov5.ProviderServer (by embedding a nil
// one and overriding one method) to let WrapProviderServer's
// ApplyResourceChange wrapping be tested without a real Terraform Plugin
// Framework/SDKv2 provider. Its ApplyResourceChange does what an AWS SDK
// call's serializeMiddleware.record would do for a successful call --
// claim an entry and mark it responded -- using the CallScope the wrapper
// attached to ctx, so the test can check whether the wrapper correctly
// promotes or releases it afterward.
type fakeInnerServer struct {
	tfprotov5.ProviderServer // nil: only ApplyResourceChange is exercised in these tests
	store                    *journal.Store
	key                      string
	// afterResponded, if set, runs after MarkResponded succeeds and before
	// ApplyResourceChange returns -- e.g. to cancel the context, modeling
	// an interruption that happens after AWS has already answered but
	// before the RPC finishes returning to Terraform.
	afterResponded func()
}

func (f *fakeInnerServer) ApplyResourceChange(ctx context.Context, req *tfprotov5.ApplyResourceChangeRequest) (*tfprotov5.ApplyResourceChangeResponse, error) {
	scope, ok := callScope(ctx)
	if !ok {
		return nil, errNoCallScope
	}
	entry, err := f.store.ClaimOrCreate(ctx, f.key, journal.Meta{ResourceType: req.TypeName}, func(int64) string { return "tok" })
	if err != nil {
		return nil, err
	}
	if err := f.store.MarkResponded(ctx, f.key, entry.Seq, scope.CallID, ""); err != nil {
		return nil, err
	}
	if f.afterResponded != nil {
		f.afterResponded()
	}
	return &tfprotov5.ApplyResourceChangeResponse{}, nil
}

func TestWrapProviderServer_PromotesOnSuccess(t *testing.T) {
	store := openStore(t)
	inner := &fakeInnerServer{store: store, key: "k1"}
	wrapped := WrapProviderServer(inner, store)

	ctx := context.Background()
	_, err := wrapped.ApplyResourceChange(ctx, &tfprotov5.ApplyResourceChangeRequest{TypeName: "aws_instance"})
	if err != nil {
		t.Fatalf("ApplyResourceChange: %v", err)
	}

	entries, err := store.List(ctx, "k1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].State != journal.StateCompleted {
		t.Fatalf("got %+v, want exactly one completed entry (a normal, non-cancelled RPC must promote responded -> completed)", entries)
	}
}

func TestWrapProviderServer_ReleasesOnCancellation(t *testing.T) {
	store := openStore(t)
	ctx, cancel := context.WithCancel(context.Background())

	inner := &fakeInnerServer{store: store, key: "k1", afterResponded: cancel}
	wrapped := WrapProviderServer(inner, store)

	// The inner call itself succeeds (MarkResponded runs while ctx is
	// still live); only afterward does the RPC's context become
	// cancelled, modeling an interruption that happens after AWS has
	// already answered but before Terraform is confirmed to have received
	// the result.
	_, _ = wrapped.ApplyResourceChange(ctx, &tfprotov5.ApplyResourceChangeRequest{TypeName: "aws_instance"}) //nolint:errcheck // the point of this test is the journal state, not the returned error

	entries, err := store.List(context.Background(), "k1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].State != journal.StatePending || entries[0].CallID != "" {
		t.Fatalf("got %+v, want exactly one pending entry with cleared call_id (a cancelled RPC must release responded -> pending, not leave it stuck)", entries)
	}
}

func TestWrapProviderServer_PassesThroughOtherMethods(t *testing.T) {
	// Any method other than ApplyResourceChange must forward to the inner
	// server via ProviderServer embedding, completely untouched by this
	// package. Exercised via StopProvider, which fakeInnerServer does not
	// override, so calling it through a nil embedded interface would panic
	// if forwarding were somehow broken -- proving the embedding actually
	// works is the point, not StopProvider's behavior itself.
	store := openStore(t)
	inner := &recordingServer{}
	wrapped := WrapProviderServer(inner, store)

	if _, err := wrapped.StopProvider(context.Background(), &tfprotov5.StopProviderRequest{}); err != nil {
		t.Fatalf("StopProvider: %v", err)
	}
	if !inner.stopCalled {
		t.Fatal("StopProvider did not reach the inner server")
	}
}

type recordingServer struct {
	tfprotov5.ProviderServer
	stopCalled bool
}

func (r *recordingServer) StopProvider(context.Context, *tfprotov5.StopProviderRequest) (*tfprotov5.StopProviderResponse, error) {
	r.stopCalled = true
	return &tfprotov5.StopProviderResponse{}, nil
}
