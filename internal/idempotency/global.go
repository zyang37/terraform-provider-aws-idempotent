// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package idempotency

import (
	"context"
	"os"
	"sync"

	"github.com/hashicorp/terraform-provider-aws/internal/idempotency/journal"
)

// Environment variables that configure this package without needing a
// provider schema change. This is a deliberately small, static
// configuration surface for the initial integration (see
// docs/DEV_PLAN.md's phase 3 notes): a full `idempotency {}` provider
// config block, resolved per-alias like the rest of the provider's
// configuration, is tracked as follow-up work rather than implemented
// here, since threading a new config value through
// sdkv2.NewProvider/framework.NewProvider's existing configuration
// resolution is a larger, separable change from the journal/middleware
// mechanism itself.
const (
	// EnvDisable, if set to any non-empty value, disables this package
	// entirely: Middleware is never registered and the gRPC wrapper in
	// server.go passes every call straight through. Useful for a quick
	// A/B comparison against upstream behavior, or as an escape hatch if
	// the journal is ever suspected of causing a problem.
	EnvDisable = "TF_AWS_IDEMPOTENCY_DISABLE"

	// EnvJournalPath overrides DefaultJournalPath.
	EnvJournalPath = "TF_AWS_IDEMPOTENCY_JOURNAL"
)

// DefaultJournalPath is where the journal lives when EnvJournalPath is not
// set: relative to the process's working directory, which for a
// `terraform apply` is the Terraform working directory -- the same place
// `.terraform/` itself lives, and, like it, ordinarily left out of version
// control.
const DefaultJournalPath = ".terraform/aws-idempotency/journal.db"

// Enabled reports whether idempotency resolution should run at all. Both
// server.go's gRPC wrapper and conns/config.go's client construction check
// this before doing any work.
func Enabled() bool {
	return os.Getenv(EnvDisable) == ""
}

func journalPath() string {
	if p := os.Getenv(EnvJournalPath); p != "" {
		return p
	}
	return DefaultJournalPath
}

var (
	globalOnce  sync.Once
	globalStore *journal.Store
	globalErr   error
)

// GlobalStore returns the single journal.Store shared by every AWS client
// and every ApplyResourceChange call in this provider process, opening it
// on first use. Every caller across the provider (the gRPC wrapper, and
// every provider alias's client construction in conns/config.go) must
// share exactly one Store: it is what lets the gRPC wrapper's
// PromoteCall/ReleaseCall (keyed by a call ID stashed in the request
// context, see WithCallScope) see and update the same entries an AWS SDK
// call's middleware just wrote via ClaimOrCreate/MarkResponded.
//
// A failure to open the journal (e.g. an unwritable working directory) is
// cached and returned on every subsequent call rather than retried, so a
// persistently broken environment fails fast and consistently rather than
// retrying expensive I/O on every API call; the caller is expected to
// treat a GlobalStore error as "idempotency is unavailable this run" and
// fall back to ordinary (non-deterministic-token) behavior rather than
// aborting the provider entirely -- see how conns/config.go and server.go
// use it.
func GlobalStore(ctx context.Context) (*journal.Store, error) {
	globalOnce.Do(func() {
		globalStore, globalErr = journal.Open(ctx, journalPath())
	})
	return globalStore, globalErr
}

// CloseGlobalStore releases the global journal, if one was ever opened.
// The provider process calls this on clean shutdown (see main.go); on a
// crash it is simply never called, which is the expected, designed-for
// input to the next process's liveness check (see journal's liveness.go).
func CloseGlobalStore() error {
	if globalStore == nil {
		return nil
	}
	return globalStore.Close()
}
