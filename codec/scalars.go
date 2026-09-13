package codec

// The walk for a plan with nothing nested in it.
//
// `writePlan` and `readField` handle every field a type can hold, composites
// included. That generality is not free even for a type that has none: the
// composite cases call out of line and take a scratch buffer, so the switch
// keeps a frame and spills registers the scalar cases would otherwise hold, and
// the cost lands on every record whether or not it has a nested field in it.
//
// Most records do not. A plan with no struct, slice-of-struct, map or pointer
// field is flagged `simple` when it is built, and the entry points send it here
// instead — the same switch with the out-of-line arms removed, so it needs no
// scratch buffer and no frame.
//
// This is the same argument the two key widths make in `wire`: a case the
// compiler can see is absent is a case it can stop paying for.

import (
	"unsafe"

	"github.com/ivanjoz/colbin/wire"
)

// simplePlan reports whether every field is a scalar, a string or an array of
// those — which is what makes the walks below sufficient.
func (plan *typePlan) simplePlan() bool {
	for _, field := range plan.fields {
		switch field.op {
		case opStruct, opStructs, opMap, opPointer:
			return false
		}
	}
	return true
}

// appendScalars is writePlan without the composite arms.
func appendScalars(writer *wire.Writer, plan *typePlan, record unsafe.Pointer) {
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
		}
	}
}

// appendScalarsWide is appendScalars for eight-bit keys.
//
// The wide walk needed this more than the narrow one did, not less: it takes a
// scratch buffer *and* a writer that the composite arms make escape, so a flat
// wide record was paying an allocation for a transposition buffer it could never
// use. Without the arms, neither escapes.
func appendScalarsWide(writer *wire.Writer8, plan *typePlan, record unsafe.Pointer) {
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
		case opPointer:
			appendPointerWide(writer, field, at)
		}
	}
}

// readScalarsWide is readRun with readWideField's composite arms removed. An
// unknown key is still skipped, which is what the wide width is for.
func readScalarsWide(reader *wire.Reader8, plan *typePlan, record unsafe.Pointer) {
	for reader.More() {
		field := plan.find(reader.Key())
		if field == nil {
			if !reader.Skip() {
				return
			}
			continue
		}
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
		case opPointer:
			readPointerWide(reader, field, at)
		}
	}
}

// readScalars is unmarshalNarrow's loop with readField's composite arms removed.
func readScalars(reader *wire.Reader, plan *typePlan, record unsafe.Pointer) bool {
	for reader.More() {
		field := plan.find(reader.Key())
		if field == nil {
			return false
		}
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
			// PackedString reads a raw blob too — the header says which — so the
			// narrow decoder needs no setting and cannot be wrong about it.
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
		}
	}
	return true
}
