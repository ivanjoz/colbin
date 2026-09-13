package codec

import (
	"reflect"
	"strings"
	"testing"
)

type grant struct {
	AccesoID int32 `cb:"1"`
	Nivel    int8  `cb:"2"`
}

// The case the envelope exists for: a caller holding a slice of records, which is
// what an ORM keeping a complex column as one blob has in its hand.
func TestRoundTripsASliceAtTheRoot(t *testing.T) {
	grants := []grant{{AccesoID: 10, Nivel: 4}, {AccesoID: 3, Nivel: 1}}

	data, err := Marshal(grants)
	if err != nil {
		t.Fatal(err)
	}

	var back []grant
	if err := Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, grants) {
		t.Fatalf("round trip gave %+v, want %+v", back, grants)
	}
}

// The point of wrapping rather than adding a root shape: the blob is an ordinary
// message, so every reader that exists already — the Rust port, the browser
// module — parses it without being taught anything.
func TestASliceRootIsAnOrdinaryStructMessage(t *testing.T) {
	data, err := Marshal([]grant{{AccesoID: 10, Nivel: 4}})
	if err != nil {
		t.Fatal(err)
	}
	if !IsColbin(data) {
		t.Fatalf("first byte %#x is outside the reserved root range", data[0])
	}

	// Byte for byte what the hand-written wrapper writes, which is the claim the
	// envelope makes and the one thing that would silently rot.
	type wrapper struct {
		Value []grant `cb:"0"`
	}
	wrapped, err := Marshal(wrapper{Value: []grant{{AccesoID: 10, Nivel: 4}}})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(wrapped) {
		t.Fatalf("envelope wrote %x, the explicit wrapper wrote %x", data, wrapped)
	}
}

// A map root is the other container a caller genuinely holds.
func TestRoundTripsAMapAtTheRoot(t *testing.T) {
	counts := map[string]int32{"a": 1, "b": 2}

	data, err := Marshal(counts)
	if err != nil {
		t.Fatal(err)
	}
	back := map[string]int32{}
	if err := Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, counts) {
		t.Fatalf("round trip gave %+v, want %+v", back, counts)
	}
}

// A lone scalar is refused rather than wrapped: a message carrying one unnamed
// number is a byte array with extra steps, and the error belongs at the call
// site rather than in whatever reads the blob back.
func TestRefusesAScalarAtTheRoot(t *testing.T) {
	if _, err := Marshal(42); err == nil {
		t.Fatal("an int was accepted as a message root")
	} else if !strings.Contains(err.Error(), "the format encodes a struct") {
		t.Fatalf("error does not say what a root may be: %v", err)
	}
}

// An envelope whose value the format cannot carry must name the type the caller
// passed, not the synthetic field they never wrote.
func TestASliceRootErrorNamesTheCallersType(t *testing.T) {
	_, err := Marshal([]complex128{1 + 2i})
	if err == nil {
		t.Fatal("a slice of an uncarriable type was accepted")
	}
	if !strings.Contains(err.Error(), "[]complex128") {
		t.Fatalf("error names the envelope rather than the argument: %v", err)
	}
}
