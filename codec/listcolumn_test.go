package codec

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

type listPart struct {
	Serial string `cb:"1"`
	Count  int16  `cb:"2"`
}

type listLine struct {
	Number   int32      `cb:"1"`
	Quantity float64    `cb:"2"`
	Mode     string     `cb:"3"`
	Note     *string    `cb:"4"`
	Parts    []listPart `cb:"5"`
}

// listOrder is a narrow row holding a slice of rows that hold a slice.
type listOrder struct {
	ID     int64      `cb:"1"`
	Status string     `cb:"2"`
	Lines  []listLine `cb:"3"`
	Total  float64    `cb:"4"`
}

// listWideOrder is listOrder with keys past four bits.
type listWideOrder struct {
	ID    int64      `cb:"1"`
	Lines []listLine `cb:"20"`
	Tags  []listPart `cb:"21"`
}

type listHolder struct {
	Head   int32           `cb:"1"`
	Orders []listOrder     `cb:"2"`
	Wide   []listWideOrder `cb:"3"`
}

type listWideHolder struct {
	Head   int32       `cb:"1"`
	Orders []listOrder `cb:"40"`
}

// listSelf holds a slice of itself, which never ends and so is no table.
type listSelf struct {
	ID       int32      `cb:"1"`
	Children []listSelf `cb:"2"`
}

func makeListLines(seed, n int) []listLine {
	if n == 0 {
		return nil
	}
	modes := []string{"AIR", "RAIL", "SHIP", "TRUCK"}
	lines := make([]listLine, n)
	for index := range lines {
		line := listLine{
			Number:   int32(index + 1),
			Quantity: float64((seed+index)%50) + 0.25,
			Mode:     modes[(seed+index)%len(modes)],
		}
		if (seed+index)%3 == 0 {
			note := fmt.Sprintf("note %d", seed%7)
			line.Note = &note
		}
		for part := range (seed + index) % 3 {
			line.Parts = append(line.Parts, listPart{Serial: fmt.Sprintf("S%d", part), Count: int16(part - 1)})
		}
		lines[index] = line
	}
	return lines
}

func makeListHolder(n int) listHolder {
	holder := listHolder{Head: 9}
	for index := range n {
		holder.Orders = append(holder.Orders, listOrder{
			ID: int64(index), Status: []string{"O", "F", "P"}[index%3],
			Lines: makeListLines(index, index%8), Total: float64(index) * 10.5,
		})
		wide := listWideOrder{ID: int64(-index), Lines: makeListLines(index+1, (index*5)%4)}
		if index%4 == 1 {
			wide.Tags = []listPart{{Serial: "t", Count: 1}}
		}
		holder.Wide = append(holder.Wide, wide)
	}
	return holder
}

// A row holding a slice of structs is a table now, the slice a list column, and
// it reads back, and walks to JSON, as the records it was.
func TestListColumnsRoundTrip(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeFor[listOrder](), reflect.TypeFor[listWideOrder](), reflect.TypeFor[listLine]()} {
		if !must(planFor(typ)).canTable {
			t.Fatalf("%v is not a table", typ)
		}
	}
	if must(planFor(reflect.TypeFor[listSelf]())).canTable {
		t.Fatal("a type that holds a slice of itself is a table")
	}
	schema, wideSchema := must(SchemaFor[listHolder]()), must(SchemaFor[listWideHolder]())
	for _, n := range []int{tableThreshold, 50, 1000, 9000} {
		holder := makeListHolder(n)
		wideHolder := listWideHolder{Head: 2, Orders: holder.Orders}
		for _, testCase := range []struct {
			value  any
			schema *Schema
		}{{&holder, schema}, {&wideHolder, wideSchema}} {
			value := reflect.ValueOf(testCase.value)
			message := roundTrips(t, value)
			want := must(json.Marshal(testCase.value))
			sameJSON(t, must(ToJSON(testCase.schema, message)), want)
			sameJSON(t, must(ToJSON(nil, must(MarshalSelfDescribing(testCase.value)))), want)
			if _, err := DecodeAny(testCase.schema, message); err != nil {
				t.Fatal(err)
			}
			var dict []byte
			withDictionary(t, func() { dict = must(Marshal(testCase.value)) })
			back := reflect.New(value.Elem().Type())
			if err := Unmarshal(dict, back.Interface()); err != nil || !reflect.DeepEqual(back.Elem().Interface(), value.Elem().Interface()) {
				t.Fatalf("n=%d %T: with dictionaries: %v", n, testCase.value, err)
			}
			sameJSON(t, must(ToJSON(testCase.schema, dict)), want)
			for cut := range min(len(message)-1, 300) {
				_ = Unmarshal(message[:len(message)-1-cut], reflect.New(value.Elem().Type()).Interface())
				_, _ = ToJSON(testCase.schema, message[:len(message)-1-cut])
			}
			t.Logf("n=%d %T: %d bytes, %d with dictionaries", n, testCase.value, len(message), len(dict))
		}
	}
}

// A decode into a value that already holds rows lands the same as a fresh one,
// a row whose slice the message leaves empty included.
func TestListColumnsReuse(t *testing.T) {
	handle := MustCodec[listHolder]()
	var back listHolder
	for _, n := range []int{300, 40, 300} {
		value := makeListHolder(n)
		if n == 40 {
			for index := range value.Orders {
				value.Orders[index].Lines = nil
			}
		}
		if err := handle.Unmarshal(must(handle.Encode(&value)), &back); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(back, value) {
			t.Fatalf("n=%d: a reused decode came back wrong", n)
		}
	}
}

// An element slice is capped at its own end, so that appending to one row's
// cannot write over the next row's elements.
func TestListColumnSlicesAreCapped(t *testing.T) {
	value := makeListHolder(100)
	var back listHolder
	if err := Unmarshal(must(Marshal(&value)), &back); err != nil {
		t.Fatal(err)
	}
	for index := range back.Orders {
		if lines := back.Orders[index].Lines; cap(lines) != len(lines) {
			t.Fatalf("row %d: len %d, cap %d", index, len(lines), cap(lines))
		}
	}
	before := back.Orders[2].Lines[0]
	back.Orders[1].Lines = append(back.Orders[1].Lines, listLine{Number: 99})
	if !reflect.DeepEqual(back.Orders[2].Lines[0], before) {
		t.Fatal("appending to a row's slice wrote over the next row's")
	}
}
