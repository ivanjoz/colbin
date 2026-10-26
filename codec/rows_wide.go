package codec

// The row program at eight key bits: writeFieldsWide's bytes, the way write
// has writeFields'. A type with no tags takes its keys from its field names, so
// this is the walk every struct gets that was declared with nothing for colbin.

import (
	"encoding/binary"
	"math/bits"
	"slices"
	"unsafe"

	"github.com/ivanjoz/colbin/wire"
)

// The wide descriptors the program writes. See wire/wide.go: an explicit
// descriptor is 0x80 | class<<4 | detail.
const (
	wideMaxInline   = 0x7F // a value up to this is the descriptor itself
	wideIntPositive = 0x88 // classInt, the positive flag; | the size code
	wideIntNegative = 0x80 // classInt; | the size code
	wideVarint      = 0xF8 // classSpecial, the varint; | the value's low three bits
	wideBlob        = 0x90 // classBlob; | the length code
	wideShort       = 0x08 // | wideBlob, a blob of up to wideShortMax bytes; | its size less one
	wideShortMax    = 8
	wideTrue        = 1 // a bool is the inline value 1
)

// writeWide is writeFieldsWide for the program's plan, with packed strings off.
func (program *rowProgram) writeWide(writer *wire.Writer8, record unsafe.Pointer, buf *scratch) {
	buffer := slices.Grow(writer.Buffer, program.reserve)
	at := len(buffer)
	out := unsafe.Pointer(unsafe.SliceData(buffer))
	for index := 0; index < len(program.ops); {
		if useRowKernel {
			done, end := rowKernelWide(&program.ops[index], len(program.ops)-index, record,
				(*byte)(out), at, cap(buffer), program.reserve)
			index, at = index+done, end
			if index == len(program.ops) {
				break
			}
		}
		op := &program.ops[index]
		index++
		from := unsafe.Add(record, op.offset)
		var value uint64
		signed := false
		switch op.kind {
		case rowInt8:
			value, signed = uint64(int64(*(*int8)(from))), true
		case rowInt16:
			value, signed = uint64(int64(*(*int16)(from))), true
		case rowInt32:
			value, signed = uint64(int64(*(*int32)(from))), true
		case rowInt64:
			value, signed = uint64(*(*int64)(from)), true
		case rowUint8:
			value = uint64(*(*uint8)(from))
		case rowUint16:
			value = uint64(*(*uint16)(from))
		case rowUint32:
			value = uint64(*(*uint32)(from))
		case rowUint64:
			value = *(*uint64)(from)
		case rowFloat32:
			value = uint64(bits.ReverseBytes32(*(*uint32)(from)))
		case rowFloat64:
			value = bits.ReverseBytes64(*(*uint64)(from))
		case rowBool:
			// The key and 1, counted only when true: a bool is 0 or 1.
			*(*[2]byte)(unsafe.Add(out, at)) = [2]byte{op.key, wideTrue}
			at += 2 * int(*(*uint8)(from))
			continue
		case rowString:
			value := *(*string)(from)
			size := len(value)
			if size == 0 {
				continue
			}
			// The key, the descriptor, a length of up to eight bytes stored
			// as a whole word, the value, and the fixed-width fields still to
			// come.
			if cap(buffer)-at < 10+size+program.reserve {
				buffer = slices.Grow(buffer[:at], 10+size+program.reserve)
				out = unsafe.Pointer(unsafe.SliceData(buffer))
			}
			head := unsafe.Add(out, at)
			code, width := uint8(0), 1
			switch {
			case size <= wideShortMax:
				// Up to eight bytes, the descriptor is the length.
				code, width = wideShort|uint8(size-1), 0
			case uint64(size) > 0xFFFF_FFFF:
				code, width = 3, 8
			case size > 0xFFFF:
				code, width = 2, 4
			case size > 0xFF:
				code, width = 1, 2
			}
			*(*[2]byte)(head) = [2]byte{op.key, wideBlob | code}
			binary.LittleEndian.PutUint64(unsafe.Slice((*byte)(unsafe.Add(head, 2)), 8), uint64(size))
			at += 2 + width
			copyShort(unsafe.Add(out, at), unsafe.StringData(value), size)
			at += size
			continue
		case rowPointer:
			if *(*unsafe.Pointer)(from) == nil {
				continue
			}
			writer.Buffer = buffer[:at]
			appendPointerWide(writer, &program.plan.fields[op.field], from, false)
			buffer = slices.Grow(writer.Buffer, program.reserve)
			at, out = len(buffer), unsafe.Pointer(unsafe.SliceData(buffer))
			continue
		default:
			writer.Buffer = buffer[:at]
			field := &program.plan.fields[op.field]
			switch op.kind {
			case rowStruct:
				appendStructField(writer, field, from, buf)
			case rowStructs:
				appendStructsField(writer, field, from, buf)
			default:
				// writeValueWide where it can: see write.
				if field.indirect || !writeValueWide(writer, field.key, field.op, from, false) {
					writeFieldsWide(writer, program.singles[op.field], record, buf)
				}
			}
			buffer = slices.Grow(writer.Buffer, program.reserve)
			at, out = len(buffer), unsafe.Pointer(unsafe.SliceData(buffer))
			continue
		}

		// See wire's Writer8.Int and Uint: zero is nothing, up to 127 is the
		// descriptor itself, and past that whichever of two forms is shorter —
		// a varint, of the zigzagged value when signed, or a sign and the
		// magnitude's byte count with the magnitude after.
		head := unsafe.Add(out, at)
		if value-1 < wideMaxInline {
			*(*[2]byte)(head) = [2]byte{op.key, uint8(value)}
			at += 2
			continue
		}
		if value == 0 {
			continue
		}
		magnitude, descriptor, varint := value, uint8(wideIntPositive), value
		if signed {
			varint = value<<1 ^ uint64(int64(value)>>63)
			if int64(value) < 0 {
				magnitude, descriptor = -value, wideIntNegative
			}
		}
		width := (bits.Len64(magnitude) + 7) >> 3
		code := uint8(width)
		if width >= 7 {
			code, width = 7, 8
		}
		// The varint's continuation bytes: at least one, seven bits each, of
		// what is left past the descriptor's three.
		rest := varint >> 3
		more := max(1, (bits.Len64(rest)+6)/7)
		if more < width {
			*(*[2]byte)(head) = [2]byte{op.key, wideVarint | uint8(varint&0b111)}
			at += 2
			for ; rest >= 0x80; rest >>= 7 {
				*(*byte)(unsafe.Add(out, at)) = uint8(rest) | 0x80
				at++
			}
			*(*byte)(unsafe.Add(out, at)) = uint8(rest)
			at++
			continue
		}
		*(*[2]byte)(head) = [2]byte{op.key, descriptor | code}
		binary.LittleEndian.PutUint64(unsafe.Slice((*byte)(unsafe.Add(head, 2)), 8), magnitude)
		at += 2 + width
	}
	writer.Buffer = buffer[:at]
}
