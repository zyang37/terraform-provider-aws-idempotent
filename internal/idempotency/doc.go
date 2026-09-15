// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

// Package idempotency gives AWS API calls made by this provider a
// deterministic, crash-safe idempotency token instead of the random one
// aws-sdk-go-v2 (or upstream terraform-provider-aws) would otherwise send.
//
// # Why
//
// Terraform's contract is: send a request, then persist the result to
// state. If the provider process is killed between those two steps -- a
// SIGKILL'd `terraform apply`, an OOM, a laptop sleep -- the request may
// already have reached AWS, but state was never updated. The next apply
// sees no resource in state and issues the request again. For an AWS API
// that accepts a client/idempotency token, sending the *same* token on the
// retry makes AWS return the original resource instead of creating a
// second one. Sending a *new* random token, which is what a fresh process
// does by default, defeats that protection entirely -- which is exactly
// what both aws-sdk-go-v2's auto-fill and upstream terraform-provider-aws's
// own `create.UniqueId(ctx)` calls do on every process start.
//
// # How
//
// Two aws-sdk-go-v2 middleware are registered on every service client via
// aws.Config.APIOptions (see Middleware):
//
//   - An Initialize-step middleware normalizes the token field: if it is
//     empty, or holds the AutoToken sentinel the codemod in
//     tools/codemod-tokens writes in place of upstream's random-token call
//     sites, it is marked for deterministic resolution. A caller-supplied
//     token (e.g. from a `client_token` resource argument, if a resource
//     exposes one) is left untouched.
//   - A Serialize-step middleware, for calls marked above, computes a
//     canonical hash of the request (see CanonicalHash), claims a journal
//     entry for that hash (see the journal subpackage), renders a
//     deterministic token from it, and records the outcome once the call
//     returns.
//
// The journal is a local SQLite file (default
// .terraform/aws-idempotency/journal.db). An entry only gets a *new*
// sequence number, and therefore a new token, once its predecessor has been
// delivered to Terraform (state written) or has failed; while an entry is
// merely "sent, response not yet delivered", re-running the same logical
// call reuses it. See journal.Store for the exact state machine, and
// docs/DEV_PLAN.md for the full design and its documented limitations
// (notably: most AWS create APIs do not accept a token at all).
package idempotency
