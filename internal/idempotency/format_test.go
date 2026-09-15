// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package idempotency

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"regexp"
	"testing"
)

// opsJSONRow mirrors one row of data/ops.json (see
// tools/gen-idempotent-ops/gen.py); only the fields this test needs are
// declared.
type opsJSONRow struct {
	GoPackage string  `json:"go_package"`
	Operation string  `json:"operation"`
	GoField   string  `json:"go_field"`
	MinLen    *int    `json:"min_len"`
	MaxLen    *int    `json:"max_len"`
	Pattern   *string `json:"pattern"`
	Format    *string `json:"format"`
}

// TestOpTable_FormatsSatisfyConstraints regenerates nothing; it cross-checks
// the *committed* zz_ops_gen.go table against the *committed*
// data/ops.json (both produced by the same tools/gen-idempotent-ops/gen.py
// run -- see docs/DEV_PLAN.md section 9) by rendering a sample token in
// each opTable entry's chosen Format and verifying it satisfies that
// operation's real Smithy length and pattern constraints. This is the
// regression test for gen.py's pick_format function: if a future SDK
// model update tightens a constraint, or a manual edit to zz_ops_gen.go
// picks the wrong format, this fails loudly instead of shipping a token
// AWS will reject with a ValidationException at apply time.
func TestOpTable_FormatsSatisfyConstraints(t *testing.T) {
	data, err := os.ReadFile("../../data/ops.json")
	if err != nil {
		t.Skipf("data/ops.json not found (expected at repo root's data/ dir): %v", err)
	}
	var parsed struct {
		Traited   []opsJSONRow `json:"traited"`
		Untraited []opsJSONRow `json:"untraited"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse data/ops.json: %v", err)
	}

	rows := append(parsed.Traited, parsed.Untraited...) //nolint:gocritic // test-local, one-shot append is fine
	byKey := make(map[string]opsJSONRow, len(rows))
	for _, r := range rows {
		key := "github.com/aws/aws-sdk-go-v2/service/" + r.GoPackage + "." + r.Operation + "Input"
		byKey[key] = r
	}

	if len(opTable) == 0 {
		t.Fatal("opTable is empty; zz_ops_gen.go was not generated (see tools/gen-idempotent-ops/gen.py --go)")
	}

	checked := 0
	for key, spec := range opTable {
		if spec.Format == FormatUnsupported {
			continue // nothing to check: never rendered at runtime
		}
		row, ok := byKey[key]
		if !ok {
			t.Errorf("%s: in opTable but not in data/ops.json; the two were generated from different inputs", key)
			continue
		}

		digest := sha256.Sum256([]byte(key)) // any deterministic 32 bytes; content doesn't matter, only length/charset of the rendered token
		token := spec.Format.Render(digest)

		if row.MinLen != nil && len(token) < *row.MinLen {
			t.Errorf("%s: Format %s renders a %d-char token, shorter than the operation's min length %d", key, spec.Format, len(token), *row.MinLen)
		}
		if row.MaxLen != nil && len(token) > *row.MaxLen {
			t.Errorf("%s: Format %s renders a %d-char token, longer than the operation's max length %d", key, spec.Format, len(token), *row.MaxLen)
		}
		if row.Pattern != nil {
			re, err := regexp.Compile(*row.Pattern)
			if err != nil {
				continue // a Smithy ECMA-262 pattern Go's RE2 cannot parse (e.g. \p{...} property classes); gen.py already skips these when picking a format, so nothing to check here
			}
			if !re.MatchString(token) {
				t.Errorf("%s: Format %s token %q does not match pattern %s", key, spec.Format, token, *row.Pattern)
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no opTable entries were checked; the cross-reference between zz_ops_gen.go and data/ops.json produced no matches")
	}
	t.Logf("checked %d operations with a resolvable format against their Smithy constraints", checked)
}
