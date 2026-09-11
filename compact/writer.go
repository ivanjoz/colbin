package compact

import (
	"math"
	"strconv"

	"github.com/ivanjoz/colbin/packed5"
)

// Writer builds a compact message. Construction writes the four header bits;
// after that the caller emits one [key][value] pair per present field and closes
// each record with End.
//
// A field holding its zero value should simply be skipped. There is no presence
// bitmap: the key run is the presence information, and a record that omits a
// field pays nothing for it beyond the terminator it already owes.
type Writer struct {
	bw          bitWriter
	shape       Shape
	allPositive bool
	keys        KeyWidth
	keyBits     uint8  // keys.bits(), hoisted out of the per-key path
	terminator  uint64 // keys.terminator(), likewise
	scratch     []byte // reused packed5 frame buffer
}

// NewWriter appends a compact message to out and returns a writer positioned
// after the header.
//
// allPositive must be true only if every signed integer the caller will write is
// non negative; it is worth one payload bit on each of them. Determining it needs
// a pass over the whole message before the first bit is written, which is why it
// is a parameter rather than something the writer infers: at three records that
// pre-scan is free, and in standard mode it would undo the single pass encoder.
//
// keys is Keys8 unless every field id the caller will write is at most
// MaxNarrowKey, in which case Keys4 halves what the key run costs. It is a
// parameter for the same reason: the ids are a property of the caller's type,
// known before the first bit, and picking the width per message means the writer
// cannot discover mid-record that it guessed wrong.
func NewWriter(out []byte, shape Shape, allPositive bool, keys KeyWidth) *Writer {
	w := &Writer{}
	w.Reset(out, shape, allPositive, keys)
	return w
}

// Reset re-aims a writer at a fresh message, so one writer can encode many. It
// is what makes a Writer poolable: the packed5 and varint scratch buffers it has
// already grown are kept, which is the allocation a per-message writer pays
// again on every message.
//
// out is appended to, exactly as in NewWriter; passing buf[:0] reuses a buffer
// the caller owns and drops the encoder's last allocation with it.
func (w *Writer) Reset(out []byte, shape Shape, allPositive bool, keys KeyWidth) {
	if !keys.narrow() {
		keys = Keys8 // anything that is not the narrow width is the wide one
	}
	w.bw.buf, w.bw.current, w.bw.nbits = out, 0, 0
	w.shape, w.allPositive = shape, allPositive
	w.keys, w.keyBits, w.terminator = keys, keys.bits(), keys.terminator()

	w.bw.put(1, modeBits)
	w.bw.putBool(allPositive)
	w.bw.put(uint64(shape), shapeBits)
	w.bw.putBool(keys.narrow())
}

// Shape, AllPositive and Keys report what the header was built with.
func (w *Writer) Shape() Shape      { return w.shape }
func (w *Writer) AllPositive() bool { return w.allPositive }
func (w *Writer) Keys() KeyWidth    { return w.keys }

// Bits is how many bits have been written, header included. Useful for sizing
// experiments; the final message is this rounded up to a byte.
func (w *Writer) Bits() int { return w.bw.bits() }

// Key writes a field id, introducing the value that follows.
//
// Under Keys4 an id above MaxNarrowKey does not fit, and truncating it would
// silently rename the field or forge a terminator. That is a caller error the
// writer has no way to report -- it has no error to return and the message is
// half built -- so it panics rather than emitting a message that decodes wrong.
func (w *Writer) Key(k uint8) {
	if w.keys.narrow() && k > MaxNarrowKey {
		panic("colbin: compact field id " + strconv.Itoa(int(k)) + " does not fit a 4-bit key")
	}
	w.bw.put(uint64(k), w.keyBits)
}

// End closes the current record with the terminator key, at whichever width the
// header declared. A nested key run -- a struct field, or one element of an
// array of structs -- is closed by this same call: the framing does not change
// with depth.
func (w *Writer) End() { w.bw.put(w.terminator, w.keyBits) }

// Count writes how many elements or entries a composite holds, introducing the
// values that follow. It is the array codecs' count varint, exposed for the
// composites whose elements are written one at a time rather than handed to a
// codec in bulk.
func (w *Writer) Count(n int) { w.bw.putVarint(uint64(n)) }

// Int writes a signed value. With ALL_POSITIVE set the payload is the magnitude,
// which is one bit narrower than the zigzag the flag's absence forces.
func (w *Writer) Int(v int64) {
	if w.allPositive {
		w.bw.putVarint(uint64(v))
		return
	}
	w.bw.putVarint(zigzag(v))
}

// Uint writes an unsigned value. It is never zigzagged: an unsigned type cannot
// be negative, so ALL_POSITIVE has nothing to say about it.
func (w *Writer) Uint(v uint64) { w.bw.putVarint(v) }

// Bool writes one bit. A bool field costs exactly that.
func (w *Writer) Bool(b bool) { w.bw.putBool(b) }

// Float32 and Float64 write raw IEEE-754 bits. Floats have no small-value form
// worth exploiting, so they are stored at their native width.
func (w *Writer) Float32(f float32) { w.bw.put(uint64(math.Float32bits(f)), 32) }
func (w *Writer) Float64(f float64) { w.bw.put(math.Float64bits(f), 64) }

// Str writes a packed5 frame. The frame is self delimiting, so no length
// accompanies it; it is byte structured internally and simply starts wherever
// the bitstream happens to be.
func (w *Writer) Str(s string) {
	w.scratch = packed5.Append(w.scratch[:0], s)
	w.bw.putBytes(w.scratch)
}

// Bytes writes a length-prefixed blob. Unlike a string it has no codec of its
// own, so the length uses the same varint as every other integer.
func (w *Writer) Bytes(b []byte) {
	w.bw.putVarint(uint64(len(b)))
	w.bw.putBytes(b)
}

// Done pads the stream to a byte boundary and returns the finished message. The
// pad is never read back: a record ends at its terminator and the message ends
// at its last record, so the decoder stops before reaching it.
func (w *Writer) Done() []byte { return w.bw.flush() }
