package codec

import (
	"fmt"
	"reflect"
	"sync"
	"unsafe"

	"github.com/ivanjoz/colbin/compact"
)

// Compact mode wiring. The compact package owns the wire format; everything here
// is the bridge between it and a Go type: deciding whether a type can use it,
// pre-scanning for ALL_POSITIVE, and moving values through the xunsafe accessors
// the columnar path already uses.
//
// Marshal may choose compact mode; MarshalJSON never does, because a compact
// message has no room for a schema section and the schema is the larger cost at
// these sizes anyway. Unmarshal accepts either, discriminating on bit 0 of byte
// 0, which is why every standard version byte is even.

// compactUsable reports whether every field of ti has a compact representation,
// which is the same question as whether the type's plan has an op for every
// field. compact_plan.go owns that per-field rule and the memo behind it.
//
// The test is purely structural: a slice's length is not in its type, but that
// no longer matters, because an array field delegates to the same varint and
// packed5 codecs the columnar path uses and therefore costs the same at any
// length.
//
// Nested structs, arrays of structs, maps and interfaces are out. Arrays of
// structs are the meaningful exclusion and the reason is the premise compact
// mode rests on: one record has nothing to amortise per-record framing over, but
// a struct holding a hundred sub-structs contains a hundred records' worth of
// columnar data and the premise fails.
//
// Pointers are out by default, because compact mode omits a zero-valued field
// and so cannot distinguish nil from a pointer to the zero value the way the
// columnar null bitmap does. Under omit-empty that distinction is one the caller
// has already given up, and the field is admitted -- see compactOpFor.
func compactUsable(ti *typeInfo) bool { return compactPlanFor(ti).usable }

// compactKeys is the key width this type's ids allow. A struct that tags its
// fields `cb:"1"`, `cb:"2"` and so on keeps every id under 15 and pays four bits
// per key instead of eight; one hashed id, which lands anywhere in 0..254, puts
// the whole type back on Keys8.
func compactKeys(ti *typeInfo) compact.KeyWidth { return compactPlanFor(ti).keys }

// compactShape maps a root kind and record count onto the header's shape bits. A
// lone struct and an array of one differ only in how a JSON reader renders them,
// which the binary path takes from the destination type.
func compactShape(rootIsSlice bool, n int) (compact.Shape, bool) {
	if n < 1 || n > compact.MaxRecords {
		return 0, false
	}
	if !rootIsSlice {
		return compact.ShapeStruct, n == 1
	}
	return compact.Shape(n), true
}

// --- encode ------------------------------------------------------------------

// appendCompact writes the whole message. Fields holding their zero value are
// omitted: the key run is the presence information, so an absent field costs
// nothing beyond the terminator the record already owes.
func appendCompact(ti *typeInfo, ptrs []unsafe.Pointer, shape compact.Shape) []byte {
	return appendCompactTo(nil, ti, ptrs, shape)
}

// appendCompactTo appends the message to dst, taking its writer from a pool. The
// writer carries the packed5 and varint scratch buffers between messages, and a
// caller that hands back its own dst each time -- Codec[T].Append does -- then
// encodes a record with no allocation at all.
func appendCompactTo(dst []byte, ti *typeInfo, ptrs []unsafe.Pointer, shape compact.Shape) []byte {
	pl := compactPlanFor(ti)
	w := compactWriterPool.Get().(*compact.Writer)
	w.Reset(growTo(dst, int(pl.size.Load())), shape, compactAllPositivePlan(pl, ptrs), pl.keys)
	for _, p := range ptrs {
		compactWriteRecord(w, pl, p)
	}
	out := w.Done()
	pl.size.Store(uint32(len(out) - len(dst) + 1))
	// Drop the reference to the caller's buffer before the writer goes back:
	// a pooled writer must not keep a message alive.
	w.Reset(nil, shape, false, compact.Keys8)
	compactWriterPool.Put(w)
	return out
}

var compactWriterPool = sync.Pool{New: func() any { return new(compact.Writer) }}

// --- decode ------------------------------------------------------------------

// decodeCompact reads a compact message into target, which the caller has already
// walked down to a concrete slice or struct.
func decodeCompact(data []byte, target reflect.Value) error {
	r, err := compact.NewReader(data)
	if err != nil {
		return err
	}
	n := r.Records()

	var ti *typeInfo
	var ptrs []unsafe.Pointer
	var backing reflect.Value // kept alive so the backing array survives

	switch target.Kind() {
	case reflect.Slice:
		elemType := target.Type().Elem()
		if elemType.Kind() != reflect.Struct {
			return fmt.Errorf("colbin: compact message needs a slice of structs, got %s", target.Type())
		}
		if ti, err = getTypeInfo(elemType); err != nil {
			return err
		}
		backing = reflect.MakeSlice(target.Type(), n, n)
		base := backing.UnsafePointer()
		size := elemType.Size()
		ptrs = make([]unsafe.Pointer, n)
		for i := range n {
			ptrs[i] = unsafe.Add(base, uintptr(i)*size)
		}
	case reflect.Struct:
		if n != 1 {
			return fmt.Errorf("colbin: compact message has %d records, cannot decode into a single struct", n)
		}
		if ti, err = getTypeInfo(target.Type()); err != nil {
			return err
		}
		// Omitted fields are never written, so anything already in the
		// destination would survive as a stale value.
		target.SetZero()
		ptrs = []unsafe.Pointer{target.Addr().UnsafePointer()}
	default:
		return fmt.Errorf("colbin: compact target must be *slice or *struct, got %s", target.Kind())
	}

	pl := compactPlanFor(ti)
	for _, p := range ptrs {
		if err := compactReadRecord(r, pl, p); err != nil {
			return err
		}
	}
	if err := r.Err(); err != nil {
		return err
	}
	if target.Kind() == reflect.Slice {
		target.Set(backing)
	}
	return nil
}
