// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package idempotency

import "reflect"

// opSpec describes one AWS operation's idempotency token member. Rows are
// generated into zz_ops_gen.go by tools/gen-idempotent-ops/gen.py from the
// aws-sdk-go-v2 Smithy models; see docs/SUPPORTED_APIS.md for the full,
// human-readable list this table is built from.
type opSpec struct {
	// Field is the exported Go struct field name of the token member on the
	// operation's <Op>Input type, e.g. "ClientToken".
	Field string

	// Format is how a resolved token is rendered. FormatUnsupported means
	// this operation's token has length/pattern constraints no generated
	// format satisfies; such entries are looked up (for documentation and
	// coverage reporting) but never resolved.
	Format Format

	// SDKAutoFill is true if aws-sdk-go-v2 itself would fill an empty
	// value for this field with a random UUID (the Smithy
	// `idempotencyToken` trait was present). It does not change our
	// behavior -- we always resolve before the SDK's own auto-fill
	// middleware runs -- but is kept for coverage reporting.
	SDKAutoFill bool

	// NameIsToken is true when the member name matched the token-name
	// heuristic (see gen.py's TOKEN_NAME_RE). A traited member whose name
	// does NOT look like a token (e.g. EFS's CreationToken is traited but
	// is a user-facing, state-stored attribute, not a request
	// deduplication key -- callers must supply it themselves, so it is
	// excluded upstream by tools/codemod-tokens/exclude.txt) is recorded
	// with NameIsToken=false and is never resolved automatically.
	NameIsToken bool

	// UsedByProvider is true if, at generation time, terraform-provider-aws
	// called this operation. Kept for coverage reporting; not read at
	// runtime.
	UsedByProvider bool
}

// typeKey computes the opTable lookup key for an SDK operation's input
// struct: "<import path>.<TypeName>", e.g.
// "github.com/aws/aws-sdk-go-v2/service/ec2.RunInstancesInput". params is
// the smithy middleware.InitializeInput/SerializeInput.Parameters value,
// which the SDK always sets to a pointer to the generated Input struct.
func typeKey(params any) (string, bool) {
	v := reflect.ValueOf(params)
	if v.Kind() != reflect.Ptr || v.IsNil() {
		return "", false
	}
	t := v.Elem().Type()
	if t.Kind() != reflect.Struct || t.PkgPath() == "" {
		return "", false
	}
	return t.PkgPath() + "." + t.Name(), true
}

// lookup returns the opSpec for an SDK operation input value, if the
// generated table has one and it names a real token field
// (NameIsToken == true).
func lookup(params any) (opSpec, bool) {
	key, ok := typeKey(params)
	if !ok {
		return opSpec{}, false
	}
	spec, ok := opTable[key]
	if !ok || !spec.NameIsToken {
		return opSpec{}, false
	}
	return spec, true
}
