package codec

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

type grant struct {
	AccesoID int32 `cb:"2"`
	Nivel    int8  `cb:"3"`
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
		Value []grant `cb:"1"`
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

// A root envelope has to describe itself, which is the delivery a browser uses:
// one message, no schema agreed in advance.
func TestASliceRootDescribesItself(t *testing.T) {
	grants := []grant{{AccesoID: 10, Nivel: 4}, {AccesoID: 3, Nivel: 1}}

	data, err := MarshalSelfDescribing(grants)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ToJSON(nil, data)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(grants)
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, out, want)

	// The body behind the section is still an ordinary message, so the Go type
	// reads it back — the property MarshalSelfDescribing promises.
	var back []grant
	if err := Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, grants) {
		t.Fatalf("round trip gave %+v, want %+v", back, grants)
	}
}

func TestAMapRootDescribesItself(t *testing.T) {
	counts := map[string]int32{"a": 1, "b": 2}

	data, err := MarshalSelfDescribing(counts)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ToJSON(nil, data)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(counts)
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, out, want)
}

// The wrapper is framing, not content. A reader without the Go type has to see
// the array a Go caller sees, and not the one-field struct that carried it.
func TestTheEnvelopeIsNotInTheDocument(t *testing.T) {
	grants := []grant{{AccesoID: 10, Nivel: 4}}

	// Both deliveries: the section sent out of band, and the one in front of the
	// message. They are the same plan and must reach the same document.
	outOfBand := toJSON(t, grants)
	if outOfBand[0] != '[' {
		t.Fatalf("out-of-band schema gave %s, want an array", outOfBand)
	}
	if strings.Contains(string(outOfBand), envelopeFieldName) {
		t.Fatalf("the synthetic field reached the document: %s", outOfBand)
	}

	data, err := MarshalSelfDescribing(grants)
	if err != nil {
		t.Fatal(err)
	}
	standalone, err := ToJSON(nil, data)
	if err != nil {
		t.Fatal(err)
	}
	if string(standalone) != string(outOfBand) {
		t.Fatalf("self-describing gave %s, out-of-band gave %s", standalone, outOfBand)
	}
}

// The other half of the same claim: a one-field struct somebody actually wrote
// is a document with one field in it.
//
// The type below is the envelope down to the field's name and id, so the two
// messages are byte for byte identical and the two sections differ in one bit.
// That bit is the whole of what makes one framing and the other content, which
// is worth pinning: reading it off the shape instead — "a one-field struct
// called rows" — would unwrap this caller's type as well.
func TestAWrapperTypeIsStillAnObject(t *testing.T) {
	type wrapper struct {
		Value []grant `cb:"rows,1"`
	}
	grants := []grant{{AccesoID: 10, Nivel: 4}}

	declared, err := Marshal(wrapper{Value: grants})
	if err != nil {
		t.Fatal(err)
	}
	enveloped, err := Marshal(grants)
	if err != nil {
		t.Fatal(err)
	}
	if string(declared) != string(enveloped) {
		t.Fatalf("the bodies differ: declared %x, envelope %x", declared, enveloped)
	}

	out := toJSON(t, wrapper{Value: grants})
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if _, ok := back[envelopeFieldName]; !ok {
		t.Fatalf("a declared one-field struct was unwrapped: %s", out)
	}
}

// DecodeAny walks the same plan, so the wrapper has to be gone there too — a
// caller asking for the value gets the slice, not a map holding one.
func TestDecodeAnyUnwrapsTheEnvelope(t *testing.T) {
	data, err := MarshalSelfDescribing([]grant{{AccesoID: 10, Nivel: 4}})
	if err != nil {
		t.Fatal(err)
	}
	value, err := DecodeAny(nil, data)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := value.([]any); !ok {
		t.Fatalf("DecodeAny gave %T, want []any", value)
	}
}

// The flag is format, so it has to survive the round trip through bytes that a
// reader in another language makes.
func TestTheEnvelopeFlagSurvivesTheSection(t *testing.T) {
	schema, err := SchemaOf([]grant{})
	if err != nil {
		t.Fatal(err)
	}
	if !schema.plan.envelope {
		t.Fatal("the plan for a slice root is not marked an envelope")
	}
	parsed, err := ParseSchema(schema.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.plan.envelope {
		t.Fatal("the envelope flag did not survive the section")
	}

	// And it is off for an ordinary type, which is the half that would rot
	// silently if the bit were simply always set.
	plain, err := SchemaOf(grant{})
	if err != nil {
		t.Fatal(err)
	}
	if plain.plan.envelope {
		t.Fatal("a declared struct is marked an envelope")
	}
}

// A scalar root is refused by the schema entry points for the same reason
// Marshal refuses it, and with the same error.
func TestSchemaRefusesAScalarRoot(t *testing.T) {
	if _, err := SchemaOf(42); err == nil {
		t.Fatal("an int was accepted as a schema root")
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
