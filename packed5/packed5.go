// Package packed5 implements the Packed-5 string codec: a compact,
// self-delimiting representation for short strings dominated by ASCII letters,
// spaces, common punctuation, digits and Spanish accented characters.
//
//	import "github.com/ivanjoz/colbin/packed5"
//
//	buf := packed5.Append(nil, "helloWorld")
//
// # Wire format
//
// A value is one frame. The first byte carries the header flags and, for all
// but the longest payloads, the payload length as well:
//
//	bit  0     PACKED_5             0 = raw UTF-8 payload, 1 = packed stream
//	bit  1     UPPERCASE_DOMINANT   default case of the packed stream
//	bit  2     ENABLE_NUMBER_0_1023 opcode 31 is a 10-bit integer, not '-'
//	bits 3-7   length code          payload byte length, or 31 = "see uvarint"
//
// A length code of 31 is followed by the payload length as an LEB128 uvarint.
// Codes 0..30 hold the length inline, so every frame whose payload fits in 30
// bytes costs exactly one byte of framing. The length is a *byte* count in both
// modes, which is what makes the frame self-delimiting.
//
// In raw mode the payload is the UTF-8 (more precisely, the original byte
// string, valid UTF-8 or not) verbatim. In packed mode the payload is a
// bitstream, written LSB-first within each byte:
//
//	3 bits    padBits: unused bits at the end of the final payload byte
//	tokens    5-bit opcodes plus their operands
//	padBits   zero bits
//
// The pad count is what terminates the stream. Every 5-bit value is a legal
// opcode, so trailing zero padding would otherwise decode as extra 'a's; with
// padBits the decoder knows the exact token bit count, 8*len(payload)-3-padBits,
// and stops there. Three bits is the cheapest possible terminator, and it is
// paid once per frame rather than once per token.
//
// # Opcodes
//
//	0..25   letter a..z, cased by the current case mode
//	26      space
//	27      CASE_TOGGLE_SIMPLE   invert the case of the next letter only
//	28      CASE_TOGGLE_LONG     invert the case mode until the next 28
//	29      + 5-bit index into symTable (indices 30 and 31 are reserved)
//	30      + 4-bit index into simpleTable; index 15 starts a UTF-8 escape
//	31      + 10-bit integer if ENABLE_NUMBER_0_1023, otherwise '-'
//
// # UTF-8 escape
//
// Opcode 30 with operand 15 is followed by a 2-bit count n-1 and then n raw
// bytes, n in 1..4:
//
//	5 + 4 + 2 + 8n bits
//
// The escape carries bytes, not runes. That makes the codec byte-exact for
// arbitrary input, including strings that are not valid UTF-8, and lets the
// encoder amortise the 11-bit header over a run of up to four bytes instead of
// paying it per rune.
//
// # Encoder
//
// The encoder never enlarges a string: it computes the exact packed frame size
// and falls back to raw UTF-8 unless the packed frame is strictly smaller. The
// packed stream itself comes from a single greedy scan, run once per candidate
// setting of the two behavioural header flags — see encode.go, which also
// records how far that lands from the size-optimal encoding.
package packed5

import "errors"

var (
	// ErrTruncated is returned when a frame ends before its declared payload,
	// or a token's operand runs past the end of the bitstream.
	ErrTruncated = errors.New("colbin: packed5 string truncated")
	// ErrReservedSymbol is returned for symbol indices 30 and 31 of opcode 29,
	// which the current symbol table leaves unassigned.
	ErrReservedSymbol = errors.New("colbin: packed5 reserved symbol index")
	// ErrBadLength is returned when the frame's length prefix is malformed or
	// describes a payload larger than the buffer.
	ErrBadLength = errors.New("colbin: packed5 bad length prefix")
	// ErrBadPadding is returned when the pad count exceeds the bits actually
	// present in the payload.
	ErrBadPadding = errors.New("colbin: packed5 bad stream padding")
)

// Header flag bits, in byte 0 of every frame.
const (
	flagPacked5   = 1 << 0
	flagUppercase = 1 << 1
	flagNumber    = 1 << 2

	flagMask = 0x07
	lenShift = 3
	// lenInline is the largest payload length the header's five length bits can
	// hold; 31 is the escape code that defers to a uvarint.
	lenInline = 30
	lenEscape = 31
)

// Base alphabet opcodes above the 26 letters.
const (
	opSpace      = 26
	opCaseSimple = 27
	opCaseLong   = 28
	opSymbol     = 29
	opSimple     = 30
	opNumber     = 31 // NUMBER_0_1023, or '-' when the flag is clear
)

// escapeCode is the simpleTable index that introduces a UTF-8 escape.
const escapeCode = 15

// maxEscapeRun is how many raw bytes one escape can carry; the count is stored
// as 2 bits.
const maxEscapeRun = 4

// numberMax is the largest integer opcode 31 can carry in its 10 bits.
const numberMax = 1023

// numberMaxDigits is the digit count of numberMax, and so the longest decimal
// run one NUMBER_0_1023 token can absorb.
const numberMaxDigits = 4

// symTable is the opcode 29 operand table. Entries are literal characters: the
// case mode does not apply to them, so 'Ñ' and 'Á' go through the UTF-8 escape
// rather than through this table.
var symTable = [32]string{
	0: "<", 1: ">", 2: "/", 3: `"`, 4: "'", 5: "%", 6: "#", 7: "|",
	8: "(", 9: ")", 10: "!", 11: "?", 12: "$", 13: "~", 14: "`", 15: "€",
	16: "@", 17: `\`, 18: "[", 19: "]", 20: "^", 21: "{", 22: "}", 23: "_",
	24: "ñ", 25: "á", 26: "é", 27: "í", 28: "ó", 29: "ú",
	30: "", 31: "", // reserved
}

// symReserved is the first unassigned symTable index.
const symReserved = 30

// simpleTable is the opcode 30 operand table. Index 15 is not a character; it
// is escapeCode.
var simpleTable = [16]byte{
	'0', '1', '2', '3', '4', '5', '6', '7', '8', '9',
	'.', '-', '+', '*', '=', 0,
}

// asciiSym and asciiSimple invert the two tables for the single-byte entries.
// A -1 means "this byte has no operand in that table". Multi-byte symTable
// entries (€ ñ á é í ó ú) are matched by runeSymbol instead.
var (
	asciiSym    [128]int8
	asciiSimple [128]int8
)

func init() {
	for i := range asciiSym {
		asciiSym[i] = -1
		asciiSimple[i] = -1
	}
	for i := range symReserved {
		s := symTable[i]
		if len(s) == 1 {
			asciiSym[s[0]] = int8(i)
		}
	}
	for i, c := range simpleTable {
		if i != escapeCode {
			asciiSimple[c] = int8(i)
		}
	}
}

// runeSymbol returns the symTable index of a non-ASCII rune, or -1.
func runeSymbol(r rune) int8 {
	switch r {
	case '€':
		return 15
	case 'ñ':
		return 24
	case 'á':
		return 25
	case 'é':
		return 26
	case 'í':
		return 27
	case 'ó':
		return 28
	case 'ú':
		return 29
	}
	return -1
}

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
