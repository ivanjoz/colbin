// Package codec is colbin's reflection façade and source generator. The
// supported API is the colbin package, which forwards here; this one is what it
// is built on.
package codec

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/ivanjoz/colbin/column"
	"github.com/ivanjoz/colbin/wire"
)

// The bridge from a Go type to the format: a plan resolved once per type and
// cached, then a switch over it per message.
//
// A field's id is its `cb:"N"` tag, counted from one, and its key on the wire is
// N-1; an untagged field takes the fnv8 hash of its name (tag.go). The key width
// is the type's: four bits while every id fits sixteen, eight otherwise (wide.go).

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

// fitsInt32 is whether a scalar op's values all fit an int32, so that a table
// column of it can be gathered and encoded at that width.
func (op fieldOp) fitsInt32() bool {
	if !column.NarrowColumns {
		return false
	}
	switch op {
	case opBool, opInt8, opInt16, opInt32, opUint8, opUint16:
		return true
	}
	return false
}

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
	// Maps only: what their keys and values are. See maps.go. typedMap is the
	// map's compiled walks, when its type has them. See maps_typed.go.
	keyKind, valueKind mapKind
	typedMap           *mapOps
	// native says op came from a Go int, uint or uintptr, whose width is the
	// platform's. The plan reads it at that width; the schema section names the
	// 64-bit op either way, so a section does not depend on where it was written.
	native bool
	// indirect says the field is a pointer to the slice or map the rest of this
	// struct describes: everything above is the pointee's, and only the walks
	// that reach the field's memory step through it. See pointer.go.
	indirect bool
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
	// hasDynamic says some field of this type carries a value whose type is not
	// declared — an `any`, a slice of them, or a map of them. Such a value is
	// written as a descriptor that names its own class, and a four-bit descriptor
	// has no room for one, so the scope goes wide. See dynamic.go.
	hasDynamic bool
	// byKey maps a wire key to an index into fields, or -1. See find.
	byKey []int16
	// sizeHint is what Marshal reserves for a record with no slices of structs.
	// See planSizeHint, and sizeFor for the rest.
	sizeHint int
	// structSlices is the index of every field that is a slice of structs held
	// directly, which is what sizeFor reads the lengths of.
	structSlices []int32
	// sizeRatio is what the last encodes came to against sizeFor, in
	// sizeRatioShift fixed point, or zero before the first. See reserve.
	sizeRatio atomic.Uint32
	// canTable says every field of this plan is a column the column codec
	// carries, which is what lets a slice of it be transposed. See table.go.
	canTable bool
	// isWide is what wide() resolved to, held rather than recomputed: it is read
	// once per message on the hot path and never changes after the plan is built.
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
	// page says this plan is a continuation page of a paged type: the fields
	// past the first 255, linked from the page before under key 255. ids is the
	// first page's, holding every field's id in declaration order, which a paged
	// type's keys no longer say on their own. See pages.go.
	page bool
	ids  []uint16
	// rows is the plan compiled for writePlan, or nil. See rows.go.
	rows *rowProgram
	// reset clears a record and keeps its slices' arrays, or is nil when the
	// type is cleared whole. See reset.go.
	reset *resetProgram
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
			// An embedded struct whose type is unexported still promotes its
			// exported fields, and dropping them would lose data with no error.
			// They are not carried — embedding is not flattened — so say so.
			if field.Anonymous && field.Tag.Get("cb") != "-" && hasExportedField(field.Type) {
				return nil, fmt.Errorf(
					"colbin: %s embeds %s, whose exported fields would be dropped: "+
						"name the field, or tag it `cb:\"-\"`",
					typeName(structType), field.Type)
			}
			continue
		}
		hashName, explicitID, skip, err := parseCbTag(field)
		if err != nil {
			return nil, fmt.Errorf("colbin: %s.%s: %w", typeName(structType), field.Name, err)
		}
		if skip {
			continue
		}
		// Ids are one-based, and past 255 they go on pages, so 1..maxFieldID is the
		// whole range. Zero gets its own sentence, because it is what a tag from
		// zero-based numbering says.
		if explicitID != noFieldID && (explicitID < 1 || explicitID > maxFieldID) {
			if explicitID == 0 {
				return nil, fmt.Errorf(
					"colbin: field ids are one-based, so %s.%s cannot be id 0: the first field is `cb:\"1\"`",
					typeName(structType), field.Name)
			}
			return nil, fmt.Errorf(
				"colbin: %s.%s has id %d, and the range is 1..%d",
				typeName(structType), field.Name, explicitID, maxFieldID)
		}

		// A pointer to a slice or a map is planned as its pointee and marked, so
		// that every question about what the field holds gets the pointee's answer.
		fieldType, indirect := field.Type, pointsToCollection(field.Type)
		if indirect {
			fieldType = fieldType.Elem()
		}
		op, sub, sliceType, stride, compositeErr := compositeOpFor(fieldType, building)
		var keyKind, valueKind mapKind
		var typedMap *mapOps
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
			switch fieldType.Kind() {
			case reflect.Map:
				var err error
				if keyKind, valueKind, err = mapOpFor(fieldType); err != nil {
					return nil, fmt.Errorf("colbin: %s.%s: %w",
						typeName(structType), field.Name, err)
				}
				op, sliceType, typedMap = opMap, fieldType, typedMapFor(fieldType)
				if valueKind == mapStruct {
					// The value's plan, the way a slice of structs holds its
					// element's: an error in it is reported as itself.
					if sub, err = planForBuilding(fieldType.Elem(), building); err != nil {
						return nil, err
					}
				}
			case reflect.Pointer:
				var err error
				if elemOp, err = pointerOpFor(fieldType); err != nil {
					return nil, fmt.Errorf("colbin: %s.%s: %w",
						typeName(structType), field.Name, err)
				}
				op, sliceType = opPointer, fieldType
			default:
				var err error
				if op, err = opFor(fieldType); err != nil {
					return nil, fmt.Errorf("colbin: %s.%s: %w",
						typeName(structType), field.Name, err)
				}
			}
		}
		if indirect {
			// What the reader allocates when the key is present. A slice of
			// structs and a map hold it already; an array of values does not.
			sliceType = fieldType
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
			typedMap:  typedMap,
			native:    isNative(fieldType),
			indirect:  indirect,
		})
		hashNames = append(hashNames, hashName)
		fieldNames = append(fieldNames, field.Name)
		declared = append(declared, explicitID)
	}
	if len(plan.fields) == 0 {
		return nil, fmt.Errorf("colbin: %s has no encodable field", typeName(structType))
	}
	// The name a field hashes to an id by is the name a reader without the Go
	// type calls it: `cb:"label,3"` names the field as well as numbering it, and
	// an untagged field is called what Go calls it. One name, not two.
	if paged(declared) {
		if err := plan.paginate(structType, hashNames, fieldNames, declared); err != nil {
			return nil, err
		}
	} else {
		if len(plan.fields) > wire.MaxWideFields {
			return nil, fmt.Errorf(
				"colbin: %s has %d encodable fields, and a one-byte key holds %d: "+
					"number them, and ids past %d put the type on pages",
				typeName(structType), len(plan.fields), wire.MaxWideFields, pageFields)
		}
		if err := plan.assignKeys(structType, hashNames, fieldNames, declared); err != nil {
			return nil, err
		}
		plan.names = hashNames
		plan.hasDynamic = anyDynamic(plan.fields)
	}
	plan.envelope = isEnvelope(structType)
	plan.finish()
	plan.reset = compileReset(structType)
	return plan, nil
}

// finish resolves what a plan derives from its fields, once they and their keys
// are final.
func (plan *typePlan) finish() {
	plan.indexKeys()
	plan.isWide = plan.wide()
	plan.canTable = plan.transposable()
	plan.simple = plan.simplePlan()
	plan.sizeHint = plan.planSizeHint()
	for index := range plan.fields {
		if field := &plan.fields[index]; field.op == opStructs && !field.indirect {
			plan.structSlices = append(plan.structSlices, int32(index))
		}
	}
	plan.rows = compileRows(plan)
}

// sizeFor is a starting capacity for encoding record: sizeHint, plus, for each
// slice of structs the record holds, its length times what its first row is
// estimated at. It reads lengths and first rows and nothing else, so it costs a
// few loads rather than the pass over the value an exact size would; where the
// first row is not typical the buffer grows past the estimate as it would have
// anyway.
//
// A row's sizeHint is a bound on a scalar row, two or three times what a row
// of small values or a table's compressed column comes to, so the estimate is
// only where reserve starts: it learns from each encode how far off it is.
func (plan *typePlan) sizeFor(record unsafe.Pointer) int {
	return plan.sizeAt(record, 0)
}

// reserve is the room to make before encoding record, and the estimate it was
// scaled from, which learn wants back.
//
// A message's size is most of what a generated encoder knows in advance and a
// plan does not; but the same type tends to encode to a similar size per row
// every time. So the first encode of a type reserves sizeFor's bound, and
// every later one the bound scaled by what the last encodes came to — the
// buffer made once and close to right, where a bound alone reserved two or
// three times the message, and the collector paid for the rest.
func (plan *typePlan) reserve(record unsafe.Pointer) (need, estimate int) {
	if len(plan.structSlices) == 0 {
		return plan.sizeHint, 0
	}
	estimate = plan.sizeFor(record)
	if ratio := plan.sizeRatio.Load(); ratio != 0 {
		// In 64 bits: a type whose messages ran far past their estimate has a
		// ratio that overflows a 32-bit int times any estimate.
		scaled := min(uint64(estimate)*uint64(ratio)>>sizeRatioShift, math.MaxInt32)
		return int(scaled) + plan.sizeHint, estimate
	}
	return estimate, estimate
}

// sizeRatioShift is the fixed point sizeRatio is in: 1<<sizeRatioShift is a
// message exactly as large as its estimate.
const sizeRatioShift = 12

// learn records how large a message came out against its estimate, with a
// sixteenth to spare. It writes only when the ratio moves by more than that,
// so that encodes of one type on many goroutines are not all storing to the
// same cache line.
func (plan *typePlan) learn(estimate, actual int) {
	if estimate <= 0 {
		return
	}
	ratio := min(uint64(actual+actual>>4)<<sizeRatioShift/uint64(estimate), math.MaxUint32)
	old := uint64(plan.sizeRatio.Load())
	if ratio > old+old>>4 || ratio < old-old>>4 {
		plan.sizeRatio.Store(uint32(max(ratio, 1)))
	}
}

// sizeEstimateDepth is how far down first rows sizeFor looks. It is a bound
// rather than the type's depth because a slice can hold itself: x[0].Rows = x.
const sizeEstimateDepth = 4

func (plan *typePlan) sizeAt(record unsafe.Pointer, depth int) int {
	total := plan.sizeHint
	for _, index := range plan.structSlices {
		field := &plan.fields[index]
		slice := (*sliceHeader)(unsafe.Add(record, field.offset))
		if slice.len == 0 {
			continue
		}
		row := field.sub.sizeHint
		if len(field.sub.structSlices) > 0 && depth < sizeEstimateDepth {
			row = field.sub.sizeAt(slice.data, depth+1)
		}
		total += slice.len * row
	}
	return total
}

// anyDynamic says some field carries a value whose type is not declared — an
// `any`, a slice of them, or a map of them. See typePlan.hasDynamic.
func anyDynamic(fields []planField) bool {
	for _, field := range fields {
		if field.op == opAny || field.op == opAnys || (field.op == opMap && field.valueKind == mapAny) {
			return true
		}
	}
	return false
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
				"colbin: field id %d is on both %s.%s and %s.%s",
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

// A Go int, uint and uintptr are read and written at the platform's width, which
// is what their memory holds. On the wire every integer is a magnitude, so an
// int32 and an int64 holding the same value are the same bytes and a message
// does not depend on where it was written; a 32-bit reader refuses a value its
// int cannot hold, as it would for an int32 field.
var (
	intOp     = byWidth(strconv.IntSize, opInt32, opInt64)
	uintOp    = byWidth(strconv.IntSize, opUint32, opUint64)
	intsOp    = byWidth(strconv.IntSize, opInt32s, opInt64s)
	uintsOp   = byWidth(strconv.IntSize, opUint32s, opUint64s)
	uintptrOp = byWidth(8*int(unsafe.Sizeof(uintptr(0))), opUint32, opUint64)
)

// byWidth is the 32-bit op on a 32-bit platform and the 64-bit one otherwise.
func byWidth(bits int, narrow, wide fieldOp) fieldOp {
	if bits == 32 {
		return narrow
	}
	return wide
}

// isNative says a field holds a platform-width integer, or a slice of or a
// pointer to one. See planField.native.
func isNative(fieldType reflect.Type) bool {
	if kind := fieldType.Kind(); kind == reflect.Slice || kind == reflect.Pointer {
		fieldType = fieldType.Elem()
	}
	switch fieldType.Kind() {
	case reflect.Int, reflect.Uint, reflect.Uintptr:
		return true
	}
	return false
}

// schemaOp is the op a schema section names for one a plan holds: the 64-bit
// form of a platform-width integer, which is what it is on a 64-bit platform.
func schemaOp(op fieldOp, native bool) fieldOp {
	if !native {
		return op
	}
	switch op {
	case opInt32:
		return opInt64
	case opUint32:
		return opUint64
	case opInt32s:
		return opInt64s
	case opUint32s:
		return opUint64s
	}
	return op
}

// hasExportedField says a struct, or the struct a pointer points at, has a field
// another package could set.
func hasExportedField(fieldType reflect.Type) bool {
	if fieldType.Kind() == reflect.Pointer {
		fieldType = fieldType.Elem()
	}
	if fieldType.Kind() != reflect.Struct {
		return false
	}
	for index := range fieldType.NumField() {
		if fieldType.Field(index).IsExported() {
			return true
		}
	}
	return false
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
	case reflect.Int64:
		return opInt64, nil
	case reflect.Int:
		return intOp, nil
	case reflect.Uint8:
		return opUint8, nil
	case reflect.Uint16:
		return opUint16, nil
	case reflect.Uint32:
		return opUint32, nil
	case reflect.Uint64:
		return opUint64, nil
	case reflect.Uint:
		return uintOp, nil
	case reflect.Uintptr:
		return uintptrOp, nil
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
	case reflect.Int64:
		return opInt64s, nil
	case reflect.Int:
		return intsOp, nil
	case reflect.Uint16:
		return opUint16s, nil
	case reflect.Uint32:
		return opUint32s, nil
	case reflect.Uint64:
		return opUint64s, nil
	case reflect.Uint:
		return uintsOp, nil
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

// Append encodes v onto dst, which may be nil. On failure it returns dst as it
// was given.
//
// Int and uint are encoded as magnitudes like every other integer, so a message
// written on one platform reads on another.
func Append(dst []byte, v any) ([]byte, error) {
	value, plan, err := addressable(v)
	if err != nil {
		return dst, err
	}
	return appendRecord(dst, plan, unsafe.Pointer(value.UnsafeAddr()))
}

// appendRecord writes a root descriptor and a record. A simple plan cannot fail —
// everything was resolved at plan time — and takes appendPlan, which has no
// error path at all; a caller on the hot path tests simple itself and saves the
// call.
//
// What can fail is a dynamic value, resolved per value, which can hold a Go type
// with no form on the wire or hold itself; and a value that nests deeper than a
// reader will follow.
func appendRecord(dst []byte, plan *typePlan, record unsafe.Pointer) ([]byte, error) {
	if plan.simple {
		return appendPlan(dst, plan, record), nil
	}
	root := rootStructNarrow
	if plan.isWide {
		root = rootStructWide
	}
	if dictionaryOn.Load() {
		return appendDictRecord(dst, root, plan, record)
	}
	var buf scratch
	out, err := appendRunInto(append(dst, root), plan, record, &buf)
	if err != nil {
		return dst, err
	}
	return out, nil
}

// addressable resolves v to a plan and to a value the plan can read fields out
// of by offset.
//
// A value handed in by interface is not addressable, so one copy into a
// temporary is what makes Marshal(v) work the same as Marshal(&v).
func addressable(v any) (reflect.Value, *typePlan, error) {
	value := reflect.ValueOf(v)
	if !value.IsValid() {
		return reflect.Value{}, nil, fmt.Errorf("colbin: cannot encode nil")
	}
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

// appendPlan writes the root descriptor and then a simple plan's run, at
// whichever key width the plan resolved to. It is the hot path, and a separate
// writer per branch is deliberate: escape analysis is per variable.
func appendPlan(dst []byte, plan *typePlan, record unsafe.Pointer) []byte {
	if plan.isWide {
		writer := wire.Writer8{Buffer: append(dst, rootStructWide)}
		appendScalarsWide(&writer, plan, record)
		return writer.Buffer
	}
	writer := wire.Writer{Buffer: append(dst, rootStructNarrow)}
	appendScalars(&writer, plan, record)
	return writer.Buffer
}

// writePlan is the narrow-key encoder for a plan that nests. The walk itself is
// writeFields (ops_gen.go): every value op inline beside the composite arms, so
// a scalar field costs one switch whether or not its plan nests. A zero-valued
// field writes nothing, which the writer decides. A dynamic value fails there:
// a plan holding one is wide, so that is a plan that disagrees with itself.
//
// It is a separate function from appendWide rather than one with a width flag,
// for the reason wire keeps the two widths in separate types: a width the
// compiler cannot see is a width it cannot fold.
func writePlan(writer *wire.Writer, plan *typePlan, record unsafe.Pointer, buf *scratch) {
	if !buf.enter() {
		return
	}
	defer buf.leave()
	if program := plan.rows; program != nil && !program.wide && useRowPrograms && !Packed5() {
		program.write(writer, record, buf)
		return
	}
	writeFields(writer, plan, record, buf)
}

// errNotValueOp is a switch over value ops meeting one that is not, which only a
// plan that disagrees with itself can cause.
func errNotValueOp(op fieldOp) error {
	return fmt.Errorf("colbin: field type %d cannot be carried at this key width", uint8(op))
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
	record := unsafe.Pointer(value.UnsafeAddr())
	need, estimate := plan.reserve(record)
	out, err := appendRecord(make([]byte, 0, need), plan, record)
	if err != nil {
		return nil, err
	}
	plan.learn(estimate, len(out))
	return out, nil
}

// Unmarshal decodes a colbin message into dst, a non-nil pointer to a
// struct of the same shape.
//
// Every field of dst is set, including the ones the message omitted: an omitted
// key means the value was zero, so the destination is cleared first rather than
// left holding whatever it had. A message that is not one — no root descriptor
// this version writes — leaves dst untouched.
//
// A slice in dst lends its array: a list that fits in its capacity is decoded
// into it, as encoding/json does, so a slice taken from dst before the call sees
// its elements overwritten. A field the message omits is still nil.
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
	section, body, wide, ok := rootParts(data)
	if !ok {
		if isDictRoot(data) {
			value.SetZero()
			return unmarshalDictionaryAs(data, plan, unsafe.Pointer(value.UnsafeAddr()), value.Type())
		}
		return errBadRoot(data, value.Type())
	}
	record := unsafe.Pointer(value.UnsafeAddr())
	if !plan.reset.holds(record) {
		value.SetZero()
		return unmarshalBody(body, section, wide, plan, record, value.Type())
	}
	plan.reset.reset(record)
	err = unmarshalBody(body, section, wide, plan, record, value.Type())
	plan.reset.settle(record)
	return err
}

// unmarshalBody decodes a message body into an already-zeroed record, at the
// width the root declared.
func unmarshalBody(
	body, section []byte, wide bool, plan *typePlan, record unsafe.Pointer, what reflect.Type,
) error {
	var err error
	if wide {
		err = unmarshalWide(body, section, plan, record)
	} else {
		err = unmarshalNarrow(body, plan, record)
	}
	if err != nil {
		return errDecoding(what, err)
	}
	return nil
}

// errDecoding names the type a decode failed in. Out of line, as errBadRoot is.
func errDecoding(what reflect.Type, err error) error {
	return fmt.Errorf("colbin: %s: %w", what, err)
}

// unmarshalNarrow decodes a narrow-key message into an already-zeroed record.
//
// A key the plan does not declare is stepped over, as it is at eight bits: the
// nibble beside a narrow key says how long the field is whatever its type, so a
// type can drop a field, or a newer peer add one, at any id and the two still
// read each other. What the narrow width cannot do is say what an unknown field
// was — that takes a class, which is what the wide descriptor spends its byte on.
func unmarshalNarrow(body []byte, plan *typePlan, record unsafe.Pointer) error {
	reader := wire.NewReader(body)
	if plan.simple {
		readScalars(&reader, plan, record)
		return reader.Err()
	}
	return unmarshalNarrowRun(&reader, body, plan, record)
}

// unmarshalNarrowRun is unmarshalNarrow for a plan that nests. It is out of line
// so the flat path does not pay for its scratch and deferred release, which
// measured 11 ns on a ten-field record that decodes in 28.
func unmarshalNarrowRun(reader *wire.Reader, body []byte, plan *typePlan, record unsafe.Pointer) error {
	buf := scratch{rowsLeft: rowBudget(len(body))}
	buf.strings.Reset(len(body))
	defer buf.release()
	if readsByProgram(plan, false) {
		return readByProgram(body, plan, record, &buf)
	}
	readNarrowRun(reader, plan, record, &buf)
	return reader.Err()
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

// FieldIDs reports the id of every field — the number its tag gives, or the one
// derived from its name, counted from one — for a type the format accepts. A
// reader in another language needs the numbers, and reading them out of the tags
// by hand is how they drift. A field's key on the wire is its id minus one.
func FieldIDs(v any) (map[string]uint16, error) {
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
	return plan.fieldIDs(structType), nil
}

// fieldIDs pairs a plan's fields with the Go names they came from, which the
// plan holds in declaration order. A paged type's fields are spread over its
// pages, so it holds the ids instead.
func (plan *typePlan) fieldIDs(structType reflect.Type) map[string]uint16 {
	names := plan.goNames(structType)
	ids := make(map[string]uint16, len(names))
	for index, name := range names {
		if plan.ids != nil {
			ids[name] = plan.ids[index]
			continue
		}
		ids[name] = uint16(plan.fields[index].key) + 1
	}
	return ids
}

// goNames is the Go name of every field the plan carries, in its order.
func (plan *typePlan) goNames(structType reflect.Type) []string {
	names := make([]string, 0, len(plan.fields))
	for index := range structType.NumField() {
		field := structType.Field(index)
		if !field.IsExported() {
			continue
		}
		if _, _, skip, _ := parseCbTag(field); skip {
			continue
		}
		names = append(names, field.Name)
	}
	return names
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
// Marshal repeats on every call. That per-call work is a real share of the cost
// of a small message. A Codec is safe for concurrent use.
//
//	var chargeCodec = colbin.MustCodec[Charge]()
//
//	buf := make([]byte, 0, 64)
//	for _, charge := range charges {
//	    buf, err = chargeCodec.Append(buf[:0], &charge)
//	    if err != nil { ... }
//	    send(buf)
//	}
type Codec[T any] struct {
	plan *typePlan
	// what names T in an error. Held rather than derived, because reflect.TypeOf
	// on the value boxes it — one allocation per decode for a message that
	// almost never fails.
	what reflect.Type
}

// NewCodec builds the handle for T, which must be a type the format accepts at
// the root: a struct, or a slice or map the envelope carries.
func NewCodec[T any]() (*Codec[T], error) {
	what := reflect.TypeFor[T]()
	if what.Kind() == reflect.Interface || what.Kind() == reflect.Pointer {
		return nil, fmt.Errorf(
			"colbin: a Codec is for the type a message holds, not %s", what)
	}
	plan, err := planForRoot(what)
	if err != nil {
		return nil, err
	}
	return &Codec[T]{plan: plan, what: what}, nil
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

// Append encodes value onto dst, which may be nil. On failure it returns dst as
// it was given.
//
// A flat T — no nesting, no `any` — cannot fail: its shape was resolved when the
// handle was built. One that nests can be given a value that nests deeper than
// a reader follows, and an `any` can hold a Go type the format has no form for,
// or hold itself.
func (codec *Codec[T]) Append(dst []byte, value *T) ([]byte, error) {
	record := unsafe.Pointer(value)
	// Room is made once, up front, the way a generated Marshal sizes its
	// buffer before writing: a dst too small for the estimate would otherwise
	// grow by doubling, copying everything written so far each time.
	out := dst
	if codec.plan.simple {
		if cap(out)-len(out) < codec.plan.sizeHint {
			out = slices.Grow(out, codec.plan.sizeHint)
		}
		return appendPlan(out, codec.plan, record), nil
	}
	need, estimate := codec.plan.reserve(record)
	if cap(out)-len(out) < need {
		out = slices.Grow(out, need)
	}
	out, err := appendRecord(out, codec.plan, record)
	if err != nil {
		return dst, err
	}
	codec.plan.learn(estimate, len(out)-len(dst))
	return out, nil
}

// Encode is Append onto a fresh buffer.
func (codec *Codec[T]) Encode(value *T) ([]byte, error) {
	return codec.Append(nil, value)
}

// Unmarshal decodes a message into value, zeroing it first: a key the message
// omits means the field was zero. A message that is not one leaves value
// untouched. A slice in value lends its array, as in the package Unmarshal.
func (codec *Codec[T]) Unmarshal(data []byte, value *T) error {
	section, body, wide, ok := rootParts(data)
	if !ok {
		if isDictRoot(data) {
			*value = *new(T)
			return unmarshalDictionaryAs(data, codec.plan, unsafe.Pointer(value), codec.what)
		}
		return errBadRoot(data, codec.what)
	}
	reset := codec.plan.reset
	held := reset.holds(unsafe.Pointer(value))
	if held {
		reset.reset(unsafe.Pointer(value))
	} else {
		*value = *new(T)
	}
	// unmarshalBody's dispatch, repeated here to save its frame: a nanosecond of
	// a 28 ns flat decode.
	var err error
	if wide {
		err = unmarshalWide(body, section, codec.plan, unsafe.Pointer(value))
	} else {
		err = unmarshalNarrow(body, codec.plan, unsafe.Pointer(value))
	}
	if held {
		reset.settle(unsafe.Pointer(value))
	}
	if err != nil {
		return errDecoding(codec.what, err)
	}
	return nil
}

// FieldIDs reports the id of every field, as the package function does.
func (codec *Codec[T]) FieldIDs() map[string]uint16 {
	if codec.plan.envelope {
		return map[string]uint16{}
	}
	return codec.plan.fieldIDs(codec.what)
}
