package codec

import (
	"encoding/binary"
	"fmt"
	"math"
	"reflect"
	"unsafe"

	"github.com/ivanjoz/colbin/packed5"
)

// decoder walks the byte stream with an explicit cursor; each column computes
// its own byte span so the cursor can advance to the next column.
type decoder struct {
	data []byte
	pos  int

	// jsonSafe is set only by the schema-driven decoders (see schema_decode.go):
	// it replaces values JSON has no form for. The typed path never reads it.
	jsonSafe bool
}

// Unmarshal decodes a colbin message into dst, which must be a non-nil pointer
// to a compatible Go value. A records message may target a slice of structs, or
// a single struct when the message contains exactly one record.
func Unmarshal(data []byte, dst any) error {
	rv := reflect.ValueOf(dst)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return fmt.Errorf("colbin: Unmarshal needs a non-nil pointer")
	}
	dec := &decoder{data: data}
	switch v := dec.readByte(); v {
	case formatVersion:
	case jsonFormatVersion:
		// A self-describing message: the body underneath is identical, so the
		// schema section is simply stepped over when the Go type is known.
		dec.pos += int(dec.readUvarint())
	default:
		return fmt.Errorf("colbin: bad version byte 0x%02x", v)
	}

	// Walk (and allocate) the destination pointer chain so we decode into the
	// concrete value: e.g. **ContentFields -> alloc *ContentFields -> struct.
	target := rv.Elem()
	for target.Kind() == reflect.Ptr {
		if target.IsNil() {
			target.Set(reflect.New(target.Type().Elem()))
		}
		target = target.Elem()
	}

	// Non-record types use value mode: a single N=1 element column, no record count.
	if !topLevelIsRecords(target.Type()) {
		t := target.Type()
		fm, err := describeType(t)
		if err != nil {
			return err
		}
		return dec.decodeElemColumn(&fm, t, t.Size(), 1,
			[]unsafe.Pointer{target.Addr().UnsafePointer()})
	}

	n64, m := binary.Uvarint(dec.data[dec.pos:])
	if m <= 0 {
		return fmt.Errorf("colbin: bad record count")
	}
	dec.pos += m
	n := int(n64)

	var elemType reflect.Type
	var recordPtrs []unsafe.Pointer
	var backing reflect.Value // kept alive so the backing array survives

	switch target.Kind() {
	case reflect.Slice:
		elemType = target.Type().Elem()
		backing = reflect.MakeSlice(target.Type(), n, n)
		base := backing.UnsafePointer()
		size := elemType.Size()
		recordPtrs = make([]unsafe.Pointer, n)
		for i := range n {
			recordPtrs[i] = unsafe.Add(base, uintptr(i)*size)
		}
	case reflect.Struct:
		if n != 1 {
			return fmt.Errorf("colbin: message has %d records, cannot decode into a single struct", n)
		}
		elemType = target.Type()
		recordPtrs = []unsafe.Pointer{target.Addr().UnsafePointer()}
	default:
		return fmt.Errorf("colbin: Unmarshal target must be *slice or *struct, got %s", target.Kind())
	}

	ti, err := getTypeInfo(elemType)
	if err != nil {
		return err
	}
	if err := dec.decodeSubTable(ti, n, recordPtrs); err != nil {
		return err
	}
	if target.Kind() == reflect.Slice {
		target.Set(backing)
	}
	return nil
}

// decodeSubTable reads [colCount] then each [id][column] into the given records.
func (dec *decoder) decodeSubTable(ti *typeInfo, n int, ptrs []unsafe.Pointer) error {
	colCount := int(dec.readByte())
	for range colCount {
		id := dec.readByte()
		fm := ti.byID[id]
		if fm == nil {
			return fmt.Errorf("colbin: unknown field id %d (schema mismatch)", id)
		}
		if err := dec.decodeColumn(fm, n, ptrs); err != nil {
			return err
		}
	}
	return nil
}

func (dec *decoder) readByte() byte {
	b := dec.data[dec.pos]
	dec.pos++
	return b
}

// decodeColumn reads one column into STRUCT FIELDS (value at ptr+fm.offset).
func (dec *decoder) decodeColumn(fm *fieldMeta, n int, ptrs []unsafe.Pointer) error {
	if fm.nullable {
		return dec.decodeNullableColumn(fm.elem, fm.pointeeType, n, offsetPtrs(ptrs, fm.offset))
	}
	switch fm.fType {
	case ftInt:
		vals, err := dec.readIntColumn(n, fm.bitWidth)
		if err != nil {
			return err
		}
		for i, p := range ptrs {
			setInt64(fm, p, vals[i])
		}
	case ftFloat:
		vals := dec.readFloatColumn(n)
		for i, p := range ptrs {
			setFloat64(fm, p, vals[i])
		}
	case ftString:
		dec.readByte() // top flags byte (carries only the type)
		if err := dec.readStringColumn(len(ptrs), func(i int, s string) {
			fm.xf.SetString(ptrs[i], s)
		}); err != nil {
			return err
		}
	case ftBytes:
		dec.readByte() // top flags byte (carries only the type)
		blobs, err := dec.readBlobs(n)
		if err != nil {
			return err
		}
		for i, p := range ptrs {
			fm.xf.SetBytes(p, cloneBytes(blobs[i]))
		}
	case ftStruct:
		dec.readByte() // flags (ftStruct)
		childPtrs := make([]unsafe.Pointer, len(ptrs))
		for i, p := range ptrs {
			childPtrs[i] = unsafe.Add(p, fm.offset)
		}
		return dec.decodeSubTable(fm.sub, n, childPtrs)
	case ftArray:
		dec.readByte() // flags (ftArray)
		shPtrs := make([]unsafe.Pointer, len(ptrs))
		for i, p := range ptrs {
			shPtrs[i] = unsafe.Add(p, fm.offset)
		}
		return dec.decodeArrayBody(fm.elem, fm.sliceType, fm.elemSize, shPtrs)
	case ftMap:
		return dec.decodeMapColumn(fm, n, offsetPtrs(ptrs, fm.offset))
	case ftAny:
		return dec.decodeAnyColumn(fm, n, offsetPtrs(ptrs, fm.offset))
	}
	return nil
}

// decodeArrayBody reads the length sub-column, allocates each record's slice,
// then decodes the flattened element column into the slices' backing arrays.
// shPtrs point at the slice headers to populate (one per record).
func (dec *decoder) decodeArrayBody(elem *fieldMeta, sliceType reflect.Type, elemSize uintptr, shPtrs []unsafe.Pointer) error {
	lengths, err := dec.readIntColumn(len(shPtrs), 64)
	if err != nil {
		return err
	}
	total := 0
	elemPtrs := make([]unsafe.Pointer, 0)
	for i, sp := range shPtrs {
		l := int(lengths[i])
		total += l
		if l == 0 {
			continue // leave the slice field as nil (matches Go zero value)
		}
		sv := reflect.MakeSlice(sliceType, l, l)
		// Store the slice header into the field; GC keeps the backing array alive
		// because the field is typed as a slice.
		*(*sliceHeader)(sp) = sliceHeader{data: sv.UnsafePointer(), len: l, cap: l}
		for j := range l {
			elemPtrs = append(elemPtrs, unsafe.Add(sv.UnsafePointer(), uintptr(j)*elemSize))
		}
	}
	if elideEmpty(elem, total) {
		return nil // the encoder wrote no element column
	}
	return dec.decodeElemColumn(elem, sliceType.Elem(), elemSize, total, elemPtrs)
}

// decodeElemColumn reads a column into ARRAY ELEMENTS (ptrs point at values).
func (dec *decoder) decodeElemColumn(elem *fieldMeta, elemType reflect.Type, elemSize uintptr, n int, ptrs []unsafe.Pointer) error {
	if elem.nullable {
		return dec.decodeNullableColumn(elem.elem, elem.pointeeType, n, ptrs)
	}
	switch elem.fType {
	case ftInt:
		vals, err := dec.readIntColumn(n, elem.bitWidth)
		if err != nil {
			return err
		}
		for i, p := range ptrs {
			setInt64At(elem.goKind, p, vals[i])
		}
	case ftFloat:
		vals := dec.readFloatColumn(n)
		for i, p := range ptrs {
			setFloat64At(elem.goKind, p, vals[i])
		}
	case ftString:
		dec.readByte()
		if err := dec.readStringColumn(len(ptrs), func(i int, s string) {
			*(*string)(ptrs[i]) = s
		}); err != nil {
			return err
		}
	case ftBytes:
		dec.readByte()
		blobs, err := dec.readBlobs(n)
		if err != nil {
			return err
		}
		for i, p := range ptrs {
			*(*[]byte)(p) = cloneBytes(blobs[i])
		}
	case ftStruct:
		dec.readByte()
		return dec.decodeSubTable(elem.sub, n, ptrs)
	case ftArray:
		dec.readByte()
		return dec.decodeArrayBody(elem.elem, elemType, elem.elemSize, ptrs)
	case ftMap:
		return dec.decodeMapColumn(elem, n, ptrs)
	case ftAny:
		return dec.decodeAnyColumn(elem, n, ptrs)
	}
	return nil
}

// readFloatColumn reads a flags byte + raw IEEE-754 payload; returns n values.
func (dec *decoder) readFloatColumn(n int) []float64 {
	flags := dec.readByte()
	width := uint8(32)
	if flags>>4&7 == 1 {
		width = 64
	}
	out := make([]float64, n)
	if flags>>7&1 == 1 { // empty column
		return out
	}
	for i := range n {
		if width == 64 {
			out[i] = math.Float64frombits(binary.LittleEndian.Uint64(dec.data[dec.pos:]))
			dec.pos += 8
		} else {
			out[i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(dec.data[dec.pos:])))
			dec.pos += 4
		}
	}
	return out
}

// readBlobs reads a length sub-column + concatenated bytes, returning n raw
// slices that alias the input buffer (callers copy as needed).
func (dec *decoder) readBlobs(n int) ([][]byte, error) {
	lengths, err := dec.readIntColumn(n, 64)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, n)
	for i := range n {
		l := int(lengths[i])
		out[i] = dec.data[dec.pos : dec.pos+l]
		dec.pos += l
	}
	return out, nil
}

// cloneBytes copies a slice so decoded []byte fields don't alias the input.
func cloneBytes(b []byte) []byte {
	c := make([]byte, len(b))
	copy(c, b)
	return c
}

// readStringColumn decodes n consecutive packed5 frames into one backing array
// and hands each string out as a slice of it, then calls set with the value for
// each slot.
//
// Decoding frame by frame allocates a string per value, which on a string-heavy
// payload is the largest single source of garbage in the decoder — and the cost
// is not only the allocation but the GC scanning that many more objects. The
// strings a column produces are retained or dropped together, so sharing one
// array between them changes nothing about their lifetime in practice.
//
// The strings can only be cut after the arena has stopped growing, so the
// offsets are recorded first and resolved in a second, allocation-free pass.
func (dec *decoder) readStringColumn(n int, set func(i int, s string)) error {
	offs := getI32(n + 1)
	defer putI32(offs)
	// Sized for the common short string; append grows it geometrically from here,
	// which costs a handful of reallocations per column against one allocation
	// per value.
	arena := make([]byte, 0, n*16)
	(*offs)[0] = 0
	for i := range n {
		next, consumed, err := packed5.AppendDecoded(arena, dec.data[dec.pos:])
		if err != nil {
			return err
		}
		arena = next
		dec.pos += consumed
		(*offs)[i+1] = int32(len(arena))
	}
	// arena is final, so pointers into it are stable. Sub-slicing a string
	// shares its backing array without copying.
	all := unsafe.String(unsafe.SliceData(arena), len(arena))
	for i := range n {
		set(i, all[(*offs)[i]:(*offs)[i+1]])
	}
	return nil
}

func (dec *decoder) readPacked5String() (string, error) {
	s, consumed, err := packed5.Decode(dec.data[dec.pos:])
	if err != nil {
		return "", err
	}
	dec.pos += consumed
	return s, nil
}

// readIntColumn reads a colbin type byte followed by one varint array frame.
func (dec *decoder) readIntColumn(n int, width uint8) ([]int64, error) {
	if flags := dec.readByte(); flags&7 != ftInt {
		return nil, fmt.Errorf("colbin: expected integer column at pos %d", dec.pos-1)
	}
	out := make([]int64, n)
	consumed, err := decodeIntColumn(dec.data[dec.pos:], n, width, out)
	if err != nil {
		return nil, err
	}
	dec.pos += consumed
	return out, nil
}
