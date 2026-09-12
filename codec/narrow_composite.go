package codec

// Composites under four-bit keys.
//
// This is the mirror of composite.go, table.go and maps.go for the narrow
// writer, and it exists for the same reason the two key widths are separate
// files in `wire`: a width the compiler cannot see is a width it cannot fold.
//
// One shape genuinely differs rather than merely repeating. A wide list's
// element carries a descriptor and a length, because a wide reader may not know
// what the element is. A narrow one carries a length alone — its shape is the
// schema's to know — which is a byte per element, and the reason a narrow list
// of small structs is smaller than a wide one rather than merely equal.

import (
	"reflect"
	"unsafe"

	"github.com/ivanjoz/colbin/wire"
)

// appendNarrowStruct writes a nested key run, at whichever width the child needs.
func appendNarrowStruct(writer *wire.Writer, field *planField, at unsafe.Pointer, buf *scratch) {
	if field.sub.isWide {
		mark := writer.OpenStructWide(field.key)
		appendWideInto(writer, field.sub, at, buf)
		writer.Close(mark)
		return
	}
	mark := writer.OpenStruct(field.key)
	writePlan(writer, field.sub, at, buf)
	writer.Close(mark)
}

// appendWideInto writes a wide key run onto the narrow writer's buffer. Both
// writers are a []byte and nothing else, so this is a hand-off rather than a
// copy — the same trick appendNarrowInto plays in the other direction.
func appendWideInto(writer *wire.Writer, plan *typePlan, at unsafe.Pointer, buf *scratch) {
	wide := wire.Writer8{Buffer: writer.Buffer}
	appendWide(&wide, plan, at, buf)
	writer.Buffer = wide.Buffer
}

// appendNarrowStructs writes a slice of structs, transposed past the threshold.
func appendNarrowStructs(writer *wire.Writer, field *planField, at unsafe.Pointer, buf *scratch) {
	slice := (*sliceHeader)(at)
	if slice.len == 0 {
		return
	}
	if field.sub.canTable && slice.len >= tableThreshold {
		appendNarrowTable(writer, field, at, buf)
		return
	}
	list := writer.OpenList(field.key, slice.len)
	for index := range slice.len {
		element := writer.OpenElement()
		elementAt := unsafe.Add(slice.data, uintptr(index)*field.stride)
		if field.sub.isWide {
			appendWideInto(writer, field.sub, elementAt, buf)
		} else {
			writePlan(writer, field.sub, elementAt, buf)
		}
		writer.Close(element)
	}
	writer.Close(list)
}

func appendNarrowTable(writer *wire.Writer, field *planField, at unsafe.Pointer, buf *scratch) {
	slice := (*sliceHeader)(at)
	buf.reserve(slice.len)
	mark := writer.OpenTable(field.key, slice.len)
	for index := range field.sub.fields {
		column := &field.sub.fields[index]
		if column.op == opString {
			buf.strings = gatherStrings(buf.strings[:0], slice, field.stride, column.offset)
			writer.Strings(column.key, buf.strings)
			continue
		}
		buf.ints = gatherInts(buf.ints[:0], slice, field.stride, column)
		writer.Column(column.key, buf.ints)
	}
	writer.Close(mark)
}

func appendNarrowMap(writer *wire.Writer, field *planField, at unsafe.Pointer) {
	value := reflect.NewAt(field.sliceType, at).Elem()
	count := value.Len()
	if count == 0 {
		return
	}
	mark := writer.OpenMap(field.key, count)
	for entries := value.MapRange(); entries.Next(); {
		writeNarrowMapValue(writer, field.keyKind, entries.Key())
		writeNarrowMapValue(writer, field.valueKind, entries.Value())
	}
	writer.Close(mark)
}

func writeNarrowMapValue(writer *wire.Writer, kind mapKind, value reflect.Value) {
	switch kind {
	case mapString:
		writer.ElementString(value.String())
	case mapInt:
		writer.ElementInt(value.Int())
	case mapUint:
		writer.ElementUint(value.Uint())
	case mapFloat32, mapFloat64:
		writer.ElementUint(reverseFloatBits(value))
	case mapBool:
		if value.Bool() {
			writer.ElementUint(1)
		} else {
			writer.ElementUint(0)
		}
	}
}

// Reader side.

func readNarrowStruct(reader *wire.Reader, field *planField, at unsafe.Pointer, buf *scratch) {
	body, wideKeys, ok := reader.StructBody()
	if !ok {
		return
	}
	readNarrowBody(reader, body, wideKeys, field.sub, at, buf)
}

// readNarrowBody fills a record from a nested run at whichever width it declared.
func readNarrowBody(parent *wire.Reader, body []byte, wideKeys bool, plan *typePlan, at unsafe.Pointer, buf *scratch) {
	if wideKeys {
		sub := wire.NewReader8(body)
		readRun(&sub, plan, at, buf)
		parent.Fail(sub.Err())
		return
	}
	sub := wire.NewReader(body)
	readNarrowRun(&sub, plan, at, buf)
	parent.Fail(sub.Err())
}

// readNarrowRun is the narrow twin of readRun. An unknown key ends it rather
// than being stepped over, which is what four descriptor bits cost.
func readNarrowRun(reader *wire.Reader, plan *typePlan, record unsafe.Pointer, buf *scratch) {
	for reader.More() {
		field := plan.find(reader.Key())
		if field == nil {
			reader.Fail(wire.ErrBadDescriptor)
			return
		}
		readField(reader, field, record, buf)
	}
}

func readNarrowStructs(reader *wire.Reader, field *planField, at unsafe.Pointer, buf *scratch) {
	if reader.IsTable() {
		readNarrowTable(reader, field, at, buf)
		return
	}
	count, elements, ok := reader.Counted()
	if !ok {
		return
	}
	newSlice(field, at, count)
	data := (*sliceHeader)(at).data
	for index := range count {
		body, ok := elements.Element()
		if !ok {
			reader.Fail(elements.Err())
			return
		}
		elementAt := unsafe.Add(data, uintptr(index)*field.stride)
		if field.sub.isWide {
			sub := wire.NewReader8(body)
			readRun(&sub, field.sub, elementAt, buf)
			reader.Fail(sub.Err())
			continue
		}
		sub := wire.NewReader(body)
		readNarrowRun(&sub, field.sub, elementAt, buf)
		reader.Fail(sub.Err())
	}
}

func readNarrowTable(reader *wire.Reader, field *planField, at unsafe.Pointer, buf *scratch) {
	rows, columns, ok := reader.Counted()
	if !ok {
		return
	}
	buf.reserve(rows)
	newSlice(field, at, rows)
	data := (*sliceHeader)(at).data
	for columns.More() {
		column := field.sub.find(columns.Key())
		if column == nil {
			// A narrow key cannot be skipped, so an unknown column ends the
			// table rather than being stepped over.
			reader.Fail(wire.ErrBadDescriptor)
			return
		}
		if column.op == opString {
			buf.strings = columns.Strings(buf.strings[:0])
			scatterStrings(buf.strings, data, field.stride, column.offset, rows)
			continue
		}
		buf.ints = columns.Column(rows, buf.ints[:0])
		scatterInts(buf.ints, data, field.stride, column, rows)
	}
	reader.Fail(columns.Err())
}

func readNarrowMap(reader *wire.Reader, field *planField, at unsafe.Pointer) {
	count, entries, ok := reader.Counted()
	if !ok {
		return
	}
	target := reflect.NewAt(field.sliceType, at).Elem()
	built := reflect.MakeMapWithSize(field.sliceType, count)
	key := reflect.New(field.sliceType.Key()).Elem()
	value := reflect.New(field.sliceType.Elem()).Elem()
	for range count {
		readNarrowMapValue(&entries, field.keyKind, key)
		readNarrowMapValue(&entries, field.valueKind, value)
		if entries.Err() != nil {
			reader.Fail(entries.Err())
			return
		}
		built.SetMapIndex(key, value)
	}
	target.Set(built)
}

func readNarrowMapValue(reader *wire.Reader, kind mapKind, into reflect.Value) {
	switch kind {
	case mapString:
		into.SetString(reader.ElementString())
	case mapInt:
		into.SetInt(reader.ElementInt())
	case mapUint:
		into.SetUint(reader.ElementUint())
	case mapFloat32, mapFloat64:
		setFloatFromReversed(into, reader.ElementUint())
	case mapBool:
		into.SetBool(reader.ElementUint() == 1)
	}
}
