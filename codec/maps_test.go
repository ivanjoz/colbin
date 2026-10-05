package codec

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

type withMaps struct {
	Head   uint32             `cb:"1"`
	Labels map[string]string  `cb:"2"`
	Counts map[string]int64   `cb:"3"`
	Ratios map[uint16]float64 `cb:"4"`
	Flags  map[string]bool    `cb:"5"`
	Empty  map[string]string  `cb:"6"`
	Tail   string             `cb:"7"`
}

func TestMapsRoundTrip(t *testing.T) {
	value := withMaps{
		Head:   7,
		Labels: map[string]string{"env": "prod", "region": "sa-east-1", "": ""},
		Counts: map[string]int64{"hits": 1 << 40, "misses": -3},
		Ratios: map[uint16]float64{1: 1.5, 2: 0.25},
		Flags:  map[string]bool{"on": true, "off": false},
		Tail:   "tail",
	}
	message, err := Marshal(&value)
	if err != nil {
		t.Fatal(err)
	}
	var back withMaps
	if err := Unmarshal(message, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, value) {
		t.Fatalf("round-tripped as %+v, want %+v", back, value)
	}
	// An empty map comes back nil, like every other zero value.
	if back.Empty != nil {
		t.Fatalf("an empty map round-tripped as %v", back.Empty)
	}
}

// A map is a composite, so it carries a byte length and a reader that does not
// know the field steps over it.
// As with any composite, skipping an unknown map needs the wide width.
type wideMaps struct {
	Head   uint32            `cb:"1"`
	Labels map[string]string `cb:"2"`
	Counts map[string]int64  `cb:"3"`
	Tail   string            `cb:"21"`
}

func TestUnknownMapIsSkipped(t *testing.T) {
	type narrower struct {
		Head uint32 `cb:"1"`
		Tail string `cb:"21"`
	}
	value := wideMaps{
		Head:   7,
		Labels: map[string]string{"env": "prod"},
		Counts: map[string]int64{"hits": 12},
		Tail:   "tail",
	}
	message, err := Marshal(&value)
	if err != nil {
		t.Fatal(err)
	}
	var back narrower
	if err := Unmarshal(message, &back); err != nil {
		t.Fatal(err)
	}
	if back.Head != 7 || back.Tail != "tail" {
		t.Fatalf("reading past two unknown maps gave %+v", back)
	}
}

// A key or value the wire has no element form for is refused, with the field
// named, rather than silently dropped.
func TestUnsupportedMapsAreRefused(t *testing.T) {
	// A nil value inside a map has no form on the wire, so a map of pointers is
	// refused where a map of structs is carried.
	type mapOfPointers struct {
		M map[string]*inner `cb:"1"`
	}
	type floatKeyed struct {
		M map[float64]string `cb:"1"`
	}
	type structKeyed struct {
		M map[inner]string `cb:"1"`
	}
	for _, value := range []any{mapOfPointers{}, floatKeyed{}, structKeyed{}} {
		if _, err := Marshal(value); err == nil {
			t.Fatalf("%T was accepted", value)
		}
	}
}

func TestMapTruncationIsRefused(t *testing.T) {
	value := withMaps{Head: 7, Labels: map[string]string{"env": "prod"}, Tail: "t"}
	checkPrefixes(t, &value, unmarshalInto[withMaps])
}

func BenchmarkMapAppend(b *testing.B) {
	value := withMaps{Head: 7,
		Labels: map[string]string{"env": "prod", "region": "sa-east-1"},
		Counts: map[string]int64{"hits": 12, "misses": 3}}
	handle := MustCodec[withMaps]()
	buffer := make([]byte, 0, 256)
	b.ReportAllocs()
	for b.Loop() {
		buffer, _ = handle.Append(buffer[:0], &value)
	}
}

func BenchmarkMapUnmarshal(b *testing.B) {
	value := withMaps{Head: 7,
		Labels: map[string]string{"env": "prod", "region": "sa-east-1"},
		Counts: map[string]int64{"hits": 12, "misses": 3}}
	handle := MustCodec[withMaps]()
	message := must(handle.Encode(&value))
	var back withMaps
	b.ReportAllocs()
	for b.Loop() {
		if err := handle.Unmarshal(message, &back); err != nil {
			b.Fatal(err)
		}
	}
}

// Maps of structs. A value is a list element behind a key, and the schema names
// its struct by index, as it does a slice of structs' element. See maps.go.

// withAny's plan is wide — it holds a dynamic value — so a narrow map holding
// it writes wide elements, which the schema has to say.
type withAny struct {
	Value any `cb:"1"`
}

type shelf struct {
	Name  string           `cb:"1"`
	Items map[uint16]inner `cb:"2"`
}

type structMaps struct {
	Head    uint32             `cb:"1"`
	ByName  map[string]inner   `cb:"2"`
	ByID    map[int64]shelf    `cb:"3"`
	Dynamic map[string]withAny `cb:"4"`
	Empty   map[string]inner   `cb:"5"`
	Tail    string             `cb:"6"`
}

// wideStructMaps is the eight-bit half of the code.
type wideStructMaps struct {
	ByName map[string]inner `cb:"1"`
	ByID   map[uint8]shelf  `cb:"2"`
	Tail   string           `cb:"30"`
}

// mapTree reaches itself through a map, which ends where a map is empty.
type mapTree struct {
	Name string             `cb:"1"`
	Kids map[string]mapTree `cb:"2"`
}

func fullStructMaps() structMaps {
	return structMaps{
		Head:   7,
		ByName: map[string]inner{"one": {ID: 1, Name: "uno"}, "zero": {}, "": {Name: "blank"}},
		ByID: map[int64]shelf{
			-5:      {Name: "low", Items: map[uint16]inner{3: {ID: 3}}},
			1 << 40: {Name: "high"},
		},
		Dynamic: map[string]withAny{"text": {Value: "v"}, "list": {Value: []any{int64(1), true}}},
		Tail:    "tail",
	}
}

func TestMapsOfStructsRoundTrip(t *testing.T) {
	original := fullStructMaps()
	var back structMaps
	roundTrip(t, &original, &back)
	if !reflect.DeepEqual(back, original) {
		t.Fatalf("round-tripped as %+v, want %+v", back, original)
	}

	wide := wideStructMaps{
		ByName: map[string]inner{"k": {ID: 9, Name: "nine"}},
		ByID:   map[uint8]shelf{2: {Items: map[uint16]inner{1: {Name: "x"}}}},
		Tail:   "t",
	}
	var wideBack wideStructMaps
	roundTrip(t, &wide, &wideBack)
	if !reflect.DeepEqual(wideBack, wide) {
		t.Fatalf("round-tripped as %+v, want %+v", wideBack, wide)
	}

	tree := mapTree{Name: "root", Kids: map[string]mapTree{
		"a": {Name: "leaf"},
		"b": {Name: "branch", Kids: map[string]mapTree{"c": {Name: "deep"}}},
	}}
	var treeBack mapTree
	roundTrip(t, &tree, &treeBack)
	if !reflect.DeepEqual(treeBack, tree) {
		t.Fatalf("round-tripped as %+v, want %+v", treeBack, tree)
	}
}

// Each entry is read into a holder reused across the map, so a field one entry
// omits must not keep the value the entry before it set.
func TestMapOfStructsEntriesDoNotLeak(t *testing.T) {
	original := structMaps{ByName: map[string]inner{
		"a": {ID: 1, Name: "full"},
		"b": {},
		"c": {Name: "name only"},
	}}
	var back structMaps
	roundTrip(t, &original, &back)
	if !reflect.DeepEqual(back.ByName, original.ByName) {
		t.Fatalf("entries leaked into each other: %+v", back.ByName)
	}
}

// Go randomises map order and the encoder sorts, so the bytes are a function of
// the value — a map of structs included.
func TestMapOfStructsIsDeterministic(t *testing.T) {
	original := fullStructMaps()
	first, err := Marshal(&original)
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		again, err := Marshal(&original)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(again, first) {
			t.Fatal("the same map of structs encoded two different ways")
		}
	}
}

// A paged type is an ordinary struct to whatever holds it, a map included.
func TestMapOfAPagedType(t *testing.T) {
	valueType := pagedType(span(1, 300))
	holder := reflect.StructOf([]reflect.StructField{{
		Name: "M", Type: reflect.MapOf(reflect.TypeFor[string](), valueType), Tag: `cb:"1"`,
	}})
	value := reflect.New(holder)
	entries := reflect.MakeMap(holder.Field(0).Type)
	entries.SetMapIndex(reflect.ValueOf("all"), pagedValue(valueType, span(1, 300)).Elem())
	entries.SetMapIndex(reflect.ValueOf("late"), pagedValue(valueType, []int{299}).Elem())
	value.Elem().Field(0).Set(entries)
	message := roundTrips(t, value)

	schema, err := SchemaOf(value.Interface())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseSchema(schema.Bytes()); err != nil {
		t.Fatalf("the section of a map of a paged type: %v", err)
	}
	document, err := ToJSON(schema, message)
	if err != nil {
		t.Fatal(err)
	}
	// One flat object per value, pages and all: the present field first, then
	// every absent one as its zero, from all three pages.
	var decoded struct{ M map[string]map[string]any }
	if err := json.Unmarshal(document, &decoded); err != nil {
		t.Fatalf("%v: %.200s", err, document)
	}
	late := decoded.M["late"]
	if len(late) != 300 || late["F299"] != 299.5 || late["F1"] != "" || late["F300"] != 0.0 {
		t.Fatalf("the pages did not merge into the map's value: %d fields, %v", len(late), late)
	}
}

// The JSON is an object of objects, and a value's omitted field renders as its
// zero, as it does anywhere else.
func TestMapOfStructsJSON(t *testing.T) {
	type small struct {
		ByName map[string]inner `cb:"1"`
		None   map[string]inner `cb:"2"`
	}
	type smallWide struct {
		ByName map[string]inner `cb:"1"`
		Far    uint8            `cb:"20"`
	}
	const want = `{"ByName":{"a":{"ID":1,"Name":"x"},"b":{"ID":0,"Name":""}},"None":null}`
	const wantWide = `{"ByName":{"a":{"ID":1,"Name":"x"},"b":{"ID":0,"Name":""}},"Far":0}`
	entries := map[string]inner{"b": {}, "a": {ID: 1, Name: "x"}}
	for _, testCase := range []struct {
		value any
		want  string
	}{
		{&small{ByName: entries}, want},
		{&smallWide{ByName: entries}, wantWide},
	} {
		message, err := MarshalSelfDescribing(testCase.value)
		if err != nil {
			t.Fatal(err)
		}
		document, err := ToJSON(nil, message)
		if err != nil {
			t.Fatal(err)
		}
		if string(document) != testCase.want {
			t.Errorf("%T:\n got %s\nwant %s", testCase.value, document, testCase.want)
		}
	}
}

func TestMapOfStructsTruncationIsRefused(t *testing.T) {
	original := fullStructMaps()
	checkPrefixes(t, &original, unmarshalInto[structMaps])
	wide := wideStructMaps{ByName: map[string]inner{"k": {ID: 9}}, Tail: "t"}
	checkPrefixes(t, &wide, unmarshalInto[wideStructMaps])
}

// The section names the value's struct after the two kinds, and a reader has
// to refuse one that is missing or points outside the table.
func TestParseSchemaRefusesABadMapStruct(t *testing.T) {
	type one struct {
		M map[string]inner `cb:"1"`
	}
	schema, err := SchemaFor[one]()
	if err != nil {
		t.Fatal(err)
	}
	good := schema.Bytes()
	// [len] [2 structs] [flags 0] [1 field] [key 0] [1 'M'] [opMap] [mapString] [mapStruct] [index 1] ...
	at := bytes.Index(good, []byte{uint8(opMap), uint8(mapString), uint8(mapStruct)})
	if at < 0 || good[at+3] != 1 {
		t.Fatalf("the fixture's section has no map of struct 1: %v", good)
	}
	pointsOut := append([]byte(nil), good...)
	pointsOut[at+3] = 9
	if _, err := ParseSchema(pointsOut); err == nil || !strings.Contains(err.Error(), "points at struct 9") {
		t.Fatalf("an index outside the table gave %v", err)
	}
	unassigned := append([]byte(nil), good...)
	unassigned[at+2] = uint8(mapKindCount)
	if _, err := ParseSchema(unassigned); err == nil || !strings.Contains(err.Error(), "does not assign") {
		t.Fatalf("an unassigned map kind gave %v", err)
	}
	for cut := range len(good) {
		if _, err := ParseSchema(good[:cut]); err == nil {
			t.Fatalf("a section truncated to %d bytes was accepted", cut)
		}
	}
}
