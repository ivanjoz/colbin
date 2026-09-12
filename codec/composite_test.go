package codec

import (
	"reflect"
	"testing"
)

type inner struct {
	ID   uint32 `cb:"0"`
	Name string `cb:"1"`
}

type outer struct {
	Head  uint32  `cb:"0"`
	One   inner   `cb:"1"`
	Many  []inner `cb:"2"`
	Tail  string  `cb:"3"`
	Empty []inner `cb:"4"`
}

// A nested struct and a slice of them round-trip, and the type goes wide of its
// own accord because a composite needs a byte length to be skippable.
func TestNestedStructsRoundTrip(t *testing.T) {
	value := outer{
		Head: 7,
		One:  inner{ID: 1, Name: "one"},
		Many: []inner{{ID: 2, Name: "two"}, {ID: 300, Name: ""}, {}},
		Tail: "tail",
	}
	message, err := Marshal(&value)
	if err != nil {
		t.Fatal(err)
	}
	// A composite no longer forces the wide width: §2.5's narrow composite
	// nibble carries the byte length with the class coming from the schema.
	if message[0] != rootStructNarrow {
		t.Fatalf("root is %#02x, want the narrow descriptor", message[0])
	}

	var back outer
	if err := Unmarshal(message, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, value) {
		t.Fatalf("round-tripped as %+v, want %+v", back, value)
	}
	// An empty slice comes back nil, like every other zero value.
	if back.Empty != nil {
		t.Fatalf("an empty slice round-tripped as %v", back.Empty)
	}
}

// A composite is skippable, so a reader that does not know a field can still
// read the ones after it — which a narrow key cannot do and is why a composite
// forces the wide one.
// Skipping an unknown composite needs the wide width, because a narrow
// descriptor has no class for a reader to size a field it does not know. The
// type therefore carries an id past fifteen, which is what puts it there.
type wideOuter struct {
	Head uint32  `cb:"0"`
	One  inner   `cb:"1"`
	Many []inner `cb:"2"`
	Tail string  `cb:"20"`
}

func TestUnknownCompositeIsSkipped(t *testing.T) {
	type narrowerOuter struct {
		Head uint32 `cb:"0"`
		Tail string `cb:"20"`
	}
	value := wideOuter{
		Head: 7,
		One:  inner{ID: 1, Name: "one"},
		Many: []inner{{ID: 2, Name: "two"}},
		Tail: "tail",
	}
	message, err := Marshal(&value)
	if err != nil {
		t.Fatal(err)
	}
	var back narrowerOuter
	if err := Unmarshal(message, &back); err != nil {
		t.Fatal(err)
	}
	if back.Head != 7 || back.Tail != "tail" {
		t.Fatalf("reading past two unknown composites gave %+v", back)
	}
}

// A type that reaches itself must build a plan and terminate on the data.
type node struct {
	Name string `cb:"0"`
	Kids []node `cb:"1"`
}

func TestRecursiveTypeRoundTrips(t *testing.T) {
	value := node{Name: "root", Kids: []node{
		{Name: "a", Kids: []node{{Name: "a1"}}},
		{Name: "b"},
	}}
	message, err := Marshal(&value)
	if err != nil {
		t.Fatal(err)
	}
	var back node
	if err := Unmarshal(message, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, value) {
		t.Fatalf("round-tripped as %+v", back)
	}
}

// Truncated bytes must never panic a nested decode.
func TestNestedTruncationIsRefused(t *testing.T) {
	value := outer{Head: 7, One: inner{ID: 1, Name: "one"},
		Many: []inner{{ID: 2, Name: "two"}}, Tail: "tail"}
	message, err := Marshal(&value)
	if err != nil {
		t.Fatal(err)
	}
	for cut := range len(message) {
		var back outer
		_ = Unmarshal(message[:cut], &back)
	}
}

func BenchmarkNestedAppend(b *testing.B) {
	value := outer{Head: 7, One: inner{ID: 1, Name: "one"},
		Many: []inner{{ID: 2, Name: "two"}, {ID: 3, Name: "three"}}, Tail: "tail"}
	handle := MustCodec[outer]()
	buffer := make([]byte, 0, 256)
	b.ReportAllocs()
	for b.Loop() {
		buffer = handle.Append(buffer[:0], &value)
	}
}

func BenchmarkNestedUnmarshal(b *testing.B) {
	value := outer{Head: 7, One: inner{ID: 1, Name: "one"},
		Many: []inner{{ID: 2, Name: "two"}, {ID: 3, Name: "three"}}, Tail: "tail"}
	handle := MustCodec[outer]()
	message := handle.Encode(&value)
	var back outer
	b.ReportAllocs()
	for b.Loop() {
		if err := handle.Unmarshal(message, &back); err != nil {
			b.Fatal(err)
		}
	}
}

// A slice of structs past the threshold is transposed into a table, and below it
// stays a list. The reader dispatches on the class it finds, so both round-trip
// through the same call.
type tableRow struct {
	ID     int32   `cb:"0"`
	UserID int32   `cb:"1"`
	Amount int64   `cb:"2"`
	Ratio  float64 `cb:"3"`
	Name   string  `cb:"4"`
	OK     bool    `cb:"5"`
}

type tableHolder struct {
	Head uint32     `cb:"0"`
	Rows []tableRow `cb:"1"`
}

func makeTableRows(n int) []tableRow {
	rows := make([]tableRow, n)
	for index := range rows {
		rows[index] = tableRow{
			ID: int32(1000 + index), UserID: int32(index % 7),
			Amount: int64(index) * 137, Ratio: float64(index) / 4,
			Name: []string{"a", "bb", "ccc"}[index%3], OK: index%2 == 0,
		}
	}
	return rows
}

func TestSliceOfStructsChoosesTheLayout(t *testing.T) {
	handle := MustCodec[tableHolder]()
	for _, n := range []int{0, 1, 2, tableThreshold - 1, tableThreshold, 100, 1000} {
		value := tableHolder{Head: 9, Rows: makeTableRows(n)}
		message := handle.Encode(&value)

		var back tableHolder
		if err := handle.Unmarshal(message, &back); err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if back.Head != 9 {
			t.Fatalf("n=%d: head %d", n, back.Head)
		}
		if n == 0 {
			if back.Rows != nil {
				t.Fatalf("n=0 round-tripped as %v", back.Rows)
			}
			continue
		}
		if !reflect.DeepEqual(back.Rows, value.Rows) {
			t.Fatalf("n=%d round-tripped wrong: first %+v want %+v", n, back.Rows[0], value.Rows[0])
		}
	}
}

// The table has to actually be smaller, or choosing it is pointless.
func TestTableBeatsRowWiseAtScale(t *testing.T) {
	handle := MustCodec[tableHolder]()
	small := len(handle.Encode(&tableHolder{Rows: makeTableRows(tableThreshold - 1)}))
	large := len(handle.Encode(&tableHolder{Rows: makeTableRows(1000)}))
	perRowSmall := float64(small) / float64(tableThreshold-1)
	perRowLarge := float64(large) / 1000
	t.Logf("list at %d rows: %.1f B/row · table at 1000 rows: %.1f B/row",
		tableThreshold-1, perRowSmall, perRowLarge)
	if perRowLarge >= perRowSmall {
		t.Fatalf("the table is %.1f B/row against the list's %.1f — it is not earning the class",
			perRowLarge, perRowSmall)
	}
}

// Truncated bytes must never panic a table decode.
func TestTableTruncationIsRefused(t *testing.T) {
	handle := MustCodec[tableHolder]()
	message := handle.Encode(&tableHolder{Head: 9, Rows: makeTableRows(50)})
	for cut := range len(message) {
		var back tableHolder
		_ = handle.Unmarshal(message[:cut], &back)
	}
}

func BenchmarkTableAppend(b *testing.B) {
	handle := MustCodec[tableHolder]()
	value := tableHolder{Head: 9, Rows: makeTableRows(1000)}
	buffer := make([]byte, 0, 1<<16)
	b.ReportAllocs()
	for b.Loop() {
		buffer = handle.Append(buffer[:0], &value)
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*1000), "ns/row")
}

func BenchmarkTableUnmarshal(b *testing.B) {
	handle := MustCodec[tableHolder]()
	value := tableHolder{Head: 9, Rows: makeTableRows(1000)}
	message := handle.Encode(&value)
	var back tableHolder
	b.ReportAllocs()
	for b.Loop() {
		if err := handle.Unmarshal(message, &back); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*1000), "ns/row")
}
