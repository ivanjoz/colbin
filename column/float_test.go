package column

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"testing"
)

func floatBits(values ...float64) []int64 {
	out := make([]int64, len(values))
	for index, value := range values {
		out[index] = int64(math.Float64bits(value))
	}
	return out
}

// floatColumns are the shapes a float column comes in: decimals at every
// exponent, integers, decimals with the values no exponent reaches mixed in,
// and noise.
func floatColumns() map[string][]float64 {
	r := rand.New(rand.NewPCG(7, 8))
	columns := map[string][]float64{}
	generate := func(name string, n int, value func(int) float64) {
		column := make([]float64, n)
		for index := range column {
			column[index] = value(index)
		}
		columns[name] = column
	}
	generate("integers", 1000, func(int) float64 { return float64(r.IntN(5000) - 100) })
	generate("cents", 1000, func(int) float64 { return float64(r.IntN(1_000_000)) / 100 })
	generate("one decimal", 300, func(int) float64 { return float64(r.IntN(100)) / 10 })
	generate("six decimals", 500, func(int) float64 { return float64(r.Int64N(1e12)-5e11) / 1e6 })
	generate("tiny", 200, func(int) float64 { return float64(r.IntN(1000)) / 1e18 })
	generate("zeros", 100, func(int) float64 { return 0 })
	generate("constant", 100, func(int) float64 { return 2.5 })
	specials := []float64{math.NaN(), math.Inf(1), math.Inf(-1), math.Copysign(0, -1), 1e300,
		math.SmallestNonzeroFloat64, 1.0 / 3, math.MaxFloat64, 1 << 60}
	generate("cents and specials", 2000, func(index int) float64 {
		if index%97 == 3 {
			return specials[index/97%len(specials)]
		}
		return float64(r.IntN(100_000)) / 100
	})
	generate("specials first and last", 300, func(index int) float64 {
		if index == 0 || index == 299 {
			return math.NaN()
		}
		return float64(index) / 4
	})
	generate("noise", 1000, func(int) float64 { return r.NormFloat64() * 1e4 })
	generate("thirds", 1000, func(index int) float64 { return float64(index) / 3 })
	return columns
}

// A float column decodes to its bit patterns exactly, whichever form it took,
// and a decimal one is smaller than its bits.
func TestFloatColumnsRoundTrip(t *testing.T) {
	for name, values := range floatColumns() {
		vals := floatBits(values...)
		encoded := AppendFloat64(nil, vals)
		back := make([]int64, len(vals))
		used, err := DecodeArray(encoded, len(vals), back)
		if err != nil || used != len(encoded) {
			t.Fatalf("%s: used %d of %d, %v", name, used, len(encoded), err)
		}
		for index := range vals {
			if back[index] != vals[index] {
				t.Fatalf("%s: value %d is %v, want %v", name, index,
					math.Float64frombits(uint64(back[index])), values[index])
			}
		}
		decimal := encoded[0] == headerDecimal
		plain := len(AppendArray(nil, vals))
		t.Logf("%-24s %5d values: %6d bytes (decimal %v), bits %6d", name, len(vals), len(encoded), decimal, plain)
		if decimal && len(encoded) >= plain {
			t.Errorf("%s: a decimal column of %d bytes where its bits take %d", name, len(encoded), plain)
		}
		// One value is the constant transform's, and noise nobody's.
		wantDecimal := name != "noise" && name != "thirds" && name != "zeros" && name != "constant"
		if decimal != wantDecimal {
			t.Errorf("%s: decimal is %v", name, decimal)
		}
		// Every cut is refused rather than read short.
		for cut := range len(encoded) {
			if _, err := DecodeArray(encoded[:cut], len(vals), back); err == nil {
				t.Fatalf("%s: a column cut at %d of %d decoded", name, cut, len(encoded))
			}
		}
	}
}

// arrayCost is what AppendArray writes, for every shape the transforms choose
// between and every length around the short-column and block boundaries.
func TestArrayCostIsTheEncodedSize(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	shapes := map[string]func(int) int64{
		"random":   func(int) int64 { return int64(r.Uint64()) },
		"small":    func(int) int64 { return int64(r.IntN(100)) },
		"negative": func(int) int64 { return int64(r.IntN(100)) - 50 },
		"sorted":   func(index int) int64 { return int64(index*7 + r.IntN(3)) },
		"offset":   func(int) int64 { return 1<<40 + int64(r.IntN(1000)) },
		"constant": func(int) int64 { return 12345 },
		"extremes": func(index int) int64 { return []int64{math.MinInt64, math.MaxInt64}[index%2] },
	}
	for name, shape := range shapes {
		for _, n := range []int{1, 2, 8, 31, 32, 33, 127, 128, 129, 1000, 5000} {
			vals := make([]int64, n)
			for index := range vals {
				vals[index] = shape(index)
			}
			if cost, size := arrayCost(vals), len(AppendArray(nil, vals)); cost != size {
				t.Errorf("%s, %d values: cost %d, written %d", name, n, cost, size)
			}
		}
	}
}

// A float column never costs more than its bits did, at any length: a short
// one or one with exceptions is scored against them, and a long one without
// exceptions is at most the decimal header over them (AppendFloat64).
func TestFloatColumnNeverGrows(t *testing.T) {
	r := rand.New(rand.NewPCG(11, 12))
	columns := floatColumns()
	// Decimal columns at every exponent and magnitude, where the argument
	// that lets a long one skip the scoring has to hold.
	for e := range powersOf10 {
		for _, scale := range []float64{1, 1e3, 1e9, 1e-3} {
			column := make([]float64, 400)
			for index := range column {
				column[index] = float64(r.Int64N(1<<50)-1<<49) / powersOf10[e] / scale
			}
			columns[fmt.Sprintf("e=%d scale %g", e, scale)] = column
		}
	}
	for name, values := range columns {
		for n := 1; n <= len(values); n++ {
			vals := floatBits(values[:n]...)
			got, was := len(AppendFloat64(nil, vals)), len(AppendArray(nil, vals))
			allowed := was
			if n > shortColumn {
				allowed += 6
			}
			if got > allowed {
				t.Fatalf("%s, %d values: %d bytes, %d as bits", name, n, got, was)
			}
		}
	}
}

// What a decimal column refuses: a column narrower than a float, a decimal
// column inside one, and exceptions out of order or outside the column.
func TestDecimalColumnRefuses(t *testing.T) {
	values := make([]float64, 100)
	for index := range values {
		values[index] = float64(index) / 4
	}
	values[10], values[20] = math.NaN(), math.Inf(1)
	vals := floatBits(values...)
	encoded := AppendFloat64(nil, vals)
	if encoded[0] != headerDecimal || encoded[2] != 2 {
		t.Fatalf("not a decimal column with two exceptions: % x", encoded[:6])
	}
	if _, err := DecodeArray(encoded, len(vals), make([]int32, len(vals))); !errors.Is(err, errOutOfRange) {
		t.Errorf("into int32: %v", err)
	}
	nested := append([]byte{headerDecimal, 0, 0, 0, 0, 0}, encoded...)
	if _, err := DecodeArray(nested, len(vals), make([]int64, len(vals))); !errors.Is(err, errBadHeader) {
		t.Errorf("nested: %v", err)
	}
	// The positions are 10 and 20; written the other way round, or past the
	// end, they are refused.
	for name, positions := range map[string][]int64{
		"backwards": {20, 10}, "repeated": {10, 10}, "past the end": {10, 100}, "negative": {-1, 10},
	} {
		ints := make([]int64, len(vals))
		decimalInts(vals, int(encoded[1]), ints, make([]byte, len(vals)/4+1), nil, nil)
		bad := append([]byte{headerDecimal, encoded[1], 2, 0, 0, 0}, AppendArray(nil, ints)...)
		bad = AppendArray(bad, positions)
		bad = AppendArray(bad, []int64{vals[10], vals[20]})
		if _, err := DecodeArray(bad, len(vals), make([]int64, len(vals))); !errors.Is(err, errBadHeader) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := DecodeArray([]byte{headerDecimal, maxDecimalExponent + 1, 0, 0, 0, 0, 0, 0}, 1, make([]int64, 1)); !errors.Is(err, errBadHeader) {
		t.Errorf("exponent past 18: %v", err)
	}
	// An integer outside [−2^51, 2^51) is refused, at either end, wherever it
	// falls: in the kernel's part of the column or in its scalar tail.
	for _, at := range []int{0, 5, 97, 99} {
		for _, i := range []int64{1 << 51, -1<<51 - 1, math.MaxInt64, math.MinInt64} {
			ints := make([]int64, 100)
			ints[at] = i
			column := append([]byte{headerDecimal, 0, 0, 0, 0, 0}, AppendArray(nil, ints)...)
			if _, err := DecodeArray(column, 100, make([]int64, 100)); !errors.Is(err, errOutOfRange) {
				t.Errorf("%d at %d: %v", i, at, err)
			}
		}
	}
}

// decimalOf rounds the product before it adds the magic, on every
// architecture: where a fused multiply-add would round once and land on the
// other integer, it must not. Go fuses an unconverted v*p + m on arm64, so this
// is the test that fails there if the conversion is ever dropped.
func TestDecimalOfDoesNotFuse(t *testing.T) {
	r := rand.New(rand.NewPCG(9, 10))
	found := 0
	for tries := 0; tries < 10_000_000 && found < 100; tries++ {
		e := 1 + r.IntN(maxDecimalExponent)
		v := float64(r.Int64N(1<<40)) / powersOf10[r.IntN(maxDecimalExponent+1)]
		bits := int64(math.Float64bits(v))
		product := v * powersOf10[e]
		split := float64(product) + decimalMagic
		if fused := math.FMA(v, powersOf10[e], decimalMagic); fused == split {
			continue
		}
		found++
		want := int64(math.Float64bits(split)) - int64(math.Float64bits(decimalMagic))
		if i, _ := decimalOf(bits, e); i != want {
			t.Fatalf("%v at e=%d: %d, the unfused integer is %d", v, e, i, want)
		}
	}
	if found == 0 {
		t.Fatal("found no value whose product rounds differently fused")
	}
}

// The kernels are decimalOf and the decoder's conversion, value for value, on
// the values at the edges: the span's ends, NaNs with payloads, the
// infinities, -0, subnormals, and the ties the rounding breaks to even.
func TestDecimalKernelsAreTheDefinition(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	edges := []float64{0, math.Copysign(0, -1), math.Inf(1), math.Inf(-1), math.NaN(),
		math.Float64frombits(0x7FF0000000000001), math.Float64frombits(0xFFF8000000000123),
		math.SmallestNonzeroFloat64, -math.SmallestNonzeroFloat64, math.MaxFloat64,
		1 << 51, 1<<51 - 1, -(1 << 51), -(1 << 51) - 1, 1<<51 - 0.5, -(1 << 51) + 0.5,
		0.5, 1.5, 2.5, -0.5, -2.5, 0.125, 1e-300, 4503599627370495.5, 1.0 / 3}
	values := make([]int64, 4096)
	for index := range values {
		switch index % 3 {
		case 0:
			values[index] = int64(math.Float64bits(edges[r.IntN(len(edges))]))
		case 1:
			values[index] = int64(r.Uint64())
		default:
			values[index] = int64(math.Float64bits(float64(r.Int64N(1<<53)-1<<52) / powersOf10[r.IntN(19)]))
		}
	}
	for e := range powersOf10 {
		ints := make([]int64, len(values))
		misses := make([]byte, len(values)/4)
		decimalEncodeVec(&values[0], len(values), powersOf10[e], &ints[0], &misses[0])
		for index, value := range values {
			i, ok := decimalOf(value, e)
			missed := misses[index/4]&(1<<(index%4)) != 0
			if missed == ok || (ok && ints[index] != i) {
				t.Fatalf("e=%d, %v: kernel %d (missed %v), definition %d (%v)", e,
					math.Float64frombits(uint64(value)), ints[index], missed, i, ok)
			}
		}
		decoded := append([]int64(nil), ints...)
		for index := range decoded {
			decoded[index] = (decoded[index] & (1<<52 - 1)) - 1<<51 // inside the span
		}
		want := append([]int64(nil), decoded...)
		for index, i := range want {
			want[index] = int64(math.Float64bits(float64(i) / powersOf10[e]))
		}
		if outside := decimalDecodeVec(&decoded[0], len(decoded), powersOf10[e]); outside>>52 != 0 {
			t.Fatalf("e=%d: integers inside the span reported outside", e)
		}
		for index := range want {
			if decoded[index] != want[index] {
				t.Fatalf("e=%d, index %d: decoded %#x, want %#x", e, index, decoded[index], want[index])
			}
		}
	}
}

// The decoder's arithmetic is the check's: every value decimalOf accepts
// decodes to itself, at every exponent.
func TestDecimalOfIsExact(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for range 200_000 {
		e := r.IntN(maxDecimalExponent + 1)
		var v float64
		switch r.IntN(3) {
		case 0:
			v = float64(r.Int64N(1<<53)-1<<52) / powersOf10[r.IntN(len(powersOf10))]
		case 1:
			v = math.Float64frombits(r.Uint64())
		default:
			v = float64(r.IntN(1_000_000)) / powersOf10[e]
		}
		bits := int64(math.Float64bits(v))
		if i, ok := decimalOf(bits, e); ok && int64(math.Float64bits(float64(i)/powersOf10[e])) != bits {
			t.Fatalf("%v at e=%d accepted as %d", v, e, i)
		}
	}
}

func FuzzDecimalDecode(f *testing.F) {
	for _, values := range floatColumns() {
		f.Add(AppendFloat64(nil, floatBits(values...)), uint16(len(values)))
	}
	f.Fuzz(func(t *testing.T, buf []byte, n uint16) {
		out := make([]int64, n)
		_, _ = DecodeArray(buf, int(n), out)
	})
}

// The kernels alone, against the column codec's own pass over the integers.
func BenchmarkDecimalKernels(b *testing.B) {
	vals := floatBits(floatColumns()["cents"]...)[:1000]
	ints := make([]int64, len(vals))
	misses := make([]byte, len(vals)/4)
	perValue := func(b *testing.B) {
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(vals)), "ns/value")
	}
	b.Run("encode", func(b *testing.B) {
		for b.Loop() {
			decimalEncodeVec(&vals[0], len(vals), 100, &ints[0], &misses[0])
		}
		perValue(b)
	})
	b.Run("decode", func(b *testing.B) {
		work := make([]int64, len(ints))
		for b.Loop() {
			copy(work, ints)
			decimalDecodeVec(&work[0], len(work), 100)
		}
		perValue(b)
	})
	encoded := AppendArray(nil, ints)
	b.Run("ints/decode", func(b *testing.B) {
		for b.Loop() {
			_, _ = DecodeArray(encoded, len(ints), ints)
		}
		perValue(b)
	})
	b.Run("ints/encode", func(b *testing.B) {
		var out []byte
		for b.Loop() {
			out = AppendArray(out[:0], ints)
		}
		perValue(b)
	})
	b.Run("ints/cost", func(b *testing.B) {
		for b.Loop() {
			arrayCost(ints)
		}
		perValue(b)
	})
}

func BenchmarkFloatColumn(b *testing.B) {
	for _, name := range []string{"cents", "six decimals", "cents and specials", "noise"} {
		vals := floatBits(floatColumns()[name]...)
		encoded := AppendFloat64(nil, vals)
		b.Run(name+"/encode", func(b *testing.B) {
			var out []byte
			for b.Loop() {
				out = AppendFloat64(out[:0], vals)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(vals)), "ns/value")
		})
		b.Run(name+"/decode", func(b *testing.B) {
			out := make([]int64, len(vals))
			for b.Loop() {
				_, _ = DecodeArray(encoded, len(vals), out)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(vals)), "ns/value")
		})
	}
}
