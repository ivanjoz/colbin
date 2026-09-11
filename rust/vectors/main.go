// Command vectors writes the corpus the Rust decoder is pinned against.
//
// The Go codecs are the specification. Every message in the output was produced
// by colbin.Marshal on a real Go value, and every expected value was read back
// out of those bytes by the Go decoder — the columnar path through
// colbin.Unmarshal, and the compact path through a compact.Reader driven by the
// same field kinds the Rust schema declares. Nothing here hand-writes a frame or
// hand-computes a field id, because an expectation written by hand only tests
// the person who wrote it.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/ivanjoz/colbin"
	"github.com/ivanjoz/colbin/compact"
	"github.com/ivanjoz/colbin/packed5"
)

// --- the corpus types --------------------------------------------------------
//
// None of these carry a json tag. A json tag renames a field in the schema
// section without touching the binary payload, and the schema section is where
// this generator reads the wire ids from — so a tag here would report a name the
// hash never saw.

// Scalars covers every scalar kind compact mode can carry, with hashed ids.
type Scalars struct {
	B    bool
	I8   int8
	I16  int16
	I32  int32
	I64  int64
	U8   uint8
	U16  uint16
	U32  uint32
	U64  uint64
	F32  float32
	F64  float64
	S    string
	Blob []byte
}

// Slices covers every primitive-slice kind. Unsigned slices ride the same-width
// signed codec, which preserves the bit pattern, so both signednesses are here.
type Slices struct {
	I8s  []int8
	I16s []int16
	I32s []int32
	I64s []int64
	U16s []uint16
	U32s []uint32
	U64s []uint64
	Bs   []bool
	Ss   []string
	F32s []float32
	F64s []float64
}

// Narrow tags every field with an id at or below 14, which is what lets compact
// mode take the 4-bit key width.
type Narrow struct {
	A int32   `cb:"1"`
	B string  `cb:"2"`
	C bool    `cb:"3"`
	D uint64  `cb:"4"`
	E []int32 `cb:"5"`
}

// Sparse is a wide struct most of whose fields stay zero, so that a two- or
// three-record message is smaller in compact mode than columnar and the encoder
// keeps the compact form.
type Sparse struct {
	A int32
	B int32
	C int32
	D int32
	E int32
	F string
	G string
	H bool
}

// Aligned puts three bools before a string: under the 8-bit key width that is
// 5 header bits plus 3*(8+1), which lands the string's packed5 frame on a byte
// boundary. See alignmentOf, which measures it rather than trusting the sum.
type Aligned struct {
	A bool
	B bool
	C bool
	S string
}

// Unaligned puts one bool before the string, so the frame starts mid byte and
// the decoder has to shift the tail into place before reading it.
type Unaligned struct {
	A bool
	S string
}

// UsuarioToken mirrors genix's core.UsuarioToken, whose json tags are dropped
// here for the reason above; they never reach the binary payload. Its Error
// field carries cb:"-" there and so is not encoded, and is left out entirely.
type UsuarioToken struct {
	CompanyID int32
	ID        int32
	Created   int32
	Hash      uint64
	User      string
}

// --- output shapes -----------------------------------------------------------

type fieldOut struct {
	Name string `json:"name"`
	// The wire id, read out of a schema section the colbin package wrote.
	ID int `json:"id"`
	// The explicit id from a cb:"N" tag, absent for a hashed field.
	Explicit *int   `json:"explicitId,omitempty"`
	Kind     string `json:"kind"`
}

type fieldValue struct {
	ID int `json:"id"`
	V  any `json:"v"`
}

type caseOut struct {
	Name      string `json:"name"`
	Mode      string `json:"mode"`
	OmitEmpty bool   `json:"omitEmpty"`
	// Byte 0 in standard mode; 0 in compact mode, which has no version field.
	Version int `json:"version"`
	// The header's shape bits in compact mode, -1 in standard mode.
	Shape       int            `json:"shape"`
	AllPositive bool           `json:"allPositive"`
	NarrowKeys  bool           `json:"narrowKeys"`
	Fields      []fieldOut     `json:"fields"`
	Message     string         `json:"message"`
	Records     [][]fieldValue `json:"records"`
	// Whether the Rust encoder can reproduce this message byte for byte. It
	// writes raw packed5 frames, and Go picks the cheaper of raw and packed --
	// so the two agree exactly unless some string in the value packs smaller.
	RustByteExact bool `json:"rustByteExact"`
}

type fieldIDCase struct {
	Name   string     `json:"name"`
	Fields []fieldOut `json:"fields"`
}

type corpus struct {
	FieldIDs []fieldIDCase `json:"fieldIds"`
	Cases    []caseOut     `json:"cases"`
}

func main() {
	out := "rust/vectors/vectors.json"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	c := corpus{FieldIDs: fieldIDCases(), Cases: cases()}
	reportCoverage(c.Cases)

	body, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		panic(err)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(out, append(body, '\n'), 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("%d field-id cases, %d messages -> %s\n", len(c.FieldIDs), len(c.Cases), out)

	// The composite corpus, whose schema representation is a tree rather than a
	// string. See composites.go for why it is a separate file.
	writeComposites(filepath.Join(filepath.Dir(out), "composites.json"))
}

// --- the cases ---------------------------------------------------------------

func cases() []caseOut {
	var out []caseOut
	add := func(c caseOut) { out = append(out, c) }

	// Compact mode. A lone struct always takes it: the columnar header and the
	// per-column type bytes have nothing to amortise over at one record.
	add(marshalCase("compact-scalars", Scalars{
		B: true, I8: -7, I16: 300, I32: -70000, I64: 1 << 40,
		U8: 200, U16: 60000, U32: 4_000_000_000, U64: math.MaxUint64,
		F32: 1.5, F64: -0.1, S: "Usuario1", Blob: []byte{0, 1, 2, 250, 255},
	}))
	// Every signed value non-negative, so ALL_POSITIVE is set and the payloads
	// are magnitudes rather than zigzags.
	add(marshalCase("compact-all-positive", Scalars{
		B: true, I8: 7, I16: 300, I32: 70000, I64: 1 << 40,
		U8: 1, U16: 2, U32: 3, U64: 4, F32: 2.5, F64: 0.5, S: "ok",
	}))
	// Only two fields set: everything else is omitted, which is the whole of
	// compact mode's presence rule.
	add(marshalCase("compact-mostly-omitted", Scalars{I32: 42, S: "x"}))
	// Nothing set at all: a record that is its terminator and nothing else.
	add(marshalCase("compact-empty-record", Scalars{}))
	add(marshalCase("compact-slices", Slices{
		I8s:  []int8{-1, 0, 1, 127, -128},
		I16s: []int16{1000, 1001, 1002, 1003},
		I32s: []int32{-70000, 70000},
		I64s: []int64{math.MinInt64, 0, math.MaxInt64},
		U16s: []uint16{0, 65535},
		U32s: []uint32{4_000_000_000, 1},
		U64s: []uint64{math.MaxUint64, 0},
		Bs:   []bool{true, false, true, true, false},
		Ss:   []string{"uno", "dos", "tres"},
		F32s: []float32{1.5, -2.25},
		F64s: []float64{0.1, 1e300, -0.0},
	}))
	add(marshalCase("compact-single-element-slices", Slices{
		I32s: []int32{5}, Ss: []string{"solo"}, Bs: []bool{true},
	}))
	// Narrow keys: every id is at or below 14, so a key costs four bits.
	add(marshalCase("compact-narrow-keys", Narrow{
		A: 12345, B: "narrow", C: true, D: 1 << 63, E: []int32{7, 8, 9},
	}))
	add(marshalCase("compact-narrow-negative", Narrow{A: -5, B: "neg"}))
	// Shapes 1..3: an array of that many records.
	add(marshalCase("compact-shape-1", []Sparse{{A: 1, F: "a"}}))
	add(marshalCase("compact-shape-2", []Sparse{{A: 1, F: "a"}, {B: 2, G: "b"}}))
	add(marshalCase("compact-shape-3", []Sparse{{A: 1}, {B: -2, H: true}, {C: 3, F: "c"}}))
	add(marshalCase("compact-token", UsuarioToken{
		CompanyID: 7, ID: 42, Created: 1234, Hash: 12_720_753_295_591_565_293, User: "tester",
	}))
	add(marshalCase("compact-token-accented", UsuarioToken{
		CompanyID: 999999, ID: 12345, Created: 1_700_000_000,
		Hash: 2_309_713_256_132_687_586, User: "ñandú@example.com",
	}))
	add(marshalCase("compact-token-empty-user", UsuarioToken{CompanyID: 1, ID: 1}))

	// The two string alignments. alignmentOf replays the record through the real
	// compact.Writer to measure where the frame lands, so the claim in each name
	// is checked rather than asserted.
	aligned := Aligned{A: true, B: true, C: true, S: "alineado"}
	requireAlignment("compact-string-aligned", alignmentOf(aligned), true)
	add(marshalCase("compact-string-aligned", aligned))
	unaligned := Unaligned{A: true, S: "desalineado"}
	requireAlignment("compact-string-unaligned", alignmentOf(unaligned), false)
	add(marshalCase("compact-string-unaligned", unaligned))

	// Standard mode. Past three records compact mode is not offered at all, so
	// four is the smallest message that is columnar by construction.
	add(marshalCase("standard-scalars-4", []Scalars{
		{B: true, I8: 1, I16: 2, I32: 3, I64: 4, U8: 5, U16: 6, U32: 7, U64: 8, F32: 1.5, F64: 2.5, S: "uno", Blob: []byte{1}},
		{B: false, I8: -1, I16: -2, I32: -3, I64: -4, U8: 9, U16: 10, U32: 11, U64: 12, F32: -1.5, F64: -2.5, S: "dos", Blob: []byte{2, 3}},
		{B: true, I8: 100, I16: 3000, I32: 70000, I64: 1 << 40, U8: 255, U16: 65535, U32: 4_000_000_000, U64: math.MaxUint64, F32: 0, F64: 0, S: "", Blob: nil},
		{B: false, I8: -128, I16: -32768, I32: math.MinInt32, I64: math.MinInt64, U8: 0, U16: 0, U32: 0, U64: 0, F32: 3.25, F64: 1e-300, S: "cuatro", Blob: []byte{9, 8, 7}},
	}))
	// Float columns set the empty bit whenever every value is zero, whatever the
	// omit-empty setting — which is where a 0x02 message gets an empty column.
	add(marshalCase("standard-empty-float-column", []Scalars{
		{I32: 1, S: "a"}, {I32: 2, S: "b"}, {I32: 3, S: "c"}, {I32: 4, S: "d"},
	}))
	add(marshalCase("standard-slices-4", []Slices{
		{I8s: []int8{1, 2}, I32s: []int32{-1}, Ss: []string{"a"}, Bs: []bool{true}, F64s: []float64{1.5}},
		{I8s: nil, I32s: []int32{7, 8, 9}, Ss: []string{"b", "c"}, U32s: []uint32{4_000_000_000}},
		{I16s: []int16{1000}, I64s: []int64{1 << 40}, F32s: []float32{2.5, 3.5}, U64s: []uint64{math.MaxUint64}},
		{U16s: []uint16{65535}, Ss: nil, Bs: []bool{false, true, false}},
	}))
	add(marshalCase("standard-tokens-4", []UsuarioToken{
		{CompanyID: 7, ID: 42, Created: 1234, Hash: 12_720_753_295_591_565_293, User: "tester"},
		{CompanyID: 1, ID: 1, Created: 0, Hash: 1, User: ""},
		{CompanyID: math.MaxInt32, ID: math.MaxInt32, Created: math.MaxInt32, Hash: math.MaxUint64, User: "x"},
		{CompanyID: 128, ID: 127, Created: 65536, Hash: 0, User: "a-very-long-user-name-for-width-testing"},
	}))
	// No records at all: every column is present and holds nothing.
	add(marshalCase("standard-zero-records", []UsuarioToken{}))
	// Enough records that the varint array codec has a column worth of data to
	// choose a transform over.
	add(marshalCase("standard-many-records", manyTokens(64)))

	// The same shapes with omit-empty on, which is version 0x06 and is where an
	// integer or string column of nothing but empty values becomes its type byte
	// alone.
	colbin.SetOmitEmpty(true)
	add(marshalCase("standard-omit-empty-columns", []Scalars{
		{I32: 1, S: "a"}, {I32: 2, S: "b"}, {I32: 3, S: "c"}, {I32: 4, S: "d"},
	}))
	add(marshalCase("standard-omit-empty-tokens", manyTokens(8)))
	add(marshalCase("standard-omit-empty-slices", []Slices{
		{I32s: []int32{1}}, {I32s: []int32{2}}, {I32s: nil}, {I32s: []int32{3, 4}},
	}))
	// Compact mode omits a zero field either way, so the flag changes nothing
	// here — which is worth a case rather than a claim.
	add(marshalCase("compact-omit-empty-token", UsuarioToken{CompanyID: 3, User: "flag"}))
	colbin.SetOmitEmpty(false)

	return out
}

func manyTokens(n int) []UsuarioToken {
	out := make([]UsuarioToken, n)
	for i := range out {
		out[i] = UsuarioToken{
			CompanyID: int32(7),
			ID:        int32(1000 + i*3),
			Created:   int32(1_700_000_000 + i),
			Hash:      uint64(i) * 0x9E3779B97F4A7C15,
			User:      fmt.Sprintf("usuario%02d", i),
		}
	}
	// One record with nothing in it, so a column has a zero among real values.
	out[n/2] = UsuarioToken{}
	return out
}

// marshalCase encodes value with the real colbin encoder, then reads the message
// back with the real colbin decoder to produce what the Rust port must reproduce.
func marshalCase(name string, value any) caseOut {
	data, err := colbin.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("%s: %v", name, err))
	}
	elem := reflect.TypeOf(value)
	if elem.Kind() == reflect.Slice {
		elem = elem.Elem()
	}
	fields := fieldsOf(elem)

	c := caseOut{
		Name:          name,
		OmitEmpty:     colbin.OmitEmpty(),
		Shape:         -1,
		Fields:        fields,
		Message:       base64.StdEncoding.EncodeToString(data),
		RustByteExact: everyStringIsRaw(value),
	}
	if compact.IsCompact(data) {
		r, err := compact.NewReader(data)
		if err != nil {
			panic(fmt.Sprintf("%s: %v", name, err))
		}
		c.Mode = "compact"
		c.Shape = int(r.Shape())
		c.AllPositive = r.AllPositive()
		c.NarrowKeys = r.Keys() == compact.Keys4
		c.Records = compactRecords(name, r, fields)
	} else {
		c.Mode = "standard"
		c.Version = int(data[0])
		c.Records = standardRecords(name, data, value, fields)
	}
	return c
}

// compactRecords walks the message with the Go compact reader, so the expected
// values and the expected *presence* both come from the format's own decoder
// rather than from this file's idea of which fields the encoder skipped.
func compactRecords(name string, r *compact.Reader, fields []fieldOut) [][]fieldValue {
	byID := map[int]fieldOut{}
	for _, f := range fields {
		byID[f.ID] = f
	}
	out := make([][]fieldValue, 0, r.Records())
	for range r.Records() {
		record := []fieldValue{}
		for {
			key := r.Key()
			if err := r.Err(); err != nil {
				panic(fmt.Sprintf("%s: %v", name, err))
			}
			if key == compact.TerminatorKey {
				break
			}
			f, ok := byID[int(key)]
			if !ok {
				panic(fmt.Sprintf("%s: message names field id %d, which the schema does not have", name, key))
			}
			record = append(record, fieldValue{ID: f.ID, V: readCompact(r, f.Kind)})
		}
		out = append(out, record)
	}
	if err := r.Err(); err != nil {
		panic(fmt.Sprintf("%s: %v", name, err))
	}
	return out
}

// readCompact reads one value at the kind the schema declares, which is the same
// information the Rust decoder works from.
func readCompact(r *compact.Reader, kind string) any {
	switch kind {
	case "bool":
		return r.Bool()
	case "i8":
		return decimal(int64(int8(r.Int())))
	case "i16":
		return decimal(int64(int16(r.Int())))
	case "i32":
		return decimal(int64(int32(r.Int())))
	case "i64":
		return decimal(r.Int())
	case "u8":
		return udecimal(uint64(uint8(r.Uint())))
	case "u16":
		return udecimal(uint64(uint16(r.Uint())))
	case "u32":
		return udecimal(uint64(uint32(r.Uint())))
	case "u64":
		return udecimal(r.Uint())
	case "f32":
		return f32bits(r.Float32())
	case "f64":
		return f64bits(r.Float64())
	case "str":
		return r.Str()
	case "bytes":
		return base64.StdEncoding.EncodeToString(r.Bytes())
	case "i8s":
		return decimals(compact.GetInts[int8](r))
	case "i16s":
		return decimals(compact.GetInts[int16](r))
	case "i32s":
		return decimals(compact.GetInts[int32](r))
	case "i64s":
		return decimals(compact.GetInts[int64](r))
	case "u16s":
		return uintsFrom(compact.GetInts[int16](r), 16)
	case "u32s":
		return uintsFrom(compact.GetInts[int32](r), 32)
	case "u64s":
		return uintsFrom(compact.GetInts[int64](r), 64)
	case "bools":
		return boolsOf(r.Bools())
	case "strs":
		return stringsOf(r.Strs())
	case "f32s":
		out := []any{}
		for _, v := range r.Float32s() {
			out = append(out, f32bits(v))
		}
		return out
	case "f64s":
		out := []any{}
		for _, v := range r.Float64s() {
			out = append(out, f64bits(v))
		}
		return out
	}
	panic("unknown kind " + kind)
}

// standardRecords decodes the message with colbin.Unmarshal and reads the values
// back off the resulting Go value. Every field is a column in this layout, so
// every field is present in every record.
func standardRecords(name string, data []byte, original any, fields []fieldOut) [][]fieldValue {
	t := reflect.TypeOf(original)
	dst := reflect.New(t)
	if err := colbin.Unmarshal(data, dst.Interface()); err != nil {
		panic(fmt.Sprintf("%s: %v", name, err))
	}
	// The corpus is only as good as its round trip: a message the Go decoder
	// cannot read back to the value that produced it is a bug in the generator,
	// and it would be blamed on the port.
	if !reflect.DeepEqual(normalize(dst.Elem().Interface()), normalize(original)) {
		panic(fmt.Sprintf("%s: round trip disagrees with the input", name))
	}

	rows := dst.Elem()
	if t.Kind() != reflect.Slice {
		single := reflect.MakeSlice(reflect.SliceOf(t), 1, 1)
		single.Index(0).Set(rows)
		rows = single
	}
	out := make([][]fieldValue, 0, rows.Len())
	for i := range rows.Len() {
		record := []fieldValue{}
		row := rows.Index(i)
		for j, f := range fields {
			record = append(record, fieldValue{ID: f.ID, V: goValue(row.Field(fieldIndex(t, j)), f.Kind)})
		}
		out = append(out, record)
	}
	return out
}

// fieldIndex maps the j-th encoded field back to its struct field index. The
// corpus types encode every exported field, so the two agree; the lookup is here
// so that adding a cb:"-" field later fails loudly rather than silently shifting
// every value by one.
func fieldIndex(t reflect.Type, j int) int {
	if t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	seen := 0
	for i := range t.NumField() {
		if t.Field(i).Tag.Get("cb") == "-" || t.Field(i).PkgPath != "" {
			continue
		}
		if seen == j {
			return i
		}
		seen++
	}
	panic("no struct field for encoded field " + strconv.Itoa(j))
}

// goValue renders one decoded Go value in the corpus's tagged form.
func goValue(v reflect.Value, kind string) any {
	switch kind {
	case "bool":
		return v.Bool()
	case "i8", "i16", "i32", "i64":
		return decimal(v.Int())
	case "u8", "u16", "u32", "u64":
		return udecimal(v.Uint())
	case "f32":
		return f32bits(float32(v.Float()))
	case "f64":
		return f64bits(v.Float())
	case "str":
		return v.String()
	case "bytes":
		return base64.StdEncoding.EncodeToString(v.Bytes())
	case "bools":
		out := []any{}
		for i := range v.Len() {
			out = append(out, v.Index(i).Bool())
		}
		return out
	case "strs":
		out := []any{}
		for i := range v.Len() {
			out = append(out, v.Index(i).String())
		}
		return out
	case "f32s":
		out := []any{}
		for i := range v.Len() {
			out = append(out, f32bits(float32(v.Index(i).Float())))
		}
		return out
	case "f64s":
		out := []any{}
		for i := range v.Len() {
			out = append(out, f64bits(v.Index(i).Float()))
		}
		return out
	case "i8s", "i16s", "i32s", "i64s":
		out := []any{}
		for i := range v.Len() {
			out = append(out, decimal(v.Index(i).Int()))
		}
		return out
	case "u16s", "u32s", "u64s":
		out := []any{}
		for i := range v.Len() {
			out = append(out, udecimal(v.Index(i).Uint()))
		}
		return out
	}
	panic("unknown kind " + kind)
}

// normalize makes a nil slice and an empty one compare equal, which is the one
// distinction the columnar layout does not carry: a length of zero is a length
// of zero, and the decoder leaves the field nil either way.
func normalize(v any) any {
	rv := reflect.ValueOf(v)
	out := reflect.New(rv.Type()).Elem()
	out.Set(rv)
	var walk func(reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Slice:
			if v.Len() == 0 {
				v.Set(reflect.Zero(v.Type()))
				return
			}
			for i := range v.Len() {
				walk(v.Index(i))
			}
		case reflect.Struct:
			for i := range v.NumField() {
				if v.Type().Field(i).PkgPath == "" {
					walk(v.Field(i))
				}
			}
		}
	}
	walk(out)
	return out.Interface()
}

// --- alignment ---------------------------------------------------------------

// alignmentOf replays a record through the real compact.Writer and reports
// whether the string field's packed5 frame starts on a byte boundary. The writer
// is what decides the layout, so this measures the property rather than
// recomputing the bit arithmetic that produced it.
func alignmentOf(value any) bool {
	rv := reflect.ValueOf(value)
	fields := fieldsOf(rv.Type())
	w := compact.NewWriter(nil, compact.ShapeStruct, true, compact.Keys8)
	for i, f := range fields {
		v := rv.Field(i)
		switch f.Kind {
		case "bool":
			if v.Bool() {
				w.Key(uint8(f.ID))
				w.Bool(true)
			}
		case "str":
			w.Key(uint8(f.ID))
			aligned := w.Bits()%8 == 0
			w.Str(v.String())
			w.End()
			// The writer must have produced exactly what Marshal did, or the
			// measurement describes a message nobody will decode.
			built := w.Done()
			data, err := colbin.Marshal(value)
			if err != nil {
				panic(err)
			}
			if !bytes.Equal(built, data) {
				panic(fmt.Sprintf("alignment probe built %x, Marshal wrote %x", built, data))
			}
			return aligned
		default:
			panic("alignmentOf handles only bool and string fields")
		}
	}
	panic("alignmentOf found no string field")
}

func requireAlignment(name string, got, want bool) {
	if got != want {
		panic(fmt.Sprintf("%s: string frame aligned=%v, wanted %v", name, got, want))
	}
}

// --- field ids ---------------------------------------------------------------

type nameSpec struct {
	name string
	id   int // -1 for a hashed field
}

func fieldIDCases() []fieldIDCase {
	sets := []struct {
		name  string
		specs []nameSpec
	}{
		{"products", hashed("id", "sku", "name", "city", "price", "stock", "active", "weight")},
		{"single", hashed("id")},
		{"reordered", hashed("weight", "active", "stock", "price", "city", "name", "sku", "id")},
		{"unicode-names", hashed("año", "niño", "precio_€", "日本語")},
		{"collision-pair", hashed(collidingNames(2)...)},
		{"collision-triple", hashed(collidingNames(3)...)},
		// Explicit ids are reserved before any name is hashed, so a hashed field
		// whose slot is taken probes past it.
		{"explicit-ids", []nameSpec{{"a", 1}, {"b", 2}, {"c", -1}, {"d", 14}, {"e", -1}}},
		// A tagged id that lands exactly where a hashed one wanted to go.
		{"explicit-collides-with-hash", explicitOnCollision()},
	}
	// A struct at the 254-field ceiling, where probing has almost no free slots
	// left and the wrap past 255 happens for real.
	wide := make([]nameSpec, 254)
	for i := range wide {
		wide[i] = nameSpec{fmt.Sprintf("f%03d", i), -1}
	}
	sets = append(sets, struct {
		name  string
		specs []nameSpec
	}{"max-fields-254", wide})

	out := make([]fieldIDCase, 0, len(sets))
	for _, set := range sets {
		fields := fieldsOf(structFor(set.specs))
		for i := range fields {
			if fields[i].Name != set.specs[i].name {
				panic(fmt.Sprintf("%s: field %d is %q, expected %q", set.name, i, fields[i].Name, set.specs[i].name))
			}
		}
		out = append(out, fieldIDCase{Name: set.name, Fields: fields})
	}
	return out
}

func hashed(names ...string) []nameSpec {
	out := make([]nameSpec, len(names))
	for i, n := range names {
		out[i] = nameSpec{n, -1}
	}
	return out
}

// explicitOnCollision pins a field to the id another field's hash lands on, so
// the corpus contains a probe caused by a tag rather than by a hash collision.
func explicitOnCollision() []nameSpec {
	taken := int(hashOf("stock"))
	return []nameSpec{{"pinned", taken}, {"stock", -1}, {"price", -1}}
}

// hashOf reads the id colbin gives a lone field of that name, which for a
// one-field struct is the unprobed hash.
func hashOf(name string) uint8 {
	return uint8(fieldsOf(structFor([]nameSpec{{name, -1}}))[0].ID)
}

func structFor(specs []nameSpec) reflect.Type {
	fields := make([]reflect.StructField, len(specs))
	for i, spec := range specs {
		tag := fmt.Sprintf("cb:%q", spec.name)
		if spec.id >= 0 {
			tag = fmt.Sprintf("cb:%q", fmt.Sprintf("%s,%d", spec.name, spec.id))
		}
		fields[i] = reflect.StructField{
			Name: fmt.Sprintf("F%d", i),
			Type: reflect.TypeOf(int64(0)),
			Tag:  reflect.StructTag(tag),
		}
	}
	return reflect.StructOf(fields)
}

// collidingNames finds field names whose FNV-1a-32 folds to the same 8 bits, so
// the corpus exercises linear probing rather than assuming it is never reached.
func collidingNames(count int) []string {
	buckets := map[uint8][]string{}
	letters := "abcdefghijklmnopqrstuvwxyz"
	for i := range len(letters) {
		for j := range len(letters) {
			for k := range len(letters) {
				s := string(letters[i]) + string(letters[j]) + string(letters[k])
				h := fold(s)
				buckets[h] = append(buckets[h], s)
				if len(buckets[h]) == count {
					return buckets[h]
				}
			}
		}
	}
	panic("no collision found")
}

// fold is FNV-1a-32 xor-folded to 8 bits. It is here only to *search* for
// colliding names; every id in the corpus is read back out of a message the
// colbin package wrote.
func fold(s string) uint8 {
	h := uint32(2166136261)
	for i := range len(s) {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return uint8(h ^ (h >> 8) ^ (h >> 16) ^ (h >> 24))
}

// fieldsOf marshals one record of t in JSON mode and reads the ids and names out
// of the schema section:
//
//	[0x04][schemaLen][flags][structCount][fieldCount]([id] packed5(name) desc)*
//
// The ids therefore come from the colbin package itself rather than from a
// second implementation of the hash.
func fieldIDsOf(t reflect.Type) []fieldOut {
	rows := reflect.MakeSlice(reflect.SliceOf(t), 1, 1)
	data, err := colbin.MarshalJSON(rows.Interface())
	if err != nil {
		panic(err)
	}
	if data[0] != 0x04 && data[0] != 0x08 {
		panic("expected a JSON-mode message")
	}
	pos := 1
	_, n := binary.Uvarint(data[pos:]) // schema length
	pos += n
	pos++                             // flags
	_, n = binary.Uvarint(data[pos:]) // struct count
	pos += n

	count := int(data[pos])
	pos++
	out := make([]fieldOut, 0, count)
	for range count {
		id := int(data[pos])
		pos++
		name, used, err := packed5.Decode(data[pos:])
		if err != nil {
			panic(err)
		}
		pos += used
		pos = skipDesc(data, pos)
		out = append(out, fieldOut{Name: name, ID: id})
	}
	// The cb:"N" tags come from the Go type, which is where they live; the schema
	// section does not carry them in a form worth re-deriving.
	for i, sf := range encodableFields(t) {
		if id, ok := explicitID(sf); ok {
			out[i].Explicit = &id
		}
	}
	return out
}

// fieldsOf is fieldIDsOf with each field's flat kind name attached, which is what
// the original corpus records. It panics on a composite type by way of kindName;
// the composite corpus uses fieldIDsOf and builds a kind tree instead.
func fieldsOf(t reflect.Type) []fieldOut {
	out := fieldIDsOf(t)
	for i, sf := range encodableFields(t) {
		out[i].Kind = kindName(sf.Type)
	}
	return out
}

func encodableFields(t reflect.Type) []reflect.StructField {
	out := []reflect.StructField{}
	for i := range t.NumField() {
		sf := t.Field(i)
		if sf.PkgPath != "" || sf.Tag.Get("cb") == "-" {
			continue
		}
		out = append(out, sf)
	}
	return out
}

// explicitID reads the integer token of a cb tag, which is the id the field is
// pinned to.
func explicitID(sf reflect.StructField) (int, bool) {
	for _, token := range strings.Split(sf.Tag.Get("cb"), ",") {
		if id, err := strconv.Atoi(token); err == nil {
			return id, true
		}
	}
	return 0, false
}

// skipDesc steps over one schema descriptor, following the layout codec/schema.go
// documents. ftAny is the one class left out: nothing here can produce it, and it
// is the one form compact mode has no representation for at all.
func skipDesc(buf []byte, pos int) int {
	flags := buf[pos]
	pos++
	switch flags & 0x07 {
	case 0, 1: // ftInt, ftFloat: one scalar-kind byte
		return pos + 1
	case 2, 3: // ftString, ftBytes: nothing
		return pos
	case 4: // ftArray: the element descriptor
		return skipDesc(buf, pos)
	case 5: // ftStruct: a uvarint index into the schema's struct table
		_, n := binary.Uvarint(buf[pos:])
		return pos + n
	case 6: // ftMap: the key descriptor, then the value's
		return skipDesc(buf, skipDesc(buf, pos))
	}
	panic("unsupported descriptor class")
}

func kindName(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Bool:
		return "bool"
	case reflect.Int8:
		return "i8"
	case reflect.Int16:
		return "i16"
	case reflect.Int32:
		return "i32"
	case reflect.Int64:
		return "i64"
	case reflect.Uint8:
		return "u8"
	case reflect.Uint16:
		return "u16"
	case reflect.Uint32:
		return "u32"
	case reflect.Uint64:
		return "u64"
	case reflect.Float32:
		return "f32"
	case reflect.Float64:
		return "f64"
	case reflect.String:
		return "str"
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return "bytes"
		}
		return kindName(t.Elem()) + "s"
	}
	panic("unsupported type " + t.String())
}

// --- value rendering ---------------------------------------------------------
//
// Integers travel as decimal strings because a JSON number cannot hold the whole
// of int64 or uint64, and floats travel as their IEEE bit patterns because a
// decimal rendering is a lossy description of the bytes actually on the wire.

func decimal(v int64) any   { return strconv.FormatInt(v, 10) }
func udecimal(v uint64) any { return strconv.FormatUint(v, 10) }
func f32bits(v float32) any { return hexBits(uint64(math.Float32bits(v)), 4) }
func f64bits(v float64) any { return hexBits(math.Float64bits(v), 8) }

func hexBits(v uint64, width int) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)
	return hex.EncodeToString(buf[8-width:])
}

func decimals[T ~int8 | ~int16 | ~int32 | ~int64](vals []T) any {
	out := []any{}
	for _, v := range vals {
		out = append(out, decimal(int64(v)))
	}
	return out
}

// uintsFrom reinterprets the signed values an unsigned slice was carried as.
func uintsFrom[T ~int16 | ~int32 | ~int64](vals []T, width uint) any {
	out := []any{}
	mask := uint64(1)<<width - 1
	if width == 64 {
		mask = math.MaxUint64
	}
	for _, v := range vals {
		out = append(out, udecimal(uint64(int64(v))&mask))
	}
	return out
}

func boolsOf(vals []bool) any {
	out := []any{}
	for _, v := range vals {
		out = append(out, v)
	}
	return out
}

func stringsOf(vals []string) any {
	out := []any{}
	for _, v := range vals {
		out = append(out, v)
	}
	return out
}

// --- coverage ----------------------------------------------------------------

// reportCoverage fails the build if the corpus misses part of the format. The
// inputs are chosen to provoke each shape, but "chosen to provoke" is a belief
// until the header says so.
func reportCoverage(cases []caseOut) {
	modes := map[string]int{}
	shapes := map[int]int{}
	versions := map[int]int{}
	narrow := map[bool]int{}
	positive := map[bool]int{}
	kinds := map[string]int{}
	for _, c := range cases {
		modes[c.Mode]++
		if c.Mode == "compact" {
			shapes[c.Shape]++
			narrow[c.NarrowKeys]++
			positive[c.AllPositive]++
		} else {
			versions[c.Version]++
		}
		for _, f := range c.Fields {
			kinds[f.Kind]++
		}
	}
	fmt.Printf("modes: %v\nshapes: %v\nversions: %v\nnarrow keys: %v\nall positive: %v\n",
		modes, shapes, versions, narrow, positive)

	missing := []string{}
	for _, mode := range []string{"compact", "standard"} {
		if modes[mode] == 0 {
			missing = append(missing, "mode="+mode)
		}
	}
	for shape := range 4 {
		if shapes[shape] == 0 {
			missing = append(missing, "shape="+strconv.Itoa(shape))
		}
	}
	for _, version := range []int{0x02, 0x06} {
		if versions[version] == 0 {
			missing = append(missing, fmt.Sprintf("version=0x%02x", version))
		}
	}
	if narrow[true] == 0 || narrow[false] == 0 {
		missing = append(missing, "narrowKeys=both")
	}
	if positive[true] == 0 || positive[false] == 0 {
		missing = append(missing, "allPositive=both")
	}
	for _, kind := range []string{
		"bool", "i8", "i16", "i32", "i64", "u8", "u16", "u32", "u64", "f32", "f64",
		"str", "bytes", "i8s", "i16s", "i32s", "i64s", "u16s", "u32s", "u64s",
		"bools", "strs", "f32s", "f64s",
	} {
		if kinds[kind] == 0 {
			missing = append(missing, "kind="+kind)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "\nCOVERAGE GAP: %v\n", missing)
		os.Exit(1)
	}
}

// everyStringIsRaw reports whether every string reachable in value goes out as a
// raw packed5 frame -- header bit 0 clear -- which is the frame the Rust encoder
// writes. Go picks the cheaper of raw and packed per string, and raw wins on
// anything short, so the two encoders agree byte for byte far more often than
// "Rust does not pack" suggests. Where they disagree, this says so rather than
// leaving the encoder test to guess.
func everyStringIsRaw(value any) bool {
	raw := true
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		if !raw || !v.IsValid() {
			return
		}
		switch v.Kind() {
		case reflect.String:
			if frame := packed5.Append(nil, v.String()); len(frame) > 0 && frame[0]&1 == 1 {
				raw = false
			}
		case reflect.Ptr, reflect.Interface:
			if !v.IsNil() {
				walk(v.Elem())
			}
		case reflect.Slice, reflect.Array:
			if v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8 {
				return // []byte is not a string column
			}
			for i := range v.Len() {
				walk(v.Index(i))
			}
		case reflect.Map:
			it := v.MapRange()
			for it.Next() {
				walk(it.Key())
				walk(it.Value())
			}
		case reflect.Struct:
			for i := range v.NumField() {
				if v.Type().Field(i).PkgPath == "" {
					walk(v.Field(i))
				}
			}
		}
	}
	walk(reflect.ValueOf(value))
	return raw
}
