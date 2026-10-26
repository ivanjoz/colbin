package codec

// The row program read back: readNarrowRun's walk over a struct body, for the
// fields whose form it can read without asking wire.Reader.
//
// It reads what the encoder writes for a field of each type — a nibble, or a
// magnitude of up to eight bytes behind one, or a raw string of a length byte
// or less — and only when the value fits the field. Everything else, from a
// negative integer to a field it has no op for to a body that is cut short,
// is the generic walk's: at the first such field the rest of the body goes to
// readNarrowFields, which reads it, and fails it, exactly as it would have.

import (
	"encoding/binary"
	"math"
	"math/bits"
	"slices"
	"unsafe"

	"github.com/ivanjoz/colbin/internal/wirefmt"
	"github.com/ivanjoz/colbin/wire"
)

// read fills record from a narrow body, at the depth the caller has taken, and
// returns the error the generic walk would have failed the body with.
func (program *rowProgram) read(body []byte, record unsafe.Pointer, buf *scratch) error {
	plan := program.plan
	ops := program.ops
	next := 0
	at := 0
	for at < len(body) {
		header := body[at]
		key := header & 0xF0
		index := next
		if index >= len(ops) || ops[index].key != key {
			if index = plan.findIndex(header >> 4); index < 0 {
				break // a key the plan does not have: the generic walk skips it
			}
		}
		op := &ops[index]
		if op.kind > rowString {
			break
		}
		to := unsafe.Add(record, op.offset)
		nibble := header & 0x0F

		if op.kind == rowString {
			start, size := at+1, int(nibble)-wirefmt.SizedBias
			if nibble == wirefmt.NibbleLength|wirefmt.FlagRaw && at+1 < len(body) && body[at+1] <= wirefmt.InlineLength {
				start, size = at+2, int(body[at+1])
			} else if size <= 0 || size > wirefmt.MaxSized {
				break
			}
			if size > len(body)-start {
				break
			}
			*(*string)(to) = buf.strings.String(body[start : start+size])
			at, next = start+size, index+1
			continue
		}

		// An integer, or a float or a bool in the unsigned shape: the nibble's
		// value, or the magnitude whose byte count the nibble is.
		var value uint64
		size := 1
		signed := op.kind <= rowInt64
		switch {
		case !signed && nibble < wirefmt.InlineValues, signed && nibble < wirefmt.SignedInline:
			value = uint64(nibble) + 1
		case signed && nibble == wirefmt.SignedMinusOne:
			value = math.MaxUint64 // −1
		case nibble >= wirefmt.NibbleSized && nibble < wirefmt.NibbleLength:
			width := int(nibble) - wirefmt.SizedBias
			if width > len(body)-at-1 {
				size = 0
			} else if value = readMagnitude(body, at+1, width); signed && value > math.MaxInt64 {
				size = 0
			} else {
				size = 1 + width
			}
		default:
			size = 0
		}
		if size == 0 || !storeNumber(op.kind, to, value) {
			break
		}
		at, next = at+size, index+1
	}
	if at < len(body) {
		rest := wire.NewReader(body[at:])
		readNarrowFields(&rest, plan, record, buf)
		return rest.Err()
	}
	return nil
}

// wideWidths is the byte count of a wide integer's magnitude by its size code:
// none for the value 1, and eight for the code seven.
var wideWidths = [8]int{0, 1, 2, 3, 4, 5, 6, 8}

// readWide is read at eight key bits: readRun's walk, for the forms wire's
// Writer8 gives a field of each type — the value in the descriptor, a sign and
// a magnitude, a varint, or a raw string — handing the rest of the body to
// readWideFields at the first field it does not read.
func (program *rowProgram) readWide(body []byte, record unsafe.Pointer, buf *scratch) error {
	plan := program.plan
	ops := program.ops
	next := 0
	at := 0
	for at+2 <= len(body) {
		key, desc := body[at], body[at+1]
		index := next
		if index >= len(ops) || ops[index].key != key {
			if index = plan.findIndex(key); index < 0 {
				break
			}
		}
		op := &ops[index]
		if op.kind > rowString {
			break
		}
		to := unsafe.Add(record, op.offset)

		if op.kind == rowString {
			// A raw blob: up to eight bytes with no length, or the length in
			// one, two, four or eight bytes.
			var size uint64
			start := at + 2
			switch {
			case desc&^0b111 == wideBlob|wideShort:
				size = uint64(desc&0b111) + 1
			case desc&^0b11 == wideBlob:
				width := 1 << (desc & 0b11)
				if start += width; start <= len(body) {
					size = readMagnitude(body, at+2, width)
				}
			default:
				size = math.MaxUint64 // anything else is the slow path's to read
			}
			if start > len(body) || size > uint64(len(body)-start) {
				break
			}
			*(*string)(to) = buf.strings.String(body[start : start+int(size)])
			at, next = start+int(size), index+1
			continue
		}

		var value uint64
		end := 0
		signed := op.kind <= rowInt64
		switch {
		case desc <= wideMaxInline:
			value, end = uint64(desc), at+2
		case desc&0xF0 == wideIntNegative:
			// A sign and a magnitude, which an unsigned field reads without
			// the sign.
			width := wideWidths[desc&0b111]
			if width > len(body)-at-2 {
				break
			}
			value, end = 1, at+2+width
			if width > 0 {
				value = readMagnitude(body, at+2, width)
			}
			if signed {
				if desc&(wideIntPositive^wideIntNegative) == 0 {
					if value > 1<<63 {
						end = 0
					}
					value = -value
				} else if value > math.MaxInt64 {
					end = 0
				}
			}
		case desc >= wideVarint:
			value, end = readWideVarint(body, at+2, desc)
			if signed {
				value = value>>1 ^ -(value & 1)
			}
		}
		if end == 0 || !storeNumber(op.kind, to, value) {
			break
		}
		at, next = end, index+1
	}
	if at < len(body) {
		rest := wire.NewReader8(body[at:])
		readWideFields(&rest, plan, record, buf)
		return rest.Err()
	}
	return nil
}

// readWideVarint reads a varint's continuation bytes from body at from onto the
// three bits its descriptor holds, and returns where the field ends, or zero
// for a run that is cut short or carries bits past the sixty-fourth: the
// generic walk's to fail.
func readWideVarint(body []byte, from int, desc uint8) (value uint64, end int) {
	value = uint64(desc & 0b111)
	shift := uint(3)
	for cursor := from; cursor < len(body); cursor++ {
		b := body[cursor]
		payload := uint64(b & 0x7F)
		if shift >= 64 || payload>>(64-shift) != 0 {
			return 0, 0
		}
		value |= payload << shift
		if b < 0x80 {
			return value, cursor + 1
		}
		shift += 7
	}
	return 0, 0
}

// readMagnitude reads width bytes, one to eight, little-endian from body at
// from: as one word when the message holds eight bytes there, the bytes past
// the field masked off, and a byte at a time at the very end of it.
func readMagnitude(body []byte, from, width int) uint64 {
	if from+8 <= cap(body) {
		word := binary.LittleEndian.Uint64(unsafe.Slice((*byte)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(body)), from)), 8))
		return word & (math.MaxUint64 >> (64 - 8*width))
	}
	var value uint64
	for index := width - 1; index >= 0; index-- {
		value = value<<8 | uint64(body[from+index])
	}
	return value
}

// storeNumber stores value into a field of kind at to, as wire.Reader's typed
// read for the kind would, and reports false, storing nothing, for a value the
// read would refuse.
func storeNumber(kind uint8, to unsafe.Pointer, value uint64) bool {
	signed := int64(value)
	switch kind {
	case rowInt8:
		if int64(int8(signed)) != signed {
			return false
		}
		*(*int8)(to) = int8(signed)
	case rowInt16:
		if int64(int16(signed)) != signed {
			return false
		}
		*(*int16)(to) = int16(signed)
	case rowInt32:
		if int64(int32(signed)) != signed {
			return false
		}
		*(*int32)(to) = int32(signed)
	case rowInt64:
		*(*int64)(to) = signed
	case rowUint8:
		if value > math.MaxUint8 {
			return false
		}
		*(*uint8)(to) = uint8(value)
	case rowUint16:
		if value > math.MaxUint16 {
			return false
		}
		*(*uint16)(to) = uint16(value)
	case rowUint32:
		if value > math.MaxUint32 {
			return false
		}
		*(*uint32)(to) = uint32(value)
	case rowUint64:
		*(*uint64)(to) = value
	case rowFloat32:
		*(*uint32)(to) = bits.ReverseBytes32(uint32(value))
	case rowFloat64:
		*(*uint64)(to) = bits.ReverseBytes64(value)
	case rowBool:
		*(*bool)(to) = value == 1
	default:
		return false
	}
	return true
}

// readFlat fills a flat record from a narrow body whose fields arrive in the
// plan's order, as the encoder writes them: each op is the next field's or it
// is absent, and an absent one is zeroed where it stands. So the record is not
// cleared first, and a list is read into the array the field already has —
// Codec.Unmarshal's reset and settle, without their passes. It is only taken
// for a plan that is every field of its type (flatReads), so that no field
// outside it keeps a value the clear would have dropped.
//
// It reports false at anything else — a key out of order or unknown, a form
// it does not read, a value the field cannot hold, a body cut short — having
// written only fields, and the caller decodes the whole body again through
// the generic walk, which reads it or fails it as it always has.
func (program *rowProgram) readFlat(body []byte, record unsafe.Pointer) bool {
	at := 0
	for index := range program.ops {
		op := &program.ops[index]
		to := unsafe.Add(record, op.offset)
		if at >= len(body) || body[at]&0xF0 != op.key {
			zeroFlat(op.kind, to)
			continue
		}
		header := body[at]
		nibble := header & 0x0F
		switch op.kind {
		case rowString:
			start, size, ok := flatLength(body, at, nibble)
			if !ok || nibble >= wirefmt.NibbleLength && nibble != wirefmt.NibbleLength|wirefmt.FlagRaw {
				return false
			}
			*(*string)(to) = string(body[start : start+size])
			at = start + size
			continue
		case rowInt32s:
			start, size, ok := flatLength(body, at, nibble)
			if !ok || nibble < wirefmt.NibbleLength {
				return false
			}
			list, ok := readInt32s((*(*[]int32)(to))[:0], body[start:start+size], nibble&0b11)
			if !ok {
				return false
			}
			if len(list) == 0 {
				list = nil
			}
			*(*[]int32)(to) = list
			at = start + size
			continue
		}
		// A number, or a float or a bool in the unsigned shape, as read reads
		// one, and a negative's magnitude behind its width.
		var value uint64
		signed := op.kind <= rowInt64
		switch {
		case !signed && nibble < wirefmt.InlineValues, signed && nibble < wirefmt.SignedInline:
			value = uint64(nibble) + 1
			at++
		case signed && nibble == wirefmt.SignedMinusOne:
			value = math.MaxUint64
			at++
		case nibble >= wirefmt.NibbleSized && nibble < wirefmt.NibbleLength:
			width := int(nibble) - wirefmt.SizedBias
			if width > len(body)-at-1 {
				return false
			}
			if value = readMagnitude(body, at+1, width); signed && value > math.MaxInt64 {
				return false
			}
			at += 1 + width
		case signed && nibble == wirefmt.NibbleLength && at+1 < len(body):
			width := int(body[at+1])
			if width < 1 || width > 8 || width > len(body)-at-2 {
				return false
			}
			// −magnitude, down to math.MinInt64, and never zero or −1, which
			// have forms of their own.
			magnitude := readMagnitude(body, at+2, width)
			if magnitude < 2 || magnitude > 1<<63 {
				return false
			}
			value = -magnitude
			at += 2 + width
		default:
			return false
		}
		if !storeNumber(op.kind, to, value) {
			return false
		}
	}
	return at == len(body)
}

// flatLength is where a string's or list's bytes start in body, and how many
// there are: the nibble's own count for one to eight, or a length in any of
// its three forms.
func flatLength(body []byte, at int, nibble uint8) (start, size int, ok bool) {
	if nibble < wirefmt.NibbleLength {
		size = int(nibble) - wirefmt.SizedBias
		start = at + 1
		return start, size, size > 0 && size <= wirefmt.MaxSized && size <= len(body)-start
	}
	if at+1 >= len(body) {
		return 0, 0, false
	}
	switch length := body[at+1]; {
	case length <= wirefmt.InlineLength:
		start, size = at+2, int(length)
	case length < wirefmt.Length16 && at+3 <= len(body):
		start, size = at+3, wirefmt.LengthPair+(int(length-wirefmt.LengthPair)<<8|int(body[at+2]))
	case length == wirefmt.Length16 && at+4 <= len(body):
		start, size = at+4, int(binary.LittleEndian.Uint16(body[at+2:]))
	case length == wirefmt.Length32 && at+6 <= len(body):
		start, size = at+6, int(binary.LittleEndian.Uint32(body[at+2:]))
	default:
		return 0, 0, false
	}
	// A 32-bit length is negative as an int on a 32-bit platform.
	return start, size, size >= 0 && size <= len(body)-start
}

// readInt32s appends an int32 list's elements, two's complement at the width
// its flags name, and reports false for a width an int32 does not hold or a
// length that is not a whole number of elements.
func readInt32s(dst []int32, elements []byte, code uint8) ([]int32, bool) {
	switch code {
	case 0:
		dst = slices.Grow(dst, len(elements))
		for _, element := range elements {
			dst = append(dst, int32(int8(element)))
		}
	case 1:
		if len(elements)%2 != 0 {
			return dst, false
		}
		dst = slices.Grow(dst, len(elements)/2)
		for at := 0; at < len(elements); at += 2 {
			dst = append(dst, int32(int16(binary.LittleEndian.Uint16(elements[at:]))))
		}
	case 2:
		if len(elements)%4 != 0 {
			return dst, false
		}
		dst = slices.Grow(dst, len(elements)/4)
		for at := 0; at < len(elements); at += 4 {
			dst = append(dst, int32(binary.LittleEndian.Uint32(elements[at:])))
		}
	default:
		return dst, false
	}
	return dst, true
}

// zeroFlat stores the zero of an op's field: what a field the body does not
// hold decodes to. An absent list is nil, its array let go, as a fresh decode
// leaves it.
func zeroFlat(kind uint8, to unsafe.Pointer) {
	switch kind {
	case rowInt8, rowUint8, rowBool:
		*(*uint8)(to) = 0
	case rowInt16, rowUint16:
		*(*uint16)(to) = 0
	case rowInt32, rowUint32, rowFloat32:
		*(*uint32)(to) = 0
	case rowString:
		*(*string)(to) = ""
	case rowInt32s:
		*(*[]int32)(to) = nil
	default:
		*(*uint64)(to) = 0
	}
}
