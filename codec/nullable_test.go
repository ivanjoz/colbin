package codec

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"testing"
)

// nullRow is a row with a pointer to every kind a column carries.
type nullRow struct {
	ID    uint32   `cb:"1"`
	Qty   *int32   `cb:"2"`
	Price *float64 `cb:"3"`
	Name  *string  `cb:"4"`
	Ok    *bool    `cb:"5"`
	Big   *uint64  `cb:"6"`
	Small *int8    `cb:"7"`
	Ratio *float32 `cb:"8"`
}

type nullTable struct {
	Rows []nullRow `cb:"1"`
	ID   uint64    `cb:"2"`
}

// wideNullRow numbers a field past fifteen, so its columns are keyed at eight
// bits under a narrow parent.
type wideNullRow struct {
	Qty  *int64  `cb:"2"`
	Name *string `cb:"5"`
	At   *int32  `cb:"20"`
}

type wideNullTable struct {
	Rows []wideNullRow `cb:"1"`
	ID   uint64        `cb:"2"`
}

// wideParentNullTable is a wide record holding narrow rows.
type wideParentNullTable struct {
	Rows []nullRow `cb:"1"`
	ID   uint64    `cb:"30"`
}

// nullRows builds count rows in which row r holds a value in a column when
// keep(r, column) says so; values cycle through zeros, negatives and -0.0 so
// that an explicit zero has to survive.
func nullRows(count int, keep func(row, column int) bool) []nullRow {
	rows := make([]nullRow, count)
	for r := range rows {
		row := &rows[r]
		row.ID = uint32(r)
		if keep(r, 0) {
			row.Qty = ptr(int32(r%5) - 2)
		}
		if keep(r, 1) {
			row.Price = ptr([]float64{0, 1.5, math.Copysign(0, -1), -2.25}[r%4])
		}
		if keep(r, 2) {
			row.Name = ptr([]string{"", "a", "bc", "longer than eight"}[r%4])
		}
		if keep(r, 3) {
			row.Ok = ptr(r%2 == 0)
		}
		if keep(r, 4) {
			row.Big = ptr(uint64(r) * 1e15)
		}
		if keep(r, 5) {
			row.Small = ptr(int8(r%3) - 1)
		}
		if keep(r, 6) {
			row.Ratio = ptr(float32(r%3) / 4)
		}
	}
	return rows
}

func wideNullRows(rows []nullRow) []wideNullRow {
	out := make([]wideNullRow, len(rows))
	for i, row := range rows {
		if row.Qty != nil {
			out[i].Qty = ptr(int64(*row.Qty) << 40)
		}
		out[i].Name = row.Name
		if row.Small != nil {
			out[i].At = ptr(int32(*row.Small))
		}
	}
	return out
}

var nullPatterns = map[string]func(row, column int) bool{
	"all nil":       func(int, int) bool { return false },
	"all present":   func(int, int) bool { return true },
	"every third":   func(row, column int) bool { return (row+column)%3 != 0 },
	"one present":   func(row, column int) bool { return row == 5 },
	"one nil":       func(row, column int) bool { return row != 5 },
	"column by col": func(row, column int) bool { return column%2 == 0 || row%7 == 1 },
}

// A slice of rows with pointer fields is a table, and every reader gives back
// what was written: nil where nil, a pointer to zero where one, at every key
// width a table takes, whole and in groups.
func TestPointerColumns(t *testing.T) {
	plan := must(planFor(reflect.TypeFor[nullRow]()))
	if !plan.canTable {
		t.Fatal("a row of pointers to scalars is not a table")
	}
	for name, keep := range nullPatterns {
		for _, count := range []int{8, 9, 64, 200} {
			for _, groupRows := range []int{4096, 7} {
				label := fmt.Sprintf("%s, %d rows, groups of %d", name, count, groupRows)
				rows := nullRows(count, keep)
				withGroupRows(groupRows, func() {
					checkPointerTable(t, label, &nullTable{Rows: rows, ID: 3},
						&nullTable{Rows: nullRows(count+3, nullPatterns["all present"])})
					checkPointerTable(t, label+", wide rows", &wideNullTable{Rows: wideNullRows(rows), ID: 3},
						&wideNullTable{Rows: wideNullRows(nullRows(count, nullPatterns["one nil"]))})
					checkPointerTable(t, label+", wide parent", &wideParentNullTable{Rows: rows, ID: 3},
						&wideParentNullTable{Rows: nullRows(2, nullPatterns["all present"])})
				})
			}
		}
	}
}

func checkPointerTable[T any](t *testing.T, name string, value, used *T) {
	t.Helper()
	message := must(Marshal(value))
	var fresh T
	if err := Unmarshal(message, &fresh); err != nil || !reflect.DeepEqual(&fresh, value) {
		t.Fatalf("%s: decoded as %+v, %v", name, fresh, err)
	}
	if err := MustCodec[T]().Unmarshal(message, used); err != nil || !reflect.DeepEqual(used, value) {
		t.Fatalf("%s: into a used value: %v", name, err)
	}
	matchesEncodingJSON(t, value)
	if _, err := DecodeAny(mustSchema(t, value), message); err != nil {
		t.Fatalf("%s: DecodeAny: %v", name, err)
	}
	described := must(MarshalSelfDescribing(value))
	out, err := ToJSON(nil, described)
	if err != nil {
		t.Fatalf("%s: a self-describing message's JSON: %v", name, err)
	}
	sameJSON(t, out, must(json.Marshal(value)))
	for cut := 2; cut < len(message); cut++ {
		var back T
		if Unmarshal(message[:cut], &back) == nil && reflect.DeepEqual(&back, value) {
			t.Fatalf("%s: a message cut at %d of %d decoded whole", name, cut, len(message))
		}
	}
}

// A table whose pointer columns are full costs what the same rows without
// pointers cost, and one with nils costs less than the list it used to be.
func TestPointerColumnSizes(t *testing.T) {
	type plainRow struct {
		ID  uint32 `cb:"1"`
		Qty int32  `cb:"2"`
	}
	type pointerRow struct {
		ID  uint32 `cb:"1"`
		Qty *int32 `cb:"2"`
	}
	plain := struct {
		Rows []plainRow `cb:"1"`
	}{}
	pointers := struct {
		Rows []pointerRow `cb:"1"`
	}{}
	for r := range 1000 {
		plain.Rows = append(plain.Rows, plainRow{ID: uint32(r), Qty: int32(r % 9)})
		pointers.Rows = append(pointers.Rows, pointerRow{ID: uint32(r), Qty: ptr(int32(r % 9))})
	}
	full, flat := len(must(Marshal(&pointers))), len(must(Marshal(&plain)))
	if full != flat {
		t.Fatalf("a full pointer column took %d bytes against the plain field's %d", full, flat)
	}
	for r := range pointers.Rows {
		if r%4 == 0 {
			pointers.Rows[r].Qty = nil
		}
	}
	if gaps := len(must(Marshal(&pointers))); gaps > flat+1000/8+8 {
		t.Fatalf("a quarter nil took %d bytes, the plain column %d", gaps, flat)
	}
}
