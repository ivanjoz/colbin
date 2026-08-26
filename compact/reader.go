package compact

import (
	"math"

	"github.com/ivanjoz/colbin/packed5"
)

// Reader walks a compact message. Reads are sticky: once one runs past the end
// of the stream every later read returns a zero value and leaves the error set,
// so a caller may decode a whole record and check Err once at the end rather
// than after every field.
type Reader struct {
	br          bitReader
	shape       Shape
	allPositive bool
	scratch     []byte // reused buffer for shifting packed5 frames into place
}

// NewReader parses the header of a compact message. It rejects a buffer whose
// first bit is clear, which is the whole of the mode discrimination.
func NewReader(buf []byte) (*Reader, error) {
	if !IsCompact(buf) {
		return nil, ErrNotCompact
	}
	r := &Reader{br: newBitReader(buf)}
	r.br.get(modeBits) // the compact bit, already checked
	r.allPositive = r.br.getBool()
	r.shape = Shape(r.br.get(shapeBits))
	if r.br.err != nil {
		return nil, r.br.err
	}
	return r, nil
}

// Shape and AllPositive report what the header declared.
func (r *Reader) Shape() Shape      { return r.shape }
func (r *Reader) AllPositive() bool { return r.allPositive }

// Records is how many records the message holds, 1..MaxRecords.
func (r *Reader) Records() int { return r.shape.Records() }

// Err reports the first error any read hit, or nil.
func (r *Reader) Err() error { return r.br.err }

// Key reads a field id. TerminatorKey closes the current record.
func (r *Reader) Key() uint8 { return uint8(r.br.get(keyBits)) }

// Int reverses Writer.Int, applying whatever ALL_POSITIVE declared.
func (r *Reader) Int() int64 {
	v := r.br.getVarint()
	if r.allPositive {
		return int64(v)
	}
	return unzigzag(v)
}

// Uint reverses Writer.Uint.
func (r *Reader) Uint() uint64 { return r.br.getVarint() }

// Bool reverses Writer.Bool.
func (r *Reader) Bool() bool { return r.br.getBool() }

// Float32 and Float64 reverse their Writer counterparts.
func (r *Reader) Float32() float32 { return math.Float32frombits(uint32(r.br.get(32))) }
func (r *Reader) Float64() float64 { return math.Float64frombits(r.br.get(64)) }

// Str reads a packed5 frame. It is deliberately not called String: that
// signature would make Reader an fmt.Stringer, and printing a reader would then
// consume a field and move the cursor. packed5 is byte structured, so when the stream
// is not byte aligned the remaining bytes are shifted into scratch first; when
// it is aligned the underlying buffer is passed straight through and nothing is
// copied.
func (r *Reader) Str() string {
	if r.br.err != nil {
		return ""
	}
	tail, scratch := r.br.alignedTail(r.scratch)
	r.scratch = scratch
	s, n, err := packed5.Decode(tail)
	if err != nil {
		r.br.err = ErrBadString
		return ""
	}
	// A well formed frame never extends into the final pad, so a frame that
	// would is a corrupt one rather than a short read of valid data.
	if !r.advanceBytes(n) {
		return ""
	}
	return s
}

// Bytes reads a length-prefixed blob into a freshly allocated slice.
//
// The declared length is compared against the bits actually remaining before it
// is converted or used to size anything. A corrupt varint can name a length near
// 2^64, and computing its bit width first would overflow int back into a small
// or negative number that passes the bounds check and then panics in make.
func (r *Reader) Bytes() []byte {
	n, ok := r.arrayLen(8)
	if !ok {
		return nil
	}
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(r.br.get(8))
	}
	return out
}

// Skip advances past a value of the given kind without materialising it, which
// is what lets a reader step over a field id its type does not know.
func (r *Reader) Skip(k Kind) {
	switch k {
	case KindInt, KindUint:
		r.br.getVarint()
	case KindBool:
		r.br.get(1)
	case KindFloat32:
		r.br.get(32)
	case KindFloat64:
		r.br.get(64)
	case KindString:
		r.Str()
	case KindBytes:
		r.Bytes()
	case KindInt8s:
		GetInts[int8](r)
	case KindInt16s:
		GetInts[int16](r)
	case KindInt32s:
		GetInts[int32](r)
	case KindInt64s:
		GetInts[int64](r)
	case KindStrs:
		r.Strs()
	case KindFloat32s:
		r.Float32s()
	case KindFloat64s:
		r.Float64s()
	case KindBools:
		r.Bools()
	}
}

// Kind names the value forms a compact field can hold. It is what Skip needs to
// step over an unknown field: every form is self delimiting, but only once you
// know which one you are looking at.
type Kind uint8

const (
	KindInt Kind = iota
	KindUint
	KindBool
	KindFloat32
	KindFloat64
	KindString
	KindBytes

	// Array forms. The integer widths are distinct kinds because varint derives
	// the element width from the Go type rather than the wire, so stepping over
	// an integer array needs the width the same way decoding it does.
	KindInt8s
	KindInt16s
	KindInt32s
	KindInt64s
	KindStrs
	KindFloat32s
	KindFloat64s
	KindBools
)
