package column

import (
	"math"
	"math/rand/v2"
	"testing"
)

func float32Columns() map[string][]float32 {
	r := rand.New(rand.NewPCG(11, 12))
	columns := map[string][]float32{}
	generate := func(name string, n int, value func(int) float32) {
		column := make([]float32, n)
		for index := range column {
			column[index] = value(index)
		}
		columns[name] = column
	}
	generate("integers", 1000, func(int) float32 { return float32(r.IntN(5000) - 100) })
	generate("cents", 1000, func(int) float32 { return float32(r.IntN(1_000_000)) / 100 })
	generate("one decimal", 300, func(int) float32 { return float32(r.IntN(100)) / 10 })
	generate("prices", 64, func(int) float32 { return float32(r.IntN(10000)) / 100 })
	generate("short", 5, func(index int) float32 { return float32(index) / 10 })
	generate("zeros", 100, func(int) float32 { return 0 })
	generate("constant", 100, func(int) float32 { return 2.5 })
	generate("one", 1, func(int) float32 { return 0.1 })
	specials := []float32{
		float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1)),
		float32(math.Copysign(0, -1)), math.MaxFloat32, math.SmallestNonzeroFloat32,
		math.Float32frombits(0x7F800001), // a signalling NaN
		math.Float32frombits(0xFFC12345), // a negative NaN with a payload
		1.0 / 3,
	}
	generate("cents and specials", 2000, func(index int) float32 {
		if index%97 == 3 {
			return specials[index/97%len(specials)]
		}
		return float32(r.IntN(100_000)) / 100
	})
	generate("specials first and last", 300, func(index int) float32 {
		if index == 0 || index == 299 {
			return math.Float32frombits(0x7F800001)
		}
		return float32(index) / 4
	})
	generate("noise", 1000, func(int) float32 { return float32(r.NormFloat64() * 1e4) })
	generate("thirds", 1000, func(index int) float32 { return float32(index) / 3 })
	generate("all specials", len(specials), func(index int) float32 { return specials[index] })
	return columns
}

// A float32 column decodes to its bit patterns exactly, whichever form it took,
// and a decimal one is smaller than its bits.
func TestFloat32ColumnsRoundTrip(t *testing.T) {
	for name, vals := range float32Columns() {
		encoded := AppendFloat32(nil, vals)
		back := make([]float32, len(vals))
		used, err := DecodeFloat32(encoded, len(vals), back)
		if err != nil || used != len(encoded) {
			t.Fatalf("%s: used %d of %d, %v", name, used, len(encoded), err)
		}
		for index := range vals {
			if math.Float32bits(back[index]) != math.Float32bits(vals[index]) {
				t.Fatalf("%s: value %d is %#x, want %#x", name, index,
					math.Float32bits(back[index]), math.Float32bits(vals[index]))
			}
		}
		raw := make([]int32, len(vals))
		for index, v := range vals {
			raw[index] = int32(math.Float32bits(v))
		}
		plain := len(AppendArray(nil, raw))
		decimal := len(encoded) > 0 && encoded[0] == headerDecimal && len(vals) > 0
		t.Logf("%-24s %5d values: %6d bytes (decimal %v), bits %6d", name, len(vals), len(encoded), decimal, plain)
		if len(encoded) > plain {
			t.Errorf("%s: %d bytes where its bits take %d", name, len(encoded), plain)
		}
		switch name {
		case "integers", "cents", "one decimal", "prices", "cents and specials":
			if !decimal {
				t.Errorf("%s: not a decimal column", name)
			}
		case "noise", "thirds":
			if decimal {
				t.Errorf("%s: a decimal column", name)
			}
		}
		// Every cut is refused rather than read short.
		for cut := range len(encoded) {
			if _, err := DecodeFloat32(encoded[:cut], len(vals), back); err == nil {
				t.Fatalf("%s: a column cut at %d of %d decoded", name, cut, len(encoded))
			}
		}
	}
}

// A decimal column costs the bits its integers need, not 32: prices up to 1000
// at two decimals are 17 bits, a smaller range fewer.
func TestFloat32DecimalsAreSmall(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	for _, tc := range []struct {
		name  string
		limit int // cents
		share int // percent of the bits at most
	}{{"prices to 1000", 100_000, 60}, {"prices to 20", 2000, 40}} {
		vals := make([]float32, 1000)
		for index := range vals {
			vals[index] = float32(r.IntN(tc.limit)) / 100
		}
		size := len(AppendFloat32(nil, vals))
		if size*100 > 4*len(vals)*tc.share {
			t.Errorf("%s: %d bytes, over %d%% of the %d the bits take", tc.name, size, tc.share, 4*len(vals))
		}
	}
}

// 0.1f is the case the float32 test exists for: AppendFloat64 over its
// widening reaches nothing, and the float32 form one integer.
func TestFloat32ReachesWhatWideningDoesNot(t *testing.T) {
	vals := make([]float32, 200)
	wide := make([]int64, len(vals))
	for index := range vals {
		vals[index] = float32(index%50) / 10
		wide[index] = int64(math.Float64bits(float64(vals[index])))
	}
	narrow, widened := len(AppendFloat32(nil, vals)), len(AppendFloat64(nil, wide))
	if narrow >= widened {
		t.Errorf("float32 form %d bytes, widened float64 form %d", narrow, widened)
	}
}

// A decoder refuses a decimal column whose values are not float32s'.
func TestFloat32RefusesWhatFloat32CannotHold(t *testing.T) {
	// 1e300 as a decimal column of float64: past float32's range.
	huge := AppendFloat64(nil, floatBits(1e300, 1e300, 1e300, 1e300))
	out := make([]float32, 4)
	if _, err := DecodeFloat32(huge, 4, out); err == nil {
		t.Error("1e300 decoded as a float32")
	}
	// A NaN whose payload does not fit float32's.
	nan := AppendArray(nil, []int64{int64(math.Float64bits(math.NaN()) | 1)})
	if _, err := DecodeFloat32(nan, 1, out); err == nil {
		t.Error("a float64 NaN payload decoded as a float32")
	}
	if _, err := DecodeFloat32(nil, 1, out); err == nil {
		t.Error("an empty buffer decoded")
	}
	if _, err := DecodeFloat32([]byte{0}, -1, out); err == nil {
		t.Error("a negative count decoded")
	}
	if _, err := DecodeFloat32([]byte{0}, 5, out); err == nil {
		t.Error("a short out decoded")
	}
}

// AppendFloat32 and AppendFloat64 share a scratch pool: a float32 column
// that grows it must leave it usable by a longer float64 one.
func TestFloat32AndFloat64ShareScratch(t *testing.T) {
	small := make([]float32, 5000)
	for index := range small {
		small[index] = float32(index%900) / 100
	}
	wide := make([]float64, 5000)
	for index := range wide {
		wide[index] = float64(index%900) / 100
	}
	for range 4 {
		AppendFloat32(nil, small)
		encoded := AppendFloat64(nil, floatBits(wide...))
		back := make([]int64, len(wide))
		if _, err := DecodeArray(encoded, len(wide), back); err != nil {
			t.Fatal(err)
		}
	}
}

// The kernels are decimalOf32 and narrowBits' conversion, value for value, on
// the values at the edges: the span's ends, NaNs with payloads and a signalling
// one, the infinities, -0, subnormals, the ties the rounding breaks to even and
// float32's overflow. They run as the assembly where there is one and as the
// scalar twins elsewhere, and the definition is the same for both.
func TestFloat32KernelsAreTheDefinition(t *testing.T) {
	if !useSIMD {
		t.Skip("this CPU runs no vector kernels")
	}
	r := rand.New(rand.NewPCG(9, 10))
	edges := []float32{0, float32(math.Copysign(0, -1)), float32(math.Inf(1)), float32(math.Inf(-1)),
		float32(math.NaN()), math.Float32frombits(0x7F800001), math.Float32frombits(0xFFC12345),
		math.SmallestNonzeroFloat32, -math.SmallestNonzeroFloat32, math.MaxFloat32, -math.MaxFloat32,
		1 << 23, 1<<24 - 1, 1 << 24, 1e10, 1e-30, 0.5, 1.5, 2.5, -0.5, -2.5, 0.125, 0.1, 1.0 / 3, 8388607.5}
	values := make([]float32, 4096)
	for index := range values {
		switch index % 4 {
		case 0:
			values[index] = edges[r.IntN(len(edges))]
		case 1:
			values[index] = math.Float32frombits(r.Uint32())
		case 2:
			values[index] = float32(r.IntN(2_000_000)-1_000_000) / float32(powersOf10[r.IntN(6)])
		default:
			values[index] = float32(r.IntN(100_000)) / 100
		}
	}
	for e := 0; e <= maxFloat32Exponent; e++ {
		ints := make([]int64, len(values))
		misses := make([]byte, len(values)/4)
		decimalEncodeVec32(&values[0], len(values), powersOf10[e], &ints[0], &misses[0])
		for index, v := range values {
			i, ok := decimalOf32(v, e)
			missed := misses[index/4]&(1<<(index%4)) != 0
			if missed == ok || (ok && ints[index] != i) {
				t.Fatalf("e=%d, %v (%#x): kernel %d (missed %v), definition %d (%v)", e, v,
					math.Float32bits(v), ints[index], missed, i, ok)
			}
		}
	}

	// The narrowing: every pattern whose result is a number, and the flag where
	// one is not.
	for round := range 200 {
		wide := make([]int64, 64)
		wantSpecial := false
		for index := range wide {
			switch r.IntN(4) {
			case 0:
				wide[index] = int64(math.Float64bits(float64(r.IntN(2_000_000)-1_000_000) / powersOf10[r.IntN(8)]))
			case 1:
				wide[index] = int64(math.Float64bits(r.NormFloat64() * 1e5))
			case 2:
				wide[index] = int64(math.Float64bits(float64(float32(r.NormFloat64()))))
			default:
				wide[index] = int64(math.Float64bits(float64(r.IntN(100_000)) / 100))
			}
			if round%20 == 0 && index == round%64 { // a special, now and then
				wide[index] = int64(math.Float64bits([]float64{math.Inf(1), math.NaN(), 1e300, -1e300}[round/20%4]))
			}
			if _, ok := narrowBits(uint64(wide[index])); !ok || float32Special(wide[index]) {
				wantSpecial = true
			}
		}
		got := make([]float32, len(wide))
		special := decimalNarrowVec(&wide[0], len(wide), &got[0]) != 0
		if special != wantSpecial {
			t.Fatalf("round %d: special %v, want %v", round, special, wantSpecial)
		}
		if special {
			continue
		}
		for index, bits := range wide {
			want, _ := narrowBits(uint64(bits))
			if math.Float32bits(got[index]) != want {
				t.Fatalf("round %d, value %d: %#x, want %#x", round, index, math.Float32bits(got[index]), want)
			}
		}
	}
}

// float32Special is whether float32 of the pattern is a NaN or infinity, the
// narrowing kernel's flag.
func float32Special(bits int64) bool {
	return math.Float32bits(float32(math.Float64frombits(uint64(bits))))&0x7F800000 == 0x7F800000
}

// Columns whose length is not a multiple of four, and ones with the specials
// in the tail and in the vector part, decode the same through the kernels and
// the scalar loop.
func TestFloat32ColumnsOfEveryLength(t *testing.T) {
	r := rand.New(rand.NewPCG(13, 14))
	specials := []float32{float32(math.Inf(1)), float32(math.NaN()), math.Float32frombits(0x7F800001), math.MaxFloat32}
	for n := 0; n <= 70; n++ {
		for _, special := range []int{-1, 0, n / 2, n - 1} {
			vals := make([]float32, n)
			for index := range vals {
				vals[index] = float32(r.IntN(100_000)) / 100
				if index == special {
					vals[index] = specials[r.IntN(len(specials))]
				}
			}
			encoded := AppendFloat32(nil, vals)
			back := make([]float32, n)
			if used, err := DecodeFloat32(encoded, n, back); err != nil || used != len(encoded) {
				t.Fatalf("n=%d special=%d: used %d of %d, %v", n, special, used, len(encoded), err)
			}
			for index := range vals {
				if math.Float32bits(back[index]) != math.Float32bits(vals[index]) {
					t.Fatalf("n=%d special=%d: value %d is %#x, want %#x", n, special, index,
						math.Float32bits(back[index]), math.Float32bits(vals[index]))
				}
			}
		}
	}
}

func TestWidenNarrowBits(t *testing.T) {
	for _, bits := range []uint32{0, 0x80000000, 1, 0x7F7FFFFF, 0x7F800000, 0xFF800000,
		0x7F800001, 0x7FC00000, 0xFFC12345, 0x3DCCCCCD} {
		back, ok := narrowBits(widenBits(bits))
		if !ok || back != bits {
			t.Errorf("%#x widened and narrowed to %#x (%v)", bits, back, ok)
		}
	}
}

func FuzzFloat32(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8}, uint8(2))
	f.Fuzz(func(t *testing.T, data []byte, mode uint8) {
		vals := make([]float32, len(data)/4)
		for index := range vals {
			bits := uint32(data[4*index]) | uint32(data[4*index+1])<<8 | uint32(data[4*index+2])<<16 | uint32(data[4*index+3])<<24
			if mode%3 == 0 { // a decimal column with the odd bit pattern
				if bits%5 != 0 {
					vals[index] = float32(bits%100000) / 100
					continue
				}
			}
			vals[index] = math.Float32frombits(bits)
		}
		encoded := AppendFloat32(nil, vals)
		back := make([]float32, len(vals))
		if used, err := DecodeFloat32(encoded, len(vals), back); err != nil || used != len(encoded) {
			t.Fatalf("used %d of %d: %v", used, len(encoded), err)
		}
		for index := range vals {
			if math.Float32bits(back[index]) != math.Float32bits(vals[index]) {
				t.Fatalf("value %d is %#x, want %#x", index, math.Float32bits(back[index]), math.Float32bits(vals[index]))
			}
		}
		// And whatever the bytes are, the decoder returns rather than panics.
		DecodeFloat32(data, len(vals), back)
	})
}
