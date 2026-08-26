package packed5

// LSB-first bit packing, matching the convention of the parent colbin package's
// bitstream. It is reimplemented here, rather than imported, so that packed5
// stays independent of its importer — the same reason varint carries its own
// copy of signExtend.
//
// No packed5 token field is wider than 10 bits, so both halves take a uint8
// width and never need the >32-bit chunking the parent's version does.

// bitWriter appends bits LSB-first onto buf. buf is the caller's output slice,
// so the frame header can be appended first and the stream written in place.
type bitWriter struct {
	buf     []byte
	current uint32 // pending bits not yet flushed to buf; fewer than 8 between calls
	nbits   uint8
}

// writeBits appends the low width bits of v (width <= 24).
func (w *bitWriter) writeBits(v uint32, width uint8) {
	w.current |= (v & (1<<width - 1)) << w.nbits
	w.nbits += width
	for w.nbits >= 8 {
		w.buf = append(w.buf, byte(w.current))
		w.current >>= 8
		w.nbits -= 8
	}
}

// flush emits any partial trailing byte, zero-padded in its high bits, and
// returns the buffer.
func (w *bitWriter) flush() []byte {
	if w.nbits > 0 {
		w.buf = append(w.buf, byte(w.current))
		w.current = 0
		w.nbits = 0
	}
	return w.buf
}

// bitReader reads bits LSB-first from buf. limit is the number of bits that are
// payload rather than trailing pad, so reads past the end of the token stream
// fail instead of returning zeros.
type bitReader struct {
	buf   []byte
	bit   int // absolute bit offset of the next unread bit
	limit int // total readable bits
}

// remaining is how many payload bits are still unread.
func (r *bitReader) remaining() int { return r.limit - r.bit }

// read returns the next width bits (width <= 24), or false if fewer than width
// payload bits remain.
func (r *bitReader) read(width uint8) (uint32, bool) {
	if r.bit+int(width) > r.limit {
		return 0, false
	}
	i := r.bit >> 3
	off := uint(r.bit & 7)
	// off <= 7 and width <= 24, so at most four bytes are touched.
	var acc uint32
	for n := 0; n*8 < int(off)+int(width); n++ {
		acc |= uint32(r.buf[i+n]) << (8 * n)
	}
	r.bit += int(width)
	return (acc >> off) & (1<<width - 1), true
}
