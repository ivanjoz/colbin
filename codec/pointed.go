package codec

// Slices of pointers to structs: `[]*T`.
//
// The pointer is not on the wire. A `[]*T` is written as the `[]T` of what its
// elements point at — the same list, the same table past the threshold, the
// same op in a schema section — so every reader there is reads one already, and
// a Go type may declare the field either way and read the other's bytes. It is
// the shape protoc-gen-go gives every repeated message, and the one a tree
// takes when it is written `Kids []*Node`.
//
// A list is written element by element from where each one lies, by the walks
// a `[]T`'s list takes. A table's walks take a base and a stride — its gather
// kernels among them — so its rows are copied into scratch, side by side, and
// written from there by the functions a `[]T` goes through. Reading is those
// functions too, into one array of T, which the pointers are then pointed into.
//
// The rows are not written where they lie, even when they lie side by side:
// that is how two allocations look as well as one, and walking from the first
// of them into the second is not something a pointer may do.
//
// A nil element has no form: an element is a struct's body. It is written as an
// empty one and read back as a pointer to a zero struct, as a nil map value is
// (maps.go).

import (
	"reflect"
	"unsafe"

	"github.com/ivanjoz/colbin/wire"
)

// opStructPtrs is a []*T where T is a struct. It is not a wire op: it is
// opCount, the first number a section cannot name, and a section calls the
// field opStructs (schemaOp). It is beside the wire ops rather than up with
// opConvert so that the generated switches stay one dense table.
const opStructPtrs fieldOp = opCount

// structPointersOf says a type is a slice of pointers to structs, and which
// struct.
func structPointersOf(fieldType reflect.Type) (reflect.Type, bool) {
	if fieldType.Kind() != reflect.Slice || fieldType.Elem().Kind() != reflect.Pointer {
		return nil, false
	}
	record := fieldType.Elem().Elem()
	return record, record.Kind() == reflect.Struct
}

// pointedRows copies what the pointers at at point at into scratch, side by
// side, and returns them as the slice of structs they then are, with the mark
// to hand releasePointed when the rows have been written.
//
// The copies are bytes in memory the collector does not scan, holding whatever
// pointers the structs hold. That is sound for as long as the encode runs,
// because every one of them is held by the value being encoded as well — the
// argument a list column's rows rest on (listcolumn.go).
//
// The scratch is a stack. An element may hold a slice of pointers of its own,
// which is copied above the rows it is being written from; growing the scratch
// for it moves nothing, because the slice header returned here keeps the array
// it was cut from.
func pointedRows(field *planField, at unsafe.Pointer, buf *scratch) (rows sliceHeader, mark int) {
	pointers := *(*[]unsafe.Pointer)(at)
	columns := buf.reserve(0, false)
	mark = len(columns.pointed)
	size := uintptr(len(pointers)) * field.stride
	words := int((size + 7) / 8)
	if cap(columns.pointed)-mark < words {
		// A new array rather than a grown one: what is below the mark belongs
		// to the callers above, and they hold the old one.
		grown := make([]uint64, mark, max(2*cap(columns.pointed), mark+words))
		columns.pointed = grown
	}
	columns.pointed = columns.pointed[:mark+words]
	if words == 0 {
		// A struct of no size: every element is the same nothing.
		return sliceHeader{data: at, len: len(pointers), cap: len(pointers)}, mark
	}
	data := unsafe.Pointer(&columns.pointed[mark])
	copyPointed(data, pointers, field.stride)
	return sliceHeader{data: data, len: len(pointers), cap: len(pointers)}, mark
}

// copyPointed copies what each pointer points at to into, stride bytes a row
// and side by side, a nil one as zeros. into holds them all.
func copyPointed(into unsafe.Pointer, pointers []unsafe.Pointer, stride uintptr) {
	if stride%8 == 0 && stride <= maxWordRow {
		copyWordRows(into, pointers, stride/8)
		return
	}
	rows := unsafe.Slice((*byte)(into), uintptr(len(pointers))*stride)
	for index, pointer := range pointers {
		row := rows[uintptr(index)*stride:][:stride]
		if pointer == nil {
			clear(row)
			continue
		}
		copy(row, unsafe.Slice((*byte)(pointer), stride))
	}
}

// maxWordRow is the largest row copyWordRows takes. Past it a memmove per row
// is as fast, and its call is a smaller share of the row.
const maxWordRow = 128

// copyWordRows copies rows of words words each, the one the pointers point at
// after the other, a nil one as zeros. A memmove per row was a call and two
// slice headers for what is a handful of loads and stores: on a thousand
// rows of five words it was most of what the copy cost.
//
// Each row's address is taken from the start rather than stepped to: a step
// past the last row is a pointer past the allocation, which checkptr refuses.
func copyWordRows(into unsafe.Pointer, pointers []unsafe.Pointer, words uintptr) {
	for index, pointer := range pointers {
		row := unsafe.Add(into, uintptr(index)*words*8)
		if pointer == nil {
			for word := range words {
				*(*uint64)(unsafe.Add(row, word*8)) = 0
			}
			continue
		}
		for word := range words {
			*(*uint64)(unsafe.Add(row, word*8)) = *(*uint64)(unsafe.Add(pointer, word*8))
		}
	}
}

// releasePointed gives back what pointedRows took.
func releasePointed(buf *scratch, mark int) {
	buf.columns.pointed = buf.columns.pointed[:mark]
}

// appendStructPtrsField writes a []*T under key as the []T it points at, and
// nothing at all when it is empty.
//
// A list is written from where each element lies: an element is a key run of
// its own, written from its own address, so it needs no neighbour. A table is
// written from the rows copied side by side, because its gather reads every
// row once per column and the copy reads each scattered row once.
func appendStructPtrsField(writer *wire.Writer8, field *planField, at unsafe.Pointer, buf *scratch) {
	pointers := *(*[]unsafe.Pointer)(at)
	if len(pointers) == 0 {
		return
	}
	if (field.sub.canTable && len(pointers) >= tableThreshold) || hasNil(pointers) {
		rows, mark := pointedRows(field, at, buf)
		appendStructsField(writer, field, unsafe.Pointer(&rows), buf)
		releasePointed(buf, mark)
		return
	}
	// What appendStructsField does for a list, at the pointers' addresses.
	if !buf.enter() {
		return
	}
	defer buf.leave()
	list := writer.OpenList(field.key, len(pointers))
	if program := field.sub.rows; program != nil && useRowPrograms && !Packed5() {
		if !buf.enter() {
			writer.Close(list)
			return
		}
		for _, element := range pointers {
			if program.wide {
				mark := writer.OpenElementStructWide()
				program.writeWide(writer, element, buf)
				writer.Close(mark)
				continue
			}
			mark := writer.OpenElementStruct()
			narrow := wire.Writer{Buffer: writer.Buffer}
			program.write(&narrow, element, buf)
			writer.Buffer = narrow.Buffer
			writer.Close(mark)
		}
		buf.leave()
		writer.Close(list)
		return
	}
	for _, element := range pointers {
		appendStructElement(writer, field.sub, element, buf)
	}
	writer.Close(list)
}

// appendNarrowStructPtrs is appendStructPtrsField at four key bits, and what
// appendNarrowStructs does for a list.
func appendNarrowStructPtrs(writer *wire.Writer, field *planField, at unsafe.Pointer, buf *scratch) {
	pointers := *(*[]unsafe.Pointer)(at)
	if len(pointers) == 0 {
		return
	}
	if (field.sub.canTable && len(pointers) >= tableThreshold) || hasNil(pointers) {
		rows, mark := pointedRows(field, at, buf)
		appendNarrowStructs(writer, field, unsafe.Pointer(&rows), buf)
		releasePointed(buf, mark)
		return
	}
	if !buf.enter() {
		return
	}
	defer buf.leave()
	if len(pointers) == 1 {
		list := writer.OpenSingle(field.key)
		appendNarrowBody(writer, field.sub, pointers[0], buf)
		writer.Close(list)
		return
	}
	list := writer.OpenList(field.key, len(pointers))
	program, tree := field.sub.rows, field.sub.tree
	switch {
	case program != nil && useRowPrograms && !Packed5():
		// The numeric row kernel takes a stride, so each element goes through
		// the program on its own; the bytes are the kernel's.
		if !buf.enter() {
			writer.Close(list)
			return
		}
		for _, element := range pointers {
			mark := writer.OpenElement()
			if program.wide {
				wide := wire.Writer8{Buffer: writer.Buffer}
				program.writeWide(&wide, element, buf)
				writer.Buffer = wide.Buffer
			} else {
				program.write(writer, element, buf)
			}
			writer.CloseElement(mark)
		}
		buf.leave()
	case tree != nil && useRowPrograms && !Packed5():
		if !buf.enter() {
			writer.Close(list)
			return
		}
		for _, element := range pointers {
			mark := writer.OpenElement()
			tree.write(writer, element, buf)
			writer.CloseElement(mark)
		}
		buf.leave()
	default:
		for _, element := range pointers {
			appendNarrowElement(writer, field.sub, element, buf)
		}
	}
	writer.Close(list)
}

// hasNil says some element is nil, which only the copy writes: as an empty
// struct, from zeroed scratch.
func hasNil(pointers []unsafe.Pointer) bool {
	for _, pointer := range pointers {
		if pointer == nil {
			return true
		}
	}
	return false
}

// readStructPtrsField reads a list or a table into one array of T and points
// the field's elements into it: two allocations whatever the count, where a
// pointee each would be one a row. The price is the one a nullable column
// pays — an element kept alive keeps the array alive.
func readStructPtrsField(reader *wire.Reader8, field *planField, at unsafe.Pointer, buf *scratch) {
	// The field is read as the []T it is on the wire, where it sits, and then
	// turned into the pointers. Whatever array it kept through a reset is an
	// array of pointers, which the rows must not be read into.
	*(*sliceHeader)(at) = sliceHeader{}
	readStructsField(reader, field, at, buf)
	pointInto(field, at)
}

// readNarrowStructPtrs is readStructPtrsField at four key bits.
func readNarrowStructPtrs(reader *wire.Reader, field *planField, at unsafe.Pointer, buf *scratch) {
	*(*sliceHeader)(at) = sliceHeader{}
	readNarrowStructs(reader, field, at, buf)
	pointInto(field, at)
}

// pointInto replaces the []T a read left in a []*T field with pointers to its
// elements. It runs whether or not the read failed: a failed decode leaves
// rows whose tail is zero (newSlice), and it must not leave them in a field of
// another type.
func pointInto(field *planField, at unsafe.Pointer) {
	rows := *(*sliceHeader)(at)
	if rows.data == nil {
		*(*[]unsafe.Pointer)(at) = nil
		return
	}
	pointers := make([]unsafe.Pointer, rows.len)
	for index := range pointers {
		pointers[index] = unsafe.Add(rows.data, uintptr(index)*field.stride)
	}
	*(*[]unsafe.Pointer)(at) = pointers
}

// windowPointers is readListColumn's last step for a []*T of each row: one
// array of pointers into the elements, and each row's slice a window of it,
// capped at its own end, as each row's []T is a window of the elements.
func windowPointers(base unsafe.Pointer, stride uintptr, lengths []int64, elements sliceHeader, elementSize uintptr) {
	pointers := make([]unsafe.Pointer, elements.len)
	for index := range pointers {
		pointers[index] = unsafe.Add(elements.data, uintptr(index)*elementSize)
	}
	at := 0
	for row, n := range lengths {
		held := (*[]unsafe.Pointer)(unsafe.Add(base, uintptr(row)*stride))
		if n == 0 {
			*held = nil
			continue
		}
		*held = pointers[at : at+int(n) : at+int(n)]
		at += int(n)
	}
}
