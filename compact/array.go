package compact

import (
	"math"

	"github.com/ivanjoz/colbin/varint"
)

// Arrays of primitives use exactly the codecs the standard mode uses, so an
// array field costs the same in either mode. Compact mode changes how records
// are framed, not how a column of values is compressed: a run of correlated ids
// gets varint's delta transform here just as it would in a columnar message.
//
// Every array is written as [count:varint] followed by the codec's own output,
// with no separate byte length -- the count plus the element type is all the
// codecs need to find the end. A count of zero writes nothing further, because
// varint.AppendArray on an empty slice still emits its header byte and
// DecodeArray cannot read one back.

// PutInts writes an integer slice with varint.AppendArray, the same codec that
// backs an ftInt column in the standard mode.
//
// The element width comes from T on both sides and never reaches the wire, so
// GetInts must be instantiated with the type used to write. int is deliberately
// absent for the reason varint gives: its width is platform dependent, so an
// []int written on a 64-bit host would decode silently wrong on a 32-bit one.
// Unsigned slices convert through the same-width signed type, which preserves
// the bit pattern, exactly as codec/integer.go does.
func PutInts[T varint.Signed](w *Writer, vals []T) {
	w.bw.putVarint(uint64(len(vals)))
	if len(vals) == 0 {
		return
	}
	w.scratch = varint.AppendArray(w.scratch[:0], vals)
	for _, b := range w.scratch {
		w.bw.put(uint64(b), 8)
	}
}

// GetInts reverses PutInts. It must be instantiated with the same T.
func GetInts[T varint.Signed](r *Reader) []T {
	n, ok := r.arrayLen(8) // varint spends at least one byte per element
	if !ok || n == 0 {
		return nil
	}
	out := make([]T, n)
	tail, scratch := r.br.alignedTail(r.scratch)
	r.scratch = scratch
	consumed, err := varint.DecodeArray(tail, n, out)
	if err != nil {
		r.br.err = err
		return nil
	}
	if !r.advanceBytes(consumed) {
		return nil
	}
	return out
}

// Strs writes a string slice as consecutive packed5 frames, which is what an
// ftString column already is in the standard mode. The frames are self
// delimiting, so only the element count precedes them.
func (w *Writer) Strs(vals []string) {
	w.bw.putVarint(uint64(len(vals)))
	for _, s := range vals {
		w.Str(s)
	}
}

// Strs reverses Writer.Strs.
func (r *Reader) Strs() []string {
	n, ok := r.arrayLen(8) // a packed5 frame is at least one byte
	if !ok || n == 0 {
		return nil
	}
	out := make([]string, n)
	for i := range out {
		out[i] = r.Str()
		if r.br.err != nil {
			return nil
		}
	}
	return out
}

// Float32s and Float64s write raw IEEE-754 elements, matching the standard
// mode's float columns. Floats have no small-value form worth exploiting, so
// there is no transform to share.
func (w *Writer) Float32s(vals []float32) {
	w.bw.putVarint(uint64(len(vals)))
	for _, f := range vals {
		w.bw.put(uint64(math.Float32bits(f)), 32)
	}
}

func (w *Writer) Float64s(vals []float64) {
	w.bw.putVarint(uint64(len(vals)))
	for _, f := range vals {
		w.bw.put(math.Float64bits(f), 64)
	}
}

func (r *Reader) Float32s() []float32 {
	n, ok := r.arrayLen(32)
	if !ok || n == 0 {
		return nil
	}
	out := make([]float32, n)
	for i := range out {
		out[i] = math.Float32frombits(uint32(r.br.get(32)))
	}
	return out
}

func (r *Reader) Float64s() []float64 {
	n, ok := r.arrayLen(64)
	if !ok || n == 0 {
		return nil
	}
	out := make([]float64, n)
	for i := range out {
		out[i] = math.Float64frombits(r.br.get(64))
	}
	return out
}

// Bools writes a bitmap, one bit per element. This is the one array form that
// does not match the standard mode, which carries bools through an integer
// column: compact mode already spends a single bit on a scalar bool, and
// spreading that convention across a slice is both cheaper and consistent.
func (w *Writer) Bools(vals []bool) {
	w.bw.putVarint(uint64(len(vals)))
	for _, b := range vals {
		w.bw.putBool(b)
	}
}

// Bools reverses Writer.Bools.
func (r *Reader) Bools() []bool {
	n, ok := r.arrayLen(1)
	if !ok || n == 0 {
		return nil
	}
	out := make([]bool, n)
	for i := range out {
		out[i] = r.br.getBool()
	}
	return out
}

// arrayLen reads an element count and checks it against the bits actually
// remaining, given the smallest number of bits one element can occupy. A corrupt
// count near 2^64 must be rejected before it reaches make, and before any
// arithmetic on it can overflow int back into a plausible-looking value.
func (r *Reader) arrayLen(minBitsPerElem int) (int, bool) {
	n := r.br.getVarint()
	if r.br.err != nil {
		return 0, false
	}
	if n > uint64(r.br.limit-r.br.pos)/uint64(minBitsPerElem) {
		r.br.err = ErrTruncated
		return 0, false
	}
	return int(n), true
}

// advanceBytes steps the cursor over a byte-structured payload a sub-codec has
// just consumed, refusing one that would run into the final pad.
func (r *Reader) advanceBytes(n int) bool {
	if uint64(n) > uint64(r.br.limit-r.br.pos)/8 {
		r.br.err = ErrTruncated
		return false
	}
	r.br.pos += 8 * n
	return true
}
