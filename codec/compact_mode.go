package codec

import (
	"fmt"
	"reflect"
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

// Eligibility, memoised per type. 0 unknown, 1 eligible, 2 not.
const (
	compactUnknown int32 = iota
	compactEligible
	compactIneligible
)

// compactUsable reports whether every field of ti has a compact representation.
// The test is purely structural: a slice's length is not in its type, but that
// no longer matters, because an array field delegates to the same varint and
// packed5 codecs the columnar path uses and therefore costs the same at any
// length.
func compactUsable(ti *typeInfo) bool {
	switch ti.compactMode.Load() {
	case compactEligible:
		return true
	case compactIneligible:
		return false
	}
	ok := len(ti.fields) > 0
	for i := range ti.fields {
		if !compactUsableField(&ti.fields[i]) {
			ok = false
			break
		}
	}
	if ok {
		ti.compactMode.Store(compactEligible)
	} else {
		ti.compactMode.Store(compactIneligible)
	}
	return ok
}

// compactUsableField is the per-field rule.
//
// Nested structs, arrays of structs, maps and interfaces are out. Arrays of
// structs are the meaningful exclusion and the reason is the premise compact
// mode rests on: one record has nothing to amortise per-record framing over, but
// a struct holding a hundred sub-structs contains a hundred records' worth of
// columnar data and the premise fails.
//
// Pointers are out because compact mode omits a zero-valued field, which cannot
// then distinguish nil from a pointer to the zero value the way the columnar
// null bitmap does.
//
// int and uint element types are out for varint's own reason: their width is
// platform dependent and never reaches the wire, so an []int written on a 64-bit
// host would decode silently wrong on a 32-bit one. Scalar int and uint fields
// are fine, since those normalise through readInt64 exactly as they do today.
func compactUsableField(fm *fieldMeta) bool {
	if fm.nullable || fm.cyclic {
		return false
	}
	switch fm.fType {
	case ftInt, ftFloat, ftString, ftBytes:
		return true
	case ftArray:
		e := fm.elem
		if e == nil || e.nullable || e.cyclic {
			return false
		}
		switch e.fType {
		case ftInt:
			return e.goKind != reflect.Int && e.goKind != reflect.Uint
		case ftFloat, ftString:
			return true
		}
	}
	return false
}

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
	w := compact.NewWriter(nil, shape, compactAllPositive(ti, ptrs))
	for _, p := range ptrs {
		for i := range ti.fields {
			fm := &ti.fields[i]
			if compactFieldIsZero(fm, p) {
				continue
			}
			w.Key(fm.id)
			compactWriteField(w, fm, p)
		}
		w.End()
	}
	return w.Done()
}

// compactAllPositive scans every scalar signed integer in the message. Array
// elements are not consulted: they go through varint, which picks zigzag per
// column from that column's own contents, so the message-wide flag has nothing
// to say about them.
func compactAllPositive(ti *typeInfo, ptrs []unsafe.Pointer) bool {
	for _, p := range ptrs {
		for i := range ti.fields {
			fm := &ti.fields[i]
			if fm.fType != ftInt || !isSignedIntKind(fm.goKind) {
				continue
			}
			if readInt64(fm, p) < 0 {
				return false
			}
		}
	}
	return true
}

func isSignedIntKind(k reflect.Kind) bool {
	switch k {
	case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Int:
		return true
	}
	return false
}

// compactFieldIsZero decides whether a field can be left off the wire.
//
// Floats are compared on their bits rather than their value so that negative
// zero survives: -0.0 == 0.0 is true, and omitting it would decode back as +0.0.
func compactFieldIsZero(fm *fieldMeta, p unsafe.Pointer) bool {
	switch fm.fType {
	case ftInt:
		return readInt64(fm, p) == 0
	case ftFloat:
		return floatBits(fm, p) == 0
	case ftString:
		return len(fm.xf.String(p)) == 0
	case ftBytes:
		return len(fm.xf.Bytes(p)) == 0
	case ftArray:
		return sliceAt(fm, p).len == 0
	}
	return false
}

func floatBits(fm *fieldMeta, p unsafe.Pointer) uint64 {
	if fm.bitWidth == 32 {
		return uint64(*(*uint32)(unsafe.Add(p, fm.offset)))
	}
	return *(*uint64)(unsafe.Add(p, fm.offset))
}

func sliceAt(fm *fieldMeta, p unsafe.Pointer) *sliceHeader {
	return (*sliceHeader)(unsafe.Add(p, fm.offset))
}

func compactWriteField(w *compact.Writer, fm *fieldMeta, p unsafe.Pointer) {
	switch fm.fType {
	case ftInt:
		switch {
		case fm.goKind == reflect.Bool:
			w.Bool(fm.xf.Bool(p))
		case isSignedIntKind(fm.goKind):
			w.Int(readInt64(fm, p))
		default:
			w.Uint(uint64(readInt64(fm, p)))
		}
	case ftFloat:
		if fm.bitWidth == 32 {
			w.Float32(float32(readFloat64(fm, p)))
		} else {
			w.Float64(readFloat64(fm, p))
		}
	case ftString:
		w.Str(fm.xf.String(p))
	case ftBytes:
		w.Bytes(fm.xf.Bytes(p))
	case ftArray:
		compactWriteArray(w, fm.elem, sliceAt(fm, p))
	}
}

// compactWriteArray hands the slice straight to the element codec. The Go slice
// already has the layout the codec wants, so the reinterpretation is free: an
// unsigned slice goes through the same-width signed type, which preserves the
// bit pattern exactly as codec/integer.go does for a column.
func compactWriteArray(w *compact.Writer, e *fieldMeta, sh *sliceHeader) {
	switch e.fType {
	case ftInt:
		if e.goKind == reflect.Bool {
			w.Bools(unsafe.Slice((*bool)(sh.data), sh.len))
			return
		}
		switch e.bitWidth {
		case 8:
			compact.PutInts(w, unsafe.Slice((*int8)(sh.data), sh.len))
		case 16:
			compact.PutInts(w, unsafe.Slice((*int16)(sh.data), sh.len))
		case 32:
			compact.PutInts(w, unsafe.Slice((*int32)(sh.data), sh.len))
		default:
			compact.PutInts(w, unsafe.Slice((*int64)(sh.data), sh.len))
		}
	case ftFloat:
		if e.bitWidth == 32 {
			w.Float32s(unsafe.Slice((*float32)(sh.data), sh.len))
		} else {
			w.Float64s(unsafe.Slice((*float64)(sh.data), sh.len))
		}
	case ftString:
		w.Strs(unsafe.Slice((*string)(sh.data), sh.len))
	}
}

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

	for _, p := range ptrs {
		for {
			k := r.Key()
			if err := r.Err(); err != nil {
				return err
			}
			if k == compact.TerminatorKey {
				break
			}
			fm := ti.byID[k]
			if fm == nil {
				return fmt.Errorf("colbin: unknown field id %d (schema mismatch)", k)
			}
			compactReadField(r, fm, p)
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

func compactReadField(r *compact.Reader, fm *fieldMeta, p unsafe.Pointer) {
	switch fm.fType {
	case ftInt:
		switch {
		case fm.goKind == reflect.Bool:
			fm.xf.SetBool(p, r.Bool())
		case isSignedIntKind(fm.goKind):
			setInt64(fm, p, r.Int())
		default:
			setInt64(fm, p, int64(r.Uint()))
		}
	case ftFloat:
		if fm.bitWidth == 32 {
			setFloat64(fm, p, float64(r.Float32()))
		} else {
			setFloat64(fm, p, r.Float64())
		}
	case ftString:
		fm.xf.SetString(p, r.Str())
	case ftBytes:
		fm.xf.SetBytes(p, r.Bytes())
	case ftArray:
		compactReadArray(r, fm, p)
	}
}

// compactReadArray stores the codec's slice into the field by writing its
// header. The backing array comes from make inside the codec and holds no
// pointers for the scalar element types, so the collector keeps it reachable
// through the field exactly as it does for a reflect.MakeSlice backing.
func compactReadArray(r *compact.Reader, fm *fieldMeta, p unsafe.Pointer) {
	e := fm.elem
	switch e.fType {
	case ftInt:
		if e.goKind == reflect.Bool {
			setSlice(fm, p, r.Bools())
			return
		}
		switch e.bitWidth {
		case 8:
			setSlice(fm, p, compact.GetInts[int8](r))
		case 16:
			setSlice(fm, p, compact.GetInts[int16](r))
		case 32:
			setSlice(fm, p, compact.GetInts[int32](r))
		default:
			setSlice(fm, p, compact.GetInts[int64](r))
		}
	case ftFloat:
		if e.bitWidth == 32 {
			setSlice(fm, p, r.Float32s())
		} else {
			setSlice(fm, p, r.Float64s())
		}
	case ftString:
		setSlice(fm, p, r.Strs())
	}
}

// setSlice writes vals into the slice field at p+fm.offset. The element types
// agree in width and pointer-ness by construction, so only the header moves.
func setSlice[T any](fm *fieldMeta, p unsafe.Pointer, vals []T) {
	sh := sliceAt(fm, p)
	if len(vals) == 0 {
		*sh = sliceHeader{}
		return
	}
	*sh = sliceHeader{data: unsafe.Pointer(&vals[0]), len: len(vals), cap: cap(vals)}
}
