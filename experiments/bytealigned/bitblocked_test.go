package bytealigned

// The idea the byte-aligned column gave up on too early: a block of 128
// residuals packed at an exact *bit* width.
//
// The reason it is worth revisiting is arithmetic. 128 values at w bits occupy
// 128w/8 = 16w bytes — a whole number of bytes for every w, and a whole number
// of 64-bit words too. So a bit-packed block is byte-aligned at both ends, no
// padding is ever wasted, and no state crosses a block boundary. The property
// that made the byte-aligned column fast (§1.3 of the plan: the width leaves the
// element loop) is untouched; only the width ladder gets finer, from
// {1,2,3,4,6,8} bytes to 1..64 bits.
//
// Unpacking one value is a single unaligned 64-bit load, a shift and a mask,
// with no dependency on the value before it:
//
//	v = (LittleEndian.Uint64(buf[pos>>3:]) >> (pos & 7)) & mask
//
// which holds for every w <= 57, because a value that starts anywhere in a byte
// and is at most 57 bits wide always finishes inside the 8 bytes that load
// covers.

import (
	"encoding/binary"
	"math/bits"
	"testing"
	"unsafe"

	"github.com/ivanjoz/colbin/column"
)

func varintSize(vals []int64) int { return len(column.AppendArray(nil, vals)) }

// asUint64s lets a block be unpacked straight into the caller's []int64 and
// reinterpreted afterwards, rather than through a second buffer.
func asUint64s(dst []int64) []uint64 {
	if len(dst) == 0 {
		return nil
	}
	return unsafe.Slice((*uint64)(unsafe.Pointer(&dst[0])), len(dst))
}

// readAhead is how many bytes past its last the unpacker may touch, because it
// loads 8 bytes to extract as few as one bit. A real decoder either pads its
// read buffer by this much or takes a slow path for the final block; the
// experiment pads, and the plan calls the choice out.
const readAhead = 8

// bitWidth is the exact width a block needs: the widest residual's bit length,
// with no rounding to a byte at all.
func bitWidth(block []uint64) int {
	var set uint64
	for _, r := range block {
		set |= r
	}
	return bits.Len64(set)
}

// bitBlockedCost is blockedCost with the byte rounding taken out.
func bitBlockedCost(vals []int64, transform int, base int64) int {
	n := residualsUnder(vals, transform)
	total := 0
	if transform != trRaw {
		total += 8
	}
	for start := 0; start < n; start += blockSize {
		end := min(start+blockSize, n)
		var set uint64
		switch transform {
		case trFOR:
			for _, v := range vals[start:end] {
				set |= uint64(v) - uint64(base)
			}
		case trDelta:
			for i := start; i < end; i++ {
				set |= zigzag(vals[i+1] - vals[i])
			}
		default:
			for _, v := range vals[start:end] {
				set |= zigzag(v)
			}
		}
		w := bits.Len64(set)
		total += 1 + ((end-start)*w+7)/8
	}
	return total
}

func AppendBitBlocked(out []byte, vals []int64) []byte {
	if len(vals) == 0 {
		return append(out, 0)
	}
	minVal := vals[0]
	for _, v := range vals {
		if v < minVal {
			minVal = v
		}
	}

	transform := trRaw
	bestCost := bitBlockedCost(vals, trRaw, minVal)
	if c := bitBlockedCost(vals, trFOR, minVal); c < bestCost {
		transform, bestCost = trFOR, c
	}
	if c := bitBlockedCost(vals, trDelta, minVal); c < bestCost {
		transform = trDelta
	}

	residuals := scratch[:0]
	out = append(out, byte(transform))
	switch transform {
	case trFOR:
		out = putLE(out, zigzag(minVal), 8)
		for _, v := range vals {
			residuals = append(residuals, uint64(v)-uint64(minVal))
		}
	case trDelta:
		out = putLE(out, zigzag(vals[0]), 8)
		for i := 1; i < len(vals); i++ {
			residuals = append(residuals, zigzag(vals[i]-vals[i-1]))
		}
	default:
		for _, v := range vals {
			residuals = append(residuals, zigzag(v))
		}
	}
	scratch = residuals[:0]

	for start := 0; start < len(residuals); start += blockSize {
		block := residuals[start:min(start+blockSize, len(residuals))]
		w := bitWidth(block)
		out = append(out, byte(w))
		out = packRun(out, block, w)
	}
	return out
}

// packRun writes len(block) values at w bits each, least significant bit first,
// with no gap between values and none at the end beyond the final partial byte.
func packRun(out []byte, block []uint64, w int) []byte {
	if w == 0 {
		return out
	}
	var acc uint64
	accBits := 0
	for _, v := range block {
		if accBits+w < 64 {
			acc |= v << accBits
			accBits += w
			continue
		}
		// This value straddles the word boundary, or lands exactly on it.
		low := 64 - accBits
		acc |= v << accBits
		out = binary.LittleEndian.AppendUint64(out, acc)
		acc, accBits = 0, 0
		if low < w {
			acc = v >> low
			accBits = w - low
		}
	}
	for accBits > 0 {
		out = append(out, byte(acc))
		acc >>= 8
		accBits -= 8
	}
	return out
}

// unpackRun is the read the whole design is for: one load, one shift, one mask
// per value, and no carry between iterations.
func unpackRun(buf []byte, dst []uint64, w int) int {
	if w == 0 {
		clear(dst)
		return 0
	}
	nbytes := (len(dst)*w + 7) / 8
	if w > 57 {
		unpackWide(buf, dst, w)
		return nbytes
	}
	mask := uint64(1)<<w - 1
	pos := 0
	for i := range dst {
		dst[i] = (binary.LittleEndian.Uint64(buf[pos>>3:]) >> (pos & 7)) & mask
		pos += w
	}
	return nbytes
}

// unpackWide covers w in 58..64, where one 8-byte load can no longer be
// guaranteed to hold a whole value. Kept out of line: it is the rare width, and
// a column that reaches it is usually incompressible anyway.
func unpackWide(buf []byte, dst []uint64, w int) {
	mask := uint64(1)<<(w-1)<<1 - 1
	pos := 0
	for i := range dst {
		lo := binary.LittleEndian.Uint64(buf[pos>>3:]) >> (pos & 7)
		if shift := pos & 7; shift != 0 {
			lo |= uint64(buf[(pos>>3)+8]) << (64 - shift)
		}
		dst[i] = lo & mask
		pos += w
	}
}

func DecodeBitBlocked(buf []byte, n int, out []int64) int {
	transform := int(buf[0])
	pos := 1
	var base int64
	dst := out[:n]
	if transform != trRaw {
		base = unzig(getLE(buf[pos:], 8))
		pos += 8
		if transform == trDelta {
			out[0] = base
			dst = out[1:n]
		}
	}
	raw := asUint64s(dst)
	for start := 0; start < len(raw); start += blockSize {
		block := raw[start:min(start+blockSize, len(raw))]
		w := int(buf[pos])
		pos++
		pos += unpackRun(buf[pos:], block, w)
	}
	switch transform {
	case trFOR:
		for i := range dst {
			dst[i] = int64(uint64(base) + uint64(dst[i]))
		}
	case trDelta:
		acc := base
		for i := range dst {
			acc += unzig(uint64(dst[i]))
			dst[i] = acc
		}
	default:
		for i := range dst {
			dst[i] = unzig(uint64(dst[i]))
		}
	}
	return pos
}

func TestBitSizes(t *testing.T) {
	for name, vals := range shapes() {
		bit := varintSize(vals)
		blocked := AppendBlocked(nil, vals)
		packed := AppendBitBlocked(nil, vals)
		per := func(n int) float64 { return float64(n) / float64(len(vals)) }
		t.Logf("%-11s bit-varint %.2f B/elem | byte-blocks %.2f (%+.1f%%) | bit-blocks %.2f (%+.1f%%)",
			name, per(bit),
			per(len(blocked)), 100*(float64(len(blocked))/float64(bit)-1),
			per(len(packed)), 100*(float64(len(packed))/float64(bit)-1))

		got := make([]int64, len(vals))
		DecodeBitBlocked(pad(packed), len(vals), got)
		for i := range vals {
			if got[i] != vals[i] {
				t.Fatalf("%s: element %d: got %d want %d", name, i, got[i], vals[i])
			}
		}
	}
}

// pad gives the unpacker the read-ahead it needs past the last block.
func pad(buf []byte) []byte {
	return append(append([]byte(nil), buf...), make([]byte, readAhead)...)
}

func BenchmarkBitBlocked(b *testing.B) {
	for name, vals := range shapes() {
		out := make([]int64, len(vals))
		b.Run(name+"/encode", func(b *testing.B) {
			buf := make([]byte, 0, 1<<14)
			for b.Loop() {
				buf = AppendBitBlocked(buf[:0], vals)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(vals)), "ns/elem")
		})
		b.Run(name+"/decode", func(b *testing.B) {
			buf := pad(AppendBitBlocked(nil, vals))
			for b.Loop() {
				DecodeBitBlocked(buf, len(vals), out)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(vals)), "ns/elem")
		})
	}
}
