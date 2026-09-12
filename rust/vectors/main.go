// Command vectors writes the corpus the Rust port is pinned against.
//
// The Go packages are the specification. Every message in the output was
// produced by colbin.Marshal on a real Go value, and every field id was read out
// of colbin.FieldIDs rather than computed here, because an expectation written
// by hand only tests the person who wrote it.
//
//	go run ./rust/vectors          # regenerate
//	go test ./rust/vectors         # fail if the committed corpus has drifted
//	cargo test -p colbin --features derive --test vectors
//
// The Rust side holds the same values in `rust/tests/vectors.rs` and asserts
// both directions: that it decodes these bytes to those values, and that it
// encodes those values to these bytes. So neither port can move without the
// other failing, which is the discipline this repository keeps and the one the
// previous corpus had silently lost.
package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/ivanjoz/colbin"
	"github.com/ivanjoz/colbin/column"
)

// --- the corpus types --------------------------------------------------------
//
// Each one is mirrored by a `#[derive(Colbin)]` struct of the same name in
// rust/tests/vectors.rs, field for field and id for id.

// Charge is the ten-field benchmark record, all ids under sixteen so the root
// picks four-bit keys.
type Charge struct {
	CompanyID uint32 `cb:"0"`
	UserID    uint32 `cb:"1"`
	RouteID   uint32 `cb:"2"`
	CPU       uint8  `cb:"3"`
	Memory    uint16 `cb:"4"`
	Duration  int64  `cb:"5"`
	Access1   uint16 `cb:"6"`
	Access2   uint16 `cb:"7"`
	Created   int64  `cb:"8"`
	Updated   int64  `cb:"9"`
}

// Scalars covers every scalar the format carries, at its extremes.
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
}

// Arrays covers every slice kind, including the blob and the string list.
type Arrays struct {
	Blob   []byte   `cb:"0"`
	Tiny   []int8   `cb:"1"`
	Small  []int16  `cb:"2"`
	Medium []int32  `cb:"3"`
	Large  []int64  `cb:"4"`
	Halves []uint16 `cb:"5"`
	Words  []uint32 `cb:"6"`
	Giants []uint64 `cb:"7"`
	Texts  []string `cb:"8"`
}

// Optionals is the pointer shape: an absent key means nil, so a pointer to a
// zero has to write the zero out loud.
type Optionals struct {
	MaybeInt   *int32   `cb:"0"`
	MaybeUint  *uint32  `cb:"1"`
	MaybeText  *string  `cb:"2"`
	MaybeFlag  *bool    `cb:"3"`
	MaybeFloat *float64 `cb:"4"`
}

// Line is a row of Order, and is transposable: every field is a column the
// column codec carries.
type Line struct {
	SKU      string  `cb:"0"`
	Quantity int32   `cb:"1"`
	Price    float64 `cb:"2"`
}

// Order holds a nested struct and a slice of them, which the writer encodes
// row-wise or transposed depending on how many there are.
type Order struct {
	ID       uint32 `cb:"0"`
	Customer Line   `cb:"1"`
	Lines    []Line `cb:"2"`
	Note     string `cb:"3"`
}

// Maps holds one entry each: Go's map iteration order is its own, so a corpus
// entry with two would not be byte-stable from one run to the next.
type Maps struct {
	Labels map[string]string  `cb:"0"`
	Counts map[int64]float64  `cb:"1"`
	Flags  map[string]bool    `cb:"2"`
	Sizes  map[uint32]float32 `cb:"3"`
}

// WideEvolved carries an id past fifteen, which puts the whole run on the
// eight-bit width — where a reader that does not know a key can step over it.
type WideEvolved struct {
	First uint32  `cb:"0"`
	Last  string  `cb:"200"`
	Added []int32 `cb:"201"`
	Also  float64 `cb:"202"`
}

// WideWidths puts a uint16 in each shape the wide writer has for one, including
// the window where the varint is shorter than the byte-count form. Go's U16
// decides that window against a constant instead of measuring both lengths —
// see wire.varintWinsToU16 — so the boundary is worth pinning across the two
// ports rather than only inside one.
type WideWidths struct {
	Inline  uint16 `cb:"20"` // the descriptor carries it
	OneByte uint16 `cb:"21"` // a one-byte magnitude
	Varint  uint16 `cb:"22"` // three bytes as a varint, four as a magnitude
	Tie     uint16 `cb:"23"` // the first value where the two forms tie
	Max     uint16 `cb:"24"` // the widest a uint16 gets
}

// Wide is WideEvolved as an older peer declares it, and is what the skip is
// demonstrated against.
type Wide struct {
	First uint32 `cb:"0"`
	Last  string `cb:"200"`
}

// Hashed numbers nothing but one field, so the rest take the hash of their
// names — which lands anywhere in 0..255 and therefore forces the wide width.
type Hashed struct {
	CompanyID int32
	User      string
	Pinned    uint32 `cb:"7"`
	Ignored   uint64 `cb:"-"`
}

// PackedText is the opt-in string encoding, which is a code in the field's own
// descriptor rather than a mode.
type PackedText struct {
	Text string `cb:"0"`
}

// --- output ------------------------------------------------------------------

type caseOut struct {
	Name string `json:"name"`
	// The Go type the value is of, which names the Rust struct that mirrors it.
	Type string `json:"type"`
	// What this case is for, so a failure reads as something other than a hex
	// diff.
	About string `json:"about"`
	// The message colbin.Marshal produced, in hex.
	Message string `json:"message"`
}

type idCase struct {
	Type string         `json:"type"`
	IDs  map[string]int `json:"ids"`
}

// columnCase is one column of the blocked codec, on its own rather than inside
// a table: the transform search, the per-block widths and the element width
// derived from the type are where the two ports have the most to disagree
// about, and a table only exercises the shapes its rows happen to have.
type columnCase struct {
	Name string `json:"name"`
	// The element width in bytes, which comes from the Go type and never
	// reaches the wire — so the Rust side must decode at the same one.
	Width  int     `json:"width"`
	Values []int64 `json:"values"`
	// What the transform search picked, as a name, so a failure says which one
	// rather than only that the bytes differ.
	Transform string `json:"transform"`
	Encoded   string `json:"encoded"`
}

type corpus struct {
	// The ids every type resolved to, so that the two ports' hash-and-probe
	// agree on a number rather than on a description of one.
	FieldIDs []idCase     `json:"fieldIds"`
	Columns  []columnCase `json:"columns"`
	Cases    []caseOut    `json:"cases"`
}

func main() {
	out := "rust/vectors/vectors.json"
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

func build() corpus {
	return corpus{FieldIDs: fieldIDs(), Columns: columns(), Cases: cases()}
}

// transformNames are the header's low two bits, for the report rather than for
// the wire.
var transformNames = [4]string{"raw", "delta", "frame-of-reference", "constant"}

func columns() []columnCase {
	var out []columnCase

	add := func(name string, width int, values []int64) {
		var encoded []byte
		switch width {
		case 1:
			narrowed := make([]int8, len(values))
			for index, value := range values {
				narrowed[index] = int8(value)
			}
			encoded = column.AppendArray(nil, narrowed)
		case 2:
			narrowed := make([]int16, len(values))
			for index, value := range values {
				narrowed[index] = int16(value)
			}
			encoded = column.AppendArray(nil, narrowed)
		case 4:
			narrowed := make([]int32, len(values))
			for index, value := range values {
				narrowed[index] = int32(value)
			}
			encoded = column.AppendArray(nil, narrowed)
		default:
			encoded = column.AppendArray(nil, values)
		}
		transform := ""
		if len(encoded) > 0 {
			transform = transformNames[encoded[0]&0b11]
		}
		out = append(out, columnCase{
			Name:      name,
			Width:     width,
			Values:    values,
			Transform: transform,
			Encoded:   hex.EncodeToString(encoded),
		})
	}

	add("empty", 8, []int64{})
	add("one value", 8, []int64{42})
	add("all zero", 8, make([]int64, 256))
	add("constant", 8, repeatInt(7, 300))
	add("small dense ids", 8, sequence(1, 1, 900))
	add("monotonic timestamps", 8, sequence(1_700_000_000_000, 37, 300))
	add("clustered", 8, scatter(50_000, 997, 500))
	add("negatives", 8, sequence(-5, -3, 300))
	add("full width", 8, spread(1024))
	// Two blocks and a partial third, which is where the block boundary and the
	// tail's slower gather have to agree.
	add("across blocks", 8, sequence(0, 1, 2*128+7))
	add("int8", 1, []int64{-128, 0, 127, 1, -1})
	add("int16", 2, []int64{-32768, 0, 32767, 300})
	add("int32", 4, []int64{-2147483648, 0, 2147483647, 70000})
	add("extremes", 8, []int64{-9223372036854775808, 0, 9223372036854775807})

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

// scatter is a spread with no structure a transform can find, which is what
// leaves the raw form winning at a middling width.
func scatter(span, stride int64, count int) []int64 {
	out := make([]int64, count)
	for index := range out {
		out[index] = (int64(index) * stride) % span
	}
	return out
}

// spread is incompressible input: the widest blocks, where the codec is
// supposed to cost one byte per block and nothing else.
func spread(count int) []int64 {
	out := make([]int64, count)
	state := uint64(0x2545_F491_4F6C_DD1D)
	for index := range out {
		state = state*6364136223846793005 + 1442695040888963407
		out[index] = int64(state)
	}
	return out
}

func fieldIDs() []idCase {
	var out []idCase
	for _, value := range []any{
		Charge{}, Scalars{}, Arrays{}, Optionals{}, Line{}, Order{}, Maps{},
		WideEvolved{}, Wide{}, Hashed{}, PackedText{},
	} {
		ids, err := colbin.FieldIDs(value)
		if err != nil {
			panic(err)
		}
		numbers := make(map[string]int, len(ids))
		for name, id := range ids {
			numbers[name] = int(id)
		}
		out = append(out, idCase{Type: fmt.Sprintf("%T", value), IDs: numbers})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Type < out[b].Type })
	return out
}

func cases() []caseOut {
	var out []caseOut
	add := func(name, about string, value any) {
		message, err := colbin.Marshal(value)
		if err != nil {
			panic(err)
		}
		out = append(out, caseOut{
			Name:    name,
			Type:    fmt.Sprintf("%T", value),
			About:   about,
			Message: hex.EncodeToString(message),
		})
	}

	add("charge.zero", "every field zero, so nothing but the root descriptor", Charge{})
	add("charge.benchmark", "the five-of-ten-fields record the ports benchmark", Charge{
		CompanyID: 7, UserID: 42, RouteID: 103, CPU: 5, Access1: 0x0139,
	})
	add("charge.wide values", "values past the inline nibble, at several widths", Charge{
		CompanyID: 70000, UserID: 4_000_000_000, RouteID: 255, CPU: 255,
		Memory: 65535, Duration: -1, Access1: 256, Access2: 128,
		Created: 1_700_000_000_000, Updated: -1_700_000_000_000,
	})

	add("scalars.zero", "the omit-zero rule over every scalar kind", Scalars{})
	add("scalars.extremes", "every scalar at the end of its range", Scalars{
		Flag: true, Tiny: -128, Small: -32768, Medium: -2147483648,
		Large: -9223372036854775808, Byte: 255, Half: 65535, Word: 4294967295,
		Giant: 18446744073709551615, Single: 1.0, Double: -0.5,
		Text: "el niño comió jamón",
	})
	add("scalars.float trim", "a float trims its low end, where an integer trims its high one", Scalars{
		Single: 1.0, Double: 1.5,
	})

	add("arrays.empty", "an empty slice is an absent key", Arrays{})
	add("arrays.mixed", "every slice kind, signed and unsigned, at several widths", Arrays{
		Blob:   []byte{0, 1, 2, 255},
		Tiny:   []int8{-128, 0, 127},
		Small:  []int16{-32768, 0, 32767},
		Medium: []int32{-2147483648, 0, 2147483647},
		Large:  []int64{-9223372036854775808, 0, 9223372036854775807},
		Halves: []uint16{0, 65535},
		Words:  []uint32{0, 4294967295},
		Giants: []uint64{0, 18446744073709551615},
		Texts:  []string{"", "ok", "el niño"},
	})
	add("arrays.long blob", "a blob past 2047 bytes, where the size escapes to a wider field", Arrays{
		Blob: make([]byte, 3000),
	})
	add("arrays.long strings", "a string array element past 254 bytes, where the length escapes", Arrays{
		Texts: []string{repeat("a", 300), "short"},
	})

	zeroInt, zeroUint, zeroText, zeroFlag, zeroFloat := int32(0), uint32(0), "", false, 0.0
	setInt, setUint, setText, setFlag, setFloat := int32(-5), uint32(70000), "x", true, 2.5
	add("optionals.nil", "a nil pointer is an absent key and costs nothing", Optionals{})
	add("optionals.zero", "a pointer to a zero writes the zero out loud", Optionals{
		MaybeInt: &zeroInt, MaybeUint: &zeroUint, MaybeText: &zeroText,
		MaybeFlag: &zeroFlag, MaybeFloat: &zeroFloat,
	})
	add("optionals.set", "a pointer to a value writes it like any other field", Optionals{
		MaybeInt: &setInt, MaybeUint: &setUint, MaybeText: &setText,
		MaybeFlag: &setFlag, MaybeFloat: &setFloat,
	})

	add("order.nested", "a nested key run and a short list of them, row-wise", Order{
		ID:       90210,
		Customer: Line{SKU: "ACME", Quantity: 1},
		Lines:    lines(3),
		Note:     "ship soon",
	})
	add("order.table", "a slice past the threshold, transposed into keyed columns", Order{
		ID:    1,
		Lines: lines(40),
	})
	add("order.empty list", "an empty slice of structs is an absent key", Order{ID: 2})

	add("maps.empty", "an empty map is an absent key", Maps{})
	add("maps.one entry each", "a map's keys are values, not field ids", Maps{
		Labels: map[string]string{"a": "one"},
		Counts: map[int64]float64{-1: 2.5},
		Flags:  map[string]bool{"on": true},
		Sizes:  map[uint32]float32{7: 1.5},
	})

	add("wide.evolved", "a field id past fifteen, which puts the run on eight-bit keys", WideEvolved{
		First: 9, Last: "tail", Added: []int32{1, 2, 3}, Also: 1.5,
	})
	add("wide.old peer", "the same record as a reader that has not heard of the last two fields", Wide{
		First: 9, Last: "tail",
	})
	add("wide.integer widths", "every shape a uint16 takes on the wide key, across the varint window",
		WideWidths{Inline: 100, OneByte: 200, Varint: 313, Tie: 1024, Max: 65535})

	add("hashed.derived ids", "ids from the hash of the field names, which forces the wide width", Hashed{
		CompanyID: -3, User: "ana", Pinned: 1000, Ignored: 7,
	})

	// packed5 is a writer setting, and turning it on is what makes a string
	// field take the BLOB descriptor's enc code. A decoder reads either form
	// regardless, because the field says which it is.
	colbin.SetPacked5(true)
	add("packed5.string", "the opt-in string encoding, chosen per field by the descriptor", PackedText{
		Text: "el niño comió jamón",
	})
	add("packed5.does not pack", "a string the packed form would not shrink is written raw", PackedText{
		Text: `{"id":1023}`,
	})
	colbin.SetPacked5(false)

	return out
}

func lines(count int) []Line {
	out := make([]Line, count)
	for index := range out {
		out[index] = Line{
			SKU:      fmt.Sprintf("SKU-%d", index),
			Quantity: int32(index),
			Price:    float64(index) * 1.5,
		}
	}
	return out
}

func repeat(unit string, times int) string {
	out := make([]byte, 0, len(unit)*times)
	for range times {
		out = append(out, unit...)
	}
	return string(out)
}
