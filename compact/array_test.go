package compact

import (
	"fmt"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/ivanjoz/colbin/varint"
)

func TestIntArrayRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 12))
	for _, n := range []int{0, 1, 2, 3, 17, 100, 1000} {
		i8 := make([]int8, n)
		i16 := make([]int16, n)
		i32 := make([]int32, n)
		i64 := make([]int64, n)
		cur := int64(100000)
		for i := range n {
			i8[i] = int8(rng.IntN(256) - 128)
			i16[i] = int16(rng.IntN(65536) - 32768)
			cur += int64(rng.IntN(50)) + 1
			i32[i] = int32(cur) // sorted: exercises the delta transform
			i64[i] = rng.Int64()
		}
		w := NewWriter(nil, ShapeStruct, false, Keys8)
		PutInts(w, i8)
		PutInts(w, i16)
		PutInts(w, i32)
		PutInts(w, i64)
		r, err := NewReader(w.Done())
		if err != nil {
			t.Fatal(err)
		}
		check(t, n, "int8", i8, GetInts[int8](r))
		check(t, n, "int16", i16, GetInts[int16](r))
		check(t, n, "int32", i32, GetInts[int32](r))
		check(t, n, "int64", i64, GetInts[int64](r))
		if err := r.Err(); err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
	}
}

func check[T comparable](t *testing.T, n int, what string, want, got []T) {
	t.Helper()
	if n == 0 {
		if got != nil {
			t.Fatalf("%s: empty slice decoded as %v", what, got)
		}
		return
	}
	if len(got) != len(want) {
		t.Fatalf("%s n=%d: got %d elements, want %d", what, n, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s n=%d: element %d = %v, want %v", what, n, i, got[i], want[i])
		}
	}
}

// The point of routing arrays through varint: a compact array field must cost
// what the same array costs in the standard mode, delta transform included.
func TestIntArrayMatchesColumnarCodec(t *testing.T) {
	rng := rand.New(rand.NewPCG(13, 14))
	for _, n := range []int{4, 16, 100, 1000} {
		sorted := make([]int32, n)
		cur := int32(100000)
		for i := range n {
			cur += int32(rng.IntN(50)) + 1
			sorted[i] = cur
		}
		w := NewWriter(nil, ShapeStruct, true, Keys8)
		before := w.Bits()
		PutInts(w, sorted)
		spent := (w.Bits() - before) / 8

		columnar := len(varint.AppendArray(nil, sorted))
		countPrefix := 1 // the element count varint, 1 byte for these lengths
		if spent > columnar+countPrefix+1 {
			t.Fatalf("n=%d: compact spent %d B, columnar codec needs %d B", n, spent, columnar)
		}
		fmt.Printf("n=%4d sorted int32: compact array field %4d B, bare columnar codec %4d B\n",
			n, spent, columnar)
	}
}

func TestStringArrayRoundTrip(t *testing.T) {
	for _, vals := range [][]string{
		nil,
		{"Ana"},
		{"", "Lima", "Pedro Gomez", "el niño comió jamón", "\x00\xff binary"},
	} {
		w := NewWriter(nil, ShapeStruct, true, Keys8)
		w.Strs(vals)
		r, _ := NewReader(w.Done())
		got := r.Strs()
		check(t, len(vals), "strings", vals, got)
		if err := r.Err(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFloatAndBoolArrays(t *testing.T) {
	f32 := []float32{0, -1.5, float32(math.Inf(1)), 3.4e38}
	f64 := []float64{0, math.Pi, math.SmallestNonzeroFloat64, math.MaxFloat64}
	bs := []bool{true, false, true, true, false, false, true}

	w := NewWriter(nil, ShapeStruct, true, Keys8)
	w.Float32s(f32)
	w.Float64s(f64)
	w.Bools(bs)
	buf := w.Done()

	r, _ := NewReader(buf)
	check(t, len(f32), "float32s", f32, r.Float32s())
	check(t, len(f64), "float64s", f64, r.Float64s())
	check(t, len(bs), "bools", bs, r.Bools())
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	// Seven bools must cost seven bits plus the count, not seven bytes.
	w2 := NewWriter(nil, ShapeStruct, true, Keys8)
	before := w2.Bits()
	w2.Bools(bs)
	if spent := w2.Bits() - before; spent > 8+len(bs) {
		t.Fatalf("%d bools cost %d bits, want at most %d", len(bs), spent, 8+len(bs))
	}
}

// Arrays sit inside an ordinary key/value run and must not disturb it.
func TestArraysInsideRecord(t *testing.T) {
	ids := []int32{101, 102, 103, 104}
	tags := []string{"alpha", "beta"}

	w := NewWriter(nil, ShapeStruct, true, Keys8)
	w.Key(0x35)
	w.Int(1234)
	w.Key(0x40)
	PutInts(w, ids)
	w.Key(0x41)
	w.Strs(tags)
	w.Key(0x42)
	w.Str("tail")
	w.End()

	r, _ := NewReader(w.Done())
	if k := r.Key(); k != 0x35 {
		t.Fatalf("key %d", k)
	}
	if v := r.Int(); v != 1234 {
		t.Fatalf("int %d", v)
	}
	if k := r.Key(); k != 0x40 {
		t.Fatalf("key %d", k)
	}
	check(t, len(ids), "ids", ids, GetInts[int32](r))
	if k := r.Key(); k != 0x41 {
		t.Fatalf("key %d", k)
	}
	check(t, len(tags), "tags", tags, r.Strs())
	if k := r.Key(); k != 0x42 {
		t.Fatalf("key %d", k)
	}
	if s := r.Str(); s != "tail" {
		t.Fatalf("tail %q", s)
	}
	if k := r.Key(); k != TerminatorKey {
		t.Fatalf("terminator %d", k)
	}
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
}

// Every array kind must be steppable, so an unknown array field does not
// derail the rest of the record.
func TestSkipArrays(t *testing.T) {
	w := NewWriter(nil, ShapeStruct, true, Keys8)
	w.Key(1)
	PutInts(w, []int32{1, 2, 3, 4, 5})
	w.Key(2)
	w.Strs([]string{"a", "bb", "ccc"})
	w.Key(3)
	w.Float64s([]float64{1, 2, 3})
	w.Key(4)
	w.Bools([]bool{true, false, true})
	w.Key(5)
	w.Int(777)
	w.End()

	r, _ := NewReader(w.Done())
	for _, k := range []Kind{KindInt32s, KindStrs, KindFloat64s, KindBools} {
		r.Key()
		r.Skip(k)
	}
	if k := r.Key(); k != 5 {
		t.Fatalf("key after skipping arrays: %d", k)
	}
	if v := r.Int(); v != 777 {
		t.Fatalf("int %d", v)
	}
	if k := r.Key(); k != TerminatorKey {
		t.Fatalf("terminator %d", k)
	}
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
}

// A corrupt element count must not reach make.
func TestArrayLengthGuards(t *testing.T) {
	w := NewWriter(nil, ShapeStruct, true, Keys8)
	w.bw.putVarint(math.MaxUint64) // a count nothing could satisfy
	buf := w.Done()

	for name, read := range map[string]func(*Reader){
		"ints":     func(r *Reader) { GetInts[int64](r) },
		"strs":     func(r *Reader) { r.Strs() },
		"float32s": func(r *Reader) { r.Float32s() },
		"float64s": func(r *Reader) { r.Float64s() },
		"bools":    func(r *Reader) { r.Bools() },
		"bytes":    func(r *Reader) { r.Bytes() },
	} {
		r, err := NewReader(buf)
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("%s panicked on a bogus count: %v", name, p)
				}
			}()
			read(r)
		}()
		if r.Err() == nil {
			t.Fatalf("%s accepted a count of MaxUint64", name)
		}
	}
}

func FuzzArrays(f *testing.F) {
	w := NewWriter(nil, ShapeStruct, true, Keys8)
	PutInts(w, []int32{1, 2, 3})
	w.Strs([]string{"a", "b"})
	w.Bools([]bool{true, false})
	f.Add(w.Done())
	f.Add([]byte{0x01, 0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, buf []byte) {
		r, err := NewReader(buf)
		if err != nil {
			return
		}
		for range 16 {
			if r.Err() != nil {
				break
			}
			GetInts[int8](r)
			GetInts[int16](r)
			GetInts[int32](r)
			GetInts[int64](r)
			r.Strs()
			r.Float32s()
			r.Float64s()
			r.Bools()
			r.Bytes()
		}
	})
}
