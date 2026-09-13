// Package packed5 implements the Packed-5 string codec: a compact,
// self-delimiting representation for short strings dominated by ASCII letters,
// spaces, common punctuation, digits and Spanish accented characters.
//
//	import "github.com/ivanjoz/colbin/packed5"
//
//	buf := packed5.Append(nil, "helloWorld")
//
// # Units
//
// The alphabet is 32 five-bit codes, and **every token is a whole number of
// them**. That is the property the whole codec is built around: eight units are
// forty bits are five bytes exactly, so the packer builds one uint64 with fixed
// shifts, stores eight bytes and advances five. No accumulator, no variable
// drain, no token that straddles the grid at an offset the compiler cannot see.
//
// # Wire format
//
// A value is one frame. The first byte carries the header flags and, for all
// but the longest payloads, the payload length as well:
//
//	bit  0     PACKED_5             0 = raw payload, 1 = packed unit stream
//	bit  1     UPPERCASE            the stream starts in uppercase mode
//	bit  2     reserved             must be zero
//	bits 3-7   length code          payload byte length, or 31 = "see uvarint"
//
// A length code of 31 is followed by the payload length as an LEB128 uvarint.
// Codes 0..30 hold the length inline, so every frame whose payload fits in 30
// bytes costs exactly one byte of framing. The length is a *byte* count in both
// modes, which is what makes the frame self-delimiting.
//
// In raw mode the payload is the original byte string verbatim, valid UTF-8 or
// not. In packed mode the payload is the unit stream, five bits per unit,
// LSB-first within each byte, starting at bit 0 of the first payload byte.
//
// There is no pad prefix and no pad count. The unit count follows from the
// payload length alone:
//
//	units = len(payload) * 8 / 5
//
// because the encoder pads the stream to the unit grid with a trailing
// CASE_TOGGLE_SIMPLE. A simple toggle applies to the next letter, so one with no
// letter after it decodes to nothing: it is a no-op the alphabet already had.
// Padding with it costs no bytes — the rounding of 5*units bits up to a whole
// number of bytes always leaves room — and it is what lets the stream start on
// the grid at bit 0 rather than three bits into it.
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
// # Raw escape
//
// Opcode 30 with operand 31 is followed by one unit holding n-1 for n in 1..4,
// then two units per raw byte — the low five bits, then the high three:
//
//	3 + 2n units
//
// The escape carries bytes, not runes. That makes the codec byte-exact for
// arbitrary input, including strings that are not valid UTF-8, and lets the
// encoder amortise the three-unit header over a run of up to four bytes instead
// of paying it per rune.
//
// # Encoder
//
// One greedy left-to-right pass, fused with the packer. There is no planning
// pass: the codec has no behavioural header flags left to price, because the
// case mode is carried by an ordinary CASE_TOGGLE_LONG that the encoder hoists
// into the header bit when it lands first. See encode.go.
//
// The encoder never enlarges a string. It writes into a buffer bounded by the
// raw length and falls back to the raw form the moment the packed stream would
// pass it, so the result is never longer than len(s) plus its framing.
package packed5

import "errors"

var (
	// ErrTruncated is returned when a frame ends before its declared payload,
	// or a token's operands run past the end of the unit stream.
	ErrTruncated = errors.New("colbin: packed5 string truncated")
	// ErrReservedSymbol is returned for extTable indices the current table
	// leaves unassigned.
	ErrReservedSymbol = errors.New("colbin: packed5 reserved symbol index")
	// ErrBadLength is returned when the frame's length prefix is malformed or
	// describes a payload larger than the buffer.
	ErrBadLength = errors.New("colbin: packed5 bad length prefix")
	// ErrBadEscape is returned for a raw escape whose count or byte halves fall
	// outside the ranges the encoder can produce.
	ErrBadEscape = errors.New("colbin: packed5 bad raw escape")
	// ErrBadHeader is returned when a reserved header bit is set.
	ErrBadHeader = errors.New("colbin: packed5 reserved header bit set")
)

// Header flag bits, in byte 0 of every frame.
const (
	flagPacked5   = 1 << 0
	flagUppercase = 1 << 1
	flagReserved  = 1 << 2

	lenShift = 3
	// lenInline is the largest payload length the header's five length bits can
	// hold; 31 is the escape code that defers to a uvarint.
	lenInline = 30
	lenEscape = 31
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

// maxEscapeRun is how many raw bytes one escape can carry; the count occupies
// one unit but only 1..4 are legal, so that the header stays worth amortising.
const maxEscapeRun = 4

// numberMax is the largest integer opcode 31 can carry in its two units.
const numberMax = 1023

// numberMaxDigits is the digit count of numberMax, and so the longest decimal
// run one number token can absorb.
const numberMaxDigits = 4

// symTable is the opcode 29 operand table: the characters a short record is
// actually made of once letters and spaces are accounted for. All 32 entries are
// assigned, so no operand of this opcode can be invalid.
//
// Digits sit here at two units rather than in a table of their own. A lone digit
// therefore costs ten bits where the pre-unit format charged nine — the one bit
// per digit that buying the grid costs, and less than the three bits per frame
// that dropping the pad prefix returns.
var symTable = [32]byte{
	'0', '1', '2', '3', '4', '5', '6', '7', '8', '9',
	'.', ',', '-', '/', ':', ';', '_', '(', ')', '%',
	'#', '"', '\'', '!', '?', '@', '=', '+', '*', '&', '<', '>',
}

// extTable is the opcode 30 operand table: everything else worth a token.
// Index 31 is not a character, it is extEscape, and indices extReserved..30 are
// unassigned — rejected by the decoder today, so claiming them later is a clean
// format change rather than a silent reinterpretation.
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
// six bits of the UTF-8 continuation byte. Every non-ASCII entry above is Latin-1
// Supplement (a C2 or C3 lead) except '€', and continuation bytes run 0x80..0xBF,
// so within well-formed input those six bits identify the character outright —
// one array index instead of a rune decode and a table walk. Spanish text takes
// this path on nearly every word.
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

// uvarintLen is the LEB128 length of v, matching appendUvarint below.
func uvarintLen(v int) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

// appendUvarint writes v as LEB128. v is always a non-negative length here.
func appendUvarint(out []byte, v int) []byte {
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}

// readUvarint reads an LEB128 value from buf, returning it with the number of
// bytes consumed. It rejects overlong encodings and values that cannot be a
// length on this platform.
func readUvarint(buf []byte) (int, int, error) {
	var v uint64
	var shift uint
	for i, b := range buf {
		if i >= 9 {
			return 0, 0, ErrBadLength
		}
		v |= uint64(b&0x7F) << shift
		if b < 0x80 {
			if v > uint64(maxLen) {
				return 0, 0, ErrBadLength
			}
			return int(v), i + 1, nil
		}
		shift += 7
	}
	return 0, 0, ErrTruncated
}

// maxLen bounds a decoded payload length so that readUvarint cannot produce a
// value that overflows int on a 32-bit platform.
const maxLen = int(^uint32(0) >> 1)

// frameOverhead is the number of framing bytes a payload of n bytes needs: the
// header byte, plus a uvarint once the length outgrows the header's length bits.
func frameOverhead(n int) int {
	if n <= lenInline {
		return 1
	}
	return 1 + uvarintLen(n)
}

// isLetter reports whether c is an ASCII letter of either case.
//
// Setting bit 5 folds 'A'..'Z' onto 'a'..'z' and moves nothing else into that
// range, so one unsigned range test decides both cases at once.
func isLetter(c byte) bool { return (c|0x20)-'a' < 26 }

// isDigit reports whether c is an ASCII decimal digit. Unsigned wraparound turns
// the pair of comparisons into one.
func isDigit(c byte) bool { return c-'0' < 10 }

// isUpper reports the case of a byte already known to be an ASCII letter: for
// those, bit 5 is the case bit.
func isUpper(c byte) bool { return c&0x20 == 0 }

// letterIndex is the 0..25 alphabet position of an ASCII letter, either case.
func letterIndex(c byte) uint8 { return (c | 0x20) - 'a' }
