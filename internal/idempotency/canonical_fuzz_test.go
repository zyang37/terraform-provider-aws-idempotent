// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package idempotency

import "testing"

// FuzzHash256 feeds structurally varied inputs at Hash256 and asserts only
// that it never panics: reflection-based encoders are exactly the kind of
// code that looks correct against hand-picked cases and then panics on
// some field combination nobody thought to write a test for (a deeply
// nested self-referential-looking pointer chain, an interface holding a
// nil interface holding a typed nil, ...). This does not, and cannot on
// its own, assert anything about hash *quality*; canonical_test.go covers
// the specific determinism/equivalence properties that actually matter.
func FuzzHash256(f *testing.F) {
	f.Add("t3.micro", int32(1), true, 0.0)
	f.Add("", int32(-5), false, 3.14159)
	f.Add("unicode: é中文", int32(2147483647), true, -0.0)

	f.Fuzz(func(t *testing.T, s string, n int32, b bool, fl float64) {
		type nested struct {
			S    string
			N    *int32
			Tags map[string]string
			List []string
		}
		v := struct {
			ClientToken *string
			Name        string
			Count       int32
			Flag        bool
			Ratio       float64
			Nested      nested
			NestedPtr   *nested
			Self        *runInstancesLike
		}{
			ClientToken: &s,
			Name:        s,
			Count:       n,
			Flag:        b,
			Ratio:       fl,
			Nested:      nested{S: s, N: &n, Tags: map[string]string{s: s}, List: []string{s, s}},
			NestedPtr:   &nested{S: s},
		}
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Hash256 panicked on input %+v: %v", v, r)
			}
		}()
		if _, err := Hash256(&v, "ClientToken"); err != nil {
			// An error (e.g. from a field type Hash256 legitimately
			// refuses) is fine; a panic is the only failure mode this
			// fuzz target checks for.
			return
		}
	})
}
