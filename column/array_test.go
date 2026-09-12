package column

import (
	"math"
	"math/rand/v2"
	"testing"
)

// ------------------------------------------------------------ array codec

func transformOf(buf []byte) uint8 { return buf[0] & 0x03 }

// blockWidthsOf reports the bit width each block chose, which is what the
// header used to carry as (k, M) and now lives one byte per 128 residuals.
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

func roundtrip[T Signed](t *testing.T, vals []T) []byte {
	t.Helper()
	w := widthOfType[T]()
	buf := AppendArray(nil, vals)
	out := make([]T, len(vals))
	n, err := DecodeArray(buf, len(vals), out)
	if err != nil {
		t.Fatalf("decode %v (w=%d): %v", vals, w, err)
	}
	if n != len(buf) {
		t.Fatalf("decode consumed %d of %d bytes for %v (w=%d)", n, len(buf), vals, w)
	}
	for i := range vals {
		if out[i] != vals[i] {
			t.Fatalf("w=%d idx=%d: got %d want %d (input %v, transform %d, widths %v)",
				w, i, out[i], vals[i], vals, transformOf(buf), blockWidthsOf(buf, len(vals)))
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

// Each element width is encoded natively: no widening at the call site, and the
// width is derived from T rather than passed in.
func TestArrayElementTypes(t *testing.T) {
	t.Run("int8", func(t *testing.T) { typedSweep[int8](t, math.MinInt8, math.MaxInt8, 1) })
	t.Run("int16", func(t *testing.T) { typedSweep[int16](t, math.MinInt16, math.MaxInt16, 2) })
	t.Run("int32", func(t *testing.T) { typedSweep[int32](t, math.MinInt32, math.MaxInt32, 3) })
	t.Run("int64", func(t *testing.T) { typedSweep[int64](t, math.MinInt64, math.MaxInt64, 4) })
}

// Defined types with a Signed underlying type must work identically, since the
// width comes from unsafe.Sizeof rather than a type switch.
type recordID int32

func TestArrayDefinedType(t *testing.T) {
	if got := widthOfType[recordID](); got != 4 {
		t.Fatalf("widthOfType[recordID] = %d, want 4", got)
	}
	roundtrip(t, []recordID{100, 240, 250, 380})
	roundtrip(t, []recordID{math.MinInt32, 0, math.MaxInt32})
}

// The same values encoded as a narrower type must never cost more than as a
// wider one: the residuals are the same, so the widths are too.
func TestArrayNarrowerTypeNeverLarger(t *testing.T) {
	r := rand.New(rand.NewPCG(77, 78))
	for range 2000 {
		n := int(r.UintN(40)) + 1
		v16 := make([]int16, n)
		v32 := make([]int32, n)
		v64 := make([]int64, n)
		for i := range v16 {
			x := int16(r.Uint64())
			v16[i], v32[i], v64[i] = x, int32(x), int64(x)
		}
		b16, b32, b64 := len(AppendArray(nil, v16)), len(AppendArray(nil, v32)), len(AppendArray(nil, v64))
		if b16 > b32 || b32 > b64 {
			t.Fatalf("n=%d: int16=%dB int32=%dB int64=%dB, want non-decreasing", n, b16, b32, b64)
		}
	}
}

// Incompressible input costs the element width plus framing, and the framing is
// one header byte and one width byte per 128 values — no more. This is the bound
// the trFixed fallback used to provide; the widest block width provides it now,
// at the cost of that one byte per block.
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

// The encoder scores each transform against what it would actually occupy in
// blocks, so no other transform may produce a shorter encoding than the one it
// chose. This is the property the old (k, M) parameter search had to be checked
// for, moved to the thing that replaced it.
func TestArrayTransformChoiceIsOptimal(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	for range 2000 {
		n := int(r.UintN(300)) + 1
		vals := make([]int64, n)
		shift := r.UintN(60)
		for i := range vals {
			vals[i] = int64(r.Uint64N(1 << (shift + 1)))
		}
		chosen := len(AppendArray(nil, vals))

		minimum := vals[0]
		for _, v := range vals {
			minimum = min(minimum, v)
		}
		alternatives := map[string]int{
			"raw":      1 + blockedCost(vals, trRaw, 0, minimum < 0),
			"FOR":      1 + blockedCost(vals, trFOR, minimum, false),
			"constant": 1 + 8,
		}
		if n > 1 {
			alternatives["delta"] = 1 + blockedCost(vals, trDelta, 0, true)
		}
		for name, cost := range alternatives {
			if name == "constant" && n > 1 {
				continue // only a genuinely constant column may use it
			}
			if cost < chosen {
				t.Fatalf("n=%d: chose %dB but %s would be %dB", n, chosen, name, cost)
			}
		}
	}
}

// The three varint transforms plus the fixed fallback must each be selectable,
// otherwise a header code is dead weight.
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

func TestArrayFuzzRoundtrip(t *testing.T) {
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
		if _, err := DecodeArray(buf[:cut], len(vals), out); err == nil {
			t.Fatalf("truncation to %d/%d bytes not detected", cut, len(buf))
		}
	}
	if _, err := DecodeArray(buf, len(vals), out[:len(vals)-1]); err == nil {
		t.Fatal("short output slice not detected")
	}
	if _, err := DecodeArray(buf, -1, out); err == nil {
		t.Fatal("negative count not detected")
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
	// The empty encoding is width-independent: one header byte for every type.
	for _, got := range []int{
		len(AppendArray[int8](nil, nil)),
		len(AppendArray[int16](nil, nil)),
		len(AppendArray[int32](nil, nil)),
	} {
		if got != 1 {
			t.Fatalf("empty array encoded to %d bytes, want 1", got)
		}
	}
}

// Delta and frame-of-reference write an eight-byte base in the clear, which is
// nothing amortised over a real column and dominant over a short one. The
// encoder has to see that, and it does: the same four values that delta would
// have won on under the old codec are cheaper raw here.
func TestABaseHasToEarnItsEightBytes(t *testing.T) {
	short := []int64{100, 240, 250, 380}
	buf := roundtrip(t, short)
	if got := transformOf(buf); got != trRaw {
		t.Fatalf("four values chose transform %d, want raw: delta's base costs more than it saves", got)
	}
	// zigzag(380) is 760, ten bits, so four values are five bytes behind two of
	// framing.
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
			fuzzRoundtrip(t, vals)
		case 1:
			vals := make([]int16, len(raw)/2)
			for i := range vals {
				vals[i] = int16(uint16(raw[i*2]) | uint16(raw[i*2+1])<<8)
			}
			fuzzRoundtrip(t, vals)
		case 2:
			vals := make([]int32, len(raw)/4)
			for i := range vals {
				var u uint32
				for b := range 4 {
					u |= uint32(raw[i*4+b]) << (8 * b)
				}
				vals[i] = int32(u)
			}
			fuzzRoundtrip(t, vals)
		default:
			vals := make([]int64, len(raw)/8)
			for i := range vals {
				var u uint64
				for b := range 8 {
					u |= uint64(raw[i*8+b]) << (8 * b)
				}
				vals[i] = int64(u)
			}
			fuzzRoundtrip(t, vals)
		}
	})
}

func fuzzRoundtrip[T Signed](t *testing.T, vals []T) {
	t.Helper()
	buf := AppendArray(nil, vals)
	blocks := (len(vals) + blockSize - 1) / blockSize
	if len(buf) > 1+blocks+len(vals)*int(widthOfType[T]()) {
		t.Fatalf("encoded %dB exceeds the raw words plus framing, %dB",
			len(buf), 1+blocks+len(vals)*int(widthOfType[T]()))
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
	f.Fuzz(func(t *testing.T, buf []byte, n uint8) {
		// Every element type must survive arbitrary bytes without panicking.
		o8 := make([]int8, n)
		o16 := make([]int16, n)
		o32 := make([]int32, n)
		o64 := make([]int64, n)
		_, _ = DecodeArray(buf, int(n), o8)
		_, _ = DecodeArray(buf, int(n), o16)
		_, _ = DecodeArray(buf, int(n), o32)
		_, _ = DecodeArray(buf, int(n), o64)
	})
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
	b.SetBytes(int64(len(vals)) * int64(widthOfType[T]()))
	for b.Loop() {
		buf = AppendArray(buf[:0], vals)
	}
}

func benchDecode[T Signed](b *testing.B, vals []T) {
	buf := AppendArray(nil, vals)
	out := make([]T, len(vals))
	b.SetBytes(int64(len(vals)) * int64(widthOfType[T]()))
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

// TestArrayTypedSizeReport shows what encoding the same logical values as a
// narrower type buys, which is the point of handling each width natively.
func TestArrayTypedSizeReport(t *testing.T) {
	r := rand.New(rand.NewPCG(11, 13))
	const n = 256
	v16 := make([]int16, n)
	v32 := make([]int32, n)
	v64 := make([]int64, n)
	for i := range v16 {
		x := int16(r.Uint64()) // full int16 range: incompressible, forces trFixed
		v16[i], v32[i], v64[i] = x, int32(x), int64(x)
	}
	t.Logf("random int16 values, n=%d", n)
	t.Logf("  as []int16 %5dB  (source %5dB)", len(AppendArray(nil, v16)), n*2)
	t.Logf("  as []int32 %5dB  (source %5dB)", len(AppendArray(nil, v32)), n*4)
	t.Logf("  as []int64 %5dB  (source %5dB)", len(AppendArray(nil, v64)), n*8)

	for i := range v16 {
		x := int16(1000 + i%50) // narrow band: compresses the same at every width
		v16[i], v32[i], v64[i] = x, int32(x), int64(x)
	}
	t.Logf("narrow-band values, n=%d", n)
	t.Logf("  as []int16 %5dB", len(AppendArray(nil, v16)))
	t.Logf("  as []int32 %5dB", len(AppendArray(nil, v32)))
	t.Logf("  as []int64 %5dB", len(AppendArray(nil, v64)))
}

// TestArraySizeReport documents what the codec achieves on representative
// shapes and pins the transform each one selects.
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
		{"deltas b_max=8", mk(256, func(i int) int64 { return int64(i) * 200 }), trDelta},
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
