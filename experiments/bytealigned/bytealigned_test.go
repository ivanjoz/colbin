package bytealigned

// A scratch comparison: colbin's current bit-level (k,M) varint column against a
// byte-aligned column that keeps the same transforms (raw / delta / frame of
// reference) but writes every residual at one fixed byte width.
//
// The point is to price byte alignment: how many bytes it costs, and how much
// encode and decode time it buys back.

import (
	"encoding/binary"
	"math/bits"
	"math/rand/v2"
	"testing"
	"unsafe"

	"github.com/ivanjoz/colbin/column"
)

const (
	trRaw   = 0
	trDelta = 1
	trFOR   = 2
)

// widthFor rounds a bit length up to a byte width the loops below can store.
func widthFor(nbits int) int {
	switch {
	case nbits <= 8:
		return 1
	case nbits <= 16:
		return 2
	case nbits <= 24:
		return 3
	case nbits <= 32:
		return 4
	case nbits <= 48:
		return 6
	default:
		return 8
	}
}

func zigzag(v int64) uint64 { return uint64(v<<1) ^ uint64(v>>63) }
func unzig(u uint64) int64  { return int64(u>>1) ^ -int64(u&1) }
func putLE(out []byte, v uint64, width int) []byte {
	for range width {
		out = append(out, byte(v))
		v >>= 8
	}
	return out
}

// AppendFixed writes a column at one byte width, picking the transform with one
// pass over the data rather than a scored search over four of them.
func AppendFixed(out []byte, vals []int64) []byte {
	if len(vals) == 0 {
		return append(out, 0)
	}
	minVal, maxVal := vals[0], vals[0]
	var maxDelta uint64
	prev := int64(0)
	for i, v := range vals {
		if v < minVal {
			minVal = v
		}
		if v > maxVal {
			maxVal = v
		}
		if i > 0 {
			if d := zigzag(v - prev); d > maxDelta {
				maxDelta = d
			}
		}
		prev = v
	}

	rawBits := max(bits.Len64(zigzag(minVal)), bits.Len64(zigzag(maxVal)))
	forBits := bits.Len64(uint64(maxVal) - uint64(minVal))
	// The first delta is against the base written in the clear, not against
	// zero: otherwise one huge leading residual sets the width for the column.
	deltaBits := bits.Len64(maxDelta)

	transform, width := trRaw, widthFor(rawBits)
	if w := widthFor(forBits); w < width {
		transform, width = trFOR, w
	}
	if w := widthFor(deltaBits); w < width {
		transform, width = trDelta, w
	}

	out = append(out, byte(transform)|byte(width)<<3)
	switch transform {
	case trFOR:
		out = putLE(out, zigzag(minVal), 8)
		for _, v := range vals {
			out = putLE(out, uint64(v)-uint64(minVal), width)
		}
	case trDelta:
		out = putLE(out, zigzag(vals[0]), 8)
		prev := vals[0]
		for _, v := range vals[1:] {
			out = putLE(out, zigzag(v-prev), width)
			prev = v
		}
	default:
		for _, v := range vals {
			out = putLE(out, zigzag(v), width)
		}
	}
	return out
}

// blockSize is how many residuals share one width byte. 128 puts the width
// header at 0.8% overhead while letting a single outlier widen its own block
// instead of the whole column.
const blockSize = 128

// residualsUnder is how many residuals a transform emits: delta writes its first
// value in the clear as a base, so it has one residual fewer than it has values.
func residualsUnder(vals []int64, transform int) int {
	if transform == trDelta {
		return len(vals) - 1
	}
	return len(vals)
}

// blockedCost is the payload size a transform would produce, block widths and
// all. One pass over the data per candidate, with the residual inlined into the
// loop rather than reached through residualOf — three tight scans, no branches
// on the transform inside them.
func blockedCost(vals []int64, transform int, base int64) int {
	n := residualsUnder(vals, transform)
	total := 0
	if transform != trRaw {
		total += 8 // the base, in the clear
	}
	for start := 0; start < n; start += blockSize {
		end := min(start+blockSize, n)
		var widest uint64
		switch transform {
		case trFOR:
			for _, v := range vals[start:end] {
				widest |= uint64(v) - uint64(base)
			}
		case trDelta:
			for i := start; i < end; i++ {
				widest |= zigzag(vals[i+1] - vals[i])
			}
		default:
			for _, v := range vals[start:end] {
				widest |= zigzag(v)
			}
		}
		// OR-ing the residuals together and taking the bit length of the result
		// is the same answer as the running max, without the compare.
		width := widthFor(bits.Len64(widest))
		if widest == 0 {
			width = 0
		}
		total += 1 + (end-start)*width
	}
	return total
}

// AppendBlocked is AppendFixed with a per-block width: the transform is chosen
// once for the column, the width once per 128 residuals.
func AppendBlocked(out []byte, vals []int64) []byte {
	if len(vals) == 0 {
		return append(out, 0)
	}
	// Only the minimum is needed up front, as the frame of reference; the widths
	// are decided per block, below.
	minVal := vals[0]
	for _, v := range vals {
		if v < minVal {
			minVal = v
		}
	}

	// With per-block widths the transform has to be scored against the blocked
	// cost, not against the column's widest residual: delta can lose on its
	// single worst element and still win on 127 blocks out of 128.
	transform := trRaw
	bestCost := blockedCost(vals, trRaw, minVal)
	if c := blockedCost(vals, trFOR, minVal); c < bestCost {
		transform, bestCost = trFOR, c
	}
	if c := blockedCost(vals, trDelta, minVal); c < bestCost {
		transform = trDelta
	}

	// The residuals, materialised so each block can be measured before it is
	// written. A real encoder would take this scratch from a pool.
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
		var bitsSet uint64
		for _, r := range block {
			bitsSet |= r
		}
		width := widthFor(bits.Len64(bitsSet))
		if bitsSet == 0 {
			width = 0 // a block of nothing but zeros carries no bytes at all
		}
		out = append(out, byte(width))
		out = storeRun(out, block, width)
	}
	return out
}

var scratch = make([]uint64, 0, 4096)

// storeRun is loadRun's mirror: the width leaves the element loop, so each
// element is one store.
func storeRun(out []byte, block []uint64, width int) []byte {
	switch width {
	case 0:
		return out
	case 1:
		for _, r := range block {
			out = append(out, byte(r))
		}
	case 2:
		for _, r := range block {
			out = append(out, byte(r), byte(r>>8))
		}
	case 4:
		for _, r := range block {
			out = append(out, byte(r), byte(r>>8), byte(r>>16), byte(r>>24))
		}
	case 8:
		for _, r := range block {
			out = append(out, byte(r), byte(r>>8), byte(r>>16), byte(r>>24),
				byte(r>>32), byte(r>>40), byte(r>>48), byte(r>>56))
		}
	default:
		for _, r := range block {
			out = putLE(out, r, width)
		}
	}
	return out
}

func DecodeBlocked(buf []byte, n int, out []int64) int {
	transform := int(buf[0])
	pos := 1
	var base int64
	count := n
	target := out
	switch transform {
	case trFOR:
		base = unzig(getLE(buf[pos:], 8))
		pos += 8
	case trDelta:
		base = unzig(getLE(buf[pos:], 8))
		pos += 8
		out[0] = base
		target, count = out[1:], n-1
	}
	for start := 0; start < count; start += blockSize {
		block := target[start:min(start+blockSize, count)]
		width := int(buf[pos])
		pos++
		for i := range block {
			block[i] = int64(getLE(buf[pos:], width))
			pos += width
		}
	}
	switch transform {
	case trFOR:
		for i := range out[:n] {
			out[i] = int64(uint64(base) + uint64(out[i]))
		}
	case trDelta:
		acc := base
		for i := 1; i < n; i++ {
			acc += unzig(uint64(out[i]))
			out[i] = acc
		}
	default:
		for i := range out[:n] {
			out[i] = unzig(uint64(out[i]))
		}
	}
	return pos
}

func getLE(buf []byte, width int) uint64 {
	var v uint64
	for i := range width {
		v |= uint64(buf[i]) << (8 * i)
	}
	return v
}

func DecodeFixed(buf []byte, n int, out []int64) (int, error) {
	hdr := buf[0]
	pos := 1
	transform := int(hdr & 0x07)
	width := int(hdr >> 3)
	switch transform {
	case trFOR:
		base := unzig(getLE(buf[pos:], 8))
		pos += 8
		for i := range n {
			out[i] = int64(uint64(base) + getLE(buf[pos:], width))
			pos += width
		}
	case trDelta:
		acc := unzig(getLE(buf[pos:], 8))
		pos += 8
		out[0] = acc
		for i := 1; i < n; i++ {
			acc += unzig(getLE(buf[pos:], width))
			pos += width
			out[i] = acc
		}
	default:
		for i := range n {
			out[i] = unzig(getLE(buf[pos:], width))
			pos += width
		}
	}
	return pos, nil
}

// loadRun is the byte-aligned decode the format is actually for: the width is
// hoisted out of the element loop, so each element is one load and one store and
// the loop is a candidate for the vectoriser. This is the difference between
// "byte aligned" and "fast" — a width-generic inner loop throws the win away.
func loadRun(buf []byte, width int, dst []uint64) {
	switch width {
	case 0:
		clear(dst)
	case 1:
		for i := range dst {
			dst[i] = uint64(buf[i])
		}
	case 2:
		for i := range dst {
			dst[i] = uint64(binary.LittleEndian.Uint16(buf[i*2:]))
		}
	case 4:
		for i := range dst {
			dst[i] = uint64(binary.LittleEndian.Uint32(buf[i*4:]))
		}
	case 8:
		for i := range dst {
			dst[i] = binary.LittleEndian.Uint64(buf[i*8:])
		}
	default: // 3 and 6, the widths no load instruction has
		for i := range dst {
			dst[i] = getLE(buf[i*width:], width)
		}
	}
}

// DecodeFixedSpec decodes a one-width column through loadRun, then applies the
// transform in a second pass over the already-loaded values.
func DecodeFixedSpec(buf []byte, n int, out []int64) int {
	hdr := buf[0]
	pos := 1
	transform := int(hdr & 0x07)
	width := int(hdr >> 3)
	var base int64
	dst := out
	if transform != trRaw {
		base = unzig(getLE(buf[pos:], 8))
		pos += 8
		if transform == trDelta {
			out[0] = base
			dst = out[1:]
		}
	}
	raw := unsafe.Slice((*uint64)(unsafe.Pointer(&dst[0])), len(dst))
	loadRun(buf[pos:], width, raw)
	pos += len(dst) * width

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

// DecodeBlockedSpec is DecodeBlocked with the same hoist: one loadRun per block.
func DecodeBlockedSpec(buf []byte, n int, out []int64) int {
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
	raw := unsafe.Slice((*uint64)(unsafe.Pointer(&dst[0])), len(dst))
	for start := 0; start < len(raw); start += blockSize {
		block := raw[start:min(start+blockSize, len(raw))]
		width := int(buf[pos])
		pos++
		loadRun(buf[pos:], width, block)
		pos += len(block) * width
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

// The data shapes a column codec actually meets.
func shapes() map[string][]int64 {
	r := rand.New(rand.NewPCG(1, 2))
	n := 1024
	out := map[string][]int64{}

	ids := make([]int64, n) // small dense ids
	for i := range ids {
		ids[i] = int64(i%900 + 1)
	}
	out["smallIDs"] = ids

	seq := make([]int64, n) // the repo's own bench shape: wide base, 4096 span
	for i := range seq {
		seq[i] = 1<<20 + int64(i*7919%4096)
	}
	out["seq"] = seq

	ts := make([]int64, n) // unix millis, monotonic, jittered step
	t := int64(1757500000000)
	for i := range ts {
		t += 30 + r.Int64N(200)
		ts[i] = t
	}
	out["timestamps"] = ts

	prices := make([]int64, n) // cents, 0..50000, no structure
	for i := range prices {
		prices[i] = r.Int64N(50000)
	}
	out["prices"] = prices

	wide := make([]int64, n) // full-width random: nothing compresses
	for i := range wide {
		wide[i] = int64(r.Uint64())
	}
	out["random64"] = wide

	return out
}

func TestSizes(t *testing.T) {
	for name, vals := range shapes() {
		bit := column.AppendArray(nil, vals)
		fixed := AppendFixed(nil, vals)
		blocked := AppendBlocked(nil, vals)
		per := func(b []byte) float64 { return float64(len(b)) / float64(len(vals)) }
		over := func(b []byte) float64 { return 100 * (float64(len(b))/float64(len(bit)) - 1) }
		t.Logf("%-11s bit-varint %.2f B/elem | one width %.2f (%+.1f%%) | 128-blocks %.2f (%+.1f%%)",
			name, per(bit), per(fixed), over(fixed), per(blocked), over(blocked))

		// round-trip checks, so the sizes above are for something that decodes
		got := make([]int64, len(vals))
		if _, err := DecodeFixed(fixed, len(vals), got); err != nil {
			t.Fatal(err)
		}
		for i := range vals {
			if got[i] != vals[i] {
				t.Fatalf("%s one-width: element %d: got %d want %d", name, i, got[i], vals[i])
			}
		}
		for label, decode := range map[string]func([]byte, int, []int64) int{
			"blocked":         DecodeBlocked,
			"blocked-hoisted": DecodeBlockedSpec,
		} {
			clear(got)
			decode(blocked, len(vals), got)
			for i := range vals {
				if got[i] != vals[i] {
					t.Fatalf("%s %s: element %d: got %d want %d", name, label, i, got[i], vals[i])
				}
			}
		}
		clear(got)
		DecodeFixedSpec(fixed, len(vals), got)
		for i := range vals {
			if got[i] != vals[i] {
				t.Fatalf("%s width-hoisted: element %d: got %d want %d", name, i, got[i], vals[i])
			}
		}
	}
}

func BenchmarkEncode(b *testing.B) {
	for name, vals := range shapes() {
		b.Run(name+"/bit-varint", func(b *testing.B) {
			buf := make([]byte, 0, 1<<14)
			for b.Loop() {
				buf = column.AppendArray(buf[:0], vals)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(vals)), "ns/elem")
		})
		b.Run(name+"/byte-aligned", func(b *testing.B) {
			buf := make([]byte, 0, 1<<14)
			for b.Loop() {
				buf = AppendFixed(buf[:0], vals)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(vals)), "ns/elem")
		})
		b.Run(name+"/blocked", func(b *testing.B) {
			buf := make([]byte, 0, 1<<14)
			for b.Loop() {
				buf = AppendBlocked(buf[:0], vals)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(vals)), "ns/elem")
		})
	}
}

func BenchmarkDecode(b *testing.B) {
	for name, vals := range shapes() {
		out := make([]int64, len(vals))
		b.Run(name+"/bit-varint", func(b *testing.B) {
			buf := column.AppendArray(nil, vals)
			for b.Loop() {
				if _, err := column.DecodeArray(buf, len(vals), out); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(vals)), "ns/elem")
		})
		b.Run(name+"/byte-aligned", func(b *testing.B) {
			buf := AppendFixed(nil, vals)
			for b.Loop() {
				if _, err := DecodeFixed(buf, len(vals), out); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(vals)), "ns/elem")
		})
		b.Run(name+"/blocked", func(b *testing.B) {
			buf := AppendBlocked(nil, vals)
			for b.Loop() {
				DecodeBlocked(buf, len(vals), out)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(vals)), "ns/elem")
		})
	}
	for name, vals := range shapes() {
		out := make([]int64, len(vals))
		b.Run(name+"/width-hoisted", func(b *testing.B) {
			buf := AppendFixed(nil, vals)
			for b.Loop() {
				DecodeFixedSpec(buf, len(vals), out)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(vals)), "ns/elem")
		})
		b.Run(name+"/blocked-hoisted", func(b *testing.B) {
			buf := AppendBlocked(nil, vals)
			for b.Loop() {
				DecodeBlockedSpec(buf, len(vals), out)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(vals)), "ns/elem")
		})
	}
}
