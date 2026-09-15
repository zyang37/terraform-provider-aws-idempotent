// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package idempotency

import (
	"encoding/hex"
	"io"
	"testing"
	"time"
)

func hashHex(t *testing.T, v any, tokenField string) string {
	t.Helper()
	d, err := Hash256(v, tokenField)
	if err != nil {
		t.Fatalf("Hash256: %v", err)
	}
	return hex.EncodeToString(d[:])
}

func mustErr(t *testing.T, v any, tokenField string) error {
	t.Helper()
	_, err := Hash256(v, tokenField)
	if err == nil {
		t.Fatalf("Hash256(%#v): expected an error, got none", v)
	}
	return err
}

type runInstancesLike struct {
	ClientToken  *string
	InstanceType *string
	MinCount     *int32
	MaxCount     *int32
	Tags         []tagLike
	Meta         map[string]string
}

type tagLike struct {
	Key   *string
	Value *string
}

func ptr[T any](v T) *T { return &v }

func TestHash256_Deterministic(t *testing.T) {
	v := runInstancesLike{
		ClientToken:  ptr("should-be-ignored"),
		InstanceType: ptr("t3.micro"),
		MinCount:     ptr(int32(1)),
		MaxCount:     ptr(int32(1)),
		Tags:         []tagLike{{Key: ptr("Name"), Value: ptr("web")}},
	}
	h1 := hashHex(t, &v, "ClientToken")
	h2 := hashHex(t, &v, "ClientToken")
	if h1 != h2 {
		t.Fatalf("hash not deterministic: %s != %s", h1, h2)
	}
}

func TestHash256_TokenFieldExcluded(t *testing.T) {
	v1 := runInstancesLike{ClientToken: ptr("token-A"), InstanceType: ptr("t3.micro")}
	v2 := runInstancesLike{ClientToken: ptr("token-B"), InstanceType: ptr("t3.micro")}
	h1 := hashHex(t, &v1, "ClientToken")
	h2 := hashHex(t, &v2, "ClientToken")
	if h1 != h2 {
		t.Fatalf("hash differs when only the token field differs: %s != %s", h1, h2)
	}
}

func TestHash256_OtherFieldDifference_ChangesHash(t *testing.T) {
	v1 := runInstancesLike{ClientToken: ptr("x"), InstanceType: ptr("t3.micro")}
	v2 := runInstancesLike{ClientToken: ptr("x"), InstanceType: ptr("t3.large")}
	h1 := hashHex(t, &v1, "ClientToken")
	h2 := hashHex(t, &v2, "ClientToken")
	if h1 == h2 {
		t.Fatal("hash identical despite a real field difference (InstanceType)")
	}
}

func TestHash256_NilAndEmptyEquivalent(t *testing.T) {
	nilSlice := runInstancesLike{InstanceType: ptr("t3.micro"), Tags: nil}
	emptySlice := runInstancesLike{InstanceType: ptr("t3.micro"), Tags: []tagLike{}}
	if hashHex(t, &nilSlice, "ClientToken") != hashHex(t, &emptySlice, "ClientToken") {
		t.Fatal("nil slice and empty slice hashed differently")
	}

	nilMap := runInstancesLike{InstanceType: ptr("t3.micro"), Meta: nil}
	emptyMap := runInstancesLike{InstanceType: ptr("t3.micro"), Meta: map[string]string{}}
	if hashHex(t, &nilMap, "ClientToken") != hashHex(t, &emptyMap, "ClientToken") {
		t.Fatal("nil map and empty map hashed differently")
	}

	var unsetPtr *string
	nilString := runInstancesLike{InstanceType: unsetPtr}
	emptyString := runInstancesLike{InstanceType: ptr("")}
	if hashHex(t, &nilString, "ClientToken") != hashHex(t, &emptyString, "ClientToken") {
		t.Fatal("nil *string and pointer-to-empty-string hashed differently")
	}
}

func TestHash256_MapKeyOrderIndependent(t *testing.T) {
	v1 := runInstancesLike{Meta: map[string]string{"a": "1", "b": "2", "c": "3"}}
	v2 := runInstancesLike{Meta: map[string]string{"c": "3", "a": "1", "b": "2"}}
	if hashHex(t, &v1, "ClientToken") != hashHex(t, &v2, "ClientToken") {
		t.Fatal("map hash depends on Go's randomized iteration order; keys must be sorted before hashing")
	}
}

func TestHash256_MapValueDifference_ChangesHash(t *testing.T) {
	v1 := runInstancesLike{Meta: map[string]string{"a": "1"}}
	v2 := runInstancesLike{Meta: map[string]string{"a": "2"}}
	if hashHex(t, &v1, "ClientToken") == hashHex(t, &v2, "ClientToken") {
		t.Fatal("differing map values produced the same hash")
	}
}

func TestHash256_SliceOrderMatters(t *testing.T) {
	v1 := runInstancesLike{Tags: []tagLike{{Key: ptr("a")}, {Key: ptr("b")}}}
	v2 := runInstancesLike{Tags: []tagLike{{Key: ptr("b")}, {Key: ptr("a")}}}
	if hashHex(t, &v1, "ClientToken") == hashHex(t, &v2, "ClientToken") {
		t.Fatal("expected slice element order to matter (unlike map keys), got equal hashes")
	}
}

func TestHash256_NoFieldNameCollision(t *testing.T) {
	// {A: 0, B: 5} must not hash the same as {A: 5, B: 0}: the field-name
	// prefix before each value must prevent this even though both structs
	// "contain the same bytes" if names were not accounted for.
	type pair struct{ A, B int64 }
	h1 := hashHex(t, &pair{A: 0, B: 5}, "")
	h2 := hashHex(t, &pair{A: 5, B: 0}, "")
	if h1 == h2 {
		t.Fatal("field name / position collision: {A:0,B:5} hashed the same as {A:5,B:0}")
	}
}

func TestHash256_TimeUsesUTC(t *testing.T) {
	loc := time.FixedZone("UTC-5", -5*60*60)
	local := time.Date(2026, 1, 1, 12, 0, 0, 0, loc)
	utc := local.UTC()
	type withTime struct{ T time.Time }
	h1 := hashHex(t, &withTime{T: local}, "")
	h2 := hashHex(t, &withTime{T: utc}, "")
	if h1 != h2 {
		t.Fatal("the same instant in two time zones hashed differently; Time must be normalized to UTC")
	}
}

func TestHash256_StreamingReaderIsNotHashable(t *testing.T) {
	type withBody struct{ Body io.Reader }
	err := mustErr(t, &withBody{Body: nil}, "")
	var nh *ErrNotHashable
	if !isErrNotHashable(err, &nh) {
		t.Fatalf("expected ErrNotHashable for an io.Reader field, got %v (%T)", err, err)
	}
}

func TestHash256_ByteSliceHashesAsOpaqueBytes(t *testing.T) {
	type withBlob struct{ B []byte }
	h1 := hashHex(t, &withBlob{B: []byte("hello")}, "")
	h2 := hashHex(t, &withBlob{B: []byte("hello")}, "")
	h3 := hashHex(t, &withBlob{B: []byte("world")}, "")
	if h1 != h2 {
		t.Fatal("identical byte slices hashed differently")
	}
	if h1 == h3 {
		t.Fatal("different byte slices hashed the same")
	}
}

func isErrNotHashable(err error, target **ErrNotHashable) bool {
	e, ok := err.(*ErrNotHashable)
	if ok {
		*target = e
	}
	return ok
}
