// Command vectors writes the corpus the AssemblyScript port is pinned against.
//
// The Go packages are the specification. Every frame, every section and every
// message below came out of colbin itself rather than out of a table written by
// hand, because an expectation written by hand only tests the person who wrote
// it.
//
//	go run ./web/vectors      # regenerate
//	go test ./web/vectors     # fail if the committed corpus has drifted
//	cd web && bun run test    # the module, against it
//
// It is the discipline rust/vectors keeps, and for the same reason: the module
// asserts against the committed file, so without the staleness test a change to
// the Go wire would leave the port passing against a corpus that no longer
// describes anything. That is how the previous port went stale in silence.
//
// # The tiers
//
//	strings   blob framing at both key widths. REFACTOR_PLAN.md §4.2 is going
//	          to change this one, and this tier is the tripwire that says so
//	columns   the blocked column codec: every transform, every block boundary
//	types     a schema section, a message, the field ids and the JSON, per type
//	numbers   JSON number literals read exactly — the reason there is a scanner
//	          in the module at all rather than a JSON.parse on the host
//	texts     JSON string literals, with the escape rules encoding/json defines
//
// Everything is generated with Packed5 off, which is Go's default. The packed
// string encoding is on its way out (REFACTOR_PLAN.md §4) and the tier that
// pins it comes back with u5b.
//
// The inference oracle — arbitrary JSON through reflect.StructOf and back out
// as the bytes the module's encoder must match — is phase 4 and is not here.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/ivanjoz/colbin"
	"github.com/ivanjoz/colbin/column"
	"github.com/ivanjoz/colbin/corpus"
	"github.com/ivanjoz/colbin/wire"
)

// --- the types the framing tier needs ----------------------------------------
//
// The corpus covers what a real record looks like. These cover what it does
// not: every scalar at its extreme, every array element width, a pointer that
// is nil beside one that points at a zero, and one id past fifteen.

// Scalars is every scalar the format carries, so that one message exercises
// every integer width, both float widths, a string and a blob.
type Scalars struct {
	Flag   bool    `cb:"0"`
	Tiny   int8    `cb:"1"`
	Small  int16   `cb:"2"`
	Medium int32   `cb:"3"`
	Large  int64   `cb:"4"`
	Byte   uint8   `cb:"5"`
	Half   uint16  `cb:"6"`
	Word   uint32  `cb:"7"`
	Giant  uint64  `cb:"8"`
	Single float32 `cb:"9"`
	Double float64 `cb:"10"`
	Text   string  `cb:"11"`
	Blob   []byte  `cb:"12"`
}

// Arrays is the VEC class at every element width, plus the string array, which
// is a different shape again.
type Arrays struct {
	Tiny     []int8   `cb:"0"`
	Small    []int16  `cb:"1"`
	Medium   []int32  `cb:"2"`
	Large    []int64  `cb:"3"`
	Unsigned []uint32 `cb:"4"`
	Texts    []string `cb:"5"`
}

// Optionals is the one value in the format written solely to say it is there: a
// nil pointer costs nothing, and a pointer to a zero writes an explicit zero.
type Optionals struct {
	Name  *string  `cb:"0"`
	Count *int32   `cb:"1"`
	Ratio *float64 `cb:"2"`
	Flag  *bool    `cb:"3"`
}

// Line and Order are the two composite shapes a slice of structs takes: a LIST
// while there are few of them, a TABLE once there are enough to amortise the
// per-column framing.
type Line struct {
	SKU   string `cb:"0"`
	Qty   uint16 `cb:"1"`
	Cents int64  `cb:"2"`
}

type Order struct {
	ID    uint32 `cb:"0"`
	Note  string `cb:"1"`
	Lines []Line `cb:"2"`
}

// Cell is Line with no string in it, which is what lets a slice of it be
// transposed: one string field would disqualify the whole table.
type Cell struct {
	ProductID uint32 `cb:"0"`
	Qty       uint32 `cb:"1"`
	Cents     int64  `cb:"2"`
}

type Sheet struct {
	ID    uint32 `cb:"0"`
	Cells []Cell `cb:"1"`
}

// Maps holds one entry each, deliberately. A Go map has no iteration order, so
// a map with two entries does not encode to stable bytes and cannot be a
// vector — the module still has to read them, which the decode side covers.
type Maps struct {
	Labels map[string]string `cb:"0"`
	Counts map[int32]int64   `cb:"1"`
}

// Wide has an id past fifteen, which — now that packed5 is out of the module
// (REFACTOR_PLAN.md §4.4) — is the only thing that puts a type on the eight-bit
// key path.
type Wide struct {
	First uint32 `cb:"0"`
	Text  string `cb:"1"`
	Far   int64  `cb:"200"`
}

// Nested is a struct inside a struct, which stays a key run rather than
// becoming anything columnar.
type Nested struct {
	ID    uint32 `cb:"0"`
	Inner Line   `cb:"1"`
}

// --- output ------------------------------------------------------------------

// stringFrame pins the framing around a blob, which is the part of the format
// REFACTOR_PLAN.md §4.2 moves. The payload is not recorded: it is Size copies
// of Fill, so a 64 KB case costs two fields rather than 128 KB of hex, and what
// is compared is the header alone.
type stringFrame struct {
	Name string `json:"name"`
	Key  int    `json:"key"`
	// Fill is the repeating unit of the payload, in hex; Size is the payload's
	// whole length in bytes.
	Fill string `json:"fill"`
	Size int    `json:"size"`
	// The bytes before the payload under each key width, in hex. Empty means the
	// field was not written at all, which is what an empty blob does.
	Narrow string `json:"narrow"`
	Wide   string `json:"wide"`
}

// columnCase is one column of the blocked codec on its own rather than inside a
// table. The transform search, the per-block widths and the element width taken
// from the Go type are where two ports have the most to disagree about, and a
// table only exercises the shapes its rows happen to have.
type columnCase struct {
	Name string `json:"name"`
	// The element width in bytes. It comes from the Go type and never reaches
	// the wire, so the module must be told to decode at the same one.
	Width int `json:"width"`
	// The values as decimal *strings*, which is not fussiness.
	//
	// A column is int64 and this file is read back by JSON.parse, which turns
	// 9223372036854775807 into 9223372036854775808. Writing them as numbers made
	// the corpus readable only by a parser that corrupts it — and an exact
	// integer column is the thing the module exists to get right, so the one
	// place that must not be wrong is the expectation it is checked against.
	Values []string `json:"values"`
	// What the transform search picked, by name, so a failure says which one
	// rather than only that the bytes differ.
	Transform string `json:"transform"`
	Encoded   string `json:"encoded"`
}

// typeCase is one value, end to end: what the schema section says about its
// type, what the message holds, and what a reader with the section and no Go
// type gets back out.
type typeCase struct {
	Name  string `json:"name"`
	About string `json:"about"`
	// Wide is the root byte's key-width bit, hoisted out so a failure reads as
	// "the module chose the other width" rather than as a hex diff.
	Wide bool `json:"wide"`
	// IDs is what colbin.FieldIDs resolved, read back out of the codec rather
	// than restated here.
	IDs     map[string]uint8 `json:"ids"`
	Section string           `json:"section"`
	Message string           `json:"message"`
	// SelfDescribing is the same value with its section in front of it, which is
	// the other of the two deliveries and a different root byte.
	SelfDescribing string `json:"selfDescribing"`
	// JSON is colbin.ToJSON on the section and the message: what the module's
	// decoder must produce, byte for byte.
	JSON string `json:"json"`
	// Refused is how many single-byte corruptions of Message the *Go* decoder
	// answers with an error, out of Corruptions tried.
	//
	// It is here to turn "the port must not do materially worse" into a number.
	// The guarantee either decoder offers is only safety — PLAN.md §4.3 measured
	// that over half of all corruptions decode to well-formed, wrong output, and
	// no amount of bounds checking changes that, because a flipped bit inside a
	// magnitude is simply a different number. What *can* be checked is that the
	// port refuses the ones Go refuses, and this is the baseline it is checked
	// against rather than a figure taken from a different format.
	Refused     int `json:"refused"`
	Corruptions int `json:"corruptions"`
}

// numberCase pins how one JSON number literal must be read. The rule is
// PLAN.md §3.2: a literal with no '.' and no exponent is an integer and must
// land in int64 or uint64 with every digit intact; anything else is a real
// number and becomes a float64, which must be finite.
type numberCase struct {
	Name    string `json:"name"`
	Literal string `json:"literal"`
	Kind    string `json:"kind"`  // int | uint | float | error
	Value   string `json:"value"` // decimal for int/uint; "" for float
	Bits    string `json:"bits"`  // float64 bits as hex; "" for int/uint
}

// textCase pins how one JSON string literal decodes. encoding/json is the
// reference for the escape rules, including the lone surrogate that PLAN.md
// §4.5 defines as U+FFFD.
type textCase struct {
	Name    string `json:"name"`
	Literal string `json:"literal"` // the JSON text, quotes included
	Decoded string `json:"decoded"` // base64 of the decoded bytes
}

type vectors struct {
	Strings []stringFrame `json:"strings"`
	Columns []columnCase  `json:"columns"`
	Types   []typeCase    `json:"types"`
	Numbers []numberCase  `json:"numbers"`
	Texts   []textCase    `json:"texts"`
}

func main() {
	out := "web/vectors/vectors.json"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	body, err := json.MarshalIndent(build(), "", "  ")
	if err != nil {
		panic(err)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(out, append(body, '\n'), 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("wrote %s\n", out)
}

func build() vectors {
	// A writer setting, and this corpus is the specification for a port that
	// does not implement it. Set it rather than assume the default.
	colbin.SetPacked5(false)
	return vectors{
		Strings: stringFrames(),
		Columns: columns(),
		Types:   types(),
		Numbers: numberCases(),
		Texts:   textCases(),
	}
}

// --- tier 1: blob framing ----------------------------------------------------

func stringFrames() []stringFrame {
	var out []stringFrame

	add := func(name string, key uint8, fill []byte, size int) {
		payload := repeatBytes(fill, size)
		one := stringFrame{
			Name: name,
			Key:  int(key),
			Fill: hex.EncodeToString(fill),
			Size: size,
		}
		// A narrow key above fifteen has no frame at all. The writer checks
		// nothing — a key is a constant of the record definition rather than
		// data — so it shifts into the descriptor's bits and writes a record
		// nothing can read back. Recording that would be teaching the port to
		// reproduce garbage, so the tier says "no narrow form" instead.
		if int(key) < wire.MaxFields {
			narrow := wire.Writer{}
			narrow.Bytes(key, payload)
			one.Narrow = hex.EncodeToString(headerOf(narrow.Buffer, size))
		}
		wide := wire.Writer8{}
		wide.Bytes(key, payload)
		one.Wide = hex.EncodeToString(headerOf(wide.Buffer, size))
		out = append(out, one)
	}

	ascii := []byte("A")
	// The sizes where the header changes shape. 2047 is what K4's eleven inline
	// bits hold; 255 and 65535 are where K8's length width steps up.
	for _, size := range []int{0, 1, 2, 7, 30, 31, 127, 128, 254, 255, 256, 2046, 2047, 2048, 65535, 65536} {
		add(fmt.Sprintf("ascii-%d", size), 3, ascii, size)
	}
	// Every key that changes the framing: the first, the last a nibble holds,
	// and two only a byte can.
	for _, key := range []uint8{0, 15, 16, 200, 255} {
		add(fmt.Sprintf("key-%d", key), key, ascii, 5)
	}
	// Content the framing must not care about, which is the point of recording
	// it as bytes rather than as a string.
	add("utf8-accented", 1, []byte("ñ"), 20)
	add("utf8-cjk", 1, []byte("日"), 30)
	add("invalid-utf8", 1, []byte{0xFF, 0xFE}, 8)
	add("nul-bytes", 1, []byte{0x00}, 12)
	add("high-bytes", 1, []byte{0x80, 0xBF}, 16)
	return out
}

// headerOf is the frame minus its payload: what the descriptor and the length
// cost, which is the whole of what this tier is pinning.
func headerOf(frame []byte, payload int) []byte {
	if len(frame) < payload {
		panic(fmt.Sprintf("frame of %d bytes cannot hold a %d-byte payload", len(frame), payload))
	}
	return frame[:len(frame)-payload]
}

func repeatBytes(unit []byte, size int) []byte {
	if size == 0 || len(unit) == 0 {
		return nil
	}
	out := make([]byte, 0, size)
	for len(out) < size {
		out = append(out, unit...)
	}
	return out[:size]
}

// --- tier 2: the column codec ------------------------------------------------

// transformNames are the header's low two bits, for the report rather than for
// the wire.
var transformNames = [4]string{"raw", "delta", "frame-of-reference", "constant"}

func columns() []columnCase {
	var out []columnCase

	add := func(name string, width int, values []int64) {
		var encoded []byte
		switch width {
		case 1:
			encoded = column.AppendArray(nil, narrow8(values))
		case 2:
			encoded = column.AppendArray(nil, narrow16(values))
		case 4:
			encoded = column.AppendArray(nil, narrow32(values))
		default:
			encoded = column.AppendArray(nil, values)
		}
		transform := ""
		if len(encoded) > 0 {
			transform = transformNames[encoded[0]&0b11]
		}
		decimal := make([]string, len(values))
		for index, value := range values {
			decimal[index] = strconv.FormatInt(value, 10)
		}
		out = append(out, columnCase{
			Name:      name,
			Width:     width,
			Values:    decimal,
			Transform: transform,
			Encoded:   hex.EncodeToString(encoded),
		})
	}

	// The four transforms, each on the shape it exists for.
	add("constant", 8, repeatInt(7, 40))
	add("zeros", 8, repeatInt(0, 40))
	add("monotonic-ids", 8, sequence(100000, 1, 256))
	add("timestamps", 8, scatter(1767225600, 37, 256))
	add("frame-of-reference", 8, offsets(1000000, 900, 200))
	add("random", 8, spread(200))

	// The block boundary. 128 residuals share one width byte, so a column that
	// is one short of it, exactly it, and one past it are three different
	// shapes and the third is where a partial final block appears.
	for _, count := range []int{1, 2, 127, 128, 129, 255, 256, 1000} {
		add(fmt.Sprintf("delta-%d", count), 8, sequence(1, 3, count))
	}

	// One outlier widens its own block and not the column, which is the whole
	// argument for a width per block rather than per column.
	widened := sequence(0, 1, 300)
	widened[200] = 1 << 40
	add("one-outlier", 8, widened)

	// Every element width the type parameter can be. The values are the same,
	// so a port that ignores the width produces the same bytes for all four and
	// the diff says which.
	for _, width := range []int{1, 2, 4, 8} {
		add(fmt.Sprintf("width-%d-small", width), width, []int64{0, 1, -1, 2, -2, 3})
	}
	add("width-1-extremes", 1, []int64{-128, 127, 0, -1, 1})
	add("width-2-extremes", 2, []int64{-32768, 32767, 0, -1, 1})
	add("width-4-extremes", 4, []int64{math.MinInt32, math.MaxInt32, 0, -1, 1})
	add("width-8-extremes", 8, []int64{math.MinInt64, math.MaxInt64, 0, -1, 1})

	// Negatives are what decide the zigzag, and a column with none of them must
	// not pay for one.
	add("all-negative", 8, sequence(-1000, -1, 64))
	add("straddling-zero", 8, sequence(-32, 1, 64))
	add("no-negatives", 8, sequence(0, 1, 64))

	// Widths 58..64 take the out-of-line read path, and a column that reaches
	// them is incompressible anyway — which is itself worth pinning.
	add("full-width", 8, []int64{math.MinInt64, math.MaxInt64, 1, -1, 1 << 62, -(1 << 62)})
	add("empty", 8, nil)
	return out
}

func narrow8(values []int64) []int8 {
	out := make([]int8, len(values))
	for index, value := range values {
		out[index] = int8(value)
	}
	return out
}

func narrow16(values []int64) []int16 {
	out := make([]int16, len(values))
	for index, value := range values {
		out[index] = int16(value)
	}
	return out
}

func narrow32(values []int64) []int32 {
	out := make([]int32, len(values))
	for index, value := range values {
		out[index] = int32(value)
	}
	return out
}

func repeatInt(value int64, count int) []int64 {
	out := make([]int64, count)
	for index := range out {
		out[index] = value
	}
	return out
}

func sequence(start, step int64, count int) []int64 {
	out := make([]int64, count)
	for index := range out {
		out[index] = start + step*int64(index)
	}
	return out
}

// scatter is a sequence with a little jitter on it, which is what a column of
// timestamps actually looks like.
func scatter(start, stride int64, count int) []int64 {
	out := make([]int64, count)
	random := rand.New(rand.NewSource(7))
	at := start
	for index := range out {
		at += stride + random.Int63n(5)
		out[index] = at
	}
	return out
}

// offsets is a tight range a long way from zero, which is what the frame of
// reference transform is for.
func offsets(base, span int64, count int) []int64 {
	out := make([]int64, count)
	random := rand.New(rand.NewSource(11))
	for index := range out {
		out[index] = base + random.Int63n(span)
	}
	return out
}

// spread is incompressible on purpose: the codec must not make it bigger than
// the raw elements by more than its header.
func spread(count int) []int64 {
	out := make([]int64, count)
	random := rand.New(rand.NewSource(13))
	for index := range out {
		out[index] = int64(random.Uint64())
	}
	return out
}

// --- tier 3: sections, messages and the JSON out of them ---------------------

func types() []typeCase {
	generated := corpus.Generate(corpus.Seed, corpus.Small)
	shortSale, longSale := salesAcrossTheThreshold(generated.Sales)

	return []typeCase{
		describe("scalars", "every scalar the format carries, at its extremes", &Scalars{
			Flag: true, Tiny: -128, Small: -32768, Medium: math.MinInt32,
			Large: math.MinInt64, Byte: 255, Half: 65535, Word: math.MaxUint32,
			Giant: math.MaxUint64, Single: 1.5, Double: -2.25,
			Text: "hola", Blob: []byte{0x00, 0x7F, 0x80, 0xFF},
		}),
		describe("scalars-zero", "every field at its zero, so the message is the root byte alone", &Scalars{}),
		describe("scalars-float-trim", "a float whose low mantissa bytes elide", &Scalars{
			Single: 1, Double: 1,
		}),
		describe("arrays", "the VEC class at every element width, and the string array", &Arrays{
			Tiny:     []int8{-128, 0, 127},
			Small:    []int16{-32768, 0, 32767},
			Medium:   []int32{math.MinInt32, 0, math.MaxInt32},
			Large:    []int64{math.MinInt64, 0, math.MaxInt64},
			Unsigned: []uint32{0, 1, math.MaxUint32},
			Texts:    []string{"", "a", strings.Repeat("x", 300)},
		}),
		describe("optionals-nil", "every pointer nil, which costs nothing at all", &Optionals{}),
		describe("optionals-zero", "every pointer at a zero, which is written to say it is there",
			&Optionals{
				Name:  new(string),
				Count: new(int32),
				Ratio: new(float64),
				Flag:  new(bool),
			}),
		describe("nested", "a struct inside a struct: another key run, not a sub-table", &Nested{
			ID: 9, Inner: Line{SKU: "ABC-1", Qty: 2, Cents: 1999},
		}),
		describe("list", "a slice of structs under the table threshold, so a LIST", &Order{
			ID: 1, Note: "pickup", Lines: makeLines(3),
		}),
		describe("list-one", "a one-element list, which is where an off-by-one shows", &Order{
			ID: 2, Lines: makeLines(1),
		}),
		describe("list-empty", "an empty slice, omitted like any other zero", &Order{ID: 3}),
		describe("table", "a slice of all-integer structs past the threshold, so a TABLE", &Sheet{
			ID: 4, Cells: makeCells(64),
		}),
		describe("table-at-threshold", "exactly the row count where the layout changes", &Sheet{
			ID: 5, Cells: makeCells(8),
		}),
		describe("table-below-threshold", "one row short of it, so the same type is a LIST", &Sheet{
			ID: 6, Cells: makeCells(7),
		}),
		describe("table-wide", "a table long enough for the column codec to have something to do", &Sheet{
			ID: 7, Cells: makeCells(300),
		}),
		describe("maps", "one entry each: more than one has no stable order and cannot be a vector",
			&Maps{
				Labels: map[string]string{"env": "prod"},
				Counts: map[int32]int64{7: 99},
			}),
		describe("wide", "an id past fifteen, which is the only thing left that widens a key", &Wide{
			First: 1, Text: "far", Far: -5,
		}),
		describe("corpus-user", "the ordinary flat record", &generated.Users[0]),
		describe("corpus-product", "strings, an array of them, and a price in cents", &generated.Products[0]),
		describe("corpus-store", "the only corpus table with floats in it", &generated.Stores[0]),
		describe("corpus-metric", "three integers, which is what the column codec was written for",
			&generated.Metrics[0]),
		describe("corpus-category", "three fields, two of them small integers", &generated.Categories[0]),
		describe("corpus-sale-list", "a sale whose detail is under the table threshold", shortSale),
		describe("corpus-sale-table", "a sale whose detail is transposed into columns", longSale),
	}
}

// describe runs one value through everything a reader without the Go type
// would be handed, and fails loudly rather than writing a case that does not
// hold together.
func describe(name, about string, value any) typeCase {
	ids, err := colbin.FieldIDs(value)
	if err != nil {
		panic(fmt.Sprintf("%s: field ids: %v", name, err))
	}
	schema, err := colbin.SchemaOf(value)
	if err != nil {
		panic(fmt.Sprintf("%s: schema: %v", name, err))
	}
	message, err := colbin.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("%s: marshal: %v", name, err))
	}
	standalone, err := colbin.MarshalSelfDescribing(value)
	if err != nil {
		panic(fmt.Sprintf("%s: self-describing: %v", name, err))
	}
	text, err := colbin.ToJSON(schema, message)
	if err != nil {
		panic(fmt.Sprintf("%s: to json: %v", name, err))
	}
	// The two deliveries must agree about the body, or one of them is not what
	// it claims to be.
	if inline, err := colbin.ToJSON(nil, standalone); err != nil {
		panic(fmt.Sprintf("%s: to json, self-describing: %v", name, err))
	} else if string(inline) != string(text) {
		panic(fmt.Sprintf("%s: the two deliveries disagree:\n %s\n %s", name, inline, text))
	}
	mustRoundTrip(name, value, message)
	refused, tried := countRefusals(schema, message)
	return typeCase{
		Name:           name,
		About:          about,
		Wide:           message[0]&0x08 != 0,
		IDs:            ids,
		Section:        hex.EncodeToString(schema.Bytes()),
		Message:        hex.EncodeToString(message),
		SelfDescribing: hex.EncodeToString(standalone),
		JSON:           string(text),
		Refused:        refused,
		Corruptions:    tried,
	}
}

// countRefusals flips every bit position of interest in every byte and counts
// how many of the results the Go decoder refuses.
//
// The masks are PLAN.md §4.3's: a low bit, a high bit and the whole byte. It is
// the same sweep the plan ran before the port started, run against the format
// that exists now rather than the one that did then.
func countRefusals(schema *colbin.Schema, message []byte) (refused, tried int) {
	for at := range message {
		for _, mask := range []byte{0x01, 0x80, 0xFF} {
			corrupt := append([]byte(nil), message...)
			corrupt[at] ^= mask
			tried++
			if _, err := colbin.ToJSON(schema, corrupt); err != nil {
				refused++
			}
		}
	}
	return refused, tried
}

// mustRoundTrip refuses to write a case the Go decoder itself would not accept.
//
// A vector is a specification, and a message that is not a fixed point of its
// own codec is one the port could only match by accident — so every case is
// decoded back into a fresh value of its type and re-encoded, and the bytes
// have to be the same bytes.
func mustRoundTrip(name string, value any, message []byte) {
	fresh := reflect.New(reflect.TypeOf(value).Elem()).Interface()
	if err := colbin.Unmarshal(message, fresh); err != nil {
		panic(fmt.Sprintf("%s: unmarshal: %v", name, err))
	}
	again, err := colbin.Marshal(fresh)
	if err != nil {
		panic(fmt.Sprintf("%s: re-marshal: %v", name, err))
	}
	if !bytes.Equal(again, message) {
		panic(fmt.Sprintf("%s: re-encoded differently\n have %x\n want %x", name, again, message))
	}
}

// salesAcrossTheThreshold finds one sale of each layout. The corpus straddles
// the threshold on purpose, so both are there; searching for them rather than
// indexing means the generator survives a change to the distribution.
func salesAcrossTheThreshold(sales []corpus.Sale) (short, long *corpus.Sale) {
	const threshold = 8 // codec.tableThreshold, which is not exported
	for index := range sales {
		sale := &sales[index]
		if short == nil && len(sale.Detail) > 0 && len(sale.Detail) < threshold {
			short = sale
		}
		if long == nil && len(sale.Detail) >= threshold {
			long = sale
		}
		if short != nil && long != nil {
			return short, long
		}
	}
	panic("the corpus no longer holds a sale of each layout")
}

func makeLines(count int) []Line {
	out := make([]Line, count)
	for index := range out {
		out[index] = Line{
			SKU:   fmt.Sprintf("SKU-%04d", index),
			Qty:   uint16(index + 1),
			Cents: int64(199 + index*37),
		}
	}
	return out
}

func makeCells(count int) []Cell {
	out := make([]Cell, count)
	for index := range out {
		out[index] = Cell{
			ProductID: uint32(1000 + index),
			Qty:       uint32(index%7 + 1),
			Cents:     int64(499 + index*13),
		}
	}
	return out
}

// --- tier 4: the JSON scanner ------------------------------------------------
//
// Neither of these two tiers is about the wire. They pin assembly/json.ts and
// assembly/decimal.ts, which the format change does not touch — and which are
// the reason the module parses JSON itself instead of taking an object graph
// from the host, since JSON.parse turns 7295013456321098765 into
// 7295013456321098800 before the codec ever sees it.

func classifyNumber(literal string) numberCase {
	one := numberCase{Literal: literal}
	if !strings.ContainsAny(literal, ".eE") {
		if value, err := strconv.ParseInt(literal, 10, 64); err == nil {
			one.Kind, one.Value = "int", strconv.FormatInt(value, 10)
			return one
		}
		if value, err := strconv.ParseUint(literal, 10, 64); err == nil {
			one.Kind, one.Value = "uint", strconv.FormatUint(value, 10)
			return one
		}
		one.Kind = "error"
		return one
	}
	value, err := strconv.ParseFloat(literal, 64)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
		one.Kind = "error"
		return one
	}
	one.Kind = "float"
	one.Bits = fmt.Sprintf("%016x", math.Float64bits(value))
	return one
}

func numberCases() []numberCase {
	literals := []struct{ name, literal string }{
		{"zero", "0"},
		{"neg-zero", "-0"},
		{"one", "1"},
		{"neg-one", "-1"},
		{"small", "42"},
		{"int64-max", "9223372036854775807"},
		{"int64-min", "-9223372036854775808"},
		{"uint64-just-past-int64", "9223372036854775808"},
		{"uint64-max", "18446744073709551615"},
		{"past-uint64", "18446744073709551616"},
		{"way-past-uint64", "123456789012345678901234567890"},
		// The case JSON.parse gets wrong, and the reason the parser is in wasm.
		{"snowflake", "7295013456321098765"},
		{"beyond-2p53", "9007199254740993"},
		{"float-simple", "1.5"},
		{"float-tenth", "0.1"},
		{"float-third", "0.3333333333333333"},
		{"float-neg", "-2.25"},
		{"float-exp", "1e10"},
		{"float-exp-plus", "1E+10"},
		{"float-exp-neg", "1e-10"},
		{"float-big-exp", "1e30"},
		{"float-tiny", "5e-324"},
		{"float-max", "1.7976931348623157e308"},
		{"float-overflow", "1e400"},
		{"float-underflow", "1e-400"},
		{"float-many-digits", "3.141592653589793238462643383279"},
		{"float-19-digits", "1234567890123456789.0"},
		{"float-leading-zeros", "0.000001"},
		{"float-trailing-zeros", "1.100000"},
		{"float-exp-boundary-22", "1e22"},
		{"float-exp-boundary-23", "1e23"},
		{"float-neg-exp-22", "1e-22"},
		{"float-neg-exp-23", "1e-23"},
		{"float-mant-2p53", "9007199254740992.0"},
		{"float-mant-past-2p53", "9007199254740993.0"},
		{"float-hard-rounding", "2.2250738585072011e-308"},
		{"float-half-even", "1.0000000000000002"},
	}
	out := make([]numberCase, 0, len(literals)+600)
	for _, one := range literals {
		held := classifyNumber(one.literal)
		held.Name = one.name
		out = append(out, held)
	}

	// The literals above test the boundaries someone thought of. These test the
	// ones nobody did: random float64s rendered several ways, each of which must
	// read back to the identical bit pattern. 'e' at 17 and 20 digits is past
	// shortest-round-trip, so it exercises the exact path rather than the fast
	// one.
	random := rand.New(rand.NewSource(20260826))
	for index := 0; index < 150; index++ {
		value := math.Float64frombits(random.Uint64())
		if math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		for _, format := range []struct{ tag, text string }{
			{"shortest", strconv.FormatFloat(value, 'g', -1, 64)},
			{"e17", strconv.FormatFloat(value, 'e', 17, 64)},
			{"e20", strconv.FormatFloat(value, 'e', 20, 64)},
		} {
			// A rendering with no '.' and no exponent is read as an integer by
			// the rule above, which is a different test.
			if !strings.ContainsAny(format.text, ".eE") {
				continue
			}
			held := classifyNumber(format.text)
			held.Name = fmt.Sprintf("random-%s-%d", format.tag, index)
			out = append(out, held)
		}
	}

	// Subnormals and the boundaries around them, where a doubly rounded
	// conversion goes wrong and nothing else notices.
	for index := 0; index < 40; index++ {
		value := math.Float64frombits(random.Uint64() % (1 << 52))
		held := classifyNumber(strconv.FormatFloat(value, 'e', 20, 64))
		held.Name = fmt.Sprintf("subnormal-%d", index)
		out = append(out, held)
	}
	return out
}

func textCases() []textCase {
	literals := []struct{ name, literal string }{
		{"empty", `""`},
		{"plain", `"hello"`},
		{"spaces", `"hello world"`},
		{"quote", `"say \"hi\""`},
		{"backslash", `"a\\b"`},
		{"slash", `"a\/b"`},
		{"control-escapes", `"\b\f\n\r\t"`},
		{"unicode-basic", `"\u0041\u0042"`},
		{"unicode-accent", `"ni\u00f1o"`},
		{"unicode-euro", `"\u20ac"`},
		{"surrogate-pair", `"\ud83c\udf89"`},
		{"lone-high-surrogate", `"\ud800"`},
		{"lone-low-surrogate", `"\udc00"`},
		{"high-then-plain", `"\ud800a"`},
		{"raw-utf8", `"el niño comió jamón"`},
		{"raw-emoji", `"party 🎉 time"`},
		{"raw-cjk", `"日本語"`},
		{"nul-escape", `"a\u0000b"`},
		{"mixed", `"SKU-00123 — Ñandú"`},
	}
	out := make([]textCase, 0, len(literals))
	for _, one := range literals {
		var decoded string
		if err := json.Unmarshal([]byte(one.literal), &decoded); err != nil {
			panic(fmt.Sprintf("%s: %v", one.name, err))
		}
		out = append(out, textCase{
			Name:    one.name,
			Literal: one.literal,
			Decoded: base64.StdEncoding.EncodeToString([]byte(decoded)),
		})
	}
	return out
}
