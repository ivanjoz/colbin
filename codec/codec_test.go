package codec

import (
	"bytes"
	"strings"
	"testing"
)

type charge struct {
	CompanyID    int32  `cb:"0"`
	UserID       int32  `cb:"1"`
	RouteID      uint16 `cb:"2"`
	CPU          uint16 `cb:"3"`
	Inference    uint16 `cb:"4"`
	ExtraAllowed bool   `cb:"5"`
	Access1      uint16 `cb:"6"`
	Access2      uint16 `cb:"7"`
}

type everyShape struct {
	Flag    bool     `cb:"0"`
	Small   int8     `cb:"1"`
	Medium  int16    `cb:"2"`
	Wide    int64    `cb:"3"`
	Counted uint32   `cb:"4"`
	Ratio   float64  `cb:"5"`
	Single  float32  `cb:"6"`
	Name    string   `cb:"7"`
	Blob    []byte   `cb:"8"`
	IDs     []int32  `cb:"9"`
	Grants  []uint16 `cb:"10"`
	Longs   []int64  `cb:"11"`
	Words   []string `cb:"12"`
	Tiny    []int8   `cb:"13"`
	Huge    []uint64 `cb:"14"`
	Counts  []uint32 `cb:"15"`
}

func TestRoundTripsEveryShape(t *testing.T) {
	record := everyShape{
		Flag:    true,
		Small:   -7,
		Medium:  -300,
		Wide:    1_767_225_600_123,
		Counted: 4_000_000_000,
		Ratio:   3.141592653589793,
		Single:  1.5,
		Name:    "responses.go:539",
		Blob:    []byte{0x00, 0x01, 0xFF},
		IDs:     []int32{1_234_567, -7_654_321},
		Grants:  []uint16{0x0139, 0x008B},
		Longs:   []int64{1 << 40, -1},
		Words:   []string{"uno", "", "tres"},
		Tiny:    []int8{-1, 2},
		Huge:    []uint64{1 << 63},
		Counts:  []uint32{7},
	}
	message, err := Marshal(&record)
	if err != nil {
		t.Fatal(err)
	}
	back := everyShape{}
	if err := Unmarshal(message, &back); err != nil {
		t.Fatal(err)
	}
	if back.Flag != record.Flag || back.Small != record.Small || back.Medium != record.Medium ||
		back.Wide != record.Wide || back.Counted != record.Counted || back.Ratio != record.Ratio ||
		back.Single != record.Single || back.Name != record.Name {
		t.Fatalf("scalars round-tripped as %+v", back)
	}
	if !bytes.Equal(back.Blob, record.Blob) {
		t.Fatalf("blob round-tripped as %v", back.Blob)
	}
	if len(back.IDs) != 2 || back.IDs[1] != -7_654_321 ||
		len(back.Grants) != 2 || back.Grants[0] != 0x0139 ||
		len(back.Longs) != 2 || back.Longs[0] != 1<<40 || back.Longs[1] != -1 ||
		len(back.Tiny) != 2 || back.Tiny[0] != -1 ||
		len(back.Huge) != 1 || back.Huge[0] != 1<<63 ||
		len(back.Counts) != 1 || back.Counts[0] != 7 {
		t.Fatalf("arrays round-tripped as %+v", back)
	}
	if len(back.Words) != 3 || back.Words[0] != "uno" || back.Words[1] != "" || back.Words[2] != "tres" {
		t.Fatalf("strings round-tripped as %q", back.Words)
	}
}

// A value and a pointer to it must encode identically: a caller should not have
// to know that the plan reads fields by offset.
func TestTakesAValueOrAPointer(t *testing.T) {
	record := charge{CompanyID: 7, UserID: 42, RouteID: 103, CPU: 5, Access1: 0x0139}
	byValue, err := Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	byPointer, err := Marshal(&record)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(byValue, byPointer) {
		t.Fatalf("value %x and pointer %x disagree", byValue, byPointer)
	}
}

// Absence is what a zero value is written as, so a decode has to clear the
// destination rather than leave whatever it held.
func TestClearsFieldsTheMessageOmits(t *testing.T) {
	message, err := Marshal(&charge{CompanyID: 7})
	if err != nil {
		t.Fatal(err)
	}
	back := charge{UserID: 999, ExtraAllowed: true, Access2: 5}
	if err := Unmarshal(message, &back); err != nil {
		t.Fatal(err)
	}
	if back != (charge{CompanyID: 7}) {
		t.Fatalf("omitted fields survived the decode: %+v", back)
	}
}

func TestAppendsOntoTheCallersBuffer(t *testing.T) {
	record := charge{CompanyID: 7, UserID: 42}
	buffer := make([]byte, 0, 64)
	buffer, err := Append(buffer, &record)
	if err != nil {
		t.Fatal(err)
	}
	first := len(buffer)
	buffer, err = Append(buffer[:0], &record)
	if err != nil {
		t.Fatal(err)
	}
	if len(buffer) != first {
		t.Fatalf("a reused buffer produced %d bytes, first pass produced %d", len(buffer), first)
	}
	if cap(buffer) != 64 {
		t.Fatalf("the caller's capacity was not kept: %d", cap(buffer))
	}
}

type untagged struct {
	CompanyID int32
}

type tooManyFields struct {
	First int32 `cb:"0"`
	Last  int32 `cb:"16"`
}

type clashing struct {
	First int32 `cb:"3"`
	Other int32 `cb:"3"`
}

type nested struct {
	Inner charge `cb:"0"`
}

// A pointer to a scalar is carried — see pointer.go. A pointer to a composite
// is not: a composite already expresses absence with a length, and nothing has
// asked what a nil one should mean.
type pointerToComposite struct {
	Values *[]int32 `cb:"0"`
}

// Every refusal names the field and says what to do about it, because each one is
// an edit the caller has to make rather than a runtime condition.
func TestRefusesTypesItCannotCarry(t *testing.T) {
	for _, testCase := range []struct {
		value any
		wants string
	}{
		{clashing{}, "id 3 is on both"},
		{pointerToComposite{}, "a pointer to []int32 is not carried"},
		{42, "encodes a struct"},
	} {
		_, err := Marshal(testCase.value)
		if err == nil {
			t.Fatalf("%T was accepted", testCase.value)
		}
		if !strings.Contains(err.Error(), testCase.wants) {
			t.Fatalf("%T said %q, wanted it to mention %q", testCase.value, err, testCase.wants)
		}
	}
}

// Seventeen fields is no longer a refusal: the ids past fifteen put the message
// on the wide path, which is what the wide path is for. It is refused only past
// 256, where a one-byte key runs out.
func TestManyFieldsGoWideRatherThanBeingRefused(t *testing.T) {
	message, err := Marshal(tooManyFields{})
	if err != nil {
		t.Fatalf("seventeen fields was refused: %v", err)
	}
	if message[0] != rootStructWide {
		t.Fatalf("root descriptor is %#02x, want the wide one %#02x", message[0], rootStructWide)
	}
	var back tooManyFields
	if err := Unmarshal(message, &back); err != nil {
		t.Fatal(err)
	}
}

// The plan is cached per type, so the second refusal has to say the same thing as
// the first rather than succeeding or panicking on a cached error.
func TestCachesRefusalsToo(t *testing.T) {
	first, second := "", ""
	if _, err := Marshal(clashing{}); err != nil {
		first = err.Error()
	}
	if _, err := Marshal(clashing{}); err != nil {
		second = err.Error()
	}
	if first == "" || first != second {
		t.Fatalf("refusals disagree: %q then %q", first, second)
	}
}

func TestRefusesAKeyTheTypeDoesNotDeclare(t *testing.T) {
	message, err := Marshal(&everyShape{Counts: []uint32{7}})
	if err != nil {
		t.Fatal(err)
	}
	back := charge{}
	err = Unmarshal(message, &back)
	if err == nil || !strings.Contains(err.Error(), "does not declare") {
		t.Fatalf("a foreign key gave %v", err)
	}
}

func TestFieldIDsReportsTheWireKeys(t *testing.T) {
	ids, err := FieldIDs(charge{})
	if err != nil {
		t.Fatal(err)
	}
	if ids["CompanyID"] != 0 || ids["Access2"] != 7 || len(ids) != 8 {
		t.Fatalf("ids are %v", ids)
	}
}

// The format carries no mode byte, so the other two must not be handed one by
// accident: Unmarshal has to refuse rather than misread it.
func TestUnmarshalRefusesGarbageInsteadOfPanicking(t *testing.T) {
	messages := [][]byte{
		{},
		{0x02},
		{0x04},
		{0x06},
		{0x08},
		{0x04, 0xFF},
		{0x08, 0x7F},
		{0x04, 0x7F, 0x01, 0x02},
		{0x02, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
		{0x08, 0x07, 0x18, 0x2A},
	}
	message, err := Marshal(&charge{CompanyID: 7, UserID: 42, Access2: 9})
	if err != nil {
		t.Fatal(err)
	}
	messages = append(messages, message)
	for cut := range len(message) {
		messages = append(messages, message[:cut])
	}

	for _, data := range messages {
		func() {
			defer func() {
				if panicked := recover(); panicked != nil {
					t.Fatalf("Unmarshal(%x) panicked: %v", data, panicked)
				}
			}()
			back := charge{}
			_ = Unmarshal(data, &back)
		}()
	}
}
