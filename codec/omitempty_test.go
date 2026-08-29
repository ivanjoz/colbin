package codec

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"

	"github.com/ivanjoz/colbin/compact"
)

// oeSparse is the shape omit-empty is for: a wide struct where most fields are
// never set, so most columns are a thousand slots of nothing.
type oeSparse struct {
	ID    int64
	A     int32
	B     int32
	C     uint16
	Name  string
	Note  string
	Ratio float64
	Blob  []byte
	Tags  []string
	Flag  bool
}

type oePtr struct {
	ID   int64    `cb:"1"`
	Qty  *int32   `cb:"2"`
	Name *string  `cb:"3"`
	Rate *float64 `cb:"4"`
}

// withOmitEmpty runs fn with the flag set, restoring it afterwards. The flag is
// global, so every test that touches it has to put it back.
func withOmitEmpty(t *testing.T, on bool, fn func()) {
	t.Helper()
	prev := OmitEmpty()
	SetOmitEmpty(on)
	defer SetOmitEmpty(prev)
	fn()
}

// The point of the flag: an all-empty column costs its type byte instead of a
// byte per record.
func TestOmitEmptyShrinksSparseColumns(t *testing.T) {
	for _, n := range []int{100, 1000} {
		recs := make([]oeSparse, n)
		for i := range recs {
			recs[i].ID = int64(i + 1)
		}
		dense, err := Marshal(recs)
		if err != nil {
			t.Fatal(err)
		}
		var sparse []byte
		withOmitEmpty(t, true, func() {
			sparse, err = Marshal(recs)
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(sparse) >= len(dense) {
			t.Fatalf("n=%d: omit-empty %d B is not smaller than %d B", n, len(sparse), len(dense))
		}
		// Nine of ten columns are empty, so most of the payload should be gone.
		if len(sparse)*4 > len(dense) {
			t.Errorf("n=%d: omit-empty %d B against %d B is less than the 4x expected",
				n, len(sparse), len(dense))
		}
		fmt.Printf("sparse 10-field struct n=%4d: dense %6d B, omit-empty %5d B\n",
			n, len(dense), len(sparse))
	}
}

// Whatever the encoder left out, the decoder has to put back — and both forms
// have to arrive at the same value, since only the encoding differs.
func TestOmitEmptyDecodesTheSame(t *testing.T) {
	recs := []oeSparse{
		{ID: 1},
		{ID: 2, Name: "set", Tags: []string{"a"}},
		{},
		{ID: 4, Ratio: -0.5, Flag: true, Blob: []byte{9}, C: 65535},
	}
	// Also the all-empty case, where every column disappears at once.
	blank := make([]oeSparse, 5)

	for _, in := range [][]oeSparse{recs, blank} {
		var dense, sparse []byte
		var err error
		if dense, err = Marshal(in); err != nil {
			t.Fatal(err)
		}
		withOmitEmpty(t, true, func() { sparse, err = Marshal(in) })
		if err != nil {
			t.Fatal(err)
		}
		var fromDense, fromSparse []oeSparse
		// Neither decode is told which encoder wrote its input: both forms are
		// self-describing, which is the property that makes the flag safe.
		if err := Unmarshal(dense, &fromDense); err != nil {
			t.Fatal(err)
		}
		if err := Unmarshal(sparse, &fromSparse); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(fromDense, fromSparse) {
			t.Fatalf("omit-empty decoded differently:\n  on %#v\n off %#v", fromSparse, fromDense)
		}
	}
}

// The same, through the self-describing mode, which has its own version byte and
// its own decoders.
func TestOmitEmptyJSONMode(t *testing.T) {
	in := []oeSparse{{ID: 1}, {ID: 2, Name: "x"}}
	var dense, sparse []byte
	var err error
	if dense, err = MarshalJSON(in); err != nil {
		t.Fatal(err)
	}
	withOmitEmpty(t, true, func() { sparse, err = MarshalJSON(in) })
	if err != nil {
		t.Fatal(err)
	}
	if len(sparse) >= len(dense) {
		t.Errorf("omit-empty JSON %d B is not smaller than %d B", len(sparse), len(dense))
	}
	a, err := DecodeJSON(dense)
	if err != nil {
		t.Fatal(err)
	}
	b, err := DecodeJSON(sparse)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("DecodeJSON differs:\n dense %s\nsparse %s", a, b)
	}
	// And the typed decoder still reads it.
	var out []oeSparse
	if err := Unmarshal(sparse, &out); err != nil {
		t.Fatal(err)
	}
}

// A message says which encoder wrote it, so a reader that predates the flag
// rejects it rather than missing the empty bit and walking into the next column.
func TestOmitEmptyVersionBytes(t *testing.T) {
	// Past MaxRecords, so the message is columnar and actually has a version
	// byte; a compact one has none at all.
	in := make([]oeSparse, compact.MaxRecords+1)
	bin, _ := Marshal(in)
	js, _ := MarshalJSON(in)
	if bin[0] != formatVersion || js[0] != jsonFormatVersion {
		t.Fatalf("dense versions 0x%02x/0x%02x", bin[0], js[0])
	}
	withOmitEmpty(t, true, func() {
		bin, _ = Marshal(in)
		js, _ = MarshalJSON(in)
	})
	if bin[0] != formatVersionOmitEmpty || js[0] != jsonFormatVersionOmitEmpty {
		t.Fatalf("omit-empty versions 0x%02x/0x%02x", bin[0], js[0])
	}
	// Both must stay even: bit 0 is the compact-mode discriminator.
	for _, v := range []byte{formatVersionOmitEmpty, jsonFormatVersionOmitEmpty} {
		if v&1 != 0 {
			t.Errorf("version 0x%02x is odd", v)
		}
	}
}

// Compact mode's part of the flag: a pointer field is admitted, because nil can
// now mean absent.
func TestOmitEmptyAdmitsPointersToCompact(t *testing.T) {
	ti, err := getTypeInfo(reflect.TypeFor[oePtr]())
	if err != nil {
		t.Fatal(err)
	}
	if compactUsable(ti) {
		t.Fatal("a pointer field was compact-eligible with the flag off")
	}
	withOmitEmpty(t, true, func() {
		if !compactUsable(ti) {
			t.Fatal("a pointer field is still not compact-eligible with the flag on")
		}
		if k := compactKeys(ti); k != compact.Keys4 {
			t.Fatalf("keys %d, want %d", k, compact.Keys4)
		}
	})
	// And back again: flipping the flag has to invalidate the cached plan, or
	// the type would stay eligible with the flag off.
	if compactUsable(ti) {
		t.Fatal("eligibility survived the flag being turned back off")
	}
}

func TestOmitEmptyPointerRoundTrip(t *testing.T) {
	q, name, rate := int32(42), "abc", 1.5
	withOmitEmpty(t, true, func() {
		for _, in := range []oePtr{
			{ID: 1, Qty: &q, Name: &name, Rate: &rate},
			{ID: 1},
			{ID: 0, Name: &name},
		} {
			buf, err := Marshal(in)
			if err != nil {
				t.Fatal(err)
			}
			if !compact.IsCompact(buf) {
				t.Fatalf("%#v did not take compact mode", in)
			}
			var out oePtr
			if err := Unmarshal(buf, &out); err != nil {
				t.Fatal(err)
			}
			if !samePtr(out, in) {
				t.Fatalf("round trip: got %v, want %v", show(out), show(in))
			}
		}
	})
}

// The one thing the flag costs, asserted so it cannot change by accident: a
// pointer to the zero value is absent, and absent comes back nil.
func TestOmitEmptyCollapsesPointerToZero(t *testing.T) {
	zero := int32(0)
	empty := ""
	withOmitEmpty(t, true, func() {
		buf, err := Marshal(oePtr{ID: 7, Qty: &zero, Name: &empty})
		if err != nil {
			t.Fatal(err)
		}
		var out oePtr
		if err := Unmarshal(buf, &out); err != nil {
			t.Fatal(err)
		}
		if out.Qty != nil || out.Name != nil {
			t.Fatalf("a pointer to the zero value survived: %v", show(out))
		}
		if out.ID != 7 {
			t.Fatalf("ID = %d, want 7", out.ID)
		}
	})
	// With the flag off the distinction is kept, which is why this is opt-in.
	buf, err := Marshal(oePtr{ID: 7, Qty: &zero})
	if err != nil {
		t.Fatal(err)
	}
	var out oePtr
	if err := Unmarshal(buf, &out); err != nil {
		t.Fatal(err)
	}
	if out.Qty == nil || *out.Qty != 0 {
		t.Fatalf("the dense form lost a pointer to zero: %v", show(out))
	}
}

// A Codec caches the plan, so it has to see the flag too.
func TestOmitEmptyThroughCodec(t *testing.T) {
	withOmitEmpty(t, true, func() {
		c := MustCodec[oePtr]()
		if !c.Compact() {
			t.Fatal("Codec[oePtr] is not compact with the flag on")
		}
		q := int32(9)
		in := oePtr{ID: 3, Qty: &q}
		buf, err := c.Marshal(&in)
		if err != nil {
			t.Fatal(err)
		}
		var out oePtr
		if err := c.Unmarshal(buf, &out); err != nil {
			t.Fatal(err)
		}
		if !samePtr(out, in) {
			t.Fatalf("got %v, want %v", show(out), show(in))
		}
	})
}

func samePtr(a, b oePtr) bool {
	eq := func(x, y *int32) bool {
		return (x == nil) == (y == nil) && (x == nil || *x == *y)
	}
	eqS := func(x, y *string) bool {
		return (x == nil) == (y == nil) && (x == nil || *x == *y)
	}
	eqF := func(x, y *float64) bool {
		return (x == nil) == (y == nil) && (x == nil || *x == *y)
	}
	return a.ID == b.ID && eq(a.Qty, b.Qty) && eqS(a.Name, b.Name) && eqF(a.Rate, b.Rate)
}

func show(v oePtr) string {
	f := func(p any) string {
		switch x := p.(type) {
		case *int32:
			if x != nil {
				return fmt.Sprint(*x)
			}
		case *string:
			if x != nil {
				return fmt.Sprintf("%q", *x)
			}
		case *float64:
			if x != nil {
				return fmt.Sprint(*x)
			}
		}
		return "nil"
	}
	return fmt.Sprintf("{ID:%d Qty:%s Name:%s Rate:%s}", v.ID, f(v.Qty), f(v.Name), f(v.Rate))
}

// The flag is global and touches every column encoder, so the corpora the rest
// of the suite round-trips have to survive it too — including the nested,
// slice-bearing one.
func TestOmitEmptyRoundTripsTheCorpora(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	scalars := randScalarRecords(200, rng)
	nested := randNestedRecords(200, rng)

	withOmitEmpty(t, true, func() {
		buf, err := Marshal(scalars)
		if err != nil {
			t.Fatal(err)
		}
		var outScalars []ScalarRecord
		if err := Unmarshal(buf, &outScalars); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(outScalars, scalars) {
			t.Fatal("scalar corpus did not survive omit-empty")
		}

		buf, err = Marshal(nested)
		if err != nil {
			t.Fatal(err)
		}
		var outNested []NestedRecord
		if err := Unmarshal(buf, &outNested); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(outNested, nested) {
			t.Fatal("nested corpus did not survive omit-empty")
		}

		// And through the self-describing path, which has its own decoders.
		js, err := MarshalJSON(nested)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeJSON(js); err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeAny(js); err != nil {
			t.Fatal(err)
		}
	})
}
