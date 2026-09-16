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
	if err != nil {
		return 0, fmt.Errorf("a pointer to %s is not carried", fieldType.Elem())
	}
	if !columnable(op) && op != opString {
		return 0, fmt.Errorf("a pointer to %s is not carried", fieldType.Elem())
	}
	return op, nil
}

// isZeroScalar reports whether the pointee is its type's zero value, which is
// what decides between the ordinary writer and the explicit-zero one.
func isZeroScalar(op fieldOp, at unsafe.Pointer) bool {
	switch op {
	case opBool:
		return !*(*bool)(at)
	case opInt8:
		return *(*int8)(at) == 0
	case opInt16:
		return *(*int16)(at) == 0
	case opInt32:
		return *(*int32)(at) == 0
	case opInt64:
		return *(*int64)(at) == 0
	case opUint8:
		return *(*uint8)(at) == 0
	case opUint16:
		return *(*uint16)(at) == 0
	case opUint32:
		return *(*uint32)(at) == 0
	case opUint64:
		return *(*uint64)(at) == 0
	case opFloat32:
		return *(*float32)(at) == 0
	case opFloat64:
		return *(*float64)(at) == 0
	case opString:
		return len(*(*string)(at)) == 0
	}
	return false
}

// appendPointer writes a pointer field: nothing for nil, and the pointee
// otherwise — explicitly, when the pointee is zero.
func appendPointer(writer *wire.Writer, field *planField, at unsafe.Pointer) {
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
	writeScalar(writer, field.key, field.elemOp, pointee)
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

func appendPointerWide(writer *wire.Writer8, field *planField, at unsafe.Pointer) {
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
	writeScalarWide(writer, field.key, field.elemOp, pointee)
}

// readPointer allocates a pointee and reads into it. The record was zeroed
// first, so a key the message omits leaves the field nil with nothing to do.
func readPointer(reader *wire.Reader, field *planField, at unsafe.Pointer) {
	value := reflect.New(field.sliceType.Elem())
	pointee := value.UnsafePointer()
	readScalar(reader, field.elemOp, pointee)
	*(*unsafe.Pointer)(at) = pointee
}

func readPointerWide(reader *wire.Reader8, field *planField, at unsafe.Pointer) {
	value := reflect.New(field.sliceType.Elem())
	pointee := value.UnsafePointer()
	readScalarWide(reader, field.elemOp, pointee)
	*(*unsafe.Pointer)(at) = pointee
}

// The scalar dispatch, standalone.
//
// The plan walkers have this switch inline, where the op is loaded once and the
// call is direct; these are for the paths that reach one scalar at a time — a
// pointer's pointee — and are not on the record-per-field path.

func writeScalar(w *wire.Writer, key uint8, op fieldOp, at unsafe.Pointer) {
	switch op {
	case opBool:
		w.Bool(key, *(*bool)(at))
	case opInt8:
		w.Int(key, int64(*(*int8)(at)))
	case opInt16:
		w.Int(key, int64(*(*int16)(at)))
	case opInt32:
		w.I32(key, *(*int32)(at))
	case opInt64:
		w.Int(key, *(*int64)(at))
	case opUint8:
		w.U16(key, uint16(*(*uint8)(at)))
	case opUint16:
		w.U16(key, *(*uint16)(at))
	case opUint32:
		w.U32(key, *(*uint32)(at))
	case opUint64:
		w.Uint(key, *(*uint64)(at))
	case opFloat32:
		w.F32(key, *(*float32)(at))
	case opFloat64:
		w.F64(key, *(*float64)(at))
	case opString:
		w.String(key, *(*string)(at))
	}
}

func writeScalarWide(w *wire.Writer8, key uint8, op fieldOp, at unsafe.Pointer) {
	switch op {
	case opBool:
		w.Bool(key, *(*bool)(at))
	case opInt8:
		w.Int(key, int64(*(*int8)(at)))
	case opInt16:
		w.Int(key, int64(*(*int16)(at)))
	case opInt32:
		w.I32(key, *(*int32)(at))
	case opInt64:
		w.Int(key, *(*int64)(at))
	case opUint8:
		w.U16(key, uint16(*(*uint8)(at)))
	case opUint16:
		w.U16(key, *(*uint16)(at))
	case opUint32:
		w.U32(key, *(*uint32)(at))
	case opUint64:
		w.Uint(key, *(*uint64)(at))
	case opFloat32:
		w.F32(key, *(*float32)(at))
	case opFloat64:
		w.F64(key, *(*float64)(at))
	case opString:
		// A *string packs like a string: the encoding is in the descriptor, so
		// the reader below takes either form.
		if Packed5() {
			w.PackedString(key, *(*string)(at))
		} else {
			w.String(key, *(*string)(at))
		}
	}
}

func readScalar(r *wire.Reader, op fieldOp, at unsafe.Pointer) {
	switch op {
	case opBool:
		*(*bool)(at) = r.Bool()
	case opInt8:
		*(*int8)(at) = int8(r.Int())
	case opInt16:
		*(*int16)(at) = int16(r.Int())
	case opInt32:
		*(*int32)(at) = r.I32()
	case opInt64:
		*(*int64)(at) = r.Int()
	case opUint8:
		*(*uint8)(at) = uint8(r.U16())
	case opUint16:
		*(*uint16)(at) = r.U16()
	case opUint32:
		*(*uint32)(at) = r.U32()
	case opUint64:
		*(*uint64)(at) = r.Uint()
	case opFloat32:
		*(*float32)(at) = r.F32()
	case opFloat64:
		*(*float64)(at) = r.F64()
	case opString:
		*(*string)(at) = r.String()
	}
}

// readScalarWide reads a string through PackedString, which takes either
// encoding: the descriptor says which, so a message written with packed5 on
// reads back with it off.
func readScalarWide(r *wire.Reader8, op fieldOp, at unsafe.Pointer) {
	if op == opString {
		*(*string)(at) = r.PackedString()
		return
	}
	switch op {
	case opBool:
		*(*bool)(at) = r.Bool()
	case opInt8:
		*(*int8)(at) = int8(r.Int())
	case opInt16:
		*(*int16)(at) = int16(r.Int())
	case opInt32:
		*(*int32)(at) = r.I32()
	case opInt64:
		*(*int64)(at) = r.Int()
	case opUint8:
		*(*uint8)(at) = uint8(r.U16())
	case opUint16:
		*(*uint16)(at) = r.U16()
	case opUint32:
		*(*uint32)(at) = r.U32()
	case opUint64:
		*(*uint64)(at) = r.Uint()
	case opFloat32:
		*(*float32)(at) = r.F32()
	case opFloat64:
		*(*float64)(at) = r.F64()
	case opString:
		*(*string)(at) = r.String()
	}
}
