// Command vectors writes js/vectors/vectors.json: Go values run through the Go
// codec, each recorded as its schema section, its message, its self-describing
// form and the JSON colbin.ToJSON renders from them. The npm package's tests
// (js/tests) check the wasm decoder against it byte for byte, and
// rust/tests/inspect.rs checks the span walk against the same messages.
//
// The Go packages are the specification. Every section and message below came
// out of colbin itself rather than out of a table written by hand, because an
// expectation written by hand only tests the person who wrote it.
//
//	go run ./js/vectors      # regenerate
//	go test ./js/vectors     # fail if the committed corpus has drifted
//	cd js && bun run test    # the module, against it
//
// The module asserts against the committed file, so without the staleness test
// a change to the Go wire would leave it passing against a corpus that no
// longer describes anything. rust/vectors keeps the same discipline.
//
// # The tiers
//
//	types    one value per case, with Packed5 off, which is Go's default
//	packed   the same shape with the opt-in string encoding on
//
// The other direction — the module encodes, Go decodes — is web_encoded.json,
// written by `bun run emit` in js/ and checked by encoded_test.go.
package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"

	"github.com/ivanjoz/colbin"
	"github.com/ivanjoz/colbin/corpus"
)

// --- the types ---------------------------------------------------------------
//
// The corpus covers what a real record looks like. These cover what it does
// not: every scalar at its extreme, every array element width, a pointer that
// is nil beside one that points at a zero, and one id past sixteen.

// Scalars is every scalar the format carries, so that one message exercises
// every integer width, both float widths, a string and a blob.
type Scalars struct {
	Flag   bool    `cb:"1"`
	Tiny   int8    `cb:"2"`
	Small  int16   `cb:"3"`
	Medium int32   `cb:"4"`
	Large  int64   `cb:"5"`
	Byte   uint8   `cb:"6"`
	Half   uint16  `cb:"7"`
	Word   uint32  `cb:"8"`
	Giant  uint64  `cb:"9"`
	Single float32 `cb:"10"`
	Double float64 `cb:"11"`
	Text   string  `cb:"12"`
	Blob   []byte  `cb:"13"`
}

// Arrays is the VEC class at every element width, plus the string array, which
// is a different shape again.
type Arrays struct {
	Tiny     []int8   `cb:"1"`
	Small    []int16  `cb:"2"`
	Medium   []int32  `cb:"3"`
	Large    []int64  `cb:"4"`
	Unsigned []uint32 `cb:"5"`
	Texts    []string `cb:"6"`
}

// Optionals is the one value in the format written solely to say it is there: a
// nil pointer costs nothing, and a pointer to a zero writes an explicit zero.
type Optionals struct {
	Name  *string  `cb:"1"`
	Count *int32   `cb:"2"`
	Ratio *float64 `cb:"3"`
	Flag  *bool    `cb:"4"`
}

// Line and Order are the two composite shapes a slice of structs takes: a LIST
// while there are few of them, a TABLE once there are enough to amortise the
// per-column framing.
type Line struct {
	SKU   string `cb:"1"`
	Qty   uint16 `cb:"2"`
	Cents int64  `cb:"3"`
}

type Order struct {
	ID    uint32 `cb:"1"`
	Note  string `cb:"2"`
	Lines []Line `cb:"3"`
}

// Cell is a row of integers only, so a table of it puts every column through
// the column codec.
type Cell struct {
	ProductID uint32 `cb:"1"`
	Qty       uint32 `cb:"2"`
	Cents     int64  `cb:"3"`
}

type Sheet struct {
	ID    uint32 `cb:"1"`
	Cells []Cell `cb:"2"`
}

// WideCell has an id past sixteen, so its key run is wide where Ledger's is
// narrow. A table of them under a Ledger keys its columns at eight bits — the
// rows' width, not the parent's.
type WideCell struct {
	ProductID uint32 `cb:"1"`
	Label     string `cb:"2"`
	Cents     int64  `cb:"20"`
}

type Ledger struct {
	ID    uint32     `cb:"1"`
	Cells []WideCell `cb:"2"`
}

// Paged numbers fields past 255, which splits it into pages of 255 linked under
// key 255. A reader writes every page's fields into one object.
type Paged struct {
	ID   uint32 `cb:"1"`
	Name string `cb:"2"`
	Late int64  `cb:"300"`
	Last string `cb:"600"`
}

// Maps holds one entry each. A typed map is written with its keys sorted, so
// more would encode to stable bytes too; one keeps the case about the map's
// framing rather than its ordering.
type Maps struct {
	Labels map[string]string `cb:"1"`
	Counts map[int32]int64   `cb:"2"`
}

// Catalog holds maps of structs, whose section names the value's struct by its
// index in the table, the way a slice of structs names its element. The values
// are narrow (Line) and wide (WideCell). Catalog is wide because the module
// renders a map at eight key bits only. Tags and Counts are pointers to
// collections, which are the collections on the wire.
type Catalog struct {
	Name   string             `cb:"1"`
	ByCode map[string]Line    `cb:"2"`
	Cells  map[int32]WideCell `cb:"3"`
	Tags   *[]string          `cb:"20"`
	Counts *map[string]uint16 `cb:"21"`
}

// Wide has an id past sixteen, whose key four bits cannot carry, so the type
// takes eight-bit keys. It is one of three things that do: the others are a
// field with no id in its tag, whose key is a hash of its name and lands
// anywhere in 0..255, and a dynamic (`any`) field, whose descriptor has to
// name its own class.
type Wide struct {
	First uint32 `cb:"1"`
	Text  string `cb:"2"`
	Far   int64  `cb:"201"`
}

// Packed is what the opt-in string encoding is for: short strings of letters,
// digits and Spanish accents. No id is past sixteen, so the run is narrow and a
// packed string names itself through the narrow blob header's escape codes.
type Packed struct {
	Name    string `cb:"1"`
	SKU     string `cb:"2"`
	City    string `cb:"3"`
	Note    string `cb:"4"`
	Mixed   string `cb:"5"`
	Raw     string `cb:"6"`
	Upper   string `cb:"7"`
	Numbers string `cb:"8"`
}

// PackedWide is Packed with an id past sixteen, so the same strings travel under
// eight-bit keys and through the descriptor's enc field rather than the blob
// header's escape codes.
type PackedWide struct {
	Name string `cb:"1"`
	SKU  string `cb:"2"`
	Far  string `cb:"201"`
}

// Nested is a struct inside a struct, which stays a key run rather than
// becoming anything columnar.
type Nested struct {
	ID    uint32 `cb:"1"`
	Inner Line   `cb:"2"`
}

// --- output ------------------------------------------------------------------

// typeCase is one value, end to end: what the schema section says about its
// type, what the message holds, and what a reader with the section and no Go
// type gets back out.
type typeCase struct {
	Name  string `json:"name"`
	About string `json:"about"`
	// Wide is the root byte's key-width bit, hoisted out so a failure reads as
	// "the module chose the other width" rather than as a hex diff.
	Wide bool `json:"wide"`
	// IDs are the wire keys colbin.FieldIDs resolved, read back out of the codec
	// rather than restated here: each id less one.
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
	// It turns "the port must not do materially worse" into a number. Either
	// decoder can only promise safety: most corruptions decode to well-formed,
	// wrong output, and no bounds check changes that, because a flipped bit
	// inside a magnitude is simply a different number. What can be checked is
	// that the port refuses the ones Go refuses, and this is the baseline it is
	// checked against.
	Refused     int `json:"refused"`
	Corruptions int `json:"corruptions"`
}

type vectors struct {
	Types []typeCase `json:"types"`
	// Packed is types() again with the opt-in string encoding on. Separate
	// because Packed5 is a process-wide writer setting: one list cannot hold
	// both, and the ordinary one must be what an ordinary service sends.
	Packed []typeCase `json:"packed"`
}

func main() {
	out := filepath.Join(sourceDir(), "vectors.json")
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	body, err := json.MarshalIndent(build(), "", "  ")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(out, append(body, '\n'), 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("wrote %s\n", out)
}

// sourceDir is the directory this file was compiled from, so the default output
// lands beside the generator whichever directory `go run` was started in.
func sourceDir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok || !filepath.IsAbs(file) {
		panic("vectors: cannot locate the source directory (built with -trimpath?); pass the output path")
	}
	return filepath.Dir(file)
}

func build() vectors {
	// A writer setting, and this corpus is the specification for a port that
	// does not implement it. Set it rather than assume the default.
	colbin.SetPacked5(false)
	return vectors{
		Types:  types(),
		Packed: packedTypes(),
	}
}

// --- sections, messages and the JSON out of them -----------------------------

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
		describe("table-of-wide-rows", "a narrow parent's table of wide rows, whose columns keep eight-bit keys",
			&Ledger{ID: 8, Cells: makeWideCells(20)}),
		describe("paged", "fields on three pages, merged into one object",
			&Paged{ID: 1, Name: "first", Late: -300, Last: "third"}),
		describe("paged-first-page", "the later pages empty, so not written, and their fields zeros",
			&Paged{ID: 2, Name: "only"}),
		describe("map-of-structs", "maps of narrow and wide structs, and pointers to collections",
			&Catalog{
				Name:   "store",
				ByCode: map[string]Line{"a-1": {SKU: "A1", Qty: 3, Cents: 250}, "empty": {}},
				Cells:  map[int32]WideCell{-1: {Label: "W", Cents: 1 << 40}, 7: {ProductID: 7}},
				Tags:   &[]string{"open", "late"},
				Counts: &map[string]uint16{"doors": 2},
			}),
		describe("maps", "one entry each, which keeps the case about a map's framing",
			&Maps{
				Labels: map[string]string{"env": "prod"},
				Counts: map[int32]int64{7: 99},
			}),
		describe("wide", "an id past sixteen, which four key bits cannot carry; an untagged field and an `any` field are the other two things that widen a key", &Wide{
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

// packedTypes is the same shape as types(), with the opt-in string encoding on.
//
// It is a separate list because Packed5 is a *writer* setting and a process-wide
// one: turning it on changes what every string field in the corpus would encode
// to, so the ordinary cases are generated with it off — which is Go's default
// and what an ordinary service sends — and these are generated with it on.
//
// The decoder needs no setting either way. The encoding is in each string's own
// descriptor, which is the whole point of choosing it per field.
func packedTypes() []typeCase {
	colbin.SetPacked5(true)
	defer colbin.SetPacked5(false)

	out := []typeCase{
		describe("packed-narrow", "the opt-in encoding under four-bit keys, through the blob header's escapes", &Packed{
			Name:    "Tin Light",
			SKU:     "SKU-00042",
			City:    "Arequipa",
			Note:    "el niño comió jamón",
			Mixed:   "Order #128: 3 items, 45% off",
			Raw:     "\x00\x01\xff\xfe",
			Upper:   "MEXICO CITY",
			Numbers: "1023 512 7",
		}),
		describe("packed-wide", "the same strings under eight-bit keys, through the descriptor's enc", &PackedWide{
			Name: "Steel Lamp",
			SKU:  "SKU-99999",
			Far:  "a string under a key past fifteen",
		}),
		describe("packed-long", "a payload past the one-byte size, so the four-byte escape is used", &Packed{
			Name: strings.Repeat("the quick brown fox jumps over the lazy dog ", 12),
			SKU:  strings.Repeat("ABCDEFGH", 40),
		}),
		describe("packed-unpackable", "strings the packed form would not shrink, written raw beside packed ones", &Packed{
			Name: "\x00\x01\x02\x03\x04\x05\x06\x07",
			SKU:  "Tin Light",
		}),
		describe("packed-empty", "an empty string is not written at all, packed or not", &Packed{SKU: "a"}),
		describe("packed-corpus-user", "a real record with the encoding on", &corpus.Generate(corpus.Seed, corpus.Small).Users[0]),
		describe("packed-corpus-product", "strings and an array of them, with the encoding on", &corpus.Generate(corpus.Seed, corpus.Small).Products[0]),
	}
	return out
}

// describe runs one value through everything a reader without the Go type
// would be handed, and fails loudly rather than writing a case that does not
// hold together.
func describe(name, about string, value any) typeCase {
	ids, err := colbin.FieldIDs(value)
	if err != nil {
		panic(fmt.Sprintf("%s: field ids: %v", name, err))
	}
	// The module is handed wire keys, which are the ids less one.
	keys := make(map[string]uint8, len(ids))
	for field, id := range ids {
		keys[field] = uint8(id - 1)
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
		IDs:            keys,
		Section:        hex.EncodeToString(schema.Bytes()),
		Message:        hex.EncodeToString(message),
		SelfDescribing: hex.EncodeToString(standalone),
		JSON:           string(text),
		Refused:        refused,
		Corruptions:    tried,
	}
}

// countRefusals flips every bit position of interest in every byte and counts
// how many of the results the Go decoder refuses. The masks are a low bit, a
// high bit and the whole byte.
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

func makeWideCells(count int) []WideCell {
	out := make([]WideCell, count)
	for index := range out {
		out[index] = WideCell{
			ProductID: uint32(2000 + index),
			Label:     []string{"a", "bb", "ccc"}[index%3],
			Cents:     int64(index) * 1001,
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
