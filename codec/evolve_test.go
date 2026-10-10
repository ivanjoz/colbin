package codec

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// A narrow reader steps over a key it does not know, which is what lets a type
// drop a field — or a newer peer add one — at any id and still read what the
// other side wrote. The nibble beside a narrow key sizes the field whatever its
// type, so every shape a field can take has to be stepped over exactly, on every
// path that reads a narrow run: the flat walk, the nested walk, a Codec, the
// generated decoder, and the JSON and `any` walks.

// grownRecord is the newer type: id 6 is a field the older readers below do
// not declare, and it sits between fields they do, so a skip that lands a byte
// off misreads everything after it.
type grownRecord[T any] struct {
	Head  uint32   `cb:"1"`
	Name  string   `cb:"2"`
	Neg   int64    `cb:"3"`
	Tags  []string `cb:"4"`
	Score float64  `cb:"5"`
	Extra T        `cb:"6"`
	Tail  uint16   `cb:"7"`
}

// olderFlat is the same type before id 6, and a simple plan, so it is read by
// the flat walk (readScalars).
type olderFlat struct {
	Head  uint32   `cb:"1"`
	Name  string   `cb:"2"`
	Neg   int64    `cb:"3"`
	Tags  []string `cb:"4"`
	Score float64  `cb:"5"`
	Tail  uint16   `cb:"7"`
}

// olderNested is olderFlat with a composite field the message never carries,
// which sends it down the nested walk (readNarrowRun) instead.
type olderNested struct {
	Head  uint32          `cb:"1"`
	Name  string          `cb:"2"`
	Neg   int64           `cb:"3"`
	Tags  []string        `cb:"4"`
	Score float64         `cb:"5"`
	Tail  uint16          `cb:"7"`
	Inner *evolveInnerOld `cb:"9"`
}

type evolveInner struct {
	A uint32 `cb:"1"`
	B string `cb:"2"`
	C int64  `cb:"3"`
}

type evolveInnerOld struct {
	A uint32 `cb:"1"`
	C int64  `cb:"3"`
}

type evolveRow struct {
	ID     uint32 `cb:"1"`
	Name   string `cb:"2"`
	Gone   int32  `cb:"3"`
	Amount int64  `cb:"4"`
}

type evolveRowOld struct {
	ID     uint32 `cb:"1"`
	Name   string `cb:"2"`
	Amount int64  `cb:"4"`
}

func evolveRows(count int) []evolveRow {
	rows := make([]evolveRow, count)
	for index := range rows {
		rows[index] = evolveRow{
			ID:     uint32(100 + index),
			Name:   strings.Repeat("n", index),
			Gone:   int32(-index * 1000),
			Amount: int64(index) * 1_000_003,
		}
	}
	return rows
}

func olderRows(rows []evolveRow) []evolveRowOld {
	out := make([]evolveRowOld, len(rows))
	for index, row := range rows {
		out[index] = evolveRowOld{ID: row.ID, Name: row.Name, Amount: row.Amount}
	}
	return out
}

func int32Pointer(value int32) *int32    { return &value }
func stringPointer(value string) *string { return &value }

func TestAnUnknownNarrowFieldOfEveryShapeIsSteppedOver(t *testing.T) {
	skipsExtra(t, "uint inline", uint64(3))
	skipsExtra(t, "uint wide", uint64(1<<40))
	skipsExtra(t, "uint max", ^uint64(0))
	skipsExtra(t, "int minus one", int64(-1))
	skipsExtra(t, "int negative", int64(-1_234_567))
	skipsExtra(t, "int min", int64(-1<<63))
	skipsExtra(t, "bool", true)
	skipsExtra(t, "float", float32(-2.25))
	skipsExtra(t, "string short", "abc")
	skipsExtra(t, "string eight", "abcdefgh")
	skipsExtra(t, "string long", strings.Repeat("x", 300))
	skipsExtra(t, "string very long", strings.Repeat("y", 70_000))
	skipsExtra(t, "bytes", []byte{0, 1, 0xFF})
	skipsExtra(t, "int array", []int32{-5, 70_000, 3})
	skipsExtra(t, "uint array", []uint64{1 << 63, 5})
	skipsExtra(t, "string array", []string{"a", "", strings.Repeat("z", 400)})
	skipsExtra(t, "struct", evolveInner{A: 1, B: "inner", C: -9})
	skipsExtra(t, "list", evolveRows(3))
	skipsExtra(t, "table", evolveRows(12))
	skipsExtra(t, "big table", evolveRows(300))
	skipsExtra(t, "map", map[string]int64{"a": -1, "b": 1 << 40})
	skipsExtra(t, "map of structs", map[uint32]evolveRow{7: evolveRows(2)[1]})
	skipsExtra(t, "explicit zero int", int32Pointer(0))
	skipsExtra(t, "explicit empty string", stringPointer(""))
	skipsExtra(t, "pointer to a struct", &evolveInner{B: "pointed"})

	SetPacked5(true)
	defer SetPacked5(false)
	skipsExtra(t, "packed short", "USUARIO1")
	skipsExtra(t, "packed long", strings.Repeat("RESPONSES-GO-539 ", 40))
}

// skipsExtra writes a grownRecord holding extra at id 6 and reads it back
// through every narrow reader that does not know id 6.
func skipsExtra[T any](t *testing.T, name string, extra T) {
	t.Run(name, func(t *testing.T) {
		grown := grownRecord[T]{
			Head: 7, Name: "kept", Neg: -300, Tags: []string{"x", "y"}, Score: 1.5,
			Extra: extra, Tail: 9,
		}
		message := mustMarshal(t, &grown)
		if message[0] != rootStructNarrow {
			t.Fatalf("the record went wide: %#02x", message[0])
		}
		// The field has to be on the wire for skipping it to mean anything.
		grown.Extra = *new(T)
		if without := mustMarshal(t, &grown); len(without) >= len(message) {
			t.Fatalf("id 6 wrote nothing: %d bytes with it, %d without", len(message), len(without))
		}
		want := olderFlat{Head: 7, Name: "kept", Neg: -300, Tags: []string{"x", "y"}, Score: 1.5, Tail: 9}

		var flat olderFlat
		if err := Unmarshal(message, &flat); err != nil || !reflect.DeepEqual(flat, want) {
			t.Fatalf("flat walk: %+v, %v", flat, err)
		}
		var handled olderFlat
		if err := MustCodec[olderFlat]().Unmarshal(message, &handled); err != nil || !reflect.DeepEqual(handled, want) {
			t.Fatalf("Codec: %+v, %v", handled, err)
		}
		var nested olderNested
		if err := Unmarshal(message, &nested); err != nil {
			t.Fatalf("nested walk: %v", err)
		}
		if got := (olderFlat{nested.Head, nested.Name, nested.Neg, nested.Tags, nested.Score, nested.Tail}); !reflect.DeepEqual(got, want) || nested.Inner != nil {
			t.Fatalf("nested walk: %+v", nested)
		}

		wantJSON, _ := json.Marshal(want)
		out, err := ToJSON(mustSchema(t, &olderFlat{}), message)
		if err != nil {
			t.Fatalf("ToJSON: %v", err)
		}
		sameJSON(t, out, wantJSON)
		decoded, err := DecodeAny(mustSchema(t, &olderFlat{}), message)
		if err != nil {
			t.Fatalf("DecodeAny: %v", err)
		}
		if record := decoded.(map[string]any); record["Tail"] != uint64(9) || record["Neg"] != int64(-300) || len(record) != 6 {
			t.Fatalf("DecodeAny: %#v", record)
		}
	})
}

// The same inside every narrow composite: a nested struct, a list element, a
// table's column and a map's struct value, each holding a field — or a column —
// the reader's type has dropped.
type evolveOuter struct {
	Head   uint32               `cb:"1"`
	Inner  evolveInner          `cb:"2"`
	Rows   []evolveRow          `cb:"3"`
	ByID   map[uint32]evolveRow `cb:"4"`
	Tail   uint16               `cb:"5"`
	Spares *evolveInner         `cb:"6"`
}

type evolveOuterOld struct {
	Head   uint32                  `cb:"1"`
	Inner  evolveInnerOld          `cb:"2"`
	Rows   []evolveRowOld          `cb:"3"`
	ByID   map[uint32]evolveRowOld `cb:"4"`
	Tail   uint16                  `cb:"5"`
	Spares *evolveInnerOld         `cb:"6"`
}

func TestAnUnknownKeyInsideANarrowCompositeIsSteppedOver(t *testing.T) {
	for _, count := range []int{3, 12, 400} { // a list, a table, a long table
		rows := evolveRows(count)
		grown := evolveOuter{
			Head:   1,
			Inner:  evolveInner{A: 2, B: strings.Repeat("b", 260), C: -3},
			Rows:   rows,
			ByID:   map[uint32]evolveRow{rows[0].ID: rows[0], rows[2].ID: rows[2]},
			Tail:   300,
			Spares: &evolveInner{A: 4, B: "spare", C: 5},
		}
		message := mustMarshal(t, &grown)
		if message[0] != rootStructNarrow {
			t.Fatalf("%d rows: the record went wide", count)
		}
		want := evolveOuterOld{
			Head:   1,
			Inner:  evolveInnerOld{A: 2, C: -3},
			Rows:   olderRows(rows),
			ByID:   map[uint32]evolveRowOld{rows[0].ID: olderRows(rows)[0], rows[2].ID: olderRows(rows)[2]},
			Tail:   300,
			Spares: &evolveInnerOld{A: 4, C: 5},
		}
		var back evolveOuterOld
		if err := Unmarshal(message, &back); err != nil {
			t.Fatalf("%d rows: %v", count, err)
		}
		if !reflect.DeepEqual(back, want) {
			t.Fatalf("%d rows: read back %+v", count, back)
		}

		wantJSON, _ := json.Marshal(want)
		out, err := ToJSON(mustSchema(t, &evolveOuterOld{}), message)
		if err != nil {
			t.Fatalf("%d rows: ToJSON: %v", count, err)
		}
		sameJSON(t, out, wantJSON)
		if _, err := DecodeAny(mustSchema(t, &evolveOuterOld{}), message); err != nil {
			t.Fatalf("%d rows: DecodeAny: %v", count, err)
		}
	}
}

// chargeGrown is GenCharge with three fields its generated decoder has never
// heard of, past and between nothing: ids 11..13 follow the last one it knows.
type chargeGrown struct {
	CompanyID    int32    `cb:"1"`
	UserID       int32    `cb:"2"`
	RouteID      uint16   `cb:"3"`
	CPU          uint16   `cb:"4"`
	Inference    uint16   `cb:"5"`
	ExtraAllowed bool     `cb:"6"`
	Access1      uint16   `cb:"7"`
	Access2      uint16   `cb:"8"`
	Note         string   `cb:"11"`
	Debt         int64    `cb:"12"`
	Scores       []int32  `cb:"13"`
	Access3      uint16   `cb:"9"`
	Access4      uint16   `cb:"10"`
	Labels       []string `cb:"14"`
}

// The generated decoder steps over an unknown key as the reflective one does.
func TestGeneratedNarrowDecodeStepsOverAnUnknownKey(t *testing.T) {
	message := mustMarshal(t, &chargeGrown{
		CompanyID: 7, UserID: -42, RouteID: 103, CPU: 5, ExtraAllowed: true,
		Access1: 0x0139, Access3: 3, Access4: 400,
		Note: strings.Repeat("note", 100), Debt: -1 << 40, Scores: []int32{-1, 1 << 20},
		Labels: []string{"a", "b"},
	})
	var back GenCharge
	if err := back.UnmarshalColbin(message); err != nil {
		t.Fatal(err)
	}
	want := GenCharge{
		CompanyID: 7, UserID: -42, RouteID: 103, CPU: 5, ExtraAllowed: true,
		Access1: 0x0139, Access3: 3, Access4: 400,
	}
	if back != want {
		t.Fatalf("read back %+v", back)
	}
}

// Stepping over a field is sizing it, and sizing it still refuses a field that
// runs past the message: a truncated unknown field is an error, not a shorter
// record.
func TestATruncatedUnknownNarrowFieldIsRefused(t *testing.T) {
	message := mustMarshal(t, &grownRecord[string]{Head: 7, Extra: strings.Repeat("x", 300)})
	for cut := len(message) - 300; cut < len(message); cut++ {
		var back olderFlat
		if err := Unmarshal(message[:cut], &back); err == nil {
			t.Fatalf("a cut at %d of %d inside the unknown field decoded", cut, len(message))
		}
		if _, err := ToJSON(mustSchema(t, &olderFlat{}), message[:cut]); err == nil {
			t.Fatalf("a cut at %d of %d inside the unknown field rendered", cut, len(message))
		}
	}
}
