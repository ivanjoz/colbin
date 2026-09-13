package packed5

// A second implementation of the format, written to be obvious rather than
// fast, and a disassembler that turns a payload back into tokens.
//
// The encoder in encode.go is one fused pass: the greedy decision, the unit and
// the byte all happen at the same point, which is what makes it quick and what
// makes it hard to assert about. Everything here exists to give the other tests
// a vocabulary — "this string is a CASE_TOGGLE_LONG then four letters" — and to
// cross-check the fused pass against a version that keeps the three steps apart:
// tokens, then units, then bits, one bit at a time.

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// A token as the reference produces it. units is what the format charges.
type tok struct {
	kind  string
	val   int    // letter index, table index, or the value of a number
	raw   []byte // escape payload
	units int
}

func (t tok) String() string {
	switch t.kind {
	case "letter":
		return fmt.Sprintf("letter %c", 'a'+t.val)
	case "num":
		return fmt.Sprintf("num %d", t.val)
	case "sym":
		return fmt.Sprintf("sym %q", symTable[t.val])
	case "ext":
		return fmt.Sprintf("ext %q", extTable[t.val])
	case "esc":
		return fmt.Sprintf("esc %x", t.raw)
	}
	return t.kind
}

// scan is the greedy walk, written as a token producer. It follows the rules in
// the package documentation directly, with no packing mixed in.
func scan(s string, upper bool) []tok {
	var out []tok
	cur := upper
	emit := func(k string, v, u int) { out = append(out, tok{kind: k, val: v, units: u}) }

	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case isLetter(c):
			if isUpper(c) == cur {
				emit("letter", int(letterIndex(c)), 1)
				i++
				continue
			}
			run := 0
			for j := i; j < len(s) && isLetter(s[j]) && isUpper(s[j]) != cur; j++ {
				run++
			}
			if run >= 3 {
				emit("caseLong", 0, 1)
				cur = !cur
				continue
			}
			emit("caseSimple", 0, 1)
			emit("letter", int(letterIndex(c)), 1)
			i++

		case c == ' ':
			emit("space", 0, 1)
			i++

		case isDigit(c):
			v, best, n := 0, 0, 0
			for l := 1; l <= numberMaxDigits && i+l <= len(s); l++ {
				if !isDigit(s[i+l-1]) || (l > 1 && s[i] == '0') {
					break
				}
				v = v*10 + int(s[i+l-1]-'0')
				if v > numberMax {
					break
				}
				best, n = v, l
			}
			if n == 1 {
				emit("sym", int(asciiSym[c]), 2)
			} else {
				emit("num", best, 3)
			}
			i += n

		default:
			if c < 0x80 && asciiSym[c] >= 0 {
				emit("sym", int(asciiSym[c]), 2)
				i++
				continue
			}
			if c < 0x80 && asciiExt[c] >= 0 {
				emit("ext", int(asciiExt[c]), 2)
				i++
				continue
			}
			if c >= 0x80 {
				if k, w := extMulti(s, i); k >= 0 {
					emit("ext", int(k), 2)
					i += w
					continue
				}
			}
			n := 1
			for n < maxEscapeRun && i+n < len(s) && escapes(s, i+n) {
				n++
			}
			out = append(out, tok{kind: "esc", raw: []byte(s[i : i+n]), units: 3 + 2*n})
			i += n
		}
	}
	return out
}

// units expands tokens to the unit sequence the format defines.
func unitsOf(toks []tok) []uint8 {
	var u []uint8
	for _, t := range toks {
		switch t.kind {
		case "letter":
			u = append(u, uint8(t.val))
		case "space":
			u = append(u, opSpace)
		case "caseSimple":
			u = append(u, opCaseSimple)
		case "caseLong":
			u = append(u, opCaseLong)
		case "sym":
			u = append(u, opSymbol, uint8(t.val))
		case "ext":
			u = append(u, opExt, uint8(t.val))
		case "num":
			u = append(u, opNumber, uint8(t.val&31), uint8(t.val>>5))
		case "esc":
			u = append(u, opExt, extEscape, uint8(len(t.raw)-1))
			for _, b := range t.raw {
				u = append(u, b&31, b>>5)
			}
		}
	}
	return u
}

// packSlow lays units down one bit at a time, LSB first within each byte. It is
// the definition the group packer in units.go has to agree with.
func packSlow(u []uint8) []byte {
	out := make([]byte, payloadBytes(len(u)))
	for i, v := range u {
		for k := range 5 {
			if v>>k&1 == 1 {
				bit := i*5 + k
				out[bit/8] |= 1 << (bit % 8)
			}
		}
	}
	return out
}

// unpackSlow is its inverse, for n units.
func unpackSlow(payload []byte, n int) []uint8 {
	u := make([]uint8, n)
	for i := range u {
		for k := range 5 {
			bit := i*5 + k
			if bit/8 < len(payload) && payload[bit/8]>>(bit%8)&1 == 1 {
				u[i] |= 1 << k
			}
		}
	}
	return u
}

// referenceAppend builds a whole frame the long way round.
func referenceAppend(s string) []byte {
	if len(s) == 0 {
		return []byte{0}
	}
	upper := opensUpper(s)
	u := unitsOf(scan(s, upper))
	// The encoder pads to the unit grid with a simple toggle, which decodes to
	// nothing because no letter follows it.
	if payloadUnits(payloadBytes(len(u))) > len(u) {
		u = append(u, opCaseSimple)
	}
	payload := packSlow(u)
	if frameOverhead(len(payload))+len(payload) >= frameOverhead(len(s))+len(s) {
		return append(appendHeader(nil, 0, len(s)), s...)
	}
	flags := byte(flagPacked5)
	if upper {
		flags |= flagUppercase
	}
	return append(appendHeader(nil, flags, len(payload)), payload...)
}

// disassemble turns a packed frame back into tokens, for tests that want to
// assert about what the encoder chose rather than about the bytes.
func disassemble(t *testing.T, frameBytes []byte) []tok {
	t.Helper()
	h, err := frame(frameBytes)
	if err != nil {
		t.Fatalf("frame: %v", err)
	}
	if !h.packed {
		t.Fatalf("frame is raw, not packed")
	}
	u := unpackSlow(h.payload, payloadUnits(len(h.payload)))

	var out []tok
	for i := 0; i < len(u); {
		op := u[i]
		switch {
		case op < opSpace:
			out = append(out, tok{kind: "letter", val: int(op), units: 1})
			i++
		case op == opSpace:
			out = append(out, tok{kind: "space", units: 1})
			i++
		case op == opCaseSimple:
			out = append(out, tok{kind: "caseSimple", units: 1})
			i++
		case op == opCaseLong:
			out = append(out, tok{kind: "caseLong", units: 1})
			i++
		case op == opSymbol:
			out = append(out, tok{kind: "sym", val: int(u[i+1]), units: 2})
			i += 2
		case op == opExt:
			if u[i+1] != extEscape {
				out = append(out, tok{kind: "ext", val: int(u[i+1]), units: 2})
				i += 2
				break
			}
			n := int(u[i+2]) + 1
			raw := make([]byte, n)
			for k := range n {
				raw[k] = u[i+3+2*k] | u[i+4+2*k]<<5
			}
			out = append(out, tok{kind: "esc", raw: raw, units: 3 + 2*n})
			i += 3 + 2*n
		default:
			out = append(out, tok{kind: "num", val: int(u[i+1]) | int(u[i+2])<<5, units: 3})
			i += 3
		}
	}
	// Drop the grid pad: a trailing simple toggle applies to no letter.
	if n := len(out); n > 0 && out[n-1].kind == "caseSimple" {
		out = out[:n-1]
	}
	return out
}

// opsOf renders a token list for an assertion message.
func opsOf(toks []tok) string {
	parts := make([]string, len(toks))
	for i, t := range toks {
		parts[i] = t.String()
	}
	return strings.Join(parts, " ")
}

func countKind(toks []tok, kind string) int {
	n := 0
	for _, t := range toks {
		if t.kind == kind {
			n++
		}
	}
	return n
}

// TestFusedMatchesReference is the cross-check the whole file exists for: the
// fused encoder and the three-step reference must produce identical frames.
func TestFusedMatchesReference(t *testing.T) {
	rng := rand.New(rand.NewPCG(31, 32))
	pools := [][]string{
		alphabet,
		{"a", "b", "c", "X", "Y", "Z"},
		{"0", "1", "2", "9"},
		{"<", ">", "/", "@", "€", "ñ", "-", "."},
		{"hola", " ", "mundo", "Test", "123", "-", "ABC"},
		{"\x00", "\xff", "\xc3", "\n", "\t"},
	}
	inputs := []string{"", "a", "A", "SKU-4217-hola", "el niño comió jamón",
		"THE QUICK BROWN FOX", "00123", `{"id":1023}`, "ab-CD-EF-gh"}
	for _, pool := range pools {
		for range 4000 {
			inputs = append(inputs, randString(rng, pool, rng.IntN(40)))
		}
	}
	for _, s := range inputs {
		got, want := Append(nil, s), referenceAppend(s)
		if string(got) != string(want) {
			t.Fatalf("%q:\n fused %x\n  ref  %x", s, got, want)
		}
	}
}

// TestGroupPackerMatchesBitPacker isolates units.go from the tokeniser: the
// eight-units-in-five-bytes kernel must lay bits down exactly where the
// one-bit-at-a-time reference does.
func TestGroupPackerMatchesBitPacker(t *testing.T) {
	rng := rand.New(rand.NewPCG(33, 34))
	for n := range 300 {
		u := make([]uint8, n)
		for i := range u {
			u[i] = uint8(rng.IntN(32))
		}
		buf := make([]byte, payloadBytes(n)+8)
		w := writer{buf: buf, limit: len(buf) - 8}
		for _, v := range u {
			w.put(v)
		}
		// finish pads to the grid, so compare against a reference that does too.
		padded := u
		if payloadUnits(payloadBytes(len(padded))) > len(padded) {
			padded = append(append([]uint8(nil), padded...), opCaseSimple)
		}
		end, ok := w.finish()
		if !ok {
			t.Fatalf("n=%d: writer gave up", n)
		}
		if want := packSlow(padded); string(buf[:end]) != string(want) {
			t.Fatalf("n=%d:\n group %x\n  bits %x", n, buf[:end], want)
		}
	}
}

// TestByteClassifiers pins the branch-free classifiers against the obvious
// definitions, over every byte.
func TestByteClassifiers(t *testing.T) {
	for i := range 256 {
		c := byte(i)
		letter := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if isLetter(c) != letter {
			t.Errorf("isLetter(%#02x) = %v, want %v", c, isLetter(c), letter)
		}
		if got := isDigit(c); got != (c >= '0' && c <= '9') {
			t.Errorf("isDigit(%#02x) = %v", c, got)
		}
		if letter {
			if got := isUpper(c); got != (c >= 'A' && c <= 'Z') {
				t.Errorf("isUpper(%#02x) = %v", c, got)
			}
			want := c | 0x20
			if got := letterIndex(c); got != want-'a' {
				t.Errorf("letterIndex(%#02x) = %d", c, got)
			}
		}
	}
}
