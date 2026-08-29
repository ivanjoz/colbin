package codec

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	"github.com/ivanjoz/colbin/packed5"
)

// Schema-driven decoding: the other direction of the JSON mode. A message
// written by MarshalJSON carries its own schema, so these decoders walk the same
// columns as decode.go without any Go type to guide them, and hand back plain
// Go values (or JSON) instead of filling a struct.
//
// The result uses the JSON-ish subset: nil, bool, int64, uint64, float32,
// float64, string, []byte, []any and map[string]any. Objects are maps, so key
// order is not preserved — JSON objects are unordered, and the schema keeps the
// declaration order for anyone who needs it.

// jsonDesc is the reader's side of a schema descriptor: the type facts the
// columns need, with no reflect.Type anywhere.
type jsonDesc struct {
	fType     uint8
	nullable  bool
	cyclic    bool
	kind      uint8     // scalar kind, for ftInt/ftFloat
	elem      *jsonDesc // ftArray
	key, val  *jsonDesc // ftMap
	structIdx int       // ftStruct: index into jsonSchema.structs
}

// jsonField is one column of a struct definition.
type jsonField struct {
	id   uint8
	name string
	desc jsonDesc
}

// jsonStruct is a struct definition: its columns in declaration order, plus the
// id lookup the body needs (columns are written in order, but the body is
// authoritative about which id comes next).
type jsonStruct struct {
	fields []jsonField
	byID   map[uint8]*jsonField
}

// jsonSchema is a parsed schema section.
type jsonSchema struct {
	records      bool
	singleStruct bool
	structs      []*jsonStruct
	root         jsonDesc
}

// DecodeAny decodes a message written by MarshalJSON without needing its Go
// type, returning []any of map[string]any for a slice of records, a single
// map[string]any for a lone struct, or the value itself in value mode.
func DecodeAny(data []byte) (any, error) {
	return decodeSelfDescribing(data, false)
}

// DecodeJSON decodes a message written by MarshalJSON straight to JSON text.
// Non-finite floats have no JSON form and are written as null; []byte columns
// follow encoding/json and are base64 strings.
func DecodeJSON(data []byte) ([]byte, error) {
	v, err := decodeSelfDescribing(data, true)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// decodeSelfDescribing parses the schema section and decodes the body under it.
// jsonSafe replaces values JSON cannot represent (see DecodeJSON).
//
// Like the typed decoder, the column readers trust their input and index the
// buffer directly, so a truncated or corrupt message can panic on a slice bound.
// These entry points take data from a wire, so the panic is converted here.
func decodeSelfDescribing(data []byte, jsonSafe bool) (out any, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("colbin: malformed self-describing message: %v", r)
		}
	}()

	dec := &decoder{data: data}
	if len(data) == 0 {
		return nil, fmt.Errorf("colbin: empty message")
	}
	switch v := dec.readByte(); v {
	case jsonFormatVersion, jsonFormatVersionOmitEmpty:
	case formatVersion, formatVersionOmitEmpty:
		return nil, fmt.Errorf("colbin: message carries no schema (encoded with Marshal, not MarshalJSON)")
	default:
		return nil, fmt.Errorf("colbin: bad version byte 0x%02x", v)
	}

	schemaLen := int(dec.readUvarint())
	end := dec.pos + schemaLen
	if schemaLen < 0 || end > len(dec.data) {
		return nil, fmt.Errorf("colbin: schema section overruns the message")
	}
	sch, err := parseSchema(dec.data[dec.pos:end])
	if err != nil {
		return nil, err
	}
	dec.pos = end
	dec.jsonSafe = jsonSafe

	if !sch.records {
		vals, err := dec.jsonColumn(&sch.root, sch, 1)
		if err != nil {
			return nil, err
		}
		return vals[0], nil
	}

	n := int(dec.readUvarint())
	rows, err := dec.jsonSubTable(sch.structs[sch.root.structIdx], sch, n)
	if err != nil {
		return nil, err
	}
	if sch.singleStruct {
		if n != 1 {
			return nil, fmt.Errorf("colbin: single-struct message has %d records", n)
		}
		return rows[0], nil
	}
	return rows, nil
}

// --- schema parsing ---

// schemaReader walks the schema section.
type schemaReader struct {
	buf []byte
	pos int
}

func parseSchema(buf []byte) (*jsonSchema, error) {
	r := &schemaReader{buf: buf}
	flags := r.byte()
	sch := &jsonSchema{
		records:      flags&schRecords != 0,
		singleStruct: flags&schSingleStruct != 0,
	}
	count := int(r.uvarint())
	if count < 0 || count > len(buf) {
		return nil, fmt.Errorf("colbin: schema declares %d struct definitions", count)
	}
	// Allocate every definition up front: a descriptor may reference a struct
	// that is defined later in the section (mutually recursive types), so the
	// slots have to exist before the definitions are read.
	sch.structs = make([]*jsonStruct, count)
	for i := range sch.structs {
		sch.structs[i] = &jsonStruct{}
	}
	for _, st := range sch.structs {
		fieldCount := int(r.byte())
		st.fields = make([]jsonField, fieldCount)
		st.byID = make(map[uint8]*jsonField, fieldCount)
		for k := range st.fields {
			f := &st.fields[k]
			f.id = r.byte()
			name, err := r.packed5()
			if err != nil {
				return nil, err
			}
			f.name = name
			if err := r.desc(&f.desc, count); err != nil {
				return nil, err
			}
			st.byID[f.id] = f
		}
	}
	if err := r.desc(&sch.root, count); err != nil {
		return nil, err
	}
	if sch.records && (sch.root.fType != ftStruct || sch.root.structIdx >= count) {
		return nil, fmt.Errorf("colbin: records schema has a non-struct root")
	}
	if r.pos != len(buf) {
		return nil, fmt.Errorf("colbin: schema section has %d trailing bytes", len(buf)-r.pos)
	}
	return sch, nil
}

// desc parses one descriptor into d, recursing for composite types.
func (r *schemaReader) desc(d *jsonDesc, structCount int) error {
	flags := r.byte()
	d.fType = flags & 7
	d.nullable = flags&descNullable != 0
	d.cyclic = flags&descCyclic != 0
	switch d.fType {
	case ftInt, ftFloat:
		d.kind = r.byte()
		if d.kind > skFloat64 {
			return fmt.Errorf("colbin: unknown scalar kind %d in schema", d.kind)
		}
	case ftArray:
		d.elem = new(jsonDesc)
		return r.desc(d.elem, structCount)
	case ftStruct:
		d.structIdx = int(r.uvarint())
		if d.structIdx < 0 || d.structIdx >= structCount {
			return fmt.Errorf("colbin: schema struct index %d out of range", d.structIdx)
		}
	case ftMap:
		d.key, d.val = new(jsonDesc), new(jsonDesc)
		if err := r.desc(d.key, structCount); err != nil {
			return err
		}
		return r.desc(d.val, structCount)
	}
	return nil
}

func (r *schemaReader) byte() byte {
	if r.pos >= len(r.buf) {
		panic("schema section truncated")
	}
	b := r.buf[r.pos]
	r.pos++
	return b
}

func (r *schemaReader) uvarint() uint64 {
	v, m := binary.Uvarint(r.buf[r.pos:])
	if m <= 0 {
		panic("bad uvarint in schema section")
	}
	r.pos += m
	return v
}

func (r *schemaReader) packed5() (string, error) {
	s, consumed, err := packed5.Decode(r.buf[r.pos:])
	if err != nil {
		return "", err
	}
	r.pos += consumed
	return s, nil
}

// --- body decoding ---

// jsonSubTable reads a sub-table of n records into n maps keyed by field name.
func (dec *decoder) jsonSubTable(st *jsonStruct, sch *jsonSchema, n int) ([]any, error) {
	colCount := int(dec.readByte())
	rows := make([]map[string]any, n)
	for i := range rows {
		rows[i] = make(map[string]any, colCount)
	}
	for range colCount {
		id := dec.readByte()
		f := st.byID[id]
		if f == nil {
			return nil, fmt.Errorf("colbin: field id %d is not in the schema", id)
		}
		vals, err := dec.jsonColumn(&f.desc, sch, n)
		if err != nil {
			return nil, err
		}
		for i, row := range rows {
			row[f.name] = vals[i]
		}
	}
	out := make([]any, n)
	for i, row := range rows {
		out[i] = row
	}
	return out, nil
}

// jsonColumn reads one column of n values, mirroring decodeElemColumn but
// producing plain Go values instead of writing through pointers.
func (dec *decoder) jsonColumn(d *jsonDesc, sch *jsonSchema, n int) ([]any, error) {
	if d.nullable {
		return dec.jsonNullableColumn(d, sch, n)
	}
	out := make([]any, n)
	switch d.fType {
	case ftInt:
		vals, err := dec.readIntColumn(n, bitWidthOfKind(d.kind))
		if err != nil {
			return nil, err
		}
		for i, v := range vals {
			out[i] = scalarFromInt64(d.kind, v)
		}
	case ftFloat:
		vals := dec.readFloatColumn(n)
		for i, v := range vals {
			out[i] = dec.floatValue(d.kind, v)
		}
	case ftString:
		if err := dec.readStringColumn(n, func(i int, s string) { out[i] = s }); err != nil {
			return nil, err
		}
	case ftBytes:
		dec.readByte() // flags (ftBytes)
		blobs, err := dec.readBlobs(n)
		if err != nil {
			return nil, err
		}
		for i, b := range blobs {
			out[i] = cloneBytes(b)
		}
	case ftStruct:
		dec.readByte() // flags (ftStruct)
		return dec.jsonSubTable(sch.structs[d.structIdx], sch, n)
	case ftArray:
		dec.readByte() // flags (ftArray)
		return dec.jsonArrayColumn(d, sch, n)
	case ftMap:
		dec.readByte() // flags (ftMap)
		return dec.jsonMapColumn(d, sch, n)
	case ftAny:
		dec.readByte() // flags (ftAny)
		for i := range n {
			v, err := dec.decodeAnyValue()
			if err != nil {
				return nil, err
			}
			out[i] = dec.sanitizeAny(v)
		}
	default:
		return nil, fmt.Errorf("colbin: unknown column type %d in schema", d.fType)
	}
	return out, nil
}

// jsonArrayColumn reads the length sub-column, then the flattened elements, and
// cuts them back into one slice per record. A zero-length slice decodes to nil,
// matching the typed decoder (and rendering as JSON null).
func (dec *decoder) jsonArrayColumn(d *jsonDesc, sch *jsonSchema, n int) ([]any, error) {
	lengths, err := dec.readIntColumn(n, 64)
	if err != nil {
		return nil, err
	}
	total := 0
	for _, l := range lengths {
		total += int(l)
	}
	var flat []any
	if !(total == 0 && d.elem.cyclic) { // the encoder elides only this case
		if flat, err = dec.jsonColumn(d.elem, sch, total); err != nil {
			return nil, err
		}
	}
	out := make([]any, n)
	at := 0
	for i, l := range lengths {
		if l == 0 {
			continue
		}
		out[i] = flat[at : at+int(l) : at+int(l)]
		at += int(l)
	}
	return out, nil
}

// jsonMapColumn reads the entry-count sub-column, then the flattened keys and
// values, and rebuilds one map per record. JSON object keys are strings, so a
// non-string map key is formatted as one.
func (dec *decoder) jsonMapColumn(d *jsonDesc, sch *jsonSchema, n int) ([]any, error) {
	lengths, err := dec.readIntColumn(n, 64)
	if err != nil {
		return nil, err
	}
	total := 0
	for _, l := range lengths {
		total += int(l)
	}
	var keys, vals []any
	if !(total == 0 && d.key.cyclic) {
		if keys, err = dec.jsonColumn(d.key, sch, total); err != nil {
			return nil, err
		}
	}
	if !(total == 0 && d.val.cyclic) {
		if vals, err = dec.jsonColumn(d.val, sch, total); err != nil {
			return nil, err
		}
	}
	out := make([]any, n)
	at := 0
	for i, l := range lengths {
		if l == 0 {
			continue // nil map, like the typed decoder
		}
		m := make(map[string]any, l)
		for range int(l) {
			m[mapKeyString(keys[at])] = vals[at]
			at++
		}
		out[i] = m
	}
	return out, nil
}

// jsonNullableColumn reads the null wrapper, then the dense inner column, and
// scatters the values back over their slots (absent slots stay nil).
func (dec *decoder) jsonNullableColumn(d *jsonDesc, sch *jsonSchema, n int) ([]any, error) {
	hasNulls := dec.readByte()&1 == 1
	present := make([]bool, n)
	numPresent := 0
	if hasNulls {
		bmBytes := (n + 7) / 8
		bitmap := dec.data[dec.pos : dec.pos+bmBytes]
		dec.pos += bmBytes
		for i := range n {
			if bitmap[i>>3]>>(uint(i)&7)&1 == 1 {
				present[i] = true
				numPresent++
			}
		}
	} else {
		for i := range present {
			present[i] = true
		}
		numPresent = n
	}
	out := make([]any, n)
	if numPresent == 0 && d.cyclic { // the encoder wrote no value column
		return out, nil
	}
	inner := *d
	inner.nullable = false
	vals, err := dec.jsonColumn(&inner, sch, numPresent)
	if err != nil {
		return nil, err
	}
	k := 0
	for i := range n {
		if !present[i] {
			continue // JSON null
		}
		out[i] = vals[k]
		k++
	}
	return out, nil
}

// scalarFromInt64 turns a column value back into its declared Go type. Unsigned
// columns travel as the same-width signed type, so the bit pattern is reread at
// that width — this is what keeps values above the signed maximum (up to the
// whole uint64 range) intact.
func scalarFromInt64(kind uint8, v int64) any {
	switch kind {
	case skBool:
		return v != 0
	case skUint8:
		return uint64(uint8(v))
	case skUint16:
		return uint64(uint16(v))
	case skUint32:
		return uint64(uint32(v))
	case skUint64, skUint:
		return uint64(v)
	}
	return v
}

// floatValue narrows a float32 column back to float32 so it renders with 32-bit
// precision (1.1, not 1.100000023841858), and drops values JSON cannot express
// when the caller asked for JSON.
func (dec *decoder) floatValue(kind uint8, v float64) any {
	if dec.jsonSafe && (math.IsNaN(v) || math.IsInf(v, 0)) {
		return nil
	}
	if kind == skFloat32 {
		return float32(v)
	}
	return v
}

// sanitizeAny walks a decoded any-column value, which is the one place a float
// can appear without a schema kind to check.
func (dec *decoder) sanitizeAny(v any) any {
	if !dec.jsonSafe {
		return v
	}
	switch t := v.(type) {
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return nil
		}
	case []any:
		for i, e := range t {
			t[i] = dec.sanitizeAny(e)
		}
	case map[string]any:
		for k, e := range t {
			t[k] = dec.sanitizeAny(e)
		}
	}
	return v
}

// mapKeyString renders a decoded map key as a JSON object key.
func mapKeyString(k any) string {
	switch t := k.(type) {
	case string:
		return t
	case int64:
		return strconv.FormatInt(t, 10)
	case uint64:
		return strconv.FormatUint(t, 10)
	case bool:
		return strconv.FormatBool(t)
	case float32:
		return strconv.FormatFloat(float64(t), 'g', -1, 32)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	}
	return fmt.Sprint(k)
}
