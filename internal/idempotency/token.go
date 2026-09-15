// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package idempotency

import (
	"encoding/hex"
	"fmt"
)

// AutoToken is the sentinel value tools/codemod-tokens writes in place of
// upstream's per-apply random token generators (create.UniqueId,
// create.UUID, sdkid.UniqueId, sdkid.PrefixedUniqueId, ...).
//
// It is never sent to AWS. The Initialize-step middleware (see
// initializeMiddleware) treats it exactly like an empty/absent field; the
// Serialize-step middleware (see serializeMiddleware) always overwrites it
// with a deterministic token before the request is marshaled. Its value is
// deliberately not a legal token for any published AWS token pattern --
// every pattern in the generated operation table requires printable
// characters, and this contains NUL bytes -- so that if the Serialize
// middleware is ever missing from the stack (a wiring bug, not a data bug:
// see Middleware's doc comment on registration order), the request fails
// closed with a visible AWS validation error instead of silently sending a
// literal, shared, meaningless token on behalf of every caller.
const AutoToken = "\x00idempotency.auto-token.v1\x00"

// Format identifies how a 32-byte digest is rendered into a token string
// that satisfies a given AWS operation's length and pattern constraints.
// tools/gen-idempotent-ops/gen.py picks the format for each table entry by
// checking sample tokens of each format against the operation's Smithy
// `length` and `pattern` traits; see pick_format in that script.
type Format uint8

const (
	// FormatUnsupported marks an operation whose token constraints no
	// known format satisfies. opSpec entries with this format are never
	// resolved deterministically; the request falls back to whatever the
	// SDK or upstream code would otherwise have sent (see
	// classifyForResolution in middleware.go).
	FormatUnsupported Format = iota

	// FormatUUID renders a canonical 8-4-4-4-12 lowercase-hex UUID
	// (36 characters), version and variant bits forced to mark it as
	// deliberately-generated rather than random. This is the format nearly
	// all `clientToken`-style members use.
	FormatUUID

	// FormatHex32 renders the first 16 digest bytes as 32 lowercase hex
	// characters. Used by short fixed-width tokens such as ACM's
	// IdempotencyToken (32-character limit).
	FormatHex32

	// FormatHex64 renders all 32 digest bytes as 64 lowercase hex
	// characters. Used by longer free-form token fields.
	FormatHex64
)

func (f Format) String() string {
	switch f {
	case FormatUUID:
		return "UUID"
	case FormatHex32:
		return "Hex32"
	case FormatHex64:
		return "Hex64"
	default:
		return "Unsupported"
	}
}

// Render deterministically encodes a 32-byte digest (see Digest) as a token
// string in the given format. It panics if f is FormatUnsupported; callers
// must check opSpec.Format != FormatUnsupported before calling Resolve.
func (f Format) Render(digest [32]byte) string {
	switch f {
	case FormatUUID:
		var b [16]byte
		copy(b[:], digest[:16])
		b[6] = (b[6] & 0x0f) | 0x50 // version 5: name-based (closest semantic match; not a real SHA1 UUIDv5)
		b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
		return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	case FormatHex32:
		return hex.EncodeToString(digest[:16])
	case FormatHex64:
		return hex.EncodeToString(digest[:32])
	default:
		panic("idempotency: Render called with FormatUnsupported")
	}
}
