// Command vectors writes the golden frames the AssemblyScript port is diffed
// against (PLAN.md §7, level 1).
//
// The Go codecs are the specification here: every frame in the output was
// produced by varint.AppendArray or packed5.Append, and the port must reproduce
// each one byte for byte and decode it back. Nothing in this file hand-writes a
// frame, because a hand-written expectation only tests the person who wrote it.
package main

import (
	"encoding/base64"
	"encoding/binary"
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
	"github.com/ivanjoz/colbin/packed5"
	"github.com/ivanjoz/colbin/varint"
)

// A varint frame's header byte, decomposed. The generator asserts the corpus
// covers every transform rather than assuming the inputs it chose provoke them.
type header struct {
	Transform string `json:"transform"`
	Zigzag    bool   `json:"zigzag"`
	K         int    `json:"k"`
	Code      int    `json:"code"`
}

var transformNames = [4]string{"raw", "delta", "for", "fixed"}

func decodeHeader(b byte) header {
	return header{
		Transform: transformNames[b&0x03],
		Zigzag:    b&0x04 != 0,
		K:         int((b>>3)&0x03) + 1,
		Code:      int(b >> 5),
	}
}

type varintCase struct {
	Name   string   `json:"name"`
	Width  int      `json:"width"`
	Values []string `json:"values"` // decimal strings: JSON numbers lose int64
	Frame  string   `json:"frame"`  // hex
	Header *header  `json:"header,omitempty"`
}

type packed5Case struct {
	Name  string `json:"name"`
	Input string `json:"input"` // base64: the input may not be valid UTF-8
	Frame string `json:"frame"` // hex
	Size  int    `json:"size"`  // packed5.Size, which must equal len(frame)
}

func main() {
	outDir := "tests/vectors"
	if len(os.Args) > 1 {
		outDir = os.Args[1]
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		panic(err)
	}

	vc := varintCases()
	pc := packed5Cases()
	nc := numberCases()
	sc := stringCases()
	fc := fieldIDCases()
	mc := messageCases()

	write(filepath.Join(outDir, "varint.json"), vc)
	write(filepath.Join(outDir, "packed5.json"), pc)
	write(filepath.Join(outDir, "numbers.json"), nc)
	write(filepath.Join(outDir, "strings.json"), sc)
	write(filepath.Join(outDir, "fieldids.json"), fc)
	write(filepath.Join(outDir, "messages.json"), mc)

	reportCoverage(vc)
	fmt.Printf("%d varint, %d packed5, %d number, %d string, %d field-id, %d message vectors -> %s\n",
		len(vc), len(pc), len(nc), len(sc), len(fc), len(mc), outDir)
}

func write(path string, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		panic(err)
	}
}

// reportCoverage fails the build if the corpus misses a transform or a k. The
// inputs are chosen to provoke each one, but "chosen to provoke" is a belief
// until the header byte says so.
func reportCoverage(cases []varintCase) {
	transforms := map[string]int{}
	ks := map[int]int{}
	widths := map[int]int{}
	zigzag := map[bool]int{}
	for _, c := range cases {
		if c.Header == nil {
			continue
		}
		transforms[c.Header.Transform]++
		ks[c.Header.K]++
		widths[c.Width]++
		zigzag[c.Header.Zigzag]++
	}
	fmt.Printf("transforms: %v\nk: %v\nwidths: %v\nzigzag: %v\n", transforms, ks, widths, zigzag)
	missing := []string{}
	for _, t := range transformNames {
		if transforms[t] == 0 {
			missing = append(missing, "transform="+t)
		}
	}
	for _, w := range []int{8, 16, 32, 64} {
		if widths[w] == 0 {
			missing = append(missing, "width="+strconv.Itoa(w))
		}
	}
	if zigzag[true] == 0 || zigzag[false] == 0 {
		missing = append(missing, "zigzag=both")
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "\nCOVERAGE GAP: %v\n", missing)
		os.Exit(1)
	}
}

// appendCase encodes vals at the given width through varint.AppendArray. The
// generic call is what selects the element width on the wire, so each width
// needs its own instantiation rather than a runtime parameter.
func appendCase(name string, width int, vals []int64) varintCase {
	var frame []byte
	switch width {
	case 8:
		buf := make([]int8, len(vals))
		for i, v := range vals {
			buf[i] = int8(v)
		}
		frame = varint.AppendArray(nil, buf)
	case 16:
		buf := make([]int16, len(vals))
		for i, v := range vals {
			buf[i] = int16(v)
		}
		frame = varint.AppendArray(nil, buf)
	case 32:
		buf := make([]int32, len(vals))
		for i, v := range vals {
			buf[i] = int32(v)
		}
		frame = varint.AppendArray(nil, buf)
	default:
		frame = varint.AppendArray(nil, vals)
	}

	// Round-trip here too: a golden frame that Go itself cannot read back is a
	// bug in the corpus, and it would be blamed on the port.
	if err := verifyVarint(frame, width, vals); err != nil {
		panic(fmt.Sprintf("%s: %v", name, err))
	}

	strs := make([]string, len(vals))
	for i, v := range vals {
		strs[i] = strconv.FormatInt(v, 10)
	}
	c := varintCase{Name: name, Width: width, Values: strs, Frame: hex.EncodeToString(frame)}
	if len(frame) > 0 {
		h := decodeHeader(frame[0])
		c.Header = &h
	}
	return c
}

func verifyVarint(frame []byte, width int, want []int64) error {
	got := make([]int64, len(want))
	var n int
	var err error
	switch width {
	case 8:
		out := make([]int8, len(want))
		n, err = varint.DecodeArray(frame, len(want), out)
		for i, v := range out {
			got[i] = int64(v)
		}
	case 16:
		out := make([]int16, len(want))
		n, err = varint.DecodeArray(frame, len(want), out)
		for i, v := range out {
			got[i] = int64(v)
		}
	case 32:
		out := make([]int32, len(want))
		n, err = varint.DecodeArray(frame, len(want), out)
		for i, v := range out {
			got[i] = int64(v)
		}
	default:
		n, err = varint.DecodeArray(frame, len(want), got)
	}
	if err != nil {
		return err
	}
	if n != len(frame) {
		return fmt.Errorf("consumed %d of %d bytes", n, len(frame))
	}
	for i := range want {
		if got[i] != want[i] {
			return fmt.Errorf("value %d: got %d want %d", i, got[i], want[i])
		}
	}
	return nil
}

func varintCases() []varintCase {
	r := rand.New(rand.NewSource(20260826))
	cases := []varintCase{}
	add := func(name string, width int, vals []int64) {
		cases = append(cases, appendCase(name, width, vals))
	}

	// Degenerate shapes. The empty frame is the one most likely to be dropped in
	// a port: it is a single trRaw header byte, not zero bytes.
	add("empty", 64, nil)
	add("single-zero", 64, []int64{0})
	add("single-one", 64, []int64{1})
	add("all-zero", 64, []int64{0, 0, 0, 0, 0, 0, 0, 0})

	// The case varint/README.md works through and TestArrayDeltaExampleIsOptimal
	// pins: deltas [100,140,10,130], b_max=8, optimal at k=1,M=1.
	add("readme-delta-example", 32, []int64{100, 240, 250, 380})

	add("small-ascending", 64, []int64{1, 2, 3, 4, 5, 6, 7, 8})
	add("small-descending", 64, []int64{8, 7, 6, 5, 4, 3, 2, 1})
	add("negatives", 64, []int64{-1, -2, -3, -4})
	add("alternating-sign", 64, []int64{1, -1, 2, -2, 3, -3})
	add("constant", 64, []int64{5, 5, 5, 5, 5, 5, 5, 5})
	add("constant-large", 64, []int64{1 << 40, 1 << 40, 1 << 40, 1 << 40})

	// Frame-of-reference: a band far from zero that deltas cannot beat. A
	// *monotonic* run near 1e6 is a delta case, not a FOR one - its deltas are
	// tiny. FOR wins only when the values scatter inside the band, so the deltas
	// are as wide as the band while the offsets from its floor stay narrow.
	forBand := make([]int64, 32)
	for i := range forBand {
		forBand[i] = 1_000_000 + int64(r.Int31n(256))
	}
	add("for-band", 64, forBand)
	forWide := make([]int64, 32)
	for i := range forWide {
		forWide[i] = 1<<50 + int64(r.Int31n(1024))
	}
	add("for-band-wide", 64, forWide)

	// k = 3 wants every value at least three varint bytes long, so a band that
	// needs 15..21 bits and never less.
	k3 := make([]int64, 32)
	for i := range k3 {
		k3[i] = int64(1<<16) + int64(r.Int31n(1<<16))
	}
	add("k3-band", 64, k3)

	// Extremes, where zigzag and the fixed fallback have to be exact.
	add("int64-limits", 64, []int64{math.MinInt64, math.MaxInt64, 0, -1})
	add("int32-limits", 32, []int64{math.MinInt32, math.MaxInt32, 0, -1})
	add("int16-limits", 16, []int64{math.MinInt16, math.MaxInt16, 0, -1})
	add("int8-limits", 8, []int64{math.MinInt8, math.MaxInt8, 0, -1})

	// Bit-length boundaries: the residue classes where a declared M is one byte
	// shorter than plain varint (b_max = 8, 15, 22, 29 ...).
	for _, bits := range []uint{7, 8, 14, 15, 21, 22, 28, 29, 35, 56, 63} {
		vals := make([]int64, 16)
		for i := range vals {
			vals[i] = int64(r.Uint64() >> (64 - bits))
		}
		add(fmt.Sprintf("random-%dbit", bits), 64, vals)
	}

	// Random at each width, which is where the fixed-width fallback lives.
	for _, w := range []int{8, 16, 32, 64} {
		vals := make([]int64, 64)
		for i := range vals {
			switch w {
			case 8:
				vals[i] = int64(int8(r.Uint32()))
			case 16:
				vals[i] = int64(int16(r.Uint32()))
			case 32:
				vals[i] = int64(int32(r.Uint32()))
			default:
				vals[i] = int64(r.Uint64())
			}
		}
		add(fmt.Sprintf("random-width%d", w), w, vals)
	}

	// A long run, so k>1 and the multi-byte paths are exercised at length.
	long := make([]int64, 300)
	for i := range long {
		long[i] = int64(1_000_000 + i*7 + int(r.Int31n(3)))
	}
	add("long-monotonic", 64, long)

	return cases
}

func packed5Cases() []packed5Case {
	inputs := []struct {
		name string
		s    string
	}{
		{"empty", ""},
		{"single-a", "a"},
		{"single-z", "z"},
		{"single-A", "A"},
		{"single-space", " "},
		{"lower-word", "hello"},
		{"lower-sentence", "hello world"},
		{"upper-dominant", "HELLO WORLD"},
		{"title-case", "Hello World"},
		{"case-alternating", "aAbBcCdD"},
		// README: 19 chars, 22 bytes, packs to 17.
		{"accented-spanish", "el niño comió jamón"},
		{"sku", "SKU-00123"},
		{"digits", "0123456789"},
		{"symbols", "<>/\"'%#|()!?$~`€@\\[]^{}_"},
		{"accented-letters", "ñáéíóú"},
		{"simple-table", "0123456789.-+*="},
		// The 0..1023 integer opcode.
		{"numbers-in-range", "room 512 and 1023"},
		{"numbers-out-of-range", "room 1024 and 4096"},
		// Length-code boundaries: 30 inline, 31 escapes to a uvarint.
		{"len-29", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{"len-30", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{"len-31", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{"len-32", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{"long-text", "the quick brown fox jumps over the lazy dog and keeps running for a while"},
		{"long-mixed", "Pedido 2024-00871 — Cliente: Ñandú S.A.C. — Total: 1234.56 PEN"},
		// Byte-exactness: these are why the escape opcode carries bytes, not runes.
		{"nul-bytes", "a\x00b\x00c"},
		{"invalid-utf8", "\xff\xfe\x80\x81"},
		{"lone-surrogate-bytes", "\xed\xa0\x80"},
		{"emoji", "party 🎉 time"},
		{"cjk", "日本語テキスト"},
		{"binary-random", string([]byte{0x00, 0xff, 0x7f, 0x80, 0x01, 0xfe, 0x55, 0xaa})},
		{"newlines-tabs", "line1\nline2\tend"},
	}

	cases := make([]packed5Case, 0, len(inputs))
	for _, in := range inputs {
		frame := packed5.Append(nil, in.s)

		// Same contract as the varint corpus: Go must read back what Go wrote,
		// and Size must agree with what Append produced.
		got, n, err := packed5.Decode(frame)
		if err != nil {
			panic(fmt.Sprintf("%s: decode: %v", in.name, err))
		}
		if got != in.s {
			panic(fmt.Sprintf("%s: round-trip mismatch: %q != %q", in.name, got, in.s))
		}
		if n != len(frame) {
			panic(fmt.Sprintf("%s: consumed %d of %d", in.name, n, len(frame)))
		}
		if sz := packed5.Size(in.s); sz != len(frame) {
			panic(fmt.Sprintf("%s: Size says %d, Append wrote %d", in.name, sz, len(frame)))
		}

		cases = append(cases, packed5Case{
			Name:  in.name,
			Input: base64.StdEncoding.EncodeToString([]byte(in.s)),
			Frame: hex.EncodeToString(frame),
			Size:  len(frame),
		})
	}
	return cases
}

// numberCase pins how one JSON number literal must be read. The rule is
// PLAN.md §3.2: a literal with no '.' and no exponent is an integer and must
// land in int64 or uint64 with every digit intact; anything else is a real
// number and becomes float64, which must be finite.
type numberCase struct {
	Name    string `json:"name"`
	Literal string `json:"literal"`
	Kind    string `json:"kind"`  // int | uint | float | error
	Value   string `json:"value"` // decimal for int/uint; "" for float
	Bits    string `json:"bits"`  // float64 bits as hex; "" for int/uint
}

func classifyNumber(lit string) numberCase {
	c := numberCase{Literal: lit}
	isFloat := strings.ContainsAny(lit, ".eE")
	if !isFloat {
		if v, err := strconv.ParseInt(lit, 10, 64); err == nil {
			c.Kind, c.Value = "int", strconv.FormatInt(v, 10)
			return c
		}
		if v, err := strconv.ParseUint(lit, 10, 64); err == nil {
			c.Kind, c.Value = "uint", strconv.FormatUint(v, 10)
			return c
		}
		c.Kind = "error"
		return c
	}
	v, err := strconv.ParseFloat(lit, 64)
	if err != nil || math.IsInf(v, 0) || math.IsNaN(v) {
		c.Kind = "error"
		return c
	}
	c.Kind = "float"
	c.Bits = fmt.Sprintf("%016x", math.Float64bits(v))
	return c
}

func numberCases() []numberCase {
	literals := []struct{ name, lit string }{
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
	for _, l := range literals {
		c := classifyNumber(l.lit)
		c.Name = l.name
		out = append(out, c)
	}

	// Hand-picked cases test the boundaries someone thought of. These test the
	// ones nobody did: random float64s rendered several ways, each of which must
	// read back to the identical bit pattern. 'g' with 17 digits and 'e' with 20
	// are both past shortest-round-trip, so they exercise the exact path rather
	// than the fast one.
	r := rand.New(rand.NewSource(20260826))
	for i := 0; i < 150; i++ {
		bits := r.Uint64()
		f := math.Float64frombits(bits)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			continue
		}
		for _, format := range []struct {
			tag  string
			text string
		}{
			{"shortest", strconv.FormatFloat(f, 'g', -1, 64)},
			{"e17", strconv.FormatFloat(f, 'e', 17, 64)},
			{"e20", strconv.FormatFloat(f, 'e', 20, 64)},
		} {
			// A rendering without '.' or an exponent would be read as an integer
			// by the rule in PLAN.md 3.2, which is a different test.
			if !strings.ContainsAny(format.text, ".eE") {
				continue
			}
			c := classifyNumber(format.text)
			c.Name = fmt.Sprintf("random-%s-%d", format.tag, i)
			out = append(out, c)
		}
	}

	// Subnormals and the boundaries around them, where a doubly-rounded
	// conversion goes wrong and nothing else notices.
	for i := 0; i < 40; i++ {
		bits := r.Uint64() % (1 << 52) // subnormal significands
		f := math.Float64frombits(bits)
		c := classifyNumber(strconv.FormatFloat(f, 'e', 20, 64))
		c.Name = fmt.Sprintf("subnormal-%d", i)
		out = append(out, c)
	}
	return out
}

// stringCase pins how one JSON string literal decodes. Go's encoding/json is
// the reference for the escape rules, including the lone-surrogate case that
// PLAN.md §4.5 defines as U+FFFD.
type stringCase struct {
	Name    string `json:"name"`
	Literal string `json:"literal"` // the JSON text, quotes included
	Decoded string `json:"decoded"` // base64 of the decoded bytes
}

func stringCases() []stringCase {
	literals := []struct{ name, lit string }{
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
	out := make([]stringCase, 0, len(literals))
	for _, l := range literals {
		var s string
		if err := json.Unmarshal([]byte(l.lit), &s); err != nil {
			panic(fmt.Sprintf("%s: %v", l.name, err))
		}
		out = append(out, stringCase{
			Name:    l.name,
			Literal: l.lit,
			Decoded: base64.StdEncoding.EncodeToString([]byte(s)),
		})
	}
	return out
}

// fieldIDCase pins the wire ids colbin assigns to a set of field names.
//
// The ids are read back out of a real MarshalJSON schema section rather than
// recomputed here. A second Go implementation of the hash would only ever agree
// with itself; this agrees with the library, which is the thing the port has to
// match.
type fieldIDCase struct {
	Name  string   `json:"name"`
	Names []string `json:"names"`
	IDs   []int    `json:"ids"`
}

// structFor builds a struct type whose fields carry the given JSON names, the
// same way the message oracle will (PLAN.md A.2).
func structFor(names []string) reflect.Type {
	fields := make([]reflect.StructField, len(names))
	for i, n := range names {
		fields[i] = reflect.StructField{
			Name: fmt.Sprintf("F%d", i),
			Type: reflect.TypeOf(int64(0)),
			Tag:  reflect.StructTag(fmt.Sprintf("cb:%q json:%q", n, n)),
		}
	}
	return reflect.StructOf(fields)
}

// fieldIDsOf marshals one record of t and reads the ids out of the schema
// section: [0x04][schemaLen][flags][structCount][fieldCount]([id] packed5(name) desc)*
func fieldIDsOf(t reflect.Type) ([]string, []int) {
	rows := reflect.MakeSlice(reflect.SliceOf(t), 1, 1)
	data, err := colbin.MarshalJSON(rows.Interface())
	if err != nil {
		panic(err)
	}
	if data[0] != 0x04 {
		panic("expected a JSON-mode message")
	}
	pos := 1
	_, n := binary.Uvarint(data[pos:]) // schema length
	pos += n
	pos++                             // flags
	_, n = binary.Uvarint(data[pos:]) // struct count
	pos += n

	fieldCount := int(data[pos])
	pos++
	names := make([]string, 0, fieldCount)
	ids := make([]int, 0, fieldCount)
	for i := 0; i < fieldCount; i++ {
		ids = append(ids, int(data[pos]))
		pos++
		name, used, err := packed5.Decode(data[pos:])
		if err != nil {
			panic(err)
		}
		names = append(names, name)
		pos += used
		pos = skipDesc(data, pos)
	}
	return names, ids
}

// skipDesc steps over one schema descriptor. Only the shapes this generator can
// produce are handled; anything else means the generator changed and the panic
// is the right outcome.
func skipDesc(buf []byte, pos int) int {
	flags := buf[pos]
	pos++
	switch flags & 0x07 {
	case 0, 1: // int, float: one scalar-kind byte
		return pos + 1
	case 2, 3, 7: // string, bytes, any: nothing
		return pos
	case 4: // array: the element descriptor
		return skipDesc(buf, pos)
	case 5: // struct: an index into the struct table
		_, n := binary.Uvarint(buf[pos:])
		return pos + n
	case 6: // map: key then value
		return skipDesc(buf, skipDesc(buf, pos))
	}
	panic("unknown descriptor class")
}

// collidingNames finds field names whose FNV-1a-32 folds to the same 8 bits, so
// the corpus exercises linear probing rather than assuming it is never reached.
func collidingNames(count int) []string {
	fold := func(s string) uint8 {
		h := uint32(2166136261)
		for i := 0; i < len(s); i++ {
			h ^= uint32(s[i])
			h *= 16777619
		}
		return uint8(h ^ (h >> 8) ^ (h >> 16) ^ (h >> 24))
	}
	buckets := map[uint8][]string{}
	letters := "abcdefghijklmnopqrstuvwxyz"
	for i := 0; i < len(letters); i++ {
		for j := 0; j < len(letters); j++ {
			for k := 0; k < len(letters); k++ {
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

func fieldIDCases() []fieldIDCase {
	sets := []struct {
		name  string
		names []string
	}{
		{"products", []string{"id", "sku", "name", "city", "price", "stock", "active", "weight"}},
		{"single", []string{"id"}},
		{"reordered", []string{"weight", "active", "stock", "price", "city", "name", "sku", "id"}},
		{"metrics", []string{"t", "v", "host"}},
		{"unicode-names", []string{"año", "niño", "precio_€", "日本語"}},
		{"long-names", []string{
			"a_very_long_field_name_that_exceeds_thirty_bytes_easily",
			"another_very_long_field_name_for_the_same_reason",
		}},
		{"collision-pair", collidingNames(2)},
		{"collision-triple", collidingNames(3)},
	}

	// A struct at the 254-field ceiling, where probing has almost no free slots
	// left and the wrap-around past 255 is exercised for real.
	wide := make([]string, 254)
	for i := range wide {
		wide[i] = fmt.Sprintf("f%03d", i)
	}
	sets = append(sets, struct {
		name  string
		names []string
	}{"max-fields-254", wide})

	out := make([]fieldIDCase, 0, len(sets))
	for _, set := range sets {
		names, ids := fieldIDsOf(structFor(set.names))
		// The schema lists fields in declaration order, so the names read back
		// must be the ones handed in; if not, the assumption is wrong and every
		// id below would be meaningless.
		for i := range names {
			if names[i] != set.names[i] {
				panic(fmt.Sprintf("%s: field %d is %q, expected %q", set.name, i, names[i], set.names[i]))
			}
		}
		out = append(out, fieldIDCase{Name: set.name, Names: set.names, IDs: ids})
	}
	return out
}

// messageCase is one whole message: JSON in, the bytes colbin.MarshalJSON
// produced, and what DecodeJSON reads back out of them.
//
// Tier names which part of the format the case needs, so the port can turn them
// on one at a time instead of failing everything until the last one lands.
type messageCase struct {
	Name    string `json:"name"`
	Tier    string `json:"tier"`
	JSON    string `json:"json"`
	Message string `json:"message"` // hex
	Decoded string `json:"decoded"` // DecodeJSON output, for the round trip
}

func messageCases() []messageCase {
	corpus := []struct{ tier, name, text string }{
		// scalar: integer, bool and string columns over flat records.
		{"scalar", "one-int-field", `[{"a":1}]`},
		{"scalar", "one-record-three-fields", `[{"id":7,"name":"Tin","active":true}]`},
		{"scalar", "three-records", `[{"id":1,"name":"Tin Light","price":1299,"active":true},{"id":2,"name":"Steel Lamp","price":24990,"active":false},{"id":3,"name":"Copper Wire","price":599,"active":true}]`},
		{"scalar", "single-object", `{"id":42,"name":"Solo","price":100}`},
		{"scalar", "negative-ints", `[{"v":-1},{"v":-1000},{"v":5}]`},
		{"scalar", "big-ints", `[{"v":7295013456321098765},{"v":1}]`},
		{"scalar", "uint64", `[{"v":18446744073709551615},{"v":0}]`},
		{"scalar", "bools-only", `[{"a":true,"b":false},{"a":false,"b":true}]`},
		{"scalar", "empty-strings", `[{"s":""},{"s":""}]`},
		{"scalar", "accented-strings", `[{"s":"el niño comió jamón"},{"s":"SKU-00123"}]`},
		{"scalar", "long-strings", `[{"s":"the quick brown fox jumps over the lazy dog and keeps going"}]`},
		{"scalar", "many-fields", `[{"a":1,"b":2,"c":3,"d":4,"e":5,"f":6,"g":7,"h":8,"i":9,"j":10}]`},
		{"scalar", "zero-values", `[{"i":0,"s":"","b":false}]`},
		{"scalar", "products-20", productsJSON(20)},
		{"scalar", "metrics-50", metricsJSON(50)},

		// float
		{"float", "one-float", `[{"v":1.5}]`},
		{"float", "mixed-int-float", `[{"v":1},{"v":2.5},{"v":3}]`},
		{"float", "float-zeros", `[{"v":0.0},{"v":0.0}]`},
		{"float", "float-precision", `[{"v":0.1},{"v":0.3333333333333333},{"v":1e30}]`},

		// nested structs
		{"nested", "one-nested", `[{"c":{"ruc":"20512345678","name":"ACME"}}]`},
		{"nested", "two-deep", `[{"a":{"b":{"c":1}}}]`},
		{"nested", "nested-plus-scalar", `[{"id":1,"c":{"n":"A"}},{"id":2,"c":{"n":"B"}}]`},
		{"nested", "repeated-shape", `[{"a":{"x":1},"b":{"x":2}}]`},

		// arrays
		{"array", "int-array", `[{"v":[1,2,3]},{"v":[4]}]`},
		{"array", "string-array", `[{"v":["a","bb"]},{"v":[]}]`},
		{"array", "array-of-objects", `[{"lines":[{"sku":"A1","qty":2},{"sku":"B2","qty":1}]},{"lines":[{"sku":"C3","qty":7}]}]`},
		{"array", "empty-arrays", `[{"v":[]},{"v":[]}]`},
		{"array", "nested-arrays", `[{"v":[[1,2],[3]]}]`},

		// nullable
		{"nullable", "explicit-null", `[{"a":1},{"a":null}]`},
		{"nullable", "missing-key", `[{"a":1,"b":2},{"a":3}]`},
		{"nullable", "all-null", `[{"a":null},{"a":null}]`},
		{"nullable", "null-string", `[{"s":"x"},{"s":null}]`},
		{"nullable", "null-nested", `[{"c":{"x":1}},{"c":null}]`},
	}

	out := make([]messageCase, 0, len(corpus))
	for _, c := range corpus {
		msg, err := encodeJSON(c.text)
		if err != nil {
			panic(fmt.Sprintf("%s: %v", c.name, err))
		}
		back, err := colbin.DecodeJSON(msg)
		if err != nil {
			panic(fmt.Sprintf("%s: decode: %v", c.name, err))
		}
		out = append(out, messageCase{
			Name:    c.name,
			Tier:    c.tier,
			JSON:    c.text,
			Message: hex.EncodeToString(msg),
			Decoded: string(back),
		})
	}
	return out
}

func productsJSON(n int) string {
	names := []string{"Tin Light", "Steel Lamp", "Copper Wire", "Brass Hinge", "Zinc Plate"}
	cities := []string{"Lima", "Arequipa", "Trujillo", "Cusco", "Piura"}
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf(`{"id":%d,"sku":"SKU-%05d","name":%q,"city":%q,"price":%d,"stock":%d,"active":%v}`,
			100000+i*7, i, names[i%5], cities[i%5], 599+i*13, i%97, i%3 != 0)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func metricsJSON(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf(`{"t":%d,"host":"node-%02d","up":%v}`, 1756200000+i*15, i%12, i%7 != 0)
	}
	return "[" + strings.Join(parts, ",") + "]"
}
