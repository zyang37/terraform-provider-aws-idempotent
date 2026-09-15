// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package idempotency

// This file exercises Middleware against real aws-sdk-go-v2 service clients
// (ec2, ecs) pointed at a local httptest server, following the pattern
// upstream terraform-provider-aws's own
// internal/conns/apicall/apicall_e2e_test.go uses to guard against
// smithy-go/codegen changes invalidating assumptions about the middleware
// stack: a fake endpoint proves the real generated client code, the real
// SDK middleware stack, and this package's middleware interact the way the
// design in docs/DEV_PLAN.md section 4.2 assumes, which a test that calls
// HandleInitialize/HandleSerialize directly could not.

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/smithy-go/middleware"

	"github.com/hashicorp/terraform-provider-aws/internal/idempotency/journal"
)

func decodeJSONBody(t *testing.T, r *http.Request, v any) {
	t.Helper()
	defer r.Body.Close() //nolint:errcheck
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		t.Fatalf("decode JSON request body: %v", err)
	}
}

func openStore(t *testing.T) *journal.Store {
	t.Helper()
	s, err := journal.Open(context.Background(), filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() }) //nolint:errcheck
	return s
}

const testAccountID = "123456789012"

func newEC2Client(t *testing.T, serverURL string, store *journal.Store) *ec2.Client {
	t.Helper()
	return ec2.NewFromConfig(aws.Config{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String(serverURL),
		Credentials:      credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		APIOptions:       []func(*middleware.Stack) error{Middleware(store, testAccountID)},
		RetryMaxAttempts: 3,
	})
}

func newECSClient(t *testing.T, serverURL string, store *journal.Store) *ecs.Client {
	t.Helper()
	return ecs.NewFromConfig(aws.Config{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(serverURL),
		Credentials:  credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		APIOptions:   []func(*middleware.Stack) error{Middleware(store, testAccountID)},
	})
}

const ec2RunInstancesOKBody = `<?xml version="1.0" encoding="UTF-8"?>
<RunInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">
  <requestId>req-1</requestId>
  <reservationId>r-1</reservationId>
  <ownerId>123456789012</ownerId>
  <groupSet/>
  <instancesSet/>
</RunInstancesResponse>`

type ec2Error struct {
	XMLName xml.Name `xml:"Response"`
	Code    string   `xml:"Errors>Error>Code"`
	Message string   `xml:"Errors>Error>Message"`
	ReqID   string   `xml:"RequestID"`
}

func ec2ErrorBody(code, msg string) string {
	b, _ := xml.Marshal(ec2Error{Code: code, Message: msg, ReqID: "req-err"})
	return xml.Header + string(b)
}

// TestMiddleware_EC2_ResolvesDeterministicToken is the core proof of the
// design: the token that actually reaches the wire for a Tier 1
// (idempotencyToken-traited) EC2 operation is the deterministic value this
// package computes -- not a random UUID from aws-sdk-go-v2's own
// OperationIdempotencyTokenAutoFill middleware, which runs in the same
// Initialize step and would fire instead if Middleware's registration
// order (see the sdkAutoFillMiddlewareID doc comment) were wrong. If that
// registration were broken, requestKey/deriveDigest would never even be
// called, and this test's captured wire value would not match the
// independently-recomputed expected token below.
func TestMiddleware_EC2_ResolvesDeterministicToken(t *testing.T) {
	store := openStore(t)
	var gotToken string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		gotToken = r.FormValue("ClientToken")
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, ec2RunInstancesOKBody) //nolint:errcheck
	}))
	defer server.Close()

	client := newEC2Client(t, server.URL, store)
	ctx := context.Background()
	_, err := client.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-12345678"),
		InstanceType: "t3.micro",
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
	})
	if err != nil {
		t.Fatalf("RunInstances: %v", err)
	}
	if gotToken == "" {
		t.Fatal("no ClientToken reached the server at all")
	}

	// Recompute the expected token exactly as resolve()/deriveDigest do,
	// using the operation table's own entry for RunInstancesInput so this
	// test tracks the generated table rather than hard-coding its format.
	spec, ok := opTable["github.com/aws/aws-sdk-go-v2/service/ec2.RunInstancesInput"]
	if !ok || spec.Field != "ClientToken" {
		t.Fatalf("opTable entry for ec2.RunInstancesInput missing or unexpected: %+v (ok=%v)", spec, ok)
	}
	input := ec2.RunInstancesInput{ImageId: aws.String("ami-12345678"), InstanceType: "t3.micro", MinCount: aws.Int32(1), MaxCount: aws.Int32(1)}
	digest, err := Hash256(&input, spec.Field)
	if err != nil {
		t.Fatalf("Hash256: %v", err)
	}
	key := requestKey(testAccountID, "us-east-1", ec2.ServiceID, "RunInstances", digest)
	want := spec.Format.Render(deriveDigest(store.Salt(), key, 0))

	if gotToken != want {
		t.Fatalf("wire ClientToken = %q, want deterministically-derived %q (if these never match, either the derivation changed or the SDK's own random auto-fill middleware fired instead)", gotToken, want)
	}

	entries, err := store.List(ctx, key)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d journal entries for key %s, want exactly 1 (a wrong registration order would leave zero: ctx never marked, so serializeMiddleware never claims anything)", len(entries), key)
	}
	if entries[0].Token != gotToken {
		t.Fatalf("journal token %q != wire token %q", entries[0].Token, gotToken)
	}
	if entries[0].State != journal.StateResponded {
		t.Fatalf("entry state = %s, want responded (call succeeded)", entries[0].State)
	}
}

// TestMiddleware_CallerSuppliedToken_PassesThrough proves a real,
// non-empty, non-sentinel token supplied by calling code (e.g. a resource
// argument the codemod's exclude.txt keeps untouched) is sent completely
// unmodified, and never touches the journal at all -- see
// docs/DEV_PLAN.md section 3 point 1.
func TestMiddleware_CallerSuppliedToken_PassesThrough(t *testing.T) {
	store := openStore(t)
	var gotToken string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm() //nolint:errcheck
		gotToken = r.FormValue("ClientToken")
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, ec2RunInstancesOKBody) //nolint:errcheck
	}))
	defer server.Close()

	client := newEC2Client(t, server.URL, store)
	ctx := context.Background()
	const callerToken = "my-caller-supplied-token-1234"
	_, err := client.RunInstances(ctx, &ec2.RunInstancesInput{
		ClientToken:  aws.String(callerToken),
		ImageId:      aws.String("ami-12345678"),
		InstanceType: "t3.micro",
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
	})
	if err != nil {
		t.Fatalf("RunInstances: %v", err)
	}
	if gotToken != callerToken {
		t.Fatalf("wire ClientToken = %q, want the untouched caller-supplied %q", gotToken, callerToken)
	}

	all, err := store.All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("expected no journal entries for a caller-supplied token, got %d: %+v", len(all), all)
	}
}

// TestMiddleware_RetriesReuseSameToken proves that when the SDK's own
// retry logic resends a request after a transient failure, it resends the
// exact same resolved token -- i.e. resolution happens once per logical
// Invoke (in the Serialize step, which runs before the retry loop in
// Finalize), not once per HTTP attempt. This is what makes an apply that
// merely experiences a slow/flaky connection (no crash at all) behave
// safely: every attempt AWS actually receives carries the same
// deduplication key.
func TestMiddleware_RetriesReuseSameToken(t *testing.T) {
	store := openStore(t)
	var mu sync.Mutex
	var tokensSeen []string
	var attempt int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm() //nolint:errcheck
		mu.Lock()
		tokensSeen = append(tokensSeen, r.FormValue("ClientToken"))
		mu.Unlock()
		n := atomic.AddInt32(&attempt, 1)
		if n <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, ec2ErrorBody("InternalError", "try again")) //nolint:errcheck
			return
		}
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, ec2RunInstancesOKBody) //nolint:errcheck
	}))
	defer server.Close()

	client := newEC2Client(t, server.URL, store)
	ctx := context.Background()
	_, err := client.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-12345678"),
		InstanceType: "t3.micro",
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
	})
	if err != nil {
		t.Fatalf("RunInstances (expected to succeed after retries): %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(tokensSeen) < 3 {
		t.Fatalf("server saw %d attempts, want at least 3 (2 failures + 1 success) -- check RetryMaxAttempts", len(tokensSeen))
	}
	for i, tok := range tokensSeen {
		if tok == "" || tok != tokensSeen[0] {
			t.Fatalf("attempt %d sent token %q, want every attempt to reuse attempt 0's token %q", i, tok, tokensSeen[0])
		}
	}

	all, err := store.All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) != 1 || all[0].State != journal.StateResponded {
		t.Fatalf("got %+v, want exactly one responded entry (the eventual success)", all)
	}
}

// TestMiddleware_ECS_Tier2_NoSDKAutoFillMiddleware exercises a Tier 2
// operation -- ECS CreateService, whose ClientToken member is not
// Smithy-traited, so aws-sdk-go-v2 has no OperationIdempotencyTokenAutoFill
// middleware for it at all. This is the branch of Middleware's
// registration where stack.Initialize.Insert(..., sdkAutoFillMiddlewareID,
// ...) fails (the ID does not exist in this operation's stack) and it must
// fall back to stack.Initialize.Add without erroring the whole client
// construction.
func TestMiddleware_ECS_Tier2_NoSDKAutoFillMiddleware(t *testing.T) {
	store := openStore(t)
	var gotToken string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ClientToken string `json:"clientToken"`
		}
		decodeJSONBody(t, r, &body)
		gotToken = body.ClientToken
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.Write([]byte(`{}`)) //nolint:errcheck
	}))
	defer server.Close()

	client := newECSClient(t, server.URL, store)
	ctx := context.Background()
	_, err := client.CreateService(ctx, &ecs.CreateServiceInput{
		ServiceName: aws.String("my-service"),
		Cluster:     aws.String("my-cluster"),
	})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	if gotToken == "" {
		t.Fatal("no clientToken reached the server; Tier 2 resolution did not run")
	}

	spec, ok := opTable["github.com/aws/aws-sdk-go-v2/service/ecs.CreateServiceInput"]
	if !ok {
		t.Fatal("opTable has no entry for ecs.CreateServiceInput")
	}
	if spec.SDKAutoFill {
		t.Fatal("test assumption violated: ecs.CreateServiceInput.clientToken is expected to be untraited (Tier 2) in the generated table")
	}

	entries, err := store.All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(entries) != 1 || entries[0].Token != gotToken {
		t.Fatalf("got %+v, want exactly one entry matching the wire token %q", entries, gotToken)
	}
}
