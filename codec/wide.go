package codec

// The wide-key path, and the root descriptor that says which path a message
// took.
//
// A message is one value: a descriptor byte, then its payload. The root
// descriptor names the class — always a struct, here — and the key width used
// inside it. Everything the old format's four version bytes carried is either in
// that byte or gone: the mode bit, because there is one format; the shape bits,
// because the class says it; the omit-empty flag, because omission is
// unconditional; ALL_POSITIVE, because sign is a bit of each integer's own
// descriptor.
//
// # Choosing the width
//
// Narrow keys are the default and the fast path, though what that now means is
// the read and the byte count rather than the write: on a ten-field record the
// two widths encode in 5.3 ns and 5.4, and narrow decodes in 14.8 against 25.4
// and writes 9 bytes against 11. A type goes wide when it has to:
//
//   - a field id above fifteen, which four key bits cannot carry; or
//   - packed5 on and a string field to spend it on, because the encoding code
//     lives in the wide descriptor and a narrow one has no room for it.
//
// The choice is a property of the type and the process setting, not of the
// values, so it is resolved once per type and then read from the plan.

import (
	"fmt"
	"reflect"
	"unsafe"

	"github.com/ivanjoz/colbin/wire"
)

// Root descriptors, from BYTE_ALIGNED_PLAN.md §2.1. They are the STRUCT class of
// an ordinary K8 descriptor, which is what puts every colbin message in
// 0xD0..0xDF — see root.go for the reservation that follows from it, and for the
// 240 first bytes an application may claim.
const (
	rootStructNarrow byte = RootFirst
	rootStructWide   byte = RootFirst | rootWide
	// The same two with a schema section in front of the body, which is what
	// MarshalSelfDescribing writes. See schema.go.
	rootStructNarrowSchema byte = RootFirst | rootSchema
	rootStructWideSchema   byte = RootFirst | rootWide | rootSchema
)

// wide reports whether a plan must use eight-bit keys.
//
// packed5 used to force it. A narrow blob header has no enc field, so a packed
// string had nowhere on the wire to say it was packed, and the whole message
// paid a byte per key to get one. It says so in the blob header's escape code
// now (wire.Writer.PackedString), so the encoding costs what it weighs and
// nothing else.
func (plan *typePlan) wide() bool {
	return plan.anyKeyPastNarrow || plan.derivedKeys
}

// appendWide is writePlan for the wide key width. It is a separate function
// rather than a flag inside one, for the reason wire keeps the two widths in
// separate files: a width the compiler cannot see is a width it cannot fold, and
// that measured 11.7 ns against 8.7 on a ten-field record.
func appendWide(writer *wire.Writer8, plan *typePlan, record unsafe.Pointer, buf *scratch) {
	packed := Packed5()
	for index := range plan.fields {
		field := &plan.fields[index]
		at := unsafe.Add(record, field.offset)
		switch field.op {
		case opBool:
			writer.Bool(field.key, *(*bool)(at))
		case opInt8:
			writer.Int(field.key, int64(*(*int8)(at)))
		case opInt16:
			writer.Int(field.key, int64(*(*int16)(at)))
		case opInt32:
			writer.I32(field.key, *(*int32)(at))
		case opInt64:
			writer.Int(field.key, *(*int64)(at))
		case opUint8:
			writer.U16(field.key, uint16(*(*uint8)(at)))
		case opUint16:
			writer.U16(field.key, *(*uint16)(at))
		case opUint32:
			writer.U32(field.key, *(*uint32)(at))
		case opUint64:
			writer.Uint(field.key, *(*uint64)(at))
		case opFloat32:
			writer.F32(field.key, *(*float32)(at))
		case opFloat64:
			writer.F64(field.key, *(*float64)(at))
		case opString:
			if packed {
				writer.PackedString(field.key, *(*string)(at))
			} else {
				writer.String(field.key, *(*string)(at))
			}
		case opBytes:
			writer.Bytes(field.key, *(*[]byte)(at))
		case opInt8s:
			writer.Int8s(field.key, *(*[]int8)(at))
		case opInt16s:
			writer.Int16s(field.key, *(*[]int16)(at))
		case opInt32s:
			writer.Int32s(field.key, *(*[]int32)(at))
		case opInt64s:
			writer.Ints(field.key, *(*[]int64)(at))
		case opUint16s:
			writer.Uint16s(field.key, *(*[]uint16)(at))
		case opUint32s:
			writer.Uint32s(field.key, *(*[]uint32)(at))
		case opUint64s:
			writer.Uint64s(field.key, *(*[]uint64)(at))
		case opStrings:
			writer.Strings(field.key, *(*[]string)(at))
		case opStruct:
			appendStructField(writer, field, at, buf)
		case opStructs:
			appendStructsField(writer, field, at, buf)
		case opMap:
			appendMapField(writer, field, at)
		case opPointer:
			appendPointerWide(writer, field, at)
		}
	}
}

// readWideField is readField for the wide key width.
func readWideField(reader *wire.Reader8, field *planField, record unsafe.Pointer, buf *scratch) {
	at := unsafe.Add(record, field.offset)
	switch field.op {
	case opBool:
		*(*bool)(at) = reader.Bool()
	case opInt8:
		*(*int8)(at) = int8(reader.Int())
	case opInt16:
		*(*int16)(at) = int16(reader.Int())
	case opInt32:
		*(*int32)(at) = reader.I32()
	case opInt64:
		*(*int64)(at) = reader.Int()
	case opUint8:
		*(*uint8)(at) = uint8(reader.U16())
	case opUint16:
		*(*uint16)(at) = reader.U16()
	case opUint32:
		*(*uint32)(at) = reader.U32()
	case opUint64:
		*(*uint64)(at) = reader.Uint()
	case opFloat32:
		*(*float32)(at) = reader.F32()
	case opFloat64:
		*(*float64)(at) = reader.F64()
	case opString:
		// PackedString reads either encoding: the descriptor says which, so a
		// message written with packed5 on reads back with it off.
		*(*string)(at) = reader.PackedString()
	case opBytes:
		*(*[]byte)(at) = append([]byte(nil), reader.Bytes()...)
	case opInt8s:
		*(*[]int8)(at) = reader.Int8s(nil)
	case opInt16s:
		*(*[]int16)(at) = reader.Int16s(nil)
	case opInt32s:
		*(*[]int32)(at) = reader.Int32s(nil)
	case opInt64s:
		*(*[]int64)(at) = reader.Ints(nil)
	case opUint16s:
		*(*[]uint16)(at) = reader.Uint16s(nil)
	case opUint32s:
		*(*[]uint32)(at) = reader.Uint32s(nil)
	case opUint64s:
		*(*[]uint64)(at) = reader.Uint64s(nil)
	case opStrings:
		*(*[]string)(at) = reader.Strings(nil)
	case opStruct:
		readStructField(reader, field, at, buf)
	case opStructs:
		readStructsField(reader, field, at, buf)
	case opMap:
		readMapField(reader, field, at)
	case opPointer:
		readPointerWide(reader, field, at)
	}
}

// unmarshalWide decodes a wide-key message into an already-zeroed record.
//
// A key the plan does not declare is *skipped* rather than refused, which is the
// whole point of the wide width: the descriptor sizes the field, so a reader can
// step over something a newer peer added. The narrow path cannot, and says so.
func unmarshalWide(body []byte, plan *typePlan, record unsafe.Pointer, what reflect.Type) error {
	// A separate reader per branch, for the reason appendPlan declares a separate
	// writer: escape analysis is per variable, and sharing one with the composite
	// walk would heap it on the flat path too.
	if plan.simple {
		reader := wire.NewReader8(body)
		readScalarsWide(&reader, plan, record)
		if err := reader.Err(); err != nil {
			return fmt.Errorf("colbin: %s: %w", what, err)
		}
		return nil
	}
	reader := wire.NewReader8(body)
	var buf scratch
	readRun(&reader, plan, record, &buf)
	if err := reader.Err(); err != nil {
		return fmt.Errorf("colbin: %s: %w", what, err)
	}
	return nil
}

// rootOf reads a message's root descriptor and returns the body after it.
//
// It reports failure as a bool and leaves the message to the caller, rather than
// taking the value to name it in an error. Taking it as an `any` boxed the
// record on every decode — one allocation and about 8 ns on a ten-field one,
// entirely to describe a failure that does not happen.
func rootOf(data []byte) (body []byte, wide, ok bool) {
	if len(data) == 0 {
		return nil, false, false
	}
	switch data[0] {
	case rootStructNarrow:
		return data[1:], false, true
	case rootStructWide:
		return data[1:], true, true
	case rootStructNarrowSchema, rootStructWideSchema:
		// A typed decode does not need the section — it has the Go type — so it
		// steps over it and reads the body behind. That is what keeps a
		// self-describing message an ordinary message to everyone else.
		_, body, ok := splitSchemaSection(data[1:])
		return body, data[0]&rootWide != 0, ok
	default:
		return nil, false, false
	}
}

// errBadRoot names what rootOf refused. It is out of line so that the happy path
// carries neither the formatting nor the type it would need.
func errBadRoot(data []byte, what reflect.Type) error {
	if len(data) == 0 {
		return fmt.Errorf("colbin: %s: empty message", what)
	}
	return fmt.Errorf(
		"colbin: %s: byte 0 is %#02x, which is not a root descriptor this version writes",
		what, data[0])
}

// structTypeOf resolves the struct type behind a value or a pointer to one.
func structTypeOf(v any) (reflect.Type, error) {
	structType := reflect.TypeOf(v)
	for structType != nil && structType.Kind() == reflect.Pointer {
		structType = structType.Elem()
	}
	if structType == nil {
		return nil, fmt.Errorf("colbin: expected a struct, got nil")
	}
	return structType, nil
}
