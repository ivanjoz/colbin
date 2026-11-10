package colbin_test

import (
	"encoding/binary"
	"math"
	"math/rand/v2"
	"reflect"
	"testing"

	"github.com/ivanjoz/colbin"
)

type storeID int32

func sliceRoundTrip[T colbin.Number](t *testing.T, name string, vals []T) []byte {
	t.Helper()
	encoded := colbin.EncodeSlice(vals)
	back, err := colbin.DecodeSlice[T](encoded)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if len(back) != len(vals) {
		t.Fatalf("%s: %d values back of %d", name, len(back), len(vals))
	}
	for index := range vals {
		// Compared as bits where floats are, so NaN and -0 count.
		if !reflect.DeepEqual(bitsOf(back[index]), bitsOf(vals[index])) {
			t.Fatalf("%s: value %d is %v, want %v", name, index, back[index], vals[index])
		}
	}
	for cut := range len(encoded) {
		if _, err := colbin.DecodeSlice[T](encoded[:cut]); err == nil && len(vals) > 0 {
			t.Fatalf("%s: a slice cut at %d of %d decoded", name, cut, len(encoded))
		}
	}
	return encoded
}

func bitsOf[T colbin.Number](v T) any {
	switch x := any(v).(type) {
	case float32:
		return math.Float32bits(x)
	case float64:
		return math.Float64bits(x)
	}
	return v
}

func fill[T colbin.Number](n int, value func(int) T) []T {
	out := make([]T, n)
	for index := range out {
		out[index] = value(index)
	}
	return out
}

func TestSliceRoundTripEveryType(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for _, n := range []int{0, 1, 2, 31, 32, 33, 127, 128, 129, 1000} {
		sliceRoundTrip(t, "int8", fill(n, func(int) int8 { return int8(r.IntN(256) - 128) }))
		sliceRoundTrip(t, "int16", fill(n, func(int) int16 { return int16(r.IntN(65536) - 32768) }))
		sliceRoundTrip(t, "int32", fill(n, func(int) int32 { return int32(r.Uint32()) }))
		sliceRoundTrip(t, "int64", fill(n, func(int) int64 { return int64(r.Uint64()) }))
		sliceRoundTrip(t, "uint8", fill(n, func(int) uint8 { return uint8(r.Uint32()) }))
		sliceRoundTrip(t, "uint16", fill(n, func(int) uint16 { return uint16(r.Uint32()) }))
		sliceRoundTrip(t, "uint32", fill(n, func(int) uint32 { return r.Uint32() }))
		sliceRoundTrip(t, "float32", fill(n, func(int) float32 { return float32(r.IntN(100000)) / 100 }))
		sliceRoundTrip(t, "float32 noise", fill(n, func(int) float32 { return math.Float32frombits(r.Uint32()) }))
		sliceRoundTrip(t, "float64", fill(n, func(int) float64 { return float64(r.IntN(100000)) / 100 }))
		sliceRoundTrip(t, "float64 noise", fill(n, func(int) float64 { return math.Float64frombits(r.Uint64()) }))
		sliceRoundTrip(t, "named", fill(n, func(int) storeID { return storeID(r.IntN(5000)) }))
	}
}

func TestSliceExtremes(t *testing.T) {
	sliceRoundTrip(t, "int64", []int64{math.MinInt64, math.MaxInt64, 0, -1, 1})
	sliceRoundTrip(t, "int8", []int8{math.MinInt8, math.MaxInt8})
	sliceRoundTrip(t, "uint32", []uint32{0, math.MaxUint32})
	sliceRoundTrip(t, "float64", []float64{math.NaN(), math.Inf(1), math.Inf(-1), math.Copysign(0, -1), math.MaxFloat64, 0.1})
	sliceRoundTrip(t, "float32", []float32{
		float32(math.NaN()), float32(math.Inf(-1)), float32(math.Copysign(0, -1)),
		math.Float32frombits(0x7F800001), math.MaxFloat32, 0.1, 12.5,
	})
}

func TestSliceEmpty(t *testing.T) {
	for _, encoded := range [][]byte{colbin.EncodeSlice([]int32(nil)), colbin.EncodeSlice([]int32{})} {
		if len(encoded) != 1 || encoded[0] != 0 {
			t.Fatalf("an empty slice is %v", encoded)
		}
		back, err := colbin.DecodeSlice[int32](encoded)
		if err != nil || back == nil || len(back) != 0 {
			t.Fatalf("decoded %v, %v", back, err)
		}
	}
	if _, err := colbin.DecodeSlice[int32]([]byte{0, 0}); err == nil {
		t.Error("bytes after an empty slice decoded")
	}
}

// The slice reads back into a wider integer type, and a value outside a
// narrower one is an error.
func TestSliceIntegerWidths(t *testing.T) {
	encoded := colbin.EncodeSlice([]int16{-5, 300, 32000})
	wide, err := colbin.DecodeSlice[int32](encoded)
	if err != nil || !reflect.DeepEqual(wide, []int32{-5, 300, 32000}) {
		t.Fatalf("%v, %v", wide, err)
	}
	if _, err := colbin.DecodeSlice[int8](encoded); err == nil {
		t.Error("32000 decoded as an int8")
	}
	if _, err := colbin.DecodeSlice[uint16](colbin.EncodeSlice([]int32{-1})); err == nil {
		t.Error("-1 decoded as a uint16")
	}
	if _, err := colbin.DecodeSlice[uint8](colbin.EncodeSlice([]int32{256})); err == nil {
		t.Error("256 decoded as a uint8")
	}
	// Unsigned and signed agree on the bytes for the same values.
	if !reflect.DeepEqual(colbin.EncodeSlice([]uint16{1, 2, 3}), colbin.EncodeSlice([]int16{1, 2, 3})) {
		t.Error("uint16 and int16 differ for the same values")
	}
}

func TestSliceRefusesBadInput(t *testing.T) {
	if _, err := colbin.DecodeSlice[int32](nil); err == nil {
		t.Error("no bytes decoded")
	}
	if _, err := colbin.DecodeSlice[int32](binary.AppendUvarint(nil, colbin.MaxSliceLen+1)); err == nil {
		t.Error("a count past MaxSliceLen decoded")
	}
	// A huge constant column is nine bytes, which is why the count is capped.
	huge := binary.AppendUvarint(nil, colbin.MaxSliceLen)
	huge = append(huge, 3, 7, 0, 0, 0, 0, 0, 0, 0) // constant, base zigzag(7)
	if _, err := colbin.DecodeSlice[int8](huge); err != nil {
		t.Errorf("a constant column of MaxSliceLen: %v", err)
	}
	encoded := append(colbin.EncodeSlice([]int32{1, 2, 3}), 0)
	if _, err := colbin.DecodeSlice[int32](encoded); err == nil {
		t.Error("a trailing byte decoded")
	}
}

func TestAppendSliceAppends(t *testing.T) {
	prefix := []byte("xy")
	out := colbin.AppendSlice(prefix, []int32{1, 2, 3})
	if string(out[:2]) != "xy" || !reflect.DeepEqual(out[2:], colbin.EncodeSlice([]int32{1, 2, 3})) {
		t.Fatalf("%v", out)
	}
}

// The reason the function exists: what the ORM stored with Marshal.
func TestSliceIsSmallerThanMarshal(t *testing.T) {
	ids := fill(200, func(index int) int32 { return int32(500000 + index*3) })
	marshaled, err := colbin.Marshal(ids)
	if err != nil {
		t.Fatal(err)
	}
	encoded := colbin.EncodeSlice(ids)
	t.Logf("200 sequential ids: Marshal %d bytes, EncodeSlice %d, raw %d", len(marshaled), len(encoded), 4*len(ids))
	if len(encoded)*5 > len(marshaled) {
		t.Errorf("EncodeSlice %d bytes against Marshal's %d", len(encoded), len(marshaled))
	}

	r := rand.New(rand.NewPCG(3, 4))
	prices := fill(500, func(int) float32 { return float32(r.IntN(10000)) / 100 })
	t.Logf("500 prices: EncodeSlice %d bytes, raw %d", len(colbin.EncodeSlice(prices)), 4*len(prices))
}

func FuzzSlice(f *testing.F) {
	f.Add([]byte{3, 3, 7, 0, 0, 0, 0, 0, 0, 0})
	f.Add(colbin.EncodeSlice([]int32{1, 2, 3}))
	f.Add(colbin.EncodeSlice([]float32{0.1, 0.2}))
	f.Fuzz(func(t *testing.T, data []byte) {
		// Whatever the bytes are, no panic, and what decodes encodes back.
		if back, err := colbin.DecodeSlice[int32](data); err == nil {
			again, err := colbin.DecodeSlice[int32](colbin.EncodeSlice(back))
			if err != nil || !reflect.DeepEqual(again, back) {
				t.Fatalf("%v then %v: %v", back, again, err)
			}
		}
		colbin.DecodeSlice[int8](data)
		colbin.DecodeSlice[uint16](data)
		colbin.DecodeSlice[int64](data)
		colbin.DecodeSlice[float32](data)
		colbin.DecodeSlice[float64](data)
	})
}
