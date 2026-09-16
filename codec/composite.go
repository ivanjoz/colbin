package codec

// Nested structs and slices of them, over the composites in wire/composite.go.
//
// A composite carries a byte length, and a byte length is a wide-key idea: four
// descriptor bits have no room for a class, so nothing narrow can size a field
// it cannot classify. A type with a nested struct therefore goes wide, the same
// way a type with a field id past sixteen does — it is not a mode, it is the
// width the type needs.
//
// # Recursion
//
// A plan holds a pointer to its children's plans, so a type that reaches itself
// would build forever. The cache is filled with the plan *before* its fields are
// walked, so a type that comes back round finds the one under construction and
// points at it. That makes `type Node struct{ Kids []Node }` a plan with a cycle
// in it and a decoder that terminates on the data rather than on the type.

import (
	"errors"
	"reflect"
	"unsafe"

	"github.com/ivanjoz/colbin/wire"
)

// appendStructField writes a nested struct under key.
func appendStructField(writer *wire.Writer8, field *planField, at unsafe.Pointer, buf *scratch) {
	if field.sub.isWide {
		mark := writer.OpenStructWide(field.key)
		appendWide(writer, field.sub, at, buf)
		writer.Close(mark)
		return
	}
	// A nested run whose ids all fit four bits uses them, which is a byte per
	// present field: the key width is a property of the scope, so the outer run
	// being wide says nothing about the inner one.
	mark := writer.OpenStruct(field.key)
	appendNarrowInto(writer, field.sub, at, buf)
	writer.Close(mark)
}

// appendPointerStructField writes a *T under key, and nothing at all when it is
// nil.
//
// Nil and non-nil-to-zero stay distinguishable without a null code, which is the
// whole reason this op is cheap: a struct body is written whether or not it is
// empty, so an absent key is nil and a present key with an empty body is a
// pointer to a zero value. See pointer.go.
func appendPointerStructField(writer *wire.Writer8, field *planField, at unsafe.Pointer, buf *scratch) {
	pointee := *(*unsafe.Pointer)(at)
	if pointee == nil {
		return
	}
	appendStructField(writer, field, pointee, buf)
}

// readPointerStructField allocates a pointee and reads the body into it. The
// record was zeroed first, so a key the message omits leaves the field nil.
func readPointerStructField(reader *wire.Reader8, field *planField, at unsafe.Pointer, buf *scratch) {
	pointee := reflect.New(field.sliceType.Elem()).UnsafePointer()
	readStructField(reader, field, pointee, buf)
	*(*unsafe.Pointer)(at) = pointee
}

// appendNarrowInto writes a narrow key run onto the wide writer's buffer. Both
// writers are a []byte and nothing else, so this is a hand-off rather than a
// copy.
func appendNarrowInto(writer *wire.Writer8, plan *typePlan, at unsafe.Pointer, buf *scratch) {
	narrow := wire.Writer{Buffer: writer.Buffer}
	writePlan(&narrow, plan, at, buf)
	writer.Buffer = narrow.Buffer
}

// sliceHeader is what a []T looks like in memory, which is how a plan reaches
// the elements of a slice whose type it only knows reflectively.
type sliceHeader struct {
	data unsafe.Pointer
	len  int
	cap  int
}

// appendStructsField writes a slice of nested structs under key, and nothing at
// all when it is empty — an absent key means an empty slice, exactly as it means
// a zero scalar.
func appendStructsField(writer *wire.Writer8, field *planField, at unsafe.Pointer, buf *scratch) {
	slice := (*sliceHeader)(at)
	if slice.len == 0 {
		return
	}
	if field.sub.canTable && slice.len >= tableThreshold {
		appendTableField(writer, field, at, buf)
		return
	}
	list := writer.OpenList(field.key, slice.len)
	for index := range slice.len {
		at := unsafe.Add(slice.data, uintptr(index)*field.stride)
		if field.sub.isWide {
			element := writer.OpenElementStructWide()
			appendWide(writer, field.sub, at, buf)
			writer.Close(element)
			continue
		}
		element := writer.OpenElementStruct()
		appendNarrowInto(writer, field.sub, at, buf)
		writer.Close(element)
	}
	writer.Close(list)
}

// readStructField reads a nested struct into an already-zeroed field.
func readStructField(reader *wire.Reader8, field *planField, at unsafe.Pointer, buf *scratch) {
	body, wideKeys, ok := reader.StructBody()
	if !ok {
		return
	}
	readBody(reader, body, wideKeys, field.sub, at, buf)
}

// readBody fills a record from a nested run at whichever key width its
// descriptor declared.
func readBody(parent *wire.Reader8, body []byte, wideKeys bool, plan *typePlan, at unsafe.Pointer, buf *scratch) {
	if wideKeys {
		sub := wire.NewReader8(body)
		readRun(&sub, plan, at, buf)
		parent.Fail(sub.Err())
		return
	}
	sub := wire.NewReader(body)
	for sub.More() {
		field := plan.find(sub.Key())
		if field == nil {
			// A narrow key cannot be skipped, so an unknown one ends the run
			// rather than being stepped over. See unmarshalNarrow.
			parent.Fail(wire.ErrBadDescriptor)
			return
		}
		readField(&sub, field, at, buf)
	}
	parent.Fail(sub.Err())
}

// readStructsField reads a slice of nested structs, allocating it at the count
// the message declares.
func readStructsField(reader *wire.Reader8, field *planField, at unsafe.Pointer, buf *scratch) {
	// The writer chose between a list and a table on the row count, and the two
	// are different classes, so the reader dispatches on what is actually there
	// rather than on anything it was told.
	if reader.IsTable() {
		readTableField(reader, field, at, buf)
		return
	}
	count, elements, ok := reader.List()
	if !ok {
		return
	}
	newSlice(field, at, count)
	data := (*sliceHeader)(at).data
	for index := range count {
		body, wideKeys, ok := elements.ElementStructBody()
		if !ok {
			reader.Fail(elements.Err())
			return
		}
		readBody(reader, body, wideKeys, field.sub,
			unsafe.Add(data, uintptr(index)*field.stride), buf)
	}
}

// newSlice allocates a slice of count elements into the field.
//
// It is published into the field before it is filled, not after: a
// reflect.MakeSlice result is not addressable, and the field is what keeps the
// backing array reachable while the elements are written into it. A failure part
// way therefore leaves a slice whose tail is zero, which is why a caller must
// check Err before reading one.
func newSlice(field *planField, at unsafe.Pointer, count int) {
	slice := reflect.MakeSlice(field.sliceType, count, count)
	*(*sliceHeader)(at) = sliceHeader{data: slice.UnsafePointer(), len: count, cap: count}
}

// readRun fills a record from a key run, which is what a nested struct's body
// is. It is the shared body of unmarshalWide and of every nested read under it.
func readRun(reader *wire.Reader8, plan *typePlan, record unsafe.Pointer, buf *scratch) {
	for reader.More() {
		field := plan.find(reader.Key())
		if field == nil {
			if !reader.Skip() {
				return
			}
			continue
		}
		readWideField(reader, field, record, buf)
	}
}

// errNotComposite says the type is not a struct, a slice of them or a pointer to
// one — so the caller should go on and try a map, a pointer to a scalar, or the
// scalar table.
//
// It is a sentinel rather than any error because the caller has to tell the two
// apart. A struct that *is* a composite and fails to plan — one field of it
// holding a type the format does not carry — used to come back indistinguishable
// from "not one of mine", and the caller would fall through and report whatever
// the scalar table said about the outer type. That is how a bad field inside an
// element got reported as `[]T` not being a carriable slice, naming the one type
// in the message that was fine.
var errNotComposite = errors.New("not a composite")

// compositeOpFor resolves a struct, a slice of structs or a pointer to a struct
// to its op and its child plan. It returns errNotComposite for anything that is
// not one, so the scalar table stays the first thing tried.
func compositeOpFor(fieldType reflect.Type, building map[reflect.Type]*typePlan) (fieldOp, *typePlan, reflect.Type, uintptr, error) {
	switch fieldType.Kind() {
	case reflect.Struct:
		sub, err := planForBuilding(fieldType, building)
		if err != nil {
			return 0, nil, nil, 0, err
		}
		return opStruct, sub, nil, 0, nil
	case reflect.Slice:
		if element := fieldType.Elem(); element.Kind() == reflect.Struct {
			sub, err := planForBuilding(element, building)
			if err != nil {
				return 0, nil, nil, 0, err
			}
			return opStructs, sub, fieldType, element.Size(), nil
		}
	case reflect.Pointer:
		if element := fieldType.Elem(); element.Kind() == reflect.Struct {
			sub, err := planForBuilding(element, building)
			if err != nil {
				return 0, nil, nil, 0, err
			}
			// sliceType carries the pointer type, which is what the reader allocates
			// a pointee from. There is no stride: there is one pointee, not a run.
			return opPointerStruct, sub, fieldType, 0, nil
		}
	}
	return 0, nil, nil, 0, errNotComposite
}
