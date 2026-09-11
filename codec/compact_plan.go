package codec

import (
	"fmt"
	"reflect"
	"sync/atomic"
	"unsafe"

	"github.com/ivanjoz/colbin/compact"
)

// The compact-mode struct cache.
//
// compact_mode.go decides whether a type may use compact mode; this file turns
// that answer into the program that runs per record. A field's encoding is fully
// determined by its type -- class, Go kind, width -- and none of that changes
// between calls, so resolving it on every field of every record is work the type
// already knows the answer to. The plan resolves it once: one op per field, in
// declaration order, holding the field's byte offset, its wire id, and a single
// opcode that names the exact load and the exact writer call.
//
// That collapses three nested switches (fType, then goKind, then bitWidth) into
// one, and it removes xunsafe from the path: the offset is in the op, so a
// scalar is a direct pointer cast rather than an accessor call.
//
// It also fuses the passes. The old shape asked each field for its value three
// times -- once to test it for zero, once for the ALL_POSITIVE scan, once to
// write it -- with the kind switch re-walked each time. Encoding now reads a
// field once, and only signed integers are visited twice, by an ALL_POSITIVE
// scan that walks a precomputed list of just those fields and skips entirely
// when a type has none.
//
// # Composites
//
// A nested struct, an array whose elements are not scalars, and a map recurse
// into values of their own, so the plan is a tree rather than a flat list. It
// stays a flat list of ops per struct: a composite op carries an index into the
// plan's subs, where the nested plan or the element descriptor lives, and the
// scalar ops are untouched and keep their direct-cast fast path.

// compactOp is one field's whole compact-mode encoding: where it lives and what
// to do with it.
type compactOp struct {
	offset uintptr // byte offset of the field within the record
	id     uint8   // wire field id
	kind   uint8   // op* below

	// aux is 0 for a field held by value. For a pointer field -- which only
	// omit-empty admits -- it is index+1 into the plan's pointees, and the load
	// at offset is a pointer to dereference rather than the value itself. It
	// rides in the padding the two bytes above already left, so the op stays
	// sixteen bytes and the hot loop pays one predictable branch.
	aux uint8

	// sub is 0 for a scalar. For a composite op it is index+1 into the plan's
	// subs. It rides in the same padding aux does, so adding it cost the op
	// nothing: eight bytes of offset plus four of tags still pads to sixteen.
	sub uint16
}

// compactElem is a value's encoding without a field's framing -- an array
// element, a map key, a map value. It is the recursive unit: the same opcodes,
// minus the offset and wire id that only a struct field has.
type compactElem struct {
	kind uint8
	sub  uint16 // index+1 into the owning plan's subs
}

// compactSub is what a composite opcode needs beyond the opcode itself. Which
// fields are set is decided by the opcode pointing here, so the three forms
// share one struct rather than an interface the hot loop would have to dispatch
// through.
type compactSub struct {
	plan *compactPlan // opStruct: the nested struct's own plan

	elem      compactElem  // opArray: the element
	elemSize  uintptr      // opArray: element stride
	sliceType reflect.Type // opArray: the slice type, to build one on decode

	key, val compactElem  // opMap
	mapType  reflect.Type // opMap: the map type, to build one on decode
}

// Opcodes. The scalar ones name a Go type exactly, because that is what decides
// both the load and which Writer method takes it; the slice ones name the
// element width, because that is what the array codec is instantiated on. The
// three composite ones name a shape instead, and take the rest from their sub.
const (
	opNone uint8 = iota
	opInt8
	opInt16
	opInt32
	opInt64
	opInt
	opUint8
	opUint16
	opUint32
	opUint64
	opUint
	opBool
	opFloat32
	opFloat64
	opString
	opBytes
	opInt8s
	opInt16s
	opInt32s
	opInt64s
	opBools
	opStrs
	opFloat32s
	opFloat64s

	// Composites. opArray is a slice the bulk codecs cannot take -- a slice of
	// structs, of maps, or of slices -- written element by element; a slice of
	// scalars keeps one of the op*s opcodes above and its bulk codec.
	opStruct
	opArray
	opMap
)

// compactPlan is the cached program for one struct type. It is built once, on
// first use, and read without locking thereafter.
type compactPlan struct {
	usable bool // false: this type must use the columnar standard mode
	ops    []compactOp
	keys   compact.KeyWidth
	gen    uint32 // the compactPlanGen this plan was built under

	// subs are the composite ops' descriptors, indexed by op.sub-1 and by
	// compactElem.sub-1. A nested struct's own subs live in its own plan, so an
	// index never crosses a plan boundary.
	subs []compactSub

	// hasComposite reports that some field is a nested struct, a non-scalar
	// array or a map. It is what splits the two mode rules in appendRecords: a
	// flat type takes compact mode outright at one record, while a composite one
	// has both forms built and the smaller kept, because a struct holding a
	// hundred sub-records has a hundred key runs where the columnar form has one
	// column per field.
	hasComposite bool

	// signedReach reports that a signed integer sits inside a composite, where
	// the ALL_POSITIVE pre-scan would have to walk the whole value to find it.
	// See compactAllPositivePlan for what is given up instead.
	signedReach bool

	// unusable is why compact mode cannot carry this type, for the error
	// MarshalForceCompact returns. Empty when usable.
	unusable string

	// signed indexes the ops that ALL_POSITIVE has anything to say about, so a
	// type with no signed integer field skips that pass rather than walking
	// every field to discover it has nothing to do.
	signed []uint8

	// pointees are the types behind the plan's pointer fields, needed to
	// allocate one on decode. Indexed by op.aux-1, and empty for the ordinary
	// type that holds everything by value.
	pointees []reflect.Type

	// size is what the last message of this type measured, used to size the
	// encoder's buffer up front rather than growing into it one append at a
	// time. It is only a hint, so a stale value costs a wasted guess and
	// nothing else.
	size atomic.Uint32

	// byID maps a wire id to its op, as index+1 so that the zero value means
	// "no such field". A decode does one array load per key where it used to
	// hash into a map, which at these record sizes was the largest single cost
	// in the decoder.
	byID [256]uint8
}

// compactPlanFor returns the type's plan, building it on first use. A race
// builds the same plan twice and keeps whichever lands first, since the two are
// identical by construction.
//
// A plan also records the omit-empty setting it was built under, because that
// setting decides whether a nullable field can be carried at all. Flipping the
// flag bumps a generation counter, and a plan from the wrong generation is
// rebuilt rather than reused.
func compactPlanFor(ti *typeInfo) *compactPlan {
	gen := compactPlanGen.Load()
	if pl := ti.cplan.Load(); pl != nil && pl.gen == gen {
		return pl
	}
	pl := buildCompactPlan(ti)
	pl.gen = gen
	ti.cplan.Store(pl)
	return pl
}

// compactPlanGen counts omit-empty changes. Plans are cheap to rebuild and the
// flag is meant to be set once at startup, so a counter beats tracking down
// every cached plan.
var compactPlanGen atomic.Uint32

func invalidateCompactPlans() { compactPlanGen.Add(1) }

func buildCompactPlan(ti *typeInfo) *compactPlan {
	if len(ti.fields) == 0 {
		return &compactPlan{unusable: "it has no encodable fields"}
	}
	pl := &compactPlan{usable: true, keys: compact.Keys4}
	b := compactPlanBuilder{pl: pl}
	// index+1 is stored in a uint8, which holds because a struct may have at
	// most 254 encodable fields.
	pl.ops = make([]compactOp, 0, len(ti.fields))
	for i := range ti.fields {
		fm := &ti.fields[i]
		e, indirect, why := b.field(fm)
		if why != "" { // one field without a compact form disqualifies the type
			return &compactPlan{unusable: fmt.Sprintf("field %s %s", fm.name, why)}
		}
		if fm.id > compact.MaxNarrowKey {
			pl.keys = compact.Keys8
		}
		var aux uint8
		if indirect {
			pl.pointees = append(pl.pointees, fm.pointeeType)
			aux = uint8(len(pl.pointees))
		}
		pl.byID[fm.id] = uint8(len(pl.ops) + 1)
		if isSignedOp(e.kind) {
			pl.signed = append(pl.signed, uint8(len(pl.ops)))
		}
		pl.ops = append(pl.ops, compactOp{offset: fm.offset, id: fm.id, kind: e.kind, aux: aux, sub: e.sub})
	}
	return pl
}

// compactPlanBuilder accumulates one plan's subs while its fields are resolved,
// so a descriptor can be added from any depth of the recursion and still get an
// index into the plan that will hold it.
type compactPlanBuilder struct{ pl *compactPlan }

// add appends a sub and returns its index+1, which is what an op or an elem
// carries: zero then means "no sub", so a scalar needs no sentinel.
func (b *compactPlanBuilder) add(s compactSub) uint16 {
	b.pl.subs = append(b.pl.subs, s)
	return uint16(len(b.pl.subs))
}

// field is the per-field rule, and a non-empty why is what makes a type
// ineligible. The string is the tail of a sentence beginning "field X", so it
// reads as a reason in MarshalForceCompact's error.
//
// A pointer field is admitted only under omit-empty. Compact mode says a field
// is present by naming it, so nil has to mean absent -- and then a pointer to
// the zero value, which is also absent, comes back nil. That is exactly what
// omit-empty promises, and without the flag it would be a silent surprise, so
// the field stays out and the type stays columnar.
func (b *compactPlanBuilder) field(fm *fieldMeta) (e compactElem, indirect bool, why string) {
	if !fm.nullable {
		e, why = b.value(fm)
		return e, false, why
	}
	if !omitEmpty.Load() {
		return e, false, "is a pointer, which compact mode carries only under SetOmitEmpty(true)"
	}
	if fm.elem == nil || fm.pointeeType == nil || fm.cyclic {
		return e, false, "is a pointer to a self-referential type"
	}
	e, why = b.value(fm.elem)
	return e, true, why
}

// value is the rule for a value held by position rather than by field: what sits
// behind a pointer, inside a slice, or in a map. It is also the rule for a field
// held by value, since a field adds only its key and its offset.
func (b *compactPlanBuilder) value(fm *fieldMeta) (compactElem, string) {
	// A self-referential type is refused here rather than in field, because this
	// is every point the recursion can re-enter: the plan is a tree of sub-plans
	// and building one for a type that contains itself does not terminate.
	if fm.cyclic {
		return compactElem{}, "can reach its own type, and a compact plan is a tree"
	}
	// A pointer reached from inside a composite has no key run to be absent
	// from: an element is identified by its position, so there is nowhere to
	// record that it was nil.
	if fm.nullable {
		return compactElem{}, "holds a pointer, which has no presence bit outside a record's key run"
	}
	switch fm.fType {
	case ftInt:
		if k := scalarOp(fm.goKind); k != opNone {
			return compactElem{kind: k}, ""
		}
		return compactElem{}, fmt.Sprintf("has kind %s, which has no compact form", fm.goKind)
	case ftFloat:
		if fm.bitWidth == 32 {
			return compactElem{kind: opFloat32}, ""
		}
		return compactElem{kind: opFloat64}, ""
	case ftString:
		return compactElem{kind: opString}, ""
	case ftBytes:
		return compactElem{kind: opBytes}, ""
	case ftStruct:
		return b.structValue(fm)
	case ftArray:
		return b.arrayValue(fm)
	case ftMap:
		return b.mapValue(fm)
	case ftAny:
		return compactElem{}, "is an interface, whose concrete type is a property of the value and has no tag on the compact wire"
	}
	return compactElem{}, "has an unsupported type"
}

// structValue resolves a nested struct to its own plan. The wire form is another
// key run closed by the same terminator -- compact mode has no columns to embed,
// so nesting reuses the only framing it has.
func (b *compactPlanBuilder) structValue(fm *fieldMeta) (compactElem, string) {
	if fm.sub == nil {
		return compactElem{}, "is a struct whose layout is missing"
	}
	nested := compactPlanFor(fm.sub)
	if !nested.usable {
		return compactElem{}, fmt.Sprintf("is %s, which cannot use compact mode: %s", fm.sub.rtype, nested.unusable)
	}
	// A nested key run spends the containing message's key bits, so the narrow
	// key is a property of the whole message: one untagged nested id widens
	// every key in it.
	if nested.keys == compact.Keys8 {
		b.pl.keys = compact.Keys8
	}
	b.pl.hasComposite = true
	if nested.signedReach || len(nested.signed) > 0 {
		b.pl.signedReach = true
	}
	return compactElem{kind: opStruct, sub: b.add(compactSub{plan: nested})}, ""
}

// arrayValue resolves a slice. A slice of scalars keeps the bulk codecs the
// columnar path uses, which is why an array field costs the same in either mode;
// anything else becomes a count followed by its elements.
func (b *compactPlanBuilder) arrayValue(fm *fieldMeta) (compactElem, string) {
	e := fm.elem
	if e == nil {
		return compactElem{}, "is a slice whose element layout is missing"
	}
	if k := scalarSliceOp(e); k != opNone {
		return compactElem{kind: k}, ""
	}
	// int and uint elements stay out for varint's reason: their width is
	// platform dependent and never reaches the wire, so an []int written on a
	// 64-bit host would decode silently wrong on a 32-bit one. Writing them
	// element by element would sidestep the bulk codec's refusal without
	// sidestepping the hazard.
	if e.fType == ftInt && (e.goKind == reflect.Int || e.goKind == reflect.Uint) {
		return compactElem{}, "is []int or []uint, whose element width is platform dependent and never reaches the wire"
	}
	ev, why := b.value(e)
	if why != "" {
		return compactElem{}, "has a slice element that " + why
	}
	b.pl.hasComposite = true
	return compactElem{kind: opArray, sub: b.add(compactSub{
		elem: ev, elemSize: fm.elemSize, sliceType: fm.sliceType,
	})}, ""
}

// mapValue resolves a map to a count followed by key/value pairs. Keys are
// already restricted to scalars and strings by describeKind; values may be
// anything compact mode can carry, a nested struct included.
func (b *compactPlanBuilder) mapValue(fm *fieldMeta) (compactElem, string) {
	if fm.mapKey == nil || fm.mapVal == nil || fm.mapType == nil {
		return compactElem{}, "is a map whose layout is missing"
	}
	ke, why := b.value(fm.mapKey)
	if why != "" {
		return compactElem{}, "has a map key that " + why
	}
	ve, why := b.value(fm.mapVal)
	if why != "" {
		return compactElem{}, "has a map value that " + why
	}
	b.pl.hasComposite = true
	// A signed key or value goes through Writer.Int, which ALL_POSITIVE governs.
	if isSignedOp(ke.kind) || isSignedOp(ve.kind) {
		b.pl.signedReach = true
	}
	return compactElem{kind: opMap, sub: b.add(compactSub{
		key: ke, val: ve, mapType: fm.mapType,
	})}, ""
}

// scalarSliceOp is the bulk-codec opcode for a slice of scalars, or opNone when
// the element is not one. The opcode names the element width, because that is
// what the array codec is instantiated on.
func scalarSliceOp(e *fieldMeta) uint8 {
	if e.nullable || e.cyclic {
		return opNone
	}
	switch e.fType {
	case ftInt:
		switch e.goKind {
		case reflect.Int, reflect.Uint:
			return opNone // platform-dependent width; see arrayValue
		case reflect.Bool:
			return opBools
		}
		// An unsigned slice rides the same-width signed op, which preserves the
		// bit pattern exactly as codec/integer.go does for a column.
		switch e.bitWidth {
		case 8:
			return opInt8s
		case 16:
			return opInt16s
		case 32:
			return opInt32s
		default:
			return opInt64s
		}
	case ftFloat:
		if e.bitWidth == 32 {
			return opFloat32s
		}
		return opFloat64s
	case ftString:
		return opStrs
	}
	return opNone
}

func scalarOp(k reflect.Kind) uint8 {
	switch k {
	case reflect.Int8:
		return opInt8
	case reflect.Int16:
		return opInt16
	case reflect.Int32:
		return opInt32
	case reflect.Int64:
		return opInt64
	case reflect.Int:
		return opInt
	case reflect.Uint8:
		return opUint8
	case reflect.Uint16:
		return opUint16
	case reflect.Uint32:
		return opUint32
	case reflect.Uint64:
		return opUint64
	case reflect.Uint:
		return opUint
	case reflect.Bool:
		return opBool
	}
	return opNone
}

// isSignedOp reports whether ALL_POSITIVE has anything to say about this op.
// Unsigned values are never zigzagged and floats are not varints, so only the
// signed integers are scanned.
func isSignedOp(k uint8) bool { return k >= opInt8 && k <= opInt }

// compactAllPositivePlan scans only the signed integer fields, which the plan
// listed when it was built. A type with none -- all-unsigned ids, strings,
// floats -- pays nothing and reports true.
func compactAllPositivePlan(pl *compactPlan, ptrs []unsafe.Pointer) bool {
	// A signed integer inside a composite would have to be found by walking the
	// whole value: a second traversal of every nested struct, slice and map, to
	// win one payload bit per integer. Reporting false costs exactly that bit,
	// and appendRecords already builds both forms for a composite type, so a
	// type cannot lose on size for it.
	if pl.signedReach {
		return false
	}
	for _, idx := range pl.signed {
		op := &pl.ops[idx]
		for _, p := range ptrs {
			q := unsafe.Add(p, op.offset)
			if op.aux != 0 {
				if q = *(*unsafe.Pointer)(q); q == nil {
					continue // absent, and an absent field writes no varint
				}
			}
			if signedAt(op.kind, q) < 0 {
				return false
			}
		}
	}
	return true
}

func signedAt(kind uint8, p unsafe.Pointer) int64 {
	switch kind {
	case opInt8:
		return int64(*(*int8)(p))
	case opInt16:
		return int64(*(*int16)(p))
	case opInt32:
		return int64(*(*int32)(p))
	case opInt64:
		return *(*int64)(p)
	case opInt:
		return int64(*(*int)(p))
	}
	return 0
}

// --- encode ------------------------------------------------------------------

// compactWriteRecord writes one record's present fields and closes it. A field
// holding its zero value is skipped: the key run is the presence information.
// Each field is read exactly once, and the read is a direct cast at a known
// offset rather than a kind switch inside an accessor.
//
// It is also how a nested struct is written, since a nested struct is another
// record -- the recursion changes the depth, not the framing.
func compactWriteRecord(w *compact.Writer, pl *compactPlan, p unsafe.Pointer) {
	for i := range pl.ops {
		op := &pl.ops[i]
		q := unsafe.Add(p, op.offset)
		if op.aux != 0 {
			// A nil pointer is an absent field, which is what it already looks
			// like: nothing is written and the key run does not name it.
			if q = *(*unsafe.Pointer)(q); q == nil {
				continue
			}
		}
		switch op.kind {
		case opInt8:
			if v := *(*int8)(q); v != 0 {
				w.Key(op.id)
				w.Int(int64(v))
			}
		case opInt16:
			if v := *(*int16)(q); v != 0 {
				w.Key(op.id)
				w.Int(int64(v))
			}
		case opInt32:
			if v := *(*int32)(q); v != 0 {
				w.Key(op.id)
				w.Int(int64(v))
			}
		case opInt64:
			if v := *(*int64)(q); v != 0 {
				w.Key(op.id)
				w.Int(v)
			}
		case opInt:
			if v := *(*int)(q); v != 0 {
				w.Key(op.id)
				w.Int(int64(v))
			}
		case opUint8:
			if v := *(*uint8)(q); v != 0 {
				w.Key(op.id)
				w.Uint(uint64(v))
			}
		case opUint16:
			if v := *(*uint16)(q); v != 0 {
				w.Key(op.id)
				w.Uint(uint64(v))
			}
		case opUint32:
			if v := *(*uint32)(q); v != 0 {
				w.Key(op.id)
				w.Uint(uint64(v))
			}
		case opUint64:
			if v := *(*uint64)(q); v != 0 {
				w.Key(op.id)
				w.Uint(v)
			}
		case opUint:
			if v := *(*uint)(q); v != 0 {
				w.Key(op.id)
				w.Uint(uint64(v))
			}
		case opBool:
			if v := *(*bool)(q); v {
				w.Key(op.id)
				w.Bool(true)
			}
		case opFloat32:
			// Compared on the bits so that negative zero survives: -0.0 == 0.0
			// is true, and omitting it would decode back as +0.0.
			if *(*uint32)(q) != 0 {
				w.Key(op.id)
				w.Float32(*(*float32)(q))
			}
		case opFloat64:
			if *(*uint64)(q) != 0 {
				w.Key(op.id)
				w.Float64(*(*float64)(q))
			}
		case opString:
			if s := *(*string)(q); len(s) != 0 {
				w.Key(op.id)
				w.Str(s)
			}
		case opBytes:
			if b := *(*[]byte)(q); len(b) != 0 {
				w.Key(op.id)
				w.Bytes(b)
			}
		case opStruct:
			// A nested struct whose every field is zero is omitted entirely, the
			// same rule a zero scalar gets. Testing it costs a walk of the
			// sub-record, which is why only composite fields pay for it.
			sub := &pl.subs[op.sub-1]
			if compactRecordZero(sub.plan, q) {
				continue
			}
			w.Key(op.id)
			compactWriteRecord(w, sub.plan, q)
		case opArray:
			sh := (*sliceHeader)(q)
			if sh.len == 0 {
				continue
			}
			w.Key(op.id)
			compactWriteArray(w, pl, &pl.subs[op.sub-1], sh)
		case opMap:
			sub := &pl.subs[op.sub-1]
			m := reflect.NewAt(sub.mapType, q).Elem()
			if m.Len() == 0 {
				continue
			}
			w.Key(op.id)
			compactWriteMap(w, pl, sub, m)
		default:
			sh := (*sliceHeader)(q)
			if sh.len == 0 {
				continue
			}
			w.Key(op.id)
			compactWriteSlice(w, op.kind, sh)
		}
	}
	w.End()
}

// compactWriteElem writes one value positionally -- an array element, a map key,
// a map value -- where position is the identity and so nothing is omitted.
//
// Its scalar cases are deliberately not shared with compactWriteRecord. That
// path fuses the zero test into the single load it makes of the field, which is
// the whole of its speed; routing it through here would mean loading every field
// once to test it and again to write it.
func compactWriteElem(w *compact.Writer, pl *compactPlan, e compactElem, q unsafe.Pointer) {
	switch e.kind {
	case opInt8:
		w.Int(int64(*(*int8)(q)))
	case opInt16:
		w.Int(int64(*(*int16)(q)))
	case opInt32:
		w.Int(int64(*(*int32)(q)))
	case opInt64:
		w.Int(*(*int64)(q))
	case opInt:
		w.Int(int64(*(*int)(q)))
	case opUint8:
		w.Uint(uint64(*(*uint8)(q)))
	case opUint16:
		w.Uint(uint64(*(*uint16)(q)))
	case opUint32:
		w.Uint(uint64(*(*uint32)(q)))
	case opUint64:
		w.Uint(*(*uint64)(q))
	case opUint:
		w.Uint(uint64(*(*uint)(q)))
	case opBool:
		w.Bool(*(*bool)(q))
	case opFloat32:
		w.Float32(*(*float32)(q))
	case opFloat64:
		w.Float64(*(*float64)(q))
	case opString:
		w.Str(*(*string)(q))
	case opBytes:
		w.Bytes(*(*[]byte)(q))
	case opStruct:
		compactWriteRecord(w, pl.subs[e.sub-1].plan, q)
	case opArray:
		compactWriteArray(w, pl, &pl.subs[e.sub-1], (*sliceHeader)(q))
	case opMap:
		sub := &pl.subs[e.sub-1]
		compactWriteMap(w, pl, sub, reflect.NewAt(sub.mapType, q).Elem())
	default:
		compactWriteSlice(w, e.kind, (*sliceHeader)(q))
	}
}

// compactWriteArray writes a count then each element in turn. A slice of scalars
// never reaches here -- it keeps its bulk codec through compactWriteSlice.
func compactWriteArray(w *compact.Writer, pl *compactPlan, sub *compactSub, sh *sliceHeader) {
	w.Count(sh.len)
	for i := 0; i < sh.len; i++ {
		compactWriteElem(w, pl, sub.elem, unsafe.Add(sh.data, uintptr(i)*sub.elemSize))
	}
}

// compactWriteMap writes a count then the entries as key/value pairs.
//
// Map keys and values are not addressable, so each is copied into a cell the
// pointer-based writers can read from -- two cells for the whole map, reused per
// entry, which is what null_map.go does with backing slices for the same reason.
//
// Iteration order is Go's, so the same map does not produce the same bytes
// twice. The columnar path has always had that property and compact mode
// inherits it rather than paying for a sort per map.
func compactWriteMap(w *compact.Writer, pl *compactPlan, sub *compactSub, m reflect.Value) {
	w.Count(m.Len())
	keyCell, valCell := reflect.New(sub.mapType.Key()), reflect.New(sub.mapType.Elem())
	keyVal, valVal := keyCell.Elem(), valCell.Elem()
	keyPtr, valPtr := keyCell.UnsafePointer(), valCell.UnsafePointer()
	var it reflect.MapIter
	it.Reset(m)
	for it.Next() {
		// SetIterKey/SetIterValue assign the whole cell, so nothing of the
		// previous entry survives; it.Key() would box each into a fresh
		// reflect.Value instead, which is the encoder's largest allocation
		// source on a map-heavy corpus.
		keyVal.SetIterKey(&it)
		valVal.SetIterValue(&it)
		compactWriteElem(w, pl, sub.key, keyPtr)
		compactWriteElem(w, pl, sub.val, valPtr)
	}
}

// compactWriteSlice hands the slice straight to the element codec. The Go slice
// already has the layout the codec wants, so the reinterpretation is free.
func compactWriteSlice(w *compact.Writer, kind uint8, sh *sliceHeader) {
	switch kind {
	case opInt8s:
		compact.PutInts(w, unsafe.Slice((*int8)(sh.data), sh.len))
	case opInt16s:
		compact.PutInts(w, unsafe.Slice((*int16)(sh.data), sh.len))
	case opInt32s:
		compact.PutInts(w, unsafe.Slice((*int32)(sh.data), sh.len))
	case opInt64s:
		compact.PutInts(w, unsafe.Slice((*int64)(sh.data), sh.len))
	case opBools:
		w.Bools(unsafe.Slice((*bool)(sh.data), sh.len))
	case opStrs:
		w.Strs(unsafe.Slice((*string)(sh.data), sh.len))
	case opFloat32s:
		w.Float32s(unsafe.Slice((*float32)(sh.data), sh.len))
	case opFloat64s:
		w.Float64s(unsafe.Slice((*float64)(sh.data), sh.len))
	}
}

// compactRecordZero reports that every field of a record holds its zero value,
// which is what lets a nested struct field be omitted the way a zero scalar is.
// It mirrors compactWriteRecord's presence rule exactly, including for a pointer
// field: a non-nil pointer to the zero value is not written either.
//
// Only nested structs are tested. A root record is written whether or not it is
// empty, because the message's shape says how many records it holds.
func compactRecordZero(pl *compactPlan, p unsafe.Pointer) bool {
	for i := range pl.ops {
		op := &pl.ops[i]
		q := unsafe.Add(p, op.offset)
		if op.aux != 0 {
			if q = *(*unsafe.Pointer)(q); q == nil {
				continue
			}
		}
		if !compactElemZero(pl, compactElem{kind: op.kind, sub: op.sub}, q) {
			return false
		}
	}
	return true
}

func compactElemZero(pl *compactPlan, e compactElem, q unsafe.Pointer) bool {
	switch e.kind {
	case opInt8:
		return *(*int8)(q) == 0
	case opInt16:
		return *(*int16)(q) == 0
	case opInt32:
		return *(*int32)(q) == 0
	case opInt64:
		return *(*int64)(q) == 0
	case opInt:
		return *(*int)(q) == 0
	case opUint8:
		return *(*uint8)(q) == 0
	case opUint16:
		return *(*uint16)(q) == 0
	case opUint32:
		return *(*uint32)(q) == 0
	case opUint64:
		return *(*uint64)(q) == 0
	case opUint:
		return *(*uint)(q) == 0
	case opBool:
		return !*(*bool)(q)
	case opFloat32:
		return *(*uint32)(q) == 0 // on the bits, so -0.0 counts as present
	case opFloat64:
		return *(*uint64)(q) == 0
	case opString:
		return len(*(*string)(q)) == 0
	case opBytes:
		return len(*(*[]byte)(q)) == 0
	case opStruct:
		return compactRecordZero(pl.subs[e.sub-1].plan, q)
	case opMap:
		return reflect.NewAt(pl.subs[e.sub-1].mapType, q).Elem().Len() == 0
	default:
		return (*sliceHeader)(q).len == 0 // every slice form, bulk-coded or not
	}
}

// --- decode ------------------------------------------------------------------

// compactReadRecord fills one record from the reader, stopping at the record's
// terminator. An id the plan does not know is an error rather than a skip: the
// wire holds no type tag, so there is no way to know how far to step over it.
//
// It is also how a nested struct is read, for the reason compactWriteRecord
// gives: a nested struct is another record.
func compactReadRecord(r *compact.Reader, pl *compactPlan, p unsafe.Pointer) error {
	for {
		k := r.Key()
		if err := r.Err(); err != nil {
			return err
		}
		if k == compact.TerminatorKey {
			return nil
		}
		i := pl.byID[k]
		if i == 0 {
			return fmt.Errorf("colbin: unknown field id %d (schema mismatch)", k)
		}
		op := &pl.ops[i-1]
		q := unsafe.Add(p, op.offset)
		if op.aux != 0 {
			// The field is a pointer and the message named it, so it points at
			// something. An absent one is left nil by the zeroing the caller
			// already did.
			pv := reflect.New(pl.pointees[op.aux-1])
			*(*unsafe.Pointer)(q) = pv.UnsafePointer()
			q = pv.UnsafePointer()
		}
		// The scalar loads are inlined here rather than taken from
		// compactReadElem, for the same reason compactWriteRecord keeps its own:
		// this is the path a flat single-record message runs, and routing every
		// field through a switch too large to inline measured 4% slower on it.
		// Only the composites, which no flat type has, pay the call.
		switch op.kind {
		case opInt8:
			*(*int8)(q) = int8(r.Int())
		case opInt16:
			*(*int16)(q) = int16(r.Int())
		case opInt32:
			*(*int32)(q) = int32(r.Int())
		case opInt64:
			*(*int64)(q) = r.Int()
		case opInt:
			*(*int)(q) = int(r.Int())
		case opUint8:
			*(*uint8)(q) = uint8(r.Uint())
		case opUint16:
			*(*uint16)(q) = uint16(r.Uint())
		case opUint32:
			*(*uint32)(q) = uint32(r.Uint())
		case opUint64:
			*(*uint64)(q) = r.Uint()
		case opUint:
			*(*uint)(q) = uint(r.Uint())
		case opBool:
			*(*bool)(q) = r.Bool()
		case opFloat32:
			*(*float32)(q) = r.Float32()
		case opFloat64:
			*(*float64)(q) = r.Float64()
		case opString:
			*(*string)(q) = r.Str()
		case opBytes:
			*(*[]byte)(q) = r.Bytes()
		default:
			if err := compactReadElem(r, pl, compactElem{kind: op.kind, sub: op.sub}, q); err != nil {
				return err
			}
		}
	}
}

// compactReadElem reads one value into q positionally -- an array element, a map
// key, a map value -- and is also where a record field's composite and bulk
// slice forms are read. A record field's scalar forms are inlined in
// compactReadRecord instead; see the note there.
func compactReadElem(r *compact.Reader, pl *compactPlan, e compactElem, q unsafe.Pointer) error {
	switch e.kind {
	case opInt8:
		*(*int8)(q) = int8(r.Int())
	case opInt16:
		*(*int16)(q) = int16(r.Int())
	case opInt32:
		*(*int32)(q) = int32(r.Int())
	case opInt64:
		*(*int64)(q) = r.Int()
	case opInt:
		*(*int)(q) = int(r.Int())
	case opUint8:
		*(*uint8)(q) = uint8(r.Uint())
	case opUint16:
		*(*uint16)(q) = uint16(r.Uint())
	case opUint32:
		*(*uint32)(q) = uint32(r.Uint())
	case opUint64:
		*(*uint64)(q) = r.Uint()
	case opUint:
		*(*uint)(q) = uint(r.Uint())
	case opBool:
		*(*bool)(q) = r.Bool()
	case opFloat32:
		*(*float32)(q) = r.Float32()
	case opFloat64:
		*(*float64)(q) = r.Float64()
	case opString:
		*(*string)(q) = r.Str()
	case opBytes:
		*(*[]byte)(q) = r.Bytes()
	case opStruct:
		// The destination is already zero -- decodeCompact zeroes each record,
		// and MakeSlice/New zero what they build -- so a sub-field the message
		// omits stays zero instead of keeping a stale value.
		return compactReadRecord(r, pl.subs[e.sub-1].plan, q)
	case opArray:
		return compactReadArray(r, pl, &pl.subs[e.sub-1], q)
	case opMap:
		return compactReadMap(r, pl, &pl.subs[e.sub-1], q)
	case opInt8s:
		setSliceAt(q, compact.GetInts[int8](r))
	case opInt16s:
		setSliceAt(q, compact.GetInts[int16](r))
	case opInt32s:
		setSliceAt(q, compact.GetInts[int32](r))
	case opInt64s:
		setSliceAt(q, compact.GetInts[int64](r))
	case opBools:
		setSliceAt(q, r.Bools())
	case opStrs:
		setSliceAt(q, r.Strs())
	case opFloat32s:
		setSliceAt(q, r.Float32s())
	case opFloat64s:
		setSliceAt(q, r.Float64s())
	}
	return nil
}

// compactReadArray reads a count then each element, into a slice built for the
// count the message declared.
func compactReadArray(r *compact.Reader, pl *compactPlan, sub *compactSub, q unsafe.Pointer) error {
	n, ok := r.Count(compactElemMinBits(r, pl, sub.elem))
	if !ok {
		return r.Err()
	}
	dst := reflect.NewAt(sub.sliceType, q).Elem()
	if n == 0 {
		dst.SetZero()
		return nil
	}
	s := reflect.MakeSlice(sub.sliceType, n, n)
	base := s.UnsafePointer()
	for i := range n {
		if err := compactReadElem(r, pl, sub.elem, unsafe.Add(base, uintptr(i)*sub.elemSize)); err != nil {
			return err
		}
	}
	// Assigned through reflect rather than by storing a slice header the way
	// setSliceAt does: these elements can hold pointers, and an unbarriered
	// store of the backing array would hide it from a concurrent mark.
	dst.Set(s)
	return nil
}

// compactReadMap reads a count then that many key/value pairs.
func compactReadMap(r *compact.Reader, pl *compactPlan, sub *compactSub, q unsafe.Pointer) error {
	// An entry is a key and a value, so the floor for one is the two forms' own.
	n, ok := r.Count(compactElemMinBits(r, pl, sub.key) + compactElemMinBits(r, pl, sub.val))
	if !ok {
		return r.Err()
	}
	dst := reflect.NewAt(sub.mapType, q).Elem()
	if n == 0 {
		dst.SetZero()
		return nil
	}
	m := reflect.MakeMapWithSize(sub.mapType, n)
	keyCell, valCell := reflect.New(sub.mapType.Key()), reflect.New(sub.mapType.Elem())
	keyVal, valVal := keyCell.Elem(), valCell.Elem()
	keyPtr, valPtr := keyCell.UnsafePointer(), valCell.UnsafePointer()
	for range n {
		// The cells are reused, and a nested struct read writes only the fields
		// the message named, so the previous entry would otherwise show through.
		keyVal.SetZero()
		valVal.SetZero()
		if err := compactReadElem(r, pl, sub.key, keyPtr); err != nil {
			return err
		}
		if err := compactReadElem(r, pl, sub.val, valPtr); err != nil {
			return err
		}
		m.SetMapIndex(keyVal, valVal)
	}
	dst.Set(m)
	return nil
}

// compactElemMinBits is the fewest bits one element of this form can occupy,
// which is what bounds-checking a count needs: a corrupt count near 2^64 has to
// be rejected before it sizes a make.
func compactElemMinBits(r *compact.Reader, pl *compactPlan, e compactElem) int {
	switch e.kind {
	case opBool:
		return 1
	case opFloat32:
		return 32
	case opFloat64:
		return 64
	case opStruct:
		// An empty record is its terminator alone.
		return r.Keys().Bits()
	}
	// Everything else opens with a varint unit, a packed5 frame or a count --
	// one byte at the very least.
	return 8
}

// setSliceAt stores vals into the slice field at q by writing its header. The
// element types agree in width and pointer-ness by construction, so only the
// header moves; the backing array stays reachable through the field exactly as a
// reflect.MakeSlice backing would.
func setSliceAt[T any](q unsafe.Pointer, vals []T) {
	sh := (*sliceHeader)(q)
	if len(vals) == 0 {
		*sh = sliceHeader{}
		return
	}
	*sh = sliceHeader{data: unsafe.Pointer(&vals[0]), len: len(vals), cap: cap(vals)}
}
