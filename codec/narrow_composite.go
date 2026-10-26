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
// schema's to know, and the list's own length is what a reader that does not
// know the field steps over — which is a byte per element, and the reason a
// narrow list of small structs is smaller than a wide one rather than merely
// equal.

import (
	"reflect"
	"unsafe"

	"github.com/ivanjoz/colbin/column"
	"github.com/ivanjoz/colbin/internal/wirefmt"
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
	if program := field.sub.rows; program != nil && useRowPrograms && !Packed5() {
		// What appendNarrowElement and writePlan do for each element, with the
		// depth taken once for them all: it is the same depth for every one.
		if !buf.enter() {
			writer.Close(list)
			return
		}
		if program.numeric && useRowKernel {
			writer.Buffer = program.writeRows(writer.Buffer, slice.data, slice.len, field.stride)
			buf.leave()
			writer.Close(list)
			return
		}
		for index := range slice.len {
			at := unsafe.Add(slice.data, uintptr(index)*field.stride)
			element := writer.OpenElement()
			if program.wide {
				wide := wire.Writer8{Buffer: writer.Buffer}
				program.writeWide(&wide, at, buf)
				writer.Buffer = wide.Buffer
			} else {
				program.write(writer, at, buf)
			}
			writer.CloseElement(element)
		}
		buf.leave()
		writer.Close(list)
		return
	}
	for index := range slice.len {
		appendNarrowElement(writer, field.sub, unsafe.Add(slice.data, uintptr(index)*field.stride), buf)
	}
	writer.Close(list)
}

// appendNarrowElement writes one struct as a narrow list element or map value:
// a length and a key run, with no descriptor, so its key width is the schema's
// to say.
func appendNarrowElement(writer *wire.Writer, plan *typePlan, at unsafe.Pointer, buf *scratch) {
	element := writer.OpenElement()
	if plan.isWide {
		appendWideInto(writer, plan, at, buf)
	} else {
		writePlan(writer, plan, at, buf)
	}
	writer.CloseElement(element)
}

// appendNarrowTable writes a table under a narrow parent, its columns keyed at
// the row type's width, and in groups past wirefmt.TableGroupRows rows. See
// table.go.
func appendNarrowTable(writer *wire.Writer, field *planField, at unsafe.Pointer, buf *scratch) {
	slice := (*sliceHeader)(at)
	if groupRows := wirefmt.TableGroupRows; slice.len > groupRows {
		mark := writer.OpenGroupedTable(field.key, slice.len, groupRows)
		writer.Buffer = appendGroups(writer.Buffer, field, slice, groupRows, field.sub.isWide, buf)
		writer.Close(mark)
		return
	}
	mark := writer.OpenTable(field.key, slice.len)
	writer.Buffer = appendTableColumns(writer.Buffer, field, slice, field.sub.isWide, buf)
	writer.Close(mark)
}

// appendNarrowColumns writes a narrow table's columns for the rows of slice.
func appendNarrowColumns(writer *wire.Writer, plan *typePlan, slice *sliceHeader, stride uintptr, buf *scratch) {
	rows := slice.len
	var narrow []int32
	var ints []int64
	var strings []string
	var payloads []int
	var scans []column.Scan
	if useScannedTables && column.Scanned(rows) {
		narrow, ints, strings, payloads, scans = gatherScanned(plan, slice, stride, buf)
	} else {
		narrow, ints, strings, payloads = gatherTable(plan, slice, stride, buf)
	}
	nulls := buf.columns.nulls
	var choices []dictColumn
	if buf.dict != nil {
		choices = buf.dict.tableIDs(plan, strings, rows)
	}
	for index := range plan.fields {
		field := &plan.fields[index]
		if field.op == opPointer {
			writePointerColumn(writer, field.key, &nulls[0], rows, field.elemOp == opString)
			nulls = nulls[1:]
			continue
		}
		if field.op == opString {
			if len(choices) != 0 {
				choice := &choices[0]
				choices = choices[1:]
				if !choice.refused {
					writer.DictColumn(field.key, choice.number, choice.ids)
					payloads = payloads[1:]
					strings = strings[rows:]
					continue
				}
			}
			writer.StringColumnPayload(field.key, strings[:rows:rows], payloads[0])
			payloads = payloads[1:]
			strings = strings[rows:]
			continue
		}
		if field.op.fitsInt32() {
			if scans != nil {
				writer.Column32Scanned(field.key, narrow[:rows:rows], scans[0])
				scans = scans[1:]
			} else {
				writer.Column32(field.key, narrow[:rows:rows])
			}
			narrow = narrow[rows:]
			continue
		}
		if scans != nil {
			writer.ColumnScanned(field.key, ints[:rows:rows], scans[0])
			scans = scans[1:]
		} else {
			writer.Column(field.key, ints[:rows:rows])
		}
		ints = ints[rows:]
	}
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
	if field.typedMap != nil {
		field.typedMap.appendNarrow(writer, field.key, at)
		return
	}
	holder := structHolder(field)
	mark := writer.OpenMap(field.key, count)
	for _, key := range sortedMapKeys(value) {
		if buf.err != nil {
			break
		}
		writeNarrowMapValue(writer, field.keyKind, key)
		if field.valueKind == mapStruct {
			holder.Set(value.MapIndex(key))
			appendNarrowElement(writer, field.sub, holder.Addr().UnsafePointer(), buf)
			continue
		}
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
	if readsByProgram(plan, wideKeys) {
		parent.Fail(readByProgram(body, plan, at, buf))
		return
	}
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

// readNarrowRun is the narrow twin of readRun, and like it steps over a key the
// type does not declare: the nibble sizes the field, composite or not.
func readNarrowRun(reader *wire.Reader, plan *typePlan, record unsafe.Pointer, buf *scratch) {
	if !buf.enter() {
		reader.Fail(buf.err)
		return
	}
	defer buf.leave()
	readNarrowFields(reader, plan, record, buf)
}

// readsByProgram is whether a body of plan's, at the key width wideKeys says,
// is read by its program: one compiled for that width.
func readsByProgram(plan *typePlan, wideKeys bool) bool {
	return plan.rows != nil && plan.rows.wide == wideKeys && useRowPrograms
}

// readByProgram is readNarrowRun or readRun through the plan's program,
// returning the error the run would have failed its reader with.
func readByProgram(body []byte, plan *typePlan, record unsafe.Pointer, buf *scratch) error {
	if !buf.enter() {
		return buf.err
	}
	defer buf.leave()
	if plan.rows.wide {
		return plan.rows.readWide(body, record, buf)
	}
	return plan.rows.read(body, record, buf)
}

// readNarrowFields is readNarrowRun's loop, at the depth the caller has taken.
func readNarrowFields(reader *wire.Reader, plan *typePlan, record unsafe.Pointer, buf *scratch) {
	// A writer emits fields in the plan's order, omitting the zero ones, so the
	// field after the last one read is checked before the table: one compare
	// against a key the field read is about to load anyway.
	fields := plan.fields
	next := 0
	for reader.More() {
		key := reader.Key()
		if next < len(fields) && fields[next].key == key {
			readField(reader, &fields[next], record, buf)
			next++
			continue
		}
		index := plan.findIndex(key)
		if index < 0 {
			if !reader.Skip() {
				return
			}
			continue
		}
		readField(reader, &fields[index], record, buf)
		next = index + 1
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
	settle := newSlice(field, at, count) && field.sub.reset.keeps()
	data := (*sliceHeader)(at).data
	for index := range count {
		body, ok := elements.Element()
		if !ok {
			reader.Fail(elements.Err())
			return
		}
		element := unsafe.Add(data, uintptr(index)*field.stride)
		readNarrowBody(reader, body, field.sub.isWide, field.sub, element, buf)
		if settle {
			field.sub.reset.settle(element)
		}
		if reader.Err() != nil {
			return
		}
	}
}

// readNarrowTable reads a table under a narrow parent, whose columns are keyed
// at the row type's width. See table.go.
func readNarrowTable(reader *wire.Reader, field *planField, at unsafe.Pointer, buf *scratch) {
	rows, groupRows, body, ok := reader.Table()
	if !ok {
		return
	}
	reader.Fail(readTable(field, at, rows, groupRows, body, field.sub.isWide, buf))
}

// readNarrowColumns fills rows rows from data on from a table's four-bit-keyed
// columns.
func readNarrowColumns(field *planField, data unsafe.Pointer, rows int, body []byte, buf *scratch) error {
	table := startTable(field, data, rows, buf)
	columns := wire.NewReader(body)
	for columns.More() {
		column := field.sub.find(columns.Key())
		if column == nil {
			// A column the row type no longer declares, stepped over like any
			// other unknown key.
			if !columns.Skip() {
				break
			}
			continue
		}
		if column.op == opPointer {
			readPointerColumn(&columns, column, data, field.stride, rows, buf)
			continue
		}
		if column.op == opString {
			if buf.dict != nil && columns.IsDictColumn() {
				if buf.dict.readDictColumn(&columns, rows, table.stringSlot()) {
					table.decoded(column)
				}
				continue
			}
			got := columns.Strings(table.stringSlot())
			if len(got) != rows {
				if columns.Err() == nil {
					columns.Fail(errColumnLength)
				}
				continue
			}
			table.decoded(column)
			continue
		}
		if got := columns.Column(rows, table.intSlot()); len(got) == rows {
			table.decoded(column)
		}
	}
	table.finish()
	return columns.Err()
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
	if field.typedMap != nil {
		reader.Fail(field.typedMap.readNarrow(&entries, count, at))
		return
	}
	target := reflect.NewAt(field.sliceType, at).Elem()
	built := reflect.MakeMapWithSize(field.sliceType, count)
	key := reflect.New(field.sliceType.Key()).Elem()
	value := reflect.New(field.sliceType.Elem()).Elem()
	for range count {
		readNarrowMapValue(&entries, field.keyKind, key)
		if field.valueKind == mapStruct {
			// Zeroed per entry for the reason readMapField gives.
			value.SetZero()
			if body, ok := entries.Element(); ok {
				readNarrowBody(&entries, body, field.sub.isWide, field.sub, value.Addr().UnsafePointer(), buf)
			}
		} else {
			readNarrowMapValue(&entries, field.valueKind, value)
		}
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
