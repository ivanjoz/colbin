package codec

import (
	"bytes"
	"math"
	"reflect"
	"testing"

	"github.com/ivanjoz/colbin/packed5"
)

func TestIntColumnRoundTrip(t *testing.T) {
	for name, values := range map[string][]int64{
		"mixed_positive": {0, 100, 105, 0, 110},
		"signed":         {-5, 0, 3, -100, 42},
		"empty":          {},
		"all_zero":       {0, 0, 0},
		"wide":           {math.MinInt64, math.MaxInt64, 1},
	} {
		data := appendIntColumn(nil, values, 64)
		dec := decoder{data: data}
		got, err := dec.readIntColumn(len(values), 64)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(got, values) {
			t.Fatalf("%s: got %v want %v", name, got, values)
		}
		if dec.pos != len(data) {
			t.Fatalf("%s: consumed %d of %d bytes", name, dec.pos, len(data))
		}
	}
}

func TestAllZeroFloat64s(t *testing.T) {
	for _, n := range []int{0, 1, 31, 32, 33, 1023, 1024, 1025, 4096} {
		vals := make([]float64, n)
		if !allZeroFloat64s(vals) {
			t.Fatalf("n=%d: zero slice reported non-zero", n)
		}
		if n == 0 {
			continue
		}
		for _, at := range []int{0, n / 2, n - 1} {
			vals[at] = 1
			if allZeroFloat64s(vals) {
				t.Fatalf("n=%d at=%d: non-zero value missed", n, at)
			}
			vals[at] = 0
		}
		vals[n-1] = math.NaN()
		if allZeroFloat64s(vals) {
			t.Fatalf("n=%d: NaN reported as zero", n)
		}
		vals[n-1] = math.Copysign(0, -1)
		if !allZeroFloat64s(vals) {
			t.Fatalf("n=%d: negative zero reported non-zero", n)
		}
	}
}

func TestPacked5StringColumnIntegration(t *testing.T) {
	type row struct {
		Text string `cb:"1"`
	}
	const value = "Factura 2024-1023"

	got, err := Marshal([]row{{Text: value}})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{formatVersion, 1, 1, 1, ftString}
	want = packed5.Append(want, value)
	if !bytes.Equal(got, want) {
		t.Fatalf("string column did not use packed5:\n got %x\nwant %x", got, want)
	}

	var out []row
	if err := Unmarshal(got, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, []row{{Text: value}}) {
		t.Fatalf("got %#v", out)
	}
}

func TestFullUint64RangeRoundTrip(t *testing.T) {
	type row struct {
		Value uint64 `cb:"1"`
	}
	in := []row{{0}, {math.MaxInt64}, {math.MaxInt64 + 1}, {math.MaxUint64}}
	data, err := Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out []row
	if err := Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("got %#v want %#v", out, in)
	}
}

func TestNativeWidthIntegerRoundTrip(t *testing.T) {
	type row struct {
		I8  int8   `cb:"1"`
		U8  uint8  `cb:"2"`
		I16 int16  `cb:"3"`
		U16 uint16 `cb:"4"`
		I32 int32  `cb:"5"`
		U32 uint32 `cb:"6"`
	}
	in := []row{
		{I8: math.MinInt8, U8: math.MaxUint8, I16: math.MinInt16, U16: math.MaxUint16, I32: math.MinInt32, U32: math.MaxUint32},
		{I8: math.MaxInt8, U8: 128, I16: math.MaxInt16, U16: 32768, I32: math.MaxInt32, U32: 1 << 31},
	}
	data, err := Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out []row
	if err := Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("got %#v want %#v", out, in)
	}
}
