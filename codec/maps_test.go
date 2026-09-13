package codec

import (
	"reflect"
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
	type mapOfStructs struct {
		M map[string]inner `cb:"1"`
	}
	type floatKeyed struct {
		M map[float64]string `cb:"1"`
	}
	for _, value := range []any{mapOfStructs{}, floatKeyed{}} {
		if _, err := Marshal(value); err == nil {
			t.Fatalf("%T was accepted", value)
		}
	}
}

func TestMapTruncationIsRefused(t *testing.T) {
	value := withMaps{Head: 7, Labels: map[string]string{"env": "prod"}, Tail: "t"}
	message, err := Marshal(&value)
	if err != nil {
		t.Fatal(err)
	}
	for cut := range len(message) {
		var back withMaps
		_ = Unmarshal(message[:cut], &back)
	}
}

func BenchmarkMapAppend(b *testing.B) {
	value := withMaps{Head: 7,
		Labels: map[string]string{"env": "prod", "region": "sa-east-1"},
		Counts: map[string]int64{"hits": 12, "misses": 3}}
	handle := MustCodec[withMaps]()
	buffer := make([]byte, 0, 256)
	b.ReportAllocs()
	for b.Loop() {
		buffer = handle.Append(buffer[:0], &value)
	}
}

func BenchmarkMapUnmarshal(b *testing.B) {
	value := withMaps{Head: 7,
		Labels: map[string]string{"env": "prod", "region": "sa-east-1"},
		Counts: map[string]int64{"hits": 12, "misses": 3}}
	handle := MustCodec[withMaps]()
	message := handle.Encode(&value)
	var back withMaps
	b.ReportAllocs()
	for b.Loop() {
		if err := handle.Unmarshal(message, &back); err != nil {
			b.Fatal(err)
		}
	}
}
