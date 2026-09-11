package minimal

import (
	"errors"
	"math"
)

// MaxFields is what four key bits buy: keys 0..15. A record needing a
// seventeenth field needs a different format, not a wider key — the key shares a
// byte with the field's own header bits and there is nothing to take them from.
const MaxFields = 16

// Integer size codes. Code 6 carries no bytes at all and means the magnitude is
// one, which is what makes a true bool a single byte.
const (
	sizeCode1Byte  = 0
	sizeCode2Bytes = 1
	sizeCode3Bytes = 2
	sizeCode4Bytes = 3
	sizeCode6Bytes = 4
	sizeCode8Bytes = 5
	sizeCodeOne    = 6
)

// Array element width codes.
const (
	widthCode1Byte  = 0
	widthCode2Bytes = 1
	widthCode4Bytes = 2
	widthCode8Bytes = 3
)

const (
	// moreSizeFlag says the size or count in this header is only its low bits and
	// that continuation bytes follow. It is bit 3 in a blob header and bit 0 in an
	// array header, because an array spends bits 2..1 on its element width.
	// An array header byte is [key:4][positive:1][width:2][more:1]; a blob header
	// byte is [key:4][more:1][size hi:3]. The flags below name those positions.
	moreSizeFlag     = 0b1000
	moreArrayLenFlag = 0b0001
	arrayWidthShift  = 1
	// intPositiveFlag is bit 3 of an integer header, directly above the three
	// size-code bits — an integer needs no continuation flag, because its size
	// code already reaches eight bytes. arrayPositiveFlag is bit 3 of an array
	// header. They are deliberately separate constants: one value used for both
	// would land on a size-code bit.
	intPositiveFlag   = 0b1000
	arrayPositiveFlag = 0b1000

	// What each header carries before a continuation byte is needed. Past these
	// the size is not refused, only continued: see appendSizeExtension.
	inlineBlobSize       = 1<<11 - 1
	inlineArrayCount     = 1<<8 - 1
	inlineStringArrayLen = 1<<11 - 1

	// maxInt is what a size is read into, and therefore the ceiling a declared
	// size is checked against before it is used.
	maxInt = int(^uint(0) >> 1)

	// A size is read into an int, so a peer cannot be allowed to accumulate one
	// past what an int holds — it would wrap into a small positive number and the
	// bounds check below it would pass. Nine continuation bytes is already past
	// any frame this port can carry; the read fails there rather than wrapping.
	maxSizeContinuationBytes = 9
)

var (
	// ErrSizeTooLarge is a size whose continuation bytes describe more than an
	// int can hold. It is a read-side error only: a writer cannot produce one,
	// because the value it is describing is already in memory.
	ErrSizeTooLarge = errors.New("minimal: a declared size is larger than this platform can address")
	// ErrFieldTooWide is a peer writing a field wider than the type this record
	// says it holds — a schema disagreement, refused rather than truncated into a
	// different, valid-looking value.
	ErrFieldTooWide = errors.New("minimal: a field is wider than its declared type")
	ErrTruncated    = errors.New("minimal: the message ends inside a field")
	ErrBadSizeCode  = errors.New("minimal: unassigned integer size code")
)

// Writer appends fields to a buffer the caller owns. Build one per message over a
// reused buffer; it holds no state but the buffer.
//
// # A write cannot fail
//
// There is no error to check and no Err method. Sizes are continued rather than
// capped, so nothing a caller can hold in memory is too large to describe, and a
// key is not data (below). Anything the format cannot express would have to be a
// value that does not exist.
//
// # Keys are not checked, on purpose
//
// The reader defends against the network; the writer trusts its own program. A
// key is a constant of the record definition, never data, so a key above fifteen
// is a compile-time mistake and checking for it once per field measured 8 ns on a
// ten-field record — a third of the encode. Assert it where the constants live:
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

// appendSizeExtension writes the bits of a size that did not fit its header, as
// LEB128 continuation bytes: seven bits each, the high bit set on all but the
// last. The header holds the low bits, so this is what the reader shifts above
// them — and why no size has a ceiling.
func appendSizeExtension(buffer []byte, remaining uint64) []byte {
	for {
		part := uint8(remaining & 0x7F)
		remaining >>= 7
		if remaining != 0 {
			part |= 0x80
		}
		buffer = append(buffer, part)
		if remaining == 0 {
			return buffer
		}
	}
}

// Uint writes an unsigned integer, and writes nothing at all when it is zero.
//
// The one-byte case is inline and everything else is a call, deliberately: a
// value under 256 is what most fields on this wire hold, and keeping that path
// small enough for the inliner is worth more than the branch it costs the rest.
func (w *Writer) Uint(key uint8, value uint64) {
	if value == 0 {
		return
	}
	if value <= 0xFF {
		w.Buffer = append(w.Buffer, key<<4|intPositiveFlag|sizeCode1Byte, uint8(value))
		return
	}
	w.uintWide(key, value)
}

func (w *Writer) uintWide(key uint8, value uint64) {
	w.magnitude(key<<4|intPositiveFlag, value)
}

// Int writes a signed integer as a sign bit and a magnitude.
func (w *Writer) Int(key uint8, value int64) {
	if value > 0 && value <= 0xFF {
		w.Buffer = append(w.Buffer, key<<4|intPositiveFlag|sizeCode1Byte, uint8(value))
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
	w.Buffer = append(w.Buffer, key<<4|intPositiveFlag|sizeCodeOne)
}

// magnitude writes the header and as many bytes as the value actually needs.
func (w *Writer) magnitude(header uint8, magnitude uint64) {
	switch {
	case magnitude == 1:
		w.Buffer = append(w.Buffer, header|sizeCodeOne)
	case magnitude <= 0xFF:
		w.Buffer = append(w.Buffer, header|sizeCode1Byte, uint8(magnitude))
	case magnitude <= 0xFFFF:
		w.Buffer = append(w.Buffer, header|sizeCode2Bytes, uint8(magnitude>>8), uint8(magnitude))
	case magnitude <= 0xFF_FFFF:
		w.Buffer = append(w.Buffer, header|sizeCode3Bytes,
			uint8(magnitude>>16), uint8(magnitude>>8), uint8(magnitude))
	case magnitude <= 0xFFFF_FFFF:
		w.Buffer = append(w.Buffer, header|sizeCode4Bytes,
			uint8(magnitude>>24), uint8(magnitude>>16), uint8(magnitude>>8), uint8(magnitude))
	case magnitude <= 0xFFFF_FFFF_FFFF:
		w.Buffer = append(w.Buffer, header|sizeCode6Bytes,
			uint8(magnitude>>40), uint8(magnitude>>32), uint8(magnitude>>24),
			uint8(magnitude>>16), uint8(magnitude>>8), uint8(magnitude))
	default:
		w.Buffer = append(w.Buffer, header|sizeCode8Bytes,
			uint8(magnitude>>56), uint8(magnitude>>48), uint8(magnitude>>40), uint8(magnitude>>32),
			uint8(magnitude>>24), uint8(magnitude>>16), uint8(magnitude>>8), uint8(magnitude))
	}
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

// blobHeader writes the header a string or blob carries: two bytes holding the
// size's low eleven bits, plus continuation bytes for whatever is above them.
func (w *Writer) blobHeader(key uint8, size int) {
	if size <= inlineBlobSize {
		w.Buffer = append(w.Buffer, key<<4|uint8(size>>8)&0b111, uint8(size))
		return
	}
	w.Buffer = append(w.Buffer,
		key<<4|moreSizeFlag|uint8(size>>8)&0b111, uint8(size))
	w.Buffer = appendSizeExtension(w.Buffer, uint64(size)>>11)
}

// Ints writes an array of signed integers at one width, chosen from the widest
// element, and nothing when the array is empty.
func (w *Writer) Ints(key uint8, values []int64) {
	if len(values) == 0 {
		return
	}

	// One pass decides both the sign flag and the width: a negative anywhere
	// turns the whole array into two's complement, which needs the width that
	// holds the most negative element as well as the largest positive one.
	allPositive := true
	var widest uint64
	for _, value := range values {
		if value < 0 {
			allPositive = false
			if magnitude := -uint64(value); magnitude > widest {
				widest = magnitude
			}
			continue
		}
		if uint64(value) > widest {
			widest = uint64(value)
		}
	}

	width, widthCode := arrayWidth(widest, allPositive)
	header := key<<4 | widthCode<<arrayWidthShift
	if allPositive {
		header |= arrayPositiveFlag
	}
	w.arrayHeader(header, len(values))
	for _, value := range values {
		w.appendWidth(uint64(value), width)
	}
}

// Int32s is Ints without the []int64 a caller would otherwise have to build.
func (w *Writer) Int32s(key uint8, values []int32) {
	if len(values) == 0 {
		return
	}
	allPositive := true
	var widest uint64
	for _, value := range values {
		if value < 0 {
			allPositive = false
			if magnitude := -uint64(int64(value)); magnitude > widest {
				widest = magnitude
			}
			continue
		}
		if uint64(value) > widest {
			widest = uint64(value)
		}
	}
	width, widthCode := arrayWidth(widest, allPositive)
	header := key<<4 | widthCode<<arrayWidthShift
	if allPositive {
		header |= arrayPositiveFlag
	}
	w.arrayHeader(header, len(values))
	for _, value := range values {
		w.appendWidth(uint64(int64(value)), width)
	}
}

// Uint16s is the packed-grant case: unsigned, never wider than two bytes.
func (w *Writer) Uint16s(key uint8, values []uint16) {
	if len(values) == 0 {
		return
	}
	var widest uint64
	for _, value := range values {
		if uint64(value) > widest {
			widest = uint64(value)
		}
	}
	width, widthCode := arrayWidth(widest, true)
	w.arrayHeader(key<<4|arrayPositiveFlag|widthCode<<arrayWidthShift, len(values))
	for _, value := range values {
		w.appendWidth(uint64(value), width)
	}
}

// arrayWidth picks the narrowest element width that holds every element. A
// two's complement array needs one more bit than its magnitude, which is what
// the shift below tests.
func arrayWidth(widest uint64, allPositive bool) (width int, code uint8) {
	if !allPositive {
		// A magnitude that already fills the top bit cannot gain one: shifting it
		// would wrap to zero and pick a one-byte width for math.MinInt64.
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

// arrayHeader writes two bytes holding the count's low eight bits, plus
// continuation bytes for whatever is above them.
func (w *Writer) arrayHeader(header uint8, count int) {
	if count <= inlineArrayCount {
		w.Buffer = append(w.Buffer, header, uint8(count))
		return
	}
	w.Buffer = append(w.Buffer, header|moreArrayLenFlag, uint8(count))
	w.Buffer = appendSizeExtension(w.Buffer, uint64(count)>>8)
}

func (w *Writer) appendWidth(value uint64, width int) {
	switch width {
	case 1:
		w.Buffer = append(w.Buffer, uint8(value))
	case 2:
		w.Buffer = append(w.Buffer, uint8(value>>8), uint8(value))
	case 4:
		w.Buffer = append(w.Buffer,
			uint8(value>>24), uint8(value>>16), uint8(value>>8), uint8(value))
	default:
		w.Buffer = append(w.Buffer,
			uint8(value>>56), uint8(value>>48), uint8(value>>40), uint8(value>>32),
			uint8(value>>24), uint8(value>>16), uint8(value>>8), uint8(value))
	}
}

// Strings writes a count and then each element behind its own LEB128 length.
//
// The element length is a varint rather than a fixed two bytes for the same
// reason the header sizes are continued: it removes the ceiling, and it makes the
// common element — anything under 128 bytes, which is every code line and most
// error texts — cost one byte instead of two.
func (w *Writer) Strings(key uint8, values []string) {
	if len(values) == 0 {
		return
	}
	count := len(values)
	if count <= inlineStringArrayLen {
		w.Buffer = append(w.Buffer, key<<4|uint8(count>>8)&0b111, uint8(count))
	} else {
		w.Buffer = append(w.Buffer, key<<4|moreSizeFlag|uint8(count>>8)&0b111, uint8(count))
		w.Buffer = appendSizeExtension(w.Buffer, uint64(count)>>11)
	}
	for _, value := range values {
		w.Buffer = appendSizeExtension(w.Buffer, uint64(len(value)))
		w.Buffer = append(w.Buffer, value...)
	}
}

// Reader walks a message field by field. The caller switches on Key and calls the
// read for the type that key holds, which it knows from the record definition.
type Reader struct {
	buffer []byte
	at     int
	err    error
}

// NewReader starts a reader over one message.
func NewReader(message []byte) Reader { return Reader{buffer: message} }

// More reports whether another field follows. It goes false on the first error,
// so a decode loop ends rather than spinning on a broken message.
func (r *Reader) More() bool { return r.err == nil && r.at < len(r.buffer) }

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
	if field := r.buffer[r.at:]; len(field) >= 2 && field[0]&0b111 == sizeCode1Byte {
		r.at += 2
		return uint64(field[1])
	}
	return r.uintWide()
}

func (r *Reader) uintWide() uint64 {
	header, ok := r.header()
	if !ok {
		return 0
	}
	width, ok := integerWidth(header & 0b111)
	if !ok {
		r.fail(ErrBadSizeCode)
		return 0
	}
	if header&0b111 == sizeCodeOne {
		r.at++
		return 1
	}
	if r.at+1+width > len(r.buffer) {
		r.fail(ErrTruncated)
		return 0
	}
	value := beUint(r.buffer[r.at+1 : r.at+1+width])
	r.at += 1 + width
	return value
}

// Int reads a signed integer field.
func (r *Reader) Int() int64 {
	header, ok := r.header()
	if !ok {
		return 0
	}
	positive := header&intPositiveFlag != 0
	magnitude := r.Uint()
	if positive {
		return int64(magnitude)
	}
	return -int64(magnitude)
}

// Bool reads a field written by Bool. Absent fields never reach here: a false
// bool is not written, so the key is simply missing.
func (r *Reader) Bool() bool { return r.Uint() == 1 }

// Bytes returns the field's bytes as a sub-slice of the message, without copying.
// It stays valid only as long as the message buffer does.
func (r *Reader) Bytes() []byte {
	header, ok := r.header()
	if !ok {
		return nil
	}
	size, start, ok := r.blobSize(header)
	if !ok {
		return nil
	}
	if start+size > len(r.buffer) {
		r.fail(ErrTruncated)
		return nil
	}
	r.at = start + size
	return r.buffer[start : start+size]
}

// String copies the field into a Go string.
func (r *Reader) String() string { return string(r.Bytes()) }

func (r *Reader) blobSize(header uint8) (size, start int, ok bool) {
	if r.at+2 > len(r.buffer) {
		r.fail(ErrTruncated)
		return 0, 0, false
	}
	size = int(header&0b111)<<8 | int(r.buffer[r.at+1])
	if header&moreSizeFlag == 0 {
		return size, r.at + 2, true
	}
	return r.continuedSize(size, 11, r.at+2)
}

// continuedSize reads the LEB128 run that follows a header whose more flag is
// set, and returns the whole size with the header's low bits under it.
//
// The run is what an unauthenticated peer controls, so two things are refused
// rather than believed: a run that never ends inside the message, and one that
// describes more than an int can hold — which would wrap into a small positive
// size whose bounds check then passes.
func (r *Reader) continuedSize(low, lowBits, at int) (size, start int, ok bool) {
	value := uint64(low)
	for shift, read := lowBits, 0; ; shift, read = shift+7, read+1 {
		if at >= len(r.buffer) {
			r.fail(ErrTruncated)
			return 0, 0, false
		}
		if read >= maxSizeContinuationBytes || shift >= 64 {
			r.fail(ErrSizeTooLarge)
			return 0, 0, false
		}
		part := r.buffer[at]
		at++
		value |= uint64(part&0x7F) << shift
		if part&0x80 == 0 {
			break
		}
	}
	if value > uint64(maxInt) {
		r.fail(ErrSizeTooLarge)
		return 0, 0, false
	}
	return int(value), at, true
}

// Ints appends the array's elements to dst, which may be nil.
func (r *Reader) Ints(dst []int64) []int64 {
	header, ok := r.header()
	if !ok {
		return dst
	}
	count, start, ok := r.arrayCount(header)
	if !ok {
		return dst
	}
	width := 1 << ((header >> arrayWidthShift) & 0b11)
	if start+count*width > len(r.buffer) {
		r.fail(ErrTruncated)
		return dst
	}
	positive := header&arrayPositiveFlag != 0
	for index := range count {
		at := start + index*width
		raw := beUint(r.buffer[at : at+width])
		if positive {
			dst = append(dst, int64(raw))
			continue
		}
		dst = append(dst, signExtend(raw, width))
	}
	r.at = start + count*width
	return dst
}

// Int32s is Ints for the []int32 a caller usually holds.
func (r *Reader) Int32s(dst []int32) []int32 {
	header, ok := r.header()
	if !ok {
		return dst
	}
	count, start, ok := r.arrayCount(header)
	if !ok {
		return dst
	}
	width := 1 << ((header >> arrayWidthShift) & 0b11)
	if start+count*width > len(r.buffer) {
		r.fail(ErrTruncated)
		return dst
	}
	positive := header&arrayPositiveFlag != 0
	for index := range count {
		at := start + index*width
		raw := beUint(r.buffer[at : at+width])
		if positive {
			dst = append(dst, int32(raw))
			continue
		}
		dst = append(dst, int32(signExtend(raw, width)))
	}
	r.at = start + count*width
	return dst
}

// Uint16s is Ints for packed grants.
func (r *Reader) Uint16s(dst []uint16) []uint16 {
	header, ok := r.header()
	if !ok {
		return dst
	}
	count, start, ok := r.arrayCount(header)
	if !ok {
		return dst
	}
	width := 1 << ((header >> arrayWidthShift) & 0b11)
	if start+count*width > len(r.buffer) {
		r.fail(ErrTruncated)
		return dst
	}
	for index := range count {
		at := start + index*width
		dst = append(dst, uint16(beUint(r.buffer[at:at+width])))
	}
	r.at = start + count*width
	return dst
}

func (r *Reader) arrayCount(header uint8) (count, start int, ok bool) {
	if r.at+2 > len(r.buffer) {
		r.fail(ErrTruncated)
		return 0, 0, false
	}
	count = int(r.buffer[r.at+1])
	if header&moreArrayLenFlag == 0 {
		return count, r.at + 2, true
	}
	return r.continuedSize(count, 8, r.at+2)
}

// StringsBytes appends each element to dst as a sub-slice of the message, without
// copying. The slices stay valid only as long as the message buffer does.
func (r *Reader) StringsBytes(dst [][]byte) [][]byte {
	count, at, ok := r.stringArrayCount()
	if !ok {
		return dst
	}
	for range count {
		size, next, ok := r.elementSize(at)
		if !ok {
			return dst
		}
		if next+size > len(r.buffer) || next+size < next {
			r.fail(ErrTruncated)
			return dst
		}
		dst = append(dst, r.buffer[next:next+size])
		at = next + size
	}
	r.at = at
	return dst
}

// stringArrayCount reads the count out of a string array's header.
func (r *Reader) stringArrayCount() (count, at int, ok bool) {
	header, ok := r.header()
	if !ok {
		return 0, 0, false
	}
	if r.at+2 > len(r.buffer) {
		r.fail(ErrTruncated)
		return 0, 0, false
	}
	count = int(header&0b111)<<8 | int(r.buffer[r.at+1])
	if header&moreSizeFlag == 0 {
		return count, r.at + 2, true
	}
	count, at, ok = r.continuedSize(count, 11, r.at+2)
	return count, at, ok
}

// elementSize reads one LEB128 element length, and is the only place a string
// array's own bytes describe a length.
func (r *Reader) elementSize(at int) (size, start int, ok bool) {
	return r.continuedSize(0, 0, at)
}

// Strings copies each element into a Go string.
func (r *Reader) Strings(dst []string) []string {
	count, at, ok := r.stringArrayCount()
	if !ok {
		return dst
	}
	for range count {
		size, next, ok := r.elementSize(at)
		if !ok {
			return dst
		}
		if next+size > len(r.buffer) || next+size < next {
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
	r.fail(errors.New("minimal: a field cannot be skipped without knowing its type"))
}

func integerWidth(sizeCode uint8) (width int, ok bool) {
	switch sizeCode {
	case sizeCode1Byte:
		return 1, true
	case sizeCode2Bytes:
		return 2, true
	case sizeCode3Bytes:
		return 3, true
	case sizeCode4Bytes:
		return 4, true
	case sizeCode6Bytes:
		return 6, true
	case sizeCode8Bytes:
		return 8, true
	case sizeCodeOne:
		return 0, true
	default:
		return 0, false
	}
}

// beUint reads 1..8 big-endian bytes. Unrolled rather than looped: the widths
// are a closed set, and a loop over a slice of length two costs more than the
// switch that names it.
func beUint(bytes []byte) uint64 {
	switch len(bytes) {
	case 1:
		return uint64(bytes[0])
	case 2:
		return uint64(bytes[0])<<8 | uint64(bytes[1])
	case 3:
		return uint64(bytes[0])<<16 | uint64(bytes[1])<<8 | uint64(bytes[2])
	case 4:
		return uint64(bytes[0])<<24 | uint64(bytes[1])<<16 |
			uint64(bytes[2])<<8 | uint64(bytes[3])
	case 6:
		return uint64(bytes[0])<<40 | uint64(bytes[1])<<32 | uint64(bytes[2])<<24 |
			uint64(bytes[3])<<16 | uint64(bytes[4])<<8 | uint64(bytes[5])
	case 8:
		return uint64(bytes[0])<<56 | uint64(bytes[1])<<48 | uint64(bytes[2])<<40 |
			uint64(bytes[3])<<32 | uint64(bytes[4])<<24 | uint64(bytes[5])<<16 |
			uint64(bytes[6])<<8 | uint64(bytes[7])
	default:
		var value uint64
		for _, b := range bytes {
			value = value<<8 | uint64(b)
		}
		return value
	}
}

func signExtend(raw uint64, width int) int64 {
	shift := 64 - 8*width
	return int64(raw<<shift) >> shift
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
	if value <= 0xFF {
		w.Buffer = append(w.Buffer, key<<4|intPositiveFlag|sizeCode1Byte, uint8(value))
		return
	}
	w.Buffer = append(w.Buffer, key<<4|intPositiveFlag|sizeCode2Bytes,
		uint8(value>>8), uint8(value))
}

// U32 writes a field whose type cannot exceed four bytes.
func (w *Writer) U32(key uint8, value uint32) {
	if value == 0 {
		return
	}
	if value <= 0xFF {
		w.Buffer = append(w.Buffer, key<<4|intPositiveFlag|sizeCode1Byte, uint8(value))
		return
	}
	w.u32Wide(key, value)
}

func (w *Writer) u32Wide(key uint8, value uint32) {
	switch {
	case value <= 0xFFFF:
		w.Buffer = append(w.Buffer, key<<4|intPositiveFlag|sizeCode2Bytes,
			uint8(value>>8), uint8(value))
	case value <= 0xFF_FFFF:
		w.Buffer = append(w.Buffer, key<<4|intPositiveFlag|sizeCode3Bytes,
			uint8(value>>16), uint8(value>>8), uint8(value))
	default:
		w.Buffer = append(w.Buffer, key<<4|intPositiveFlag|sizeCode4Bytes,
			uint8(value>>24), uint8(value>>16), uint8(value>>8), uint8(value))
	}
}

// I32 writes a signed field no wider than four bytes. Negatives go the long way
// round: they are rare on this wire and not worth the inline budget.
func (w *Writer) I32(key uint8, value int32) {
	if value > 0 && value <= 0xFF {
		w.Buffer = append(w.Buffer, key<<4|intPositiveFlag|sizeCode1Byte, uint8(value))
		return
	}
	if value == 0 {
		return
	}
	w.intWide(key, int64(value))
}

// Width-typed readers, the mirror of the writers above.

// U16 reads a field written by U16, or by any writer that kept it under 65 536.
func (r *Reader) U16() uint16 {
	if field := r.buffer[r.at:]; len(field) >= 2 && field[0]&0b111 == sizeCode1Byte {
		r.at += 2
		return uint16(field[1])
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
	if field := r.buffer[r.at:]; len(field) >= 2 && field[0]&0b111 == sizeCode1Byte {
		r.at += 2
		return uint32(field[1])
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

// Floats ride in the integer field shape: the header's size code counts the
// bytes of the IEEE-754 bit pattern, written big-endian like everything else.
// Nothing new is needed on the wire because the reader already knows the field
// is a float — it knows its type from the key — and a bit pattern with zero high
// bytes simply costs fewer of them. Positive zero is not written at all, like
// every other zero value.

// F32 writes a float32 as its bit pattern.
func (w *Writer) F32(key uint8, value float32) {
	w.Uint(key, uint64(math.Float32bits(value)))
}

// F64 writes a float64 as its bit pattern.
func (w *Writer) F64(key uint8, value float64) {
	w.Uint(key, math.Float64bits(value))
}

// F32 reads a field written by F32.
func (r *Reader) F32() float32 { return math.Float32frombits(uint32(r.Uint())) }

// F64 reads a field written by F64.
func (r *Reader) F64() float64 { return math.Float64frombits(r.Uint()) }

// Integer is every integer type an array field can hold. The array writers and
// readers are package functions rather than methods because a method cannot take
// a type parameter, and a []int16 should not have to become a []int64 first.
type Integer interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64
}

// WriteInts writes an array of any integer type at one width, chosen from the
// widest element, and nothing when the array is empty.
func WriteInts[T Integer](w *Writer, key uint8, values []T) {
	if len(values) == 0 {
		return
	}
	allPositive := true
	var widest uint64
	for _, value := range values {
		signed := int64(value)
		// An unsigned type never has a negative element, however its top bit
		// reads as an int64: the conversion below is what keeps a uint64 past
		// 2^63 from being mistaken for a negative number.
		if signed < 0 && isSigned[T]() {
			allPositive = false
			if magnitude := -uint64(signed); magnitude > widest {
				widest = magnitude
			}
			continue
		}
		if magnitude := uint64(value); magnitude > widest {
			widest = magnitude
		}
	}
	width, widthCode := arrayWidth(widest, allPositive)
	header := key<<4 | widthCode<<arrayWidthShift
	if allPositive {
		header |= arrayPositiveFlag
	}
	w.arrayHeader(header, len(values))
	for _, value := range values {
		w.appendWidth(uint64(value), width)
	}
}

// isSigned reports whether T is a signed integer type, which the zero value
// answers without reflection: -1 converted to T stays negative only if it can.
func isSigned[T Integer]() bool {
	var minusOne T
	minusOne--
	return int64(minusOne) < 0
}

// ReadInts appends an array's elements to dst, converting each to T.
func ReadInts[T Integer](r *Reader, dst []T) []T {
	header, ok := r.header()
	if !ok {
		return dst
	}
	count, start, ok := r.arrayCount(header)
	if !ok {
		return dst
	}
	width := 1 << ((header >> arrayWidthShift) & 0b11)
	if start+count*width > len(r.buffer) || start+count*width < start {
		r.fail(ErrTruncated)
		return dst
	}
	positive := header&arrayPositiveFlag != 0
	for index := range count {
		at := start + index*width
		raw := beUint(r.buffer[at : at+width])
		if positive {
			dst = append(dst, T(raw))
			continue
		}
		dst = append(dst, T(signExtend(raw, width)))
	}
	r.at = start + count*width
	return dst
}
