package codec

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Maps whose values are lists, and maps whose values are pointers. See maps.go.

type listMaps struct {
	Head     uint32               `cb:"1"`
	Rows     map[string][]inner   `cb:"2"`
	RowPtrs  map[string]*[]inner  `cb:"3"`
	Records  map[int64]*inner     `cb:"4"`
	Ints     map[string][]int32   `cb:"5"`
	Names    map[uint16][]string  `cb:"6"`
	Floats   map[string][]float64 `cb:"7"`
	Halves   map[string][]float32 `cb:"8"`
	Flags    map[string]*[]bool   `cb:"9"`
	Big      map[string][]uint64  `cb:"10"`
	Platform map[string][]int     `cb:"11"`
	Tail     string               `cb:"12"`
}

// rowsOf is count records, enough of them past tableThreshold to make a
// table.
func rowsOf(count int) []inner {
	rows := make([]inner, count)
	for index := range rows {
		rows[index] = inner{ID: uint32(index * 3), Name: strings.Repeat("n", index%4)}
	}
	return rows
}

func fullListMaps() listMaps {
	few, many := rowsOf(3), rowsOf(tableThreshold*3)
	flags := []bool{true, false, true}
	return listMaps{
		Head:    7,
		Rows:    map[string][]inner{"few": few, "many": many, "empty": {}},
		RowPtrs: map[string]*[]inner{"few": &few, "many": &many},
		Records: map[int64]*inner{-1: {ID: 1, Name: "uno"}, 1 << 40: {}},
		Ints:    map[string][]int32{"a": {1, -2, 1 << 30}, "b": {}},
		Names:   map[uint16][]string{1: {"x", "", "yz"}},
		Floats:  map[string][]float64{"f": {0.5, -1e300, 0}},
		Halves:  map[string][]float32{"h": {1.5, -0.25}},
		Flags:   map[string]*[]bool{"on": &flags},
		Big:     map[string][]uint64{"u": {0, 1 << 63}},
		Platform: map[string][]int{
			"p": {-1, 1 << 30},
		},
		Tail: "tail",
	}
}

func TestMapsOfListsRoundTrip(t *testing.T) {
	original := fullListMaps()
	var back listMaps
	roundTrip(t, &original, &back)
	if !reflect.DeepEqual(back, original) {
		t.Fatalf("round-tripped as\n%+v\nwant\n%+v", back, original)
	}
}

// A nil value inside a map has no form on the wire, so it is written as an
// empty one and every entry reads back as a pointer to something.
func TestMapNilValuesReadBackEmpty(t *testing.T) {
	original := listMaps{
		Rows:    map[string][]inner{"nil": nil},
		RowPtrs: map[string]*[]inner{"nil": nil},
		Records: map[int64]*inner{3: nil},
		Ints:    map[string][]int32{"nil": nil},
		Flags:   map[string]*[]bool{"nil": nil},
	}
	var back listMaps
	roundTrip(t, &original, &back)
	want := listMaps{
		Rows:    map[string][]inner{"nil": {}},
		RowPtrs: map[string]*[]inner{"nil": {}},
		Records: map[int64]*inner{3: {}},
		Ints:    map[string][]int32{"nil": {}},
		Flags:   map[string]*[]bool{"nil": {}},
	}
	if !reflect.DeepEqual(back, want) {
		t.Fatalf("nil values read back as %+v, want %+v", back, want)
	}
}

// Entries are read through one reused value, so neither a record's omitted
// field nor a slice's array may carry over from the entry before, and two
// pointer entries must not share a pointee.
func TestMapListEntriesDoNotShare(t *testing.T) {
	original := listMaps{
		Rows:    map[string][]inner{"a": {{ID: 1, Name: "full"}}, "b": {{}}},
		Records: map[int64]*inner{1: {ID: 1, Name: "full"}, 2: {}},
	}
	var back listMaps
	roundTrip(t, &original, &back)
	if !reflect.DeepEqual(back, original) {
		t.Fatalf("entries leaked into each other: %+v", back)
	}
	back.Rows["a"][0].ID = 99
	if back.Rows["b"][0].ID == 99 {
		t.Fatal("two list entries share an array")
	}
	if back.Records[1] == back.Records[2] {
		t.Fatal("two pointer entries share a pointee")
	}
}

// The pointer is not on the wire: a map of pointers is the same bytes, and the
// same schema, as the map of what they point at.
func TestMapPointerValuesAreTheirPointees(t *testing.T) {
	type byValue struct {
		Rows    map[string][]inner `cb:"1"`
		Records map[int64]inner    `cb:"2"`
	}
	type byPointer struct {
		Rows    map[string]*[]inner `cb:"1"`
		Records map[int64]*inner    `cb:"2"`
	}
	rows := rowsOf(tableThreshold + 1)
	record := inner{ID: 4, Name: "four"}
	plain, err := MarshalSelfDescribing(&byValue{
		Rows: map[string][]inner{"r": rows}, Records: map[int64]inner{4: record},
	})
	if err != nil {
		t.Fatal(err)
	}
	pointed, err := MarshalSelfDescribing(&byPointer{
		Rows: map[string]*[]inner{"r": &rows}, Records: map[int64]*inner{4: &record},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plain, pointed) {
		t.Fatalf("a map of pointers wrote\n%v\nand the map of values\n%v", pointed, plain)
	}
}

// A map of pointers to structs needs no list, so it stays at four key bits when
// its type does; a map of lists does not.
func TestMapOfListsGoesWide(t *testing.T) {
	type pointers struct {
		M map[string]*inner `cb:"1"`
	}
	type lists struct {
		M map[string][]int32 `cb:"1"`
	}
	if plan, err := planFor(reflect.TypeFor[pointers]()); err != nil || plan.isWide {
		t.Fatalf("a map of pointers: wide %v, %v", plan != nil && plan.isWide, err)
	}
	if plan, err := planFor(reflect.TypeFor[lists]()); err != nil || !plan.isWide {
		t.Fatalf("a map of lists: wide %v, %v", plan != nil && plan.isWide, err)
	}
	original := pointers{M: map[string]*inner{"a": {ID: 1}, "nil": nil}}
	var back pointers
	roundTrip(t, &original, &back)
	if back.M["a"].ID != 1 || back.M["nil"] == nil || *back.M["nil"] != (inner{}) {
		t.Fatalf("a narrow map of pointers read back as %+v", back.M)
	}
}

// listTree reaches itself through a map of lists, which ends where a list is
// empty.
type listTree struct {
	Name string                `cb:"1"`
	Kids map[string][]listTree `cb:"2"`
}

func TestMapOfListsRecursive(t *testing.T) {
	tree := listTree{Name: "root", Kids: map[string][]listTree{
		"a": {{Name: "leaf", Kids: map[string][]listTree{}}},
		"b": {{Name: "branch", Kids: map[string][]listTree{"c": {{Name: "deep"}}}}},
	}}
	var back listTree
	roundTrip(t, &tree, &back)
	// An empty map is not written and reads back nil, as anywhere else.
	tree.Kids["a"][0].Kids = nil
	if !reflect.DeepEqual(back, tree) {
		t.Fatalf("round-tripped as %+v, want %+v", back, tree)
	}
}

func TestMapOfListsIsDeterministic(t *testing.T) {
	original := fullListMaps()
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
			t.Fatal("the same map of lists encoded two different ways")
		}
	}
}

// The JSON is what encoding/json makes of the same value, lists and tables
// alike, and DecodeAny is the same document.
func TestMapOfListsJSON(t *testing.T) {
	type document struct {
		Rows    map[string][]inner   `cb:"1"`
		Records map[string]*inner    `cb:"2"`
		Ints    map[string][]int32   `cb:"3"`
		Names   map[string][]string  `cb:"4"`
		Floats  map[string][]float64 `cb:"5"`
		Flags   map[string][]bool    `cb:"6"`
	}
	value := document{
		Rows:    map[string][]inner{"few": rowsOf(2), "many": rowsOf(tableThreshold * 2), "none": {}},
		Records: map[string]*inner{"r": {ID: 5, Name: "five"}},
		Ints:    map[string][]int32{"i": {1, -2}},
		Names:   map[string][]string{"n": {"a", ""}},
		Floats:  map[string][]float64{"f": {0.5, 2}},
		Flags:   map[string][]bool{"b": {true, false}},
	}
	want, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	message, err := MarshalSelfDescribing(&value)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ToJSON(nil, message)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}

	decoded, err := DecodeAny(nil, message)
	if err != nil {
		t.Fatal(err)
	}
	// Through encoding/json both ways, which sorts an object's keys the same
	// for each.
	var fromJSON any
	if err := json.Unmarshal(want, &fromJSON); err != nil {
		t.Fatal(err)
	}
	normal, err := json.Marshal(fromJSON)
	if err != nil {
		t.Fatal(err)
	}
	again, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(normal) {
		t.Fatalf("DecodeAny gave\n%s\nwant\n%s", again, normal)
	}
}

func TestMapOfListsTruncationIsRefused(t *testing.T) {
	original := fullListMaps()
	checkPrefixes(t, &original, unmarshalInto[listMaps])
}

// A message holding a map of lists does not decode into a map of scalars or
// of structs under the same key: the values are not what the type says.
func TestMapOfListsIntoAnotherMapIsRefused(t *testing.T) {
	type lists struct {
		M map[string][]inner `cb:"1"`
	}
	type records struct {
		M map[string]inner `cb:"1"`
	}
	type scalars struct {
		M map[string]int32 `cb:"1"`
	}
	type scalarLists struct {
		M map[string][]int32 `cb:"1"`
	}
	message, err := Marshal(&lists{M: map[string][]inner{"a": rowsOf(2)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := Unmarshal(message, &records{}); err == nil {
		t.Error("a map of lists decoded into a map of structs")
	}
	if err := Unmarshal(message, &scalars{}); err == nil {
		t.Error("a map of lists decoded into a map of scalars")
	}
	if err := Unmarshal(message, &scalarLists{}); err == nil {
		t.Error("a map of lists of structs decoded into a map of lists of scalars")
	}
}

// The section names a mapStructs value's struct, and a mapList's element kind
// in the byte after the two kinds, which a reader has to find and check.
func TestParseSchemaMapsOfLists(t *testing.T) {
	type one struct {
		Rows map[string][]inner  `cb:"1"`
		Ints map[string][]uint32 `cb:"2"`
	}
	schema, err := SchemaFor[one]()
	if err != nil {
		t.Fatal(err)
	}
	good := schema.Bytes()
	parsed, err := ParseSchema(good)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(parsed.Bytes(), good) {
		t.Fatal("a parsed section does not write back as itself")
	}
	structs := bytes.Index(good, []byte{uint8(opMap), uint8(mapString), uint8(mapStructs)})
	list := bytes.Index(good, []byte{uint8(opMap), uint8(mapString), uint8(mapList), uint8(mapUint)})
	if structs < 0 || list < 0 {
		t.Fatalf("the fixture's section has no map of lists: %v", good)
	}
	pointsOut := append([]byte(nil), good...)
	pointsOut[structs+3] = 9
	if _, err := ParseSchema(pointsOut); err == nil || !strings.Contains(err.Error(), "points at struct 9") {
		t.Fatalf("an index outside the table gave %v", err)
	}
	notScalar := append([]byte(nil), good...)
	notScalar[list+3] = uint8(mapStruct)
	if _, err := ParseSchema(notScalar); err == nil || !strings.Contains(err.Error(), "not a scalar") {
		t.Fatalf("a list of structs named as a scalar list gave %v", err)
	}
	for cut := range len(good) {
		if _, err := ParseSchema(good[:cut]); err == nil {
			t.Fatalf("a section truncated to %d bytes was accepted", cut)
		}
	}
}
