package codec

// Reading a value as the section says it is and storing it as the Go type holds
// it. This is the half of a foreign decode that runs per value; foreign.go decides
// *which* fields need it, once, when the translation is built.
//
// A field the section and the Go type agree on is read by the ordinary arm of
// the ordinary reader and never comes here. What comes here is the rest: an
// integer that the section calls int64 and the Go type narrowed or made unsigned
// or made a float, a float64 the Go type holds as a float32, an array of one
// integer width read into another, and a value the section makes a pointer that
// the Go type does not (or the other way round).
//
// The wire is not what differs. An integer is a magnitude whatever width the type
// declared, so int64 5 and int32 5 are the same bytes; what a narrower reader
// adds is the range check, and what none of them can do is read a signed
// integer as an unsigned one, because the two parts of the inline table at
// nibble 3 (wire/narrow.go Int). So the conversion reads by the *section's* class
// — signed, unsigned, float — which is what the bytes were written as, and then
// converts the value it got.

import (
	"fmt"
	"math"
	"reflect"
	"unsafe"

	"github.com/ivanjoz/colbin/wire"
)

// convClass is how a value is read off the wire, which is the section's side of a
// conversion.
type convClass uint8

const (
	classSigned convClass = iota
	classUnsigned
	classFloat32
	classFloat64
	classBool
	classString
	classSignedArray
	classUnsignedArray
)

// classOfOp is the class a section op is read as, and false for an op that no
// conversion reads.
func classOfOp(op fieldOp) (convClass, bool) {
	switch op {
	case opInt8, opInt16, opInt32, opInt64:
		return classSigned, true
	case opUint8, opUint16, opUint32, opUint64:
		return classUnsigned, true
	case opFloat32:
		return classFloat32, true
	case opFloat64:
		return classFloat64, true
	case opBool:
		return classBool, true
	case opString:
		return classString, true
	case opInt8s, opInt16s, opInt32s, opInt64s:
		return classSignedArray, true
	case opUint16s, opUint32s, opUint64s:
		return classUnsignedArray, true
	}
	return 0, false
}

func isIntegerClass(class convClass) bool {
	return class == classSigned || class == classUnsigned
}

func isFloatClass(class convClass) bool {
	return class == classFloat32 || class == classFloat64
}

func isArrayClass(class convClass) bool {
	return class == classSignedArray || class == classUnsignedArray
}

// convertible is the conversion table of the plan, one row per pair of ops: whether
// a value the section calls src can be stored in a Go field that holds dst. It is
// asked once per field when a translation is built, never per value.
//
// A float is never stored in an integer, whatever its value: a column that
// happens to hold whole numbers today is not one that does tomorrow, and the
// decoder refusing it at plan time is the same on every message.
func convertible(src, dst fieldOp) bool {
	from, ok := classOfOp(src)
	if !ok {
		return false
	}
	to, ok := classOfOp(dst)
	if !ok {
		return false
	}
	switch {
	case isIntegerClass(from):
		return isIntegerClass(to) || isFloatClass(to)
	case isFloatClass(from):
		return isFloatClass(to)
	case isArrayClass(from):
		return isArrayClass(to)
	}
	return from == to
}

// fieldConv is one field's conversion: what the section says is on the wire and
// what the Go field holds.
type fieldConv struct {
	// path names the Go field for an error: Product.Seller.Rating.
	path string
	// src and dst are ops with any pointer taken off, and class is src's.
	src, dst fieldOp
	class    convClass
	// dstPointer says the Go field is a pointer to dst's type, which is allocated
	// when a value is present. dstElem is what it points at, and dstElems a slice
	// of it, for a column that allocates its pointees together.
	dstPointer bool
	dstElem    reflect.Type
	dstElems   reflect.Type
}

// foreignPlan is what a decode through a foreign section carries: the root plan,
// which the section's keys and ops shape, and the conversions its fields index.
type foreignPlan struct {
	root  *typePlan
	convs []fieldConv
}

// convOf is the conversion a field of this decode names.
func (buf *scratch) convOf(field *planField) *fieldConv {
	if field.op != opConvert || buf.foreign == nil || int(field.conv-1) >= len(buf.foreign.convs) {
		return nil
	}
	return &buf.foreign.convs[field.conv-1]
}

// integer is every type a stored integer can be.
type integer interface {
	~int8 | ~int16 | ~int32 | ~int64 | ~uint8 | ~uint16 | ~uint32 | ~uint64
}

// putSigned stores a signed value into an integer field, and reports false when it
// does not fit. Converting and converting back is the range check for every width
// at once, and the sign test catches the one case it cannot: a negative value that
// wraps into an unsigned type of the same width reads back the same.
func putSigned[T integer](at unsafe.Pointer, value int64) bool {
	converted := T(value)
	if int64(converted) != value || (converted < 0) != (value < 0) {
		return false
	}
	*(*T)(at) = converted
	return true
}

// putUnsigned is putSigned for a value read as unsigned. A signed target that
// comes out negative took a value past its half of the range.
func putUnsigned[T integer](at unsafe.Pointer, value uint64) bool {
	converted := T(value)
	if uint64(converted) != value || converted < 0 {
		return false
	}
	*(*T)(at) = converted
	return true
}

func (conv *fieldConv) overflow(value any) error {
	return fmt.Errorf("%s: section %s value %v does not fit Go %s",
		conv.path, opLabel(conv.src), value, opLabel(conv.dst))
}

func (conv *fieldConv) overflowAt(value any, element int) error {
	return fmt.Errorf("%s: section %s element %d (%v) does not fit Go %s",
		conv.path, opLabel(conv.src), element, value, opLabel(conv.dst))
}

// storeSigned stores a value the section wrote as a signed integer.
func (conv *fieldConv) storeSigned(at unsafe.Pointer, value int64) error {
	var ok bool
	switch conv.dst {
	case opInt8:
		ok = putSigned[int8](at, value)
	case opInt16:
		ok = putSigned[int16](at, value)
	case opInt32:
		ok = putSigned[int32](at, value)
	case opInt64:
		*(*int64)(at), ok = value, true
	case opUint8:
		ok = putSigned[uint8](at, value)
	case opUint16:
		ok = putSigned[uint16](at, value)
	case opUint32:
		ok = putSigned[uint32](at, value)
	case opUint64:
		ok = putSigned[uint64](at, value)
	case opFloat32:
		*(*float32)(at), ok = float32(value), true
	case opFloat64:
		*(*float64)(at), ok = float64(value), true
	}
	if !ok {
		return conv.overflow(value)
	}
	return nil
}

// storeUnsigned stores a value the section wrote as an unsigned integer.
func (conv *fieldConv) storeUnsigned(at unsafe.Pointer, value uint64) error {
	var ok bool
	switch conv.dst {
	case opInt8:
		ok = putUnsigned[int8](at, value)
	case opInt16:
		ok = putUnsigned[int16](at, value)
	case opInt32:
		ok = putUnsigned[int32](at, value)
	case opInt64:
		ok = putUnsigned[int64](at, value)
	case opUint8:
		ok = putUnsigned[uint8](at, value)
	case opUint16:
		ok = putUnsigned[uint16](at, value)
	case opUint32:
		ok = putUnsigned[uint32](at, value)
	case opUint64:
		*(*uint64)(at), ok = value, true
	case opFloat32:
		*(*float32)(at), ok = float32(value), true
	case opFloat64:
		*(*float64)(at), ok = float64(value), true
	}
	if !ok {
		return conv.overflow(value)
	}
	return nil
}

// storeFloat stores a value the section wrote as a float. Going down to a float32
// rounds, as encoding/json does, but a finite value too large for one is refused
// rather than turned into an infinity.
func (conv *fieldConv) storeFloat(at unsafe.Pointer, value float64) error {
	switch conv.dst {
	case opFloat32:
		if math.Abs(value) > math.MaxFloat32 && !math.IsInf(value, 0) {
			return conv.overflow(value)
		}
		*(*float32)(at) = float32(value)
	case opFloat64:
		*(*float64)(at) = value
	}
	return nil
}

// storeWord stores one value of a table column, which arrives as the int64 the
// column codec decodes to: an integer as itself, a float as its bit pattern.
func (conv *fieldConv) storeWord(at unsafe.Pointer, word int64) error {
	switch conv.class {
	case classSigned:
		return conv.storeSigned(at, word)
	case classUnsigned:
		return conv.storeUnsigned(at, uint64(word))
	case classFloat32:
		return conv.storeFloat(at, float64(math.Float32frombits(uint32(word))))
	case classFloat64:
		return conv.storeFloat(at, math.Float64frombits(uint64(word)))
	case classBool:
		*(*bool)(at) = word == 1
	}
	return nil
}

// convertSigned and convertUnsigned are the array forms of putSigned and
// putUnsigned. They return the index of the first element that does not fit, or
// -1.
//
// They write into dst's array when it has room, as the readers of the same-typed
// arrays do: a decode into a value that already holds a slice lends it. See
// reset.go.
func convertSigned[T integer](dst []T, values []int64) ([]T, int) {
	out := dst[:0]
	if cap(out) < len(values) {
		out = make([]T, 0, len(values))
	}
	for index, value := range values {
		converted := T(value)
		if int64(converted) != value || (converted < 0) != (value < 0) {
			return nil, index
		}
		out = append(out, converted)
	}
	return out, -1
}

func convertUnsigned[T integer](dst []T, values []uint64) ([]T, int) {
	out := dst[:0]
	if cap(out) < len(values) {
		out = make([]T, 0, len(values))
	}
	for index, value := range values {
		converted := T(value)
		if uint64(converted) != value || converted < 0 {
			return nil, index
		}
		out = append(out, converted)
	}
	return out, -1
}

// storeSignedArray stores an array the section wrote as signed integers into a
// slice of another integer type, every element range-checked.
func (conv *fieldConv) storeSignedArray(at unsafe.Pointer, values []int64) error {
	var failed int
	switch conv.dst {
	case opInt8s:
		*(*[]int8)(at), failed = convertSigned(*(*[]int8)(at), values)
	case opInt16s:
		*(*[]int16)(at), failed = convertSigned(*(*[]int16)(at), values)
	case opInt32s:
		*(*[]int32)(at), failed = convertSigned(*(*[]int32)(at), values)
	case opInt64s:
		*(*[]int64)(at), failed = convertSigned(*(*[]int64)(at), values)
	case opUint16s:
		*(*[]uint16)(at), failed = convertSigned(*(*[]uint16)(at), values)
	case opUint32s:
		*(*[]uint32)(at), failed = convertSigned(*(*[]uint32)(at), values)
	case opUint64s:
		*(*[]uint64)(at), failed = convertSigned(*(*[]uint64)(at), values)
	default:
		return nil
	}
	if failed >= 0 {
		return conv.overflowAt(values[failed], failed)
	}
	return nil
}

func (conv *fieldConv) storeUnsignedArray(at unsafe.Pointer, values []uint64) error {
	var failed int
	switch conv.dst {
	case opInt8s:
		*(*[]int8)(at), failed = convertUnsigned(*(*[]int8)(at), values)
	case opInt16s:
		*(*[]int16)(at), failed = convertUnsigned(*(*[]int16)(at), values)
	case opInt32s:
		*(*[]int32)(at), failed = convertUnsigned(*(*[]int32)(at), values)
	case opInt64s:
		*(*[]int64)(at), failed = convertUnsigned(*(*[]int64)(at), values)
	case opUint16s:
		*(*[]uint16)(at), failed = convertUnsigned(*(*[]uint16)(at), values)
	case opUint32s:
		*(*[]uint32)(at), failed = convertUnsigned(*(*[]uint32)(at), values)
	case opUint64s:
		*(*[]uint64)(at), failed = convertUnsigned(*(*[]uint64)(at), values)
	default:
		return nil
	}
	if failed >= 0 {
		return conv.overflowAt(values[failed], failed)
	}
	return nil
}

// target is where a value is stored: the field, or a new pointee when the Go field
// is a pointer, which is published into the field only once the value is in it.
func (conv *fieldConv) target(at unsafe.Pointer) unsafe.Pointer {
	if conv.dstPointer {
		return reflect.New(conv.dstElem).UnsafePointer()
	}
	return at
}

func (conv *fieldConv) publish(at, target unsafe.Pointer) {
	if conv.dstPointer {
		*(*unsafe.Pointer)(at) = target
	}
}

// readConverted and readConvertedWide read one field of a foreign plan, as the
// default arm of the nested walk at each key width. A field that is not a
// conversion at all is the failure that arm always was.
//
// The two are the same body over two reader types, kept as two because a reader
// behind an interface or a type parameter is a reader on the heap, which the
// ordinary walk takes care not to have.
func readConverted(reader *wire.Reader, field *planField, at unsafe.Pointer, buf *scratch) {
	conv := buf.convOf(field)
	if conv == nil {
		reader.Fail(errNotValueOp(field.op))
		return
	}
	target := conv.target(at)
	var err error
	switch conv.class {
	case classSigned:
		err = conv.storeSigned(target, reader.Int())
	case classUnsigned:
		err = conv.storeUnsigned(target, reader.Uint())
	case classFloat32:
		err = conv.storeFloat(target, float64(reader.F32()))
	case classFloat64:
		err = conv.storeFloat(target, reader.F64())
	case classBool:
		*(*bool)(target) = reader.Bool()
	case classString:
		*(*string)(target) = reader.StringIn(&buf.strings)
	case classSignedArray:
		// Short arrays are read into the stack and converted from there: only the
		// result, which the Go field keeps, is allocated.
		var small [32]int64
		err = conv.storeSignedArray(target, reader.Ints(small[:0]))
	case classUnsignedArray:
		var small [32]uint64
		err = conv.storeUnsignedArray(target, reader.Uint64s(small[:0]))
	}
	if err != nil {
		reader.Fail(err)
		return
	}
	if reader.Err() == nil {
		conv.publish(at, target)
	}
}

func readConvertedWide(reader *wire.Reader8, field *planField, at unsafe.Pointer, buf *scratch) {
	conv := buf.convOf(field)
	if conv == nil {
		reader.Fail(errNotValueOp(field.op))
		return
	}
	target := conv.target(at)
	var err error
	switch conv.class {
	case classSigned:
		err = conv.storeSigned(target, reader.Int())
	case classUnsigned:
		err = conv.storeUnsigned(target, reader.Uint())
	case classFloat32:
		err = conv.storeFloat(target, float64(reader.F32()))
	case classFloat64:
		err = conv.storeFloat(target, reader.F64())
	case classBool:
		*(*bool)(target) = reader.Bool()
	case classString:
		*(*string)(target) = reader.StringIn(&buf.strings)
	case classSignedArray:
		// Short arrays are read into the stack and converted from there: only the
		// result, which the Go field keeps, is allocated.
		var small [32]int64
		err = conv.storeSignedArray(target, reader.Ints(small[:0]))
	case classUnsignedArray:
		var small [32]uint64
		err = conv.storeUnsignedArray(target, reader.Uint64s(small[:0]))
	}
	if err != nil {
		reader.Fail(err)
		return
	}
	if reader.Err() == nil {
		conv.publish(at, target)
	}
}

// readConvertedColumn reads one column of a table into the rows, converting each
// value, as readPointerColumn does for a pointer column: the column is decoded
// whole into a buffer of its own, which keeps it clear of the columns the table
// has decoded and not yet scattered.
//
// The section's column may be a plain one or a nullable one whichever the Go
// field is — a column that was null in some rows is nullable, and the Go field may
// be a plain int32 that those rows leave at zero. The wire says which it is, so
// that is what is read, not what the section's op would suggest.
func readConvertedColumn(
	reader pointerColumnReader, field *planField, data unsafe.Pointer, stride uintptr, rows int, buf *scratch,
) {
	conv := buf.convOf(field)
	if conv == nil {
		reader.Fail(errNotValueOp(field.op))
		return
	}
	if conv.class == classString {
		readConvertedStrings(reader, conv, field, data, stride, rows)
		return
	}
	columns := buf.reserve(0, false)
	columns.reservePointers(rows, 0, 0)
	scratch := columns.nullInts[:0:rows]
	var presence []byte
	var values []int64
	if reader.IsNullable() {
		presence, values = reader.NullableColumn(rows, scratch)
	} else {
		values = reader.Column(rows, scratch)
	}
	if reader.Err() != nil {
		return
	}
	var pointees unsafe.Pointer
	if conv.dstPointer {
		pointees = reflect.MakeSlice(conv.dstElems, len(values), len(values)).UnsafePointer()
	}
	next := 0
	for row := range rows {
		if presence != nil && !wire.Present(presence, row) {
			continue
		}
		if next >= len(values) {
			reader.Fail(errColumnLength)
			return
		}
		at := unsafe.Add(data, uintptr(row)*stride+field.offset)
		target := at
		if conv.dstPointer {
			target = unsafe.Add(pointees, uintptr(next)*conv.dstElem.Size())
		}
		if err := conv.storeWord(target, values[next]); err != nil {
			reader.Fail(fmt.Errorf("row %d: %w", row, err))
			return
		}
		conv.publish(at, target)
		next++
	}
}

// readConvertedStrings is readConvertedColumn for a column of strings, whose only
// conversion is in where the pointer is.
func readConvertedStrings(
	reader pointerColumnReader, conv *fieldConv, field *planField, data unsafe.Pointer, stride uintptr, rows int,
) {
	var presence []byte
	var values []string
	if reader.IsNullable() {
		presence, values = reader.NullableStrings(rows, nil)
	} else if values = reader.Strings(make([]string, 0, rows)); len(values) != rows && reader.Err() == nil {
		reader.Fail(errColumnLength)
	}
	if reader.Err() != nil {
		return
	}
	if conv.dstPointer {
		pointRows(values, presence, data, stride, field.offset, rows)
		return
	}
	next := 0
	for row := range rows {
		if presence != nil && !wire.Present(presence, row) {
			continue
		}
		if next >= len(values) {
			reader.Fail(errColumnLength)
			return
		}
		*(*string)(unsafe.Add(data, uintptr(row)*stride+field.offset)) = values[next]
		next++
	}
}

// opLabel is how an error says an op: the Go type it reads and writes.
func opLabel(op fieldOp) string {
	switch op {
	case opBool:
		return "bool"
	case opInt8:
		return "int8"
	case opInt16:
		return "int16"
	case opInt32:
		return "int32"
	case opInt64:
		return "int64"
	case opUint8:
		return "uint8"
	case opUint16:
		return "uint16"
	case opUint32:
		return "uint32"
	case opUint64:
		return "uint64"
	case opFloat32:
		return "float32"
	case opFloat64:
		return "float64"
	case opString:
		return "string"
	case opBytes:
		return "[]byte"
	case opInt8s:
		return "[]int8"
	case opInt16s:
		return "[]int16"
	case opInt32s:
		return "[]int32"
	case opInt64s:
		return "[]int64"
	case opUint16s:
		return "[]uint16"
	case opUint32s:
		return "[]uint32"
	case opUint64s:
		return "[]uint64"
	case opStrings:
		return "[]string"
	case opStruct:
		return "struct"
	case opStructs:
		return "[]struct"
	case opMap:
		return "map"
	case opPointer:
		return "pointer"
	case opAny:
		return "any"
	case opAnys:
		return "[]any"
	case opPointerStruct:
		return "*struct"
	}
	return fmt.Sprintf("op %d", uint8(op))
}
