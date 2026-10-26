package wire

// The compact form: dynamic values inside a dynamic container.
//
// A dynamic value at a field says what it is in an ordinary descriptor, because
// a reader that does not know the field has to step over it, and Skip sizes a
// field by its descriptor alone. A container holding dynamic values — a map, a
// list — is such a field: its descriptor and its length are what Skip needs. But
// what is *inside* it is read only by something that reads every value, and that
// reader needs no length to step over anything. So inside one the values are
// written in a code table of their own, where a byte is spent on what a document
// is made of rather than on what Skip needs:
//
//	0x00–0x7F  an unsigned integer, the code itself
//	0x80–0x8F  a map of 0..15 entries, each a key and a value
//	0x90–0x9F  a list of 0..15 values
//	0xA0–0xBF  a string of 0..31 bytes, which follow
//	0xC0       null
//	0xC1 0xC2  false, true
//	0xC3–0xC5  a string, its length in 1, 2 or 4 bytes
//	0xC6–0xC8  bytes, their length in 1, 2 or 4 bytes
//	0xC9 0xCA  a map, a list, its count after (one byte up to 254, or 0xFF and four)
//	0xCB       TYPED: a struct index, then a STRUCT, LIST or TABLE element as anywhere
//	0xCC       a float64 holding an integer, the compact integer after
//	0xD0–0xD7  an unsigned integer of 1..8 bytes
//	0xD8–0xDF  a negative integer, its magnitude in 1..8 bytes
//	0xE0–0xE8  a float64, its byte-reversed bits trimmed to 0..8 bytes
//	0xE9–0xED  a float32, the same in 0..4 bytes
//	0xF0–0xFF  a float64 holding 0..15
//
// 0xCD–0xCF and 0xEE–0xEF are unassigned and refused.
//
// A container inside a container carries its count and no length. That is what
// the compact form saves most — a length is a byte at least, and its backpatch
// a memmove past 253 — and what it gives up is stepping over one without reading
// it, which nothing inside a dynamic container has to do.
//
// A float rides as its byte-reversed bits, as everywhere in the format: a round
// value's mantissa is its high bytes, which the reversal makes the trimmed ones.
// A float64 holding a whole number — which is every number JSON hands Go — is
// written as that number when that is shorter, and a small one is a code.

import (
	"encoding/binary"
	"math"
	"math/bits"

	"github.com/ivanjoz/colbin/internal/wirefmt"
)

const (
	compactMap      uint8 = 0x80
	compactList     uint8 = 0x90
	compactString   uint8 = 0xA0
	compactNull     uint8 = 0xC0
	compactFalse    uint8 = 0xC1
	compactTrue     uint8 = 0xC2
	compactString8  uint8 = 0xC3 // and 0xC4, 0xC5
	compactBytes8   uint8 = 0xC6 // and 0xC7, 0xC8
	compactMapN     uint8 = 0xC9
	compactListN    uint8 = 0xCA
	compactTyped    uint8 = 0xCB
	compactWhole    uint8 = 0xCC
	compactUint     uint8 = 0xD0 // | width-1
	compactNegative uint8 = 0xD8 // | width-1
	compactFloat64  uint8 = 0xE0 // + width
	compactFloat32  uint8 = 0xE9 // + width
	compactSmall    uint8 = 0xF0 // + a float64's value

	compactFixMax    = 15 // the most entries or values a fix code holds
	compactFixString = 31 // the longest string a fix code holds
)

// Writer side.

// CompactNull writes null.
func (w *Writer8) CompactNull() { w.Buffer = append(w.Buffer, compactNull) }

// CompactBool writes false or true.
func (w *Writer8) CompactBool(value bool) {
	code := compactFalse
	if value {
		code = compactTrue
	}
	w.Buffer = append(w.Buffer, code)
}

// CompactUint writes an unsigned integer.
func (w *Writer8) CompactUint(value uint64) {
	if value < uint64(compactMap) {
		w.Buffer = append(w.Buffer, uint8(value))
		return
	}
	width := (bits.Len64(value) + 7) >> 3
	w.Buffer = wirefmt.AppendMagnitude(append(w.Buffer, compactUint|uint8(width-1)), value, width)
}

// CompactInt writes a signed integer.
func (w *Writer8) CompactInt(value int64) {
	if value >= 0 {
		w.CompactUint(uint64(value))
		return
	}
	magnitude := -uint64(value)
	width := (bits.Len64(magnitude) + 7) >> 3
	w.Buffer = wirefmt.AppendMagnitude(append(w.Buffer, compactNegative|uint8(width-1)), magnitude, width)
}

// CompactFloat64 writes a float64: a whole number from 0 to 15 as a code, any
// other whole number as the integer when that is shorter, and the trimmed
// reversed bits otherwise. Negative zero is not a whole number here, so it
// keeps its sign.
func (w *Writer8) CompactFloat64(value float64) {
	reversed := bits.ReverseBytes64(math.Float64bits(value))
	width := (bits.Len64(reversed) + 7) >> 3
	if value == math.Trunc(value) && math.Abs(value) < 1<<63 && !(value == 0 && math.Signbit(value)) {
		whole := int64(value)
		if uint64(whole) <= compactFixMax {
			w.Buffer = append(w.Buffer, compactSmall+uint8(whole))
			return
		}
		magnitude := uint64(whole)
		if whole < 0 {
			magnitude = -magnitude
		}
		size := 1
		if whole < 0 || magnitude >= uint64(compactMap) {
			size += (bits.Len64(magnitude) + 7) >> 3
		}
		if 1+size < 1+width {
			w.Buffer = append(w.Buffer, compactWhole)
			w.CompactInt(whole)
			return
		}
	}
	w.Buffer = wirefmt.AppendMagnitude(append(w.Buffer, compactFloat64+uint8(width)), reversed, width)
}

// CompactFloat32 writes a float32 as its trimmed reversed bits.
func (w *Writer8) CompactFloat32(value float32) {
	reversed := uint64(bits.ReverseBytes32(math.Float32bits(value)))
	width := (bits.Len64(reversed) + 7) >> 3
	w.Buffer = wirefmt.AppendMagnitude(append(w.Buffer, compactFloat32+uint8(width)), reversed, width)
}

// CompactString writes a string.
func (w *Writer8) CompactString(value string) {
	if len(value) <= compactFixString {
		w.Buffer = append(append(w.Buffer, compactString|uint8(len(value))), value...)
		return
	}
	w.Buffer = append(w.compactLength(compactString8, len(value)), value...)
}

// CompactBytes writes opaque bytes.
func (w *Writer8) CompactBytes(value []byte) {
	w.Buffer = append(w.compactLength(compactBytes8, len(value)), value...)
}

// compactLength writes code, widened to the length's width, and the length.
func (w *Writer8) compactLength(code uint8, length int) []byte {
	switch {
	case length <= 0xFF:
		return append(w.Buffer, code, uint8(length))
	case length <= 0xFFFF:
		return binary.LittleEndian.AppendUint16(append(w.Buffer, code+1), uint16(length))
	default:
		return binary.LittleEndian.AppendUint32(append(w.Buffer, code+2), uint32(length))
	}
}

// CompactMap begins a map of count entries, each a key and a value written
// after it. There is nothing to close.
func (w *Writer8) CompactMap(count int) {
	if count <= compactFixMax {
		w.Buffer = append(w.Buffer, compactMap|uint8(count))
		return
	}
	w.Buffer = appendCount(append(w.Buffer, compactMapN), count)
}

// CompactList begins a list of count values written after it.
func (w *Writer8) CompactList(count int) {
	if count <= compactFixMax {
		w.Buffer = append(w.Buffer, compactList|uint8(count))
		return
	}
	w.Buffer = appendCount(append(w.Buffer, compactListN), count)
}

// CompactTyped writes the tag that names a struct def. The value follows as an
// element: OpenElementStruct for one record, OpenElementList or OpenElementTable
// for many.
func (w *Writer8) CompactTyped(index int) {
	w.Buffer = appendCount(append(w.Buffer, compactTyped), index)
}

// Reader side. A compact reader sits on the code, as an element reader sits on
// its descriptor.

// CompactKind says what the value at the cursor is, without advancing, and
// KindInvalid for a code this version does not assign or a message that has
// run out.
func (r *Reader8) CompactKind() Kind {
	if r.err != nil || r.at >= len(r.buffer) {
		return KindInvalid
	}
	return compactKinds[r.buffer[r.at]]
}

var compactKinds = func() (kinds [256]Kind) {
	for code := range 256 {
		kind := KindInvalid
		switch {
		case code < 0x80:
			kind = KindInt
		case code < 0x90:
			kind = KindMap
		case code < 0xA0:
			kind = KindList
		case code < 0xC0:
			kind = KindString
		case code == 0xC0:
			kind = KindNull
		case code <= 0xC2:
			kind = KindBool
		case code <= 0xC5:
			kind = KindString
		case code <= 0xC8:
			kind = KindBytes
		case code == 0xC9:
			kind = KindMap
		case code == 0xCA:
			kind = KindList
		case code == 0xCB:
			kind = KindTyped
		case code == 0xCC:
			kind = KindFloat64
		case code >= 0xD0 && code <= 0xDF:
			kind = KindInt
		case code >= 0xE0 && code <= 0xE8:
			kind = KindFloat64
		case code >= 0xE9 && code <= 0xED:
			kind = KindFloat32
		case code >= 0xF0:
			kind = KindFloat64
		}
		kinds[code] = kind
	}
	return kinds
}()

// CompactNull steps over a null.
func (r *Reader8) CompactNull() {
	if r.CompactKind() != KindNull {
		r.fail(r.compactError())
		return
	}
	r.at++
}

// CompactBool reads false or true.
func (r *Reader8) CompactBool() bool {
	if r.CompactKind() != KindBool {
		r.fail(r.compactError())
		return false
	}
	r.at++
	return r.buffer[r.at-1] == compactTrue
}

// CompactInteger reads an integer as its magnitude and its sign.
func (r *Reader8) CompactInteger() (magnitude uint64, negative bool) {
	if r.at >= len(r.buffer) {
		r.fail(ErrTruncated)
		return 0, false
	}
	code := r.buffer[r.at]
	if code < compactMap {
		r.at++
		return uint64(code), false
	}
	if code < compactUint || code > compactNegative|7 {
		r.fail(ErrBadDescriptor)
		return 0, false
	}
	magnitude = r.compactMagnitude(int(code&7) + 1)
	negative = code >= compactNegative
	if negative && magnitude > 1<<63 {
		// Past the most negative int64, which nothing writes.
		r.fail(ErrBadDescriptor)
		return 0, false
	}
	return magnitude, negative
}

// compactMagnitude reads the width bytes after the code.
func (r *Reader8) compactMagnitude(width int) uint64 {
	start := r.at + 1
	if width > len(r.buffer)-start {
		r.fail(ErrTruncated)
		return 0
	}
	r.at = start + width
	var value [8]byte
	copy(value[:], r.buffer[start:r.at])
	return binary.LittleEndian.Uint64(value[:])
}

// CompactFloat reads a float64 or a float32, and says which it was.
func (r *Reader8) CompactFloat() (value float64, width int) {
	if r.at >= len(r.buffer) {
		r.fail(ErrTruncated)
		return 0, 64
	}
	switch code := r.buffer[r.at]; {
	case code >= compactSmall:
		r.at++
		return float64(code - compactSmall), 64
	case code == compactWhole:
		r.at++
		magnitude, negative := r.CompactInteger()
		if negative {
			return -float64(magnitude), 64
		}
		return float64(magnitude), 64
	case code >= compactFloat64 && code < compactFloat32:
		reversed := r.compactMagnitude(int(code - compactFloat64))
		return math.Float64frombits(bits.ReverseBytes64(reversed)), 64
	case code >= compactFloat32 && code <= compactFloat32+4:
		reversed := r.compactMagnitude(int(code - compactFloat32))
		return float64(math.Float32frombits(bits.ReverseBytes32(uint32(reversed)))), 32
	}
	r.fail(ErrBadDescriptor)
	return 0, 64
}

// CompactString returns a string's bytes as a sub-slice of the message.
func (r *Reader8) CompactString() []byte {
	if r.at >= len(r.buffer) {
		r.fail(ErrTruncated)
		return nil
	}
	if code := r.buffer[r.at]; code&0xE0 == compactString {
		return r.compactBody(r.at+1, int(code&compactFixString))
	}
	return r.compactSized(compactString8)
}

// CompactBytes returns opaque bytes as a sub-slice of the message.
func (r *Reader8) CompactBytes() []byte { return r.compactSized(compactBytes8) }

// compactSized reads a value whose code is first+0, +1 or +2 for a length of
// one, two or four bytes.
func (r *Reader8) compactSized(first uint8) []byte {
	if r.at >= len(r.buffer) {
		r.fail(ErrTruncated)
		return nil
	}
	code := r.buffer[r.at]
	if code < first || code > first+2 {
		r.fail(ErrBadDescriptor)
		return nil
	}
	width := 1 << (code - first)
	start := r.at + 1
	if width > len(r.buffer)-start {
		r.fail(ErrTruncated)
		return nil
	}
	var length [4]byte
	copy(length[:], r.buffer[start:start+width])
	size := binary.LittleEndian.Uint32(length[:])
	if uint64(size) > uint64(maxInt) {
		r.fail(ErrTruncated)
		return nil
	}
	return r.compactBody(start+width, int(size))
}

func (r *Reader8) compactBody(start, size int) []byte {
	if size > len(r.buffer)-start {
		r.fail(ErrTruncated)
		return nil
	}
	r.at = start + size
	return r.buffer[start:r.at]
}

// CompactMap reads a map's entry count, and leaves the cursor on its first
// key. A count the bytes left cannot hold, at two codes an entry, is refused,
// which is what makes it safe to allocate for.
func (r *Reader8) CompactMap() (int, bool) { return r.compactCount(compactMap, compactMapN, 2) }

// CompactList reads a list's value count, and leaves the cursor on its first
// value.
func (r *Reader8) CompactList() (int, bool) { return r.compactCount(compactList, compactListN, 1) }

func (r *Reader8) compactCount(fix, counted uint8, minSize int) (int, bool) {
	if r.at >= len(r.buffer) {
		r.fail(ErrTruncated)
		return 0, false
	}
	code := r.buffer[r.at]
	count := 0
	switch {
	case code&0xF0 == fix:
		count = int(code & compactFixMax)
		r.at++
	case code == counted:
		value, width, ok := readCount(r.buffer[r.at+1:])
		if !ok {
			r.fail(ErrTruncated)
			return 0, false
		}
		count = value
		r.at += 1 + width
	default:
		r.fail(ErrBadDescriptor)
		return 0, false
	}
	if count > (len(r.buffer)-r.at)/minSize {
		r.fail(ErrTruncated)
		return 0, false
	}
	return count, true
}

// CompactTyped reads the tag that names a struct def and leaves the cursor on
// the element behind it.
func (r *Reader8) CompactTyped() (int, bool) {
	if r.CompactKind() != KindTyped {
		r.fail(r.compactError())
		return 0, false
	}
	index, width, ok := readCount(r.buffer[r.at+1:])
	if !ok {
		r.fail(ErrTruncated)
		return 0, false
	}
	r.at += 1 + width
	return index, true
}

// compactError is why the value at the cursor is not the one asked for.
func (r *Reader8) compactError() error {
	if r.err != nil {
		return r.err
	}
	if r.at >= len(r.buffer) {
		return ErrTruncated
	}
	return ErrBadDescriptor
}
