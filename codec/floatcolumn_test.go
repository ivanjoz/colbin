package codec

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"math/rand/v2"
	"reflect"
	"testing"
)

// A long array of floats is a column when that is smaller (wire/arrays.go).

type floatsNarrow struct {
	F64  []float64 `cb:"1"`
	F32  []float32 `cb:"2"`
	Tail int32     `cb:"3"`
}

type floatsWide struct {
	F64  []float64 `cb:"30"`
	F32  []float32 `cb:"31"`
	Tail int32     `cb:"32"`
}

// prices is count decimals, which is what a column is smaller on.
func prices(count int) floatsNarrow {
	value := floatsNarrow{Tail: 9}
	for index := range count {
		value.F64 = append(value.F64, float64(1000+index*37%911)/100)
		value.F32 = append(value.F32, float32(200+index*13%450)/10)
	}
	return value
}

func floatBitsEqual(a, b floatsNarrow) bool {
	if len(a.F64) != len(b.F64) || len(a.F32) != len(b.F32) || a.Tail != b.Tail {
		return false
	}
	for index := range a.F64 {
		if math.Float64bits(a.F64[index]) != math.Float64bits(b.F64[index]) {
			return false
		}
	}
	for index := range a.F32 {
		if math.Float32bits(a.F32[index]) != math.Float32bits(b.F32[index]) {
			return false
		}
	}
	return true
}

func roundTripFloats(t *testing.T, original floatsNarrow) (narrow, wide []byte) {
	t.Helper()
	narrow = must(Marshal(&original))
	var back floatsNarrow
	if err := Unmarshal(narrow, &back); err != nil {
		t.Fatalf("%d floats: %v", len(original.F64), err)
	}
	if !floatBitsEqual(back, original) {
		t.Fatalf("%d floats read back differently", len(original.F64))
	}
	asWide := floatsWide(original)
	wide = must(Marshal(&asWide))
	var wideBack floatsWide
	if err := Unmarshal(wide, &wideBack); err != nil {
		t.Fatalf("%d floats, wide: %v", len(original.F64), err)
	}
	if !floatBitsEqual(floatsNarrow(wideBack), original) {
		t.Fatalf("%d floats, wide, read back differently", len(original.F64))
	}
	return narrow, wide
}

func TestLongFloatArraysAreColumns(t *testing.T) {
	for _, count := range []int{127, 128, 129, 254, 255, 256, 1000, 20000} {
		original := prices(count)
		narrow, wide := roundTripFloats(t, original)
		plain := count * (8 + 4)
		for _, message := range [][]byte{narrow, wide} {
			if count < 128 && len(message) < plain {
				t.Fatalf("%d floats took %d bytes: a short array is its bits", count, len(message))
			}
			if count >= 128 && len(message) > plain/2 {
				t.Fatalf("%d decimals took %d bytes of %d", count, len(message), plain)
			}
		}
		checkPrefixes(t, &original, unmarshalInto[floatsNarrow])
		asWide := floatsWide(original)
		checkPrefixes(t, &asWide, unmarshalInto[floatsWide])
	}
}

// Floats a column is not smaller on stay their bits, at exactly their size:
// the column costs such an array nothing on the wire and nothing to read.
func TestIncompressibleFloatArraysStayTheirBits(t *testing.T) {
	random := rand.New(rand.NewPCG(1, 2))
	original := floatsNarrow{}
	for range 4096 {
		original.F64 = append(original.F64, random.NormFloat64())
		original.F32 = append(original.F32, float32(random.NormFloat64()))
	}
	narrow, wide := roundTripFloats(t, original)
	// The bits, and for each array a key and a length of at most four bytes,
	// behind the byte a wide root opens with.
	for _, message := range [][]byte{narrow, wide} {
		if bits := 4096 * 12; len(message) < bits || len(message) > bits+2*6+1 {
			t.Fatalf("%d bytes for %d of bits", len(message), bits)
		}
	}
}

// A column far smaller than a bit a value is kept as bits: a reader allocates
// for a column's count, and refuses one its bytes do not vouch for.
func TestConstantFloatArraysStayTheirBits(t *testing.T) {
	original := floatsNarrow{F64: make([]float64, 5000), F32: make([]float32, 5000)}
	for index := range original.F64 {
		original.F64[index], original.F32[index] = 2.5, 2.5
	}
	narrow, wide := roundTripFloats(t, original)
	if len(narrow) < 5000*12 || len(wide) < 5000*12 {
		t.Fatalf("constant arrays took %d and %d bytes", len(narrow), len(wide))
	}
}

// Every bit pattern survives a column: an exception is carried as its bits.
func TestFloatColumnsKeepEveryBit(t *testing.T) {
	original := prices(600)
	original.F64[3] = math.Float64frombits(0x7FF8_0000_DEAD_BEEF)
	original.F64[70] = math.Inf(-1)
	original.F64[200] = math.Copysign(0, -1)
	original.F64[599] = math.SmallestNonzeroFloat64
	original.F32[5] = math.Float32frombits(0x7FC0_BEEF)
	original.F32[90] = float32(math.Inf(1))
	original.F32[300] = float32(math.Copysign(0, -1))
	narrow, _ := roundTripFloats(t, original)
	if len(narrow) > 600*12/2 {
		t.Fatalf("a few exceptions cost the column: %d bytes", len(narrow))
	}
}

// A count the column's bytes do not vouch for is refused before anything is
// allocated for it.
func TestFloatColumnCountIsBounded(t *testing.T) {
	type one struct {
		F64 []float64 `cb:"1"`
	}
	type oneWide struct {
		F64 []float64 `cb:"30"`
	}
	original := prices(300).F64
	for _, value := range []any{&one{original}, &oneWide{original}} {
		message := must(Marshal(value))
		// The count is the escape and 300 in four bytes.
		at := -1
		for index := 0; index+5 <= len(message); index++ {
			if message[index] == 0xFF && binary.LittleEndian.Uint32(message[index+1:]) == 300 {
				at = index
				break
			}
		}
		if at < 0 {
			t.Fatalf("%T: no count in %x", value, message[:16])
		}
		into := reflect.New(reflect.TypeOf(value).Elem()).Interface()
		for _, count := range []uint32{301, 299, 1 << 20, 1 << 31, math.MaxUint32} {
			hostile := append([]byte{}, message...)
			binary.LittleEndian.PutUint32(hostile[at+1:], count)
			if err := Unmarshal(hostile, into); err == nil && count > 301 {
				t.Fatalf("%T: a count of %d was accepted", value, count)
			}
		}
	}
}

func TestFloatColumnsRenderAsJSON(t *testing.T) {
	original := prices(400)
	for _, value := range []any{&original, func() any { wide := floatsWide(original); return &wide }()} {
		text, err := ToJSON(nil, must(MarshalSelfDescribing(value)))
		if err != nil {
			t.Fatal(err)
		}
		var got, expected any
		if json.Unmarshal(text, &got) != nil || json.Unmarshal(must(json.Marshal(value)), &expected) != nil ||
			!reflect.DeepEqual(got, expected) {
			t.Fatalf("%T rendered differently from encoding/json", value)
		}
	}
}

// A column read through a section converts as the bits do.
func TestForeignFloatColumnsConvert(t *testing.T) {
	type source struct {
		Small []float32 `cb:"1"`
		Big   []float64 `cb:"2"`
	}
	type target struct {
		Big   []float32 `cb:"1"`
		Small []float64 `cb:"2"`
	}
	original := prices(500)
	message := must(MarshalSelfDescribing(&source{Small: original.F32, Big: original.F64}))
	var back target
	if err := Unmarshal(message, &back); err != nil {
		t.Fatal(err)
	}
	for index := range original.F64 {
		if back.Big[index] != float32(original.F64[index]) || back.Small[index] != float64(original.F32[index]) {
			t.Fatalf("element %d converted to %v and %v", index, back.Big[index], back.Small[index])
		}
	}
}

// A handle decoding into a held value reads a column after bits and bits after
// a column.
func TestFloatColumnsThroughAHandle(t *testing.T) {
	codec := must(NewCodec[floatsNarrow]())
	var held floatsNarrow
	for _, count := range []int{500, 3, 2000, 127, 128} {
		original := prices(count)
		message := must(codec.Append(nil, &original))
		if err := codec.Unmarshal(message, &held); err != nil {
			t.Fatal(err)
		}
		if !floatBitsEqual(held, original) {
			t.Fatalf("%d floats read back differently into a held value", count)
		}
	}
}

func BenchmarkFloatArray(b *testing.B) {
	type holder struct {
		F64 []float64 `cb:"1"`
	}
	random := rand.New(rand.NewPCG(3, 4))
	noise := make([]float64, 1024)
	for index := range noise {
		noise[index] = random.NormFloat64()
	}
	for _, kase := range []struct {
		name   string
		values []float64
	}{
		{"decimals=100", prices(100).F64},
		{"decimals=1024", prices(1024).F64},
		{"noise=1024", noise},
	} {
		value := holder{kase.values}
		message := must(Marshal(&value))
		b.Run(kase.name+"/encode", func(b *testing.B) {
			var buffer []byte
			b.ReportAllocs()
			b.ReportMetric(float64(len(message)), "msg-B")
			for b.Loop() {
				buffer, _ = Append(buffer[:0], &value)
			}
		})
		b.Run(kase.name+"/decode", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var back holder
				if err := Unmarshal(message, &back); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
