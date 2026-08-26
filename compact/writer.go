package compact

import (
	"math"

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
func NewWriter(out []byte, shape Shape, allPositive bool) *Writer {
	w := &Writer{bw: bitWriter{buf: out}, shape: shape, allPositive: allPositive}
	w.bw.put(1, modeBits)
	w.bw.putBool(allPositive)
	w.bw.put(uint64(shape), shapeBits)
	return w
}

// Shape and AllPositive report what the header was built with.
func (w *Writer) Shape() Shape      { return w.shape }
func (w *Writer) AllPositive() bool { return w.allPositive }

// Bits is how many bits have been written, header included. Useful for sizing
// experiments; the final message is this rounded up to a byte.
func (w *Writer) Bits() int { return w.bw.bits() }

// Key writes a field id, introducing the value that follows.
func (w *Writer) Key(k uint8) { w.bw.put(uint64(k), keyBits) }

// End closes the current record with the terminator key.
func (w *Writer) End() { w.bw.put(uint64(TerminatorKey), keyBits) }

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
	for _, b := range w.scratch {
		w.bw.put(uint64(b), 8)
	}
}

// Bytes writes a length-prefixed blob. Unlike a string it has no codec of its
// own, so the length uses the same varint as every other integer.
func (w *Writer) Bytes(b []byte) {
	w.bw.putVarint(uint64(len(b)))
	for _, x := range b {
		w.bw.put(uint64(x), 8)
	}
}

// Done pads the stream to a byte boundary and returns the finished message. The
// pad is never read back: a record ends at its terminator and the message ends
// at its last record, so the decoder stops before reaching it.
func (w *Writer) Done() []byte { return w.bw.flush() }
