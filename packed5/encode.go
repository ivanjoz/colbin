package packed5

import (
	"sync"
	"unicode/utf8"
)

// The encoder is a single greedy left-to-right scan, following the rules the
// specification lays out:
//
//   - an opposite-case run of one or two letters takes a CASE_TOGGLE_SIMPLE
//     each; three or more takes a CASE_TOGGLE_LONG.
//   - a decimal run takes the longest prefix NUMBER_0_1023 can legally carry,
//     except that a lone digit takes the cheaper 4-bit simple symbol instead.
//   - a byte with no token of its own is escaped, merged with the following
//     unrepresentable bytes up to the escape's four-byte limit.
//
// The two behavioural header flags are not guessed. UPPERCASE_DOMINANT and
// ENABLE_NUMBER_0_1023 change what the scan emits, so the scan is simply run
// once per candidate setting and the cheapest kept: two passes for most
// strings, four for those holding a decimal run. Counting letters to pick the
// dominant case is the obvious shortcut and it is wrong often enough to matter
// — what decides the flag is the number of case *runs*, not of letters.
//
// This is not a size-optimal encoder. The optimum is a shortest path over
// (offset, case mode) nodes, which is exact but roughly 2.3x slower; measured
// against it, this scan is byte-for-byte identical on every corpus of realistic
// short strings tried, and loses at most a couple of percent on synthetic input
// where case changes are dense and interleaved with symbols. encode_test.go
// keeps a brute-force reference and pins that gap.

// Token kinds. Each is one token, except edLetterCased, which is a
// CASE_TOGGLE_SIMPLE together with the letter it applies to.
const (
	edLetter uint8 = iota // pay = letter index 0..25
	edLetterCased
	edSpace
	edToggleLong
	edSymbol // pay = symTable index
	edSimple // pay = simpleTable index
	edDash   // opcode 31 as '-', number mode off
	edNumber // pay = value 0..1023
	edEscape // pay = byte count 1..4, src = offset of the first byte
)

// Token bit costs. A letter, a space, a long toggle and a bare '-' are a lone
// 5-bit opcode; every other token carries an operand.
const (
	costLetter      = 5
	costLetterCased = 10 // CASE_TOGGLE_SIMPLE + letter
	costSpace       = 5
	costToggleLong  = 5
	costSymbol      = 10 // opcode + 5-bit index
	costSimple      = 9  // opcode + 4-bit index
	costDash        = 5
	costNumber      = 15 // opcode + 10-bit integer
	costEscapeBase  = 11 // opcode + escape code + 2-bit count
	costEscapeByte  = 8
)

// padBitsWidth is the width of the pad count that opens a packed stream.
const padBitsWidth = 3

// token is one emitted unit of the packed stream.
type token struct {
	src  int32 // offset of the first source byte; only edEscape reads it
	pay  uint16
	kind uint8
}

// stackLimit is the input length up to which a scan runs entirely on the
// stack. Packed-5 targets short strings, so the common case allocates nothing
// beyond the caller's output slice.
const stackLimit = 64

// A scan emits at most two tokens per byte: a long toggle, then the letter that
// triggered it.
const scratchTokens = 2 * stackLimit

// tokenPool backs strings past stackLimit, mirroring the parent package's
// column scratch pools. Nothing needs clearing on reuse: scan truncates the
// buffer and appends, so it only ever reads back what it just wrote.
var tokenPool = sync.Pool{New: func() any { s := make([]token, 0, 2*stackLimit); return &s }}

// isLetter reports whether c is an ASCII letter of either case.
func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// symbolAt returns the non-letter, non-space token for the byte at i with its
// width in bytes and bit cost, or ok=false when the byte must be escaped.
func symbolAt(s string, i int, number bool) (t token, width, cost int, ok bool) {
	c := s[i]
	if c < utf8.RuneSelf {
		// With number mode off, opcode 31 carries '-' for 5 bits rather than
		// the 9 the simple table would charge.
		if !number && c == '-' {
			return token{kind: edDash}, 1, costDash, true
		}
		if idx := asciiSym[c]; idx >= 0 {
			return token{kind: edSymbol, pay: uint16(idx)}, 1, costSymbol, true
		}
		if idx := asciiSimple[c]; idx >= 0 {
			return token{kind: edSimple, pay: uint16(idx)}, 1, costSimple, true
		}
		return token{}, 0, 0, false
	}
	// A width of 1 here is RuneError from an invalid byte, which has no symbol.
	if r, w := utf8.DecodeRuneInString(s[i:]); w > 1 {
		if idx := runeSymbol(r); idx >= 0 {
			return token{kind: edSymbol, pay: uint16(idx)}, w, costSymbol, true
		}
	}
	return token{}, 0, 0, false
}

// mustEscape reports whether the byte at i has no token of its own.
func mustEscape(s string, i int, number bool) bool {
	if isLetter(s[i]) || s[i] == ' ' {
		return false
	}
	_, _, _, ok := symbolAt(s, i, number)
	return !ok
}

// scan tokenises s under one setting of the two behavioural flags, appending
// onto dst[:0] and returning the tokens with their total bit cost.
func scan(dst []token, s string, upper, number bool) ([]token, int) {
	toks, bits := dst[:0], 0
	emit := func(t token, cost int) {
		toks = append(toks, t)
		bits += cost
	}
	cur := upper
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case isLetter(c):
			isUp := c >= 'A' && c <= 'Z'
			idx := uint16(c - 'a')
			if isUp {
				idx = uint16(c - 'A')
			}
			if isUp == cur {
				emit(token{kind: edLetter, pay: idx}, costLetter)
				i++
				continue
			}
			// Length of the consecutive opposite-case letter run at i. Two
			// simple toggles cost the same as a pair of long ones, so the long
			// form only wins from three.
			run := 0
			for j := i; j < len(s) && isLetter(s[j]) && (s[j] >= 'A' && s[j] <= 'Z') != cur; j++ {
				run++
			}
			if run >= 3 {
				emit(token{kind: edToggleLong}, costToggleLong)
				cur = !cur
				continue // re-read the letter, now in the matching mode
			}
			emit(token{kind: edLetterCased, pay: idx}, costLetterCased)
			i++
		case c == ' ':
			emit(token{kind: edSpace}, costSpace)
			i++
		case number && c >= '0' && c <= '9':
			// The longest prefix that fits in ten bits. A token may not carry a
			// leading zero, since it decodes as a plain decimal integer and
			// "00123" must not come back as "123"; a lone "0" is fine.
			v, best, bestLen := 0, 0, 0
			for l := 1; l <= numberMaxDigits && i+l <= len(s); l++ {
				d := s[i+l-1]
				if d < '0' || d > '9' || (l > 1 && s[i] == '0') {
					break
				}
				v = v*10 + int(d-'0')
				if v > numberMax {
					break
				}
				best, bestLen = v, l
			}
			if bestLen == 1 {
				emit(token{kind: edSimple, pay: uint16(asciiSimple[c])}, costSimple)
			} else {
				emit(token{kind: edNumber, pay: uint16(best)}, costNumber)
			}
			i += bestLen
		default:
			if t, w, cost, ok := symbolAt(s, i, number); ok {
				emit(t, cost)
				i += w
				continue
			}
			n := 1
			for n < maxEscapeRun && i+n < len(s) && mustEscape(s, i+n, number) {
				n++
			}
			emit(token{kind: edEscape, pay: uint16(n), src: int32(i)}, costEscapeBase+costEscapeByte*n)
			i += n
		}
	}
	return toks, bits
}

// worthNumberMode reports whether s holds a decimal run of at least two digits,
// the only thing NUMBER_0_1023 can profit from. A single digit costs 15 bits as
// an integer against 9 as a simple symbol, so without such a run the flag can
// only lose: it also gives up the 5-bit '-' on opcode 31. Skipping those two
// candidate scans is the difference between two passes and four.
func worthNumberMode(s string) bool {
	run := 0
	for i := range len(s) {
		if s[i] >= '0' && s[i] <= '9' {
			run++
			if run >= 2 {
				return true
			}
			continue
		}
		run = 0
	}
	return false
}

// plan scans s under every candidate flag setting and returns the cheapest,
// along with the flags that produced it. buf is scratch, reused by every
// candidate; the tokens are deliberately not returned, since each scan
// overwrites the one before it. The caller re-scans the winning setting when it
// actually needs the tokens.
func plan(buf []token, s string) (bits int, upper, number bool) {
	_, bits = scan(buf, s, false, false)
	try := func(u, n bool) {
		if _, b := scan(buf, s, u, n); b < bits {
			bits, upper, number = b, u, n
		}
	}
	try(true, false)
	if worthNumberMode(s) {
		try(false, true)
		try(true, true)
	}
	return bits, upper, number
}

// payloadBytes is the packed payload size for a token cost of bits.
func payloadBytes(bits int) int { return (padBitsWidth + bits + 7) / 8 }

// Append encodes s as one self-delimiting frame appended to out and returns the
// extended slice.
//
// The packed form is used only when its frame is strictly smaller than the raw
// UTF-8 frame, so Append never inflates a string: the result is never longer
// than len(s) plus the framing bytes.
func Append(out []byte, s string) []byte {
	if len(s) == 0 {
		return append(out, 0) // raw, empty payload
	}
	var arr [scratchTokens]token
	buf := arr[:0]
	if need := 2 * len(s); need > len(arr) {
		p := tokenPool.Get().(*[]token)
		defer tokenPool.Put(p)
		if cap(*p) < need {
			*p = make([]token, 0, need)
		}
		buf = (*p)[:0]
	}

	bits, upper, number := plan(buf, s)
	payload := payloadBytes(bits)
	if frameOverhead(payload)+payload >= frameOverhead(len(s))+len(s) {
		return append(appendHeader(out, 0, len(s)), s...)
	}
	toks, _ := scan(buf, s, upper, number)

	flags := byte(flagPacked5)
	if upper {
		flags |= flagUppercase
	}
	if number {
		flags |= flagNumber
	}
	out = appendHeader(out, flags, payload)

	w := bitWriter{buf: out}
	w.writeBits(uint32(payload*8-padBitsWidth-bits), padBitsWidth)
	for _, t := range toks {
		writeToken(&w, s, t)
	}
	return w.flush()
}

// Size returns the number of bytes Append would add for s, without encoding it.
func Size(s string) int {
	if len(s) == 0 {
		return 1
	}
	var arr [scratchTokens]token
	buf := arr[:0]
	if need := 2 * len(s); need > len(arr) {
		p := tokenPool.Get().(*[]token)
		defer tokenPool.Put(p)
		if cap(*p) < need {
			*p = make([]token, 0, need)
		}
		buf = (*p)[:0]
	}

	bits, _, _ := plan(buf, s)
	payload := payloadBytes(bits)
	raw := frameOverhead(len(s)) + len(s)
	if packed := frameOverhead(payload) + payload; packed < raw {
		return packed
	}
	return raw
}

// appendHeader writes the header byte and, when the payload length outgrows the
// header nibble, the uvarint that carries it.
func appendHeader(out []byte, flags byte, payloadLen int) []byte {
	if payloadLen <= lenInline {
		return append(out, flags|byte(payloadLen)<<lenShift)
	}
	out = append(out, flags|lenEscape<<lenShift)
	return appendUvarint(out, payloadLen)
}

// writeToken emits one token of the chosen tokenisation.
func writeToken(w *bitWriter, s string, t token) {
	switch t.kind {
	case edLetter:
		w.writeBits(uint32(t.pay), 5)
	case edLetterCased:
		w.writeBits(opCaseSimple, 5)
		w.writeBits(uint32(t.pay), 5)
	case edSpace:
		w.writeBits(opSpace, 5)
	case edToggleLong:
		w.writeBits(opCaseLong, 5)
	case edSymbol:
		w.writeBits(opSymbol, 5)
		w.writeBits(uint32(t.pay), 5)
	case edSimple:
		w.writeBits(opSimple, 5)
		w.writeBits(uint32(t.pay), 4)
	case edDash:
		w.writeBits(opNumber, 5)
	case edNumber:
		w.writeBits(opNumber, 5)
		w.writeBits(uint32(t.pay), 10)
	case edEscape:
		w.writeBits(opSimple, 5)
		w.writeBits(escapeCode, 4)
		w.writeBits(uint32(t.pay)-1, 2)
		for k := range int(t.pay) {
			w.writeBits(uint32(s[int(t.src)+k]), 8)
		}
	}
}
