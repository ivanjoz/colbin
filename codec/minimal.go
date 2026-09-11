package codec

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"unsafe"

	"github.com/ivanjoz/colbin/minimal"
)

// The bridge from a Go type to minimal mode, the same shape as the compact-mode
// bridge beside it: a plan resolved once per type and cached, then a flat switch
// over it per message.
//
// What minimal mode is, and why it is a third mode rather than a variant of the
// other two, is in minimal/README.md. What matters here is the one property that
// shapes this file: **a minimal message carries no version byte and no field
// ids beyond four bits**, so the type has to say everything. A field needs an
// explicit `cb:"N"` with N under sixteen, and a type that does not number its
// fields is refused rather than hashed into ids that would not fit.

type minimalOp uint8

const (
	minOpBool minimalOp = iota
	minOpInt8
	minOpInt16
	minOpInt32
	minOpInt64
	minOpUint8
	minOpUint16
	minOpUint32
	minOpUint64
	minOpFloat32
	minOpFloat64
	minOpString
	minOpBytes
	minOpInt8s
	minOpInt16s
	minOpInt32s
	minOpInt64s
	minOpUint16s
	minOpUint32s
	minOpUint64s
	minOpStrings
)

type minimalField struct {
	key    uint8
	offset uintptr
	op     minimalOp
}

type minimalPlan struct {
	fields []minimalField
}

var minimalPlans sync.Map // reflect.Type -> *minimalPlan or error

// minimalPlanFor resolves and caches the plan for a struct type.
func minimalPlanFor(structType reflect.Type) (*minimalPlan, error) {
	if cached, ok := minimalPlans.Load(structType); ok {
		if plan, ok := cached.(*minimalPlan); ok {
			return plan, nil
		}
		return nil, cached.(error)
	}
	plan, err := buildMinimalPlan(structType)
	if err != nil {
		minimalPlans.Store(structType, err)
		return nil, err
	}
	minimalPlans.Store(structType, plan)
	return plan, nil
}

func buildMinimalPlan(structType reflect.Type) (*minimalPlan, error) {
	if structType.Kind() != reflect.Struct {
		return nil, fmt.Errorf(
			"colbin: minimal mode encodes a struct, got %s", structType.Kind())
	}
	plan := &minimalPlan{}
	taken := map[uint8]string{}
	for index := range structType.NumField() {
		field := structType.Field(index)
		if !field.IsExported() {
			continue
		}
		_, explicitID, skip := parseCbTag(field)
		if skip {
			continue
		}
		// Hashed ids land anywhere in 0..254 and four key bits cannot hold them,
		// so minimal mode asks for the number rather than inventing one. The
		// error names the field, because that is the edit the caller has to make.
		if explicitID < 0 {
			return nil, fmt.Errorf(
				"colbin: minimal mode needs a numbered field id: %s.%s has no cb tag, "+
					"and a hashed id does not fit four key bits — tag it `cb:\"N\"` with N ≤ %d",
				minimalTypeName(structType), field.Name, minimal.MaxFields-1)
		}
		if explicitID >= minimal.MaxFields {
			return nil, fmt.Errorf(
				"colbin: minimal mode carries at most %d fields: %s.%s has id %d",
				minimal.MaxFields, minimalTypeName(structType), field.Name, explicitID)
		}
		key := uint8(explicitID)
		if other, clash := taken[key]; clash {
			return nil, fmt.Errorf(
				"colbin: minimal mode field id %d is on both %s.%s and %s.%s",
				key, minimalTypeName(structType), other, minimalTypeName(structType), field.Name)
		}
		taken[key] = field.Name

		op, err := minimalOpFor(field.Type)
		if err != nil {
			return nil, fmt.Errorf("colbin: %s.%s: %w", minimalTypeName(structType), field.Name, err)
		}
		plan.fields = append(plan.fields, minimalField{
			key:    key,
			offset: field.Offset,
			op:     op,
		})
	}
	if len(plan.fields) == 0 {
		return nil, fmt.Errorf("colbin: %s has no encodable field", minimalTypeName(structType))
	}
	return plan, nil
}

func minimalOpFor(fieldType reflect.Type) (minimalOp, error) {
	switch fieldType.Kind() {
	case reflect.Bool:
		return minOpBool, nil
	case reflect.Int8:
		return minOpInt8, nil
	case reflect.Int16:
		return minOpInt16, nil
	case reflect.Int32:
		return minOpInt32, nil
	case reflect.Int64, reflect.Int:
		return minOpInt64, nil
	case reflect.Uint8:
		return minOpUint8, nil
	case reflect.Uint16:
		return minOpUint16, nil
	case reflect.Uint32:
		return minOpUint32, nil
	case reflect.Uint64, reflect.Uint, reflect.Uintptr:
		return minOpUint64, nil
	case reflect.Float32:
		return minOpFloat32, nil
	case reflect.Float64:
		return minOpFloat64, nil
	case reflect.String:
		return minOpString, nil
	case reflect.Slice:
		return minimalSliceOp(fieldType.Elem())
	}
	return 0, fmt.Errorf(
		"minimal mode carries scalars, strings and slices of those, not %s", fieldType)
}

func minimalSliceOp(elementType reflect.Type) (minimalOp, error) {
	switch elementType.Kind() {
	case reflect.Uint8:
		return minOpBytes, nil
	case reflect.Int8:
		return minOpInt8s, nil
	case reflect.Int16:
		return minOpInt16s, nil
	case reflect.Int32:
		return minOpInt32s, nil
	case reflect.Int64, reflect.Int:
		return minOpInt64s, nil
	case reflect.Uint16:
		return minOpUint16s, nil
	case reflect.Uint32:
		return minOpUint32s, nil
	case reflect.Uint64, reflect.Uint:
		return minOpUint64s, nil
	case reflect.String:
		return minOpStrings, nil
	}
	return 0, fmt.Errorf(
		"minimal mode carries slices of integers and strings, not []%s", elementType)
}

// AppendMinimal encodes v in minimal mode onto dst, which may be nil.
//
// Int and uint are encoded as their 64-bit forms, so a message written on one
// platform reads on another — the wire never carries a width that depends on the
// compiler.
func AppendMinimal(dst []byte, v any) ([]byte, error) {
	value := reflect.ValueOf(v)
	for value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil, fmt.Errorf("colbin: minimal mode cannot encode a nil pointer")
		}
		value = value.Elem()
	}
	plan, err := minimalPlanFor(value.Type())
	if err != nil {
		return nil, err
	}
	if !value.CanAddr() {
		// A value handed in by interface is not addressable, and the plan reads
		// fields by offset. One copy into an addressable temporary is what makes
		// Marshal(v) work the same as Marshal(&v).
		addressable := reflect.New(value.Type()).Elem()
		addressable.Set(value)
		value = addressable
	}
	writer := minimal.Writer{Buffer: dst}
	writeMinimalPlan(&writer, plan, unsafe.Pointer(value.UnsafeAddr()))
	return writer.Buffer, nil
}

// writeMinimalPlan is the whole encoder: one switch over a resolved plan, no
// reflection left. A zero-valued field writes nothing, which the writer decides.
func writeMinimalPlan(writer *minimal.Writer, plan *minimalPlan, record unsafe.Pointer) {
	for index := range plan.fields {
		field := &plan.fields[index]
		at := unsafe.Add(record, field.offset)
		switch field.op {
		case minOpBool:
			writer.Bool(field.key, *(*bool)(at))
		case minOpInt8:
			writer.Int(field.key, int64(*(*int8)(at)))
		case minOpInt16:
			writer.Int(field.key, int64(*(*int16)(at)))
		case minOpInt32:
			writer.I32(field.key, *(*int32)(at))
		case minOpInt64:
			writer.Int(field.key, *(*int64)(at))
		case minOpUint8:
			writer.U16(field.key, uint16(*(*uint8)(at)))
		case minOpUint16:
			writer.U16(field.key, *(*uint16)(at))
		case minOpUint32:
			writer.U32(field.key, *(*uint32)(at))
		case minOpUint64:
			writer.Uint(field.key, *(*uint64)(at))
		case minOpFloat32:
			writer.F32(field.key, *(*float32)(at))
		case minOpFloat64:
			writer.F64(field.key, *(*float64)(at))
		case minOpString:
			writer.String(field.key, *(*string)(at))
		case minOpBytes:
			writer.Bytes(field.key, *(*[]byte)(at))
		case minOpInt8s:
			minimal.WriteInts(writer, field.key, *(*[]int8)(at))
		case minOpInt16s:
			minimal.WriteInts(writer, field.key, *(*[]int16)(at))
		case minOpInt32s:
			writer.Int32s(field.key, *(*[]int32)(at))
		case minOpInt64s:
			writer.Ints(field.key, *(*[]int64)(at))
		case minOpUint16s:
			writer.Uint16s(field.key, *(*[]uint16)(at))
		case minOpUint32s:
			minimal.WriteInts(writer, field.key, *(*[]uint32)(at))
		case minOpUint64s:
			minimal.WriteInts(writer, field.key, *(*[]uint64)(at))
		case minOpStrings:
			writer.Strings(field.key, *(*[]string)(at))
		}
	}
}

// MarshalMinimal encodes v in minimal mode.
func MarshalMinimal(v any) ([]byte, error) { return AppendMinimal(nil, v) }

// UnmarshalMinimal decodes a minimal message into dst, a non-nil pointer to a
// struct of the same shape.
//
// Every field of dst is set, including the ones the message omitted: an omitted
// key means the value was zero, so the destination is cleared first rather than
// left holding whatever it had.
func UnmarshalMinimal(data []byte, dst any) error {
	pointer := reflect.ValueOf(dst)
	if pointer.Kind() != reflect.Pointer || pointer.IsNil() {
		return fmt.Errorf("colbin: UnmarshalMinimal needs a non-nil pointer, got %T", dst)
	}
	value := pointer.Elem()
	plan, err := minimalPlanFor(value.Type())
	if err != nil {
		return err
	}
	value.SetZero()

	record := unsafe.Pointer(value.UnsafeAddr())
	reader := minimal.NewReader(data)
	for reader.More() {
		key := reader.Key()
		field := plan.find(key)
		if field == nil {
			// Not skippable: the header sizes a field but does not say which of
			// the four layouts it is, so a reader that does not know the key
			// cannot step over it. See minimal/README.md.
			return fmt.Errorf(
				"colbin: minimal message holds field id %d, which %s does not declare",
				key, value.Type())
		}
		readMinimalField(&reader, field, record)
	}
	return reader.Err()
}

func (plan *minimalPlan) find(key uint8) *minimalField {
	for index := range plan.fields {
		if plan.fields[index].key == key {
			return &plan.fields[index]
		}
	}
	return nil
}

func readMinimalField(reader *minimal.Reader, field *minimalField, record unsafe.Pointer) {
	at := unsafe.Add(record, field.offset)
	switch field.op {
	case minOpBool:
		*(*bool)(at) = reader.Bool()
	case minOpInt8:
		*(*int8)(at) = int8(reader.Int())
	case minOpInt16:
		*(*int16)(at) = int16(reader.Int())
	case minOpInt32:
		*(*int32)(at) = reader.I32()
	case minOpInt64:
		*(*int64)(at) = reader.Int()
	case minOpUint8:
		*(*uint8)(at) = uint8(reader.U16())
	case minOpUint16:
		*(*uint16)(at) = reader.U16()
	case minOpUint32:
		*(*uint32)(at) = reader.U32()
	case minOpUint64:
		*(*uint64)(at) = reader.Uint()
	case minOpFloat32:
		*(*float32)(at) = reader.F32()
	case minOpFloat64:
		*(*float64)(at) = reader.F64()
	case minOpString:
		*(*string)(at) = reader.String()
	case minOpBytes:
		// Copied, not aliased: the message buffer is usually a read buffer the
		// caller reuses, and a field pointing into it would change underneath.
		*(*[]byte)(at) = append([]byte(nil), reader.Bytes()...)
	case minOpInt8s:
		*(*[]int8)(at) = minimal.ReadInts(reader, []int8(nil))
	case minOpInt16s:
		*(*[]int16)(at) = minimal.ReadInts(reader, []int16(nil))
	case minOpInt32s:
		*(*[]int32)(at) = reader.Int32s(nil)
	case minOpInt64s:
		*(*[]int64)(at) = reader.Ints(nil)
	case minOpUint16s:
		*(*[]uint16)(at) = reader.Uint16s(nil)
	case minOpUint32s:
		*(*[]uint32)(at) = minimal.ReadInts(reader, []uint32(nil))
	case minOpUint64s:
		*(*[]uint64)(at) = minimal.ReadInts(reader, []uint64(nil))
	case minOpStrings:
		*(*[]string)(at) = reader.Strings(nil)
	}
}

// MinimalFieldIDs reports the wire key of every field, in declaration order, for
// a type minimal mode accepts. It exists for the same reason the compact mode's
// id dump does: the other language's reader needs the numbers, and reading them
// out of the tags by hand is how they drift.
func MinimalFieldIDs(v any) (map[string]uint8, error) {
	structType := reflect.TypeOf(v)
	for structType != nil && structType.Kind() == reflect.Pointer {
		structType = structType.Elem()
	}
	if structType == nil {
		return nil, fmt.Errorf("colbin: MinimalFieldIDs needs a struct, got nil")
	}
	plan, err := minimalPlanFor(structType)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]uint8, len(plan.fields))
	fieldIndex := 0
	for index := range structType.NumField() {
		field := structType.Field(index)
		if !field.IsExported() {
			continue
		}
		if _, _, skip := parseCbTag(field); skip {
			continue
		}
		ids[field.Name] = plan.fields[fieldIndex].key
		fieldIndex++
	}
	return ids, nil
}

// minimalTypeName is only for error text, and keeps an anonymous struct from
// printing as an empty name.
func minimalTypeName(structType reflect.Type) string {
	if name := structType.Name(); name != "" {
		return name
	}
	return strings.TrimSpace(structType.String())
}

// MinimalCodec is a handle for one type: the plan resolved once and held, so
// encoding a record costs neither the type lookup nor the reflect entry that
// MarshalMinimal repeats on every call. It is the same idea as Codec[T], for the
// same reason — many small messages, where that per-call work is a real share of
// the cost — and it is safe for concurrent use.
//
//	var chargeCodec = colbin.MustMinimalCodec[Charge]()
//
//	buf := make([]byte, 0, 64)
//	for _, charge := range charges {
//	    buf, _ = chargeCodec.Append(buf[:0], &charge)   // no allocation per record
//	    send(buf)
//	}
type MinimalCodec[T any] struct {
	plan *minimalPlan
}

// NewMinimalCodec builds the handle for T, which must be a struct minimal mode
// accepts.
func NewMinimalCodec[T any]() (*MinimalCodec[T], error) {
	var zero T
	plan, err := minimalPlanFor(reflect.TypeOf(zero))
	if err != nil {
		return nil, err
	}
	return &MinimalCodec[T]{plan: plan}, nil
}

// MustMinimalCodec is NewMinimalCodec for a package-level variable, where a type
// error is a programming error and there is nobody to return it to.
func MustMinimalCodec[T any]() *MinimalCodec[T] {
	codec, err := NewMinimalCodec[T]()
	if err != nil {
		panic(err)
	}
	return codec
}

// Append encodes value onto dst, which may be nil.
func (codec *MinimalCodec[T]) Append(dst []byte, value *T) []byte {
	writer := minimal.Writer{Buffer: dst}
	writeMinimalPlan(&writer, codec.plan, unsafe.Pointer(value))
	return writer.Buffer
}

// Encode is Append onto a fresh buffer.
func (codec *MinimalCodec[T]) Encode(value *T) []byte {
	return codec.Append(nil, value)
}

// Unmarshal decodes a message into value, zeroing it first: a key the message
// omits means the field was zero.
func (codec *MinimalCodec[T]) Unmarshal(data []byte, value *T) error {
	*value = *new(T)
	record := unsafe.Pointer(value)
	reader := minimal.NewReader(data)
	for reader.More() {
		key := reader.Key()
		field := codec.plan.find(key)
		if field == nil {
			return fmt.Errorf(
				"colbin: minimal message holds field id %d, which %T does not declare",
				key, *value)
		}
		readMinimalField(&reader, field, record)
	}
	return reader.Err()
}

// FieldIDs reports the wire key of every field, for handing to a reader in
// another language.
func (codec *MinimalCodec[T]) FieldIDs() map[string]uint8 {
	var zero T
	ids, _ := MinimalFieldIDs(zero)
	return ids
}
