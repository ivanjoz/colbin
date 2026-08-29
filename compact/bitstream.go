package compact

import "encoding/binary"

// LSB-first bit packing. Values are written low bit first and the reader mirrors
// that order, so nothing in a compact message is byte aligned except the final
// pad. Writes and reads are split into chunks of at most 32 bits so the uint64
// accumulator, which holds up to 31 leftover bits between calls, can never
// overflow: 31 + 32 < 64.
//
// Both sides move four bytes at a time. A bit codec that appends or loads one
// byte per iteration spends most of its time on the loop rather than on the
// bits: a compact record is a run of short fields, so the per-call overhead is
// what the format actually costs. The writer therefore holds up to 31 pending
// bits and emits a whole uint32 when it has one, and the reader takes eight
// bytes in a single load and shifts the field out of them.

// bitWriter accumulates bits LSB-first into buf.
type bitWriter struct {
	buf     []byte
	current uint64 // pending bits not yet flushed to buf (always < 32 between calls)
	nbits   uint8  // valid bits currently held in current
}

// put appends the low width bits of v to the stream (width 0..64).
func (w *bitWriter) put(v uint64, width uint8) {
	for width > 32 {
		w.chunk(v&0xFFFFFFFF, 32)
		v >>= 32
		width -= 32
	}
	w.chunk(v, width)
}

// putBool writes one bit.
func (w *bitWriter) putBool(b bool) {
	if b {
		w.chunk(1, 1)
		return
	}
	w.chunk(0, 1)
}

// chunk writes width <= 32 bits, keeping every shift inside uint64 range: at
// most 31 pending bits plus 32 new ones is 63.
func (w *bitWriter) chunk(v uint64, width uint8) {
	if width == 0 {
		return
	}
	v &= (uint64(1) << width) - 1
	w.current |= v << w.nbits
	w.nbits += width
	if w.nbits >= 32 {
		w.buf = binary.LittleEndian.AppendUint32(w.buf, uint32(w.current))
		w.current >>= 32
		w.nbits -= 32
	}
}

// putBytes writes b as consecutive 8-bit units. When the stream is already byte
// aligned -- which it is for a packed5 frame that follows a whole number of
// bytes -- the bytes are copied straight in rather than shifted through the
// accumulator one at a time.
func (w *bitWriter) putBytes(b []byte) {
	if w.nbits%8 == 0 {
		w.flushWhole()
		w.buf = append(w.buf, b...)
		return
	}
	for _, x := range b {
		w.chunk(uint64(x), 8)
	}
}

// flushWhole moves every complete pending byte into buf, leaving current with
// only the bits that do not fill one. It is only correct to call when what
// follows starts on a byte boundary.
func (w *bitWriter) flushWhole() {
	for w.nbits >= 8 {
		w.buf = append(w.buf, byte(w.current))
		w.current >>= 8
		w.nbits -= 8
	}
}

// bits is how many bits have been written so far, flushed plus pending.
func (w *bitWriter) bits() int { return len(w.buf)*8 + int(w.nbits) }

// flush writes any remaining partial byte, zero padded in its high bits, and
// returns the finished buffer. The padding is never read back: a record ends at
// its terminator key and a message ends at its last record, so the decoder stops
// before reaching it.
func (w *bitWriter) flush() []byte {
	w.flushWhole()
	if w.nbits > 0 {
		w.buf = append(w.buf, byte(w.current))
		w.current = 0
		w.nbits = 0
	}
	return w.buf
}

// bitReader reads bits LSB-first from buf, mirroring bitWriter. pos is a bit
// offset rather than a byte offset, and every read is bounds checked: a
// truncated or corrupt message must return an error, never panic.
type bitReader struct {
	buf   []byte
	pos   int // next bit to consume
	limit int // total bits available, len(buf)*8
	err   error
}

func newBitReader(buf []byte) bitReader {
	return bitReader{buf: buf, limit: len(buf) * 8}
}

// get returns the next width bits (width 0..64) as the low bits of the result.
// Once a read has run past the end the reader is sticky: every later call
// returns zero and leaves err set, so callers may batch their error checks.
func (r *bitReader) get(width uint8) uint64 {
	if r.err != nil {
		return 0
	}
	if r.pos+int(width) > r.limit {
		r.err = ErrTruncated
		return 0
	}
	var out uint64
	var shift uint8
	for width > 32 {
		out |= r.chunk(32) << shift
		shift += 32
		width -= 32
	}
	return out | r.chunk(width)<<shift
}

// getBool reads one bit.
func (r *bitReader) getBool() bool { return r.get(1) == 1 }

// chunk reads width <= 32 bits. The caller has already bounds checked the whole
// read, so this only has to assemble bytes.
//
// A width of up to 32 bits starting mid byte spans at most 5 bytes, so eight
// bytes always cover it and the shift stays inside uint64: 7 + 32 is 39. Away
// from the end of the buffer that is one unaligned load; the last few bytes take
// the copy path, since a load there would run off the slice.
func (r *bitReader) chunk(width uint8) uint64 {
	if width == 0 {
		return 0
	}
	i, sh := r.pos/8, uint(r.pos%8)
	var v uint64
	if i+8 <= len(r.buf) {
		v = binary.LittleEndian.Uint64(r.buf[i:])
	} else {
		var tail [8]byte
		copy(tail[:], r.buf[i:])
		v = binary.LittleEndian.Uint64(tail[:])
	}
	r.pos += int(width)
	return (v >> sh) & ((uint64(1) << width) - 1)
}

// align advances to the next byte boundary and reports the bit offset it had.
// Nothing in the format needs it; it exists for tests that assert alignment.
func (r *bitReader) align() { r.pos = (r.pos + 7) &^ 7 }

// alignedTail returns the unread bits as a byte slice starting at a byte
// boundary, so packed5 -- a byte-structured codec -- can be read out of a stream
// that is not byte aligned. When the reader already sits on a boundary the
// underlying buffer is returned directly and nothing is copied.
//
// Otherwise the tail is shifted into scratch, which costs one pass over the
// remaining bytes per string field. Compact messages hold at most three records,
// so that remainder is bounded by a small constant rather than by the payload
// size the way it would be in standard mode.
func (r *bitReader) alignedTail(scratch []byte) ([]byte, []byte) {
	i, sh := r.pos/8, uint(r.pos%8)
	if sh == 0 {
		return r.buf[i:], scratch
	}
	n := len(r.buf) - i
	if cap(scratch) < n {
		scratch = make([]byte, n)
	}
	scratch = scratch[:n]
	for j := range n {
		v := r.buf[i+j] >> sh
		if i+j+1 < len(r.buf) {
			v |= r.buf[i+j+1] << (8 - sh)
		}
		scratch[j] = v
	}
	return scratch, scratch
}
