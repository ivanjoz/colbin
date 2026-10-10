package wire

// The four-bit key width, K4. A field's key shares one byte with a four-bit
// nibble, and four bits have no room for a type — so the nibble does the one
// thing a reader needs before it knows the type: it says how long the field is.
// Keys are 0..15. Every multi-byte quantity is little-endian.
//
//	[key:4][nibble:4] [payload]
//
//	    nibble 0..3    no payload
//	    nibble 4..11   exactly nibble−3 bytes, one to eight
//	    nibble 12..15  [1 1 f1 f0] [length] [length bytes]
//
//	    length         one byte up to 253 · 0xFE then a u16 · 0xFF then a u32
//
// The type decides only what the bytes mean, and the flag bits f1 f0 only what
// a length-form payload means — never how long it is. So a reader steps over a
// key it does not know exactly as a wide one does (Skip), and a type can drop or
// gain a field at any id and still read what was written before.
//
//	unsigned       0..3 the value 1..4 · 4..11 the value, in the fewest bytes
//	               · 1100 with length 0 an explicit zero
//
//	signed         0..2 the value 1..3 · 3 is −1 · 4..11 a positive value
//	               · 1100 the magnitude of a negative one, 1..8 bytes
//	               (length 0 an explicit zero)
//
//	float          the unsigned shape, carrying the IEEE-754 bit pattern with its
//	               bytes reversed, so a float's low zero mantissa bytes land
//	               where the trim removes them: 1.0 costs three bytes
//
//	string/bytes   4..11 one to eight raw bytes, with no length at all
//	               · 11 f1 f0: 00 raw, 01 / 10 packed5 opening in lower / upper
//	               case (packed.go), 11 refused
//
//	integer array  11 f1 f0, f1f0 the element width: 0→1B 1→2B 2→4B 3→8B, and
//	               count = length >> f1f0. An unsigned type's elements are
//	               magnitudes, a signed type's two's complement
//
//	string array   1100, then per element
//	               [size: 1 byte, 0xFF → 4 bytes follow] [bytes: size]
//	               and no count: the reader counts the sizes it walks anyway
//
//	composite      11 0 f0 [length] [body] (narrow_composite.go)
//
// A key-less element — a map's key or value — is not a field and does not use
// this nibble: it sits inside a composite whose length already steps over it,
// and has a code table of its own (narrow_composite.go).

import (
	"encoding/binary"
	"errors"
	"math"
	"math/bits"
)

// MaxFields is what four key bits buy: keys 0..15.
const MaxFields = 16

// The nibble.
const (
	// nibbleSized is the first nibble with a payload, which is nibble−sizedBias
	// bytes long, up to maxSized at nibble 11.
	nibbleSized = 4
	sizedBias   = 3
	maxSized    = 8
	// nibbleLength is the length form, 0b11xx. Its low two bits are flags whose
	// meaning is the type's: they never change the size.
	nibbleLength uint8 = 0b1100
	flagBits     uint8 = 0b11

	// An unsigned field's nibbles 0..3 are the values 1..4. A signed field's
	// are 1, 2 and 3, and then −1 — the one negative common enough to deserve a
	// byte of its own, which every other negative pays for by riding the length
	// form.
	inlineValues   = 4
	signedInline   = 3
	signedMinusOne = 3

	// A length: one byte up to 253, and two escapes above it. The escapes name
	// the width of the length that follows rather than continuing it, so a
	// length is a branch and a load, never a loop whose trip count is data.
	inlineLength = 0xFD
	length16     = 0xFE
	length32     = 0xFF

	// maxInt is what a size is read into, and therefore the ceiling a declared
	// size is checked against before it is used.
	maxInt = int(^uint(0) >> 1)
)

// The flags of a string's length form. A narrow field has no descriptor to put
// an encoding in, so a packed string names itself here, together with the one
// bit packed5 needs and the schema cannot know: the case its stream opens in.
const (
	flagRaw         uint8 = 0b00
	flagPackedLower uint8 = 0b01
	flagPackedUpper uint8 = 0b10
)

// The narrow element code table (§5.2 of INTERNALS.md), which is not the field
// nibble: an unsigned element's code 0..7 is the value itself and 8..15 is a
// magnitude of code−7 bytes, and a signed one's is [positive:1][size:3], the
// wide INT detail.
const (
	elementInlineMax = 7
	elementWidthBase = 8
	// intPositiveFlag is bit 3 of a signed element code and of a wide INT
	// detail, directly above the three size-code bits.
	intPositiveFlag = 0b1000
)

// sizeCode8Bytes is the last integer size code, for the signed element above and
// the wide INT class. Code 0 carries no bytes at all and means the magnitude is
// one. Codes 1..6 are the byte count outright; code 7 is eight bytes, so a
// seven-byte magnitude rounds up to eight and every other width is exact.
const sizeCode8Bytes = 7

// magnitudeWidth maps a size code to its byte count.
var magnitudeWidth = [8]int{0, 1, 2, 3, 4, 5, 6, 8}

// magnitudeMask keeps the low n bytes of an eight-byte load, which is how a
// magnitude is read without a loop or a switch.
var magnitudeMask = [9]uint64{
	0,
	0xFF,
	0xFFFF,
	0xFF_FFFF,
	0xFFFF_FFFF,
	0xFF_FFFF_FFFF,
	0xFFFF_FFFF_FFFF,
	0xFF_FFFF_FFFF_FFFF,
	^uint64(0),
}

// Array element width codes.
const (
	widthCode1Byte  = 0
	widthCode2Bytes = 1
	widthCode4Bytes = 2
	widthCode8Bytes = 3
)

const (
	// inlineElementSize is the largest element length — of a string array, a
	// list element, a count — written in one byte; 0xFF escapes to four.
	inlineElementSize = 0xFE
	elementSizeEscape = 0xFF
)

var (
	// ErrSizeTooLarge is a size this platform cannot address. It is a read-side
	// error only: a writer cannot produce one, because the value it is
	// describing is already in memory.
	ErrSizeTooLarge = errors.New("wire: a declared size is larger than this platform can address")
	// ErrFieldTooWide is a peer writing a field wider than the type this record
	// says it holds — a schema disagreement, refused rather than truncated into
	// a different, valid-looking value.
	ErrFieldTooWide = errors.New("wire: a field is wider than its declared type")
	// ErrTruncated is a field, or a count of fields, that runs past the bytes
	// that hold it.
	ErrTruncated = errors.New("wire: the message ends inside a field")
	// ErrBadEscape is a narrow field in a form its type does not take: a nibble
	// with no payload where a string was expected, a length form whose flags
	// this version does not assign, a packed string read as bytes. The field is
	// still sized — Skip steps over it — but it cannot be read as asked.
	ErrBadEscape = errors.New("wire: a narrow field is in a form its type does not take")

	// errFieldTooLarge is a writer's one refusal, and a panic rather than an
	// error: a narrow length is at most four bytes, and a payload past that is a
	// wide field's job.
	errFieldTooLarge = errors.New("wire: a narrow field holds at most 2^32-1 bytes")
)

// Writer appends fields to a buffer the caller owns. Build one per message over
// a reused buffer; it holds no state but the buffer.
//
// # A write does not fail
//
// There is no error to check and no Err method. Sizes escalate rather than cap,
// up to the four-byte length a narrow field can declare — four gigabytes, past
// which the writer panics rather than write a field nothing can read — and a key
// is not data (below).
//
// # Keys are not checked, on purpose
//
// The reader defends against the network; the writer trusts its own program. A
// key is a constant of the record definition, never data, so a key above fifteen
// is a compile-time mistake and checking for it once per field measured 8 ns on
// a ten-field record — a third of the encode. Assert it where the constants
// live:
//
//	const _ = uint(15 - chargeKeyAccess4) // fails to build if a key exceeds 15
//
// A key above fifteen otherwise shifts into the next field's bits and writes a
// record nothing can read back.
type Writer struct {
	Buffer []byte
}

// sizeCodeFor is the code that describes a magnitude, and the byte count that
// code implies — which is the magnitude's own byte count except at seven, which
// rounds to eight.
func sizeCodeFor(magnitude uint64) (code uint8, width int) {
	width = (bits.Len64(magnitude) + 7) / 8
	if width >= 7 {
		return sizeCode8Bytes, 8
	}
	return uint8(width), width
}

// appendMagnitude writes a magnitude's low width bytes, little-endian.
//
// It stores all eight and cuts back rather than switching on the width: two
// instructions against a jump table. Storing exactly `width` bytes through a
// switch was tried and measured 30.5 ns against 23.0 on a six-field record — the
// jump table costs more than the discarded stores, and the extra capacity is
// wanted anyway on the buffer this is usually appending to.
func appendMagnitude(buffer []byte, magnitude uint64, width int) []byte {
	buffer = binary.LittleEndian.AppendUint64(buffer, magnitude)
	return buffer[:len(buffer)-(8-width)]
}

// appendSized writes a header whose nibble sizes a magnitude of the fewest bytes
// that hold it, and the magnitude. The nibble is the width plus sizedBias, so
// the byte count is exact at every width from one to eight.
func appendSized(buffer []byte, key uint8, value uint64) []byte {
	width := (bits.Len64(value) + 7) / 8
	return appendMagnitude(append(buffer, key<<4|uint8(width+sizedBias)), value, width)
}

// appendLength writes a length in the shortest of its three forms.
//
// A length past four bytes is a programming error rather than data: the value
// is already in memory, and a narrow field cannot say how long it is.
func appendLength(buffer []byte, length int) []byte {
	switch {
	case length <= inlineLength:
		return append(buffer, uint8(length))
	case length <= 0xFFFF:
		return append(buffer, length16, uint8(length), uint8(length>>8))
	case uint64(length) <= math.MaxUint32:
		return binary.LittleEndian.AppendUint32(append(buffer, length32), uint32(length))
	}
	panic(errFieldTooLarge)
}

// lengthBytes is what appendLength spends on a length.
func lengthBytes(length int) int {
	switch {
	case length <= inlineLength:
		return 1
	case length <= 0xFFFF:
		return 3
	}
	return 5
}

// Uint writes an unsigned integer, and writes nothing at all when it is zero.
//
// The field that fits its own nibble is inline and everything else is a call,
// deliberately: a value of one to four is what a flag, a small count or a bool
// holds, and keeping that path inside the inliner is worth more than the branch
// it costs the rest.
func (w *Writer) Uint(key uint8, value uint64) {
	// One compare covers both the inline range and the omit-zero rule, because
	// zero wraps: decremented, it is not below four, so a zero field falls
	// through to uintWide and is dropped there. The decrement is spelled in
	// place, rather than as `value-1` in the compare and again in the append,
	// because the inliner charges for each: that spelling costs 82 against a
	// budget of 80, and a Uint that does not inline puts *every* field of the
	// record through a call rather than only the wide ones. Sending a ten-field
	// record's writes out of line that way measured 18.9 ns against 7.0 on the
	// wide key's equivalent.
	if value--; value < inlineValues {
		w.Buffer = append(w.Buffer, key<<4|uint8(value))
	} else {
		w.uintWide(key, value)
	}
}

// uintWide is not inlined on purpose: it is the cold half of Uint, and letting
// it fold back in is what would push Uint itself out of the budget. It takes the
// value Uint decremented, which saves Uint the node that would undo it.
//
//go:noinline
func (w *Writer) uintWide(key uint8, less uint64) {
	if value := less + 1; value != 0 {
		w.Buffer = appendSized(w.Buffer, key, value)
	}
}

// Int writes a signed integer: a positive value as an unsigned one would be
// written, past the three that fit the nibble, and a negative one as its
// magnitude in the length form — except −1, which has a nibble of its own.
//
// It does not fit the inliner whichever way it is spelled — the signed range
// test costs a conversion the unsigned one does not — so it is one call that
// keeps every value up to 255 inside it, rather than an inline nibble case in
// front of a second call. The one-byte magnitude is tested first: it is the
// commonest value a signed field holds that is not zero, and putting it ahead
// of the nibble case measured a third of a nanosecond a field.
func (w *Writer) Int(key uint8, value int64) {
	if value > signedInline && value <= 0xFF {
		w.Buffer = append(w.Buffer, key<<4|nibbleSized, uint8(value))
		return
	}
	if uint64(value)-1 < signedInline {
		w.Buffer = append(w.Buffer, key<<4|uint8(value-1))
		return
	}
	// A zero is the commonest value a field holds, and it is dropped here
	// rather than one call further in.
	if value != 0 {
		w.intWide(key, value)
	}
}

func (w *Writer) intWide(key uint8, value int64) {
	switch {
	case value > 0:
		w.Buffer = appendSized(w.Buffer, key, uint64(value))
	case value == -1:
		w.Buffer = append(w.Buffer, key<<4|signedMinusOne)
	case value < 0:
		// Negating through uint64 rather than int64 keeps math.MinInt64, whose
		// positive counterpart does not exist as an int64.
		magnitude := -uint64(value)
		width := (bits.Len64(magnitude) + 7) / 8
		w.Buffer = appendMagnitude(
			append(w.Buffer, key<<4|nibbleLength, uint8(width)), magnitude, width)
	}
}

// Bool writes one byte when true and nothing when false.
func (w *Writer) Bool(key uint8, value bool) {
	if !value {
		return
	}
	// True is the unsigned value 1, nibble 0, which is a whole field in one byte.
	w.Buffer = append(w.Buffer, key<<4)
}

// Bytes writes a blob, and nothing when it is empty.
func (w *Writer) Bytes(key uint8, value []byte) {
	if len(value) == 0 {
		return
	}
	w.blobHeader(key, len(value))
	w.Buffer = append(w.Buffer, value...)
}

// String is Bytes for a string, which Go appends without copying it first.
func (w *Writer) String(key uint8, value string) {
	if len(value) == 0 {
		return
	}
	w.blobHeader(key, len(value))
	w.Buffer = append(w.Buffer, value...)
}

// blobHeader writes a raw string's header: the nibble alone for one to eight
// bytes — a code, a short name, a currency — and the length form past that.
//
// It is kept out of line so that String and Bytes stay inside the inliner's
// budget themselves: an append and a call is what fits.
//
//go:noinline
func (w *Writer) blobHeader(key uint8, size int) {
	if size <= maxSized {
		w.Buffer = append(w.Buffer, key<<4|uint8(size+sizedBias))
		return
	}
	w.Buffer = appendLength(append(w.Buffer, key<<4|nibbleLength|flagRaw), size)
}

// Array writers, one concrete method per element type.
//
// The repetition is not an oversight. A generic function that takes a *Writer
// is reached through a shape dictionary, and the escape information a caller in
// another package gets for it puts the writer on the heap: an allocation per
// message. Keeping the pointer out of every generic signature is what keeps a
// plan-driven encode at zero allocations; the generic work below touches slices
// and values only.
//
// Each is marked not to inline. Small enough to, they would fold into every
// switch over a field's op — codec's flat walk among them — and seven copies of
// an array's framing in the middle of the scalar arms cost the scalars their
// registers: the flat encode measured 8% slower with them inlined.

// Ints writes an array of signed integers at one width, the narrowest that holds
// every element in two's complement, and nothing when the array is empty.
//
//go:noinline
func (w *Writer) Ints(key uint8, values []int64) {
	if len(values) == 0 {
		return
	}
	w.Buffer = appendArrayField(w.Buffer, key, values)
}

// Int8s, Int16s, Int32s, Uint16s, Uint32s and Uint64s are Ints for the slice a
// caller actually holds, without the []int64 it would otherwise have to build.
// An unsigned type's elements are magnitudes rather than two's complement, so a
// []uint16 of values past 255 is two bytes each where a []int16 of them would
// still be.
//
//go:noinline
func (w *Writer) Int8s(key uint8, values []int8) {
	if len(values) == 0 {
		return
	}
	w.Buffer = appendArrayField(w.Buffer, key, values)
}

//go:noinline
func (w *Writer) Int16s(key uint8, values []int16) {
	if len(values) == 0 {
		return
	}
	w.Buffer = appendArrayField(w.Buffer, key, values)
}

//go:noinline
func (w *Writer) Int32s(key uint8, values []int32) {
	if len(values) == 0 {
		return
	}
	w.Buffer = appendArrayField(w.Buffer, key, values)
}

//go:noinline
func (w *Writer) Uint16s(key uint8, values []uint16) {
	if len(values) == 0 {
		return
	}
	w.Buffer = appendArrayField(w.Buffer, key, values)
}

//go:noinline
func (w *Writer) Uint32s(key uint8, values []uint32) {
	if len(values) == 0 {
		return
	}
	w.Buffer = appendArrayField(w.Buffer, key, values)
}

//go:noinline
func (w *Writer) Uint64s(key uint8, values []uint64) {
	if len(values) == 0 {
		return
	}
	w.Buffer = appendArrayField(w.Buffer, key, values)
}

// appendArrayField writes an integer array in the length form: the flags are
// the element width, and the length is in bytes, so the count is never written
// and a reader that does not know the key steps over the field like any other.
func appendArrayField[T Integer](buffer []byte, key uint8, values []T) []byte {
	width, code := narrowArrayWidth(values)
	buffer = appendLength(append(buffer, key<<4|nibbleLength|code), len(values)*width)
	return appendElements(buffer, values, width)
}

// narrowArrayWidth is the one pass that picks the width, and the element type
// alone decides what the width has to hold: a signed type's elements go as two's
// complement and an unsigned type's as magnitudes, whatever the values are.
// There is no flag on the wire saying which, because the reader has the type.
//
// It ORs rather than compares. Every width boundary is a power of two less one,
// so the OR of the values crosses a boundary exactly when the largest does, and
// the loop has no branch in it. A signed value is folded first — a negative one
// to its complement, −1 to 0 and −128 to 127 — which leaves the bits a two's
// complement element needs, less the sign bit the shift puts back.
func narrowArrayWidth[T Integer](values []T) (width int, code uint8) {
	var used uint64
	if isSigned[T]() {
		for _, value := range values {
			signed := int64(value)
			used |= uint64(signed ^ signed>>63)
		}
		used <<= 1
	} else {
		for _, value := range values {
			used |= uint64(value)
		}
	}
	switch {
	case used <= 0xFF:
		return 1, widthCode1Byte
	case used <= 0xFFFF:
		return 2, widthCode2Bytes
	case used <= 0xFFFF_FFFF:
		return 4, widthCode4Bytes
	}
	return 8, widthCode8Bytes
}

// Strings writes each element behind its own size, inside one length that lets
// the whole field be skipped. There is no count: a reader walks the sizes to
// read the elements anyway, and counting them as it goes costs nothing.
//
// The element size is one byte with an escape rather than a fixed two for the
// same reason a length escalates: it removes the ceiling, and it makes the
// common element — anything under 255 bytes, which is every code line and most
// error texts — cost one byte instead of two.
func (w *Writer) Strings(key uint8, values []string) {
	if len(values) == 0 {
		return
	}
	payload := 0
	for _, value := range values {
		payload += elementSizeBytes(len(value)) + len(value)
	}
	w.Buffer = appendLength(append(w.Buffer, key<<4|nibbleLength), payload)
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

// elementSizeBytes is what an element size costs: one byte, or the escape and
// four.
func elementSizeBytes(size int) int {
	if size <= inlineElementSize {
		return 1
	}
	return 5
}

// Reader walks a message field by field. The caller switches on Key and calls
// the read for the type that key holds, which it knows from the record
// definition, and Skip for a key it does not know.
type Reader struct {
	buffer []byte
	at     int
	err    error
}

// NewReader starts a reader over one message.
func NewReader(message []byte) Reader { return Reader{buffer: message} }

// More reports whether another field follows. It goes false on the first error,
// so a decode loop ends rather than spinning on a broken message — which is why
// it does not have to test the error itself: fail parks the cursor at the end of
// the buffer, so one comparison answers both questions. That matters because
// More runs once per field and measured 12% of a ten-field decode.
func (r *Reader) More() bool { return r.at < len(r.buffer) }

// Key is the field the cursor is on. It does not advance: the typed read does.
// Only valid while More reports true.
func (r *Reader) Key() uint8 { return r.buffer[r.at] >> 4 }

// Err reports the first failure. A decode must check it before using the record:
// every read answers zero after a failure, which is indistinguishable from a
// field that was legitimately omitted.
func (r *Reader) Err() error { return r.err }

func (r *Reader) fail(err error) {
	if r.err == nil {
		r.err = err
	}
	// Parking the cursor at the end is what stops More from looping.
	r.at = len(r.buffer)
}

// header returns the byte the cursor is on, or false at the end of the message.
// Every typed read goes through it, so a caller that reads past the last field
// gets an error instead of a panic.
func (r *Reader) header() (uint8, bool) {
	if r.at >= len(r.buffer) {
		r.fail(ErrTruncated)
		return 0, false
	}
	return r.buffer[r.at], true
}

// field reads the header at the cursor and sizes the field from its nibble
// alone, returning the nibble and where the payload is, and advancing past the
// whole field. It is the one place a narrow field is sized — Skip is this and
// nothing more, and every read that is not on an inline fast path comes through
// here — so a read and a skip cannot disagree about where the next field starts.
func (r *Reader) field() (nibble uint8, start, size int, ok bool) {
	header, ok := r.header()
	if !ok {
		return 0, 0, 0, false
	}
	nibble = header & 0b1111
	start = r.at + 1
	switch {
	case nibble < nibbleSized:
	case nibble < nibbleLength:
		size = int(nibble) - sizedBias
	default:
		var err error
		if size, start, err = readLength(r.buffer, start); err != nil {
			r.fail(err)
			return 0, 0, 0, false
		}
	}
	if size > len(r.buffer)-start {
		r.fail(ErrTruncated)
		return 0, 0, 0, false
	}
	r.at = start + size
	return nibble, start, size, true
}

// readLength reads a length whose first byte is at `at`, returning it with the
// offset just past it. Any of the three forms is accepted for any length: the
// writer's choosing the shortest is a property of the writer, not a rule a
// reader has to police.
func readLength(buffer []byte, at int) (length, start int, err error) {
	if at >= len(buffer) {
		return 0, 0, ErrTruncated
	}
	switch first := buffer[at]; first {
	case length16:
		if len(buffer)-at < 3 {
			return 0, 0, ErrTruncated
		}
		return int(binary.LittleEndian.Uint16(buffer[at+1:])), at + 3, nil
	case length32:
		if len(buffer)-at < 5 {
			return 0, 0, ErrTruncated
		}
		value := binary.LittleEndian.Uint32(buffer[at+1:])
		if uint64(value) > uint64(maxInt) {
			return 0, 0, ErrSizeTooLarge
		}
		return int(value), at + 5, nil
	default:
		return int(first), at + 1, nil
	}
}

// Skip steps over the field at the cursor without knowing what it is, and
// reports false — with Err set — when the field runs past the message. The
// nibble sizes every field whatever its type, so this is one branch and, for the
// length form, one length read: the same capability Reader8.Skip has, at a byte
// less per field.
func (r *Reader) Skip() bool {
	_, _, _, ok := r.field()
	return ok
}

// Uint reads an unsigned integer field.
//
// Split for the same reason the writer is: the one- and two-byte fields are the
// common ones, and their path is spelled out here with everything else one call
// away.
func (r *Reader) Uint() uint64 {
	if field := r.buffer[r.at:]; len(field) >= 1 {
		if nibble := field[0] & 0b1111; nibble < inlineValues {
			r.at++
			return uint64(nibble) + 1
		} else if nibble == nibbleSized && len(field) >= 2 {
			r.at += 2
			return uint64(field[1])
		}
	}
	return r.uintWide()
}

func (r *Reader) uintWide() uint64 {
	nibble, start, size, ok := r.field()
	switch {
	case !ok:
		return 0
	case nibble < nibbleSized:
		return uint64(nibble) + 1
	case nibble < nibbleLength:
		return leUint(r.buffer[start:], size)
	}
	r.explicitZero(nibble, size)
	return 0
}

// explicitZero checks that a length-form field on an integer is the one form
// an integer takes there, `1100` with nothing in it.
func (r *Reader) explicitZero(nibble uint8, size int) {
	if nibble != nibbleLength || size != 0 {
		r.fail(ErrBadEscape)
	}
}

// leUint reads a magnitude's width bytes as a little-endian integer. The whole
// eight bytes are loaded and masked when the buffer has them, which is one
// instruction where a switch over the widths would be a jump table; the tail of
// a message takes the loop instead.
func leUint(buffer []byte, width int) uint64 {
	if len(buffer) >= 8 {
		return binary.LittleEndian.Uint64(buffer) & magnitudeMask[width]
	}
	var value uint64
	for index := width - 1; index >= 0; index-- {
		value = value<<8 | uint64(buffer[index])
	}
	return value
}

// Int reads a signed integer field.
//
// It is not Uint with a sign applied: the two tables part at nibble 3, which is
// the value 4 to an unsigned field and −1 to a signed one, and the length form
// carries a negative magnitude here and only an explicit zero there.
func (r *Reader) Int() int64 {
	if field := r.buffer[r.at:]; len(field) >= 1 {
		if nibble := field[0] & 0b1111; nibble < signedInline {
			r.at++
			return int64(nibble) + 1
		} else if nibble == nibbleSized && len(field) >= 2 {
			r.at += 2
			return int64(field[1])
		}
	}
	return r.intWide()
}

func (r *Reader) intWide() int64 {
	nibble, start, size, ok := r.field()
	switch {
	case !ok:
		return 0
	case nibble < signedMinusOne:
		return int64(nibble) + 1
	case nibble == signedMinusOne:
		return -1
	case nibble < nibbleLength:
		magnitude := leUint(r.buffer[start:], size)
		if magnitude > math.MaxInt64 {
			r.fail(ErrFieldTooWide)
			return 0
		}
		return int64(magnitude)
	case nibble != nibbleLength:
		r.fail(ErrBadEscape)
		return 0
	}
	// The length form: a negative value's magnitude, or nothing at all for an
	// explicit zero.
	if size > 8 {
		r.fail(ErrFieldTooWide)
		return 0
	}
	magnitude := leUint(r.buffer[start:], size)
	if magnitude > 1<<63 {
		r.fail(ErrFieldTooWide)
		return 0
	}
	return -int64(magnitude)
}

// Bool reads a field written by Bool. Absent fields never reach here: a false
// bool is not written, so the key is simply missing.
func (r *Reader) Bool() bool { return r.Uint() == 1 }

// Bytes returns the field's bytes as a sub-slice of the message, without
// copying. It stays valid only as long as the message buffer does. A packed5
// string is not bytes and is refused with ErrBadEscape; String reads it.
func (r *Reader) Bytes() []byte {
	nibble, start, size, ok := r.field()
	if !ok {
		return nil
	}
	if nibble < nibbleSized || nibble > nibbleLength|flagRaw {
		r.fail(ErrBadEscape)
		return nil
	}
	return r.buffer[start : start+size]
}

// String reads a string written by String or by PackedString. The flags say
// which, so a reader needs no configuration and cannot be wrong about it.
//
// The common field is a raw string of up to eight bytes, or one with a one-byte
// length, so those two paths are spelled out here and everything else is one
// call away.
func (r *Reader) String() string {
	at := r.at
	if at+2 <= len(r.buffer) {
		nibble, next := r.buffer[at]&0b1111, r.buffer[at+1]
		if nibble == nibbleLength|flagRaw && next <= inlineLength {
			if size := int(next); size <= len(r.buffer)-at-2 {
				r.at = at + 2 + size
				return string(r.buffer[at+2 : at+2+size])
			}
		} else if size := int(nibble) - sizedBias; size > 0 && size <= maxSized && size <= len(r.buffer)-at-1 {
			r.at = at + 1 + size
			return string(r.buffer[at+1 : at+1+size])
		}
	}
	return r.stringWide()
}

// Array readers, the mirror of the writers: one concrete method per element
// type, for the reason given there — a *Reader in a generic signature costs the
// caller an allocation. The type decides how the elements read back, as it
// decided how they were written: sign-extended for a signed type, zero-extended
// for an unsigned one.

// Ints appends the array's elements to dst, which may be nil.
func (r *Reader) Ints(dst []int64) []int64 {
	if elements, width, ok := r.arrayElements(); ok {
		return appendTwosComplement(dst, elements, width)
	}
	return dst
}

func (r *Reader) Int8s(dst []int8) []int8 {
	if elements, width, ok := r.arrayElements(); ok {
		return appendTwosComplement(dst, elements, width)
	}
	return dst
}

func (r *Reader) Int16s(dst []int16) []int16 {
	if elements, width, ok := r.arrayElements(); ok {
		return appendTwosComplement(dst, elements, width)
	}
	return dst
}

func (r *Reader) Int32s(dst []int32) []int32 {
	if elements, width, ok := r.arrayElements(); ok {
		return appendTwosComplement(dst, elements, width)
	}
	return dst
}

func (r *Reader) Uint16s(dst []uint16) []uint16 {
	if elements, width, ok := r.arrayElements(); ok {
		return appendMagnitudes(dst, elements, width)
	}
	return dst
}

func (r *Reader) Uint32s(dst []uint32) []uint32 {
	if elements, width, ok := r.arrayElements(); ok {
		return appendMagnitudes(dst, elements, width)
	}
	return dst
}

func (r *Reader) Uint64s(dst []uint64) []uint64 {
	if elements, width, ok := r.arrayElements(); ok {
		return appendMagnitudes(dst, elements, width)
	}
	return dst
}

// arrayElements reads an array field and returns its payload as a sub-slice of
// the message, with the element width its flags declare. It advances the
// cursor: the caller only has to turn bytes into elements. A length that is not
// a whole number of elements is refused rather than read short.
func (r *Reader) arrayElements() (elements []byte, width int, ok bool) {
	nibble, start, size, ok := r.field()
	if !ok {
		return nil, 0, false
	}
	if nibble < nibbleLength {
		r.fail(ErrBadEscape)
		return nil, 0, false
	}
	width = 1 << (nibble & flagBits)
	if size&(width-1) != 0 {
		r.fail(ErrTruncated)
		return nil, 0, false
	}
	return r.buffer[start : start+size], width, true
}

// stringElements reads a string array's framing and returns a reader over
// exactly its elements. An element is [size][bytes], the shape a list element
// has, so Element reads it — and reading inside the field's own bytes is what
// keeps a bad element size from running into the field that follows.
func (r *Reader) stringElements() (Reader, bool) {
	nibble, start, size, ok := r.field()
	if !ok {
		return Reader{}, false
	}
	if nibble != nibbleLength {
		r.fail(ErrBadEscape)
		return Reader{}, false
	}
	return Reader{buffer: r.buffer[start : start+size]}, true
}

// StringsBytes appends each element to dst as a sub-slice of the message,
// without copying. The slices stay valid only as long as the message buffer
// does.
func (r *Reader) StringsBytes(dst [][]byte) [][]byte {
	elements, ok := r.stringElements()
	for ok && elements.More() {
		element, ok := elements.Element()
		if !ok {
			r.fail(elements.err)
			return dst
		}
		dst = append(dst, element)
	}
	return dst
}

// Strings copies each element into a Go string.
func (r *Reader) Strings(dst []string) []string {
	elements, ok := r.stringElements()
	for ok && elements.More() {
		element, ok := elements.Element()
		if !ok {
			r.fail(elements.err)
			return dst
		}
		dst = append(dst, string(element))
	}
	return dst
}

// Width-typed writers.
//
// Uint takes a uint64 and therefore carries every width the format has, which is
// more code than the inliner will take. A field's Go type already fixes how wide
// it can be: a uint16 is one byte or two and never three, so U16 is two appends
// with no call under them. A generator picks the one that matches the field.

// U16 writes a field whose type cannot exceed two bytes.
func (w *Writer) U16(key uint8, value uint16) {
	if value == 0 {
		return
	}
	if value <= inlineValues {
		w.Buffer = append(w.Buffer, key<<4|uint8(value-1))
		return
	}
	if value <= 0xFF {
		w.Buffer = append(w.Buffer, key<<4|nibbleSized, uint8(value))
		return
	}
	w.Buffer = append(w.Buffer, key<<4|nibbleSized+1, uint8(value), uint8(value>>8))
}

// U32 writes a field whose type cannot exceed four bytes.
//
// It is shaped like Uint and for the same reason: one compare, one append and
// one call is all the inliner's budget holds.
func (w *Writer) U32(key uint8, value uint32) {
	if value--; value < inlineValues {
		w.Buffer = append(w.Buffer, key<<4|uint8(value))
	} else {
		w.u32Wide(key, value)
	}
}

// u32Wide takes the value U32 decremented, as uintWide takes Uint's.
//
//go:noinline
func (w *Writer) u32Wide(key uint8, less uint32) {
	switch value := less + 1; {
	case value == 0:
	case value <= 0xFF:
		w.Buffer = append(w.Buffer, key<<4|nibbleSized, uint8(value))
	case value <= 0xFFFF:
		w.Buffer = append(w.Buffer, key<<4|nibbleSized+1,
			uint8(value), uint8(value>>8))
	case value <= 0xFF_FFFF:
		w.Buffer = append(w.Buffer, key<<4|nibbleSized+2,
			uint8(value), uint8(value>>8), uint8(value>>16))
	default:
		w.Buffer = append(w.Buffer, key<<4|nibbleSized+3,
			uint8(value), uint8(value>>8), uint8(value>>16), uint8(value>>24))
	}
}

// I32 writes a signed field no wider than four bytes, as Int does. It is Int
// spelled again at 32 bits rather than Int behind a widening, which inlined and
// still measured slower: the same one call, with the comparisons at the width
// the field already has.
func (w *Writer) I32(key uint8, value int32) {
	if value > signedInline && value <= 0xFF {
		w.Buffer = append(w.Buffer, key<<4|nibbleSized, uint8(value))
		return
	}
	if uint32(value)-1 < signedInline {
		w.Buffer = append(w.Buffer, key<<4|uint8(value-1))
		return
	}
	if value != 0 {
		w.intWide(key, int64(value))
	}
}

// Width-typed readers, the mirror of the writers above.

// U16 reads a field written by U16, or by any writer that kept it under 65 536.
//
// Both widths a uint16 can occupy are inline. Only the one-byte case was, at
// first, and a uint16 field holding something above 255 is not the exception —
// it is what the type is for, so every such field was paying a call. This and
// More's dropped error test together took a ten-field decode from 19.2 ns to
// 13.9.
func (r *Reader) U16() uint16 {
	field := r.buffer[r.at:]
	if len(field) >= 3 {
		switch nibble := field[0] & 0b1111; {
		case nibble < inlineValues:
			r.at++
			return uint16(nibble) + 1
		case nibble == nibbleSized:
			r.at += 2
			return uint16(field[1])
		case nibble == nibbleSized+1:
			r.at += 3
			return uint16(field[1]) | uint16(field[2])<<8
		}
	} else if len(field) >= 1 {
		if nibble := field[0] & 0b1111; nibble < inlineValues {
			r.at++
			return uint16(nibble) + 1
		} else if nibble == nibbleSized && len(field) == 2 {
			r.at += 2
			return uint16(field[1])
		}
	}
	return r.u16Wide()
}

func (r *Reader) u16Wide() uint16 {
	value := r.uintWide()
	if value > 0xFFFF {
		r.fail(ErrFieldTooWide)
		return 0
	}
	return uint16(value)
}

// U32 reads a field written by U32, or by any writer that kept it under 2^32.
func (r *Reader) U32() uint32 {
	if field := r.buffer[r.at:]; len(field) >= 1 {
		if nibble := field[0] & 0b1111; nibble < inlineValues {
			r.at++
			return uint32(nibble) + 1
		} else if nibble == nibbleSized && len(field) >= 2 {
			r.at += 2
			return uint32(field[1])
		}
	}
	return r.u32Wide()
}

func (r *Reader) u32Wide() uint32 {
	value := r.uintWide()
	if value > 0xFFFF_FFFF {
		r.fail(ErrFieldTooWide)
		return 0
	}
	return uint32(value)
}

// U8 reads an unsigned field into a byte, refusing a wider one as U16 does.
func (r *Reader) U8() uint8 {
	value := r.Uint()
	if value > 0xFF {
		r.fail(ErrFieldTooWide)
		return 0
	}
	return uint8(value)
}

// I8, I16 and I32 read a signed field into a narrower type, refusing a value
// the type cannot hold for the reason U16 does. Each is written out rather than
// sharing a range helper, so that it inlines into the caller's loop.
func (r *Reader) I8() int8 {
	value := r.Int()
	if int64(int8(value)) != value {
		r.fail(ErrFieldTooWide)
		return 0
	}
	return int8(value)
}

func (r *Reader) I16() int16 {
	value := r.Int()
	if int64(int16(value)) != value {
		r.fail(ErrFieldTooWide)
		return 0
	}
	return int16(value)
}

func (r *Reader) I32() int32 {
	value := r.Int()
	if int64(int32(value)) != value {
		r.fail(ErrFieldTooWide)
		return 0
	}
	return int32(value)
}

// Floats ride in the unsigned field shape, carrying the IEEE-754 bit pattern
// with its bytes reversed.
//
// The reversal is what makes the trim work at all. An integer's zero bytes are
// its most significant ones, so writing it little-endian and dropping the top
// puts the zeros where the nibble's byte count can elide them. A float's zero
// bytes are its *least* significant — the low mantissa bits — while its exponent
// and sign are never zero, so trimming a float the same way as an integer saves
// nothing. Reversing the bytes swaps the two ends and the integer path then works
// unchanged: 1.0 costs three bytes rather than nine, and a float64 holding a
// value that is exactly a float32 has twenty-nine zero low bits — three whole
// bytes and five over, and only whole bytes trim — so it costs six.
//
// Positive zero is not written at all, like every other zero value.

// F32 writes a float32 as its reversed bit pattern.
func (w *Writer) F32(key uint8, value float32) {
	w.Uint(key, uint64(bits.ReverseBytes32(math.Float32bits(value))))
}

// F64 writes a float64 as its reversed bit pattern.
func (w *Writer) F64(key uint8, value float64) {
	w.Uint(key, bits.ReverseBytes64(math.Float64bits(value)))
}

// F32 reads a field written by F32.
func (r *Reader) F32() float32 {
	return math.Float32frombits(bits.ReverseBytes32(uint32(r.Uint())))
}

// F64 reads a field written by F64.
func (r *Reader) F64() float64 {
	return math.Float64frombits(bits.ReverseBytes64(r.Uint()))
}

// Integer is every integer type an array's elements can be.
type Integer interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64
}

// appendElements writes the elements with the width hoisted out of the loop, so
// each one is a store rather than a switch. That hoist is the difference between
// a byte-aligned array and a fast one.
//
// It takes the buffer rather than the writer, which is not a style choice: a
// generic function reached through a shape dictionary has its pointer parameters
// marked as escaping, so a *Writer here would put every writer in the program on
// the heap — including the one in codec's plan walk, which measured 29.6 ns and
// an allocation per record against 19.5 and none.
func appendElements[T Integer](buffer []byte, values []T, width int) []byte {
	switch width {
	case 1:
		for _, value := range values {
			buffer = append(buffer, uint8(value))
		}
	case 2:
		for _, value := range values {
			buffer = append(buffer, uint8(value), uint8(uint64(value)>>8))
		}
	case 4:
		for _, value := range values {
			buffer = binary.LittleEndian.AppendUint32(buffer, uint32(value))
		}
	default:
		for _, value := range values {
			buffer = binary.LittleEndian.AppendUint64(buffer, uint64(value))
		}
	}
	return buffer
}

// isSigned reports whether T is a signed integer type, which the zero value
// answers without reflection: its complement is −1 for a signed type and the
// type's largest value for an unsigned one.
//
// It used to convert −1 to T and back through int64, which a uint64 — and a
// uint, on a 64-bit platform — survives unchanged, so it answered true for the
// two widest unsigned types. The wide VEC still writes what that rule wrote; see
// vecSigned.
func isSigned[T Integer]() bool {
	var zero T
	return ^zero < zero
}

// appendArray turns an array field's bytes into elements, by the sign flag a
// wide VEC carries. Generic over the element type and touching no pointer, so it
// is safe to reach from anywhere.
func appendArray[T Integer](dst []T, elements []byte, width int, positive bool) []T {
	if positive {
		return appendMagnitudes(dst, elements, width)
	}
	return appendTwosComplement(dst, elements, width)
}

// appendMagnitudes and appendTwosComplement read the elements with the width
// hoisted out of the loop, the mirror of appendElements.
func appendMagnitudes[T Integer](dst []T, elements []byte, width int) []T {
	switch width {
	case 1:
		for _, raw := range elements {
			dst = append(dst, T(raw))
		}
	case 2:
		for at := 0; at < len(elements); at += 2 {
			dst = append(dst, T(binary.LittleEndian.Uint16(elements[at:])))
		}
	case 4:
		for at := 0; at < len(elements); at += 4 {
			dst = append(dst, T(binary.LittleEndian.Uint32(elements[at:])))
		}
	default:
		for at := 0; at < len(elements); at += 8 {
			dst = append(dst, T(binary.LittleEndian.Uint64(elements[at:])))
		}
	}
	return dst
}

func appendTwosComplement[T Integer](dst []T, elements []byte, width int) []T {
	switch width {
	case 1:
		for _, raw := range elements {
			dst = append(dst, T(int8(raw)))
		}
	case 2:
		for at := 0; at < len(elements); at += 2 {
			dst = append(dst, T(int16(binary.LittleEndian.Uint16(elements[at:]))))
		}
	case 4:
		for at := 0; at < len(elements); at += 4 {
			dst = append(dst, T(int32(binary.LittleEndian.Uint32(elements[at:]))))
		}
	default:
		for at := 0; at < len(elements); at += 8 {
			dst = append(dst, T(int64(binary.LittleEndian.Uint64(elements[at:]))))
		}
	}
	return dst
}

// Zero writes a field that the omit-zero rule would otherwise drop.
//
// It exists for pointers. An absent key means a nil pointer, so a non-nil
// pointer *to* a zero value has to put something on the wire or the two would be
// indistinguishable — which is the one place this format needs to say "zero" out
// loud rather than by omission.
//
// It is one writer for every type: the length form with nothing in it, which an
// integer, a float and a bool read as zero, a string as empty and an array as
// having no elements. The nibble sizes the field rather than typing it, so
// nothing about a zero depends on what it is a zero of.
func (w *Writer) Zero(key uint8) {
	w.Buffer = append(w.Buffer, key<<4|nibbleLength, 0)
}
