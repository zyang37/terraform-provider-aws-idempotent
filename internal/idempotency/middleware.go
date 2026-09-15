// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package idempotency

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"

	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	smithy "github.com/aws/smithy-go"
	"github.com/aws/smithy-go/logging"
	"github.com/aws/smithy-go/middleware"

	"github.com/hashicorp/terraform-provider-aws/internal/idempotency/journal"
)

const (
	initializeMiddlewareID = "IdempotencyMarkAuto"
	serializeMiddlewareID  = "IdempotencyResolveToken"

	// sdkAutoFillMiddlewareID is aws-sdk-go-v2's own Initialize-step
	// middleware ID for Tier 1 (Smithy `idempotencyToken`-traited)
	// operations; see e.g. ec2's
	// idempotencyToken_initializeOpRunInstances.ID(). Registering ours
	// immediately before it, in the same step, ensures ours runs first:
	// by the time the SDK's auto-fill middleware checks "is the token
	// field nil?", we have already written a non-nil placeholder, so its
	// own random-UUID fill never fires.
	sdkAutoFillMiddlewareID = "OperationIdempotencyTokenAutoFill"

	// sdkSerializerMiddlewareID is the SDK's own request-marshaling
	// middleware. Ours must finish resolving the real token before this
	// runs, since this is what turns in.Parameters into the wire request.
	sdkSerializerMiddlewareID = "OperationSerializer"
)

// Middleware returns an aws.Config-compatible middleware stack mutator
// (suitable for cfg.APIOptions) that gives every AWS operation the
// generated operation table (zz_ops_gen.go) knows about a deterministic,
// journal-backed idempotency token. See the package doc comment for the
// overall design.
//
// store is the journal all resolved tokens are recorded in; it must
// outlive every client built from the resulting aws.Config. accountID
// scopes claim keys so that two AWS accounts (e.g. two provider aliases
// pointed at different accounts, sharing one journal file) never collide
// on the same key even if their requests happen to be byte-identical.
func Middleware(store *journal.Store, accountID string) func(*middleware.Stack) error {
	return func(stack *middleware.Stack) error {
		initMW := &initializeMiddleware{}
		if err := stack.Initialize.Insert(initMW, sdkAutoFillMiddlewareID, middleware.Before); err != nil {
			// The SDK auto-fill middleware only exists for Tier 1
			// (traited) operations; for a Tier 2 operation (a token-named
			// member with no Smithy trait, e.g. ECS CreateService) it is
			// simply absent from this operation's stack, and Insert
			// returns an error because the relativeTo ID was not found.
			// There is nothing to race against in that case: just add
			// ours as the first Initialize middleware.
			if err := stack.Initialize.Add(initMW, middleware.Before); err != nil {
				return fmt.Errorf("idempotency: register initialize middleware: %w", err)
			}
		}

		serMW := &serializeMiddleware{store: store, accountID: accountID}
		if err := stack.Serialize.Insert(serMW, sdkSerializerMiddlewareID, middleware.Before); err != nil {
			return fmt.Errorf("idempotency: register serialize middleware: %w", err)
		}
		return nil
	}
}

// initializeMiddleware runs first in the Initialize step (see Middleware's
// registration). For an operation the table recognizes, it normalizes the
// token field: an absent value (nil or "") or the codemod's AutoToken
// sentinel is replaced with AutoToken (a no-op if it was already that) and
// the context is marked for resolution by serializeMiddleware; a
// caller-supplied real token is left completely untouched and the context
// is not marked, so serializeMiddleware passes the request through as-is.
//
// Writing AutoToken here, rather than merely reading the field and marking
// ctx, matters: it is what stops the SDK's own auto-fill middleware
// (sdkAutoFillMiddlewareID), which runs immediately after this one on
// Tier 1 operations, from seeing a nil field and filling it with a random
// UUID before we ever get to resolve it ourselves in the Serialize step.
type initializeMiddleware struct{}

func (*initializeMiddleware) ID() string { return initializeMiddlewareID }

func (m *initializeMiddleware) HandleInitialize(
	ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler,
) (middleware.InitializeOutput, middleware.Metadata, error) {
	if spec, ok := lookup(in.Parameters); ok {
		if fv, ok := stringFieldValue(in.Parameters, spec.Field); ok {
			cur, isSet := readStringField(fv)
			if !isSet || cur == "" || cur == AutoToken {
				writeStringField(fv, AutoToken)
				ctx = markAuto(ctx, spec)
			}
		}
	}
	return next.HandleInitialize(ctx, in)
}

// serializeMiddleware runs last in the Serialize step, immediately before
// the request is marshaled (see Middleware's registration). For a call
// initializeMiddleware marked, it computes the deterministic token and
// writes it into the request; for everything else it is a pass-through.
type serializeMiddleware struct {
	store     *journal.Store
	accountID string
}

func (*serializeMiddleware) ID() string { return serializeMiddlewareID }

func (m *serializeMiddleware) HandleSerialize(
	ctx context.Context, in middleware.SerializeInput, next middleware.SerializeHandler,
) (middleware.SerializeOutput, middleware.Metadata, error) {
	spec, marked := autoSpec(ctx)
	if !marked {
		return next.HandleSerialize(ctx, in)
	}
	fv, ok := stringFieldValue(in.Parameters, spec.Field)
	if !ok {
		// Should not happen: initializeMiddleware only calls markAuto
		// after successfully resolving this same field. Fail safe by
		// treating it as unmarked rather than risking a bad reflect.Value.
		return next.HandleSerialize(ctx, in)
	}

	key, seq, err := m.resolve(ctx, in.Parameters, spec, fv)
	if err != nil {
		logf(ctx, logging.Warn, "idempotency: falling back to a random token for %s.%s: %s",
			awsmiddleware.GetServiceID(ctx), middleware.GetOperationName(ctx), err)
		if randErr := writeRandomFallback(fv, spec); randErr != nil {
			return middleware.SerializeOutput{}, middleware.Metadata{}, fmt.Errorf("idempotency: %s field %s has no usable fallback: %w", spec.Format, spec.Field, randErr)
		}
		return next.HandleSerialize(ctx, in)
	}

	out, md, callErr := next.HandleSerialize(ctx, in)
	m.record(ctx, key, seq, callErr)
	return out, md, callErr
}

// resolve computes this request's claim key, claims (or reclaims) a
// journal entry for it, and writes the resulting token into fv. It
// returns the key and sequence number so the caller can pass them to
// record once the call completes.
func (m *serializeMiddleware) resolve(ctx context.Context, params any, spec opSpec, fv reflect.Value) (key string, seq int64, _ error) {
	if spec.Format == FormatUnsupported {
		return "", 0, fmt.Errorf("operation has no token format this build knows how to render")
	}

	service := awsmiddleware.GetServiceID(ctx)
	operation := middleware.GetOperationName(ctx)
	region := awsmiddleware.GetRegion(ctx)

	digest, err := Hash256(params, spec.Field)
	if err != nil {
		return "", 0, err
	}
	key = requestKey(m.accountID, region, service, operation, digest)

	resourceType := ""
	if scope, ok := callScope(ctx); ok {
		resourceType = scope.ResourceType
	}

	salt := m.store.Salt()
	entry, err := m.store.ClaimOrCreate(ctx, key, journal.Meta{Service: service, Operation: operation, ResourceType: resourceType},
		func(seq int64) string {
			return spec.Format.Render(deriveDigest(salt, key, seq))
		})
	if err != nil {
		return "", 0, fmt.Errorf("journal claim failed: %w", err)
	}

	writeStringField(fv, entry.Token)
	return key, entry.Seq, nil
}

// record updates the claimed entry once the AWS call returns: responded on
// success, failed on a client-fault error AWS will never accept a
// byte-identical retry of, or left exactly as pending -- untouched -- for
// anything ambiguous (a timeout, a cancelled context, a connection reset,
// a server-fault/throttling error, ...), since those are precisely the
// cases where we cannot tell whether AWS actually received and is
// processing the request. Leaving it pending is what makes the
// crash-recovery case work even without an actual crash: an apply that
// merely times out waiting for a slow AWS response behaves identically to
// one that was SIGKILLed mid-request.
func (m *serializeMiddleware) record(ctx context.Context, key string, seq int64, callErr error) {
	switch classify(callErr) {
	case outcomeSuccess:
		callID := ""
		if scope, ok := callScope(ctx); ok {
			callID = scope.CallID
		}
		if err := m.store.MarkResponded(ctx, key, seq, callID, ""); err != nil {
			logf(ctx, logging.Warn, "idempotency: journal MarkResponded failed for a successful call (the request still succeeded; only crash-recovery bookkeeping for %s.%s#%d was affected): %s",
				awsmiddleware.GetServiceID(ctx), middleware.GetOperationName(ctx), seq, err)
		}
	case outcomeRejected:
		if err := m.store.MarkFailed(ctx, key, seq); err != nil {
			logf(ctx, logging.Warn, "idempotency: journal MarkFailed failed: %s", err)
		}
	case outcomeAmbiguous:
		// Leave pending: see the doc comment above.
	}
}

// outcome classifies an AWS SDK call error for the purpose of deciding
// whether its journal entry's token may safely be reused.
type outcome int

const (
	outcomeAmbiguous outcome = iota // default: request delivery to AWS is uncertain
	outcomeSuccess
	outcomeRejected
)

// classify inspects err the way the rest of the provider already does for
// retry/waiter logic (see internal/errs), but with a narrower question:
// not "should the SDK retry", but "is it certain AWS never durably
// accepted this exact request, and never will on a byte-identical retry".
// Only a modeled API error (one generated from the service's Smithy model)
// whose fault is attributed to the client -- a genuine input validation
// failure -- qualifies. A server-fault or throttling error is a
// smithy.APIError too, but is exactly the ambiguous case (AWS may have
// partially processed the request before failing), so it is deliberately
// NOT classified as rejected; see record's doc comment.
func classify(err error) outcome {
	if err == nil {
		return outcomeSuccess
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorFault() == smithy.FaultClient {
		return outcomeRejected
	}
	return outcomeAmbiguous
}

// requestKey derives the claim key used to look up/create a journal entry.
// It intentionally omits the AWS partition (see docs/DEV_PLAN.md section
// 4.2's note): no two regions across different partitions (aws, aws-cn,
// aws-us-gov, ...) share a name, so region alone already disambiguates
// them without needing the partition ID, which in any case is not
// available this early in the middleware stack (it is set by endpoint
// resolution, which runs in the Finalize step, after Serialize).
func requestKey(accountID, region, service, operation string, digest [32]byte) string {
	h := sha256.New()
	fmt.Fprintf(h, "v1\x00%s\x00%s\x00%s\x00%s\x00", accountID, region, service, operation)
	h.Write(digest[:])
	return fmt.Sprintf("%x", h.Sum(nil))
}

// deriveDigest computes the per-sequence-number token material:
// HMAC-SHA256(salt, key || seq). Mixing in the workspace salt (see
// journal.Store.Salt) is what stops two workspaces with byte-identical
// configuration against the same account from being able to predict, and
// thereby silently adopt, each other's resources.
func deriveDigest(salt [32]byte, key string, seq int64) [32]byte {
	mac := hmac.New(sha256.New, salt[:])
	fmt.Fprintf(mac, "%s\x00%d", key, seq)
	var out [32]byte
	copy(out[:], mac.Sum(nil))
	return out
}

// writeRandomFallback is used only when a marked call's request cannot be
// canonically hashed (see resolve). It sends the same kind of
// process-random, non-deterministic token the SDK or upstream code would
// have sent anyway -- no crash-recovery protection, but no worse than
// before this package existed, and it still satisfies the operation's
// length/pattern constraints via spec.Format when one is known.
func writeRandomFallback(fv reflect.Value, spec opSpec) error {
	if spec.Format == FormatUnsupported {
		return fmt.Errorf("no supported token format for field %s", spec.Field)
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	writeStringField(fv, spec.Format.Render(b))
	return nil
}

func logf(ctx context.Context, class logging.Classification, format string, args ...any) {
	if l := middleware.GetLogger(ctx); l != nil {
		l.Logf(class, format, args...)
	}
}
