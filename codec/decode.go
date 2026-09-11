package codec

import (
	"encoding/binary"
	"fmt"
	"math"
	"reflect"
	"unsafe"

	"github.com/ivanjoz/colbin/compact"
	"github.com/ivanjoz/colbin/packed5"
)

// derefTarget walks (and allocates) the destination pointer chain so decoding
// lands on the concrete value: e.g. **ContentFields -> alloc *ContentFields ->
// struct. rv is the non-nil pointer Unmarshal was handed.
func derefTarget(rv reflect.Value) reflect.Value {
	target := rv.Elem()
	for target.Kind() == reflect.Ptr {
		if target.IsNil() {
			target.Set(reflect.New(target.Type().Elem()))
		}
		target = target.Elem()
	}
	return target
}

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
//
// A destination slice with enough capacity is decoded into in place rather than
// replaced, so a caller decoding many messages through one hoisted variable
// allocates for the records once. The elements are overwritten, which means the
// slice must not be one the caller still holds a live reference to; passing a
// fresh or nil slice opts out.
func Unmarshal(data []byte, dst any) error {
	rv := reflect.ValueOf(dst)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return fmt.Errorf("colbin: Unmarshal needs a non-nil pointer")
	}
	// Walk the destination pointer chain first: both modes decode into the
	// concrete value, and compact mode dispatches on bit 0 of byte 0 before any
	// version byte exists to read.
	return decodeInto(data, derefTarget(rv))
}

// decodeInto decodes into a target the caller has already walked down to a
// concrete value. Unmarshal reaches it through reflect on the destination
// pointer; Codec[T] reaches it only for the forms its fast path does not cover.
func decodeInto(data []byte, target reflect.Value) error {
	if compact.IsCompact(data) {
		return decodeCompact(data, target)
	}

	dec := &decoder{data: data}
	if err := dec.header(); err != nil {
		return err
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

	n, err := dec.recordCount()
	if err != nil {
		return err
	}

	var elemType reflect.Type
	var recordPtrs *[]unsafe.Pointer
	var backing reflect.Value // kept alive so the backing array survives

	switch target.Kind() {
	case reflect.Slice:
		elemType = target.Type().Elem()
		backing = reuseSlice(target, n)
		recordPtrs = spreadPtrs(backing.UnsafePointer(), n, elemType.Size())
	case reflect.Struct:
		if n != 1 {
			return fmt.Errorf("colbin: message has %d records, cannot decode into a single struct", n)
		}
		elemType = target.Type()
		recordPtrs = getPtrs(1)
		(*recordPtrs)[0] = target.Addr().UnsafePointer()
	default:
		return fmt.Errorf("colbin: Unmarshal target must be *slice or *struct, got %s", target.Kind())
	}
	defer putPtrs(recordPtrs)

	ti, err := getTypeInfo(elemType)
	if err != nil {
		return err
	}
	if err := dec.decodeSubTable(ti, n, *recordPtrs); err != nil {
		return err
	}
	if target.Kind() == reflect.Slice {
		target.Set(backing)
	}
	return nil
}

// header validates the version byte and steps over a JSON message's schema
// section, leaving the cursor where the body begins.
func (dec *decoder) header() error {
	// Bounds first, and everything below stays inside them. A message arrives
	// from a file or a socket, so a length it declares is an instruction from
	// somewhere else: acting on one before checking it is how a decoder reads
	// memory it was never given. Every branch here used to trust one.
	if dec.pos >= len(dec.data) {
		return fmt.Errorf("colbin: message is empty")
	}
	switch v := dec.readByte(); v {
	case formatVersion, formatVersionOmitEmpty:
	case jsonFormatVersion, jsonFormatVersionOmitEmpty:
		// A self-describing message: the body underneath is identical, so the
		// schema section is simply stepped over when the Go type is known.
		length, read := binary.Uvarint(dec.data[dec.pos:])
		if read <= 0 || length > uint64(len(dec.data)-dec.pos-read) {
			return fmt.Errorf("colbin: schema section runs past the message")
		}
		dec.pos += read + int(length)
	default:
		return fmt.Errorf("colbin: bad version byte 0x%02x", v)
	}
	return nil
}

// recordCount reads the record count that opens a records-layout body.
func (dec *decoder) recordCount() (int, error) {
	if dec.pos > len(dec.data) {
		return 0, fmt.Errorf("colbin: message ends before its record count")
	}
	n64, m := binary.Uvarint(dec.data[dec.pos:])
	if m <= 0 {
		return 0, fmt.Errorf("colbin: bad record count")
	}
	dec.pos += m
	return int(n64), nil
}

// reuseSlice returns the n-record slice to decode into, reusing the
// destination's own backing array when it is already big enough. A caller
// decoding many messages into one hoisted variable then allocates nothing for
// the records at all.
//
// The reused elements are zeroed first. A column the message does not carry --
// which is what a reader sees when the writer's struct had fewer fields -- is
// simply not written, and without the zeroing the previous message's values
// would show through underneath it. reflect's Clear is a typed bulk zero, so it
// costs one call and keeps the write barriers a raw memclr would skip.
func reuseSlice(target reflect.Value, n int) reflect.Value {
	if target.Cap() < n {
		return reflect.MakeSlice(target.Type(), n, n)
	}
	out := target.Slice3(0, n, n)
	out.Clear()
	return out
}

// spreadPtrs fills a pooled buffer with one pointer per record of a contiguous
// backing array. Every records-layout decode starts here.
func spreadPtrs(base unsafe.Pointer, n int, size uintptr) *[]unsafe.Pointer {
	ptrs := getPtrs(n)
	for i := range n {
		(*ptrs)[i] = unsafe.Add(base, uintptr(i)*size)
	}
	return ptrs
}

// decodeSubTable reads [colCount] then each [id][column] into the given records.
//
// The id is resolved through the type's precomputed table rather than a map, so
// a message decoded a second time pays one array load per column. That matters
// most for a nested array of structs: the sub-table is re-entered for every such
// column of every message, and the type it names was already described the first
// time this program saw it.
func (dec *decoder) decodeSubTable(ti *typeInfo, n int, ptrs []unsafe.Pointer) error {
	colCount := int(dec.readByte())
	for range colCount {
		id := dec.readByte()
		i := ti.byID[id]
		if i == 0 {
			return fmt.Errorf("colbin: unknown field id %d (schema mismatch)", id)
		}
		if err := dec.decodeColumn(&ti.fields[i-1], n, ptrs); err != nil {
			return err
		}
	}
	return nil
}

// offsetPtrsPooled is offsetPtrs onto a pooled buffer. Every caller here hands
// the result to one decode call and drops it, so the buffer goes straight back.
func (dec *decoder) offsetPtrsPooled(ptrs []unsafe.Pointer, offset uintptr) *[]unsafe.Pointer {
	out := getPtrs(len(ptrs))
	for i, p := range ptrs {
		(*out)[i] = unsafe.Add(p, offset)
	}
	return out
}

func (dec *decoder) readByte() byte {
	b := dec.data[dec.pos]
	dec.pos++
	return b
}

// decodeColumn reads one column into STRUCT FIELDS (value at ptr+fm.offset).
func (dec *decoder) decodeColumn(fm *fieldMeta, n int, ptrs []unsafe.Pointer) error {
	if fm.nullable {
		slots := dec.offsetPtrsPooled(ptrs, fm.offset)
		err := dec.decodeNullableColumn(fm.elem, fm.pointeeType, n, *slots)
		putPtrs(slots)
		return err
	}
	switch fm.fType {
	case ftInt:
		vals, err := dec.readIntColumn(n, fm.bitWidth)
		if err != nil {
			return err
		}
		for i, p := range ptrs {
			setInt64(fm, p, (*vals)[i])
		}
		putI64(vals)
	case ftFloat:
		vals := dec.readFloatColumn(n)
		for i, p := range ptrs {
			setFloat64(fm, p, (*vals)[i])
		}
		putF64(vals)
	case ftString:
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
			fm.xf.SetBytes(p, cloneBytes((*blobs)[i]))
		}
		putBlobs(blobs)
	case ftStruct:
		dec.readByte() // flags (ftStruct)
		childPtrs := dec.offsetPtrsPooled(ptrs, fm.offset)
		err := dec.decodeSubTable(fm.sub, n, *childPtrs)
		putPtrs(childPtrs)
		return err
	case ftArray:
		dec.readByte() // flags (ftArray)
		shPtrs := dec.offsetPtrsPooled(ptrs, fm.offset)
		err := dec.decodeArrayBody(fm.elem, fm.sliceType, fm.elemSize, *shPtrs)
		putPtrs(shPtrs)
		return err
	case ftMap:
		slots := dec.offsetPtrsPooled(ptrs, fm.offset)
		err := dec.decodeMapColumn(fm, n, *slots)
		putPtrs(slots)
		return err
	case ftAny:
		slots := dec.offsetPtrsPooled(ptrs, fm.offset)
		err := dec.decodeAnyColumn(fm, n, *slots)
		putPtrs(slots)
		return err
	}
	return nil
}

// decodeArrayBody reads the length sub-column, allocates the records' slices,
// then decodes the flattened element column into their backing arrays.
// shPtrs point at the slice headers to populate (one per record).
//
// The column gets ONE backing array, sub-sliced per record, rather than a
// MakeSlice per record. The wire already stores these elements flattened and
// contiguous, so this is the layout the column is in; the old shape allocated
// once per record and then reassembled the same contiguous run in elemPtrs, an
// append into a slice that started at capacity zero. Each record's header is cut
// with cap == len, so appending to one record's slice reallocates instead of
// stomping the next record's elements. The arrays a column produces are retained
// or dropped together, which is the same trade readStringColumn already makes
// for the strings of one column.
func (dec *decoder) decodeArrayBody(elem *fieldMeta, sliceType reflect.Type, elemSize uintptr, shPtrs []unsafe.Pointer) error {
	lengths, err := dec.readIntColumn(len(shPtrs), 64)
	if err != nil {
		return err
	}
	total := 0
	for _, l := range *lengths {
		// A negative or absurd length is a corrupt message. It has to be caught
		// before it is summed, because the sum sizes the one array every record
		// then points into.
		if l < 0 || l > int64(len(dec.data)) {
			putI64(lengths)
			return fmt.Errorf("colbin: array length %d out of range", l)
		}
		total += int(l)
	}

	var elemPtrs *[]unsafe.Pointer
	if total > 0 {
		backing := reflect.MakeSlice(sliceType, total, total)
		base := backing.UnsafePointer()
		elemPtrs = getPtrs(total)
		for j := range total {
			(*elemPtrs)[j] = unsafe.Add(base, uintptr(j)*elemSize)
		}
		off := 0
		for i, sp := range shPtrs {
			l := int((*lengths)[i])
			if l == 0 {
				continue // leave the slice field as nil (matches Go zero value)
			}
			// GC keeps the whole array alive through any one of these interior
			// pointers, because the field is typed as a slice of its elements.
			*(*sliceHeader)(sp) = sliceHeader{data: (*elemPtrs)[off], len: l, cap: l}
			off += l
		}
	}
	putI64(lengths)

	if elideEmpty(elem, total) { // the encoder wrote no element column
		if elemPtrs != nil {
			putPtrs(elemPtrs)
		}
		return nil
	}
	var ptrs []unsafe.Pointer
	if elemPtrs != nil {
		ptrs = *elemPtrs
	}
	err = dec.decodeElemColumn(elem, sliceType.Elem(), elemSize, total, ptrs)
	if elemPtrs != nil {
		putPtrs(elemPtrs)
	}
	return err
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
			setInt64At(elem.goKind, p, (*vals)[i])
		}
		putI64(vals)
	case ftFloat:
		vals := dec.readFloatColumn(n)
		for i, p := range ptrs {
			setFloat64At(elem.goKind, p, (*vals)[i])
		}
		putF64(vals)
	case ftString:
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
			*(*[]byte)(p) = cloneBytes((*blobs)[i])
		}
		putBlobs(blobs)
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

// readFloatColumn reads a flags byte + raw IEEE-754 payload; returns n values in
// a pooled buffer the caller returns once it has read them out.
func (dec *decoder) readFloatColumn(n int) *[]float64 {
	flags := dec.readByte()
	width := uint8(32)
	if flags>>4&7 == 1 {
		width = 64
	}
	buf := getF64(n)
	out := *buf
	if flags>>7&1 == 1 { // empty column: the pooled buffer still holds the last
		clear(out) // column's values, so a zero column has to be written out
		return buf
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
	return buf
}

// readBlobs reads a length sub-column + concatenated bytes into a pooled buffer,
// returning n raw slices that alias the input (callers copy as needed, and
// return the buffer once they have).
func (dec *decoder) readBlobs(n int) (*[][]byte, error) {
	lengths, err := dec.readIntColumn(n, 64)
	if err != nil {
		return nil, err
	}
	defer putI64(lengths)
	buf := getBlobs(n)
	out := *buf
	for i := range n {
		l := int((*lengths)[i])
		if l < 0 || dec.pos+l > len(dec.data) {
			putBlobs(buf)
			return nil, fmt.Errorf("colbin: blob length %d out of range", l)
		}
		out[i] = dec.data[dec.pos : dec.pos+l]
		dec.pos += l
	}
	return buf, nil
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
	flags := dec.readByte()
	if flags&7 != ftString {
		return fmt.Errorf("colbin: expected string column at pos %d", dec.pos-1)
	}
	if flags&emptyColumnBit != 0 {
		for i := range n {
			set(i, "")
		}
		return nil
	}
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

// readIntColumn reads a colbin type byte followed by one varint array frame. The
// empty bit says the column held nothing but zeros and carries no frame at all,
// which is what a zero value costs under omit-empty.
//
// The values land in a pooled buffer, which the caller returns once it has read
// them into the destination. decodeIntColumn fills all n on success, so the only
// path that has to zero the buffer is the empty column — everything else
// overwrites whatever the previous column left there.
func (dec *decoder) readIntColumn(n int, width uint8) (*[]int64, error) {
	flags := dec.readByte()
	if flags&7 != ftInt {
		return nil, fmt.Errorf("colbin: expected integer column at pos %d", dec.pos-1)
	}
	buf := getI64(n)
	if flags&emptyColumnBit != 0 {
		clear(*buf)
		return buf, nil
	}
	consumed, err := decodeIntColumn(dec.data[dec.pos:], n, width, *buf)
	if err != nil {
		putI64(buf)
		return nil, err
	}
	dec.pos += consumed
	return buf, nil
}
