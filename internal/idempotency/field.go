// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package idempotency

import "reflect"

// stringFieldValue returns the addressable reflect.Value of params's
// exported *string field named name, if params is a pointer to a struct
// and that field exists and has exactly that type. Every opTable entry's
// Field names an AWS SDK generated *string member -- the SDK's own
// generated types use this convention universally for optional string
// members, token fields included -- so this narrow check is intentional:
// it fails closed (returns ok=false) rather than trying to handle every
// possible field type, which callers treat as "nothing to do here" rather
// than an error.
func stringFieldValue(params any, name string) (fv reflect.Value, ok bool) {
	v := reflect.ValueOf(params)
	if v.Kind() != reflect.Ptr || v.IsNil() {
		return reflect.Value{}, false
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return reflect.Value{}, false
	}
	f := v.FieldByName(name)
	if !f.IsValid() || !f.CanSet() || f.Kind() != reflect.Ptr || f.Type().Elem().Kind() != reflect.String {
		return reflect.Value{}, false
	}
	return f, true
}

// readStringField reads the current value of a field located by
// stringFieldValue. isSet is false for a nil pointer.
func readStringField(fv reflect.Value) (value string, isSet bool) {
	if fv.IsNil() {
		return "", false
	}
	return fv.Elem().String(), true
}

// writeStringField sets the field located by stringFieldValue to a new
// pointer to s (AWS SDK types always take a *string, never share a caller
// -owned pointer, so this always allocates a fresh string rather than
// mutating through the existing pointer, which may be nil or may be a
// value the caller's own code still holds a reference to).
func writeStringField(fv reflect.Value, s string) {
	fv.Set(reflect.ValueOf(&s))
}
