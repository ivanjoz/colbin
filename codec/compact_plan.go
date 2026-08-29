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
}

// Opcodes. The scalar ones name a Go type exactly, because that is what decides
// both the load and which Writer method takes it; the slice ones name the
// element width, because that is what the array codec is instantiated on.
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
)

// compactPlan is the cached program for one struct type. It is built once, on
// first use, and read without locking thereafter.
type compactPlan struct {
	usable bool // false: this type must use the columnar standard mode
	ops    []compactOp
	keys   compact.KeyWidth
	gen    uint32 // the compactPlanGen this plan was built under

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
	pl := &compactPlan{usable: len(ti.fields) > 0, keys: compact.Keys4}
	if !pl.usable {
		return pl
	}
	// index+1 is stored in a uint8, which holds because a struct may have at
	// most 254 encodable fields.
	pl.ops = make([]compactOp, 0, len(ti.fields))
	for i := range ti.fields {
		fm := &ti.fields[i]
		kind, indirect := compactOpFor(fm)
		if kind == opNone {
			return &compactPlan{} // one field without a compact form disqualifies the type
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
		if isSignedOp(kind) {
			pl.signed = append(pl.signed, uint8(len(pl.ops)))
		}
		pl.ops = append(pl.ops, compactOp{offset: fm.offset, id: fm.id, kind: kind, aux: aux})
	}
	return pl
}

// compactOpFor is the per-field rule, and opNone is what makes a type
// ineligible. compactUsable in compact_mode.go documents why each exclusion is
// there; this is where the exclusions actually live.
//
// A pointer field is admitted only under omit-empty. Compact mode says a field
// is present by naming it, so nil has to mean absent -- and then a pointer to
// the zero value, which is also absent, comes back nil. That is exactly what
// omit-empty promises, and without the flag it would be a silent surprise, so
// the field stays out and the type stays columnar.
func compactOpFor(fm *fieldMeta) (kind uint8, indirect bool) {
	if fm.cyclic {
		return opNone, false
	}
	if fm.nullable {
		e := fm.elem
		if !omitEmpty.Load() || e == nil || e.nullable || e.cyclic || fm.pointeeType == nil {
			return opNone, false
		}
		if k := compactValueOp(e); k != opNone {
			return k, true
		}
		return opNone, false
	}
	return compactValueOp(fm), false
}

// compactValueOp is the rule for a field held by value, which is also the rule
// for what sits behind a pointer.
func compactValueOp(fm *fieldMeta) uint8 {
	switch fm.fType {
	case ftInt:
		return scalarOp(fm.goKind)
	case ftFloat:
		if fm.bitWidth == 32 {
			return opFloat32
		}
		return opFloat64
	case ftString:
		return opString
	case ftBytes:
		return opBytes
	case ftArray:
		e := fm.elem
		if e == nil || e.nullable || e.cyclic {
			return opNone
		}
		switch e.fType {
		case ftInt:
			// int and uint elements are out for varint's reason: their width is
			// platform dependent and never reaches the wire. An unsigned slice
			// rides the same-width signed op, which preserves the bit pattern
			// exactly as codec/integer.go does for a column.
			switch e.goKind {
			case reflect.Int, reflect.Uint:
				return opNone
			case reflect.Bool:
				return opBools
			}
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

// --- decode ------------------------------------------------------------------

// compactReadRecord fills one record from the reader, stopping at the record's
// terminator. An id the plan does not know is an error rather than a skip: the
// wire holds no type tag, so there is no way to know how far to step over it.
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
	}
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
