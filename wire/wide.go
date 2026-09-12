package wire

// The eight-bit key width, K8: a byte for the key and a byte for a descriptor
// that names the field's class, where the four-bit width shares one byte and
// takes the class from the schema.
//
// # What the extra byte buys
//
//   - **256 fields**, against sixteen.
//   - **Skip.** Every class either carries a byte length or has one derivable
//     from the descriptor alone, so a reader that has never heard of a key can
//     step over it — which is schema evolution, and the foundation for an
//     interface field. Reader.Skip refuses under K4 and works here.
//   - **An inline value.** A descriptor whose top bit is clear *is* the value,
//     0..127, so a small integer costs two bytes — the same as under K4, where
//     it costs a header byte and a value byte. That is why the wide key is not
//     simply a byte worse per field: it is a byte worse only above 127.
//
// # Why this is a separate file, not a flag
//
// The key width must not be a variable the field loop can see. Measured on a
// ten-field record, one decoder carrying a `k8 bool` runs at 11.7 ns where a
// decoder per width runs at 8.7 — not because the branch mispredicts, it never
// does, but because a width the compiler cannot see is a width it cannot fold.
// The framing is therefore written twice; the value codecs underneath —
// magnitudes, blobs, array elements — are written once and shared.
//
// # Descriptor
//
//	0 vvvvvvv                   the value, 0..127, no payload
//	1 ccc dddd                  class ccc, detail dddd
//
//	class 0 INT      [pos:1][n:3]        n magnitude bytes, as K4
//	class 1 BLOB     [enc:2][lw:2]       [size: lw] then size bytes
//	class 2 VEC      [w:2][pos:1][lw:1]  [bytelen: lw] then the elements
//	class 3 COL      reserved for the column codec
//	class 4 LIST     [homog:1][-:1][lw:2] [bytelen: lw][count: lw] then elements
//	class 5 STRUCT   reserved for a nested key run
//	class 6 MAP      reserved
//	class 7 SPECIAL  [detail:4]          null, true, false, NaN, ...
//
// `lw` names the width of the length that follows: 0 → 1 byte, 1 → 2, 2 → 4,
// 3 → 8. There is no varint anywhere, so reading a length is a branch and one
// load rather than a loop whose trip count is data.

import (
	"encoding/binary"
	"errors"
	"math"
	"math/bits"
)

// MaxWideFields is what eight key bits buy: keys 0..255.
const MaxWideFields = 256

// Descriptor classes, in bits 6..4 of an explicit descriptor.
const (
	classInt     uint8 = 0
	classBlob    uint8 = 1
	classVec     uint8 = 2
	classCol     uint8 = 3
	classList    uint8 = 4
	classStruct  uint8 = 5
	classMap     uint8 = 6
	classSpecial uint8 = 7
)

const (
	// descExplicit is the top bit: set means the descriptor names a class, clear
	// means the remaining seven bits are the value itself.
	descExplicit uint8 = 0x80
	// maxInlineValue is the largest integer a descriptor can be.
	maxInlineValue = 0x7F
)

// SPECIAL details. Details 0..7 are the reserved door; detail bit 3 is the
// varint integer, which takes the upper half of the nibble.
const (
	specialNull  uint8 = 0
	specialTrue  uint8 = 1
	specialFalse uint8 = 2
)

// The K8 varint integer.
//
// # Why a second form exists
//
// A wide field spends a whole byte on its key, so its descriptor byte begins
// with nothing of the value in it. The INT class then spends all four detail
// bits on [positive:1][size:3] — a sign and a byte count — which leaves the
// descriptor carrying no payload at all. A value of 300 therefore costs four
// bytes: key, descriptor, and two magnitude bytes.
//
// This form puts three value bits in the descriptor and continues seven at a
// time:
//
//	[key:8] [1 111 1 vvv] ( [more:1] [v:7] )+
//
// 300 costs three. The continuation is a plain LEB128 over whatever is left
// after those three bits, and at least one continuation byte always follows —
// values small enough not to need one are already cheaper in the inline form.
//
// # Why it does not replace the byte-count form
//
// Seven bits per byte loses to eight once a value is wide: a random int64 costs
// eleven bytes as a varint against ten as a sign and a magnitude. So both forms
// stay and **the writer emits whichever is shorter**, which makes this strictly
// a saving — measured at 7% on small ids and on deltas, and zero everywhere
// else, with no value anywhere that got larger.
//
// A reader does not have to be told which form it is looking at: the class says
// so, which is what keeps an unknown wide field skippable.
const specialVarint uint8 = 0b1000

// varintBits is how much of the value the descriptor's detail nibble carries.
const varintBits = 3

// zigzag folds a signed value so that small negatives stay small, which is what
// makes the varint worth using for a delta. Uint does not use it: an unsigned
// value would pay a bit for a sign it does not have.
func zigzag(value int64) uint64 {
	return uint64(value<<1) ^ uint64(value>>63)
}

func unzigzag(value uint64) int64 {
	return int64(value>>1) ^ -int64(value&1)
}

// varintLen is the total field length this form would take, key byte included,
// which is what the writer compares against the byte-count form.
func varintLen(value uint64) int {
	rest := value >> varintBits
	length := 2 // the key and the descriptor
	for {
		length++
		if rest < 0x80 {
			return length
		}
		rest >>= 7
	}
}

// appendVarint writes the descriptor and the continuation bytes.
func appendVarint(buffer []byte, key uint8, value uint64) []byte {
	buffer = append(buffer,
		key, descriptor(classSpecial, specialVarint|uint8(value&0b111)))
	rest := value >> varintBits
	for rest >= 0x80 {
		buffer = append(buffer, uint8(rest)|0x80)
		rest >>= 7
	}
	return append(buffer, uint8(rest))
}

// readVarint reads the form back, returning the byte length of the whole field.
func readVarint(field []byte, detail uint8) (value uint64, length int, ok bool) {
	value = uint64(detail & 0b111)
	shift := uint(varintBits)
	for at := 2; at < len(field); at++ {
		b := field[at]
		value |= uint64(b&0x7F) << shift
		if b < 0x80 {
			return value, at + 1, true
		}
		shift += 7
		if shift > 63+varintBits {
			return 0, 0, false
		}
	}
	return 0, 0, false
}

// listHomogeneous is the LIST detail bit saying the elements share one shape and
// carry no descriptor of their own — which is what a list of strings is. Clear,
// the elements each carry a descriptor, which is what a list of structs or of
// dynamically typed values needs.
const listHomogeneous uint8 = 0b1000

// lengthWidth maps a two-bit lw code to its byte count.
var lengthWidth = [4]int{1, 2, 4, 8}

// ErrBadDescriptor is a descriptor this version does not assign, or one whose
// class is not the one the caller asked to read.
var ErrBadDescriptor = errors.New("narrow: unassigned or mismatched descriptor")

// lengthCodeFor is the narrowest lw that holds n.
func lengthCodeFor(n uint64) (code uint8, width int) {
	switch {
	case n <= 0xFF:
		return 0, 1
	case n <= 0xFFFF:
		return 1, 2
	case n <= 0xFFFF_FFFF:
		return 2, 4
	default:
		return 3, 8
	}
}

func descriptor(class, detail uint8) uint8 { return descExplicit | class<<4 | detail }

// Writer8 appends fields with eight-bit keys. Like Writer it holds no state but
// the buffer, checks nothing, and cannot fail.
type Writer8 struct {
	Buffer []byte
}

// Reset points the writer at a buffer, keeping its capacity.
func (w *Writer8) Reset(buffer []byte) { w.Buffer = buffer[:0] }

// Uint writes an unsigned integer, and nothing at all when it is zero.
//
// Values to 127 are the descriptor itself, which is the whole reason a wide key
// is affordable: the common small field is two bytes here and two under K4.
func (w *Writer8) Uint(key uint8, value uint64) {
	if value == 0 {
		return
	}
	if value <= maxInlineValue {
		w.Buffer = append(w.Buffer, key, uint8(value))
		return
	}
	w.uintWide(key, value)
}

func (w *Writer8) uintWide(key uint8, value uint64) {
	code, width := sizeCodeFor(value)
	// An unsigned value is not zigzagged: it has no sign to fold, and folding it
	// would cost a bit for nothing.
	if varintLen(value) < 2+width {
		w.Buffer = appendVarint(w.Buffer, key, value)
		return
	}
	w.Buffer = appendMagnitude(
		append(w.Buffer, key, descriptor(classInt, intPositiveFlag|code)), value, width)
}

// Int writes a signed integer, as a zigzag varint or as a sign and a magnitude,
// whichever is shorter.
//
// A positive value does not go through Uint. The varint under SPECIAL is raw
// when Uint wrote it and zigzagged when Int did — the schema picks the reader,
// so the two never meet, but only if each writer stays on its own side.
func (w *Writer8) Int(key uint8, value int64) {
	if value == 0 {
		return
	}
	if value > 0 && value <= maxInlineValue {
		w.Buffer = append(w.Buffer, key, uint8(value))
		return
	}
	magnitude := uint64(value)
	detail := intPositiveFlag
	if value < 0 {
		// Negating through uint64 rather than int64 keeps math.MinInt64, whose
		// positive counterpart does not exist as an int64.
		magnitude, detail = -uint64(value), 0
	}
	code, width := sizeCodeFor(magnitude)
	if folded := zigzag(value); varintLen(folded) < 2+width {
		w.Buffer = appendVarint(w.Buffer, key, folded)
		return
	}
	w.Buffer = appendMagnitude(
		append(w.Buffer, key, descriptor(classInt, uint8(detail)|code)), magnitude, width)
}

// Bool writes two bytes when true and nothing when false. True rides in the
// inline form as the value one, so it needs no special code of its own.
func (w *Writer8) Bool(key uint8, value bool) {
	if !value {
		return
	}
	w.Buffer = append(w.Buffer, key, 1)
}

// U16, U32 and I32 are the width-typed entry points, for the reason Writer's
// are: a field's Go type already fixes how wide it can be.
func (w *Writer8) U16(key uint8, value uint16) {
	if value == 0 {
		return
	}
	if value <= maxInlineValue {
		w.Buffer = append(w.Buffer, key, uint8(value))
		return
	}
	if value <= 0xFF {
		// Three bytes either way at this width, so the fast path keeps it.
		w.Buffer = append(w.Buffer, key, descriptor(classInt, intPositiveFlag|1), uint8(value))
		return
	}
	// Past a byte the varint can be shorter, and a uint16 should not encode
	// differently from a uint32 holding the same value.
	w.uintWide(key, uint64(value))
}

func (w *Writer8) U32(key uint8, value uint32) {
	if value == 0 {
		return
	}
	if value <= maxInlineValue {
		w.Buffer = append(w.Buffer, key, uint8(value))
		return
	}
	w.uintWide(key, uint64(value))
}

func (w *Writer8) I32(key uint8, value int32) { w.Int(key, int64(value)) }

// F32 and F64 reverse the bit pattern's bytes so the trim reaches the zeros,
// exactly as Writer's do.
func (w *Writer8) F32(key uint8, value float32) {
	w.Uint(key, uint64(bits.ReverseBytes32(math.Float32bits(value))))
}

func (w *Writer8) F64(key uint8, value float64) {
	w.Uint(key, bits.ReverseBytes64(math.Float64bits(value)))
}

// Bytes writes a length-prefixed blob, and nothing when it is empty.
func (w *Writer8) Bytes(key uint8, value []byte) {
	if len(value) == 0 {
		return
	}
	w.blobHeader(key, len(value))
	w.Buffer = append(w.Buffer, value...)
}

// String is Bytes for a string, which Go appends without copying it first.
func (w *Writer8) String(key uint8, value string) {
	if len(value) == 0 {
		return
	}
	w.blobHeader(key, len(value))
	w.Buffer = append(w.Buffer, value...)
}

func (w *Writer8) blobHeader(key uint8, size int) {
	code, width := lengthCodeFor(uint64(size))
	w.Buffer = appendMagnitude(
		append(w.Buffer, key, descriptor(classBlob, code)), uint64(size), width)
}

// Array writers, one concrete method per element type, for the reason the narrow
// ones are concrete: a *Writer8 in a generic signature costs the caller an
// allocation.

func (w *Writer8) Ints(key uint8, values []int64)     { w.Buffer = appendVec(w.Buffer, key, values) }
func (w *Writer8) Int8s(key uint8, values []int8)     { w.Buffer = appendVec(w.Buffer, key, values) }
func (w *Writer8) Int16s(key uint8, values []int16)   { w.Buffer = appendVec(w.Buffer, key, values) }
func (w *Writer8) Int32s(key uint8, values []int32)   { w.Buffer = appendVec(w.Buffer, key, values) }
func (w *Writer8) Uint16s(key uint8, values []uint16) { w.Buffer = appendVec(w.Buffer, key, values) }
func (w *Writer8) Uint32s(key uint8, values []uint32) { w.Buffer = appendVec(w.Buffer, key, values) }
func (w *Writer8) Uint64s(key uint8, values []uint64) { w.Buffer = appendVec(w.Buffer, key, values) }

// appendVec is the shared body, and it takes the buffer rather than the writer.
//
// That is not a style choice. A `*Writer8` in a *generic* signature makes the
// compiler hand cross-package callers pessimistic escape information, and the
// writer they allocated on their stack goes to the heap instead — one allocation
// per message, on every record with an array field in it. The narrow side was
// fixed for this and the wide side was not, which is why an untagged flat record
// allocated where a tagged one did not.
func appendVec[T Integer](buffer []byte, key uint8, values []T) []byte {
	if len(values) == 0 {
		return buffer
	}
	allPositive, width, widthCode := arrayPlanOf(values)
	byteLength := len(values) * width
	// A vector's length is in bytes, not elements, so a reader that does not
	// know the key can still skip it. The count is byteLength >> widthCode and
	// never goes on the wire.
	lengthCode := uint8(0)
	lengthBytes := 1
	if byteLength > 0xFF {
		lengthCode, lengthBytes = 1, 4
	}
	detail := widthCode<<2 | lengthCode
	if allPositive {
		detail |= 0b10
	}
	buffer = appendMagnitude(
		append(buffer, key, descriptor(classVec, detail)), uint64(byteLength), lengthBytes)
	return appendElements(buffer, values, width)
}

// Strings writes a count and then each element behind its own length, inside a
// byte length that lets the whole field be skipped.
func (w *Writer8) Strings(key uint8, values []string) {
	if len(values) == 0 {
		return
	}
	payload := 0
	for _, value := range values {
		if len(value) <= inlineElementSize {
			payload += 1 + len(value)
		} else {
			payload += 5 + len(value)
		}
	}
	// The declared length covers the count as well as the elements, which is
	// what makes a skip uniform across every composite: two bytes of framing,
	// the length field, and then exactly that many bytes.
	counted := payload + countBytes(len(values))
	code, width := lengthCodeFor(uint64(counted))
	w.Buffer = append(w.Buffer, key, descriptor(classList, listHomogeneous))
	w.Buffer[len(w.Buffer)-1] |= code
	w.Buffer = appendMagnitude(w.Buffer, uint64(counted), width)
	w.Buffer = appendCount(w.Buffer, len(values))
	for _, value := range values {
		if len(value) <= inlineElementSize {
			w.Buffer = append(w.Buffer, uint8(len(value)))
		} else {
			w.Buffer = binary.LittleEndian.AppendUint32(
				append(w.Buffer, elementSizeEscape), uint32(len(value)))
		}
		w.Buffer = append(w.Buffer, value...)
	}
}

// Reader8 walks a message written by Writer8.
type Reader8 struct {
	buffer []byte
	at     int
	err    error
}

// NewReader8 starts a reader over one message.
func NewReader8(message []byte) Reader8 { return Reader8{buffer: message} }

// More reports whether another field follows. It does not test the error
// because fail parks the cursor at the end, so one comparison answers both.
func (r *Reader8) More() bool { return r.at+1 < len(r.buffer) }

// Key is the field the cursor is on. It does not advance: the typed read does.
func (r *Reader8) Key() uint8 { return r.buffer[r.at] }

// Err reports the first failure.
func (r *Reader8) Err() error { return r.err }

func (r *Reader8) fail(err error) {
	if r.err == nil {
		r.err = err
	}
	r.at = len(r.buffer)
}

// Uint reads an unsigned integer field, ignoring the sign bit.
func (r *Reader8) Uint() uint64 {
	if field := r.buffer[r.at:]; len(field) >= 2 && field[1] < descExplicit {
		r.at += 2
		return uint64(field[1])
	}
	return r.uintWide()
}

func (r *Reader8) uintWide() uint64 {
	if r.at+2 > len(r.buffer) {
		r.fail(ErrTruncated)
		return 0
	}
	desc := r.buffer[r.at+1]
	if desc < descExplicit {
		r.at += 2
		return uint64(desc)
	}
	if class := (desc >> 4) & 0b111; class != classInt {
		if class == classSpecial && desc&specialVarint != 0 {
			value, length, ok := readVarint(r.buffer[r.at:], desc)
			if !ok {
				r.fail(ErrTruncated)
				return 0
			}
			r.at += length
			return value
		}
		r.fail(ErrBadDescriptor)
		return 0
	}
	width := magnitudeWidth[desc&0b111]
	rest := r.buffer[r.at+2:]
	if len(rest) < width {
		r.fail(ErrTruncated)
		return 0
	}
	r.at += 2 + width
	if width == 0 {
		return 1
	}
	return leUint(rest, width)
}

// Int reads a signed integer field.
func (r *Reader8) Int() int64 {
	if r.at+2 > len(r.buffer) {
		r.fail(ErrTruncated)
		return 0
	}
	desc := r.buffer[r.at+1]
	// A varint written by Int is zigzagged, which carries its own sign.
	if desc >= descExplicit && (desc>>4)&0b111 == classSpecial && desc&specialVarint != 0 {
		return unzigzag(r.uintWide())
	}
	negative := desc >= descExplicit && (desc>>4)&0b111 == classInt && desc&intPositiveFlag == 0
	magnitude := r.uintWide()
	if negative {
		return -int64(magnitude)
	}
	return int64(magnitude)
}

// Bool reads a field written by Bool.
func (r *Reader8) Bool() bool { return r.Uint() == 1 }

// U16 and U32 refuse a field wider than the type this side declares, which is a
// schema disagreement rather than a value to truncate into something plausible.
func (r *Reader8) U16() uint16 {
	value := r.Uint()
	if value > 0xFFFF {
		r.fail(ErrFieldTooWide)
		return 0
	}
	return uint16(value)
}

func (r *Reader8) U32() uint32 {
	value := r.Uint()
	if value > 0xFFFF_FFFF {
		r.fail(ErrFieldTooWide)
		return 0
	}
	return uint32(value)
}

func (r *Reader8) I32() int32 { return int32(r.Int()) }

func (r *Reader8) F32() float32 {
	return math.Float32frombits(bits.ReverseBytes32(uint32(r.Uint())))
}

func (r *Reader8) F64() float64 {
	return math.Float64frombits(bits.ReverseBytes64(r.Uint()))
}

// Bytes returns the field's bytes as a sub-slice of the message, without
// copying. It stays valid only as long as the message buffer does.
func (r *Reader8) Bytes() []byte {
	size, start, ok := r.lengthOf(classBlob)
	if !ok {
		return nil
	}
	if size > len(r.buffer)-start {
		r.fail(ErrTruncated)
		return nil
	}
	r.at = start + size
	return r.buffer[start : start+size]
}

// String copies the field into a Go string.
func (r *Reader8) String() string { return string(r.Bytes()) }

// lengthOf reads the length a class's descriptor declares and returns it with
// the offset just past it. It also checks the class, so a reader asking for a
// string and finding an array is told rather than handed nonsense.
func (r *Reader8) lengthOf(want uint8) (length, start int, ok bool) {
	if r.at+2 > len(r.buffer) {
		r.fail(ErrTruncated)
		return 0, 0, false
	}
	desc := r.buffer[r.at+1]
	if desc < descExplicit || (desc>>4)&0b111 != want {
		r.fail(ErrBadDescriptor)
		return 0, 0, false
	}
	width := lengthWidth[desc&0b11]
	if want == classVec {
		width = 1
		if desc&1 != 0 {
			width = 4
		}
	}
	rest := r.buffer[r.at+2:]
	if len(rest) < width {
		r.fail(ErrTruncated)
		return 0, 0, false
	}
	value := leUint(rest, width)
	if value > uint64(maxInt) {
		r.fail(ErrSizeTooLarge)
		return 0, 0, false
	}
	return int(value), r.at + 2 + width, true
}

// Array readers, the mirror of the writers.

// Each one is written out rather than sharing a generic body that takes the
// reader, for the reason appendVec gives: a `*Reader8` in a generic signature
// makes a cross-package caller heap-allocate its reader. The framing is shared
// through vecBody, which is an ordinary method, and the decode through
// appendArray, which takes only slices.

func (r *Reader8) Ints(dst []int64) []int64 {
	if elements, width, positive, ok := r.vecBody(); ok {
		return appendArray(dst, elements, width, positive)
	}
	return dst
}

func (r *Reader8) Int8s(dst []int8) []int8 {
	if elements, width, positive, ok := r.vecBody(); ok {
		return appendArray(dst, elements, width, positive)
	}
	return dst
}

func (r *Reader8) Int16s(dst []int16) []int16 {
	if elements, width, positive, ok := r.vecBody(); ok {
		return appendArray(dst, elements, width, positive)
	}
	return dst
}

func (r *Reader8) Int32s(dst []int32) []int32 {
	if elements, width, positive, ok := r.vecBody(); ok {
		return appendArray(dst, elements, width, positive)
	}
	return dst
}

func (r *Reader8) Uint16s(dst []uint16) []uint16 {
	if elements, width, positive, ok := r.vecBody(); ok {
		return appendArray(dst, elements, width, positive)
	}
	return dst
}

func (r *Reader8) Uint32s(dst []uint32) []uint32 {
	if elements, width, positive, ok := r.vecBody(); ok {
		return appendArray(dst, elements, width, positive)
	}
	return dst
}

func (r *Reader8) Uint64s(dst []uint64) []uint64 {
	if elements, width, positive, ok := r.vecBody(); ok {
		return appendArray(dst, elements, width, positive)
	}
	return dst
}

// vecBody reads a vector's framing and hands back the element bytes. It is the
// part that needs the reader, and it is deliberately not generic.
func (r *Reader8) vecBody() (elements []byte, width int, positive, ok bool) {
	byteLength, start, ok := r.lengthOf(classVec)
	if !ok {
		return nil, 0, false, false
	}
	desc := r.buffer[r.at+1]
	width = 1 << ((desc >> 2) & 0b11)
	if byteLength%width != 0 || byteLength > len(r.buffer)-start {
		r.fail(ErrTruncated)
		return nil, 0, false, false
	}
	elements = r.buffer[start : start+byteLength]
	r.at = start + byteLength
	return elements, width, desc&0b10 != 0, true
}

// Strings copies each element into a Go string.
func (r *Reader8) Strings(dst []string) []string {
	count, at, ok := r.listHeader()
	if !ok {
		return dst
	}
	for range count {
		size, next, ok := r.elementSize(at)
		if !ok {
			return dst
		}
		if size > len(r.buffer)-next {
			r.fail(ErrTruncated)
			return dst
		}
		dst = append(dst, string(r.buffer[next:next+size]))
		at = next + size
	}
	r.at = at
	return dst
}

// StringsBytes appends each element as a sub-slice of the message.
func (r *Reader8) StringsBytes(dst [][]byte) [][]byte {
	count, at, ok := r.listHeader()
	if !ok {
		return dst
	}
	for range count {
		size, next, ok := r.elementSize(at)
		if !ok {
			return dst
		}
		if size > len(r.buffer)-next {
			r.fail(ErrTruncated)
			return dst
		}
		dst = append(dst, r.buffer[next:next+size])
		at = next + size
	}
	r.at = at
	return dst
}

// listHeader reads a list's byte length and count and returns the count with the
// offset of the first element.
func (r *Reader8) listHeader() (count, at int, ok bool) {
	length, start, ok := r.lengthOf(classList)
	if !ok {
		return 0, 0, false
	}
	if length > len(r.buffer)-start {
		r.fail(ErrTruncated)
		return 0, 0, false
	}
	count, consumed, ok := readCount(r.buffer[start : start+length])
	if !ok {
		r.fail(ErrTruncated)
		return 0, 0, false
	}
	return count, start + consumed, true
}

// elementSize reads one list element length: one byte, or four more behind the
// escape.
func (r *Reader8) elementSize(at int) (size, start int, ok bool) {
	if at >= len(r.buffer) {
		r.fail(ErrTruncated)
		return 0, 0, false
	}
	if size := r.buffer[at]; size != elementSizeEscape {
		return int(size), at + 1, true
	}
	if at+5 > len(r.buffer) {
		r.fail(ErrTruncated)
		return 0, 0, false
	}
	value := binary.LittleEndian.Uint32(r.buffer[at+1:])
	if uint64(value) > uint64(maxInt) {
		r.fail(ErrSizeTooLarge)
		return 0, 0, false
	}
	return int(value), at + 5, true
}

// Skip steps over the field at the cursor without knowing what it is, which is
// the capability the wide key exists for. Reader.Skip, at four key bits, can
// only refuse: its descriptor has no room for a class, so the same four bits
// mean different things under different keys and nothing can size a field it
// cannot classify.
func (r *Reader8) Skip() bool {
	size, ok := r.fieldSize()
	if !ok {
		return false
	}
	r.at += size
	return true
}

// fieldSize is the whole of the skip: every class either carries a byte length
// or has one derivable from its descriptor.
func (r *Reader8) fieldSize() (int, bool) {
	if r.at+2 > len(r.buffer) {
		r.fail(ErrTruncated)
		return 0, false
	}
	desc := r.buffer[r.at+1]
	if desc < descExplicit {
		return 2, true // the descriptor is the value
	}
	switch (desc >> 4) & 0b111 {
	case classInt:
		width := magnitudeWidth[desc&0b111]
		if len(r.buffer)-(r.at+2) < width {
			r.fail(ErrTruncated)
			return 0, false
		}
		return 2 + width, true
	case classSpecial:
		if desc&specialVarint == 0 {
			return 2, true
		}
		// A varint is self-delimiting, so a reader that does not know the key can
		// still step over it: walk the continuation bits to their end.
		_, length, ok := readVarint(r.buffer[r.at:], desc)
		if !ok {
			r.fail(ErrTruncated)
			return 0, false
		}
		return length, true
	case classBlob, classVec, classList, classStruct, classMap, classCol:
		// Every one of these declares a byte length covering the whole of its
		// payload, which is the property that makes an unknown field skippable
		// without its sub-schema.
		length, start, ok := r.lengthOf((desc >> 4) & 0b111)
		if !ok {
			return 0, false
		}
		if length > len(r.buffer)-start {
			r.fail(ErrTruncated)
			return 0, false
		}
		return start + length - r.at, true
	default:
		r.fail(ErrBadDescriptor)
		return 0, false
	}
}

// Fail records an error from a sub-reader on its parent, which is what a caller
// descending into a composite needs: the child has its own cursor and its own
// error, and a failure inside it must stop the parent rather than be read past.
func (r *Reader8) Fail(err error) {
	if err != nil {
		r.fail(err)
	}
}

// Zero and EmptyString write a field the omit-zero rule would otherwise drop.
// See the narrow pair for why pointers need them.

// Zero writes an explicit zero integer.
func (w *Writer8) Zero(key uint8) {
	w.Buffer = append(w.Buffer, key, 0)
}

// EmptyString writes a blob of no bytes.
func (w *Writer8) EmptyString(key uint8) {
	w.Buffer = append(w.Buffer, key, descriptor(classBlob, 0), 0)
}
