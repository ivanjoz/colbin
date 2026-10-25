package codec

// The row program: a narrow plan compiled, once, into the walk its records take.
//
// writeFields interprets a plan: for every field of every record it loads the
// planField, switches on its op and calls a writer, and each call spills the
// buffer and every other live value, because Go's calling convention keeps
// nothing in registers across one. A generated codec has none of that — its
// fields are straight-line code at fixed offsets — and a hand-written walk of
// TPC-H's line items, the same bytes, measured a quarter faster.
//
// The program is the part of that a type's first encode can work out without
// generating code, so it needs nothing from the program that imports colbin:
//
//   - The ops are what the walk needs and nothing else, eight bytes each, with
//     the key already shifted into its nibble.
//   - The room every fixed-width field could take is known per type, so it is
//     reserved once per record and the fields store into it unchecked, where an
//     append checks the capacity of every one.
//   - The integer forms are spelled inline, and a string up to sixteen bytes is
//     copied with two moves that may overlap: no call for a header, a value or
//     a memmove.
//   - A field it does not spell — a map, a pointer, an array, anything behind a
//     pointer — goes through writeFields alone, as a plan of one field, so the
//     bytes are writeFields' whatever the type holds.

import (
	"encoding/binary"
	"math/bits"
	"slices"
	"unsafe"

	"github.com/ivanjoz/colbin/internal/wirefmt"
	"github.com/ivanjoz/colbin/wire"
)

// useRowPrograms is whether writePlan runs a plan's program. The tests turn it
// off to hold the program to writeFields' bytes.
var useRowPrograms = true

// The program's ops. The numeric ones are in two groups, signed and unsigned,
// which is all the integer forms tell apart.
const (
	rowInt8 uint8 = iota
	rowInt16
	rowInt32
	rowInt64
	rowUint8
	rowUint16
	rowUint32
	rowUint64
	rowFloat32
	rowFloat64
	rowBool
	rowString
	rowStruct
	rowStructs
	rowField // anything else, through writeFields
)

type rowOp struct {
	kind   uint8
	key    uint8 // the field's key: shifted into the high nibble when narrow
	field  uint16
	offset uint32
}

type rowProgram struct {
	ops []rowOp
	// reserve is the most the fixed-width ops can write, with the eight bytes a
	// magnitude's whole-word store runs past its last byte.
	reserve int
	plan    *typePlan
	// singles holds a one-field plan for each rowField op, by field index.
	singles []*typePlan
	// wide says the plan's keys are eight bits, so writeWide runs it.
	wide bool
}

// fixedRoom is the most a numeric field writes: a header, a length byte for a
// negative magnitude, and the magnitude stored as a whole word. A wide field's
// key and descriptor and the word are the same ten.
const fixedRoom = 10

// compileRows builds a plan's program, or nil when the plan has no layout or
// no field the program spells itself.
func compileRows(plan *typePlan) *rowProgram {
	if plan.fromSchema || len(plan.fields) > 1<<16 {
		return nil
	}
	program := &rowProgram{plan: plan, reserve: 8, wide: plan.isWide}
	spelled := false
	for index := range plan.fields {
		field := &plan.fields[index]
		op := rowOp{key: field.key << 4, field: uint16(index)}
		if program.wide {
			op.key = field.key
		}
		if field.offset > 1<<32-1 {
			return nil
		}
		op.offset = uint32(field.offset)
		kind, ok := rowKind(field)
		if !ok {
			kind = rowField
			if program.singles == nil {
				program.singles = make([]*typePlan, len(plan.fields))
			}
			program.singles[index] = &typePlan{fields: plan.fields[index : index+1 : index+1]}
		} else if kind != rowStruct && kind != rowStructs {
			spelled = true
		}
		op.kind = kind
		if kind <= rowBool {
			program.reserve += fixedRoom
		}
		program.ops = append(program.ops, op)
	}
	if !spelled {
		return nil
	}
	return program
}

// rowKind is the op a field compiles to, if the program spells it.
func rowKind(field *planField) (uint8, bool) {
	if field.indirect {
		return 0, false
	}
	switch field.op {
	case opBool:
		return rowBool, true
	case opInt8:
		return rowInt8, true
	case opInt16:
		return rowInt16, true
	case opInt32:
		return rowInt32, true
	case opInt64:
		return rowInt64, true
	case opUint8:
		return rowUint8, true
	case opUint16:
		return rowUint16, true
	case opUint32:
		return rowUint32, true
	case opUint64:
		return rowUint64, true
	case opFloat32:
		return rowFloat32, true
	case opFloat64:
		return rowFloat64, true
	case opString:
		return rowString, true
	case opStruct:
		return rowStruct, true
	case opStructs:
		return rowStructs, true
	}
	return 0, false
}

// write is writeFields for the program's plan, with packed strings off.
func (program *rowProgram) write(writer *wire.Writer, record unsafe.Pointer, buf *scratch) {
	buffer := slices.Grow(writer.Buffer, program.reserve)
	at := len(buffer)
	out := unsafe.Pointer(unsafe.SliceData(buffer))
	for index := 0; index < len(program.ops); {
		if useRowKernel {
			// The kernel takes every op it spells from here, and leaves this
			// loop the one it stopped at.
			done, end := rowKernel(&program.ops[index], len(program.ops)-index, record,
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
			// The key, counted only when true: a bool is 0 or 1.
			*(*byte)(unsafe.Add(out, at)) = op.key
			at += int(*(*uint8)(from))
			continue
		case rowString:
			value := *(*string)(from)
			size := len(value)
			if size == 0 {
				continue
			}
			if size > 0xFFFF {
				// The four-byte length form, and the copy of a value that size,
				// are wirefmt's.
				buffer = wirefmt.AppendString(buffer[:at], op.key>>4, value)
				buffer = slices.Grow(buffer, program.reserve)
				at, out = len(buffer), unsafe.Pointer(unsafe.SliceData(buffer))
				continue
			}
			// Room for the header, a three-byte length and the value, and the
			// fixed-width fields still to come.
			if cap(buffer)-at < 4+size+program.reserve {
				buffer = slices.Grow(buffer[:at], 4+size+program.reserve)
				out = unsafe.Pointer(unsafe.SliceData(buffer))
			}
			head := unsafe.Add(out, at)
			if size <= wirefmt.MaxSized {
				*(*byte)(head) = op.key | uint8(size+wirefmt.SizedBias)
				at++
			} else if size <= wirefmt.InlineLength {
				*(*[2]byte)(head) = [2]byte{op.key | wirefmt.NibbleLength | wirefmt.FlagRaw, uint8(size)}
				at += 2
			} else {
				*(*[4]byte)(head) = [4]byte{op.key | wirefmt.NibbleLength | wirefmt.FlagRaw,
					wirefmt.Length16, uint8(size), uint8(size >> 8)}
				at += 4
			}
			copyShort(unsafe.Add(out, at), unsafe.StringData(value), size)
			at += size
			continue
		default:
			// A composite, or a field the program does not spell: through the
			// writer, and the room reserved again after it.
			writer.Buffer = buffer[:at]
			field := &program.plan.fields[op.field]
			switch op.kind {
			case rowStruct:
				appendNarrowStruct(writer, field, from, buf)
			case rowStructs:
				appendNarrowStructs(writer, field, from, buf)
			default:
				writeFields(writer, program.singles[op.field], record, buf)
			}
			buffer = slices.Grow(writer.Buffer, program.reserve)
			at, out = len(buffer), unsafe.Pointer(unsafe.SliceData(buffer))
			continue
		}

		// The integer forms: see wirefmt.AppendInt and AppendUint. A value
		// that fits the nibble is the nibble; zero is nothing; −1 has a nibble
		// of its own and any other negative is its magnitude in the length
		// form; the rest is its fewest bytes behind a nibble that counts them,
		// stored as a whole word that the next field lands over.
		head := unsafe.Add(out, at)
		if signed {
			if int64(value) < 0 {
				if int64(value) == -1 {
					*(*byte)(head) = op.key | wirefmt.SignedMinusOne
					at++
					continue
				}
				magnitude := -value
				width := (bits.Len64(magnitude) + 7) >> 3
				*(*[2]byte)(head) = [2]byte{op.key | wirefmt.NibbleLength, uint8(width)}
				binary.LittleEndian.PutUint64(unsafe.Slice((*byte)(unsafe.Add(head, 2)), 8), magnitude)
				at += 2 + width
				continue
			}
			if value-1 < wirefmt.SignedInline {
				*(*byte)(head) = op.key | uint8(value-1)
				at++
				continue
			}
		} else if value-1 < wirefmt.InlineValues {
			*(*byte)(head) = op.key | uint8(value-1)
			at++
			continue
		}
		if value == 0 {
			continue
		}
		width := (bits.Len64(value) + 7) >> 3
		*(*byte)(head) = op.key | uint8(width+wirefmt.SizedBias)
		binary.LittleEndian.PutUint64(unsafe.Slice((*byte)(unsafe.Add(head, 1)), 8), value)
		at += 1 + width
	}
	writer.Buffer = buffer[:at]
}

// copyShort copies size bytes, at least one, from source to dst. Up to sixteen
// it is two moves that may overlap, the first from the start and the second
// ending at the end, so nothing outside the value is read or written; past
// that it is copy's memmove.
func copyShort(dst unsafe.Pointer, source *byte, size int) {
	from := unsafe.Pointer(source)
	switch {
	case size > 16:
		copy(unsafe.Slice((*byte)(dst), size), unsafe.Slice(source, size))
	case size >= 8:
		head := binary.LittleEndian.Uint64(unsafe.Slice(source, 8))
		tail := binary.LittleEndian.Uint64(unsafe.Slice((*byte)(unsafe.Add(from, size-8)), 8))
		binary.LittleEndian.PutUint64(unsafe.Slice((*byte)(dst), 8), head)
		binary.LittleEndian.PutUint64(unsafe.Slice((*byte)(unsafe.Add(dst, size-8)), 8), tail)
	case size >= 4:
		head := binary.LittleEndian.Uint32(unsafe.Slice(source, 4))
		tail := binary.LittleEndian.Uint32(unsafe.Slice((*byte)(unsafe.Add(from, size-4)), 4))
		binary.LittleEndian.PutUint32(unsafe.Slice((*byte)(dst), 4), head)
		binary.LittleEndian.PutUint32(unsafe.Slice((*byte)(unsafe.Add(dst, size-4)), 4), tail)
	default:
		// One to three bytes: the first, the middle and the last.
		*(*byte)(dst) = *source
		*(*byte)(unsafe.Add(dst, size>>1)) = *(*byte)(unsafe.Add(from, size>>1))
		*(*byte)(unsafe.Add(dst, size-1)) = *(*byte)(unsafe.Add(from, size-1))
	}
}
