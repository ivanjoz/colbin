package wire

// The third framing of a key run: a presence bitmap instead of a key per field.
//
// A struct writes its fields in ascending key order and omits the zero ones, so
// the keys it writes are an ordered subset of a known set — which is a bitmap,
// not a sequence of numbers. Bit i set means key i is present; the fields follow
// in that order, each stripped of its key and reduced to a descriptor and a
// payload.
//
//	[bitmap bytes:1] [bitmap] ( [descriptor] [payload] )*
//
// # It is smaller and faster at the same time
//
// Faster because there is no key to read and none to look up: the decoder walks
// its plan and the set bits in lockstep. Smaller whenever `1 + b < p` for p
// present fields and a b-byte bitmap — so with keys under sixteen, from four
// present fields up.
//
// On the ten-field benchmark record with five fields set it is eleven bytes,
// against twelve for the narrow key and thirteen for the wide keyed form. On the
// same ten fields all present and small it is fourteen against twenty-one. It
// loses only on the very sparse: two present fields of ten is six bytes against
// the narrow key's five.
//
// # Why it is a wide-key idea only
//
// At four key bits the key rides in the descriptor byte the field needs anyway,
// so removing the key removes nothing and the bitmap is pure addition. The gain
// here is exactly the byte K8 spends on a key and K4 does not.
//
// # Skipping still works
//
// The descriptors are unchanged, so every field still carries or implies its own
// length, and an unknown field is still nameable: its key is its bit position.

import (
	"errors"
	"math/bits"
)

// ErrBadBitmap is a bitmap wider than the keys a message can hold, or one the
// buffer does not contain.
var ErrBadBitmap = errors.New("narrow: bad presence bitmap")

// MaxBitmapKey is the largest key a bitmap run carries. It is 63 rather than 255
// because the writer holds the bitmap in one word: a wider one would have to
// live in memory, and the read-modify-write that costs is the whole reason this
// framing is fast enough to be worth having. A struct with more than sixty-four
// fields uses the keyed wide form, which has no such bound.
const MaxBitmapKey = 63

// maxBitmapBytes is what MaxBitmapKey implies.
const maxBitmapBytes = (MaxBitmapKey + 1) / 8

// BitmapWriter writes a key run whose keys are a bitmap. Keys must be written in
// ascending order; the writer does not check, for the reason Writer does not
// check a key — it is a constant of the record definition, not data.
//
// The embedded Writer8 is what writes the fields. Calling its methods directly
// would write keys as well, so use the methods below.
type BitmapWriter struct {
	inner  Writer8
	bits   uint64 // the presence bitmap, held in a register until Buffer patches it
	offset int    // where it will be written in the buffer
	width  int    // its byte count
}

// NewBitmapWriter starts a run over a buffer, reserving a bitmap wide enough for
// keys 0..maxKey. The bitmap is written zeroed and filled in as fields arrive,
// which is why this costs no second pass: setting a bit is one OR into a byte
// already in cache.
// A key above MaxBitmapKey is a compile-time mistake, like a key above fifteen
// under K4, and is not checked for the same reason.
func NewBitmapWriter(buffer []byte, maxKey uint8) BitmapWriter {
	width := int(maxKey)/8 + 1
	buffer = append(buffer, uint8(width))
	offset := len(buffer)
	for range width {
		buffer = append(buffer, 0)
	}
	return BitmapWriter{inner: Writer8{Buffer: buffer}, offset: offset, width: width}
}

// Buffer patches the bitmap into the bytes reserved for it and returns the
// message. Call it once, after the last field.
func (w *BitmapWriter) Buffer() []byte {
	bits := w.bits
	for index := range w.width {
		w.inner.Buffer[w.offset+index] = uint8(bits)
		bits >>= 8
	}
	return w.inner.Buffer
}

// present marks a key. It ORs into a struct field rather than into the buffer,
// which is the difference between a register operation and a bounds-checked
// read-modify-write: the buffer form measured 22% of a ten-field encode, for a
// bitmap that is not read until the message is finished anyway.
func (w *BitmapWriter) present(key uint8) {
	w.bits |= 1 << key
}

// The write methods mirror Writer8's, minus the key byte. Each is the same
// omit-zero test, the same descriptor, and one OR.

func (w *BitmapWriter) Uint(key uint8, value uint64) {
	if value == 0 {
		return
	}
	w.present(key)
	if value <= maxInlineValue {
		w.inner.Buffer = append(w.inner.Buffer, uint8(value))
		return
	}
	code, width := sizeCodeFor(value)
	w.inner.Buffer = appendMagnitude(
		append(w.inner.Buffer, descriptor(classInt, intPositiveFlag|code)), value, width)
}

func (w *BitmapWriter) Int(key uint8, value int64) {
	if value >= 0 {
		w.Uint(key, uint64(value))
		return
	}
	w.present(key)
	magnitude := -uint64(value)
	code, width := sizeCodeFor(magnitude)
	w.inner.Buffer = appendMagnitude(
		append(w.inner.Buffer, descriptor(classInt, code)), magnitude, width)
}

func (w *BitmapWriter) Bool(key uint8, value bool) {
	if !value {
		return
	}
	w.present(key)
	w.inner.Buffer = append(w.inner.Buffer, 1)
}

// U16 and U32 are width-typed for the reason Writer's are: a uint64 parameter
// forces the method to carry every width the format has, which pushes it past
// the inline budget. Delegating them to Uint instead measured 18.9 ns against
// 5.3 for the narrow writer on a ten-field record — most of the gap, and none of
// it inherent to the bitmap.
func (w *BitmapWriter) U16(key uint8, value uint16) {
	if value == 0 {
		return
	}
	w.present(key)
	if value <= maxInlineValue {
		w.inner.Buffer = append(w.inner.Buffer, uint8(value))
		return
	}
	if value <= 0xFF {
		w.inner.Buffer = append(w.inner.Buffer,
			descriptor(classInt, intPositiveFlag|1), uint8(value))
		return
	}
	w.inner.Buffer = append(w.inner.Buffer,
		descriptor(classInt, intPositiveFlag|2), uint8(value), uint8(value>>8))
}

func (w *BitmapWriter) U32(key uint8, value uint32) {
	if value == 0 {
		return
	}
	w.present(key)
	if value <= maxInlineValue {
		w.inner.Buffer = append(w.inner.Buffer, uint8(value))
		return
	}
	w.u32Wide(value)
}

func (w *BitmapWriter) u32Wide(value uint32) {
	code, width := sizeCodeFor(uint64(value))
	w.inner.Buffer = appendMagnitude(
		append(w.inner.Buffer, descriptor(classInt, intPositiveFlag|code)), uint64(value), width)
}

func (w *BitmapWriter) I32(key uint8, value int32) { w.Int(key, int64(value)) }

func (w *BitmapWriter) String(key uint8, value string) {
	if len(value) == 0 {
		return
	}
	w.present(key)
	code, width := lengthCodeFor(uint64(len(value)))
	w.inner.Buffer = appendMagnitude(
		append(w.inner.Buffer, descriptor(classBlob, code)), uint64(len(value)), width)
	w.inner.Buffer = append(w.inner.Buffer, value...)
}

func (w *BitmapWriter) Bytes(key uint8, value []byte) {
	if len(value) == 0 {
		return
	}
	w.String(key, string(value))
}

// BitmapReader walks a run written by BitmapWriter, yielding the set bits in
// order as keys.
type BitmapReader struct {
	inner  Reader8
	bitmap []byte
	word   uint64 // the set bits of the current chunk, consumed lowest first
	base   int    // the key of bit 0 of word
	cur    uint8
	valid  bool
}

// NewBitmapReader starts a reader over one run.
func NewBitmapReader(message []byte) BitmapReader {
	if len(message) < 1 {
		return BitmapReader{inner: Reader8{buffer: message, err: ErrTruncated}}
	}
	width := int(message[0])
	if width < 1 || width > maxBitmapBytes || len(message) < 1+width {
		return BitmapReader{inner: Reader8{buffer: message, err: ErrBadBitmap}}
	}
	reader := BitmapReader{
		inner:  Reader8{buffer: message, at: 1 + width},
		bitmap: message[1 : 1+width],
	}
	reader.word = bitmapWord(reader.bitmap, 0)
	reader.advance()
	return reader
}

// bitmapWord reads up to eight bitmap bytes as one little-endian word, zero
// padded. A struct with sixty-four keys or fewer — which is every struct — needs
// exactly one, so the refill below never runs.
func bitmapWord(bitmap []byte, at int) uint64 {
	var word uint64
	for index := 0; index < 8 && at+index < len(bitmap); index++ {
		word |= uint64(bitmap[at+index]) << (8 * index)
	}
	return word
}

// advance moves to the next set bit, which is the next key present.
//
// Scanning the bits one at a time was the first version and it is why the
// bitmap reader measured 48 ns against the narrow reader's 14 on a ten-field
// record. A trailing-zero count over a word finds the next key in two
// instructions and clearing it takes one more, so a sparse bitmap costs nothing
// for the keys it does not hold.
func (r *BitmapReader) advance() {
	for r.word == 0 {
		next := r.base + 64
		if next >= len(r.bitmap)*8 {
			r.valid = false
			return
		}
		r.base = next
		r.word = bitmapWord(r.bitmap, next/8)
	}
	bit := bits.TrailingZeros64(r.word)
	r.word &= r.word - 1
	r.cur, r.valid = uint8(r.base+bit), true
}

// More reports whether another field follows.
func (r *BitmapReader) More() bool { return r.valid && r.inner.err == nil }

// Key is the field the cursor is on.
func (r *BitmapReader) Key() uint8 { return r.cur }

// Err reports the first failure.
func (r *BitmapReader) Err() error { return r.inner.err }

// The read methods reach the same descriptor readers Reader8 uses, with the
// cursor stepped back one byte because there is no key. Backing it up is what
// lets the whole of Reader8's value handling be shared rather than written a
// third time — and they are written out rather than routed through a closure,
// which would put the return value on the heap.

func (r *BitmapReader) Uint() uint64 {
	r.inner.at--
	value := r.inner.Uint()
	r.advance()
	return value
}

func (r *BitmapReader) Int() int64 {
	r.inner.at--
	value := r.inner.Int()
	r.advance()
	return value
}

func (r *BitmapReader) Bool() bool { return r.Uint() == 1 }

func (r *BitmapReader) U16() uint16 {
	r.inner.at--
	value := r.inner.U16()
	r.advance()
	return value
}

func (r *BitmapReader) U32() uint32 {
	r.inner.at--
	value := r.inner.U32()
	r.advance()
	return value
}

func (r *BitmapReader) I32() int32 {
	r.inner.at--
	value := r.inner.I32()
	r.advance()
	return value
}

func (r *BitmapReader) Bytes() []byte {
	r.inner.at--
	value := r.inner.Bytes()
	r.advance()
	return value
}

func (r *BitmapReader) String() string {
	r.inner.at--
	value := r.inner.String()
	r.advance()
	return value
}

// Skip steps over a field whose key this reader does not know, which works here
// for the same reason it works under a wide key: the descriptor sizes the field.
func (r *BitmapReader) Skip() bool {
	r.inner.at--
	size, ok := r.inner.fieldSize()
	if !ok {
		return false
	}
	r.inner.at += size
	r.advance()
	return true
}
