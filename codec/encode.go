package codec

import (
	"encoding/binary"
	"fmt"
	"math"
	"reflect"
	"slices"
	"unsafe"

	"github.com/ivanjoz/colbin/packed5"
)

// Marshal encodes v into the colbin format. Structs and slices of structs use
// the columnar records layout; other supported values use a single-element value
// layout. Pointers are dereferenced. Decode with Unmarshal into a compatible Go
// type.
func Marshal(v any) (out []byte, err error) {
	// The any encoder panics with encodeError for dynamic types it can't represent
	// (interface{}-held struct/chan/func, non-string map keys); recover into an error.
	defer recoverEncode(&out, &err)
	rv, err := marshalRoot(v)
	if err != nil {
		return nil, err
	}
	return appendMessage(binaryPrefix(), rv, true)
}

// recoverEncode turns an encodeError panic raised deep in the any encoder into a
// normal error return. Anything else keeps unwinding.
func recoverEncode(out *[]byte, err *error) {
	if r := recover(); r != nil {
		ce, ok := r.(encodeError)
		if !ok {
			panic(r)
		}
		*out, *err = nil, ce.err
	}
}

// marshalRoot resolves the value Marshal was handed: pointers are followed to
// the value they name, which is the type both the schema and the body describe.
func marshalRoot(v any) (reflect.Value, error) {
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return reflect.Value{}, fmt.Errorf("colbin: Marshal nil pointer")
		}
		rv = rv.Elem()
	}
	if !rv.IsValid() {
		return reflect.Value{}, fmt.Errorf("colbin: Marshal nil value")
	}
	return rv, nil
}

// appendMessage writes prefix — the version byte, plus the schema section in
// JSON mode — followed by the message body, which both marshal modes share and
// which is therefore byte for byte the same in either. The prefix is copied in
// after the record layout is known so that the whole message still comes from
// the single sized allocation it always did.
// allowCompact is set by Marshal and clear by MarshalJSON: a compact message has
// no room for a schema section, and at these sizes the schema is the larger cost
// anyway (47 bytes of field names against a 24-byte body), so the self-describing
// form stays columnar.
func appendMessage(prefix []byte, rv reflect.Value, allowCompact bool) (out []byte, err error) {
	// Non-record top-level types (maps, []*struct, scalars, …) use value mode: a
	// single N=1 element column reusing the element machinery. struct / []struct
	// keep the columnar records layout below.
	if !topLevelIsRecords(rv.Type()) {
		out = append(make([]byte, 0, len(prefix)+32), prefix...)
		return encodeValueMode(out, rv), nil
	}

	elemType, recordPtrs, err := recordPointers(rv)
	if err != nil {
		return nil, err
	}
	ti, err := getTypeInfo(elemType)
	if err != nil {
		return nil, err
	}
	return appendRecords(nil, prefix, ti, recordPtrs, rv.Kind() == reflect.Slice, allowCompact), nil
}

// recordPointers resolves the record element type and a pointer to each record,
// which is what both marshal modes and MarshalForceCompact all need before they
// can encode anything.
func recordPointers(rv reflect.Value) (reflect.Type, []unsafe.Pointer, error) {
	switch rv.Kind() {
	case reflect.Slice:
		elemType := rv.Type().Elem()
		n := rv.Len()
		ptrs := make([]unsafe.Pointer, n)
		base := rv.UnsafePointer() // &elem[0]
		size := elemType.Size()
		for i := range n {
			ptrs[i] = unsafe.Add(base, uintptr(i)*size)
		}
		return elemType, ptrs, nil
	case reflect.Struct:
		// Non-addressable value: copy into an addressable location to take its pointer.
		cp := reflect.New(rv.Type())
		cp.Elem().Set(rv)
		return rv.Type(), []unsafe.Pointer{cp.UnsafePointer()}, nil
	default:
		return nil, nil, fmt.Errorf("colbin: Marshal expects struct or slice of structs, got %s", rv.Kind())
	}
}

// appendRecords is the records layout itself, with the element type and one
// pointer per record already resolved. appendMessage reaches it through reflect;
// Codec[T] reaches it with both known at compile time, which is the whole of the
// difference between the two entry points.
//
// dst is appended to, so a caller encoding many messages can hand back the same
// buffer and pay no allocation per message.
func appendRecords(dst, prefix []byte, ti *typeInfo, recordPtrs []unsafe.Pointer, rootIsSlice, allowCompact bool) []byte {
	// Compact mode, when the type allows it and the message is small enough for
	// per-record framing to beat amortised columns. For a flat struct at one
	// record it always wins -- the columnar header and per-column type bytes have
	// nothing to spread over -- so it is taken without building the alternative.
	//
	// Otherwise the answer is not predictable cheaply, so both are built and the
	// smaller kept. That is so at two or three records, where it depends on how
	// many fields are zero, and it is so at any count for a composite type: a
	// struct holding a hundred sub-records spends a hundred key runs on them
	// where the columnar form spends one column per field, and compact mode can
	// come out larger.
	var compactBuf []byte
	if allowCompact && compactUsable(ti) {
		if shape, ok := compactShape(rootIsSlice, len(recordPtrs)); ok {
			if len(recordPtrs) == 1 && !compactPlanFor(ti).hasComposite {
				return appendCompactTo(dst, ti, recordPtrs, shape)
			}
			// The columnar form goes into dst, so the candidate needs a buffer
			// of its own until the two can be compared.
			compactBuf = appendCompactTo(nil, ti, recordPtrs, shape)
		}
	}

	base := len(dst)
	out := growTo(dst, len(prefix)+16+bodySizeHint(ti, len(recordPtrs)))
	out = append(out, prefix...)
	out = binary.AppendUvarint(out, uint64(len(recordPtrs)))
	out = encodeSubTable(out, ti, recordPtrs)
	if n := len(recordPtrs); n > 0 {
		ti.bytesPerRecord.Store(uint32((len(out)-base-len(prefix))/n + 1))
	}
	if compactBuf != nil && len(compactBuf) < len(out)-base {
		return append(out[:base], compactBuf...)
	}
	return out
}

// growTo returns dst with room for n more bytes, allocating only when it does
// not already have it. A fresh buffer is sized from what this type last measured
// per record, which is the difference between one allocation and a dozen
// regrows.
func growTo(dst []byte, n int) []byte {
	if cap(dst)-len(dst) >= n {
		return dst
	}
	out := make([]byte, len(dst), len(dst)+n)
	copy(out, dst)
	return out
}

// bodySizeHint estimates the body of an n-record message of this type, from what
// its last encode measured per record (falling back to the field count, which is
// only ever right for a flat struct of small scalars).
//
// The estimate is capped: records of one type can vary in size without limit — one
// message holding a single huge record would otherwise leave a per-record figure
// that a later thousand-record batch multiplies into a wild allocation. Past the
// cap the buffer just grows the ordinary way, and the regrows matter less the
// larger the payload already is.
func bodySizeHint(ti *typeInfo, n int) int {
	hint := int64(ti.bytesPerRecord.Load())
	if hint == 0 {
		hint = int64(len(ti.fields))
	}
	if need := int64(n) * hint; need < maxSizeHint {
		return int(need)
	}
	return maxSizeHint
}

// maxSizeHint bounds how much output buffer a size estimate may ask for up
// front, which is also the most a stale estimate can waste. Above it the buffer
// grows the ordinary way from 1 MiB, and a payload that large amortises its
// regrows over proportionally more encoding work.
const maxSizeHint = 1 << 20 // 1 MiB

// encodeSubTable writes [colCount] then every field as [id][column]. Used at the
// top level and recursively for nested structs (struct fields / struct elements).
func encodeSubTable(out []byte, ti *typeInfo, ptrs []unsafe.Pointer) []byte {
	out = append(out, byte(len(ti.fields)))
	for i := range ti.fields {
		fm := &ti.fields[i]
		out = append(out, fm.id)
		out = encodeColumn(out, fm, ptrs)
	}
	return out
}

// encodeColumn appends a column for a STRUCT FIELD: a flags byte + payload.
// ptrs point at the containing struct; the value sits at ptr+fm.offset.
func encodeColumn(out []byte, fm *fieldMeta, ptrs []unsafe.Pointer) []byte {
	if fm.nullable { // pointer field: null wrapper + dense value column
		return appendNullableColumn(out, fm.elem, offsetPtrs(ptrs, fm.offset))
	}
	switch fm.fType {
	case ftInt:
		buf := getI64(len(ptrs))
		for i, p := range ptrs {
			(*buf)[i] = readInt64(fm, p)
		}
		out = appendIntColumn(out, *buf, fm.bitWidth)
		putI64(buf)
		return out
	case ftFloat:
		buf := getF64(len(ptrs))
		for i, p := range ptrs {
			(*buf)[i] = readFloat64(fm, p)
		}
		out = appendFloatColumn(out, *buf, fm.bitWidth)
		putF64(buf)
		return out
	case ftString:
		return appendStringColumn(out, ptrs, fm.offset)
	case ftBytes:
		out = append(out, ftBytes)
		buf := getBlobs(len(ptrs))
		for i, p := range ptrs {
			(*buf)[i] = fm.xf.Bytes(p)
		}
		out = appendBlobColumn(out, *buf)
		putBlobs(buf)
		return out
	case ftStruct:
		out = append(out, ftStruct)
		childPtrs := make([]unsafe.Pointer, len(ptrs))
		for i, p := range ptrs {
			childPtrs[i] = unsafe.Add(p, fm.offset) // nested struct sits inline
		}
		return encodeSubTable(out, fm.sub, childPtrs)
	case ftArray:
		out = append(out, ftArray)
		shPtrs := make([]unsafe.Pointer, len(ptrs))
		for i, p := range ptrs {
			shPtrs[i] = unsafe.Add(p, fm.offset) // -> slice header of this record
		}
		return encodeArrayBody(out, fm.elem, fm.elemSize, shPtrs)
	case ftMap:
		out = append(out, ftMap)
		return encodeMapColumn(out, fm, offsetPtrs(ptrs, fm.offset))
	case ftAny:
		out = append(out, ftAny)
		return encodeAnyColumn(out, fm, offsetPtrs(ptrs, fm.offset))
	}
	return out
}

// offsetPtrs returns ptr+offset for each pointer (locate a field within its struct).
func offsetPtrs(ptrs []unsafe.Pointer, offset uintptr) []unsafe.Pointer {
	out := make([]unsafe.Pointer, len(ptrs))
	for i, p := range ptrs {
		out[i] = unsafe.Add(p, offset)
	}
	return out
}

// encodeArrayBody writes an array column: a per-record length sub-column, then
// the flattened element values as one nested element column. shPtrs point at the
// slice headers (one per record).
func encodeArrayBody(out []byte, elem *fieldMeta, elemSize uintptr, shPtrs []unsafe.Pointer) []byte {
	lenBuf := getI64(len(shPtrs))
	total := 0
	for i, sp := range shPtrs {
		sh := (*sliceHeader)(sp)
		(*lenBuf)[i] = int64(sh.len)
		total += sh.len
	}
	out = appendIntColumn(out, *lenBuf, 64)
	putI64(lenBuf)
	if elideEmpty(elem, total) {
		return out
	}

	elemPtrs := make([]unsafe.Pointer, 0, total) // value pointer of every element, flattened
	for _, sp := range shPtrs {
		sh := (*sliceHeader)(sp)
		for j := 0; j < sh.len; j++ {
			elemPtrs = append(elemPtrs, unsafe.Add(sh.data, uintptr(j)*elemSize))
		}
	}
	return encodeElemColumn(out, elem, elemPtrs)
}

// encodeElemColumn appends a column for ARRAY ELEMENTS: ptrs point directly at
// each value (no struct offset). Scalars use direct casts; struct/array elements
// recurse.
func encodeElemColumn(out []byte, elem *fieldMeta, ptrs []unsafe.Pointer) []byte {
	if elem.nullable { // ptrs point at *T slots (e.g. []*T or map[K]*V)
		return appendNullableColumn(out, elem.elem, ptrs)
	}
	switch elem.fType {
	case ftInt:
		buf := getI64(len(ptrs))
		for i, p := range ptrs {
			(*buf)[i] = readInt64At(elem.goKind, p)
		}
		out = appendIntColumn(out, *buf, elem.bitWidth)
		putI64(buf)
		return out
	case ftFloat:
		buf := getF64(len(ptrs))
		for i, p := range ptrs {
			(*buf)[i] = readFloat64At(elem.goKind, p)
		}
		out = appendFloatColumn(out, *buf, elem.bitWidth)
		putF64(buf)
		return out
	case ftString:
		return appendStringColumn(out, ptrs, 0)
	case ftBytes:
		out = append(out, ftBytes)
		buf := getBlobs(len(ptrs))
		for i, p := range ptrs {
			(*buf)[i] = *(*[]byte)(p)
		}
		out = appendBlobColumn(out, *buf)
		putBlobs(buf)
		return out
	case ftStruct:
		out = append(out, ftStruct)
		return encodeSubTable(out, elem.sub, ptrs)
	case ftArray:
		out = append(out, ftArray)
		return encodeArrayBody(out, elem.elem, elem.elemSize, ptrs) // ptrs already at slice headers
	case ftMap:
		out = append(out, ftMap)
		return encodeMapColumn(out, elem, ptrs) // ptrs already at map slots
	case ftAny:
		out = append(out, ftAny)
		return encodeAnyColumn(out, elem, ptrs) // ptrs already at interface slots
	}
	return out
}

// appendStringColumn writes consecutive packed5 frames, or -- when omit-empty is
// on and every string is "" -- the type byte alone. An empty frame is one byte,
// so an untouched string field otherwise costs a byte per record to say nothing.
// The strings live at p+offset, which is the struct field offset for a record
// column and zero for an array element column; taking the offset here keeps both
// callers from having to materialise a second pointer slice.
func appendStringColumn(out []byte, ptrs []unsafe.Pointer, offset uintptr) []byte {
	if omitEmpty.Load() && allEmptyStrings(ptrs, offset) {
		return append(out, ftString|emptyColumnBit)
	}
	out = append(out, ftString)
	for _, p := range ptrs {
		out = packed5.Append(out, *(*string)(unsafe.Add(p, offset)))
	}
	return out
}

// appendFloatColumn stores raw IEEE-754 values (no delta) straight onto out.
// Empty (all-zero) columns carry only the flags byte. The precision bit records
// 32 vs 64 (the decoder also knows from the Go type).
func appendFloatColumn(out []byte, vals []float64, width uint8) []byte {
	empty := allZeroFloat64s(vals)
	var prec uint8
	if width == 64 {
		prec = 1
	}
	out = append(out, ftFloat|prec<<4|boolBit(empty, 7))
	if empty {
		return out
	}
	// The column is a fixed width times a known count, so it is sized once here
	// rather than grown underneath the append loop. Hoisting the width test out
	// of the loop costs nothing and leaves each branch a straight copy.
	out = slices.Grow(out, len(vals)*int(width)/8)
	if width == 64 {
		for _, v := range vals {
			out = binary.LittleEndian.AppendUint64(out, math.Float64bits(v))
		}
	} else {
		for _, v := range vals {
			out = binary.LittleEndian.AppendUint32(out, math.Float32bits(float32(v)))
		}
	}
	return out
}

// appendBlobColumn writes a varint length sub-column then concatenated bytes.
func appendBlobColumn(out []byte, blobs [][]byte) []byte {
	lenBuf := getI64(len(blobs))
	for i, b := range blobs {
		(*lenBuf)[i] = int64(len(b))
	}
	out = appendIntColumn(out, *lenBuf, 64)
	putI64(lenBuf)
	for _, b := range blobs {
		out = append(out, b...)
	}
	return out
}

// encodeValueMode encodes a single top-level value (map, []*struct, scalar, …) as
// one element column with N=1. The value is copied into an addressable cell so the
// element encoders can take its pointer. describeType errors surface as encodeError
// (recovered by Marshal), matching the any encoder's panic contract.
func encodeValueMode(out []byte, rv reflect.Value) []byte {
	fm, err := describeType(rv.Type())
	if err != nil {
		panic(encodeError{err})
	}
	cell := reflect.New(rv.Type()) // addressable copy to take &value
	cell.Elem().Set(rv)
	return encodeElemColumn(out, &fm, []unsafe.Pointer{cell.UnsafePointer()})
}

// boolBit returns 1<<shift if set, else 0 — for packing flag bits.
func boolBit(set bool, shift uint8) byte {
	if set {
		return 1 << shift
	}
	return 0
}
