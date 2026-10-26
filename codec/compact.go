package codec

// Dynamic values inside a dynamic container, in wire's compact form.
//
// A map or a list in a dynamic position is an ordinary MAP or LIST — a
// descriptor and a length, which is what lets a reader that does not want it
// step over it — and everything inside it is compact (wire/compact.go): a code
// that holds a short string's length, a small container's count or a small
// number, and a container that carries its count and no length.
//
// A map goes out in the order Go hands its entries over, which is not the same
// twice. Ordering them was most of a document's encode, and the format's answer
// to "is this the same document" is to decode both, not to compare the bytes.
//
// The root of a message can be a dynamic document itself (root.go): a
// map[string]any as its entries to the end of the message, and a []any as one
// compact value. That is a msgpack document's framing plus the root byte.

import (
	"fmt"
	"math"
	"reflect"
	"strconv"
	"unsafe"

	"github.com/ivanjoz/colbin/wire"
)

// --- encoding ----------------------------------------------------------------

// appendCompactValue is appendAnyValue inside a dynamic container.
func appendCompactValue(writer *wire.Writer8, value any, buf *scratch) {
	switch typed := value.(type) {
	case nil:
		writer.CompactNull()
	case bool:
		writer.CompactBool(typed)
	case string:
		writer.CompactString(typed)
	case float64:
		writer.CompactFloat64(typed)
	case map[string]any:
		appendCompactStringMap(writer, typed, buf)
	case []any:
		appendCompactList(writer, typed, buf)
	case int:
		writer.CompactInt(int64(typed))
	case int64:
		writer.CompactInt(typed)
	case []byte:
		writer.CompactBytes(typed)
	case int8:
		writer.CompactInt(int64(typed))
	case int16:
		writer.CompactInt(int64(typed))
	case int32:
		writer.CompactInt(int64(typed))
	case uint:
		writer.CompactUint(uint64(typed))
	case uint16:
		writer.CompactUint(uint64(typed))
	case uint32:
		writer.CompactUint(uint64(typed))
	case uint64:
		writer.CompactUint(typed)
	case float32:
		writer.CompactFloat32(typed)
	default:
		appendCompactReflect(writer, reflect.ValueOf(value), buf)
	}
}

// appendCompactStringMap writes a map[string]any as a compact map.
func appendCompactStringMap(writer *wire.Writer8, value map[string]any, buf *scratch) {
	if value == nil || !buf.enter() {
		writer.CompactNull()
		return
	}
	writer.CompactMap(len(value))
	appendCompactEntries(writer, value, buf)
	buf.leave()
}

// appendCompactEntries writes a map[string]any's entries, keys and values
// compact, in the order the map hands them over.
func appendCompactEntries(writer *wire.Writer8, value map[string]any, buf *scratch) {
	written := 0
	for key, element := range value {
		if buf.err != nil {
			return
		}
		writer.CompactString(key)
		appendCompactValue(writer, element, buf)
		written++
	}
	if written != len(value) && buf.err == nil {
		// Only a map written to while it is encoded gets here, and the count
		// already out says otherwise.
		buf.fail(errMapChanged)
	}
}

var errMapChanged = fmt.Errorf("colbin: a map changed while it was encoded")

// appendCompactList writes a []any as a compact list, or as one typed run when
// every element turns out to be the same record.
func appendCompactList(writer *wire.Writer8, values []any, buf *scratch) {
	if appendBoxedSequence(writer, len(values), func(index int) reflect.Value {
		return reflect.ValueOf(values[index])
	}, buf, true) {
		return
	}
	if !buf.enter() {
		writer.CompactNull()
		return
	}
	writer.CompactList(len(values))
	for _, value := range values {
		if buf.err != nil {
			break
		}
		appendCompactValue(writer, value, buf)
	}
	buf.leave()
}

// appendCompactReflect is appendAnyReflect inside a dynamic container.
func appendCompactReflect(writer *wire.Writer8, value reflect.Value, buf *scratch) {
	if !value.IsValid() {
		writer.CompactNull()
		return
	}
	switch value.Kind() {
	case reflect.Bool:
		writer.CompactBool(value.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		writer.CompactInt(value.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Uintptr:
		writer.CompactUint(value.Uint())
	case reflect.Float32:
		writer.CompactFloat32(float32(value.Float()))
	case reflect.Float64:
		writer.CompactFloat64(value.Float())
	case reflect.String:
		writer.CompactString(value.String())
	case reflect.Interface:
		// Back to the type switch, as appendAnyReflect goes back to its own.
		if value.CanInterface() {
			appendCompactValue(writer, value.Interface(), buf)
			return
		}
		fallthrough
	case reflect.Pointer:
		if value.IsNil() {
			writer.CompactNull()
			return
		}
		appendCompactReflect(writer, value.Elem(), buf)
	case reflect.Slice, reflect.Array:
		appendCompactSequence(writer, value, buf)
	case reflect.Map:
		if value.IsNil() {
			writer.CompactNull()
			return
		}
		if !dynamicMapKey(value, buf) || !buf.enter() {
			writer.CompactNull()
			return
		}
		writer.CompactMap(value.Len())
		appendCompactMapEntries(writer, value, buf)
		buf.leave()
	case reflect.Struct:
		if opaqueStruct(value.Type()) {
			if text, ok := opaqueText(value, buf); ok {
				writer.CompactString(text)
			} else {
				writer.CompactNull()
			}
			return
		}
		appendAnyStruct(writer, value, buf, true)
	default:
		buf.fail(fmt.Errorf("colbin: a %s has no form in a dynamic value", value.Type()))
		writer.CompactNull()
	}
}

// appendCompactSequence is appendAnySequence inside a dynamic container.
func appendCompactSequence(writer *wire.Writer8, value reflect.Value, buf *scratch) {
	if value.Kind() == reflect.Slice && value.IsNil() {
		writer.CompactNull()
		return
	}
	if value.Type().Elem().Kind() == reflect.Uint8 {
		writer.CompactBytes(bytesOf(value))
		return
	}
	if appendTypedSequence(writer, value, buf, true) ||
		appendBoxedSequence(writer, value.Len(), value.Index, buf, true) {
		return
	}
	if !buf.enter() {
		writer.CompactNull()
		return
	}
	writer.CompactList(value.Len())
	appendCompactElements(writer, value, buf)
	buf.leave()
}

// appendCompactElements writes a sequence's elements compact.
func appendCompactElements(writer *wire.Writer8, value reflect.Value, buf *scratch) {
	for index := range value.Len() {
		if buf.err != nil {
			return
		}
		appendCompactReflect(writer, value.Index(index), buf)
	}
}

// appendCompactMapEntries writes any map's entries compact: a key a string or
// an integer, which dynamicMapKey has checked.
func appendCompactMapEntries(writer *wire.Writer8, value reflect.Value, buf *scratch) {
	written := 0
	for entries := value.MapRange(); entries.Next(); written++ {
		if buf.err != nil {
			return
		}
		appendCompactReflect(writer, entries.Key(), buf)
		appendCompactReflect(writer, entries.Value(), buf)
	}
	if written != value.Len() && buf.err == nil {
		buf.fail(errMapChanged)
	}
}

// dynamicMapKey says a map's keys can be names, which is what a JSON object
// needs and what a list or a struct is not, and fails the encode when not.
func dynamicMapKey(value reflect.Value, buf *scratch) bool {
	if _, err := mapKindOf(value.Type().Key(), "key"); err != nil {
		buf.fail(fmt.Errorf("colbin: %w", err))
		return false
	}
	return true
}

// appendCompactStructEntries writes a struct's fields as a map's entries, by
// the names the schema would have used.
func appendCompactStructEntries(writer *wire.Writer8, value reflect.Value, buf *scratch) {
	structType := value.Type()
	for index := range structType.NumField() {
		name, ok := dynamicFieldName(structType.Field(index))
		if !ok {
			continue
		}
		if buf.err != nil {
			return
		}
		writer.CompactString(name)
		appendCompactReflect(writer, value.Field(index), buf)
	}
}

// appendDynamicRoot writes a message whose root is a dynamic document: the
// root byte, and a map's entries to the end of the message or a list as one
// compact value. section is the struct table a TYPED value names into, or nil.
func appendDynamicRoot(dst []byte, plan *typePlan, record unsafe.Pointer, section *sectionBuilder) ([]byte, error) {
	root := rootDynamicMap
	if plan.dynamicRoot == dynamicRootList {
		root = rootDynamicList
	}
	writer := wire.Writer8{Buffer: append(dst, root)}
	buf := scratch{section: section}
	if buf.enter() {
		if plan.dynamicRoot == dynamicRootList {
			appendCompactList(&writer, *(*[]any)(record), &buf)
		} else {
			appendCompactEntries(&writer, *(*map[string]any)(record), &buf)
		}
		buf.leave()
	}
	buf.release()
	if buf.err != nil {
		return dst, buf.err
	}
	return writer.Buffer, nil
}

// --- decoding into Go --------------------------------------------------------

// compact reads one compact value: anyReader.value, inside a container.
func (r *anyReader) compact(reader *wire.Reader8) any {
	out := &r.out
	var value any
	switch reader.CompactKind() {
	case wire.KindString:
		value = boxIn(&out.boxes.strings, out.copied(reader.CompactString()), stringKind)
	case wire.KindFloat64, wire.KindFloat32:
		float, _ := reader.CompactFloat()
		value = boxIn(&out.boxes.floats, float, floatKind)
	case wire.KindMap:
		value = r.compactObject(reader)
	case wire.KindList:
		value = r.compactList(reader)
	case wire.KindInt:
		value = r.compactInt(reader)
	case wire.KindBool:
		value = reader.CompactBool()
	case wire.KindNull:
		reader.CompactNull()
	case wire.KindBytes:
		value = append([]byte(nil), reader.CompactBytes()...)
	default:
		// TYPED, and every code this version does not assign: the walk's to
		// read or to refuse.
		walk, sink := r.borrow()
		walk.compactValue(reader)
		r.give(walk, sink)
		return sink.root
	}
	r.fail(reader.Err())
	return value
}

// compactInt is an integer in the shape dynamicInt picks for it.
func (r *anyReader) compactInt(reader *wire.Reader8) any {
	magnitude, negative := reader.CompactInteger()
	switch {
	case negative:
		return r.out.signedBox(-int64(magnitude))
	case magnitude <= math.MaxInt64:
		return r.out.signedBox(int64(magnitude))
	}
	return magnitude
}

func (r *anyReader) compactList(reader *wire.Reader8) any {
	count, ok := reader.CompactList()
	if !ok || !r.enter() {
		r.fail(reader.Err())
		return nil
	}
	defer r.leave()
	list := make([]any, count)
	for index := range list {
		list[index] = r.compact(reader)
		if r.err != nil {
			return nil
		}
	}
	return boxIn(&r.out.boxes.arrays, list, arrayKind)
}

func (r *anyReader) compactObject(reader *wire.Reader8) any {
	count, ok := reader.CompactMap()
	if !ok || !r.enter() {
		r.fail(reader.Err())
		return nil
	}
	defer r.leave()
	object := make(map[string]any, count)
	for range count {
		if !r.compactEntry(reader, object) {
			return nil
		}
	}
	return object
}

// compactEntry reads one entry into object.
func (r *anyReader) compactEntry(reader *wire.Reader8, object map[string]any) bool {
	var key string
	if reader.CompactKind() == wire.KindString {
		key = r.out.copied(reader.CompactString())
	} else {
		// A number for a name, or no name at all: compactKey's to read or to
		// refuse, onto a level of the borrowed sink's.
		walk, sink := r.borrow()
		sink.stack = append(sink.stack, anyLevel{})
		ok := walk.compactKey(reader)
		r.give(walk, sink)
		if !ok {
			return false
		}
		key = sink.stack[0].key
	}
	object[key] = r.compact(reader)
	return r.err == nil
}

// entries reads a dynamic container's count entries into document.
func (r *anyReader) entries(reader *wire.Reader8, count int, document map[string]any) error {
	for range count {
		if !r.compactEntry(reader, document) {
			return r.err
		}
	}
	return reader.Err()
}

// readCompactAny decodes one compact value into the Go shapes DecodeAny
// produces: readAnyValue, inside a container.
func readCompactAny(reader *wire.Reader8, buf *scratch) any {
	plans, err := buf.structPlans()
	if err != nil {
		reader.Fail(err)
		return nil
	}
	read := anyReader{plans: plans, depth: buf.depth, rowsLeft: buf.rowsLeft}
	read.out.strings = buf.strings
	value := read.compact(reader)
	buf.rowsLeft, buf.strings = read.rowsLeft, read.out.strings
	if read.err != nil {
		reader.Fail(read.err)
		return nil
	}
	return value
}

// readCompactKey reads a compact map key into a typed map's key.
func readCompactKey(reader *wire.Reader8, kind mapKind, into reflect.Value) {
	switch kind {
	case mapString:
		into.SetString(string(reader.CompactString()))
	case mapInt:
		magnitude, negative := reader.CompactInteger()
		if negative {
			into.SetInt(-int64(magnitude))
		} else {
			into.SetInt(int64(magnitude))
		}
	case mapUint:
		magnitude, negative := reader.CompactInteger()
		if negative {
			reader.Fail(fmt.Errorf("colbin: a negative key for an unsigned one"))
		}
		into.SetUint(magnitude)
	}
}

// unmarshalDynamicRoot decodes a message whose root is a dynamic document into
// the record, which has to be the document's own type: a map[string]any for a
// map root and a []any for a list.
func unmarshalDynamicRoot(data []byte, plan *typePlan, record unsafe.Pointer, what reflect.Type) error {
	section, body, list, ok := dynamicRootParts(data)
	if !ok {
		return errBadRoot(data, what)
	}
	if plan.dynamicRoot == 0 || list != (plan.dynamicRoot == dynamicRootList) {
		if list {
			return fmt.Errorf("colbin: %s: the message is a dynamic list, a []any", what)
		}
		return fmt.Errorf("colbin: %s: the message is a dynamic document, a map[string]any", what)
	}
	buf := scratch{rawSection: section, rowsLeft: rowBudget(len(body))}
	buf.strings.Reset(len(body))
	defer buf.release()
	plans, err := buf.structPlans()
	if err != nil {
		return errDecoding(what, err)
	}
	read := anyReader{plans: plans, rowsLeft: buf.rowsLeft}
	read.out.strings = buf.strings
	reader := wire.NewReader8(body)
	if list {
		value := read.compact(&reader)
		if read.err == nil && reader.More() {
			read.fail(errTrailing)
		}
		if read.err == nil {
			switch typed := value.(type) {
			case []any:
				*(*[]any)(record) = typed
			case nil:
				*(*[]any)(record) = nil
			default:
				read.fail(fmt.Errorf("colbin: the message's root is a %T rather than a list", value))
			}
		}
	} else {
		document := *(*map[string]any)(record)
		if document == nil {
			document = map[string]any{}
		} else {
			clear(document)
		}
		for reader.More() && read.err == nil {
			read.compactEntry(&reader, document)
		}
		read.fail(reader.Err())
		*(*map[string]any)(record) = document
	}
	buf.strings = read.out.strings
	if read.err != nil {
		return errDecoding(what, read.err)
	}
	return nil
}

var errTrailing = fmt.Errorf("colbin: bytes past the end of the document")

// dynamicRootParts reads a dynamic root byte and returns the section, if the
// message carries one, and the body after it.
func dynamicRootParts(data []byte) (section, body []byte, list, ok bool) {
	if len(data) == 0 || data[0]&^(rootSchema|rootDynamicListBit) != rootDynamicMap {
		return nil, nil, false, false
	}
	list = data[0]&rootDynamicListBit != 0
	if data[0]&rootSchema == 0 {
		return nil, data[1:], list, true
	}
	section, body, ok = splitSchemaSection(data[1:])
	return section, body, list, ok
}

// --- walking -----------------------------------------------------------------

// compactValue writes one compact value into the sink: dynamicValue, inside a
// container.
func (w *walker) compactValue(reader *wire.Reader8) {
	switch kind := reader.CompactKind(); kind {
	case wire.KindNull:
		reader.CompactNull()
		w.to.null()
	case wire.KindBool:
		w.to.boolean(reader.CompactBool())
	case wire.KindInt:
		magnitude, negative := reader.CompactInteger()
		switch {
		case negative:
			w.to.signed(-int64(magnitude))
		case magnitude <= math.MaxInt64:
			w.to.signed(int64(magnitude))
		default:
			w.to.unsigned(magnitude)
		}
	case wire.KindFloat64, wire.KindFloat32:
		w.floatValue(reader.CompactFloat())
	case wire.KindString:
		w.to.textBytes(reader.CompactString())
	case wire.KindBytes:
		w.to.blob(reader.CompactBytes())
	case wire.KindList:
		count, ok := reader.CompactList()
		if !ok || !w.enter() {
			w.fail(reader.Err())
			return
		}
		w.to.beginArray(count)
		for range count {
			w.compactValue(reader)
			if w.err != nil {
				w.leave()
				return
			}
		}
		w.to.endArray()
		w.leave()
	case wire.KindMap:
		count, ok := reader.CompactMap()
		if !ok {
			w.fail(reader.Err())
			return
		}
		w.compactEntries(reader, count)
	case wire.KindTyped:
		index, ok := reader.CompactTyped()
		if !ok {
			w.fail(reader.Err())
			return
		}
		w.typedValue(reader, index)
	default:
		w.fail(reader.Err())
		w.fail(fmt.Errorf(
			"colbin: a dynamic value carries a code this version does not assign"))
	}
	w.fail(reader.Err())
}

// compactEntries writes count compact entries as an object.
func (w *walker) compactEntries(reader *wire.Reader8, count int) {
	if !w.enter() {
		return
	}
	defer w.leave()
	w.to.beginObject(count)
	for range count {
		if !w.compactKey(reader) {
			return
		}
		w.compactValue(reader)
		if w.err != nil {
			return
		}
	}
	w.to.endObject()
}

// compactKey writes an entry's key, which has to be a name.
func (w *walker) compactKey(reader *wire.Reader8) bool {
	switch reader.CompactKind() {
	case wire.KindString:
		w.to.keyBytes(reader.CompactString())
	case wire.KindInt:
		magnitude, negative := reader.CompactInteger()
		if negative {
			w.to.key(strconv.FormatInt(-int64(magnitude), 10))
		} else {
			w.to.key(strconv.FormatUint(magnitude, 10))
		}
	default:
		w.fail(reader.Err())
		w.fail(fmt.Errorf("colbin: a dynamic map holds a key that is not a name"))
		return false
	}
	if err := reader.Err(); err != nil {
		w.fail(err)
		return false
	}
	return true
}

// dynamicRoot walks a message whose root is a dynamic document.
func (w *walker) dynamicRoot(body []byte, list bool) {
	reader := wire.NewReader8(body)
	if list {
		w.compactValue(&reader)
		if w.err == nil && reader.More() {
			w.fail(errTrailing)
		}
		return
	}
	if !w.enter() {
		return
	}
	defer w.leave()
	// The entries run to the end of the message, so their count is not known
	// before they are read; a sink takes the size only as a hint.
	w.to.beginObject(0)
	for reader.More() {
		if !w.compactKey(&reader) {
			return
		}
		w.compactValue(&reader)
		if w.err != nil {
			return
		}
	}
	w.to.endObject()
}
