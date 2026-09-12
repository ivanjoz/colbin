package codec

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/ivanjoz/colbin/wire"
)

// encoding/json over the same value is the reference throughout this file. There
// is no other one worth having: the whole claim of the feature is that a reader
// without the Go type gets what a reader with it would have got.

// toJSON encodes a value, describes its type, and walks the message back out as
// JSON — the out-of-band delivery, which is the one to document first.
func toJSON(t *testing.T, value any) []byte {
	t.Helper()
	message, err := Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := SchemaOf(value)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ToJSON(schema, message)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// sameJSON compares two documents by value, so that key order does not matter.
// It cannot: colbin writes the fields a record carries and omits the rest, so
// the ones that were absent come out at the end.
func sameJSON(t *testing.T, got, want []byte) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("the walk produced invalid JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON mismatch\n got %s\nwant %s", got, want)
	}
}

func matchesEncodingJSON(t *testing.T, value any) {
	t.Helper()
	want, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, toJSON(t, value), want)
}

// Phase 3: the flat record, which is most of a REST payload.
func TestJSONFlatRecords(t *testing.T) {
	full := everyShape{
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
	matchesEncodingJSON(t, &full)
	matchesEncodingJSON(t, &everyShape{})
	matchesEncodingJSON(t, &charge{CompanyID: 7, UserID: 42, Access2: 9})
	// A derived-id type, which is the wide key path.
	matchesEncodingJSON(t, &bare{
		SensorID: 9124, Timestamp: 1767225600123, Value: 21.5,
		Unit: "C", Samples: []int32{21, 22, 21}, Valid: true,
	})
}

// A record with every field present comes out in declaration order, so it can be
// compared byte for byte — which is what pins the number spelling, the escaping
// and the base64 rather than merely their meaning.
func TestJSONIsByteForByteEncodingJSON(t *testing.T) {
	value := &everyShape{
		Flag: true, Small: -7, Medium: -300, Wide: 1 << 40, Counted: 4_000_000_000,
		Ratio: 3.141592653589793, Single: 1.5,
		Name: `a "quoted" <tag> & an ampersand`,
		Blob: []byte{0x00, 0x01, 0xFF},
		IDs:  []int32{1}, Grants: []uint16{2}, Longs: []int64{3},
		Words: []string{"uno"}, Tiny: []int8{4}, Huge: []uint64{1 << 63},
		Counts: []uint32{7},
	}
	want, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if got := toJSON(t, value); string(got) != string(want) {
		t.Fatalf("byte for byte:\n got %s\nwant %s", got, want)
	}
}

// Omission is the encoding of a zero value, so a record that writes two fields
// still has eight and the JSON has to say so.
func TestJSONFillsInWhatTheMessageOmitted(t *testing.T) {
	out := toJSON(t, &charge{CompanyID: 7})
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if len(back) != 8 {
		t.Fatalf("%d keys, want all 8: %s", len(back), out)
	}
	if back["UserID"] != float64(0) || back["ExtraAllowed"] != false {
		t.Fatalf("an omitted field did not come back as its zero: %s", out)
	}
}

// Phase 4: a nested struct and a list of them, at both key widths.
func TestJSONNestedStructs(t *testing.T) {
	matchesEncodingJSON(t, &outer{
		Head: 7,
		One:  inner{ID: 1, Name: "one"},
		Many: []inner{{ID: 2, Name: "two"}, {ID: 300}, {}},
		Tail: "tail",
	})
	matchesEncodingJSON(t, &outer{})
	matchesEncodingJSON(t, &wideOuter{
		Head: 7,
		One:  inner{ID: 1, Name: "one"},
		Many: []inner{{ID: 2, Name: "two"}},
		Tail: "tail",
	})
}

// basket puts its slice first so that a test can look straight at the field's
// descriptor and see which layout the encoder chose.
type basket struct {
	Lines []line `cb:"1"`
	ID    uint64 `cb:"2"`
}

type line struct {
	ProductID uint32 `cb:"1"`
	Quantity  uint32 `cb:"2"`
	UnitCents int64  `cb:"3"`
	Note      string `cb:"4"`
}

// wideBasket is the same thing with an id past fifteen, which is what puts the
// message on the eight-bit key path.
type wideBasket struct {
	Lines []line `cb:"1"`
	ID    uint64 `cb:"20"`
}

func lines(count int) []line {
	if count == 0 {
		// An empty slice and a nil one are the same absence on this wire, so the
		// reference value has to be the one the format can express. See
		// TestJSONCannotTellAnEmptySliceFromANilOne.
		return nil
	}
	built := make([]line, count)
	for index := range built {
		built[index] = line{
			ProductID: uint32(1000 + index),
			UnitCents: int64(199 + index*100),
			Note:      "note",
		}
		if index%3 == 0 {
			built[index].Note = ""
		}
	}
	return built
}

// columnar reports whether the message wrote its first field as a table.
func columnar(t *testing.T, message []byte) bool {
	t.Helper()
	body, wide, ok := rootOf(message)
	if !ok {
		t.Fatalf("root byte %#02x", message[0])
	}
	if wide {
		reader := wire.NewReader8(body)
		return reader.IsTable()
	}
	reader := wire.NewReader(body)
	return reader.IsTable()
}

// Phases 4 and 5 together: the same rows, both layouts, one answer.
//
// Quantity is left zero in every row on purpose. A column of nothing but zeros
// is not written at all, so the table path has to put the zeros back from the
// column's *absence* — which is the one thing the list path never has to do.
func TestJSONListAndTableAgreeWithEncodingJSON(t *testing.T) {
	for _, count := range []int{0, 1, 4, tableThreshold - 1, tableThreshold, 40} {
		value := &basket{ID: 9, Lines: lines(count)}
		message, err := Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		// An empty slice is not written at all, so there is no descriptor to
		// look at and nothing to assert about the layout.
		if wantTable := count >= tableThreshold; count > 0 && columnar(t, message) != wantTable {
			t.Fatalf("%d lines: columnar=%v, want %v", count, !wantTable, wantTable)
		}
		matchesEncodingJSON(t, value)
		matchesEncodingJSON(t, &wideBasket{ID: 9, Lines: lines(count)})
	}
}

// The one place the JSON and encoding/json disagree, written down rather than
// left to be discovered: an empty slice and a nil slice are the same absence on
// this wire, so both come back as null. Unmarshal into the Go type says the same
// thing, which is what makes null the honest answer rather than a shortcut.
func TestJSONCannotTellAnEmptySliceFromANilOne(t *testing.T) {
	empty := &basket{ID: 9, Lines: []line{}}
	out := toJSON(t, empty)
	if !strings.Contains(string(out), `"Lines":null`) {
		t.Fatalf("an empty slice came out as %s", out)
	}
	var back basket
	if err := Unmarshal(mustMarshal(t, empty), &back); err != nil {
		t.Fatal(err)
	}
	if back.Lines != nil {
		t.Fatalf("Unmarshal disagrees: it gave %#v", back.Lines)
	}
}

// Phase 6, floats: a scalar float travels byte-reversed and a column float does
// not, so reading either as the other is silent nonsense rather than an error.
// The table path is what this is really testing.
type readings struct {
	Rows []reading `cb:"1"`
	Name string    `cb:"2"`
}

type reading struct {
	Lat   float64 `cb:"1"`
	Lon   float64 `cb:"2"`
	Level float32 `cb:"3"`
}

func TestJSONFloatsThroughBothPaths(t *testing.T) {
	rows := make([]reading, 12)
	for index := range rows {
		rows[index] = reading{
			Lat:   -33.45694 + float64(index)/100,
			Lon:   70.64827,
			Level: float32(index) / 4,
		}
	}
	table := &readings{Name: "santiago", Rows: rows}
	if !columnar(t, mustMarshal(t, table)) {
		t.Fatal("twelve rows did not take the columnar path")
	}
	matchesEncodingJSON(t, table)
	// The same rows short of the threshold, which is the row-wise path.
	matchesEncodingJSON(t, &readings{Name: "santiago", Rows: rows[:3]})
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	message, err := Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

// A NaN is refused rather than quietly turned into null, and DecodeAny keeps it.
// Turning "not a number" into "no value" loses a distinction the record made.
func TestJSONRefusesNonFiniteFloats(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		record := &everyShape{Ratio: value}
		message, schema := mustMarshal(t, record), mustSchema(t, record)
		if _, err := ToJSON(schema, message); err == nil ||
			!strings.Contains(err.Error(), "no JSON spelling") {
			t.Fatalf("%v gave %v", value, err)
		}
		// Nothing is written before the refusal: a caller's buffer is either the
		// whole document or what it was.
		buffer := []byte("prefix")
		if out, err := AppendJSON(buffer, schema, message); err == nil {
			t.Fatalf("%v was accepted: %s", value, out)
		}
		if string(buffer) != "prefix" {
			t.Fatalf("the caller's buffer was left as %q", buffer)
		}
		decoded, err := DecodeAny(schema, message)
		if err != nil {
			t.Fatalf("DecodeAny(%v): %v", value, err)
		}
		got := decoded.(map[string]any)["Ratio"].(float64)
		if math.IsNaN(value) != math.IsNaN(got) || (!math.IsNaN(value) && got != value) {
			t.Fatalf("DecodeAny gave %v, want %v", got, value)
		}
	}
}

func mustSchema(t *testing.T, value any) *Schema {
	t.Helper()
	schema, err := SchemaOf(value)
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

// Phase 6, pointers: nil is null, and a pointer to a zero is that zero — which
// is the distinction pointer.go spends an explicit zero on the wire to keep.
func TestJSONPointers(t *testing.T) {
	for _, value := range []any{
		&pointers{Tag: 7},
		&pointers{Count: ptr(int32(0)), Name: ptr(""), Ratio: ptr(0.0), Flag: ptr(false)},
		&pointers{Count: ptr(int32(-5)), Name: ptr("set"), Ratio: ptr(2.5), Flag: ptr(true)},
		&widePointers{Far: 7},
		&widePointers{Count: ptr(int32(0)), Name: ptr(""), Ratio: ptr(0.0), Flag: ptr(false)},
		&widePointers{Count: ptr(int32(-5)), Name: ptr("set"), Ratio: ptr(2.5), Flag: ptr(true)},
	} {
		matchesEncodingJSON(t, value)
	}
	// Spelled out, because "null" and "0" being different is the whole point.
	var back map[string]any
	if err := json.Unmarshal(toJSON(t, &pointers{Count: ptr(int32(0))}), &back); err != nil {
		t.Fatal(err)
	}
	if back["Count"] != float64(0) {
		t.Fatalf("a pointer to zero came out as %v, want 0", back["Count"])
	}
	if back["Name"] != nil {
		t.Fatalf("a nil pointer came out as %v, want null", back["Name"])
	}
}

// Phase 6, maps. A map does not round-trip to identical JSON bytes — Go map
// iteration has no order — so this compares by value, which is also all a JSON
// object promises.
func TestJSONMaps(t *testing.T) {
	matchesEncodingJSON(t, &withMaps{
		Head:   7,
		Labels: map[string]string{"env": "prod", "region": "sa-east-1", "": ""},
		Counts: map[string]int64{"hits": 1 << 40, "misses": -3},
		Ratios: map[uint16]float64{1: 1.5, 2: 0.25},
		Flags:  map[string]bool{"on": true, "off": false},
		Tail:   "tail",
	})
	matchesEncodingJSON(t, &withMaps{})

	// A JSON object key is a string, so an integer key is rendered as one —
	// which is what encoding/json does as well, and why the comparison above
	// holds at all.
	out := toJSON(t, &withMaps{Ratios: map[uint16]float64{42: 1.5}})
	if !strings.Contains(string(out), `"42":1.5`) {
		t.Fatalf("an integer map key came out as %s", out)
	}
}

// A map of float32 values is the case that needs the width in the schema: the
// wire carries a 32-bit reversed pattern, and nothing but the section says so.
func TestJSONFloat32MapValues(t *testing.T) {
	type ratios struct {
		Narrow map[string]float32 `cb:"1"`
		Wide   map[string]float64 `cb:"2"`
	}
	matchesEncodingJSON(t, &ratios{
		Narrow: map[string]float32{"a": 1.1},
		Wide:   map[string]float64{"b": 1.1},
	})
}

// packed5 is a per-field encoding code, so the walk reads either form with no
// setting of its own. It is on the wide path only: a narrow descriptor has no
// room for the code.
func TestJSONReadsPackedStrings(t *testing.T) {
	SetPacked5(true)
	defer SetPacked5(false)
	value := &bare{Unit: "ABC-123", SensorID: 9124}
	message := mustMarshal(t, value)
	schema := mustSchema(t, value)
	want, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ToJSON(schema, message)
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, out, want)
}

// Phase 7: a message that carries its own schema needs none passed in.
func TestJSONFromASelfDescribingMessage(t *testing.T) {
	value := &basket{ID: 9, Lines: lines(12)}
	message, err := MarshalSelfDescribing(value)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ToJSON(nil, message)
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, out, want)

	// The schema may also be passed in for a message that carries one, which is
	// what a reader that already has it would do.
	withBoth, err := ToJSON(mustSchema(t, value), message)
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, withBoth, want)

	// Without a schema and without a section there is nothing to go on, and the
	// error says which of the two to fix.
	if _, err := ToJSON(nil, mustMarshal(t, value)); err == nil ||
		!strings.Contains(err.Error(), "MarshalSelfDescribing") {
		t.Fatalf("a schemaless message gave %v", err)
	}
}

// Phase 8: DecodeAny, on the same walk.
func TestDecodeAnyShapes(t *testing.T) {
	value := &everyShape{
		Flag: true, Small: -7, Wide: 1 << 40, Counted: 4_000_000_000,
		Ratio: 2.5, Single: 1.5, Name: "uno", Blob: []byte{1, 2, 3},
		IDs: []int32{1, -2}, Words: []string{"a", "b"}, Huge: []uint64{1 << 63},
	}
	decoded, err := DecodeAny(mustSchema(t, value), mustMarshal(t, value))
	if err != nil {
		t.Fatal(err)
	}
	record, ok := decoded.(map[string]any)
	if !ok {
		t.Fatalf("DecodeAny gave %T, want a map", decoded)
	}
	for name, want := range map[string]any{
		"Flag":    true,
		"Small":   int64(-7),
		"Wide":    int64(1 << 40),
		"Counted": uint64(4_000_000_000),
		"Ratio":   2.5,
		"Single":  1.5,
		"Name":    "uno",
		"Medium":  int64(0), // absent, and therefore zero
		"Tiny":    nil,      // absent, and therefore nil
	} {
		if got := record[name]; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s = %#v (%T), want %#v", name, got, got, want)
		}
	}
	if got := record["Blob"].([]byte); !reflect.DeepEqual(got, []byte{1, 2, 3}) {
		t.Fatalf("Blob = %v", got)
	}
	if got := record["IDs"].([]any); !reflect.DeepEqual(got, []any{int64(1), int64(-2)}) {
		t.Fatalf("IDs = %#v", got)
	}
	if got := record["Huge"].([]any); !reflect.DeepEqual(got, []any{uint64(1 << 63)}) {
		t.Fatalf("Huge = %#v", got)
	}
	if got := record["Words"].([]any); !reflect.DeepEqual(got, []any{"a", "b"}) {
		t.Fatalf("Words = %#v", got)
	}
}

func TestDecodeAnyNestedAndColumnar(t *testing.T) {
	value := &basket{ID: 9, Lines: lines(12)}
	decoded, err := DecodeAny(mustSchema(t, value), mustMarshal(t, value))
	if err != nil {
		t.Fatal(err)
	}
	rows := decoded.(map[string]any)["Lines"].([]any)
	if len(rows) != 12 {
		t.Fatalf("%d rows, want 12", len(rows))
	}
	first := rows[0].(map[string]any)
	if first["ProductID"] != uint64(1000) || first["UnitCents"] != int64(199) {
		t.Fatalf("row 0 is %#v", first)
	}
	// Quantity is an all-zero column and is not on the wire at all.
	if first["Quantity"] != uint64(0) {
		t.Fatalf("an elided column came back as %#v", first["Quantity"])
	}
}

// A narrow key the schema does not list cannot be skipped, and the error has to
// say so rather than leave a reader wondering which side is wrong.
func TestJSONRefusesAnUnknownNarrowKey(t *testing.T) {
	message := mustMarshal(t, &everyShape{Counts: []uint32{7}})
	schema := mustSchema(t, &charge{})
	_, err := ToJSON(schema, message)
	if err == nil || !strings.Contains(err.Error(), "narrow key cannot be skipped") {
		t.Fatalf("a foreign key gave %v", err)
	}
}

// A wide key the schema does not list is stepped over, which is what the wide
// width is for — the same evolution the Go decoder gets.
func TestJSONSkipsAnUnknownWideKey(t *testing.T) {
	type grown struct {
		Head  uint32 `cb:"0"`
		Extra string `cb:"1"`
		Tail  string `cb:"20"`
	}
	type older struct {
		Head uint32 `cb:"0"`
		Tail string `cb:"20"`
	}
	message := mustMarshal(t, &grown{Head: 7, Extra: "new", Tail: "tail"})
	out, err := ToJSON(mustSchema(t, &older{}), message)
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, out, []byte(`{"Head":7,"Tail":"tail"}`))
}

// Whatever arrives, the walk answers with an error rather than a panic.
func TestJSONHandlesGarbageWithoutPanicking(t *testing.T) {
	values := []any{
		&everyShape{Name: "uno", IDs: []int32{1, 2}, Words: []string{"a"}},
		&outer{Head: 1, One: inner{ID: 2, Name: "x"}, Many: []inner{{ID: 3}}},
		&basket{ID: 1, Lines: lines(12)},
		&withMaps{Labels: map[string]string{"a": "b"}},
		&bare{SensorID: 1, Unit: "C"},
	}
	for _, value := range values {
		schema := mustSchema(t, value)
		message := mustMarshal(t, value)
		for cut := range len(message) + 1 {
			func() {
				defer func() {
					if panicked := recover(); panicked != nil {
						t.Fatalf("%T cut to %d panicked: %v", value, cut, panicked)
					}
				}()
				_, _ = ToJSON(schema, message[:cut])
				_, _ = DecodeAny(schema, message[:cut])
			}()
		}
		// The schema of one type over the message of another, which is the
		// mismatch a long-lived connection eventually produces.
		for _, other := range values {
			func() {
				defer func() {
					if panicked := recover(); panicked != nil {
						t.Fatalf("%T under %T's schema panicked: %v", value, other, panicked)
					}
				}()
				_, _ = ToJSON(mustSchema(t, other), message)
			}()
		}
	}
}

// FuzzJSONWalk is the other half of FuzzParseSchema: a trusted schema over an
// arbitrary message, which is what a decoder actually faces.
func FuzzJSONWalk(f *testing.F) {
	schema, err := SchemaFor[basket]()
	if err != nil {
		f.Fatal(err)
	}
	for _, count := range []int{0, 1, 4, 12} {
		message, err := Marshal(&basket{ID: 9, Lines: lines(count)})
		if err != nil {
			f.Fatal(err)
		}
		f.Add(message)
	}
	f.Fuzz(func(t *testing.T, message []byte) {
		_, _ = ToJSON(schema, message)
		_, _ = DecodeAny(schema, message)
	})
}

// A section describing a struct that contains itself is finite on the wire and
// must stay finite in the walk: the depth limit is what makes a crafted one an
// error rather than a stack overflow.
func TestJSONRecursiveTypes(t *testing.T) {
	value := &recursive{
		Name: "root",
		Kids: []recursive{
			{Name: "a"},
			{Name: "b", Kids: []recursive{{Name: "b1"}}},
		},
	}
	matchesEncodingJSON(t, value)
}
