package codec

// Pointers to scalars, which is how a field says "absent" rather than "zero".
//
// # Absence is already the encoding
//
// A field holding its zero value is not written, so an absent key already means
// nothing was there. A nil pointer is exactly that and costs nothing: it is
// omitted, and the decoder leaves the field nil because it zeroes the record
// first.
//
// What needs saying out loud is the other case. A non-nil pointer *to* a zero
// value — `new(int32)`, a `*bool` to false — would be omitted by the same rule
// and read back as nil, which is a different value. So it writes an explicit
// zero: `Zero` for a number and `EmptyString` for a string. That is the one
// place in this format where a zero goes on the wire, and it is there because
// the alternative is losing a distinction the Go type makes.
//
// # Scalars here, structs next door
//
// This file is pointers to scalars. A pointer to a *struct* is carried too, but
// by opPointerStruct in composite.go, and it needs none of the machinery above:
// a struct body goes on the wire whether or not it is empty, so an absent key is
// already nil and a present key with an empty body is already a pointer to a
// zero value. The distinction the explicit-zero dance buys for a scalar comes
// free for a struct, and the wire is unchanged — a reader in another language
// sees a struct field that is present or absent, which it already handles.
//
// A pointer to a slice or a map is still refused. Those collapse the other way:
// a nil one and an empty one are the same thing on this wire, so telling them
// apart would need the SPECIAL null code and a decision about what `*[]T` nil
// means that nothing has asked for yet.

import (
	"fmt"
	"reflect"
	"unsafe"

	"github.com/ivanjoz/colbin/wire"
)

// pointerOpFor resolves a pointer-to-scalar field to the op of what it points
// at, or refuses it.
func pointerOpFor(fieldType reflect.Type) (fieldOp, error) {
	if fieldType.Kind() != reflect.Pointer {
		return 0, fmt.Errorf("not a pointer")
	}
	op, err := opFor(fieldType.Elem())
	if err != nil || !columnable(op) {
		return 0, fmt.Errorf("a pointer to %s is not carried", fieldType.Elem())
	}
	return op, nil
}

// isZeroScalar reports whether the pointee is what the omit-zero rule drops,
// which is what decides between the ordinary writer and the explicit-zero one.
//
// A float is compared by its bits, not its value: -0 equals 0 but is not
// omitted, because it does not encode as zero.
func isZeroScalar(op fieldOp, at unsafe.Pointer) bool {
	switch op {
	case opBool:
		return !*(*bool)(at)
	case opInt8, opUint8:
		return *(*uint8)(at) == 0
	case opInt16, opUint16:
		return *(*uint16)(at) == 0
	case opInt32, opUint32, opFloat32:
		return *(*uint32)(at) == 0
	case opInt64, opUint64, opFloat64:
		return *(*uint64)(at) == 0
	case opString:
		return len(*(*string)(at)) == 0
	}
	return false
}

// appendPointer writes a pointer field: nothing for nil, and the pointee
// otherwise — explicitly, when the pointee is zero.
func appendPointer(writer *wire.Writer, field *planField, at unsafe.Pointer, packed bool) {
	pointee := *(*unsafe.Pointer)(at)
	if pointee == nil {
		return
	}
	if isZeroScalar(field.elemOp, pointee) {
		switch {
		case field.elemOp == opString:
			writer.EmptyString(field.key)
		case signedOp(field.elemOp):
			writer.ZeroSigned(field.key)
		default:
			writer.Zero(field.key)
		}
		return
	}
	writeValue(writer, field.key, field.elemOp, pointee, packed)
}

// signedOp says the field is read back through Int rather than through Uint,
// which is what decides the shape an explicit zero has to take. Floats are not
// signed here: they ride in the unsigned field as a reversed bit pattern.
func signedOp(op fieldOp) bool {
	switch op {
	case opInt8, opInt16, opInt32, opInt64:
		return true
	}
	return false
}

func appendPointerWide(writer *wire.Writer8, field *planField, at unsafe.Pointer, packed bool) {
	pointee := *(*unsafe.Pointer)(at)
	if pointee == nil {
		return
	}
	if isZeroScalar(field.elemOp, pointee) {
		if field.elemOp == opString {
			writer.EmptyString(field.key)
		} else {
			writer.Zero(field.key)
		}
		return
	}
	writeValueWide(writer, field.key, field.elemOp, pointee, packed)
}

// readPointer allocates a pointee and reads into it. The record was zeroed
// first, so a key the message omits leaves the field nil with nothing to do.
func readPointer(reader *wire.Reader, field *planField, at unsafe.Pointer) {
	pointee := reflect.New(field.sliceType.Elem()).UnsafePointer()
	if !readValue(reader, field.elemOp, pointee) {
		reader.Fail(errNotValueOp(field.elemOp))
		return
	}
	*(*unsafe.Pointer)(at) = pointee
}

func readPointerWide(reader *wire.Reader8, field *planField, at unsafe.Pointer) {
	pointee := reflect.New(field.sliceType.Elem()).UnsafePointer()
	if !readValueWide(reader, field.elemOp, pointee) {
		reader.Fail(errNotValueOp(field.elemOp))
		return
	}
	*(*unsafe.Pointer)(at) = pointee
}
