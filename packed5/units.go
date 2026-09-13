package packed5

import "encoding/binary"

// The packing kernel: eight five-bit units in, five bytes out.
//
// 8 * 5 = 40 bits = 5 bytes exactly, so a group needs no padding and no
// accumulator that survives it. The writer builds one uint64 with eight
// compile-time shifts, stores eight bytes and advances five; the three bytes it
// overshoots are the three the next group rewrites. The reader is the mirror:
// one unaligned load, eight shifts, advance five.
//
// Measured against the bit accumulator this replaces — which drained 32 bits at
// a time, so it was already writing whole bytes — that is 3.4x on the write side
// and 5.5x on the read side, for byte-identical output. See
// experiments/stringpack for the isolated kernel benchmark.
//
// The cost is slack. A writer needs eight spare bytes past the payload it is
// building, which slices.Grow supplies; a reader needs up to seven readable past
// the payload, which in a column of back-to-back frames comes from the next
// frame and at the very end comes from the bounded tail path below.

// writer packs units into a caller-supplied buffer.
//
// limit is the byte position the payload may not pass. The packed form is only
// ever used when it beats the raw bytes, so bounding the buffer by the raw
// length is both the allocation bound and the early-out: a stream that would not
// have been kept stops being written the moment it grows past its own budget.
type writer struct {
	buf   []byte
	p     int // next byte to write
	limit int // p may not exceed this before a store
	acc   uint64
	n     uint8 // units pending in acc, always < 8
	units int
	over  bool // the packed form passed limit; the caller must fall back to raw
}

// A writer is built as a literal rather than through a reset method: storing a
// slice into a struct through a pointer receiver is an unconditional leak as far
// as escape analysis is concerned, and that is enough to push a caller's stack
// scratch onto the heap. See Size, which depends on it staying there.
//
// The caller owns the buffer and must leave eight bytes past limit for the final
// store to overshoot into.

// put appends one unit. Every eighth one stores a group; the rest are a shift,
// an or and an increment.
func (w *writer) put(v uint8) {
	w.acc |= uint64(v) << (5 * w.n)
	w.n++
	w.units++
	if w.n == 8 {
		if w.p > w.limit {
			w.over = true
		} else {
			binary.LittleEndian.PutUint64(w.buf[w.p:], w.acc)
			w.p += 5
		}
		w.n, w.acc = 0, 0
	}
}

// finish flushes the partial group, pads the stream to the unit grid and returns
// the payload byte length. It reports false when the stream passed its budget,
// in which case nothing it wrote may be used.
func (w *writer) finish() (int, bool) {
	// One trailing CASE_TOGGLE_SIMPLE lands the stream on the grid when the byte
	// rounding leaves room for a unit the tokens did not fill. It applies to no
	// letter, so it decodes to nothing, and it cannot grow the payload: the gap
	// exists exactly when 5*(units+1) still fits in the same number of bytes.
	if payloadUnits(payloadBytes(w.units)) > w.units {
		w.put(opCaseSimple)
	}
	if w.n > 0 {
		if w.p > w.limit {
			w.over = true
		} else {
			binary.LittleEndian.PutUint64(w.buf[w.p:], w.acc)
			w.p += payloadBytes(int(w.n))
			w.n, w.acc = 0, 0
		}
	}
	if w.over {
		return 0, false
	}
	return w.p, true
}
