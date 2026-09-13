package wire

import (
	"math"
	"testing"
)

// listOf writes a key-less list of values built by fill, and returns a reader
// over its elements — which is the position every dynamic value is written in.
func listOf(t *testing.T, count int, fill func(w *Writer8)) Reader8 {
	t.Helper()
	writer := Writer8{}
	mark := writer.OpenList(0, count)
	fill(&writer)
	writer.Close(mark)

	reader := NewReader8(writer.Buffer)
	got, elements, ok := reader.List()
	if !ok {
		t.Fatalf("list: %v", reader.Err())
	}
	if got != count {
		t.Fatalf("list declares %d elements, wrote %d", got, count)
	}
	return elements
}

func TestDynamicValuesRoundTrip(t *testing.T) {
	elements := listOf(t, 12, func(w *Writer8) {
		w.ElementNull()
		w.ElementBool(true)
		w.ElementBool(false)
		w.ElementUint(7)
		w.ElementInt(-1234567)
		w.ElementFloat64(2)
		w.ElementFloat64(-0.1)
		w.ElementFloat32(1.5)
		w.ElementString("hola")
		w.ElementBytes([]byte{0x00, 0x7F, 0x80, 0xFF})
		w.ElementString("")
		w.ElementBytes(nil)
	})

	want := []Kind{
		KindNull, KindBool, KindBool, KindInt, KindInt,
		KindFloat64, KindFloat64, KindFloat32,
		KindString, KindBytes, KindString, KindBytes,
	}
	for at, kind := range want {
		if got := elements.ElementKind(); got != kind {
			t.Fatalf("element %d is kind %d, want %d", at, got, kind)
		}
		switch at {
		case 0:
			elements.ElementNull()
		case 1:
			if !elements.ElementBool() {
				t.Fatal("true came back false")
			}
		case 2:
			if elements.ElementBool() {
				t.Fatal("false came back true")
			}
		case 3:
			if got := elements.ElementUint(); got != 7 {
				t.Fatalf("uint %d", got)
			}
		case 4:
			if got := elements.ElementInt(); got != -1234567 {
				t.Fatalf("int %d", got)
			}
		case 5:
			if got := elements.ElementFloat64(); got != 2 {
				t.Fatalf("float64 %v", got)
			}
		case 6:
			if got := elements.ElementFloat64(); got != -0.1 {
				t.Fatalf("float64 %v", got)
			}
		case 7:
			if got := elements.ElementFloat32(); got != 1.5 {
				t.Fatalf("float32 %v", got)
			}
		case 8:
			if got := string(elements.ElementBlob()); got != "hola" {
				t.Fatalf("string %q", got)
			}
		case 9:
			if got := elements.ElementBytes(); string(got) != "\x00\x7F\x80\xFF" {
				t.Fatalf("bytes %x", got)
			}
		case 10:
			if got := elements.ElementBlob(); len(got) != 0 {
				t.Fatalf("empty string %q", got)
			}
		case 11:
			if got := elements.ElementBytes(); len(got) != 0 {
				t.Fatalf("empty bytes %x", got)
			}
		}
		if err := elements.Err(); err != nil {
			t.Fatalf("element %d: %v", at, err)
		}
	}
	if elements.MoreElements() {
		t.Fatal("bytes left after the last element")
	}
}

// A float in a dynamic position keeps the trimming a schema-driven one gets: the
// reversal puts a round value's zero bytes at the top, where the magnitude form
// drops them. Losing that would make every dynamic float eight bytes.
func TestADynamicRoundFloatIsTwoBytes(t *testing.T) {
	writer := Writer8{}
	writer.ElementFloat64(2)
	if len(writer.Buffer) != 2 {
		t.Fatalf("2.0 took %d bytes: %x", len(writer.Buffer), writer.Buffer)
	}

	writer.Reset(nil)
	writer.ElementFloat64(1.5)
	if len(writer.Buffer) != 4 {
		t.Fatalf("1.5 took %d bytes: %x", len(writer.Buffer), writer.Buffer)
	}
}

// The values JSON cannot spell still have to survive the wire, because the `any`
// decoder hands them back and only the JSON writer refuses them.
func TestDynamicFloatsCarryTheNonFinites(t *testing.T) {
	for _, value := range []float64{
		math.NaN(), math.Inf(1), math.Inf(-1), math.Copysign(0, -1),
	} {
		elements := listOf(t, 1, func(w *Writer8) { w.ElementFloat64(value) })
		got := elements.ElementFloat64()
		if err := elements.Err(); err != nil {
			t.Fatalf("%v: %v", value, err)
		}
		if math.IsNaN(value) {
			if !math.IsNaN(got) {
				t.Fatalf("NaN came back %v", got)
			}
			continue
		}
		if got != value || math.Signbit(got) != math.Signbit(value) {
			t.Fatalf("%v came back %v", value, got)
		}
	}
}

// A dynamic value is a composite element like any other, so the classes that
// already self-describe have to classify without a schema too.
func TestDynamicCompositesClassify(t *testing.T) {
	elements := listOf(t, 4, func(w *Writer8) {
		inner := w.OpenElementList(2)
		w.ElementUint(1)
		w.ElementUint(2)
		w.Close(inner)

		entries := w.OpenElementMap(1)
		w.ElementString("k")
		w.ElementUint(9)
		w.Close(entries)

		record := w.OpenElementStruct()
		w.U32(0, 5)
		w.Close(record)

		table := w.OpenElementTable(2)
		w.Column(0, []int64{1, 2})
		w.Close(table)
	})

	if got := elements.ElementKind(); got != KindList {
		t.Fatalf("list is kind %d", got)
	}
	count, inner, ok := elements.ElementList()
	if !ok || count != 2 {
		t.Fatalf("inner list: %d, %v", count, elements.Err())
	}
	if a, b := inner.ElementUint(), inner.ElementUint(); a != 1 || b != 2 {
		t.Fatalf("inner list held %d, %d", a, b)
	}

	if got := elements.ElementKind(); got != KindMap {
		t.Fatalf("map is kind %d", got)
	}
	count, entries, ok := elements.ElementMap()
	if !ok || count != 1 {
		t.Fatalf("inner map: %d, %v", count, elements.Err())
	}
	if key := string(entries.ElementBlob()); key != "k" {
		t.Fatalf("entry key %q", key)
	}
	if value := entries.ElementUint(); value != 9 {
		t.Fatalf("entry value %d", value)
	}

	if got := elements.ElementKind(); got != KindStruct {
		t.Fatalf("struct is kind %d", got)
	}
	record, ok := elements.ElementStruct()
	if !ok {
		t.Fatalf("inner struct: %v", elements.Err())
	}
	if got := record.U32(); got != 5 {
		t.Fatalf("struct field %d", got)
	}

	if got := elements.ElementKind(); got != KindTable {
		t.Fatalf("table is kind %d", got)
	}
	rows, columns, ok := elements.ElementTable()
	if !ok || rows != 2 {
		t.Fatalf("inner table: %d, %v", rows, elements.Err())
	}
	if got := columns.Column(rows, nil); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("column %v", got)
	}
}

// TYPED is the form that keeps an array of records cheap: a tag naming a struct
// def, and then the ordinary value.
func TestTypedElementNamesAStructAndThenIsOrdinary(t *testing.T) {
	for _, index := range []int{0, 3, 255, 300} {
		elements := listOf(t, 1, func(w *Writer8) {
			w.ElementTyped(index)
			list := w.OpenElementList(2)
			record := w.OpenElementStruct()
			w.U32(0, 11)
			w.Close(record)
			record = w.OpenElementStruct()
			w.U32(0, 22)
			w.Close(record)
			w.Close(list)
		})

		if got := elements.ElementKind(); got != KindTyped {
			t.Fatalf("index %d: kind %d", index, got)
		}
		got, ok := elements.ElementTypedIndex()
		if !ok {
			t.Fatalf("index %d: %v", index, elements.Err())
		}
		if got != index {
			t.Fatalf("tag says struct %d, wrote %d", got, index)
		}
		// What follows is an ordinary list, which is the whole point: the rows
		// cost what they cost in a typed field.
		if kind := elements.ElementKind(); kind != KindList {
			t.Fatalf("index %d: the tagged value is kind %d", index, kind)
		}
		count, rows, ok := elements.ElementList()
		if !ok || count != 2 {
			t.Fatalf("index %d: rows %d, %v", index, count, elements.Err())
		}
		first, ok := rows.ElementStruct()
		if !ok {
			t.Fatalf("index %d: first row: %v", index, rows.Err())
		}
		if value := first.U32(); value != 11 {
			t.Fatalf("index %d: first row holds %d", index, value)
		}
	}
}

// Every dynamic form has to be steppable without being understood, or a reader
// meeting one it has never heard of stops rather than carries on.
func TestSkipElementStepsOverEveryKind(t *testing.T) {
	writers := []struct {
		name  string
		write func(w *Writer8)
	}{
		{"null", func(w *Writer8) { w.ElementNull() }},
		{"true", func(w *Writer8) { w.ElementBool(true) }},
		{"inline int", func(w *Writer8) { w.ElementUint(7) }},
		{"wide int", func(w *Writer8) { w.ElementUint(math.MaxUint64) }},
		{"negative int", func(w *Writer8) { w.ElementInt(-1234567) }},
		{"float64", func(w *Writer8) { w.ElementFloat64(-0.1) }},
		{"float32", func(w *Writer8) { w.ElementFloat32(1.5) }},
		{"string", func(w *Writer8) { w.ElementString("hola") }},
		{"long string", func(w *Writer8) { w.ElementString(string(make([]byte, 400))) }},
		{"bytes", func(w *Writer8) { w.ElementBytes([]byte{1, 2, 3}) }},
		{"list", func(w *Writer8) {
			mark := w.OpenElementList(1)
			w.ElementUint(1)
			w.Close(mark)
		}},
		{"map", func(w *Writer8) {
			mark := w.OpenElementMap(1)
			w.ElementString("k")
			w.ElementNull()
			w.Close(mark)
		}},
		{"struct", func(w *Writer8) {
			mark := w.OpenElementStruct()
			w.U32(0, 5)
			w.Close(mark)
		}},
		{"typed", func(w *Writer8) {
			w.ElementTyped(2)
			mark := w.OpenElementStruct()
			w.U32(0, 5)
			w.Close(mark)
		}},
	}

	for _, one := range writers {
		// The value under test, then a sentinel. Skipping the first must land
		// exactly on the second — one byte out either way and the sentinel is
		// unreadable, which is the property being pinned.
		elements := listOf(t, 2, func(w *Writer8) {
			one.write(w)
			w.ElementUint(99)
		})
		if !elements.SkipElement() {
			t.Fatalf("%s: skip: %v", one.name, elements.Err())
		}
		if got := elements.ElementUint(); got != 99 {
			t.Fatalf("%s: skipped onto %d, want the sentinel", one.name, got)
		}
		if err := elements.Err(); err != nil {
			t.Fatalf("%s: %v", one.name, err)
		}
		if elements.MoreElements() {
			t.Fatalf("%s: bytes left over", one.name)
		}
	}
}

// The same values as a keyed field: an eight-bit key is skippable whatever the
// payload, and that has to keep holding now that there are payloads the skipper
// has no case for.
func TestSkipStepsOverADynamicField(t *testing.T) {
	var writer Writer8

	cases := []struct {
		name  string
		write func(w *Writer8)
	}{
		{"null", func(w *Writer8) { w.Key(0); w.ElementNull() }},
		{"float64", func(w *Writer8) { w.Key(0); w.ElementFloat64(-0.1) }},
		{"float32", func(w *Writer8) { w.Key(0); w.ElementFloat32(1.5) }},
		{"bytes", func(w *Writer8) { w.Key(0); w.ElementBytes([]byte{1, 2}) }},
		{"typed", func(w *Writer8) {
			w.Key(0)
			w.ElementTyped(1)
			mark := w.OpenElementStruct()
			w.U32(0, 5)
			w.Close(mark)
		}},
	}

	for _, one := range cases {
		writer.Reset(nil)
		one.write(&writer)
		writer.U32(1, 4242)

		reader := NewReader8(writer.Buffer)
		if !reader.Skip() {
			t.Fatalf("%s: skip: %v", one.name, reader.Err())
		}
		if key := reader.Key(); key != 1 {
			t.Fatalf("%s: skipped onto key %d", one.name, key)
		}
		if got := reader.U32(); got != 4242 {
			t.Fatalf("%s: field after the skip is %d", one.name, got)
		}
	}
}

// A truncated dynamic value must be refused rather than read past, and must not
// index outside the buffer on the way to saying so.
func TestTruncatedDynamicValuesAreRefused(t *testing.T) {
	writer := Writer8{}
	writer.ElementFloat64(-0.1)
	writer.ElementBytes([]byte{1, 2, 3})
	writer.ElementTyped(2)
	mark := writer.OpenElementStruct()
	writer.U32(0, 5)
	writer.Close(mark)
	whole := writer.Buffer

	for cut := range len(whole) {
		reader := Reader8{buffer: whole[:cut]}
		for reader.Err() == nil && reader.at < len(reader.buffer) {
			if !reader.SkipElement() {
				break
			}
		}
		// Either it stepped over what was there or it failed; what it must not do
		// is panic, which is what this loop is here to catch.
		_ = reader.Err()
	}
}
