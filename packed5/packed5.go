// Package packed5 is colbin's opt-in string encoding: a compact form for short
// strings made mostly of ASCII letters, spaces, digits, common punctuation and
// Spanish accented characters, used only when it is smaller than the raw bytes.
//
// It writes a bare payload with no header of its own. The container stores the
// payload's byte length and one bit, the case mode the stream opens in — in
// colbin, a BLOB descriptor carries both:
//
//	buf, size, upper, ok := packed5.AppendPayload(buf, s) // !ok: write s raw
//	out, err := packed5.AppendString(dst, src, size, upper)
//
// # Units
//
// The alphabet is 32 five-bit codes, units, and every token is a whole number of
// them. Eight units are forty bits are five bytes exactly, so the packer builds
// one uint64 with fixed shifts, stores eight bytes and advances five; see
// units.go.
//
// # Payload
//
// Units are laid down LSB-first within each byte, starting at bit 0 of the first
// payload byte. The unit count follows from the length alone,
//
//	units = len(payload) * 8 / 5
//
// because the encoder pads the stream to that count with a trailing
// CASE_TOGGLE_SIMPLE, which applies to no letter and so decodes to nothing. The
// pad never costs a byte: it is emitted only when rounding 5*units bits up to a
// whole byte leaves room for it.
//
// # Opcodes
//
//	0..25   letter a..z, cased by the current case mode
//	26      space
//	27      CASE_TOGGLE_SIMPLE   invert the case of the next letter only
//	28      CASE_TOGGLE_LONG     invert the case mode until the next 28
//	29      + 1 unit: index into symTable  (digits and common punctuation)
//	30      + 1 unit: index into extTable  (accents and rare punctuation),
//	                  or 31 to start a raw-byte escape
//	31      + 2 units: integer 0..1023, low five bits first
//
// A raw escape is opcode 30, operand 31, one unit holding n-1 for n in 1..4, and
// two units per byte — the low five bits, then the high three: 3 + 2n units. It
// carries bytes rather than runes, which is what makes the codec byte-exact for
// input that is not valid UTF-8.
package packed5

import "errors"

var (
	// ErrTruncated is returned when a token's operands run past the end of the
	// unit stream, or when the size given to AppendString exceeds its buffer.
	ErrTruncated = errors.New("colbin: packed5 string truncated")
	// ErrBadLength is returned when the size given to AppendString is negative.
	ErrBadLength = errors.New("colbin: packed5 negative payload length")
	// ErrReservedSymbol is returned for an extTable index the table leaves
	// unassigned.
	ErrReservedSymbol = errors.New("colbin: packed5 reserved symbol index")
	// ErrBadEscape is returned for a raw escape whose count or byte halves fall
	// outside the ranges the encoder can produce.
	ErrBadEscape = errors.New("colbin: packed5 bad raw escape")
)

// The alphabet. Every code is one unit; the operands below are units too.
const (
	opSpace      = 26
	opCaseSimple = 27
	opCaseLong   = 28
	opSymbol     = 29
	opExt        = 30
	opNumber     = 31
)

// extEscape is the extTable operand that introduces a run of raw bytes.
const extEscape = 31

// maxEscapeRun is how many raw bytes one escape can carry. The count occupies a
// whole unit, but only 1..4 are legal.
const maxEscapeRun = 4

// numberMax is the largest integer opcode 31 can carry in its two units.
const numberMax = 1023

// numberMaxDigits is the digit count of numberMax, and so the longest decimal
// run one number token can absorb.
const numberMaxDigits = 4

// symTable is the opcode 29 operand table. All 32 entries are assigned, so no
// operand of this opcode can be invalid. A lone digit takes this table; a run of
// two or more takes the number token.
var symTable = [32]byte{
	'0', '1', '2', '3', '4', '5', '6', '7', '8', '9',
	'.', ',', '-', '/', ':', ';', '_', '(', ')', '%',
	'#', '"', '\'', '!', '?', '@', '=', '+', '*', '&', '<', '>',
}

// extTable is the opcode 30 operand table. Index 31 is not a character, it is
// extEscape, and indices extReserved..30 are unassigned — rejected by the
// decoder, so claiming them later is a clean format change rather than a silent
// reinterpretation.
//
// The accented characters are literals: the case mode does not apply to them,
// which is why both cases appear.
var extTable = [32]string{
	0: "ñ", 1: "á", 2: "é", 3: "í", 4: "ó", 5: "ú", 6: "ü",
	7: "Ñ", 8: "Á", 9: "É", 10: "Í", 11: "Ó", 12: "Ú",
	13: "€", 14: "$", 15: "~", 16: "`", 17: `\`, 18: "[", 19: "]",
	20: "^", 21: "{", 22: "}", 23: "|", 24: "\n", 25: "\t", 26: "\r", 27: "¿",
	28: "", 29: "", 30: "", // reserved
	31: "", // extEscape
}

// extReserved is the first unassigned extTable index.
const extReserved = 28

// asciiSym and asciiExt invert the two tables for their single-byte entries. A
// -1 means "this byte has no operand in that table".
//
// latinC2 and latinC3 do the same for the multi-byte entries, keyed on the low
// six bits of the UTF-8 continuation byte. Every non-ASCII entry is Latin-1
// Supplement (a C2 or C3 lead) except '€', so within well-formed input those six
// bits identify the character — one array index instead of a rune decode and a
// table walk.
var (
	asciiSym [128]int8
	asciiExt [128]int8
	latinC2  [64]int8
	latinC3  [64]int8
	extEuro  int8 = -1
)

func init() {
	for i := range asciiSym {
		asciiSym[i] = -1
		asciiExt[i] = -1
	}
	for i := range latinC2 {
		latinC2[i] = -1
		latinC3[i] = -1
	}
	for i, c := range symTable {
		asciiSym[c] = int8(i)
	}
	for i, s := range extTable {
		if i >= extReserved {
			continue
		}
		switch {
		case len(s) == 1:
			asciiExt[s[0]] = int8(i)
		case len(s) == 2 && s[0] == 0xC2:
			latinC2[s[1]&0x3F] = int8(i)
		case len(s) == 2 && s[0] == 0xC3:
			latinC3[s[1]&0x3F] = int8(i)
		case s == "€":
			extEuro = int8(i)
		}
	}
}

// extMulti returns the extTable index of the multi-byte character at s[i] with
// its width in bytes, or -1 and 0.
func extMulti(s string, i int) (int8, int) {
	if i+1 >= len(s) || s[i+1]&0xC0 != 0x80 {
		return -1, 0
	}
	switch s[i] {
	case 0xC2:
		return latinC2[s[i+1]&0x3F], 2
	case 0xC3:
		return latinC3[s[i+1]&0x3F], 2
	case 0xE2:
		if i+2 < len(s) && s[i+1] == 0x82 && s[i+2] == 0xAC {
			return extEuro, 3
		}
	}
	return -1, 0
}

// payloadUnits is how many units a payload of n bytes holds. The encoder pads to
// the grid, so this is exact rather than an upper bound.
func payloadUnits(n int) int { return n * 8 / 5 }

// payloadBytes is how many bytes n units occupy.
func payloadBytes(n int) int { return (n*5 + 7) / 8 }

// isLetter reports whether c is an ASCII letter of either case. Setting bit 5
// folds 'A'..'Z' onto 'a'..'z' and moves nothing else into that range, so one
// unsigned range test decides both cases.
func isLetter(c byte) bool { return (c|0x20)-'a' < 26 }

// isDigit reports whether c is an ASCII decimal digit.
func isDigit(c byte) bool { return c-'0' < 10 }

// isUpper reports the case of a byte already known to be an ASCII letter: for
// those, bit 5 is the case bit.
func isUpper(c byte) bool { return c&0x20 == 0 }

// letterIndex is the 0..25 alphabet position of an ASCII letter, either case.
func letterIndex(c byte) uint8 { return (c | 0x20) - 'a' }
