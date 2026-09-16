package codec

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"unsafe"

	"github.com/ivanjoz/colbin/wire"
)

// The bridge from a Go type to the format, the same shape as the compact-mode
// bridge beside it: a plan resolved once per type and cached, then a flat switch
// over it per message.
//
// What the format is, and why it is a third mode rather than a variant of the
// other two, is in wire/README.md. What matters here is the one property that
// shapes this file: **a colbin message carries no version byte and no field
// ids beyond four bits**, so the type has to say everything. A field needs an
// explicit `cb:"N"` with N no higher than sixteen — ids count from one, and the
// key on the wire is N-1 — and a type that does not number its fields is refused
// rather than hashed into ids that would not fit.

// fieldOp says what a field is. It is the whole of what a plan holds about a
// field's type, and — since the schema section — the whole of what the *wire*
// holds about it too.
//
// # These values are on the wire
//
// A schema section describes a field as a key, a name and one of these numbers.
// Reordering the block therefore retypes every field of every section already
// written: an op is a byte, every byte is a valid op, and a reader would decode
// a string as an integer without anything looking wrong. New ops go on the end
// and none of these ever moves. TestFieldOpsArePinned is what catches it.
type fieldOp uint8

const (
	opBool fieldOp = iota
	opInt8
	opInt16
	opInt32
	opInt64
	opUint8
	opUint16
	opUint32
	opUint64
	opFloat32
	opFloat64
	opString
	opBytes
	opInt8s
	opInt16s
	opInt32s
	opInt64s
	opUint16s
	opUint32s
	opUint64s
	opStrings
	opStruct
	opStructs
	opMap
	opPointer
	// opAny is a field of type `any`, and opAnys a slice of them: a value with no
	// declared type, which says on the wire what it is. See dynamic.go.
	opAny
	opAnys
	// opPointerStruct is a *T where T is a struct: the body of opStruct, with the
	// key omitted when the pointer is nil. See pointer.go.
	//
	// It sits after opAnys rather than beside opPointer because these numbers are
	// format — a schema section writes the op byte — so a new one goes on the end
	// and everything already written keeps its meaning.
	opPointerStruct

	// opCount bounds the block, so a schema section carrying a number this
	// version does not assign is refused rather than indexed on.
	opCount
)

type planField struct {
	key    uint8
	offset uintptr
	op     fieldOp
	// Composites only: the child's plan, and what a slice of them needs to walk
	// its elements without reflection per element.
	sub       *typePlan
	sliceType reflect.Type
	stride    uintptr
	// Pointers only: the op of what is pointed at. See pointer.go.
	elemOp fieldOp
	// Maps only: what their keys and values are. See maps.go.
	keyKind, valueKind mapKind
}

type typePlan struct {
	fields []planField
	// names is what each field is called, parallel to fields. It is the one
	// thing a schema section carries that the encode and decode paths never
	// read — see schema.go.
	//
	// It is a slice beside fields rather than a string inside planField because
	// planField is loaded once per field per *message* and a name is read once
	// per *type*: putting it in the struct would drag sixteen bytes through the
	// cache on every field of every decode to serve a path that does not run.
	names []string
	// What decides the key width, resolved once with the plan. See wide.go.
	anyKeyPastNarrow bool
	hasStrings       bool
	// hasDynamic says some field of this type carries a value whose type is not
	// declared — an `any`, a slice of them, or a map of them. Such a value is
	// written as a descriptor that names its own class, and a four-bit descriptor
	// has no room for one, so the scope goes wide. See dynamic.go.
	hasDynamic bool
	// byKey maps a wire key to an index into fields, or -1. See find.
	byKey []int16
	// sizeHint is what Marshal reserves. See planSizeHint.
	sizeHint int
	// canTable says every field of this plan is a column the column codec
	// carries, which is what lets a slice of it be transposed. See table.go.
	canTable bool
	// isWide is what wide() resolved to, held rather than recomputed: it is read
	// once per message on the hot path and never changes after the plan is built,
	// except by SetPacked5, which invalidates the cache.
	isWide bool
	// simple says nothing in this type is nested, which sends a message down the
	// walk in scalars.go instead. See simplePlan.
	simple bool
	// derivedKeys says at least one id came from a field name rather than a tag.
	// Such a key lands anywhere in 0..255, so the type uses eight-bit keys. See
	// assignKeys.
	derivedKeys bool
	// fromSchema says this plan was parsed off a wire section rather than
	// resolved from a Go type, which means **it has no layout in it**: every
	// offset and stride is zero, and writing a field through one would store
	// over the head of the record. Only the JSON walkers may use such a plan.
	// See schema_plan.go.
	fromSchema bool
	// envelope says this plan is the synthetic one-field struct that carries a
	// slice or a map at the root, rather than a type somebody declared. The
	// encoding is an ordinary message either way; what the flag changes is the
	// JSON walk, which unwraps it so that the document is the value and not a
	// struct holding one. See envelope.go.
	envelope bool
}

var planCache sync.Map // reflect.Type -> *typePlan or error

// planFor resolves and caches the plan for a struct type.
func planFor(structType reflect.Type) (*typePlan, error) {
	if cached, ok := planCache.Load(structType); ok {
		if plan, ok := cached.(*typePlan); ok {
			return plan, nil
		}
		return nil, cached.(error)
	}
	plan, err := planForBuilding(structType, map[reflect.Type]*typePlan{})
	if err != nil {
		planCache.Store(structType, err)
		return nil, err
	}
	planCache.Store(structType, plan)
	return plan, nil
}

// planForBuilding resolves a type, reusing the plan for one already under
// construction further up the stack.
//
// That is what makes a recursive type terminate: the plan goes into `building`
// before its fields are walked, so a field that reaches back to it finds the
// half-built plan and points at it rather than starting again.
func planForBuilding(structType reflect.Type, building map[reflect.Type]*typePlan) (*typePlan, error) {
	if plan, ok := building[structType]; ok {
		return plan, nil
	}
	if cached, ok := planCache.Load(structType); ok {
		if plan, ok := cached.(*typePlan); ok {
			return plan, nil
		}
	}
	return buildPlan(structType, building)
}

func buildPlan(structType reflect.Type, building map[reflect.Type]*typePlan) (*typePlan, error) {
	if structType.Kind() != reflect.Struct {
		return nil, fmt.Errorf(
			"colbin: the format encodes a struct, got %s", structType.Kind())
	}
	plan := &typePlan{}
	building[structType] = plan
	// Keys are assigned after the walk, not during it: an unnumbered field takes
	// the hash of its name and then probes past whatever is already used, so it
	// has to know every *explicit* id first. See assignKeys.
	var (
		hashNames  []string // the name the id is derived from, tag override included
		fieldNames []string // the Go name, for errors
		declared   []int    // >= 1 explicit, noFieldID to be derived
	)
	for index := range structType.NumField() {
		field := structType.Field(index)
		if !field.IsExported() {
			continue
		}
		hashName, explicitID, skip := parseCbTag(field)
		if skip {
			continue
		}
		// Ids are one-based and a key is one byte, so 1..256 is the whole range at
		// either width. Zero gets its own sentence, because it is what a tag from
		// the old zero-based numbering says and a reader deserves to be told that
		// rather than left to wonder why every field moved by one.
		if explicitID != noFieldID && (explicitID < 1 || explicitID > wire.MaxWideFields) {
			if explicitID == 0 {
				return nil, fmt.Errorf(
					"colbin: field ids are one-based, so %s.%s cannot be id 0: the first field is `cb:\"1\"`",
					typeName(structType), field.Name)
			}
			return nil, fmt.Errorf(
				"colbin: a field id is one byte counted from one: %s.%s has id %d, and the range is 1..%d",
				typeName(structType), field.Name, explicitID, wire.MaxWideFields)
		}

		op, sub, sliceType, stride, compositeErr := compositeOpFor(field.Type, building)
		var keyKind, valueKind mapKind
		var elemOp fieldOp
		// A composite that failed to plan is reported as itself. Only
		// errNotComposite means "try the other kinds" — see errNotComposite. The
		// error is passed up unwrapped because it already names the type and the
		// field that could not be carried, which is the one the reader has to go
		// and change; another frame of `colbin: Outer.Field:` in front of it only
		// buries that.
		if compositeErr != nil && !errors.Is(compositeErr, errNotComposite) {
			return nil, compositeErr
		}
		if compositeErr != nil {
			switch field.Type.Kind() {
			case reflect.Map:
				var err error
				if keyKind, valueKind, err = mapOpFor(field.Type); err != nil {
					return nil, fmt.Errorf("colbin: %s.%s: %w",
						typeName(structType), field.Name, err)
				}
				op, sliceType = opMap, field.Type
			case reflect.Pointer:
				var err error
				if elemOp, err = pointerOpFor(field.Type); err != nil {
					return nil, fmt.Errorf("colbin: %s.%s: %w",
						typeName(structType), field.Name, err)
				}
				op, sliceType = opPointer, field.Type
			default:
				var err error
				if op, err = opFor(field.Type); err != nil {
					return nil, fmt.Errorf("colbin: %s.%s: %w",
						typeName(structType), field.Name, err)
				}
			}
		}
		// A composite no longer forces the wide width: §2.5's narrow composite
		// nibble carries the same byte length with the class coming from the
		// schema. What it still cannot do is let a reader skip one it does not
		// know, which is K4's standing trade.
		if op == opString || op == opStrings || elemOp == opString {
			plan.hasStrings = true
		}
		if op == opAny || op == opAnys || (op == opMap && valueKind == mapAny) {
			plan.hasDynamic = true
		}
		plan.fields = append(plan.fields, planField{
			offset:    field.Offset,
			op:        op,
			sub:       sub,
			sliceType: sliceType,
			stride:    stride,
			elemOp:    elemOp,
			keyKind:   keyKind,
			valueKind: valueKind,
		})
		hashNames = append(hashNames, hashName)
		fieldNames = append(fieldNames, field.Name)
		declared = append(declared, explicitID)
	}
	if len(plan.fields) > wire.MaxWideFields {
		return nil, fmt.Errorf(
			"colbin: %s has %d encodable fields, and a one-byte key holds %d",
			typeName(structType), len(plan.fields), wire.MaxWideFields)
	}
	if err := plan.assignKeys(structType, hashNames, fieldNames, declared); err != nil {
		return nil, err
	}
	// The name a field hashes to an id by is the name a reader without the Go
	// type calls it: `cb:"label,3"` names the field as well as numbering it, and
	// an untagged field is called what Go calls it. One name, not two.
	plan.names = hashNames
	plan.indexKeys()
	plan.envelope = isEnvelope(structType)
	plan.isWide = plan.wide()
	plan.canTable = plan.transposable()
	plan.simple = plan.simplePlan()
	plan.sizeHint = plan.planSizeHint()
	if len(plan.fields) == 0 {
		return nil, fmt.Errorf("colbin: %s has no encodable field", typeName(structType))
	}
	return plan, nil
}

// assignKeys gives every field its wire key: the declared ones first, so that
// the derived ones can probe past them.
//
// The order matters and is the contract. A field that says `cb:"5"` gets key 4
// whatever else is in the type, and a field that says nothing takes fnv8 of its
// name and then the next free slot upward. Doing it the other way round would
// let a hash squat on a number somebody had asked for.
//
// This is the one place the one-based id becomes the zero-based key, and it is
// the only subtraction in the package. Everything downstream — the clash map,
// the plan, the schema section, the generated code — is keys, so nothing else
// has to remember which of the two numbers it is holding. The errors are the
// exception and speak in ids, because an id is what the tag says.
func (plan *typePlan) assignKeys(structType reflect.Type, hashNames, fieldNames []string, declared []int) error {
	taken := make(map[uint8]string, len(declared))
	for index, id := range declared {
		if id == noFieldID {
			continue
		}
		key := uint8(id - 1)
		if other, clash := taken[key]; clash {
			return fmt.Errorf(
				"colbin: the format field id %d is on both %s.%s and %s.%s",
				id, typeName(structType), other, typeName(structType), fieldNames[index])
		}
		taken[key] = fieldNames[index]
		plan.fields[index].key = key
		if id > wire.MaxFields {
			// Past the sixteenth id the message has to use eight-bit keys, which
			// costs a byte per present field and buys 256 of them.
			plan.anyKeyPastNarrow = true
		}
	}
	for index, id := range declared {
		if id != noFieldID {
			continue
		}
		key := probeFieldID(fnv8(hashNames[index]), taken)
		taken[key] = fieldNames[index]
		plan.fields[index].key = key
		plan.derivedKeys = true
	}
	return nil
}

func opFor(fieldType reflect.Type) (fieldOp, error) {
	switch fieldType.Kind() {
	case reflect.Bool:
		return opBool, nil
	case reflect.Int8:
		return opInt8, nil
	case reflect.Int16:
		return opInt16, nil
	case reflect.Int32:
		return opInt32, nil
	case reflect.Int64, reflect.Int:
		return opInt64, nil
	case reflect.Uint8:
		return opUint8, nil
	case reflect.Uint16:
		return opUint16, nil
	case reflect.Uint32:
		return opUint32, nil
	case reflect.Uint64, reflect.Uint, reflect.Uintptr:
		return opUint64, nil
	case reflect.Float32:
		return opFloat32, nil
	case reflect.Float64:
		return opFloat64, nil
	case reflect.String:
		return opString, nil
	case reflect.Slice:
		return sliceOp(fieldType.Elem())
	case reflect.Interface:
		if err := dynamicInterface(fieldType); err != nil {
			return 0, err
		}
		return opAny, nil
	}
	return 0, fmt.Errorf(
		"the format carries scalars, strings, slices of those, nested structs, slices of structs, maps and `any`, not %s", fieldType)
}

// dynamicInterface refuses an interface that is not the empty one.
//
// A dynamic value is decoded into whatever the wire says it is, and there is no
// way to promise that will satisfy a method set. `any` is the shape this carries
// and saying so at plan time is better than a type assertion failing per value.
func dynamicInterface(fieldType reflect.Type) error {
	if fieldType.NumMethod() != 0 {
		return fmt.Errorf(
			"a dynamic value has to be `any`, and %s has %d method(s)",
			fieldType, fieldType.NumMethod())
	}
	return nil
}

func sliceOp(elementType reflect.Type) (fieldOp, error) {
	switch elementType.Kind() {
	case reflect.Uint8:
		return opBytes, nil
	case reflect.Int8:
		return opInt8s, nil
	case reflect.Int16:
		return opInt16s, nil
	case reflect.Int32:
		return opInt32s, nil
	case reflect.Int64, reflect.Int:
		return opInt64s, nil
	case reflect.Uint16:
		return opUint16s, nil
	case reflect.Uint32:
		return opUint32s, nil
	case reflect.Uint64, reflect.Uint:
		return opUint64s, nil
	case reflect.String:
		return opStrings, nil
	case reflect.Interface:
		if err := dynamicInterface(elementType); err != nil {
			return 0, err
		}
		return opAnys, nil
	}
	return 0, fmt.Errorf(
		"the format carries slices of integers, strings and `any`, not []%s", elementType)
}

// Append encodes v onto dst, which may be nil.
//
// Int and uint are encoded as their 64-bit forms, so a message written on one
// platform reads on another — the wire never carries a width that depends on the
// compiler.
func Append(dst []byte, v any) ([]byte, error) {
	value, plan, err := addressable(v)
	if err != nil {
		return nil, err
	}
	record := unsafe.Pointer(value.UnsafeAddr())
	if plan.hasDynamic {
		return appendPlanChecked(dst, plan, record)
	}
	return appendPlan(dst, plan, record), nil
}

// appendPlanChecked is appendPlan for a type whose encode can fail.
//
// Only a dynamic value can: everything else was resolved at plan time, which is
// the whole reason the writers below take no error path. An `any` is resolved
// per value, so it can hold a Go type with no form on the wire, or hold itself.
// A type with no `any` in it does not pay for this.
func appendPlanChecked(dst []byte, plan *typePlan, record unsafe.Pointer) ([]byte, error) {
	root := rootStructNarrow
	if plan.isWide {
		root = rootStructWide
	}
	var buf scratch
	return appendRunInto(append(dst, root), plan, record, &buf)
}

// addressable resolves v to a plan and to a value the plan can read fields out
// of by offset.
//
// A value handed in by interface is not addressable, so one copy into a
// temporary is what makes Marshal(v) work the same as Marshal(&v).
func addressable(v any) (reflect.Value, *typePlan, error) {
	value := reflect.ValueOf(v)
	for value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return reflect.Value{}, nil, fmt.Errorf("colbin: cannot encode a nil pointer")
		}
		value = value.Elem()
	}
	plan, err := planForRoot(value.Type())
	if err != nil {
		return reflect.Value{}, nil, err
	}
	if !value.CanAddr() {
		addressable := reflect.New(value.Type()).Elem()
		addressable.Set(value)
		value = addressable
	}
	return value, plan, nil
}

// appendPlan writes the root descriptor and then the run, at whichever key width
// the plan resolved to.
func appendPlan(dst []byte, plan *typePlan, record unsafe.Pointer) []byte {
	if plan.isWide {
		// Two declarations rather than one, because escape analysis is per
		// variable: sharing a writer with the composite branch would heap it on
		// the flat path too, which is the allocation this split exists to remove.
		if plan.simple {
			writer := wire.Writer8{Buffer: append(dst, rootStructWide)}
			appendScalarsWide(&writer, plan, record)
			return writer.Buffer
		}
		writer := wire.Writer8{Buffer: append(dst, rootStructWide)}
		var buf scratch
		appendWide(&writer, plan, record, &buf)
		return writer.Buffer
	}
	writer := wire.Writer{Buffer: append(dst, rootStructNarrow)}
	if plan.simple {
		appendScalars(&writer, plan, record)
		return writer.Buffer
	}
	var narrowBuf scratch
	writePlan(&writer, plan, record, &narrowBuf)
	return writer.Buffer
}

// writePlan is the whole of the narrow-key encoder: one switch over a resolved
// plan, no reflection left. A zero-valued field writes nothing, which the writer
// decides.
//
// It is a separate function from appendWide rather than one with a width flag,
// for the reason wire keeps the two widths in separate files: a width the
// compiler cannot see is a width it cannot fold.
func writePlan(writer *wire.Writer, plan *typePlan, record unsafe.Pointer, buf *scratch) {
	for index := range plan.fields {
		field := &plan.fields[index]
		at := unsafe.Add(record, field.offset)
		switch field.op {
		case opBool:
			writer.Bool(field.key, *(*bool)(at))
		case opInt8:
			writer.Int(field.key, int64(*(*int8)(at)))
		case opInt16:
			writer.Int(field.key, int64(*(*int16)(at)))
		case opInt32:
			writer.I32(field.key, *(*int32)(at))
		case opInt64:
			writer.Int(field.key, *(*int64)(at))
		case opUint8:
			writer.U16(field.key, uint16(*(*uint8)(at)))
		case opUint16:
			writer.U16(field.key, *(*uint16)(at))
		case opUint32:
			writer.U32(field.key, *(*uint32)(at))
		case opUint64:
			writer.Uint(field.key, *(*uint64)(at))
		case opFloat32:
			writer.F32(field.key, *(*float32)(at))
		case opFloat64:
			writer.F64(field.key, *(*float64)(at))
		case opString:
			writer.String(field.key, *(*string)(at))
		case opBytes:
			writer.Bytes(field.key, *(*[]byte)(at))
		case opInt8s:
			writer.Int8s(field.key, *(*[]int8)(at))
		case opInt16s:
			writer.Int16s(field.key, *(*[]int16)(at))
		case opInt32s:
			writer.Int32s(field.key, *(*[]int32)(at))
		case opInt64s:
			writer.Ints(field.key, *(*[]int64)(at))
		case opUint16s:
			writer.Uint16s(field.key, *(*[]uint16)(at))
		case opUint32s:
			writer.Uint32s(field.key, *(*[]uint32)(at))
		case opUint64s:
			writer.Uint64s(field.key, *(*[]uint64)(at))
		case opStrings:
			writer.Strings(field.key, *(*[]string)(at))
		case opStruct:
			appendNarrowStruct(writer, field, at, buf)
		case opStructs:
			appendNarrowStructs(writer, field, at, buf)
		case opMap:
			appendNarrowMap(writer, field, at)
		case opPointer:
			appendPointer(writer, field, at)
		case opPointerStruct:
			appendNarrowPointerStruct(writer, field, at, buf)
		}
	}
}

// planSizeHint is a starting capacity for Marshal: the root descriptor, plus the
// largest a fixed-width field can be, plus a little for the variable ones.
//
// Marshal onto a nil buffer grew the slice three times on a six-field record —
// which is what protobuf avoids by sizing the message first, and what made its
// Marshal one allocation where this was three. Sizing exactly would need a pass
// over the value; a bound from the type needs none and gets the same answer for
// every record that does not hold a long string.
func (plan *typePlan) planSizeHint() int {
	total := 1 // the root descriptor
	for _, field := range plan.fields {
		width := 2 // key or descriptor, plus one payload byte
		switch field.op {
		case opPointer:
			width = 9 // the widest a pointee can be without being a long string
		case opInt64, opUint64, opFloat64:
			width = 9
		case opInt32, opUint32, opFloat32:
			width = 5
		case opString, opBytes, opInt8s, opInt16s, opInt32s, opInt64s,
			opUint16s, opUint32s, opUint64s, opStrings:
			width = 18 // a short string or a small array, inline
		}
		total += width
	}
	return total
}

// Marshal encodes v in the format.
func Marshal(v any) ([]byte, error) {
	value, plan, err := addressable(v)
	if err != nil {
		return nil, err
	}
	// One plan lookup, not two: sizing the buffer and writing it need the same
	// plan, and resolving it twice cost about 12 ns of a 95 ns Marshal.
	dst := make([]byte, 0, plan.sizeHint)
	record := unsafe.Pointer(value.UnsafeAddr())
	if plan.hasDynamic {
		return appendPlanChecked(dst, plan, record)
	}
	return appendPlan(dst, plan, record), nil
}

// Unmarshal decodes a colbin message into dst, a non-nil pointer to a
// struct of the same shape.
//
// Every field of dst is set, including the ones the message omitted: an omitted
// key means the value was zero, so the destination is cleared first rather than
// left holding whatever it had.
func Unmarshal(data []byte, dst any) error {
	pointer := reflect.ValueOf(dst)
	if pointer.Kind() != reflect.Pointer || pointer.IsNil() {
		return fmt.Errorf("colbin: Unmarshal needs a non-nil pointer, got %T", dst)
	}
	value := pointer.Elem()
	plan, err := planForRoot(value.Type())
	if err != nil {
		return err
	}
	value.SetZero()

	record := unsafe.Pointer(value.UnsafeAddr())
	section, body, wide, ok := rootParts(data)
	if !ok {
		return errBadRoot(data, value.Type())
	}
	if wide {
		return unmarshalWide(body, section, plan, record, value.Type())
	}
	return unmarshalNarrow(body, plan, record, value.Type())
}

// unmarshalNarrow decodes a narrow-key message into an already-zeroed record.
//
// An unknown key is refused rather than skipped. Four descriptor bits have no
// room for a class, so the same bits mean different things under different keys
// and nothing can size a field it cannot classify — which is the trade the
// narrow width makes for its byte. A type that needs to evolve past its readers
// should carry an id above sixteen, which puts the message on the wide path.
func unmarshalNarrow(body []byte, plan *typePlan, record unsafe.Pointer, what reflect.Type) error {
	reader := wire.NewReader(body)
	if plan.simple {
		if !readScalars(&reader, plan, record) {
			return errUnknownNarrowKey(reader.Key(), what)
		}
		return reader.Err()
	}
	var buf scratch
	for reader.More() {
		key := reader.Key()
		field := plan.find(key)
		if field == nil {
			return errUnknownNarrowKey(key, what)
		}
		readField(&reader, field, record, &buf)
	}
	return reader.Err()
}

// errUnknownNarrowKey names the field a narrow message carried and the type did
// not declare. It is out of line so that neither decode loop carries the
// formatting for a failure that ends the message anyway.
func errUnknownNarrowKey(key uint8, what reflect.Type) error {
	return fmt.Errorf(
		"colbin: message holds field id %d, which %s does not declare, "+
			"and a narrow key cannot be skipped", key, what)
}

// find resolves a wire key to its field.
//
// It is a table rather than a scan. Scanning was O(fields) per field and so
// O(fields²) per record, and measured 24% of a ten-field decode — the single
// largest line in the profile, for a lookup whose answer is fixed the moment the
// plan is built.
func (plan *typePlan) find(key uint8) *planField {
	if index := plan.findIndex(key); index >= 0 {
		return &plan.fields[index]
	}
	return nil
}

// findIndex is find for a caller that needs the field's position as well as the
// field — which is what a walk carrying a name per field does, and what a
// present-field bitmap is indexed by. See json.go.
func (plan *typePlan) findIndex(key uint8) int {
	// A key past the largest the type declares is the unknown-field case, which
	// is the one this has to get right rather than index into.
	if int(key) >= len(plan.byKey) {
		return -1
	}
	return int(plan.byKey[key])
}

// indexKeys builds the lookup table. It is sized to the largest key rather than
// to 256, so a narrow type spends sixteen bytes and not a quarter of a kilobyte.
func (plan *typePlan) indexKeys() {
	var highest uint8
	for _, field := range plan.fields {
		highest = max(highest, field.key)
	}
	plan.byKey = make([]int16, int(highest)+1)
	for index := range plan.byKey {
		plan.byKey[index] = -1
	}
	for index, field := range plan.fields {
		plan.byKey[field.key] = int16(index)
	}
}

func readField(reader *wire.Reader, field *planField, record unsafe.Pointer, buf *scratch) {
	at := unsafe.Add(record, field.offset)
	switch field.op {
	case opBool:
		*(*bool)(at) = reader.Bool()
	case opInt8:
		*(*int8)(at) = int8(reader.Int())
	case opInt16:
		*(*int16)(at) = int16(reader.Int())
	case opInt32:
		*(*int32)(at) = reader.I32()
	case opInt64:
		*(*int64)(at) = reader.Int()
	case opUint8:
		*(*uint8)(at) = uint8(reader.U16())
	case opUint16:
		*(*uint16)(at) = reader.U16()
	case opUint32:
		*(*uint32)(at) = reader.U32()
	case opUint64:
		*(*uint64)(at) = reader.Uint()
	case opFloat32:
		*(*float32)(at) = reader.F32()
	case opFloat64:
		*(*float64)(at) = reader.F64()
	case opString:
		*(*string)(at) = reader.String()
	case opBytes:
		// Copied, not aliased: the message buffer is usually a read buffer the
		// caller reuses, and a field pointing into it would change underneath.
		*(*[]byte)(at) = append([]byte(nil), reader.Bytes()...)
	case opInt8s:
		*(*[]int8)(at) = reader.Int8s(nil)
	case opInt16s:
		*(*[]int16)(at) = reader.Int16s(nil)
	case opInt32s:
		*(*[]int32)(at) = reader.Int32s(nil)
	case opInt64s:
		*(*[]int64)(at) = reader.Ints(nil)
	case opUint16s:
		*(*[]uint16)(at) = reader.Uint16s(nil)
	case opUint32s:
		*(*[]uint32)(at) = reader.Uint32s(nil)
	case opUint64s:
		*(*[]uint64)(at) = reader.Uint64s(nil)
	case opStrings:
		*(*[]string)(at) = reader.Strings(nil)
	case opStruct:
		readNarrowStruct(reader, field, at, buf)
	case opStructs:
		readNarrowStructs(reader, field, at, buf)
	case opMap:
		readNarrowMap(reader, field, at)
	case opPointer:
		readPointer(reader, field, at)
	case opPointerStruct:
		readNarrowPointerStruct(reader, field, at, buf)
	}
}

// FieldIDs reports the wire key of every field, in declaration order, for
// a type the format accepts. It exists for the same reason the compact mode's
// id dump does: the other language's reader needs the numbers, and reading them
// out of the tags by hand is how they drift.
func FieldIDs(v any) (map[string]uint8, error) {
	structType := reflect.TypeOf(v)
	for structType != nil && structType.Kind() == reflect.Pointer {
		structType = structType.Elem()
	}
	if structType == nil {
		return nil, fmt.Errorf("colbin: FieldIDs needs a struct, got nil")
	}
	plan, err := planFor(structType)
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

// typeName is only for error text, and keeps an anonymous struct from
// printing as an empty name.
func typeName(structType reflect.Type) string {
	if name := structType.Name(); name != "" {
		return name
	}
	return strings.TrimSpace(structType.String())
}

// Codec is a handle for one type: the plan resolved once and held, so
// encoding a record costs neither the type lookup nor the reflect entry that
// Marshal repeats on every call. It is the same idea as Codec[T], for the
// same reason — many small messages, where that per-call work is a real share of
// the cost — and it is safe for concurrent use.
//
//	var chargeCodec = colbin.MustCodec[Charge]()
//
//	buf := make([]byte, 0, 64)
//	for _, charge := range charges {
//	    buf, _ = chargeCodec.Append(buf[:0], &charge)   // no allocation per record
//	    send(buf)
//	}
type Codec[T any] struct {
	plan *typePlan
	// what names T in an error. Held rather than derived, because reflect.TypeOf
	// on the value boxes it — one allocation per decode for a message that
	// almost never fails.
	what reflect.Type
}

// NewCodec builds the handle for T, which must be a struct the format
// accepts.
func NewCodec[T any]() (*Codec[T], error) {
	var zero T
	plan, err := planFor(reflect.TypeOf(zero))
	if err != nil {
		return nil, err
	}
	return &Codec[T]{plan: plan, what: reflect.TypeOf(zero)}, nil
}

// MustCodec is NewCodec for a package-level variable, where a type
// error is a programming error and there is nobody to return it to.
func MustCodec[T any]() *Codec[T] {
	codec, err := NewCodec[T]()
	if err != nil {
		panic(err)
	}
	return codec
}

// Append encodes value onto dst, which may be nil.
//
// It cannot fail, and for every type but one that is a fact rather than a
// promise: the shape was resolved when the handle was built, so there is nothing
// left to go wrong per record. The exception is a T holding an `any`, whose
// shape is resolved per *value* — it can hold a Go type the format has no form
// for, or hold itself. Append writes a null in place of such a value and has
// nowhere to say so; AppendChecked is the same encode with the error.
func (codec *Codec[T]) Append(dst []byte, value *T) []byte {
	record := unsafe.Pointer(value)
	if codec.plan.isWide {
		if codec.plan.simple {
			writer := wire.Writer8{Buffer: append(dst, rootStructWide)}
			appendScalarsWide(&writer, codec.plan, record)
			return writer.Buffer
		}
		writer := wire.Writer8{Buffer: append(dst, rootStructWide)}
		var buf scratch
		appendWide(&writer, codec.plan, record, &buf)
		return writer.Buffer
	}
	writer := wire.Writer{Buffer: append(dst, rootStructNarrow)}
	if codec.plan.simple {
		appendScalars(&writer, codec.plan, record)
		return writer.Buffer
	}
	var narrowBuf scratch
	writePlan(&writer, codec.plan, record, &narrowBuf)
	return writer.Buffer
}

// Encode is Append onto a fresh buffer.
func (codec *Codec[T]) Encode(value *T) []byte {
	return codec.Append(nil, value)
}

// AppendChecked is Append with the failure a dynamic value can have.
//
// For a T with no `any` in it the error is always nil and Append is the same
// call without the check. For one with an `any`, this is the way to hear that a
// value could not be carried rather than to find a null where it was.
func (codec *Codec[T]) AppendChecked(dst []byte, value *T) ([]byte, error) {
	if !codec.plan.hasDynamic {
		return codec.Append(dst, value), nil
	}
	return appendPlanChecked(dst, codec.plan, unsafe.Pointer(value))
}

// Unmarshal decodes a message into value, zeroing it first: a key the message
// omits means the field was zero.
func (codec *Codec[T]) Unmarshal(data []byte, value *T) error {
	*value = *new(T)
	record := unsafe.Pointer(value)
	section, body, wide, ok := rootParts(data)
	if !ok {
		return errBadRoot(data, codec.what)
	}
	if wide {
		return unmarshalWide(body, section, codec.plan, record, codec.what)
	}
	return unmarshalNarrow(body, codec.plan, record, codec.what)
}

// FieldIDs reports the wire key of every field, for handing to a reader in
// another language.
func (codec *Codec[T]) FieldIDs() map[string]uint8 {
	var zero T
	ids, _ := FieldIDs(zero)
	return ids
}
