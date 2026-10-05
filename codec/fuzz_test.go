package codec

import (
	"bytes"
	"encoding/json"
	"testing"
)

// FuzzUnmarshal hands arbitrary bytes to every decoder, through two types that
// between them reach every op at both key widths. Beyond not panicking — the
// typed decode writes through unsafe, so a panic is the lucky outcome — it
// checks that whatever a decode accepts is a value: it encodes, and its
// encoding decodes back to the same bytes. And whatever ToJSON writes is JSON.

type fuzzLeaf struct {
	A int8    `cb:"1"`
	B uint16  `cb:"2"`
	S string  `cb:"3"`
	F float32 `cb:"4"`
}

type fuzzNarrow struct {
	Bool   bool             `cb:"1"`
	I16    int16            `cb:"2"`
	I32    int32            `cb:"3"`
	U8     uint8            `cb:"4"`
	U32    uint32           `cb:"5"`
	U64    uint64           `cb:"6"`
	F64    float64          `cb:"7"`
	Name   string           `cb:"8"`
	Blob   []byte           `cb:"9"`
	Ints   []int32          `cb:"10"`
	Words  []string         `cb:"11"`
	Leaf   fuzzLeaf         `cb:"12"`
	Leaves []fuzzLeaf       `cb:"13"`
	Counts map[string]int64 `cb:"14"`
	Opt    *int64           `cb:"15"`
	Next   *fuzzNarrow      `cb:"16"`
}

// fuzzNarrowMaps is the same at four key bits — fuzzNarrow has no id left —
// and reaches itself through a pointer to a map of itself.
type fuzzNarrowMaps struct {
	ByName map[string]fuzzLeaf        `cb:"1"`
	Ints   *[]int32                   `cb:"2"`
	Self   *map[string]fuzzNarrowMaps `cb:"3"`
}

type fuzzWide struct {
	ID     int             `cb:"1"`
	Any    any             `cb:"2"`
	Anys   []any           `cb:"3"`
	Doc    map[string]any  `cb:"4"`
	Leaves []fuzzLeaf      `cb:"20"`
	Narrow fuzzNarrow      `cb:"21"`
	Uints  []uint64        `cb:"22"`
	Flags  map[uint32]bool `cb:"23"`
	Opt    *string         `cb:"24"`
	// A map of structs at eight key bits, and pointers to collections.
	ByName map[string]fuzzLeaf  `cb:"25"`
	Tags   *[]string            `cb:"26"`
	Rows   *map[uint16]fuzzLeaf `cb:"27"`
	Maps   fuzzNarrowMaps       `cb:"28"`
	// Past 255, so the type is paged: Late is on the second page, and Last on
	// the third with the second linking to it. See pages.go.
	Late string `cb:"300"`
	Last int32  `cb:"700"`
}

func FuzzUnmarshal(f *testing.F) {
	count, word := int64(-7), "opt"
	narrow := fuzzNarrow{
		Bool: true, I16: -300, I32: 1 << 20, U8: 255, U32: 7, U64: 1 << 63, F64: -0.5,
		Name: "name", Blob: []byte{0, 1}, Ints: []int32{1, -1}, Words: []string{"a", ""},
		Leaf:   fuzzLeaf{A: -1, B: 2, S: "leaf", F: 1.5},
		Leaves: []fuzzLeaf{{A: 1}, {B: 2}, {S: "s"}},
		Counts: map[string]int64{"a": 1, "b": -2}, Opt: &count,
		Next: &fuzzNarrow{Name: "next"},
	}
	wide := fuzzWide{
		ID: -9, Any: map[string]any{"k": []any{1, "two", nil}}, Anys: []any{true, 2.5},
		Doc:    map[string]any{"rows": []fuzzLeaf{{A: 1}, {A: 2}}},
		Leaves: make([]fuzzLeaf, tableThreshold), Narrow: narrow,
		Uints: []uint64{1, 1 << 40}, Flags: map[uint32]bool{1: true, 2: false}, Opt: &word,
		Late: "late", Last: -1,
		ByName: map[string]fuzzLeaf{"a": {A: 1}, "b": {}},
		Tags:   &[]string{"t"}, Rows: &map[uint16]fuzzLeaf{3: {S: "row"}},
		Maps: fuzzNarrowMaps{
			ByName: map[string]fuzzLeaf{"x": {B: 9}}, Ints: &[]int32{-1},
			Self: &map[string]fuzzNarrowMaps{"inner": {Ints: &[]int32{2}}},
		},
	}
	for _, value := range []any{&narrow, &wide} {
		for _, encode := range []func(any) ([]byte, error){Marshal, MarshalSelfDescribing} {
			message, err := encode(value)
			if err != nil {
				f.Fatal(err)
			}
			f.Add(message)
		}
	}
	f.Add([]byte{rootStructNarrow, 0x01})
	f.Add([]byte{rootStructWide, 0x00, 0x30})

	narrowSchema, err := SchemaOf(fuzzNarrow{})
	if err != nil {
		f.Fatal(err)
	}
	wideSchema, err := SchemaOf(fuzzWide{})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		fuzzDecode[fuzzNarrow](t, data)
		fuzzDecode[fuzzWide](t, data)
		for _, schema := range []*Schema{nil, narrowSchema, wideSchema} {
			if text, err := ToJSON(schema, data); err == nil && !json.Valid(text) {
				t.Fatalf("ToJSON wrote invalid JSON: %q", text)
			}
			_, _ = DecodeAny(schema, data)
		}
	})
}

// fuzzDecode decodes data into a T and, when that succeeds, checks the value
// is one: it encodes, and decoding that encoding and encoding again gives the
// same bytes.
func fuzzDecode[T any](t *testing.T, data []byte) {
	var value T
	if Unmarshal(data, &value) != nil {
		return
	}
	once, err := Marshal(&value)
	if err != nil {
		t.Fatalf("a decoded %T does not encode: %v", value, err)
	}
	var again T
	if err := Unmarshal(once, &again); err != nil {
		t.Fatalf("a re-encoded %T does not decode: %v\n%x", value, err, once)
	}
	twice, err := Marshal(&again)
	if err != nil || !bytes.Equal(once, twice) {
		t.Fatalf("a %T re-encodes differently:\n%x\n%x (%v)", value, once, twice, err)
	}
}
