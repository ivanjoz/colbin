package codec

// Decoding a message whose schema section is not the Go type's own.
//
// # The problem
//
// A message is read by key against the Go type's plan, and the keys are the
// type's: a `cb:"3"` tag, or the hash of a name. The browser's encoder has no
// such type. It numbers fields 0, 1, 2 in the order it first sees them, writes
// every integer as an int64 and every fractional column as a float64, and says so
// in the section it puts in front of the message. Unmarshal used to step over that
// section, read key 0 as whatever the Go type numbered 0, and fail on the first
// field whose op differed.
//
// # The fix
//
// The section describes the message exactly, so the message is read through it:
// each section field is matched to a Go field by *name* — the name the section
// writer uses, the json tag first and then the Go name — and the plan that reads
// the body takes its keys and its ops from the section and its offsets from the
// type. The existing readers then run unchanged. What a field needs beyond that
// (a range check, an integer into a float, a pointer that is not one) is a
// conversion, decided here once per field and carried as an opConvert field
// whose conversion foreign_convert.go applies per value.
//
// Matching by name rather than by number is what lets a frontend and a backend
// deployed at different times still agree: a field the Go type does not have is
// stepped over, a Go field the message does not carry stays zero, and a cb tag
// can change without the browser knowing.
//
// # What is refused, and when
//
// Everything is decided when the translation is built, so a refusal is the same
// for every message that carries the section and costs nothing per value:
//
//   - a section field whose op cannot be stored in the Go field (convertible),
//   - a struct with fields on both sides and none matching,
//   - a root that is an envelope on one side only.
//
// The one refusal that depends on a value is an integer that does not fit its
// field, and that names the field's path.
//
// # The cache
//
// A translation costs a walk of the section and of the type, so it is cached by
// (type, section bytes). The section comes from a peer, so the cache is bounded:
// a few entries per type, oldest out, and none for a section too large to be a
// schema anyone wrote. A miss is a rebuild, never a wrong decode.

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"unsafe"

	"github.com/ivanjoz/colbin/wire"
)

const (
	// maxForeignPerType is how many translations of one Go type are kept. A
	// frontend sends one section per type, or a handful across releases; eight is
	// room for every version in flight.
	maxForeignPerType = 8
	// maxCachedSection is the largest section whose translation is kept. A real
	// one is hundreds of bytes; a hostile one is as long as the sender likes, and
	// holding it would hold that many bytes for every entry.
	maxCachedSection = 64 << 10
)

// foreignFor resolves the plan to decode a message through, or nil when the
// ordinary path applies: the message has no section, the section is the Go type's
// own, or the type carries values with no declared type, whose sections are per
// message and not per type.
func foreignFor(rootType reflect.Type, native *typePlan, section []byte) (*foreignPlan, error) {
	if section == nil || reachesDynamic(native) {
		return nil, nil
	}
	if own, err := schemaForRoot(rootType); err == nil && bytes.Equal(section, own.section) {
		return nil, nil
	}
	if cached := foreignCache.lookup(rootType, section); cached != nil {
		return cached, nil
	}
	built, err := buildForeign(rootType, native, section)
	if err != nil {
		return nil, err
	}
	if len(section) <= maxCachedSection {
		foreignCache.store(rootType, section, built)
	}
	return built, nil
}

// unmarshalForeign decodes a body into an already-zeroed record, through a
// translation. It is unmarshalNarrowRun and unmarshalWideRun with the plan's
// conversions in the scratch.
func unmarshalForeign(body, section []byte, wide bool, foreign *foreignPlan, record unsafe.Pointer) error {
	plan := foreign.root
	if plan.isWide != wide {
		return fmt.Errorf("the root descriptor says %s keys and the section says %s",
			keyWidth(wide), keyWidth(plan.isWide))
	}
	buf := scratch{rawSection: section, rowsLeft: rowBudget(len(body)), foreign: foreign}
	buf.strings.Reset(len(body))
	defer buf.release()
	if wide {
		reader := wire.NewReader8(body)
		readRun(&reader, plan, record, &buf)
		return reader.Err()
	}
	reader := wire.NewReader(body)
	readNarrowRun(&reader, plan, record, &buf)
	return reader.Err()
}

func keyWidth(wide bool) string {
	if wide {
		return "eight-bit"
	}
	return "four-bit"
}

// foreignCache is the bounded cache of translations. It is one mutex over a few
// entries per type rather than a sync.Map, because what it must do that a map
// cannot is evict.
var foreignCache foreignStore

type foreignStore struct {
	mu     sync.Mutex
	byType map[reflect.Type]*foreignEntries
}

// foreignEntries is one type's translations, least recently used first.
type foreignEntries struct {
	list []foreignEntry
}

type foreignEntry struct {
	section string
	plan    *foreignPlan
}

func (store *foreignStore) lookup(rootType reflect.Type, section []byte) *foreignPlan {
	store.mu.Lock()
	defer store.mu.Unlock()
	entries := store.byType[rootType]
	if entries == nil {
		return nil
	}
	for index := range entries.list {
		// string(section) in a comparison does not allocate.
		if entries.list[index].section != string(section) {
			continue
		}
		hit := entries.list[index]
		copy(entries.list[index:], entries.list[index+1:])
		entries.list[len(entries.list)-1] = hit
		return hit.plan
	}
	return nil
}

func (store *foreignStore) store(rootType reflect.Type, section []byte, plan *foreignPlan) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.byType == nil {
		store.byType = map[reflect.Type]*foreignEntries{}
	}
	entries := store.byType[rootType]
	if entries == nil {
		entries = &foreignEntries{}
		store.byType[rootType] = entries
	}
	for _, entry := range entries.list {
		if entry.section == string(section) {
			return // another decode built it first
		}
	}
	if len(entries.list) == maxForeignPerType {
		entries.list = append(entries.list[:0], entries.list[1:]...)
	}
	entries.list = append(entries.list, foreignEntry{section: string(section), plan: plan})
}

// size is how many translations are held, for the test that says it is bounded.
func (store *foreignStore) size() (entries, bytes int) {
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, one := range store.byType {
		for _, entry := range one.list {
			entries++
			bytes += len(entry.section)
		}
	}
	return entries, bytes
}

// dynamicReach remembers, per plan, whether it reaches a value with no declared
// type. See reachesDynamic.
var dynamicReach sync.Map // *typePlan -> bool

// reachesDynamic says a plan, or any struct under it, holds an `any`, a slice of
// them or a map of them. The section such a type writes is not a property of the
// type — a dynamic value can name a struct the type never mentions, so the table
// grows with the value — and a message that carries one is read the ordinary way,
// which takes the extra structs from the section itself. See dynamic.go.
func reachesDynamic(plan *typePlan) bool {
	if cached, ok := dynamicReach.Load(plan); ok {
		return cached.(bool)
	}
	found := plan.reachesDynamic(map[*typePlan]bool{})
	dynamicReach.Store(plan, found)
	return found
}

func (plan *typePlan) reachesDynamic(seen map[*typePlan]bool) bool {
	if seen[plan] {
		return false
	}
	seen[plan] = true
	if plan.hasDynamic {
		return true
	}
	for index := range plan.fields {
		if sub := plan.fields[index].sub; sub != nil && sub.reachesDynamic(seen) {
			return true
		}
	}
	return false
}

// foreignKey is one pair the translation visits: a struct of the section against
// the plan of the Go struct it is read into.
type foreignKey struct {
	def, native *typePlan
}

type foreignBuilder struct {
	memo  map[foreignKey]*typePlan
	convs []fieldConv
}

// buildForeign translates a section for one Go type.
func buildForeign(rootType reflect.Type, native *typePlan, section []byte) (*foreignPlan, error) {
	schema, err := ParseSchema(section)
	if err != nil {
		return nil, err
	}
	structType, path := rootType, typeName(rootType)
	if rootType.Kind() != reflect.Struct {
		structType, path = envelopeFor(rootType), rootType.String()
	}
	def := schema.root()
	if def.envelope != native.envelope {
		if native.envelope {
			return nil, fmt.Errorf(
				"the message is a struct and %s is a slice or a map, which a message carries as a one-field envelope", rootType)
		}
		return nil, fmt.Errorf(
			"the message is a slice or a map and %s is a struct", rootType)
	}
	builder := &foreignBuilder{memo: map[foreignKey]*typePlan{}}
	root, err := builder.translate(def, native, structType, path)
	if err != nil {
		return nil, err
	}
	return &foreignPlan{root: root, convs: builder.convs}, nil
}

// goFields is the Go fields a plan carries, in the plan's order — the exported ones
// the cb tag does not drop, as goNames lists them.
func goFields(structType reflect.Type) []reflect.StructField {
	fields := make([]reflect.StructField, 0, structType.NumField())
	for index := range structType.NumField() {
		field := structType.Field(index)
		if !field.IsExported() {
			continue
		}
		if _, _, skip, _ := parseCbTag(field); skip {
			continue
		}
		fields = append(fields, field)
	}
	return fields
}

// translate builds the plan that reads a section's struct into a Go struct: the
// section's keys and key width, the Go type's offsets, and a conversion where the
// two disagree on what a value is.
//
// A pair is memoized before its fields are walked, so a type that holds itself
// terminates the way planForBuilding does.
func (builder *foreignBuilder) translate(
	def, native *typePlan, structType reflect.Type, path string,
) (*typePlan, error) {
	key := foreignKey{def, native}
	if plan, ok := builder.memo[key]; ok {
		return plan, nil
	}
	if def.page || native.page || native.ids != nil {
		return nil, fmt.Errorf("%s: a struct on pages is not read through a foreign section", path)
	}
	fields := goFields(structType)
	if len(fields) != len(native.fields) || len(native.names) != len(native.fields) {
		return nil, fmt.Errorf("%s: its plan does not describe its fields", path)
	}
	byName := make(map[string]int, len(fields))
	for index, name := range native.names {
		byName[name] = index
	}

	// The reset program is the Go type's, not the section's: it says how to clear
	// a record of that layout and which slices lend their arrays, and the decode
	// of a foreign message wants the same reuse a Go one gets.
	plan := &typePlan{isWide: def.isWide, envelope: def.envelope, reset: native.reset}
	builder.memo[key] = plan
	plan.fields = make([]planField, 0, len(def.fields))
	plan.names = make([]string, 0, len(def.fields))
	for index := range def.fields {
		name := def.names[index]
		goIndex, ok := byName[name]
		if !ok {
			// A field the Go type does not have: its key is not in the plan, so the
			// reader steps over it as it does any key it does not know.
			continue
		}
		field, err := builder.translateField(
			&def.fields[index], name, &native.fields[goIndex], fields[goIndex], structType, path)
		if err != nil {
			return nil, err
		}
		plan.fields = append(plan.fields, field)
		plan.names = append(plan.names, name)
	}
	// A struct the message describes with fields and the Go type shares none of is
	// not a struct with fields to spare; it is the wrong type, and decoding it into
	// zeros without a word is how that goes unnoticed.
	if len(def.fields) > 0 && len(plan.fields) == 0 {
		return nil, fmt.Errorf("%s: no field of the message's struct (%s) matches a field of the Go type by name",
			path, quotedNames(def.names))
	}
	plan.indexKeys()
	return plan, nil
}

func quotedNames(names []string) string {
	const most = 6
	shown := names
	if len(shown) > most {
		shown = shown[:most]
	}
	text := strings.Join(shown, ", ")
	if len(names) > most {
		text += ", ..."
	}
	return text
}

// translateField decides one field: what the section calls it and what the Go
// type holds, and so how it is read.
func (builder *foreignBuilder) translateField(
	section *planField, name string, native *planField, goField reflect.StructField,
	parent reflect.Type, path string,
) (planField, error) {
	field := *native
	field.key = section.key
	field.conv = 0
	fieldPath := path
	if !isEnvelope(parent) {
		fieldPath = path + "." + goField.Name
	}
	mismatch := func(why string) error {
		return fmt.Errorf("%s: the message's field %q is %s, and the Go field is %s%s",
			fieldPath, name, describeOp(section), goField.Type, why)
	}

	switch {
	case isStructOp(section.op) && isStructOp(native.op):
		sub, err := builder.translateSub(section, native, goField.Type, fieldPath)
		if err != nil {
			return planField{}, err
		}
		field.sub = sub
		return field, nil
	case section.op == opStructs && (native.op == opStructs || native.op == opStructPtrs):
		sub, err := builder.translateSub(section, native, goField.Type, fieldPath)
		if err != nil {
			return planField{}, err
		}
		field.sub = sub
		return field, nil
	case section.op == opMap && native.op == opMap:
		if section.keyKind != native.keyKind || section.valueKind != native.valueKind ||
			section.listKind != native.listKind {
			return planField{}, mismatch("")
		}
		if section.valueKind == mapStruct || section.valueKind == mapStructs {
			return planField{}, mismatch(": a map of structs is read only through the Go type's own section")
		}
		return field, nil
	case section.op == opAny || section.op == opAnys || native.op == opAny || native.op == opAnys:
		if section.op != native.op {
			return planField{}, mismatch("")
		}
		return field, nil
	}

	srcOp, srcPointer := section.op, false
	if section.op == opPointer {
		srcOp, srcPointer = section.elemOp, true
	}
	dstOp, dstPointer := native.op, false
	if native.op == opPointer {
		dstOp, dstPointer = native.elemOp, true
	}
	if srcOp == dstOp && srcPointer == dstPointer {
		// The same on both sides: the ordinary arm reads it. Asked before the
		// conversion table, which has rows only for what converts — a []string
		// or a []byte is not in it, and was refused against itself.
		return field, nil
	}
	if !convertible(srcOp, dstOp) {
		why := ""
		from, _ := classOfOp(srcOp)
		to, _ := classOfOp(dstOp)
		if isFloatClass(from) && isIntegerClass(to) || isFloatArrayClass(from) && isIntArrayClass(to) {
			why = ": a float is never stored in an integer"
		}
		return planField{}, mismatch(why)
	}
	class, _ := classOfOp(srcOp)
	conv := fieldConv{path: fieldPath, src: srcOp, dst: dstOp, class: class, dstPointer: dstPointer}
	if dstPointer {
		conv.dstElem = native.sliceType.Elem()
		conv.dstElems = reflect.SliceOf(conv.dstElem)
	}
	builder.convs = append(builder.convs, conv)
	field.op = opConvert
	field.conv = uint32(len(builder.convs))
	return field, nil
}

// translateSub is translate for the struct a composite field holds.
func (builder *foreignBuilder) translateSub(
	section, native *planField, goType reflect.Type, fieldPath string,
) (*typePlan, error) {
	subType := goType
	for subType.Kind() == reflect.Pointer || subType.Kind() == reflect.Slice {
		subType = subType.Elem()
	}
	if subType.Kind() != reflect.Struct || section.sub == nil || native.sub == nil {
		return nil, fmt.Errorf("%s: the field does not hold a struct", fieldPath)
	}
	// A struct is named by its own type, not by the way it was reached: the same
	// pair of structs is translated once however many fields hold it, and a type
	// that holds itself is only finite this way. So an error says Product.Qty,
	// which finds the field whichever of the fields holding a Product it was.
	return builder.translate(section.sub, native.sub, subType, typeName(subType))
}

func isStructOp(op fieldOp) bool { return op == opStruct || op == opPointerStruct }

// describeOp says what a section's field is, for an error.
func describeOp(field *planField) string {
	if field.op == opPointer {
		return "a pointer to " + opLabel(field.elemOp)
	}
	return opLabel(field.op)
}
