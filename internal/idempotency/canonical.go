// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package idempotency

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"io"
	"math"
	"reflect"
	"sort"
	"time"
)

// encodingVersion is written as the first bytes of every hash. Any change
// to the encoding rules below (a new type case, different float handling,
// a different empty/nil convention, ...) must bump this, so that the
// change alters every derived key instead of silently colliding an old
// request encoding with a new one that happens to produce the same bytes.
const encodingVersion uint32 = 1

// ErrNotHashable is returned by CanonicalHash when a value cannot be
// encoded deterministically: a streaming request body (io.Reader), an
// opaque Smithy document (smithy-go's document.Interface, used by a few
// schema-less JSON APIs), or a Go func/chan/complex/unsafe.Pointer, none of
// which a caller-supplied argument should ever contain but which
// reflection cannot rule out statically.
//
// Callers (see resolveToken in middleware.go) treat this as "this
// particular call cannot be made deterministic"; they fall back to
// whatever the SDK or upstream resource code would otherwise have sent,
// rather than failing the call.
type ErrNotHashable struct {
	Path string
	Kind reflect.Kind
}

func (e *ErrNotHashable) Error() string {
	return fmt.Sprintf("idempotency: value at %s (kind %s) cannot be canonically hashed", e.Path, e.Kind)
}

var (
	timeType   = reflect.TypeOf(time.Time{})
	readerType = reflect.TypeOf((*io.Reader)(nil)).Elem()
)

// Hash256 returns the SHA-256 canonical hash of v (normally a pointer to an
// SDK <Op>Input struct), with the exported top-level field named
// tokenField excluded so the token itself never affects its own key.
func Hash256(v any, tokenField string) ([32]byte, error) {
	h := sha256.New()
	if err := CanonicalHash(h, v, tokenField); err != nil {
		return [32]byte{}, err
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}

// CanonicalHash writes a deterministic, self-delimiting encoding of v into
// h. The encoding is stable under struct field reordering by Go tooling
// (fields are read via reflection in declaration order, but each is
// prefixed by its name), under map key insertion order (keys are sorted),
// and treats a nil pointer/slice/map identically to an empty one of the
// same field (an explicitly-constructed empty []string{} and an unset
// []string field hash the same).
//
// tokenField is the name of the top-level struct field to skip -- the
// token member itself, whose value must never influence the hash of its
// own request. Only the top-level occurrence of that name is skipped;
// nested fields with the same name (there are none in the generated SDK
// input types, but reflection cannot assume that) are hashed normally.
func CanonicalHash(h hash.Hash, v any, tokenField string) error {
	var vb [4]byte
	binary.BigEndian.PutUint32(vb[:], encodingVersion)
	if _, err := h.Write(vb[:]); err != nil {
		return err
	}
	enc := &canonEncoder{w: h, skipTopField: tokenField}
	return enc.encode(reflect.ValueOf(v), "$", true)
}

type canonEncoder struct {
	w            io.Writer
	skipTopField string
}

func (e *canonEncoder) tag(b byte) error {
	_, err := e.w.Write([]byte{b})
	return err
}

func (e *canonEncoder) uvarint(n uint64) error {
	var buf [binary.MaxVarintLen64]byte
	l := binary.PutUvarint(buf[:], n)
	_, err := e.w.Write(buf[:l])
	return err
}

// bytesField writes a length-prefixed byte string: a varint length,
// followed by the bytes. Used for both string values and, distinctly
// tagged, struct field names, so that e.g. a struct {A, B string} with
// A="x", B="" cannot be confused with {A: "", B: "x"} or with a struct
// that has a single field named "AB".
func (e *canonEncoder) bytesField(b []byte) error {
	if err := e.uvarint(uint64(len(b))); err != nil {
		return err
	}
	_, err := e.w.Write(b)
	return err
}

// encode writes v's canonical encoding. top is true only for the
// caller-supplied root value, so skipTopField applies to its immediate
// struct fields only, not to any nested struct that happens to reuse the
// same field name.
func (e *canonEncoder) encode(v reflect.Value, path string, top bool) error {
	if !v.IsValid() {
		return e.tag('N')
	}

	// A streaming request body (e.g. S3 PutObjectInput.Body io.Reader) is
	// checked against the *static* field type, before the
	// interface-unwrapping loop below would replace it with whatever
	// concrete reader happens to be behind it. Its content cannot be read
	// twice (or, for a non-seekable reader, read at all) without consuming
	// it, so it cannot be hashed deterministically; fail loudly here
	// rather than silently ignoring it as "nil".
	//
	// Smithy documents (arbitrary embedded JSON, typed
	// smithydocument.Marshaler / .Unmarshaler in a handful of services)
	// are deliberately not special-cased: aws-sdk-go-v2 represents an
	// unmarshaled document as an ordinary Go map/slice/scalar tree, which
	// the cases below already encode; if a caller's document value somehow
	// resolves to something reflection genuinely cannot walk (a func or
	// chan), the default case at the bottom of this function still catches
	// it.
	if v.Type().Implements(readerType) {
		return &ErrNotHashable{Path: path, Kind: v.Kind()}
	}

	for v.Kind() == reflect.Interface || v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return e.tag('N')
		}
		v = v.Elem()
	}

	if v.Type() == timeType {
		t := v.Interface().(time.Time) //nolint:forcetypeassert // guarded by the type equality check above
		if err := e.tag('T'); err != nil {
			return err
		}
		return e.bytesField([]byte(t.UTC().Format(time.RFC3339Nano)))
	}

	switch v.Kind() {
	case reflect.Bool:
		b := byte(0)
		if v.Bool() {
			b = 1
		}
		if err := e.tag('B'); err != nil {
			return err
		}
		return e.tag(b)

	case reflect.String:
		if v.Len() == 0 {
			return e.tag('N')
		}
		if err := e.tag('S'); err != nil {
			return err
		}
		return e.bytesField([]byte(v.String()))

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n := v.Int()
		if n == 0 {
			return e.tag('N')
		}
		if err := e.tag('I'); err != nil {
			return err
		}
		return e.uvarint(zigzag(n))

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		n := v.Uint()
		if n == 0 {
			return e.tag('N')
		}
		if err := e.tag('U'); err != nil {
			return err
		}
		return e.uvarint(n)

	case reflect.Float32, reflect.Float64:
		f := v.Float()
		if f == 0 {
			return e.tag('N')
		}
		if err := e.tag('F'); err != nil {
			return err
		}
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], math.Float64bits(f))
		_, err := e.w.Write(buf[:])
		return err

	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 { // []byte: encode as an opaque byte string, not an element list
			if v.Len() == 0 {
				return e.tag('N')
			}
			if err := e.tag('Y'); err != nil {
				return err
			}
			return e.bytesField(v.Bytes())
		}
		return e.encodeSequence(v, path)

	case reflect.Array:
		return e.encodeSequence(v, path)

	case reflect.Map:
		return e.encodeMap(v, path)

	case reflect.Struct:
		return e.encodeStruct(v, path, top)

	default: // Func, Chan, Complex64/128, UnsafePointer
		return &ErrNotHashable{Path: path, Kind: v.Kind()}
	}
}

func (e *canonEncoder) encodeSequence(v reflect.Value, path string) error {
	if v.Len() == 0 {
		return e.tag('N')
	}
	if err := e.tag('L'); err != nil {
		return err
	}
	if err := e.uvarint(uint64(v.Len())); err != nil {
		return err
	}
	for i := 0; i < v.Len(); i++ {
		if err := e.encode(v.Index(i), fmt.Sprintf("%s[%d]", path, i), false); err != nil {
			return err
		}
	}
	return nil
}

func (e *canonEncoder) encodeMap(v reflect.Value, path string) error {
	if v.Len() == 0 {
		return e.tag('N')
	}
	if v.Type().Key().Kind() != reflect.String {
		return &ErrNotHashable{Path: path, Kind: reflect.Map}
	}
	keys := v.MapKeys()
	ks := make([]string, len(keys))
	for i, k := range keys {
		ks[i] = k.String()
	}
	sort.Strings(ks)
	if err := e.tag('M'); err != nil {
		return err
	}
	if err := e.uvarint(uint64(len(ks))); err != nil {
		return err
	}
	for _, k := range ks {
		if err := e.bytesField([]byte(k)); err != nil {
			return err
		}
		if err := e.encode(v.MapIndex(reflect.ValueOf(k).Convert(v.Type().Key())), path+"."+k, false); err != nil {
			return err
		}
	}
	return nil
}

func (e *canonEncoder) encodeStruct(v reflect.Value, path string, top bool) error {
	t := v.Type()
	n := t.NumField()
	if err := e.tag('R'); err != nil {
		return err
	}
	if err := e.uvarint(uint64(n)); err != nil {
		return err
	}
	for i := 0; i < n; i++ {
		sf := t.Field(i)
		if err := e.bytesField([]byte(sf.Name)); err != nil {
			return err
		}
		switch {
		case !sf.IsExported():
			// Reflection cannot read an unexported field's value without a
			// panic (or unsafe, which we don't use). Encode a stable
			// placeholder rather than skip the field position entirely, so
			// two structs that differ only in an exported field appearing
			// before vs. after this one still hash differently. This does
			// mean unexported-field content never affects the hash; the
			// generated SDK <Op>Input types this package is meant to hash
			// have no unexported fields, so this path exists only as a
			// safe (non-panicking), documented fallback for nested types
			// that might.
			if err := e.tag('N'); err != nil {
				return err
			}
		case top && sf.Name == e.skipTopField:
			if err := e.tag('N'); err != nil {
				return err
			}
		default:
			if err := e.encode(v.Field(i), path+"."+sf.Name, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func zigzag(n int64) uint64 {
	return uint64((n << 1) ^ (n >> 63))
}
