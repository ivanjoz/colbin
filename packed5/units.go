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
// The cost is slack. A writer needs eight spare bytes past the payload it is
// building, which AppendPayload reserves; a reader wants up to seven readable
// past the payload, which AppendString takes from whatever follows it in src and
// otherwise supplies with a bounded copy of the last few bytes.

// writer packs units into a caller-supplied buffer, which must leave eight bytes
// past limit for the final store to overshoot into.
//
// limit is the byte position the payload may not pass. The packed form is only
// used when it beats the raw bytes, so bounding the buffer by the raw length is
// both the allocation bound and the early-out: a stream that would not be kept
// stops being written the moment it passes its budget.
//
// Build it as a literal rather than through a reset method: storing a slice into
// a struct through a pointer receiver leaks it as far as escape analysis is
// concerned, which would push a caller's stack buffer onto the heap.
type writer struct {
	buf   []byte
	p     int // next byte to write
	limit int // p may not exceed this before a store
	acc   uint64
	n     uint8 // units pending in acc, always < 8
	units int
	over  bool // the packed form passed limit; the caller must fall back to raw
}

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
	// The grid pad: a trailing CASE_TOGGLE_SIMPLE applies to no letter, so it
	// decodes to nothing, and it is only added when 5*(units+1) bits still fit in
	// the same number of bytes.
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
