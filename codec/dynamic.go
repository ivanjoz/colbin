package codec

// `any`: a value the type does not describe.
//
// Everywhere else in this package the Go type is the schema and the wire carries
// none of it. `map[string]any` breaks that by construction — the values have no
// declared type, only the one each of them happens to have — so those values
// carry their own, in the self-describing forms wire/dynamic.go adds.
//
// # Three shapes, one encoder
//
//	Extra any             opAny     the value, whatever it is
//	Rows  []any           opAnys    a LIST of them
//	Extra map[string]any  opMap with valueKind mapAny
//
// # The shape this is for
//
// A service answering with `map[string]any{"rows": []Sale{...}, "total": 91}` is
// the case that matters, and the naive encoding of it is a disaster: a list of
// maps writes every field *name* on every row, which is the one thing this
// format exists not to do. For a six-field record that is about thirty bytes a
// row of pure repetition.
//
// So a slice of structs in a dynamic position is written as a TYPED value: a tag
// naming a struct the *section* describes, and then the rows exactly as a typed
// field writes them — a LIST, or a TABLE with the columns transposed and run
// through the column codec. Two bytes of framing for the whole array, one struct
// def in the section, and rows that cost what they cost anywhere else.
//
// # Which means the section is built from the value
//
// A struct only reachable through an `any` is not in the type, so the table
// cannot be resolved from the type alone. `marshalDynamic` therefore writes the
// body first, with the section builder open, and emits the section after — the
// indices a TYPED tag names are assigned as the walk meets them.
//
// That is why **a dynamic struct needs MarshalSelfDescribing**. Plain Marshal
// has no section to put a def in, so it falls back to writing the struct as a
// map of its field names, which is correct, self-describing and several times
// larger. The two are different encodings of the same value and both decode; the
// difference is bytes, and it is worth knowing about rather than discovering.
//
// # What it costs
//
// A `map[string]any` writes its keys as strings, per entry, per message. That is
// the cost this format exists to avoid, so this is the escape hatch for a subtree
// whose shape is genuinely unknown — not a way to avoid declaring a type. A
// struct with the same fields is smaller, faster, and says what it is.

import (
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"unsafe"

	"github.com/ivanjoz/colbin/wire"
)

// maxDynamicDepth bounds how deep the encoder will follow a value.
//
// Nesting here is data rather than type, and `m["self"] = m` is a value a caller
// can build by accident. The walk has the same bound for the same reason; the
// two are the same number so that anything this writes, that can read.
const maxDynamicDepth = maxSchemaDepth

var errDynamicDepth = fmt.Errorf(
	"colbin: a dynamic value nests more than %d deep, or holds itself", maxDynamicDepth)

// --- encoding ----------------------------------------------------------------

// appendAnyField writes an `any` field: nothing at all when it is nil, since an
// absent key means the zero value and nil is what the zero of `any` is.
func appendAnyField(writer *wire.Writer8, field *planField, at unsafe.Pointer, buf *scratch) {
	value := *(*any)(at)
	if value == nil {
		return
	}
	writer.Key(field.key)
	appendAnyValue(writer, value, buf)
}

// appendAnysField writes a `[]any` field as a LIST of dynamic values.
func appendAnysField(writer *wire.Writer8, field *planField, at unsafe.Pointer, buf *scratch) {
	slice := *(*[]any)(at)
	if len(slice) == 0 {
		return
	}
	writer.Key(field.key)
	appendAnyList(writer, slice, buf)
}

// appendAnyValue writes one value of unknown type.
//
// The type switch is not an optimisation over the reflective fallback below it,
// or not only: `map[string]any`, `[]any`, `string` and the integers are what a
// dynamic value nearly always is, and reaching them without a reflect.Value at
// all is what keeps this from being slower than it has to be.
func appendAnyValue(writer *wire.Writer8, value any, buf *scratch) {
	switch typed := value.(type) {
	case nil:
		writer.ElementNull()
	case bool:
		writer.ElementBool(typed)
	case string:
		writer.ElementString(typed)
	case []byte:
		writer.ElementBytes(typed)
	case int:
		writer.ElementInt(int64(typed))
	case int8:
		writer.ElementInt(int64(typed))
	case int16:
		writer.ElementInt(int64(typed))
	case int32:
		writer.ElementInt(int64(typed))
	case int64:
		writer.ElementInt(typed)
	case uint:
		writer.ElementUint(uint64(typed))
	case uint16:
		writer.ElementUint(uint64(typed))
	case uint32:
		writer.ElementUint(uint64(typed))
	case uint64:
		writer.ElementUint(typed)
	case float32:
		writer.ElementFloat32(typed)
	case float64:
		writer.ElementFloat64(typed)
	case map[string]any:
		appendAnyStringMap(writer, typed, buf)
	case []any:
		appendAnyList(writer, typed, buf)
	default:
		appendAnyReflect(writer, reflect.ValueOf(value), buf)
	}
}

// appendAnyReflect is appendAnyValue for everything the type switch does not
// name: a named scalar type, a pointer, a struct, and every map and slice whose
// key or element is not `any`.
func appendAnyReflect(writer *wire.Writer8, value reflect.Value, buf *scratch) {
	if !value.IsValid() {
		writer.ElementNull()
		return
	}
	switch value.Kind() {
	case reflect.Bool:
		writer.ElementBool(value.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		writer.ElementInt(value.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Uintptr:
		writer.ElementUint(value.Uint())
	case reflect.Float32:
		writer.ElementFloat32(float32(value.Float()))
	case reflect.Float64:
		writer.ElementFloat64(value.Float())
	case reflect.String:
		writer.ElementString(value.String())
	case reflect.Pointer, reflect.Interface:
		if value.IsNil() {
			writer.ElementNull()
			return
		}
		appendAnyReflect(writer, value.Elem(), buf)
	case reflect.Slice, reflect.Array:
		appendAnySequence(writer, value, buf)
	case reflect.Map:
		appendAnyMap(writer, value, buf)
	case reflect.Struct:
		appendAnyStruct(writer, value, buf)
	default:
		buf.fail(fmt.Errorf("colbin: a %s has no form in a dynamic value", value.Type()))
		writer.ElementNull()
	}
}

// appendAnySequence writes a slice or an array: opaque bytes, a typed run of
// records, or a list of dynamic values.
func appendAnySequence(writer *wire.Writer8, value reflect.Value, buf *scratch) {
	if value.Kind() == reflect.Slice && value.IsNil() {
		writer.ElementNull()
		return
	}
	if value.Type().Elem().Kind() == reflect.Uint8 {
		writer.ElementBytes(bytesOf(value))
		return
	}
	if appendTypedSequence(writer, value, buf) {
		return
	}
	if appendBoxedSequence(writer, value.Len(), value.Index, buf) {
		return
	}
	if !buf.enter() {
		writer.ElementNull()
		return
	}
	defer buf.leave()
	mark := writer.OpenElementList(value.Len())
	for index := range value.Len() {
		appendAnyReflect(writer, value.Index(index), buf)
	}
	writer.Close(mark)
}

// appendBoxedSequence writes a sequence of *boxed* records as one typed run, and
// reports whether it could.
//
// `[]any` holding a thousand Sales is the same thousand records as `[]Sale`, and
// without this it costs half as much again: a type tag on every element, and no
// table, because each record is tagged and framed on its own. Measured on a
// six-field row it is 33.7 bytes against 22.4.
//
// What stands in the way is that the elements are not contiguous. An `any` is a
// type word and a data pointer, and the pointers go wherever the values were
// allocated — so there is no stride for the column gatherers to walk. This
// copies them into one slice of the concrete type and hands that to the ordinary
// writer, which costs an allocation and a memcpy of the rows to save the tags
// and win the transpose.
//
// It is the *sequence* that has to be uniform, not the type: a `[]any` of unlike
// things, or holding one nil, falls through to the list of dynamic values below.
func appendBoxedSequence(
	writer *wire.Writer8, count int, at func(int) reflect.Value, buf *scratch,
) bool {
	if buf.section == nil || count == 0 {
		return false
	}
	elemType := boxedStructType(count, at)
	if elemType == nil {
		return false
	}
	plan, err := planFor(elemType)
	if err != nil {
		return false
	}
	rows := reflect.MakeSlice(reflect.SliceOf(elemType), count, count)
	for index := range count {
		rows.Index(index).Set(boxedRecord(at(index)))
	}
	writer.ElementTyped(buf.section.structIndex(plan))
	slice := sliceHeader{data: rows.UnsafePointer(), len: count, cap: count}
	appendStructsElement(writer, plan, &slice, elemType.Size(), buf)
	return true
}

// boxedStructType is the struct type every element of a sequence holds, or nil
// when they do not all hold one.
//
// A pointer to a struct counts, and is dereferenced: `[]any{&sale, &sale}` is
// the same document as `[]any{sale, sale}` and should be the same bytes. A nil
// anywhere disqualifies the run, because null is not a record and the typed form
// has no way to write one.
func boxedStructType(count int, at func(int) reflect.Value) reflect.Type {
	var found reflect.Type
	for index := range count {
		element := boxedRecord(at(index))
		if !element.IsValid() || element.Kind() != reflect.Struct {
			return nil
		}
		if found == nil {
			found = element.Type()
			continue
		}
		if found != element.Type() {
			return nil
		}
	}
	return found
}

// boxedRecord unwraps a sequence element to the value it carries: out of the
// interface, and through a pointer.
//
// The two callers reach an element by different routes — `reflect.ValueOf` on a
// `[]any`'s element gives the concrete value, `Index` on a reflective sequence
// gives the interface holding it — so this takes either and answers the same
// thing. An invalid value is a nil element, which the caller refuses.
func boxedRecord(element reflect.Value) reflect.Value {
	if element.Kind() == reflect.Interface {
		element = element.Elem()
	}
	if element.Kind() == reflect.Pointer {
		if element.IsNil() {
			return reflect.Value{}
		}
		element = element.Elem()
	}
	return element
}

// bytesOf reaches a []byte without a copy where it can, which is every slice
// whose element type *is* byte rather than merely byte-sized.
func bytesOf(value reflect.Value) []byte {
	if value.Kind() == reflect.Slice && value.Type().Elem() == byteType {
		return value.Bytes()
	}
	out := make([]byte, value.Len())
	for index := range value.Len() {
		out[index] = uint8(value.Index(index).Uint())
	}
	return out
}

var byteType = reflect.TypeOf(byte(0))

// appendTypedSequence writes a slice of structs the cheap way, and reports
// whether it could.
//
// This is the whole point of the feature. `[]Sale` inside an `any` goes out as a
// tag naming a struct def and then the rows as a typed field writes them — the
// LIST or the TABLE, the column codec, all of it — rather than as a list of
// objects with the field names spelled out per row.
//
// It declines when there is no section open to describe the element type in, and
// the caller then writes a list of maps. See the file comment.
func appendTypedSequence(writer *wire.Writer8, value reflect.Value, buf *scratch) bool {
	if buf.section == nil || value.Kind() != reflect.Slice {
		return false
	}
	elemType := value.Type().Elem()
	if elemType.Kind() != reflect.Struct {
		return false
	}
	plan, err := planFor(elemType)
	if err != nil {
		// Not a type the format carries as a struct. The caller's fallback will
		// name the field that cannot go, which is a better error than this one.
		return false
	}
	writer.ElementTyped(buf.section.structIndex(plan))
	slice := sliceHeader{data: value.UnsafePointer(), len: value.Len(), cap: value.Cap()}
	appendStructsElement(writer, plan, &slice, elemType.Size(), buf)
	return true
}

// appendAnyStringMap is appendAnyMap for the shape a dynamic map nearly always
// has, reached without a reflect.Value per entry.
func appendAnyStringMap(writer *wire.Writer8, value map[string]any, buf *scratch) {
	if value == nil {
		writer.ElementNull()
		return
	}
	if !buf.enter() {
		writer.ElementNull()
		return
	}
	defer buf.leave()
	mark := writer.OpenElementMap(len(value))
	for _, key := range sortedKeys(value) {
		writer.ElementString(key)
		appendAnyValue(writer, value[key], buf)
	}
	writer.Close(mark)
}

// sortedKeys is why a dynamic map is written in order. Go's map iteration is
// deliberately random, so without this the same value encodes to different bytes
// every time — which rules out a golden vector, an ETag, and any two ports
// agreeing about a `map[string]any` at all.
//
// A typed map field does not sort, and that difference is deliberate: it has a
// key set the schema declares, it is usually small, and it is on a path where
// the allocation this costs would be paid per message for nothing.
func sortedKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// appendAnyMap writes any other map: the keys have to be renderable as names,
// which is what a JSON object needs and what a list or a struct is not.
func appendAnyMap(writer *wire.Writer8, value reflect.Value, buf *scratch) {
	if value.IsNil() {
		writer.ElementNull()
		return
	}
	if _, err := mapKindOf(value.Type().Key(), "key"); err != nil {
		buf.fail(fmt.Errorf("colbin: %w", err))
		writer.ElementNull()
		return
	}
	if !buf.enter() {
		writer.ElementNull()
		return
	}
	defer buf.leave()
	keys := value.MapKeys()
	sortMapKeys(keys)
	mark := writer.OpenElementMap(len(keys))
	for _, key := range keys {
		appendAnyReflect(writer, key, buf)
		appendAnyReflect(writer, value.MapIndex(key), buf)
	}
	writer.Close(mark)
}

// sortMapKeys orders a reflective key set the way sortedKeys orders a string
// one, and for the same reason.
func sortMapKeys(keys []reflect.Value) {
	sort.Slice(keys, func(a, b int) bool {
		switch keys[a].Kind() {
		case reflect.String:
			return keys[a].String() < keys[b].String()
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return keys[a].Int() < keys[b].Int()
		default:
			return keys[a].Uint() < keys[b].Uint()
		}
	})
}

// appendAnyStruct writes a struct: as a TYPED value when there is a section to
// name it in, and as a map of its field names when there is not.
// A struct reached through an `any` takes a depth level like every other
// container. `type Node struct{ Next any }` with `node.Next = &node` is a cycle
// the *type* does not have — planFor terminates on it — and only the value walk
// can see it.
func appendAnyStruct(writer *wire.Writer8, value reflect.Value, buf *scratch) {
	if buf.section != nil {
		if plan, err := planFor(value.Type()); err == nil {
			if !buf.enter() {
				writer.ElementNull()
				return
			}
			defer buf.leave()
			writer.ElementTyped(buf.section.structIndex(plan))
			appendStructElement(writer, plan, addressableCopy(value), buf)
			return
		}
	}
	appendStructAsMap(writer, value, buf)
}

// appendStructAsMap writes a struct the way encoding/json would: an object keyed
// by the names the schema would have used, which is the tag's name where there
// is one and the Go name otherwise.
//
// It walks the type rather than the plan because a plan holds no field index —
// deliberately, since one would ride in planField through every decode to serve
// this path alone. parseCbTag is the same rule that named the field for the
// schema, so the two cannot drift.
func appendStructAsMap(writer *wire.Writer8, value reflect.Value, buf *scratch) {
	if !buf.enter() {
		writer.ElementNull()
		return
	}
	defer buf.leave()
	structType := value.Type()
	count := 0
	for index := range structType.NumField() {
		if _, ok := dynamicFieldName(structType.Field(index)); ok {
			count++
		}
	}
	mark := writer.OpenElementMap(count)
	for index := range structType.NumField() {
		name, ok := dynamicFieldName(structType.Field(index))
		if !ok {
			continue
		}
		writer.ElementString(name)
		appendAnyReflect(writer, value.Field(index), buf)
	}
	writer.Close(mark)
}

// dynamicFieldName reports what a field is called on the wire, and whether it
// goes there at all.
func dynamicFieldName(field reflect.StructField) (string, bool) {
	if !field.IsExported() {
		return "", false
	}
	name, _, skip := parseCbTag(field)
	if skip {
		return "", false
	}
	return name, true
}

// addressableCopy gives a value an address, which the plan-driven writers need:
// they read fields by offset through an unsafe pointer, and a value that came
// out of an interface has none.
func addressableCopy(value reflect.Value) unsafe.Pointer {
	if value.CanAddr() {
		return unsafe.Pointer(value.UnsafeAddr())
	}
	copied := reflect.New(value.Type()).Elem()
	copied.Set(value)
	return unsafe.Pointer(copied.UnsafeAddr())
}

// appendAnyList writes a []any as a LIST of dynamic values, or as one typed run
// when every element turns out to be the same record.
func appendAnyList(writer *wire.Writer8, values []any, buf *scratch) {
	if appendBoxedSequence(writer, len(values), func(index int) reflect.Value {
		return reflect.ValueOf(values[index])
	}, buf) {
		return
	}
	if !buf.enter() {
		writer.ElementNull()
		return
	}
	defer buf.leave()
	mark := writer.OpenElementList(len(values))
	for _, value := range values {
		appendAnyValue(writer, value, buf)
	}
	writer.Close(mark)
}

// appendStructElement and appendStructsElement are appendStructField and
// appendStructsField for a key-less position.
//
// They are written out rather than folded into the keyed pair, for the reason
// wire keeps two key widths: the keyed openers put the key, the descriptor and
// the length placeholder down in one append, and splitting that to share this
// path would cost every list of structs a bounds check to serve a path that runs
// once per dynamic value.

func appendStructElement(writer *wire.Writer8, plan *typePlan, at unsafe.Pointer, buf *scratch) {
	if plan.isWide {
		mark := writer.OpenElementStructWide()
		appendWide(writer, plan, at, buf)
		writer.Close(mark)
		return
	}
	mark := writer.OpenElementStruct()
	appendNarrowInto(writer, plan, at, buf)
	writer.Close(mark)
}

func appendStructsElement(
	writer *wire.Writer8, plan *typePlan, slice *sliceHeader, stride uintptr, buf *scratch,
) {
	if plan.canTable && slice.len >= tableThreshold {
		appendTableElement(writer, plan, slice, stride, buf)
		return
	}
	mark := writer.OpenElementList(slice.len)
	for index := range slice.len {
		appendStructElement(writer, plan, unsafe.Add(slice.data, uintptr(index)*stride), buf)
	}
	writer.Close(mark)
}

func appendTableElement(
	writer *wire.Writer8, plan *typePlan, slice *sliceHeader, stride uintptr, buf *scratch,
) {
	buf.reserve(slice.len)
	mark := writer.OpenElementTable(slice.len)
	for index := range plan.fields {
		column := &plan.fields[index]
		if column.op == opString {
			buf.strings = gatherStrings(buf.strings[:0], slice, stride, column.offset)
			writer.StringColumn(column.key, buf.strings)
			continue
		}
		buf.ints = gatherInts(buf.ints[:0], slice, stride, column)
		writer.Column(column.key, buf.ints)
	}
	writer.Close(mark)
}

// --- decoding into Go --------------------------------------------------------

// readAnyValue decodes one dynamic value into the Go shapes DecodeAny produces.
//
// It runs the *walk* with an `any` sink rather than reading the wire a second
// time. That is not a shortcut: the two would have to agree about every kind,
// every integer width and every framing, and the way two such switches end is
// one of them growing a case the other does not have.
func readAnyValue(reader *wire.Reader8, buf *scratch) any {
	plans, err := buf.structPlans()
	if err != nil {
		reader.Fail(err)
		return nil
	}
	out := anySink{}
	walk := walker{to: &out, plans: plans}
	walk.dynamicValue(reader)
	if walk.err != nil {
		reader.Fail(walk.err)
		return nil
	}
	return out.root
}

// readAnyList decodes a `[]any` field, whose payload is a LIST of dynamic
// values.
func readAnyList(reader *wire.Reader8, buf *scratch) []any {
	value := readAnyValue(reader, buf)
	if value == nil {
		return nil
	}
	list, ok := value.([]any)
	if !ok {
		reader.Fail(fmt.Errorf(
			"colbin: a []any field holds a %T rather than a list", value))
		return nil
	}
	return list
}

// --- walking -----------------------------------------------------------------

// dynamicValue writes one self-describing value into the sink.
func (w *walker) dynamicValue(reader *wire.Reader8) {
	switch kind := reader.ElementKind(); kind {
	case wire.KindNull:
		reader.ElementNull()
		w.to.null()
	case wire.KindBool:
		w.to.boolean(reader.ElementBool())
	case wire.KindInt:
		w.dynamicInt(reader)
	case wire.KindFloat64:
		w.floatValue(reader.ElementFloat64(), 64)
	case wire.KindFloat32:
		w.floatValue(float64(reader.ElementFloat32()), 32)
	case wire.KindString:
		w.to.textBytes(reader.ElementBlob())
	case wire.KindBytes:
		w.to.blob(reader.ElementBytes())
	case wire.KindList:
		w.dynamicList(reader)
	case wire.KindMap:
		w.dynamicMap(reader)
	case wire.KindTyped:
		w.dynamicTyped(reader)
	case wire.KindStruct, wire.KindTable:
		// Both are reachable only behind a TYPED tag, which is what says which
		// struct they are. Bare, there is nothing to name their fields with.
		w.fail(fmt.Errorf(
			"colbin: a dynamic value is a %s with no type named in front of it",
			dynamicKindName(kind)))
	default:
		// The reader answers KindInvalid for a truncated message as well as for a
		// descriptor it does not know, so its own error comes first: "ran out" is
		// the more useful of the two and the one that is actually true.
		w.fail(reader.Err())
		w.fail(fmt.Errorf(
			"colbin: a dynamic value carries a descriptor this version does not assign"))
	}
	w.fail(reader.Err())
}

// dynamicInt picks the Go shape an integer comes back as.
//
// Signed where it fits, which is what a caller who put an `int` in the map wants
// back, and unsigned only past 2^63, which is a value the format carries and
// int64 is not where it fits. JSON spells both the same way, so this only shows
// in DecodeAny.
func (w *walker) dynamicInt(reader *wire.Reader8) {
	if reader.ElementNegative() {
		w.to.signed(reader.ElementInt())
		return
	}
	if value := reader.ElementUint(); value <= math.MaxInt64 {
		w.to.signed(int64(value))
	} else {
		w.to.unsigned(value)
	}
}

func (w *walker) dynamicList(reader *wire.Reader8) {
	count, elements, ok := reader.ElementList()
	if !ok {
		w.fail(reader.Err())
		return
	}
	if !w.enter() {
		return
	}
	defer w.leave()
	w.to.beginArray()
	for range count {
		w.dynamicValue(&elements)
		if w.err != nil {
			return
		}
	}
	w.to.endArray()
	w.fail(elements.Err())
}

func (w *walker) dynamicMap(reader *wire.Reader8) {
	count, entries, ok := reader.ElementMap()
	if !ok {
		w.fail(reader.Err())
		return
	}
	if !w.enter() {
		return
	}
	defer w.leave()
	w.to.beginObject()
	for range count {
		if !w.dynamicKey(&entries) {
			return
		}
		w.dynamicValue(&entries)
		if w.err != nil {
			return
		}
	}
	w.to.endObject()
	w.fail(entries.Err())
}

// dynamicKey writes an entry's key, which has to be a name.
func (w *walker) dynamicKey(entries *wire.Reader8) bool {
	switch entries.ElementKind() {
	case wire.KindString:
		w.to.keyBytes(entries.ElementBlob())
	case wire.KindInt:
		if entries.ElementNegative() {
			w.to.key(strconv.FormatInt(entries.ElementInt(), 10))
			break
		}
		w.to.key(strconv.FormatUint(entries.ElementUint(), 10))
	default:
		w.fail(fmt.Errorf("colbin: a dynamic map holds a key that is not a name"))
		return false
	}
	if err := entries.Err(); err != nil {
		w.fail(err)
		return false
	}
	return true
}

// dynamicTyped walks a value whose type the section names: the rows of an array
// of records, which is the shape this whole file exists for.
func (w *walker) dynamicTyped(reader *wire.Reader8) {
	index, ok := reader.ElementTypedIndex()
	if !ok {
		w.fail(reader.Err())
		return
	}
	if index >= len(w.plans) {
		w.fail(fmt.Errorf(
			"colbin: a dynamic value names struct %d of %d", index, len(w.plans)))
		return
	}
	plan := w.plans[index]
	switch reader.ElementKind() {
	case wire.KindStruct:
		body, wideKeys, ok := reader.ElementStructBody()
		if !ok {
			w.fail(reader.Err())
			return
		}
		w.body(plan, body, wideKeys)
	case wire.KindList:
		count, elements, ok := reader.ElementList()
		if !ok {
			w.fail(reader.Err())
			return
		}
		w.structList(count, &elements, plan)
	case wire.KindTable:
		rows, columns, ok := reader.ElementTable()
		if !ok {
			w.fail(reader.Err())
			return
		}
		w.structTable(rows, &columns, plan)
	default:
		w.fail(fmt.Errorf(
			"colbin: a type tag names struct %d and what follows is not a record", index))
	}
}

func dynamicKindName(kind wire.Kind) string {
	if kind == wire.KindTable {
		return "table"
	}
	return "struct"
}
