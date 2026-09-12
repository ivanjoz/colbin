package codec

import (
	"testing"
)

type pointers struct {
	Count *int32   `cb:"0"`
	Name  *string  `cb:"1"`
	Ratio *float64 `cb:"2"`
	Flag  *bool    `cb:"3"`
	Tag   int32    `cb:"4"`
}

// widePointers has an id past fifteen, which is what puts it on the eight-bit
// key path — the same fields, the other half of the code.
type widePointers struct {
	Count *int32   `cb:"0"`
	Name  *string  `cb:"1"`
	Ratio *float64 `cb:"2"`
	Flag  *bool    `cb:"3"`
	Far   int32    `cb:"20"`
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

func TestPointerToCompositeIsRefused(t *testing.T) {
	type inner struct {
		A int32 `cb:"0"`
	}
	type holder struct {
		Sub *inner `cb:"0"`
	}
	if _, err := Marshal(&holder{}); err == nil {
		t.Fatal("a pointer to a struct was accepted; it has no form on the wire yet")
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
