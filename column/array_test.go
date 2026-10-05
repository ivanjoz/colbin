package column

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

// ------------------------------------------------------------ array codec

func transformOf(buf []byte) uint8 { return buf[0] & 0x03 }

// blockWidthsOf reports the bit width each block chose.
func blockWidthsOf(buf []byte, count int) []int {
	transform := transformOf(buf)
	if transform == trConstant {
		return nil
	}
	at := 1
	if transform != trRaw {
		at += 8
	}
	var widths []int
	for remaining := residualsUnder(transform, count); remaining > 0; {
		take := min(remaining, blockSize)
		w := int(buf[at])
		widths = append(widths, w)
		at += 1 + blockBytes(take, w)
		remaining -= take
	}
	return widths
}

// forced is AppendArray with the transform named by the caller rather than
// scored: the header, base and blocks the encoder writes when it picks that
// transform. It is only a valid encoding of a constant column for trConstant.
func forced(vals []int64, transform uint8) []byte {
	minimum := slices.Min(vals)
	zz := transform == trDelta || (transform == trRaw && minimum < 0)
	header := transform
	if zz {
		header |= zigzagFlag
	}
	out := []byte{header}
	switch transform {
	case trConstant:
		return binary.LittleEndian.AppendUint64(out, zigzag(minimum))
	case trFOR:
		out = binary.LittleEndian.AppendUint64(out, zigzag(minimum))
	case trDelta:
		out = binary.LittleEndian.AppendUint64(out, zigzag(vals[0]))
	}
	return appendBlocks(out, vals, transform, minimum, zz)
}

func roundtrip[T Signed](t *testing.T, vals []T) []byte {
	t.Helper()
	buf := AppendArray(nil, vals)
	out := make([]T, len(vals))
	n, err := DecodeArray(buf, len(vals), out)
	if err != nil {
		t.Fatalf("decode %v: %v", vals, err)
	}
	if n != len(buf) {
		t.Fatalf("decode consumed %d of %d bytes for %v", n, len(buf), vals)
	}
	for i := range vals {
		if out[i] != vals[i] {
			t.Fatalf("idx=%d: got %d want %d (input %v, transform %d, widths %v)",
				i, out[i], vals[i], vals, transformOf(buf), blockWidthsOf(buf, len(vals)))
		}
	}
	return buf
}

func TestArrayRoundtripTable(t *testing.T) {
	cases := []struct {
		name string
		vals []int64
	}{
		{"empty", nil},
		{"single zero", []int64{0}},
		{"single one", []int64{1}},
		{"single negative", []int64{-1}},
		{"all zeros", []int64{0, 0, 0, 0, 0, 0}},
		{"spec example", []int64{100, 230, 233, 500}},
		{"delta example", []int64{100, 240, 250, 380}},
		{"ascending", []int64{1, 2, 3, 4, 5, 6, 7, 8}},
		{"descending", []int64{8, 7, 6, 5, 4, 3, 2, 1}},
		{"negatives", []int64{-5, -3, -1, 0, 1, 3, 5}},
		{"all negative", []int64{-100, -200, -300, -400}},
		{"oscillating", []int64{0, 1000, 0, 1000, 0, 1000}},
		{"clustered high", []int64{1 << 40, 1<<40 + 3, 1<<40 + 1, 1<<40 + 9}},
		{"skewed", []int64{1, 2, 3, 1 << 62, 4, 5}},
		{"int64 extremes", []int64{math.MinInt64, math.MaxInt64}},
		{"extremes and zero", []int64{math.MinInt64, 0, math.MaxInt64, 0, math.MinInt64}},
		{"max span step", []int64{math.MinInt64, math.MaxInt64, math.MinInt64, math.MaxInt64}},
		{"boundary 2^7", []int64{126, 127, 128, 129}},
		{"boundary 2^14", []int64{16382, 16383, 16384, 16385}},
		{"boundary 2^21", []int64{2097150, 2097151, 2097152, 2097153}},
		{"powers of two", []int64{1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024}},
		{"repeated large", []int64{1 << 55, 1 << 55, 1 << 55, 1 << 55}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			roundtrip(t, tc.vals)
		})
	}
}

// typedSweep exercises one element type end to end: its extremes, values
// pinned to each boundary, and random arrays drawn from the full type range.
func typedSweep[T Signed](t *testing.T, lo, hi T, seed uint64) {
	t.Helper()
	roundtrip(t, []T{lo, hi, 0, lo, hi})
	roundtrip(t, []T{lo})
	roundtrip(t, []T{hi})
	roundtrip(t, []T{0})
	roundtrip(t, []T{lo, lo + 1, hi - 1, hi})

	r := rand.New(rand.NewPCG(seed, seed+1))
	span := uint64(int64(hi)) - uint64(int64(lo))
	for range 400 {
		n := int(r.UintN(40)) + 1
		vals := make([]T, n)
		for i := range vals {
			if span+1 == 0 { // full int64 range: no modulus available
				vals[i] = T(int64(r.Uint64()))
				continue
			}
			vals[i] = T(int64(uint64(int64(lo)) + r.Uint64N(span+1)))
		}
		roundtrip(t, vals)
	}
	// Narrow bands, where delta and FOR are the interesting transforms.
	for range 400 {
		n := int(r.UintN(40)) + 1
		base := T(int64(uint64(int64(lo)) + r.Uint64N(min(span, 1<<40)+1)))
		vals := make([]T, n)
		for i := range vals {
			step := T(r.Uint64N(16))
			if hi-base < step { // stay inside the type
				step = 0
			}
			vals[i] = base + step
		}
		roundtrip(t, vals)
	}
}

// Each element type end to end, across its whole range.
func TestArrayElementTypes(t *testing.T) {
	t.Run("int8", func(t *testing.T) { typedSweep[int8](t, math.MinInt8, math.MaxInt8, 1) })
	t.Run("int16", func(t *testing.T) { typedSweep[int16](t, math.MinInt16, math.MaxInt16, 2) })
	t.Run("int32", func(t *testing.T) { typedSweep[int32](t, math.MinInt32, math.MaxInt32, 3) })
	t.Run("int64", func(t *testing.T) { typedSweep[int64](t, math.MinInt64, math.MaxInt64, 4) })
}

// encodesAs checks that vals written as []T are want byte for byte, and that
// want decodes into []T as vals.
func encodesAs[T Signed](t *testing.T, vals []int64, want []byte) {
	t.Helper()
	typed := make([]T, len(vals))
	for i, v := range vals {
		typed[i] = T(v)
	}
	if got := AppendArray(nil, typed); !bytes.Equal(got, want) {
		t.Fatalf("as %T: % x\n as []int64: % x\n values %v", typed, got, want, vals)
	}
	out := make([]T, len(vals))
	if _, err := DecodeArray(want, len(vals), out); err != nil {
		t.Fatalf("into %T: %v (values %v)", out, err, vals)
	}
	for i := range vals {
		if int64(out[i]) != vals[i] {
			t.Fatalf("into %T: idx %d is %d, want %d", out, i, out[i], vals[i])
		}
	}
}

// A defined type over a Signed type is just another element type.
type recordID int32

// The encoding depends on the values alone: written as any element type that can
// hold them, the same values are the same bytes, and those bytes decode into any
// of those types.
func TestArrayEncodingIsIndependentOfType(t *testing.T) {
	r := rand.New(rand.NewPCG(77, 78))
	for range 3000 {
		n := int(r.UintN(300))
		// The narrowest type every value will fit, and values that use its range.
		bits := []uint{8, 16, 32}[r.UintN(3)]
		lo, span := -int64(1)<<(bits-1), uint64(1)<<bits
		vals := make([]int64, n)
		start := lo + int64(r.Uint64N(span))
		for i := range vals {
			switch r.UintN(3) {
			case 0: // anywhere in the range
				vals[i] = lo + int64(r.Uint64N(span))
			case 1: // a ramp, which delta wins on, clamped to the range
				vals[i] = min(start+int64(i), lo+int64(span)-1)
			default: // a cluster, which frame of reference wins on
				vals[i] = max(lo, start-int64(r.Uint64N(16)))
			}
		}
		want := AppendArray(nil, vals)
		encodesAs[int32](t, vals, want)
		encodesAs[recordID](t, vals, want)
		if bits <= 16 {
			encodesAs[int16](t, vals, want)
		}
		if bits <= 8 {
			encodesAs[int8](t, vals, want)
		}
	}
}

// Decoding into a type too narrow for a value is an error, never a truncation,
// whichever transform carried the value — and a value at the type's edge still
// decodes.
func TestArrayDecodeRejectsOutOfRange(t *testing.T) {
	for _, c := range []struct {
		name      string
		vals      []int64
		transform uint8
	}{
		{"raw", []int64{1, 2, 200}, trRaw},
		{"raw wraps to an in-range value", []int64{1, 2, 256}, trRaw},
		{"raw negative", []int64{-1, -200}, trRaw},
		{"FOR residual", []int64{100, 101, 300}, trFOR},
		{"FOR base", []int64{-200, -199}, trFOR},
		{"delta step", []int64{0, 100, 200}, trDelta},
		{"delta base", []int64{1000, 1001}, trDelta},
		{"constant", []int64{511, 511, 511}, trConstant},
	} {
		t.Run(c.name, func(t *testing.T) {
			buf := forced(c.vals, c.transform)
			wide := make([]int64, len(c.vals))
			if _, err := DecodeArray(buf, len(c.vals), wide); err != nil || !slices.Equal(wide, c.vals) {
				t.Fatalf("as int64: %v, %v", wide, err)
			}
			if _, err := DecodeArray(buf, len(c.vals), make([]int8, len(c.vals))); !errors.Is(err, errOutOfRange) {
				t.Errorf("into int8: got %v, want errOutOfRange", err)
			}
			if _, err := DecodeArray(buf, len(c.vals), make([]int16, len(c.vals))); err != nil {
				t.Errorf("into int16: %v", err)
			}
		})
	}
	for _, transform := range []uint8{trRaw, trFOR, trDelta} {
		edges := []int64{math.MinInt8, math.MaxInt8, 0, math.MinInt8}
		out := make([]int8, len(edges))
		if _, err := DecodeArray(forced(edges, transform), len(edges), out); err != nil {
			t.Errorf("transform %d, int8 edges: %v", transform, err)
		}
	}
	// The same check at every width, through the encoder.
	for _, c := range []struct {
		value int64
		into  func(buf []byte) error
	}{
		{1 << 7, func(buf []byte) error { _, err := DecodeArray(buf, 3, make([]int8, 3)); return err }},
		{1 << 15, func(buf []byte) error { _, err := DecodeArray(buf, 3, make([]int16, 3)); return err }},
		{1 << 31, func(buf []byte) error { _, err := DecodeArray(buf, 3, make([]int32, 3)); return err }},
		{-1<<31 - 1, func(buf []byte) error { _, err := DecodeArray(buf, 3, make([]int32, 3)); return err }},
	} {
		if err := c.into(AppendArray(nil, []int64{0, 1, c.value})); !errors.Is(err, errOutOfRange) {
			t.Errorf("%d: got %v, want errOutOfRange", c.value, err)
		}
	}
}

// The decoder accepts exactly the headers the encoder writes: raw with or
// without zigzag, delta with it, frame of reference and constant without it, and
// for an empty column the bare raw header alone.
func TestArrayDecodeRejectsUnwrittenHeaders(t *testing.T) {
	written := map[uint8]bool{
		trRaw: true, trRaw | zigzagFlag: true, trDelta | zigzagFlag: true, trFOR: true, trConstant: true,
	}
	for header := range 256 {
		// Zeros after the header are a valid base and a width-0 block, so the
		// header is the only thing that can be wrong.
		buf := append([]byte{uint8(header)}, make([]byte, 16)...)
		for _, n := range []int{0, 1, 4} {
			_, err := DecodeArray(buf, n, make([]int64, n))
			ok := written[uint8(header)] && (n > 0 || header == int(trRaw))
			if ok && err != nil {
				t.Errorf("header %#02x, n=%d: %v", header, n, err)
			}
			if !ok && !errors.Is(err, errBadHeader) {
				t.Errorf("header %#02x, n=%d: got %v, want errBadHeader", header, n, err)
			}
		}
	}
}

// Incompressible input costs the element width plus framing, and the framing is
// one header byte and one width byte per 128 values — no more.
func TestArrayNeverExceedsTheElementWidth(t *testing.T) {
	r := rand.New(rand.NewPCG(99, 100))
	for range 2000 {
		n := int(r.UintN(400)) + 1
		vals := make([]int64, n)
		for i := range vals {
			vals[i] = int64(r.Uint64())
		}
		buf := roundtrip(t, vals)
		blocks := (n + blockSize - 1) / blockSize
		if want := 1 + blocks + 8*n; len(buf) > want {
			t.Fatalf("n=%d: encoded %d bytes, the raw words plus framing are %d", n, len(buf), want)
		}
	}
}

// The encoder scores each transform by its cost model; this holds the choice
// against the real thing. Each transform the encoder could have picked is
// written out in full, checked to decode to the same values, and must not be
// shorter than what the encoder chose.
func TestArrayTransformChoiceIsOptimal(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	for iter := range 3000 {
		n := int(r.UintN(300)) + 1
		vals := make([]int64, n)
		shift := r.UintN(62)
		base := int64(r.Uint64())
		for i := range vals {
			switch iter % 5 {
			case 0: // small non-negative
				vals[i] = int64(r.Uint64N(1 << (shift + 1)))
			case 1: // with negatives
				vals[i] = int64(r.Uint64N(1<<(shift+1))) - 1<<shift
			case 2: // a noisy ramp from anywhere
				vals[i] = base + int64(i)*int64(shift) + int64(r.Uint64N(8))
			case 3: // constant, short or long
				vals[i] = base >> shift
			default: // anything at all, deltas that overflow included
				vals[i] = int64(r.Uint64())
			}
		}
		chosen := AppendArray(nil, vals)

		candidates := []uint8{trRaw, trFOR}
		overflows := false
		for i := 1; i < n; i++ {
			overflows = overflows || subOverflows(vals[i], vals[i-1])
		}
		if n > 1 && !overflows {
			candidates = append(candidates, trDelta)
		}
		if slices.Min(vals) == slices.Max(vals) {
			candidates = append(candidates, trConstant)
		}
		for _, transform := range candidates {
			alt := forced(vals, transform)
			out := make([]int64, n)
			if used, err := DecodeArray(alt, n, out); err != nil || used != len(alt) || !slices.Equal(out, vals) {
				t.Fatalf("n=%d: the forced transform %d does not decode: %v", n, transform, err)
			}
			if len(alt) < len(chosen) {
				t.Fatalf("n=%d: chose transform %d at %dB, but transform %d is %dB",
					n, transformOf(chosen), len(chosen), transform, len(alt))
			}
		}
	}
}

// Each of the four transforms must be selectable, otherwise a header code is
// dead weight.
func TestArrayAllTransformsSelected(t *testing.T) {
	corpus := [][]int64{
		{1, 2, 3, 4}, // raw
		{1000000, 1000001, 1000002, 1000003, 1000004, 1000005}, // delta
		{0, 1000, 0, 1000, 0, 1000, 0, 1000},                   // FOR
		{7, 7, 7, 7, 7, 7},                                     // constant
	}
	seen := map[uint8]bool{}
	for _, vals := range corpus {
		buf := roundtrip(t, vals)
		seen[transformOf(buf)] = true
	}
	r := rand.New(rand.NewPCG(1, 2))
	for iter := 0; iter < 4000 && len(seen) < 4; iter++ {
		n := int(r.UintN(20)) + 1
		vals := make([]int64, n)
		for i := range vals {
			switch r.UintN(4) {
			case 0:
				vals[i] = int64(r.Uint64N(100))
			case 1:
				vals[i] = int64(r.Uint64())
			case 2:
				vals[i] = int64(1<<40) + int64(i)
			default:
				vals[i] = -int64(r.Uint64N(1 << 30))
			}
		}
		seen[transformOf(AppendArray(nil, vals))] = true
	}
	for _, tr := range []uint8{trRaw, trDelta, trFOR, trConstant} {
		if !seen[tr] {
			t.Errorf("transform %d never selected", tr)
		}
	}
}

func TestArrayRandomShapesRoundtrip(t *testing.T) {
	r := rand.New(rand.NewPCG(2024, 8))
	shapes := []func(i, n int) int64{
		func(i, n int) int64 { return int64(r.Uint64N(128)) },
		func(i, n int) int64 { return int64(r.Uint64()) },
		func(i, n int) int64 { return int64(i) * 1000 },
		func(i, n int) int64 { return -int64(i) * 1000 },
		func(i, n int) int64 { return int64(1<<40) + int64(r.Uint64N(256)) },
		func(i, n int) int64 { return int64(r.Uint64N(1<<20)) - (1 << 19) },
		func(i, n int) int64 { return 0 },
		func(i, n int) int64 {
			if i%7 == 0 {
				return int64(r.Uint64())
			}
			return int64(r.Uint64N(64))
		},
		func(i, n int) int64 { return math.MaxInt64 - int64(i) },
		func(i, n int) int64 { return math.MinInt64 + int64(i) },
	}
	for _, shape := range shapes {
		for range 400 {
			n := int(r.UintN(80))
			vals := make([]int64, n)
			for i := range vals {
				vals[i] = shape(i, n)
			}
			roundtrip(t, vals)
		}
	}
}

func TestArrayTruncated(t *testing.T) {
	vals := []int64{100, 240, 250, 380, -7, 1 << 40, 0, 99999}
	buf := AppendArray(nil, vals)
	out := make([]int64, len(vals))
	for cut := range buf {
		if _, err := DecodeArray(buf[:cut], len(vals), out); !errors.Is(err, errTruncated) {
			t.Fatalf("truncation to %d/%d bytes: got %v, want errTruncated", cut, len(buf), err)
		}
	}
	if _, err := DecodeArray(buf, len(vals), out[:len(vals)-1]); !errors.Is(err, errShortBuffer) {
		t.Fatalf("short output slice: got %v", err)
	}
	if _, err := DecodeArray(buf, -1, out); !errors.Is(err, errNegativeCount) {
		t.Fatalf("negative count: got %v", err)
	}
	wide := bytes.Clone(buf)
	firstWidth := 1
	if transformOf(buf) != trRaw {
		firstWidth += 8
	}
	wide[firstWidth] = 65
	if _, err := DecodeArray(wide, len(vals), out); !errors.Is(err, errBadWidth) {
		t.Fatalf("width 65: got %v", err)
	}
}

// Arbitrary bytes must never panic the decoder.
func TestArrayDecodeGarbage(t *testing.T) {
	r := rand.New(rand.NewPCG(31337, 4))
	out := make([]int64, 64)
	for range 20000 {
		buf := make([]byte, r.UintN(40))
		for i := range buf {
			buf[i] = byte(r.UintN(256))
		}
		n := int(r.UintN(65))
		_, _ = DecodeArray(buf, n, out) // must not panic
	}
}

func TestArrayEmptyAndSingle(t *testing.T) {
	// A nil slice carries no type to infer from, so name it explicitly.
	buf := AppendArray[int64](nil, nil)
	if len(buf) != 1 {
		t.Fatalf("empty array encoded to %d bytes, want 1", len(buf))
	}
	n, err := DecodeArray[int64](buf, 0, nil)
	if err != nil || n != 1 {
		t.Fatalf("empty decode: n=%d err=%v", n, err)
	}
	// The empty encoding is the same one header byte for every type.
	for _, got := range [][]byte{
		AppendArray[int8](nil, nil),
		AppendArray[int16](nil, nil),
		AppendArray[int32](nil, nil),
	} {
		if !bytes.Equal(got, buf) {
			t.Fatalf("empty array encoded to % x, want % x", got, buf)
		}
	}
}

// Delta and frame-of-reference write an eight-byte base in the clear, which is
// nothing amortised over a real column and dominant over a short one. The
// encoder has to see that: four values are cheaper raw.
func TestABaseHasToEarnItsEightBytes(t *testing.T) {
	short := []int64{100, 240, 250, 380}
	buf := roundtrip(t, short)
	if got := transformOf(buf); got != trRaw {
		t.Fatalf("four values chose transform %d, want raw: delta's base costs more than it saves", got)
	}
	// 380 is nine bits, so four values are five bytes behind two of framing.
	if len(buf) != 1+1+5 {
		t.Fatalf("encoded %d bytes, want 7: % x", len(buf), buf)
	}

	// The same shape, long enough to amortise the base, goes to delta.
	long := make([]int64, 1000)
	for index := range long {
		long[index] = 1<<40 + int64(index)*130
	}
	buf = roundtrip(t, long)
	if got := transformOf(buf); got != trDelta {
		t.Fatalf("a thousand monotonic values chose transform %d, want delta", got)
	}
	// Every step is 130, so zigzag makes every residual 260 and every block nine
	// bits wide — against the forty-one the values themselves need.
	for index, width := range blockWidthsOf(buf, len(long)) {
		if width != 9 {
			t.Fatalf("block %d has width %d, want 9: %v",
				index, width, blockWidthsOf(buf, len(long)))
		}
	}
}

// A column whose values are all the same carries the value and no blocks, which
// is both the smallest and the fastest thing it could do.
func TestAConstantColumnIsNineBytes(t *testing.T) {
	for _, value := range []int64{0, 1, -1, 1 << 40, math.MinInt64} {
		vals := make([]int64, 5000)
		for index := range vals {
			vals[index] = value
		}
		buf := roundtrip(t, vals)
		if got := transformOf(buf); got != trConstant {
			t.Fatalf("%d repeated chose transform %d, want constant", value, got)
		}
		if len(buf) != 9 {
			t.Fatalf("%d repeated encoded to %d bytes, want 9", value, len(buf))
		}
	}
}

// A block of nothing but zeros carries no bytes at all, which is what makes a
// sparse column nearly free.
func TestAZeroBlockIsOneByte(t *testing.T) {
	vals := make([]int64, blockSize*4)
	vals[0] = 1 // not constant, so it goes through the blocks
	buf := roundtrip(t, vals)
	widths := blockWidthsOf(buf, len(vals))
	if len(widths) != 4 {
		t.Fatalf("got %d blocks, want 4", len(widths))
	}
	for index, width := range widths[1:] {
		if width != 0 {
			t.Fatalf("block %d of zeros has width %d, want 0", index+1, width)
		}
	}
	// One header, one width byte per block, and two bytes for the first block's
	// 128 one-bit residuals.
	if want := 1 + 4 + blockBytes(blockSize, 1); len(buf) != want {
		t.Fatalf("encoded %d bytes, want %d", len(buf), want)
	}
}

func FuzzArrayRoundtrip(f *testing.F) {
	f.Add([]byte{100, 240, 250, 124}, uint8(8))
	f.Add([]byte{}, uint8(8))
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 1}, uint8(4))
	f.Add([]byte{0xFF, 0x7F, 0x80, 0x00, 0x01}, uint8(2))
	f.Fuzz(func(t *testing.T, raw []byte, sel uint8) {
		// Reinterpret the fuzz bytes as elements of one concrete type.
		switch sel % 4 {
		case 0:
			vals := make([]int8, len(raw))
			for i, b := range raw {
				vals[i] = int8(b)
			}
			fuzzRoundtrip(t, vals, 1)
		case 1:
			vals := make([]int16, len(raw)/2)
			for i := range vals {
				vals[i] = int16(uint16(raw[i*2]) | uint16(raw[i*2+1])<<8)
			}
			fuzzRoundtrip(t, vals, 2)
		case 2:
			vals := make([]int32, len(raw)/4)
			for i := range vals {
				var u uint32
				for b := range 4 {
					u |= uint32(raw[i*4+b]) << (8 * b)
				}
				vals[i] = int32(u)
			}
			fuzzRoundtrip(t, vals, 4)
		default:
			vals := make([]int64, len(raw)/8)
			for i := range vals {
				var u uint64
				for b := range 8 {
					u |= uint64(raw[i*8+b]) << (8 * b)
				}
				vals[i] = int64(u)
			}
			fuzzRoundtrip(t, vals, 8)
		}
	})
}

// fuzzRoundtrip round-trips vals and holds the encoding to the size bound: no
// more than width bytes per value plus a header and a width byte per block.
func fuzzRoundtrip[T Signed](t *testing.T, vals []T, width int) {
	t.Helper()
	buf := AppendArray(nil, vals)
	blocks := (len(vals) + blockSize - 1) / blockSize
	if bound := 1 + blocks + len(vals)*width; len(buf) > bound {
		t.Fatalf("encoded %dB exceeds the raw words plus framing, %dB", len(buf), bound)
	}
	out := make([]T, len(vals))
	got, err := DecodeArray(buf, len(vals), out)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != len(buf) {
		t.Fatalf("consumed %d of %d", got, len(buf))
	}
	for i := range vals {
		if out[i] != vals[i] {
			t.Fatalf("idx %d: got %d want %d", i, out[i], vals[i])
		}
	}
}

func FuzzArrayDecode(f *testing.F) {
	f.Add([]byte{0x00}, uint8(4))
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF}, uint8(10))
	f.Add(AppendArray(nil, []int64{511, 511, 511}), uint8(3))
	f.Fuzz(func(t *testing.T, buf []byte, n uint8) {
		// Arbitrary bytes must not panic any element type, and every narrower
		// type must agree with int64: the same values when they fit, an error
		// when they do not.
		wide := make([]int64, n)
		used, err := DecodeArray(buf, int(n), wide)
		agreesWithInt64[int8](t, buf, wide, used, err)
		agreesWithInt64[int16](t, buf, wide, used, err)
		agreesWithInt64[int32](t, buf, wide, used, err)
	})
}

// agreesWithInt64 decodes buf as []T and checks it against the int64 decode of
// the same bytes.
func agreesWithInt64[T Signed](t *testing.T, buf []byte, wide []int64, used int, wideErr error) {
	t.Helper()
	out := make([]T, len(wide))
	got, err := DecodeArray(buf, len(wide), out)
	if wideErr != nil {
		if err == nil {
			t.Fatalf("%T decoded what int64 refused with %v", out, wideErr)
		}
		return
	}
	for _, v := range wide {
		if !fits[T](v) {
			if !errors.Is(err, errOutOfRange) {
				t.Fatalf("%T took %d: got %v, want errOutOfRange", out, v, err)
			}
			return
		}
	}
	if err != nil || got != used {
		t.Fatalf("%T: %v, consumed %d where int64 consumed %d", out, err, got, used)
	}
	for i := range wide {
		if int64(out[i]) != wide[i] {
			t.Fatalf("%T: idx %d is %d, int64 says %d", out, i, out[i], wide[i])
		}
	}
}

// mkSeq builds a clustered ramp that fits every element width.
func mkSeq[T Signed](n int, base int64) []T {
	v := make([]T, n)
	for i := range v {
		v[i] = T(base + int64(i*7919%4096))
	}
	return v
}

func benchEncode[T Signed](b *testing.B, vals []T) {
	buf := make([]byte, 0, 1<<14)
	b.SetBytes(int64(binary.Size(vals)))
	for b.Loop() {
		buf = AppendArray(buf[:0], vals)
	}
}

func benchDecode[T Signed](b *testing.B, vals []T) {
	buf := AppendArray(nil, vals)
	out := make([]T, len(vals))
	b.SetBytes(int64(binary.Size(vals)))
	for b.Loop() {
		if _, err := DecodeArray(buf, len(vals), out); err != nil {
			b.Fatal(err)
		}
	}
}

// Short columns, which is what an array field inside a record actually holds
// and where every block is the partial one the tail path handles.
func BenchmarkArrayEncodeShort(b *testing.B) { benchEncode(b, mkSeq[int32](16, 1000)) }
func BenchmarkArrayDecodeShort(b *testing.B) { benchDecode(b, mkSeq[int32](16, 1000)) }

func BenchmarkArrayEncodeInt16(b *testing.B) { benchEncode(b, mkSeq[int16](1024, 1000)) }
func BenchmarkArrayEncodeInt32(b *testing.B) { benchEncode(b, mkSeq[int32](1024, 1<<20)) }
func BenchmarkArrayEncodeInt64(b *testing.B) { benchEncode(b, mkSeq[int64](1024, 1<<30)) }
func BenchmarkArrayDecodeInt16(b *testing.B) { benchDecode(b, mkSeq[int16](1024, 1000)) }
func BenchmarkArrayDecodeInt32(b *testing.B) { benchDecode(b, mkSeq[int32](1024, 1<<20)) }
func BenchmarkArrayDecodeInt64(b *testing.B) { benchDecode(b, mkSeq[int64](1024, 1<<30)) }

// TestArraySizeReport pins the transform each representative shape selects;
// with -v it prints the sizes column/README.md quotes.
func TestArraySizeReport(t *testing.T) {
	mk := func(n int, f func(i int) int64) []int64 {
		v := make([]int64, n)
		for i := range v {
			v[i] = f(i)
		}
		return v
	}
	names := [4]string{"raw", "delta", "FOR", "constant"}
	rr := rand.New(rand.NewPCG(17, 23))
	cases := []struct {
		name string
		vals []int64
		want uint8
	}{
		{"tiny values 0..99", mk(256, func(i int) int64 { return int64(i * 7 % 100) }), trRaw},
		{"monotonic ids", mk(256, func(i int) int64 { return 1<<40 + int64(i)*13 }), trDelta},
		{"timestamps (sec)", mk(256, func(i int) int64 { return 1700000000 + int64(i)*60 }), trDelta},
		{"clustered +-500", mk(256, func(i int) int64 { return 1<<30 + int64(i*7919%1000) - 500 }), trFOR},
		{"steps of 200", mk(256, func(i int) int64 { return int64(i) * 200 }), trDelta},
		{"random int64", mk(256, func(i int) int64 { return int64(rr.Uint64()) }), trRaw},
		// Not constant: a width-0 block carries no bytes at all, so raw beats the
		// constant transform's eight-byte value outright.
		{"all zeros", mk(256, func(i int) int64 { return 0 }), trRaw},
		{"negatives", mk(256, func(i int) int64 { return -int64(i) * 3 }), trDelta},
	}
	for _, tc := range cases {
		buf := roundtrip(t, tc.vals)
		raw := len(tc.vals) * 8
		got := transformOf(buf)
		t.Logf("%-20s %5dB -> %5dB (%4.1f%%)  transform=%-8s widths=%v",
			tc.name, raw, len(buf), 100*float64(len(buf))/float64(raw),
			names[got], blockWidthsOf(buf, len(tc.vals)))
		if got != tc.want {
			t.Errorf("%s: transform = %s, want %s", tc.name, names[got], names[tc.want])
		}
	}
}
