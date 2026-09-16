package codec

import (
	"strings"
	"testing"
)

type pointers struct {
	Count *int32   `cb:"1"`
	Name  *string  `cb:"2"`
	Ratio *float64 `cb:"3"`
	Flag  *bool    `cb:"4"`
	Tag   int32    `cb:"5"`
}

// widePointers has an id past sixteen, which is what puts it on the eight-bit
// key path — the same fields, the other half of the code.
type widePointers struct {
	Count *int32   `cb:"1"`
	Name  *string  `cb:"2"`
	Ratio *float64 `cb:"3"`
	Flag  *bool    `cb:"4"`
	Far   int32    `cb:"21"`
}

func ptr[T any](value T) *T { return &value }

func TestPointerNilIsAbsent(t *testing.T) {
	original := pointers{Tag: 7}
	data, err := Marshal(&original)
	if err != nil {
		t.Fatal(err)
	}
	var back pointers
	if err := Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Count != nil || back.Name != nil || back.Ratio != nil || back.Flag != nil {
		t.Fatalf("a nil pointer came back non-nil: %+v", back)
	}
	if back.Tag != 7 {
		t.Fatalf("Tag = %d, want 7", back.Tag)
	}
	// Nothing on the wire for four nil pointers: the root, and the one scalar.
	if len(data) != 3 {
		t.Fatalf("four nil pointers cost %d bytes, want 3 (root + one field)", len(data))
	}
}

// TestPointerToZero is the case the whole file exists for: a non-nil pointer to
// a zero value must not read back as nil.
func TestPointerToZero(t *testing.T) {
	original := pointers{
		Count: ptr(int32(0)),
		Name:  ptr(""),
		Ratio: ptr(0.0),
		Flag:  ptr(false),
	}
	var back pointers
	roundTrip(t, &original, &back)

	for name, got := range map[string]bool{
		"Count": back.Count == nil,
		"Name":  back.Name == nil,
		"Ratio": back.Ratio == nil,
		"Flag":  back.Flag == nil,
	} {
		if got {
			t.Errorf("%s: a pointer to zero came back nil", name)
		}
	}
	if back.Count == nil || *back.Count != 0 {
		t.Errorf("Count = %v, want a pointer to 0", back.Count)
	}
	if back.Name == nil || *back.Name != "" {
		t.Errorf("Name = %v, want a pointer to the empty string", back.Name)
	}
	if back.Ratio == nil || *back.Ratio != 0 {
		t.Errorf("Ratio = %v, want a pointer to 0", back.Ratio)
	}
	if back.Flag == nil || *back.Flag != false {
		t.Errorf("Flag = %v, want a pointer to false", back.Flag)
	}
}

func TestPointerToValue(t *testing.T) {
	original := pointers{
		Count: ptr(int32(-90210)),
		Name:  ptr("ordinary"),
		Ratio: ptr(1.5),
		Flag:  ptr(true),
		Tag:   3,
	}
	var back pointers
	roundTrip(t, &original, &back)

	if *back.Count != -90210 || *back.Name != "ordinary" || *back.Ratio != 1.5 || !*back.Flag {
		t.Fatalf("round trip lost a value: %d %q %v %v",
			*back.Count, *back.Name, *back.Ratio, *back.Flag)
	}
	if back.Count == original.Count {
		t.Fatal("the decoder aliased the original's pointer rather than allocating")
	}
}

// TestPointerWide runs the same three cases through the eight-bit key path.
func TestPointerWide(t *testing.T) {
	for _, original := range []widePointers{
		{Far: 7},
		{Count: ptr(int32(0)), Name: ptr(""), Ratio: ptr(0.0), Flag: ptr(false)},
		{Count: ptr(int32(11)), Name: ptr("wide"), Ratio: ptr(-2.25), Flag: ptr(true), Far: 1},
	} {
		var back widePointers
		roundTrip(t, &original, &back)
		if !samePointers(original.Count, back.Count) ||
			!samePointers(original.Name, back.Name) ||
			!samePointers(original.Ratio, back.Ratio) ||
			!samePointers(original.Flag, back.Flag) ||
			original.Far != back.Far {
			t.Errorf("wide round trip changed %+v into %+v", original, back)
		}
	}
}

// TestPointerPacked5 is the wide path with the string encoding on, which a
// *string reaches through the same descriptor a string does.
func TestPointerPacked5(t *testing.T) {
	SetPacked5(true)
	defer SetPacked5(false)

	original := widePointers{Name: ptr("ORDER-2024-XY")}
	var back widePointers
	roundTrip(t, &original, &back)
	if back.Name == nil || *back.Name != "ORDER-2024-XY" {
		t.Fatalf("Name = %v, want the packed string back", back.Name)
	}
}

// TestPointerTruncated checks that a message cut short fails rather than
// returning a pointer to a half-read value.
func TestPointerTruncated(t *testing.T) {
	data, err := Marshal(&pointers{Name: ptr("a reasonably long string")})
	if err != nil {
		t.Fatal(err)
	}
	// From two: one byte is the root descriptor and nothing else, which is a
	// message with every field absent rather than a truncated one.
	for cut := 2; cut < len(data); cut++ {
		var back pointers
		if err := Unmarshal(data[:cut], &back); err == nil {
			t.Errorf("cut to %d of %d bytes decoded without an error", cut, len(data))
		}
	}
}

// TestPointerToStructRoundTrips covers the distinction the op exists to keep: a
// nil pointer writes no key and reads back nil, and a pointer to a zero struct
// writes an empty body and reads back non-nil. Collapsing those two would make
// the pointer indistinguishable from the value.
func TestPointerToStructRoundTrips(t *testing.T) {
	type inner struct {
		A int32  `cb:"1"`
		B string `cb:"2"`
	}
	type holder struct {
		Sub  *inner `cb:"1"`
		Tail int32  `cb:"2"`
	}
	for _, sub := range []*inner{nil, {}, {A: 7, B: "seven"}} {
		original := holder{Sub: sub, Tail: 9}
		var back holder
		roundTrip(t, &original, &back)
		if back.Tail != 9 {
			t.Errorf("sub %+v: the field after the pointer read back %d", sub, back.Tail)
		}
		if (back.Sub == nil) != (sub == nil) {
			t.Fatalf("sub %+v read back as %+v: nil-ness was not preserved", sub, back.Sub)
		}
		if sub != nil && *back.Sub != *sub {
			t.Errorf("sub %+v read back as %+v", *sub, *back.Sub)
		}
	}
}

// TestPointerToStructJSON is the same distinction seen by a reader that has only
// the schema section: nil is null, and a pointer to a zero struct is an object
// of zeros. A field the message omits is rendered after the ones it carries,
// which is why the nil case puts Sub last.
func TestPointerToStructJSON(t *testing.T) {
	type inner struct {
		A int32  `cb:"1"`
		B string `cb:"2"`
	}
	type holder struct {
		Sub  *inner `cb:"1"`
		Tail int32  `cb:"2"`
	}
	for _, want := range []struct {
		value holder
		json  string
	}{
		{holder{Sub: nil, Tail: 9}, `{"Tail":9,"Sub":null}`},
		{holder{Sub: &inner{}, Tail: 9}, `{"Sub":{"A":0,"B":""},"Tail":9}`},
		{holder{Sub: &inner{A: 7, B: "x"}, Tail: 9}, `{"Sub":{"A":7,"B":"x"},"Tail":9}`},
	} {
		message, err := MarshalSelfDescribing(&want.value)
		if err != nil {
			t.Fatalf("%+v: %v", want.value, err)
		}
		out, err := ToJSON(nil, message)
		if err != nil {
			t.Fatalf("%+v: %v", want.value, err)
		}
		if string(out) != want.json {
			t.Errorf("%+v:\n got %s\nwant %s", want.value, out, want.json)
		}
	}
}

// A pointer to a struct is carried; a pointer to a slice or a map is not, since
// a nil and an empty one are the same thing on this wire. See pointer.go.
func TestPointerToSliceOrMapIsRefused(t *testing.T) {
	type toSlice struct {
		Sub *[]int32 `cb:"1"`
	}
	type toMap struct {
		Sub *map[string]int32 `cb:"1"`
	}
	if _, err := Marshal(&toSlice{}); err == nil {
		t.Error("a pointer to a slice was accepted; it has no form on the wire yet")
	}
	if _, err := Marshal(&toMap{}); err == nil {
		t.Error("a pointer to a map was accepted; it has no form on the wire yet")
	}
}

// TestUncarriableFieldIsNamed is the regression for an error that named the
// wrong type. A struct whose field the format cannot carry used to come back
// from compositeOpFor indistinguishable from "not a composite", so the caller
// fell through to the scalar table and reported that []holder was not a
// carriable slice — naming the one type in the message that was fine.
func TestUncarriableFieldIsNamed(t *testing.T) {
	type holder struct {
		Lookup map[string]*int32 `cb:"1"`
	}
	_, err := Marshal([]holder{{}})
	if err == nil {
		t.Fatal("a map of pointers was accepted")
	}
	if !strings.Contains(err.Error(), "Lookup") {
		t.Errorf("the error does not name the field that could not be carried: %v", err)
	}
}

func samePointers[T comparable](want, got *T) bool {
	if want == nil || got == nil {
		return want == nil && got == nil
	}
	return *want == *got
}

func roundTrip[T any](t *testing.T, original, back *T) {
	t.Helper()
	data, err := Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if err := Unmarshal(data, back); err != nil {
		t.Fatal(err)
	}
}
