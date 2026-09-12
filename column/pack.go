// Package varint implements colbin's column codec for arrays of int8, int16,
// int32 and int64: a transform, then blocks of 128 residuals packed at an exact
// bit width chosen per block.
//
// # Why blocks of 128
//
// 128 values at w bits occupy 128w/8 = 16w bytes — a whole number of bytes for
// every w, and a whole number of 64-bit words too. So a block is byte-aligned at
// both ends, no padding is ever wasted, and no state crosses a block boundary.
// The width leaves the element loop, which is what keeps the read fast, and the
// ladder is still one bit fine, which is what keeps it small.
//
// That combination is why this replaced the (k, M) bit varint that used to live
// here: measured over five column shapes, it is smaller on every one of them —
// from −59% to +1% — and 2–3× faster to decode, 2–4× faster to encode. The old
// codec spent a continuation flag per byte and reclaimed some of them with two
// declared parameters; this one spends nothing per value and one byte per 128.
//
// # Reading a value is one load
//
// Values are packed least-significant-bit first with no gap, so value i starts
// at bit i*w and:
//
//	v = (LittleEndian.Uint64(buf[pos>>3:]) >> (pos & 7)) & mask
//
// holds for every w <= 57, with no dependency on the value before it. The cost
// is that the read touches up to 8 bytes past the value it wants; DecodeArray
// takes a slower path for the tail of a buffer rather than requiring slack.
//
// # Layout
//
//	column := [header:1] [base: 8 bytes]? block*
//
//	header  bits 0-1  transform: raw / delta / frame-of-reference / constant
//	        bit  2    zigzag applied to residuals
//	        bits 3-7  reserved
//
//	base    the delta's first value or the frame's minimum, zigzagged, in the
//	        clear at full width. Folding it into the residuals instead would set
//	        the first block's width from one element, which measured +97% on a
//	        column of timestamps.
//
//	block  := [width: 1 byte, in bits 0..64] [16 × width bytes]
//
//	        width 0 is a block of nothing but zeros and occupies one byte, which
//	        is what makes a sparse or constant run nearly free. A final block
//	        holding fewer than 128 residuals occupies ceil(count*width/8) bytes.
//
//	constant columns have no blocks: the header is followed by the one value.
//
// The element count is not stored; it is known from the record count, matching
// the convention of the other colbin column codecs.
//
// This file holds the packing primitives; array.go holds the transforms, the
// block planner and the exported array codec.
package column

import (
	"encoding/binary"
	"errors"
	"math/bits"
)

var (
	ErrTruncated     = errors.New("colbin: varint array truncated")
	ErrNegativeCount = errors.New("colbin: negative varint array count")
	ErrShortBuffer   = errors.New("colbin: varint array output slice too short")
	ErrBadWidth      = errors.New("colbin: varint block width above 64 bits")
)

// blockSize is how many residuals share one width byte. 128 is what makes every
// width land on a byte boundary, and it puts the width header at 0.8% overhead
// while letting a single outlier widen its own block rather than the column.
const blockSize = 128

// wideWidth is the largest width the one-load read covers. A value that starts
// anywhere in a byte and is at most 57 bits wide always finishes inside the
// eight bytes that load spans; past it the read needs a second byte and takes
// the out-of-line path.
const wideWidth = 57

// zigzag maps signed to unsigned so that small magnitudes stay short:
// 0,-1,1,-2,2 -> 0,1,2,3,4.
func zigzag(v int64) uint64 { return uint64(v<<1) ^ uint64(v>>63) }

// unzigzag inverts zigzag.
func unzigzag(u uint64) int64 { return int64(u>>1) ^ -int64(u&1) }

// blockBytes is what a run of count residuals occupies at w bits.
func blockBytes(count, w int) int { return (count*w + 7) / 8 }

// widthOf is the width a block of residuals needs: the widest one's bit length,
// with no rounding. OR-ing them together and taking one bit length is the same
// answer as a running maximum, without the compare.
func widthOf(block []uint64) int {
	var set uint64
	for _, residual := range block {
		set |= residual
	}
	return bits.Len64(set)
}

// packRun writes len(block) residuals at w bits each, least significant bit
// first, with no gap between them and none at the end beyond the final partial
// byte.
//
// A full block is 2w whole 64-bit words, so the accumulator flushes exactly and
// only the last block of a column can end mid-word.
func packRun(out []byte, block []uint64, w int) []byte {
	if w == 0 {
		return out
	}
	var accumulator uint64
	accumulated := 0
	for _, residual := range block {
		if accumulated+w < 64 {
			accumulator |= residual << accumulated
			accumulated += w
			continue
		}
		// This residual straddles the word boundary, or lands exactly on it.
		low := 64 - accumulated
		accumulator |= residual << accumulated
		out = binary.LittleEndian.AppendUint64(out, accumulator)
		accumulator, accumulated = 0, 0
		if low < w {
			accumulator = residual >> low
			accumulated = w - low
		}
	}
	for accumulated > 0 {
		out = append(out, byte(accumulator))
		accumulator >>= 8
		accumulated -= 8
	}
	return out
}

// unpackRun reads len(dst) residuals at w bits each. It is the read the whole
// layout is for: one load, one shift and one mask per value, with the width
// hoisted out of the loop and no carry between iterations.
//
// buf must hold the run; whether it holds eight bytes beyond it decides which
// path is taken, not whether the read succeeds.
func unpackRun(buf []byte, dst []uint64, w int) {
	if w == 0 {
		clear(dst)
		return
	}
	if w > wideWidth {
		unpackWide(buf, dst, w)
		return
	}
	mask := uint64(1)<<w - 1
	if len(buf) < blockBytes(len(dst), w)+8 {
		unpackTail(buf, dst, w, mask)
		return
	}
	position := 0
	for index := range dst {
		dst[index] = (binary.LittleEndian.Uint64(buf[position>>3:]) >> (position & 7)) & mask
		position += w
	}
}

// unpackTail is unpackRun where the eight-byte load could read past the buffer,
// which is only ever the last block of a column. It gathers each value from the
// bytes that are actually there.
//
// Splitting this — computing how many values still fit a whole load and running
// those through the fast path — was tried and reverted. It measured within noise
// on both a 1024-element column, where the tail is one block in eight, and a
// 16-element one, where the tail is the whole thing. The profiler attributes 9%
// to this function; recovering it is not worth a division and two loops.
func unpackTail(buf []byte, dst []uint64, w int, mask uint64) {
	position := 0
	for index := range dst {
		// w is at most 57 here, so the value ends within eight bytes of where it
		// starts and gathering those eight is enough.
		dst[index] = (gather8(buf, position>>3) >> (position & 7)) & mask
		position += w
	}
}

// unpackWide covers w in 58..64, where one eight-byte load can no longer be
// guaranteed to hold a whole value: starting at bit 7 of a byte, a 64-bit value
// reaches into a ninth. It is out of line because it is the rare width, and a
// column that reaches it is incompressible anyway.
func unpackWide(buf []byte, dst []uint64, w int) {
	// The mask is built in two shifts so that w == 64 does not shift by 64,
	// which is undefined for a uint64.
	mask := uint64(1)<<(w-1)<<1 - 1
	position := 0
	for index := range dst {
		at := position >> 3
		shift := position & 7
		value := gather8(buf, at) >> shift
		if shift != 0 && at+8 < len(buf) {
			value |= uint64(buf[at+8]) << (64 - shift)
		}
		dst[index] = value & mask
		position += w
	}
}

// gather8 reads eight little-endian bytes from at, stopping at the end of the
// buffer rather than past it. It is the safe form of the one-load read, for the
// tail of a buffer where there is nothing to load.
func gather8(buf []byte, at int) uint64 {
	if at+8 <= len(buf) {
		return binary.LittleEndian.Uint64(buf[at:])
	}
	var value uint64
	for index := 0; at+index < len(buf) && index < 8; index++ {
		value |= uint64(buf[at+index]) << (8 * index)
	}
	return value
}
