package wire

import (
	"strings"
	"testing"
)

func TestNestedStructRoundTrip(t *testing.T) {
	writer := Writer8{}
	writer.U32(0, 7)
	inner := writer.OpenStruct(1)
	writer.U32(0, 42)
	writer.String(1, "hero")
	deeper := writer.OpenStruct(2)
	writer.Bool(0, true)
	writer.Int(1, -1234567)
	writer.Close(deeper)
	writer.Close(inner)
	writer.String(2, "tail")

	reader := NewReader8(writer.Buffer)
	if got := reader.U32(); got != 7 {
		t.Fatalf("outer scalar %d", got)
	}
	sub, ok := reader.Struct()
	if !ok {
		t.Fatalf("struct: %v", reader.Err())
	}
	if got := sub.U32(); got != 42 {
		t.Fatalf("inner scalar %d", got)
	}
	if got := sub.String(); got != "hero" {
		t.Fatalf("inner string %q", got)
	}
	deep, ok := sub.Struct()
	if !ok {
		t.Fatalf("deep struct: %v", sub.Err())
	}
	if !deep.Bool() {
		t.Fatal("deep bool")
	}
	if got := deep.Int(); got != -1234567 {
		t.Fatalf("deep int %d", got)
	}
	if deep.More() || sub.More() {
		t.Fatal("trailing bytes in a nested run")
	}
	if got := reader.String(); got != "tail" {
		t.Fatalf("tail %q", got)
	}
	if reader.More() || reader.Err() != nil {
		t.Fatalf("trailing or %v", reader.Err())
	}
}

// A body past 255 bytes widens its length placeholder, shifting what follows.
// The shift is the one part of the writer that moves bytes already written, so
// it gets its own test at the boundary.
func TestCompositeLengthWidens(t *testing.T) {
	for _, size := range []int{1, 250, 253, 254, 255, 256, 300, 70000} {
		writer := Writer8{}
		writer.U32(0, 9)
		mark := writer.OpenStruct(1)
		writer.String(0, strings.Repeat("x", size))
		writer.Close(mark)
		writer.U32(2, 11)

		reader := NewReader8(writer.Buffer)
		if got := reader.U32(); got != 9 {
			t.Fatalf("size %d: leading scalar %d", size, got)
		}
		sub, ok := reader.Struct()
		if !ok {
			t.Fatalf("size %d: struct: %v", size, reader.Err())
		}
		if got := sub.String(); len(got) != size {
			t.Fatalf("size %d: inner string is %d bytes", size, len(got))
		}
		if got := reader.U32(); got != 11 {
			t.Fatalf("size %d: trailing scalar %d — the shift lost bytes", size, got)
		}
		if reader.More() || reader.Err() != nil {
			t.Fatalf("size %d: trailing or %v", size, reader.Err())
		}
	}
}

func TestListOfStructsRoundTrip(t *testing.T) {
	type row struct {
		id   uint32
		name string
	}
	rows := []row{{1, "a"}, {2, "bb"}, {300, strings.Repeat("c", 400)}}

	writer := Writer8{}
	list := writer.OpenList(1, len(rows))
	for _, r := range rows {
		element := writer.OpenElementStruct()
		writer.U32(0, r.id)
		writer.String(1, r.name)
		writer.Close(element)
	}
	writer.Close(list)
	writer.U32(2, 77)

	reader := NewReader8(writer.Buffer)
	count, elements, ok := reader.List()
	if !ok {
		t.Fatalf("list: %v", reader.Err())
	}
	if count != len(rows) {
		t.Fatalf("count %d", count)
	}
	for index := range count {
		sub, ok := elements.ElementStruct()
		if !ok {
			t.Fatalf("element %d: %v", index, elements.Err())
		}
		if got := sub.U32(); got != rows[index].id {
			t.Fatalf("element %d id %d", index, got)
		}
		if got := sub.String(); got != rows[index].name {
			t.Fatalf("element %d name %q", index, got)
		}
	}
	if elements.MoreElements() {
		t.Fatal("trailing elements")
	}
	if got := reader.U32(); got != 77 {
		t.Fatalf("after the list: %d", got)
	}
}

func TestListOfScalarsAndMapRoundTrip(t *testing.T) {
	writer := Writer8{}
	list := writer.OpenList(1, 4)
	writer.ElementUint(7)
	writer.ElementUint(1000)
	writer.ElementInt(-5)
	writer.ElementString("hi")
	writer.Close(list)

	entries := map[string]int64{"a": 1, "bb": -2}
	m := writer.OpenMap(2, len(entries))
	for _, key := range []string{"a", "bb"} {
		writer.ElementString(key)
		writer.ElementInt(entries[key])
	}
	writer.Close(m)

	reader := NewReader8(writer.Buffer)
	count, elements, ok := reader.List()
	if !ok || count != 4 {
		t.Fatalf("list count %d: %v", count, reader.Err())
	}
	if got := elements.ElementUint(); got != 7 {
		t.Fatalf("element 0 %d", got)
	}
	if got := elements.ElementUint(); got != 1000 {
		t.Fatalf("element 1 %d", got)
	}
	if got := elements.ElementInt(); got != -5 {
		t.Fatalf("element 2 %d", got)
	}
	if got := elements.ElementString(); got != "hi" {
		t.Fatalf("element 3 %q", got)
	}

	count, pairs, ok := reader.Map()
	if !ok || count != 2 {
		t.Fatalf("map count %d: %v", count, reader.Err())
	}
	for range count {
		key := pairs.ElementString()
		value := pairs.ElementInt()
		if entries[key] != value {
			t.Fatalf("map[%q] = %d", key, value)
		}
	}
	if reader.More() || reader.Err() != nil {
		t.Fatalf("trailing or %v", reader.Err())
	}
}

// The capability the byte length buys: a composite is skippable without its
// sub-schema, which is exactly what compact mode's ErrSkipComposite exists to
// say cannot be done.
func TestCompositesAreSkippable(t *testing.T) {
	writer := Writer8{}
	writer.U32(0, 7)
	inner := writer.OpenStruct(1)
	writer.String(0, strings.Repeat("x", 400))
	deeper := writer.OpenStruct(1)
	writer.U32(0, 5)
	writer.Close(deeper)
	writer.Close(inner)
	list := writer.OpenList(2, 2)
	element := writer.OpenElementStruct()
	writer.U32(0, 1)
	writer.Close(element)
	element = writer.OpenElementStruct()
	writer.U32(0, 2)
	writer.Close(element)
	writer.Close(list)
	m := writer.OpenMap(3, 1)
	writer.ElementString("k")
	writer.ElementUint(9)
	writer.Close(m)
	writer.U32(4, 11)

	reader := NewReader8(writer.Buffer)
	keys := []uint8{}
	for reader.More() {
		keys = append(keys, reader.Key())
		if !reader.Skip() {
			t.Fatalf("skip of key %d: %v", reader.Key(), reader.Err())
		}
	}
	if err := reader.Err(); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 5 {
		t.Fatalf("skipped %d fields, want 5: %v", len(keys), keys)
	}

	// And a reader that knows only the last field still finds it.
	reader = NewReader8(writer.Buffer)
	found := false
	for reader.More() {
		if reader.Key() == 4 {
			if got := reader.U32(); got != 11 {
				t.Fatalf("key 4 read as %d", got)
			}
			found = true
			continue
		}
		if !reader.Skip() {
			t.Fatalf("skip: %v", reader.Err())
		}
	}
	if !found {
		t.Fatal("key 4 not found past three composites")
	}
}

// Truncated and arbitrary bytes must never panic a composite read.
func TestCompositeGarbageIsRefused(t *testing.T) {
	writer := Writer8{}
	inner := writer.OpenStruct(1)
	writer.String(0, "responses.go:539")
	writer.Close(inner)
	list := writer.OpenList(2, 1)
	element := writer.OpenElementStruct()
	writer.U32(0, 1)
	writer.Close(element)
	writer.Close(list)
	full := writer.Buffer

	for cut := range len(full) {
		reader := NewReader8(full[:cut])
		for reader.More() {
			if !reader.Skip() {
				break
			}
		}
		reader = NewReader8(full[:cut])
		for reader.More() {
			switch reader.Key() {
			case 1:
				sub, ok := reader.Struct()
				if ok {
					for sub.More() {
						sub.Bytes()
					}
				}
			case 2:
				_, elements, ok := reader.List()
				if ok {
					for elements.MoreElements() {
						if sub, ok := elements.ElementStruct(); ok {
							for sub.More() {
								sub.Uint()
							}
						} else {
							break
						}
					}
				}
			default:
				if !reader.Skip() {
					reader.fail(ErrTruncated)
				}
			}
		}
	}

	for seed := range 20000 {
		message := make([]byte, seed%29)
		for index := range message {
			message[index] = byte(seed*37 + index*11)
		}
		reader := NewReader8(message)
		for reader.More() {
			if !reader.Skip() {
				break
			}
		}
		reader = NewReader8(message)
		for reader.More() {
			if sub, ok := reader.Struct(); ok {
				for sub.More() {
					sub.Uint()
				}
			} else {
				break
			}
		}
		reader = NewReader8(message)
		for reader.More() {
			if _, elements, ok := reader.List(); ok {
				for elements.MoreElements() {
					elements.ElementUint()
				}
			} else {
				break
			}
		}
	}
}
