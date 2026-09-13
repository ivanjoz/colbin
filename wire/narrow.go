// Package narrow implements colbin's wire format for a single record of at
// most sixteen primitive fields, where a zero-valued field is not written at
// all.
//
//	w := wire.Writer{}
//	w.U32(0, companyID)
//	w.U16(1, routeID)
//	w.String(2, name)
//	send(w.Buffer)
//
// This package owns the field framing and nothing else. It has no reflection and
// no type registry: a caller that already knows the Go type drives it, and
// colbin.MarshalMinimal is the reflection façade over it.
//
// # Everything is byte-aligned
//
// Nothing is packed across a byte boundary, so an encode is a header byte and a
// few stores, a decode is a switch over the key, and a string field is a
// sub-slice of the message rather than a copy. No size is a varint: a header
// carries the common size and escalates to a fixed width, so no read is ever a
// loop whose trip count is data. See BYTE_ALIGNED_PLAN.md for what that buys and
// what it costs.
//
// # Wire format
//
// A message is a sequence of fields and ends when its buffer ends — the frame
// that carries it already states its length. Keys are 0..15. Every multi-byte
// quantity is little-endian, which is one native load on every machine this runs
// on. A field whose value is zero, empty or false is omitted, which is where
// most of the saving comes from.
//
//	integer        [key:4][positive:1][n:3]                    [magnitude: n bytes]
//
//	    n          0 → no bytes, the value is 1 (which is what makes a true bool
//	               one byte) · 1..6 → that many bytes · 7 → eight bytes
//	    positive   1 = the bytes are a magnitude; 0 = negative, bytes are |value|
//	    an integer needs no continuation flag: n already reaches eight bytes
//
//	float          the integer shape, carrying the IEEE-754 bit pattern with its
//	               bytes reversed
//
//	    A float's zero bytes are its low mantissa bytes, where an integer's are
//	    its high ones, so reversing puts them where n can elide them. 1.0 costs
//	    two bytes rather than eight, and a float64 that holds an exact float32
//	    costs five without anything being added to detect it.
//
//	string/bytes   [key:4][more:1][size hi:3] [size lo:8]      [bytes: size]
//	               [key:4][1][escape:3]       [size: 2|4|8]    [bytes: size]
//
//	    2047 bytes fit the two header bytes. Past that the three size bits carry
//	    nothing, so they name the width of the size that follows instead: a 5 KB
//	    string pays one extra byte, not four.
//
//	integer array  [key:4][positive:1][width:2][more:1] [count:8]  [count × width]
//	               ... [count: 4 bytes] instead, when more = 1
//
//	    width code 0→1B  1→2B  2→4B  3→8B, taken from the widest element
//	    positive   1 = magnitudes; 0 = two's complement at that width
//
//	string array   [key:4][more:1][count hi:3] [count lo:8]   then per element
//	               [size: 1 byte, 0xFF → 4 bytes follow] [bytes: size]
//
// # Nothing has a size ceiling
//
// Every size escalates to a width that holds it, so the common size costs
// nothing extra and no size is refused. That is also why **a write cannot
// fail**: Writer has no error and no Err method. Anything the format could not
// express would have to be a value that does not fit in memory.
package wire

import (
	"encoding/binary"
	"errors"
	"math"
	"math/bits"
)

// MaxFields is what four key bits buy: keys 0..15. A record needing a
// seventeenth field needs a different key width, not a wider key — the key
// shares a byte with the field's own header bits and there is nothing to take
// them from.
const MaxFields = 16

// Integer size codes. Code 0 carries no bytes at all and means the magnitude is
// one, which is what makes a true bool a single byte. Codes 1..6 are the byte
// count outright; code 7 is eight bytes, so a seven-byte magnitude rounds up to
// eight and every other width is exact.
const (
	sizeCodeOne    = 0
	sizeCode8Bytes = 7
)

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
	// moreSizeFlag says the size or count in this header did not fit its three
	// inline bits and that a wider one follows. It is bit 3 in a blob header and
	// bit 0 in an array header, because an array spends bits 2..1 on its element
	// width. A blob header byte is [key:4][more:1][size hi:3]; an array header
	// byte is [key:4][positive:1][width:2][more:1].
	moreSizeFlag     = 0b1000
	moreArrayLenFlag = 0b0001
	arrayWidthShift  = 1
	// intPositiveFlag is bit 3 of a *signed* integer header, directly above the
	// three size-code bits. arrayPositiveFlag is bit 3 of an array header. They
	// are deliberately separate constants: one value used for both would land on
	// a size-code bit.
	intPositiveFlag   = 0b1000
	arrayPositiveFlag = 0b1000

	// An unsigned narrow field spends no sign bit, so all sixteen nibble codes
	// carry information rather than eight.
	//
	// A K4 reader has the schema and therefore already knows whether the field it
	// is looking at is signed. A `positive` bit on a uint64 is a bit that is
	// always set — one of four, on the field width that most of this wire uses.
	// Reclaiming it buys two things: the values 0..7 cost a single byte with no
	// payload at all, and the eight width codes become exact, so a seven-byte
	// magnitude no longer rounds up to eight the way the signed form still does.
	//
	//	nibble 0..7   the value itself, no payload
	//	nibble 8..15  a magnitude of (nibble - 7) bytes, little-endian
	//
	// Signed fields keep [positive:1][size:3] unchanged. The two forms share the
	// nibble and mean different things in it, which is safe for exactly the reason
	// K4 exists at all: nothing reads a narrow field without knowing its type.
	uintInlineMax = 7
	uintWidthBase = 8

	// Escape codes, which occupy a blob header's three size bits once more is
	// set and they no longer carry size.
	escape2Bytes = 0
	escape4Bytes = 1
	escape8Bytes = 2

	// The packed5 escapes. A narrow blob header has no enc field — under K4 the
	// schema says what a field is, not the wire — so a packed string names
	// itself here instead, and carries the one bit the schema cannot know: the
	// case mode its unit stream opens in.
	//
	// Spending four codes on it buys a two-byte header, the same as a raw blob's,
	// where a separate flag byte would have cost three. Code 7 is still free.
	escapePacked1Lo = 3 // 1-byte size, stream opens lowercase
	escapePacked1Up = 4 // 1-byte size, stream opens uppercase
	escapePacked4Lo = 5 // 4-byte size, stream opens lowercase
	escapePacked4Up = 6 // 4-byte size, stream opens uppercase

	// What each header carries before an escape is needed.
	inlineBlobSize       = 1<<11 - 1
	inlineArrayCount     = 1<<8 - 1
	inlineStringArrayLen = 1<<11 - 1

	// inlineElementSize is the largest element length a string array writes in
	// one byte; 0xFF escapes to four bytes.
	inlineElementSize = 0xFE
	elementSizeEscape = 0xFF

	// maxInt is what a size is read into, and therefore the ceiling a declared
	// size is checked against before it is used.
	maxInt = int(^uint(0) >> 1)
)

var (
	// ErrSizeTooLarge is a size this platform cannot address. It is a read-side
	// error only: a writer cannot produce one, because the value it is
	// describing is already in memory.
	ErrSizeTooLarge = errors.New("narrow: a declared size is larger than this platform can address")
	// ErrFieldTooWide is a peer writing a field wider than the type this record
	// says it holds — a schema disagreement, refused rather than truncated into
	// a different, valid-looking value.
	ErrFieldTooWide = errors.New("narrow: a field is wider than its declared type")
	ErrTruncated    = errors.New("narrow: the message ends inside a field")
	// ErrBadEscape is a size escape code this version does not assign.
	ErrBadEscape = errors.New("narrow: unassigned size escape code")
)

// Writer appends fields to a buffer the caller owns. Build one per message over
// a reused buffer; it holds no state but the buffer.
//
// # A write cannot fail
//
// There is no error to check and no Err method. Sizes escalate rather than cap,
// so nothing a caller can hold in memory is too large to describe, and a key is
// not data (below).
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

// Reset points the writer at a buffer, keeping its capacity.
func (w *Writer) Reset(buffer []byte) {
	w.Buffer = buffer[:0]
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

// Uint writes an unsigned integer, and writes nothing at all when it is zero.
//
// The field that fits its own nibble is inline and everything else is a call,
// deliberately: a value under eight is what a flag, a small count or a bool
// holds, and keeping that path inside the inliner is worth more than the branch
// it costs the rest.
func (w *Writer) Uint(key uint8, value uint64) {
	// One compare covers both the inline range and the omit-zero rule, because
	// zero wraps: `0-1` is not below eight, so a zero field falls through to
	// uintWide and is dropped there. Two compares here — the obvious spelling —
	// cost 83 against the inliner's budget of 80, and a Uint that does not
	// inline puts *every* field of the record through a call rather than only
	// the wide ones. Sending a ten-field record's writes out of line that way
	// measured 18.9 ns against 7.0 on the wide key's equivalent.
	if value-1 < uintInlineMax {
		w.Buffer = append(w.Buffer, key<<4|uint8(value))
		return
	}
	w.uintWide(key, value)
}

// uintWide is not inlined on purpose: it is the cold half of Uint, and letting
// it fold back in is what would push Uint itself out of the budget.
//
//go:noinline
func (w *Writer) uintWide(key uint8, value uint64) {
	if value == 0 {
		return
	}
	width := (bits.Len64(value) + 7) / 8
	w.Buffer = appendMagnitude(
		append(w.Buffer, key<<4|uintWidthBase+uint8(width)-1), value, width)
}

// Int writes a signed integer as a sign bit and a magnitude.
func (w *Writer) Int(key uint8, value int64) {
	if value > 0 && value <= 0xFF {
		w.Buffer = append(w.Buffer, key<<4|intPositiveFlag|1, uint8(value))
		return
	}
	if value == 0 {
		return
	}
	w.intWide(key, value)
}

func (w *Writer) intWide(key uint8, value int64) {
	header := key<<4 | intPositiveFlag
	magnitude := uint64(value)
	if value < 0 {
		header = key << 4
		// Negating through uint64 rather than int64 keeps math.MinInt64, whose
		// positive counterpart does not exist as an int64.
		magnitude = -uint64(value)
	}
	w.magnitude(header, magnitude)
}

// Bool writes one byte when true and nothing when false.
func (w *Writer) Bool(key uint8, value bool) {
	if !value {
		return
	}
	// True is the unsigned inline value 1, which is a whole field in one byte.
	w.Buffer = append(w.Buffer, key<<4|1)
}

// magnitude writes the header and as many bytes as the value actually needs.
func (w *Writer) magnitude(header uint8, magnitude uint64) {
	if magnitude == 1 {
		w.Buffer = append(w.Buffer, header|sizeCodeOne)
		return
	}
	code, width := sizeCodeFor(magnitude)
	w.Buffer = appendMagnitude(append(w.Buffer, header|code), magnitude, width)
}

// Bytes writes a length-prefixed blob, and nothing when it is empty.
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

// blobHeader writes the header a string, blob or string array carries: two bytes
// holding an eleven-bit size, or one byte and a wider size when that will not
// hold it.
func (w *Writer) blobHeader(key uint8, size int) {
	if size <= inlineBlobSize {
		w.Buffer = append(w.Buffer, key<<4|uint8(size>>8)&0b111, uint8(size))
		return
	}
	w.escapedSize(key, uint64(size))
}

// escapedSize writes the one-byte header whose three size bits name the width of
// the size that follows it.
func (w *Writer) escapedSize(key uint8, size uint64) {
	switch {
	case size <= 0xFFFF:
		w.Buffer = append(w.Buffer, key<<4|moreSizeFlag|escape2Bytes,
			uint8(size), uint8(size>>8))
	case size <= 0xFFFF_FFFF:
		w.Buffer = binary.LittleEndian.AppendUint32(
			append(w.Buffer, key<<4|moreSizeFlag|escape4Bytes), uint32(size))
	default:
		w.Buffer = binary.LittleEndian.AppendUint64(
			append(w.Buffer, key<<4|moreSizeFlag|escape8Bytes), size)
	}
}

// Array writers, one concrete method per element type.
//
// These are not wrappers over the generic WriteInts, and the repetition is not
// an oversight. A generic function that takes a *Writer is reached through a
// shape dictionary, and the escape information a *caller in another package*
// gets for it is conservative enough to put the writer on the heap — measured,
// an allocation and 3 ns per record on codec's plan walk. Keeping the pointer
// out of every generic signature is what keeps a plan-driven encode at zero
// allocations. The generic work below touches slices and values only.

// Ints writes an array of signed integers at one width, chosen from the widest
// element, and nothing when the array is empty.
func (w *Writer) Ints(key uint8, values []int64) {
	if len(values) == 0 {
		return
	}
	header, width := arrayPlan(key, values)
	w.arrayHeader(header, len(values))
	w.Buffer = appendElements(w.Buffer, values, width)
}

// Int8s, Int16s, Int32s, Uint16s, Uint32s and Uint64s are Ints for the slice a
// caller actually holds, without the []int64 it would otherwise have to build.
func (w *Writer) Int8s(key uint8, values []int8) {
	if len(values) == 0 {
		return
	}
	header, width := arrayPlan(key, values)
	w.arrayHeader(header, len(values))
	w.Buffer = appendElements(w.Buffer, values, width)
}

func (w *Writer) Int16s(key uint8, values []int16) {
	if len(values) == 0 {
		return
	}
	header, width := arrayPlan(key, values)
	w.arrayHeader(header, len(values))
	w.Buffer = appendElements(w.Buffer, values, width)
}

func (w *Writer) Int32s(key uint8, values []int32) {
	if len(values) == 0 {
		return
	}
	header, width := arrayPlan(key, values)
	w.arrayHeader(header, len(values))
	w.Buffer = appendElements(w.Buffer, values, width)
}

func (w *Writer) Uint16s(key uint8, values []uint16) {
	if len(values) == 0 {
		return
	}
	header, width := arrayPlan(key, values)
	w.arrayHeader(header, len(values))
	w.Buffer = appendElements(w.Buffer, values, width)
}

func (w *Writer) Uint32s(key uint8, values []uint32) {
	if len(values) == 0 {
		return
	}
	header, width := arrayPlan(key, values)
	w.arrayHeader(header, len(values))
	w.Buffer = appendElements(w.Buffer, values, width)
}

func (w *Writer) Uint64s(key uint8, values []uint64) {
	if len(values) == 0 {
		return
	}
	header, width := arrayPlan(key, values)
	w.arrayHeader(header, len(values))
	w.Buffer = appendElements(w.Buffer, values, width)
}

// arrayPlan is the one pass that decides both the sign flag and the width: a
// negative anywhere turns the whole array into two's complement, which needs the
// width that holds the most negative element as well as the largest positive
// one. It is generic over the element type and takes no pointer.
//
// It returns the pieces rather than a header byte, because the two key widths
// spend them in different places: K4 ORs them into the byte it shares with the
// key, K8 into a descriptor of its own.
func arrayPlanOf[T Integer](values []T) (allPositive bool, width int, widthCode uint8) {
	signed := isSigned[T]()
	allPositive = true
	var widest uint64
	for _, value := range values {
		// An unsigned type never has a negative element, however its top bit
		// reads as an int64: the test below is what keeps a uint64 past 2^63
		// from being mistaken for a negative number.
		if asSigned := int64(value); signed && asSigned < 0 {
			allPositive = false
			if magnitude := -uint64(asSigned); magnitude > widest {
				widest = magnitude
			}
			continue
		}
		if magnitude := uint64(value); magnitude > widest {
			widest = magnitude
		}
	}
	width, widthCode = arrayWidth(widest, allPositive)
	return allPositive, width, widthCode
}

// arrayPlan is arrayPlanOf with the pieces folded into a narrow header byte.
func arrayPlan[T Integer](key uint8, values []T) (header uint8, width int) {
	allPositive, width, widthCode := arrayPlanOf(values)
	header = key<<4 | widthCode<<arrayWidthShift
	if allPositive {
		header |= arrayPositiveFlag
	}
	return header, width
}

// arrayWidth picks the narrowest element width that holds every element. A
// two's complement array needs one more bit than its magnitude, which is what
// the shift below tests.
func arrayWidth(widest uint64, allPositive bool) (width int, code uint8) {
	if !allPositive {
		// A magnitude that already fills the top bit cannot gain one: shifting
		// it would wrap to zero and pick a one-byte width for math.MinInt64.
		if widest > 1<<62 {
			return 8, widthCode8Bytes
		}
		widest <<= 1
	}
	switch {
	case widest <= 0xFF:
		return 1, widthCode1Byte
	case widest <= 0xFFFF:
		return 2, widthCode2Bytes
	case widest <= 0xFFFF_FFFF:
		return 4, widthCode4Bytes
	default:
		return 8, widthCode8Bytes
	}
}

// arrayHeader writes two bytes holding an eight-bit count, or one byte and a
// four-byte count when that will not hold it.
func (w *Writer) arrayHeader(header uint8, count int) {
	if count <= inlineArrayCount {
		w.Buffer = append(w.Buffer, header, uint8(count))
		return
	}
	w.Buffer = binary.LittleEndian.AppendUint32(
		append(w.Buffer, header|moreArrayLenFlag), uint32(count))
}

// Strings writes a count and then each element behind its own length.
//
// The element length is one byte with an escape rather than a fixed two for the
// same reason the header sizes escalate: it removes the ceiling, and it makes
// the common element — anything under 255 bytes, which is every code line and
// most error texts — cost one byte instead of two.
func (w *Writer) Strings(key uint8, values []string) {
	if len(values) == 0 {
		return
	}
	w.blobHeader(key, len(values))
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

// Reader walks a message field by field. The caller switches on Key and calls
// the read for the type that key holds, which it knows from the record
// definition.
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

// Uint reads an unsigned integer field, ignoring the sign bit.
//
// Split for the same reason the writer is: the one-byte field is the common one,
// and its path is small enough to inline into the caller's switch.
func (r *Reader) Uint() uint64 {
	if field := r.buffer[r.at:]; len(field) >= 1 {
		if code := field[0] & 0b1111; code <= uintInlineMax {
			r.at++
			return uint64(code)
		} else if code == uintWidthBase && len(field) >= 2 {
			r.at += 2
			return uint64(field[1])
		}
	}
	return r.uintWide()
}

func (r *Reader) uintWide() uint64 {
	header, ok := r.header()
	if !ok {
		return 0
	}
	code := header & 0b1111
	if code <= uintInlineMax {
		r.at++
		return uint64(code)
	}
	width := int(code) - uintWidthBase + 1
	rest := r.buffer[r.at+1:]
	if len(rest) < width {
		r.fail(ErrTruncated)
		return 0
	}
	r.at += 1 + width
	return leUint(rest, width)
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
// The magnitude goes through signedMagnitude and not through Uint: a signed
// nibble is [positive:1][size:3] and an unsigned one is a sixteen-code table, so
// the same four bits mean different things and only the schema says which.
func (r *Reader) Int() int64 {
	header, ok := r.header()
	if !ok {
		return 0
	}
	positive := header&intPositiveFlag != 0
	magnitude := r.signedMagnitude()
	if positive {
		return int64(magnitude)
	}
	return -int64(magnitude)
}

// signedMagnitude reads the [size:3] form: code 0 means the magnitude is one,
// codes 1..6 are that many bytes, and code 7 is eight.
func (r *Reader) signedMagnitude() uint64 {
	header, ok := r.header()
	if !ok {
		return 0
	}
	width := magnitudeWidth[header&0b111]
	if width == 0 {
		r.at++
		return 1
	}
	rest := r.buffer[r.at+1:]
	if len(rest) < width {
		r.fail(ErrTruncated)
		return 0
	}
	r.at += 1 + width
	return leUint(rest, width)
}

// Bool reads a field written by Bool. Absent fields never reach here: a false
// bool is not written, so the key is simply missing.
func (r *Reader) Bool() bool { return r.Uint() == 1 }

// Bytes returns the field's bytes as a sub-slice of the message, without
// copying. It stays valid only as long as the message buffer does.
func (r *Reader) Bytes() []byte {
	size, start, ok := r.blobSize()
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
func (r *Reader) String() string { return string(r.Bytes()) }

// blobSize reads a blob or string-array header and returns the size it declares
// with the offset just past it.
func (r *Reader) blobSize() (size, start int, ok bool) {
	header, ok := r.header()
	if !ok {
		return 0, 0, false
	}
	if header&moreSizeFlag == 0 {
		if r.at+2 > len(r.buffer) {
			r.fail(ErrTruncated)
			return 0, 0, false
		}
		return int(header&0b111)<<8 | int(r.buffer[r.at+1]), r.at + 2, true
	}
	return r.escapedSize(header & 0b111)
}

// escapedSize reads the wide size that follows a header whose more flag is set.
// Its width is named by the header rather than discovered byte by byte, so there
// is no continuation run for a peer to make unbounded — the only thing refused
// here is a size this platform cannot address.
func (r *Reader) escapedSize(escape uint8) (size, start int, ok bool) {
	var width int
	switch escape {
	case escape2Bytes:
		width = 2
	case escape4Bytes:
		width = 4
	case escape8Bytes:
		width = 8
	default:
		r.fail(ErrBadEscape)
		return 0, 0, false
	}
	rest := r.buffer[r.at+1:]
	if len(rest) < width {
		r.fail(ErrTruncated)
		return 0, 0, false
	}
	value := leUint(rest, width)
	if value > uint64(maxInt) {
		r.fail(ErrSizeTooLarge)
		return 0, 0, false
	}
	return int(value), r.at + 1 + width, true
}

// Array readers, the mirror of the writers: one concrete method per element
// type, for the reason given there — a *Reader in a generic signature costs the
// caller an allocation.

// Ints appends the array's elements to dst, which may be nil.
func (r *Reader) Ints(dst []int64) []int64 {
	elements, positive, width, ok := r.arrayElements()
	if !ok {
		return dst
	}
	return appendArray(dst, elements, width, positive)
}

func (r *Reader) Int8s(dst []int8) []int8 {
	elements, positive, width, ok := r.arrayElements()
	if !ok {
		return dst
	}
	return appendArray(dst, elements, width, positive)
}

func (r *Reader) Int16s(dst []int16) []int16 {
	elements, positive, width, ok := r.arrayElements()
	if !ok {
		return dst
	}
	return appendArray(dst, elements, width, positive)
}

func (r *Reader) Int32s(dst []int32) []int32 {
	elements, positive, width, ok := r.arrayElements()
	if !ok {
		return dst
	}
	return appendArray(dst, elements, width, positive)
}

func (r *Reader) Uint16s(dst []uint16) []uint16 {
	elements, positive, width, ok := r.arrayElements()
	if !ok {
		return dst
	}
	return appendArray(dst, elements, width, positive)
}

func (r *Reader) Uint32s(dst []uint32) []uint32 {
	elements, positive, width, ok := r.arrayElements()
	if !ok {
		return dst
	}
	return appendArray(dst, elements, width, positive)
}

func (r *Reader) Uint64s(dst []uint64) []uint64 {
	elements, positive, width, ok := r.arrayElements()
	if !ok {
		return dst
	}
	return appendArray(dst, elements, width, positive)
}

// arrayElements reads an array field's header and returns its payload as a
// sub-slice of the message, with the element width and sign the header declares.
// It advances the cursor: the caller only has to turn bytes into elements.
func (r *Reader) arrayElements() (elements []byte, positive bool, width int, ok bool) {
	header, ok := r.header()
	if !ok {
		return nil, false, 0, false
	}
	count, start, ok := r.arrayCount(header)
	if !ok {
		return nil, false, 0, false
	}
	width = 1 << ((header >> arrayWidthShift) & 0b11)
	if count < 0 || count > (len(r.buffer)-start)/width {
		r.fail(ErrTruncated)
		return nil, false, 0, false
	}
	r.at = start + count*width
	return r.buffer[start : start+count*width], header&arrayPositiveFlag != 0, width, true
}

// arrayCount reads an integer array's header and returns its element count with
// the offset just past it.
func (r *Reader) arrayCount(header uint8) (count, start int, ok bool) {
	if header&moreArrayLenFlag == 0 {
		if r.at+2 > len(r.buffer) {
			r.fail(ErrTruncated)
			return 0, 0, false
		}
		return int(r.buffer[r.at+1]), r.at + 2, true
	}
	rest := r.buffer[r.at+1:]
	if len(rest) < 4 {
		r.fail(ErrTruncated)
		return 0, 0, false
	}
	return int(binary.LittleEndian.Uint32(rest)), r.at + 5, true
}

// StringsBytes appends each element to dst as a sub-slice of the message,
// without copying. The slices stay valid only as long as the message buffer
// does.
func (r *Reader) StringsBytes(dst [][]byte) [][]byte {
	count, at, ok := r.blobSize()
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

// elementSize reads one string-array element length: one byte, or four more
// behind the escape.
func (r *Reader) elementSize(at int) (size, start int, ok bool) {
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

// Strings copies each element into a Go string.
func (r *Reader) Strings(dst []string) []string {
	count, at, ok := r.blobSize()
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

// Skip is refused rather than guessed: the header says how wide a field is only
// once the reader knows which of the four layouts it is reading, and that comes
// from the key. An unknown key is a record definition the two sides no longer
// share.
func (r *Reader) Skip() {
	r.fail(errors.New("narrow: a field cannot be skipped without knowing its type"))
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
	if value <= uintInlineMax {
		w.Buffer = append(w.Buffer, key<<4|uint8(value))
		return
	}
	if value <= 0xFF {
		w.Buffer = append(w.Buffer, key<<4|uintWidthBase, uint8(value))
		return
	}
	w.Buffer = append(w.Buffer, key<<4|uintWidthBase+1, uint8(value), uint8(value>>8))
}

// U32 writes a field whose type cannot exceed four bytes.
//
// It is shaped like Uint and for the same reason: one compare, one append and
// one call is all the inliner's budget holds.
func (w *Writer) U32(key uint8, value uint32) {
	if value-1 < uintInlineMax {
		w.Buffer = append(w.Buffer, key<<4|uint8(value))
		return
	}
	w.u32Wide(key, value)
}

//go:noinline
func (w *Writer) u32Wide(key uint8, value uint32) {
	switch {
	case value == 0:
	case value <= 0xFF:
		w.Buffer = append(w.Buffer, key<<4|uintWidthBase, uint8(value))
	case value <= 0xFFFF:
		w.Buffer = append(w.Buffer, key<<4|uintWidthBase+1,
			uint8(value), uint8(value>>8))
	case value <= 0xFF_FFFF:
		w.Buffer = append(w.Buffer, key<<4|uintWidthBase+2,
			uint8(value), uint8(value>>8), uint8(value>>16))
	default:
		w.Buffer = append(w.Buffer, key<<4|uintWidthBase+3,
			uint8(value), uint8(value>>8), uint8(value>>16), uint8(value>>24))
	}
}

// I32 writes a signed field no wider than four bytes. Negatives go the long way
// round: they are rare on this wire and not worth the inline budget.
func (w *Writer) I32(key uint8, value int32) {
	if value > 0 && value <= 0xFF {
		w.Buffer = append(w.Buffer, key<<4|intPositiveFlag|1, uint8(value))
		return
	}
	if value == 0 {
		return
	}
	w.intWide(key, int64(value))
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
		switch code := field[0] & 0b1111; {
		case code <= uintInlineMax:
			r.at++
			return uint16(code)
		case code == uintWidthBase:
			r.at += 2
			return uint16(field[1])
		case code == uintWidthBase+1:
			r.at += 3
			return uint16(field[1]) | uint16(field[2])<<8
		}
	} else if len(field) >= 1 {
		if code := field[0] & 0b1111; code <= uintInlineMax {
			r.at++
			return uint16(code)
		} else if code == uintWidthBase && len(field) == 2 {
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
		if code := field[0] & 0b1111; code <= uintInlineMax {
			r.at++
			return uint32(code)
		} else if code == uintWidthBase && len(field) >= 2 {
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

// I32 reads a field written by I32.
func (r *Reader) I32() int32 { return int32(r.Int()) }

// Floats ride in the integer field shape, carrying the IEEE-754 bit pattern with
// its bytes reversed.
//
// The reversal is what makes the trim work at all. An integer's zero bytes are
// its most significant ones, so writing it little-endian and dropping the top
// puts the zeros where the size code can elide them. A float's zero bytes are
// its *least* significant — the low mantissa bits — while its exponent and sign
// are never zero, so trimming a float the same way as an integer saves nothing.
// Reversing the bytes swaps the two ends and the integer path then works
// unchanged: 1.0 costs two bytes rather than eight, and a float64 holding a
// value that is exactly a float32 has twenty-nine zero low bits — three whole
// bytes and five over, and only whole bytes trim — so it costs five.
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

// Integer is every integer type an array field can hold. The array writers and
// readers are package functions rather than methods because a method cannot take
// a type parameter, and a []int16 should not have to become a []int64 first.
type Integer interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64
}

// WriteInts writes an array of any integer type, for an element type the
// concrete methods above do not cover — a named type, or a uint.
//
// Prefer the concrete method when one exists. This one takes a *Writer into a
// generic signature, and a caller in another package therefore gets escape
// information conservative enough to put its writer on the heap: one allocation
// per message, which is exactly what the concrete methods exist to avoid.
func WriteInts[T Integer](w *Writer, key uint8, values []T) {
	if len(values) == 0 {
		return
	}
	header, width := arrayPlan(key, values)
	w.arrayHeader(header, len(values))
	w.Buffer = appendElements(w.Buffer, values, width)
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
// answers without reflection: -1 converted to T stays negative only if it can.
func isSigned[T Integer]() bool {
	var minusOne T
	minusOne--
	return int64(minusOne) < 0
}

// ReadInts appends an array's elements to dst, for an element type the concrete
// methods above do not cover. Prefer a concrete method when one exists, for the
// reason WriteInts gives.
func ReadInts[T Integer](r *Reader, dst []T) []T {
	elements, positive, width, ok := r.arrayElements()
	if !ok {
		return dst
	}
	return appendArray(dst, elements, width, positive)
}

// appendArray turns an array field's bytes into elements. Generic over the
// element type and touching no pointer, so it is safe to reach from anywhere.
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

// Zero and EmptyString write a field that the omit-zero rule would otherwise
// drop.
//
// They exist for pointers. An absent key means a nil pointer, so a non-nil
// pointer *to* a zero value has to put something on the wire or the two would be
// indistinguishable — which is the one place this format needs to say "zero" out
// loud rather than by omission.

// Zero writes an explicit zero in the unsigned form, which is what Uint, Bool,
// F32 and F64 all read. It is a single byte: the unsigned nibble carries 0..7
// outright.
func (w *Writer) Zero(key uint8) {
	w.Buffer = append(w.Buffer, key<<4)
}

// ZeroSigned writes an explicit zero in the signed form, for a field Int will
// read. The two nibbles are different tables, so a zero has to be written in the
// one its reader will use.
func (w *Writer) ZeroSigned(key uint8) {
	w.Buffer = append(w.Buffer, key<<4|intPositiveFlag|1, 0)
}

// EmptyString writes a blob of no bytes.
func (w *Writer) EmptyString(key uint8) {
	w.Buffer = append(w.Buffer, key<<4, 0)
}
