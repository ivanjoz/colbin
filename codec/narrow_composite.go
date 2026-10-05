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
	"fmt"
	"reflect"
	"unsafe"

	"github.com/ivanjoz/colbin/wire"
)

// appendNarrowStruct writes a nested key run, at whichever width the child needs.
// The depth is taken by the run's own walk, writePlan or appendWide.
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

// appendNarrowPointerStruct is appendPointerStructField at four key bits.
func appendNarrowPointerStruct(writer *wire.Writer, field *planField, at unsafe.Pointer, buf *scratch) {
	pointee := *(*unsafe.Pointer)(at)
	if pointee == nil {
		return
	}
	appendNarrowStruct(writer, field, pointee, buf)
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
	// A list or a table is a level of depth of its own, as it is to every reader.
	if !buf.enter() {
		return
	}
	defer buf.leave()
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
		writer.CloseElement(element)
	}
	writer.Close(list)
}

// appendNarrowTable writes a table under a narrow parent, its columns keyed at
// the row type's width. See table.go.
func appendNarrowTable(writer *wire.Writer, field *planField, at unsafe.Pointer, buf *scratch) {
	slice := (*sliceHeader)(at)
	mark := writer.OpenTable(field.key, slice.len)
	if field.sub.isWide {
		wide := wire.Writer8{Buffer: writer.Buffer}
		appendColumns(&wide, field.sub, slice, field.stride, buf)
		writer.Buffer = wide.Buffer
		writer.Close(mark)
		return
	}
	columns := buf.reserve(slice.len, field.sub.hasStringColumn())
	for index := range field.sub.fields {
		column := &field.sub.fields[index]
		if column.op == opString {
			columns.strings = gatherStrings(columns.strings[:0], slice, field.stride, column.offset)
			writer.Strings(column.key, columns.strings)
			continue
		}
		columns.ints = gatherInts(columns.ints[:0], slice, field.stride, column)
		writer.Column(column.key, columns.ints)
	}
	writer.Close(mark)
}

func appendNarrowMap(writer *wire.Writer, field *planField, at unsafe.Pointer, buf *scratch) {
	value := reflect.NewAt(field.sliceType, at).Elem()
	count := value.Len()
	if count == 0 {
		return
	}
	if !buf.enter() {
		return
	}
	defer buf.leave()
	mark := writer.OpenMap(field.key, count)
	for _, key := range sortedMapKeys(value) {
		writeNarrowMapValue(writer, field.keyKind, key)
		writeNarrowMapValue(writer, field.valueKind, value.MapIndex(key))
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

// readNarrowPointerStruct is readPointerStructField at four key bits.
func readNarrowPointerStruct(reader *wire.Reader, field *planField, at unsafe.Pointer, buf *scratch) {
	pointee := reflect.New(field.sliceType.Elem()).UnsafePointer()
	readNarrowStruct(reader, field, pointee, buf)
	*(*unsafe.Pointer)(at) = pointee
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
	if !buf.enter() {
		reader.Fail(buf.err)
		return
	}
	defer buf.leave()
	for reader.More() {
		key := reader.Key()
		field := plan.find(key)
		if field == nil {
			reader.Fail(fmt.Errorf(
				"message holds field id %d, which the type does not declare, "+
					"and a narrow key cannot be skipped", int(key)+1))
			return
		}
		readField(reader, field, record, buf)
	}
}

func readNarrowStructs(reader *wire.Reader, field *planField, at unsafe.Pointer, buf *scratch) {
	if !buf.enter() {
		reader.Fail(buf.err)
		return
	}
	defer buf.leave()
	if reader.IsTable() {
		readNarrowTable(reader, field, at, buf)
		return
	}
	count, elements, ok := reader.List()
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
		readNarrowBody(reader, body, field.sub.isWide, field.sub,
			unsafe.Add(data, uintptr(index)*field.stride), buf)
		if reader.Err() != nil {
			return
		}
	}
}

// readNarrowTable reads a table under a narrow parent, whose columns are keyed
// at the row type's width. See table.go.
func readNarrowTable(reader *wire.Reader, field *planField, at unsafe.Pointer, buf *scratch) {
	rows, body, ok := reader.Table()
	if !ok {
		return
	}
	if field.sub.isWide {
		columns := wire.NewReader8(body)
		readColumns(&columns, field, at, rows, buf)
		reader.Fail(columns.Err())
		return
	}
	if err := takeRows(&buf.rowsLeft, rows, len(field.sub.fields)); err != nil {
		reader.Fail(err)
		return
	}
	scratchColumns := buf.reserve(rows, field.sub.hasStringColumn())
	newSlice(field, at, rows)
	data := (*sliceHeader)(at).data
	columns := wire.NewReader(body)
	for columns.More() {
		key := columns.Key()
		column := field.sub.find(key)
		if column == nil {
			// A narrow key cannot be skipped, so an unknown column ends the
			// table rather than being stepped over.
			reader.Fail(fmt.Errorf(
				"a table holds column id %d, which the row type does not declare, "+
					"and a narrow key cannot be skipped", int(key)+1))
			return
		}
		if column.op == opString {
			scratchColumns.strings = columns.Strings(scratchColumns.strings[:0])
			if columns.Err() == nil && len(scratchColumns.strings) != rows {
				columns.Fail(errColumnLength)
			}
			scatterStrings(scratchColumns.strings, data, field.stride, column.offset, rows)
			continue
		}
		scratchColumns.ints = columns.Column(rows, scratchColumns.ints[:0])
		scatterInts(scratchColumns.ints, data, field.stride, column, rows)
	}
	reader.Fail(columns.Err())
}

func readNarrowMap(reader *wire.Reader, field *planField, at unsafe.Pointer, buf *scratch) {
	if field.valueKind == mapAny {
		// A map of dynamic values is always wide. See errBadMapValue.
		reader.Fail(errBadMapValue(field.valueKind))
		return
	}
	count, entries, ok := reader.Map()
	if !ok {
		return
	}
	if !buf.enter() {
		reader.Fail(buf.err)
		return
	}
	defer buf.leave()
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
		reader.Fail(setInt(into, reader.ElementInt()))
	case mapUint:
		reader.Fail(setUint(into, reader.ElementUint()))
	case mapFloat32, mapFloat64:
		setFloatFromReversed(into, reader.ElementUint())
	case mapBool:
		into.SetBool(reader.ElementUint() == 1)
	default:
		reader.Fail(errBadMapValue(kind))
	}
}
