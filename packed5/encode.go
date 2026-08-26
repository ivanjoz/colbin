package packed5

import (
	"slices"
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
// ENABLE_NUMBER_0_1023 change what the scan emits, so plan computes the exact
// cost of all four settings in one pass and picks the cheapest. Counting letters
// to pick the dominant case is the obvious shortcut and it is wrong often enough
// to matter — what decides the flag is the number of case *runs*, not of letters.
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

// isLetter reports whether c is an ASCII letter of either case.
//
// Setting bit 5 folds 'A'..'Z' onto 'a'..'z' and moves nothing else into that
// range, so one unsigned range test decides both cases at once. The classifier
// runs on every input byte in both the planning and the writing pass, and the
// four-comparison form was measurably visible in the encoder's profile.
func isLetter(c byte) bool { return (c|0x20)-'a' < 26 }

// isDigit reports whether c is an ASCII decimal digit. Unsigned wraparound
// turns the pair of comparisons into one.
func isDigit(c byte) bool { return c-'0' < 10 }

// isUpper reports the case of a byte already known to be an ASCII letter: for
// those, bit 5 is the case bit.
func isUpper(c byte) bool { return c&0x20 == 0 }

// letterIndex is the 0..25 alphabet position of an ASCII letter, either case.
func letterIndex(c byte) uint32 { return uint32(c|0x20) - 'a' }

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

// writeStream performs the greedy walk and emits each token as bits at the
// point it is decided.
//
// This is the same walk the reference tokeniser in tokens_test.go makes; the
// token slice it produces only ever existed to carry the walk's output to the
// writer, so materialising it cost a second pass and up to 2*len(s) slots of
// scratch — a stack array for short strings and a pool for the rest. Both are
// gone. TestWriteStreamMatchesTokenWriter pins the two against each other.
func writeStream(w *bitWriter, s string, upper, number bool) {
	cur := upper
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case isLetter(c):
			isUp := isUpper(c)
			idx := letterIndex(c)
			if isUp == cur {
				w.writeBits(idx, 5)
				i++
				continue
			}
			// Length of the consecutive opposite-case letter run at i. Two
			// simple toggles cost the same as a pair of long ones, so the long
			// form only wins from three.
			run := 0
			for j := i; j < len(s) && isLetter(s[j]) && isUpper(s[j]) != cur; j++ {
				run++
			}
			if run >= 3 {
				w.writeBits(opCaseLong, 5)
				cur = !cur
				continue // re-read the letter, now in the matching mode
			}
			w.writeBits(opCaseSimple, 5)
			w.writeBits(idx, 5)
			i++
		case c == ' ':
			w.writeBits(opSpace, 5)
			i++
		case number && isDigit(c):
			// The longest prefix that fits in ten bits. A token may not carry a
			// leading zero, since it decodes as a plain decimal integer and
			// "00123" must not come back as "123"; a lone "0" is fine.
			v, best, bestLen := 0, 0, 0
			for l := 1; l <= numberMaxDigits && i+l <= len(s); l++ {
				d := s[i+l-1]
				if !isDigit(d) || (l > 1 && s[i] == '0') {
					break
				}
				v = v*10 + int(d-'0')
				if v > numberMax {
					break
				}
				best, bestLen = v, l
			}
			if bestLen == 1 {
				w.writeBits(opSimple, 5)
				w.writeBits(uint32(asciiSimple[c]), 4)
			} else {
				w.writeBits(opNumber, 5)
				w.writeBits(uint32(best), 10)
			}
			i += bestLen
		default:
			if t, width, _, ok := symbolAt(s, i, number); ok {
				switch t.kind {
				case edDash:
					w.writeBits(opNumber, 5)
				case edSymbol:
					w.writeBits(opSymbol, 5)
					w.writeBits(uint32(t.pay), 5)
				default: // edSimple
					w.writeBits(opSimple, 5)
					w.writeBits(uint32(t.pay), 4)
				}
				i += width
				continue
			}
			n := 1
			for n < maxEscapeRun && i+n < len(s) && mustEscape(s, i+n, number) {
				n++
			}
			w.writeBits(opSimple, 5)
			w.writeBits(escapeCode, 4)
			w.writeBits(uint32(n)-1, 2)
			for k := range n {
				w.writeBits(uint32(s[i+k]), 8)
			}
			i += n
		}
	}
}

// plan computes the exact greedy-scan cost for all four flag combinations in
// one pass. Case costs depend only on homogeneous ASCII-letter runs; number mode
// affects only decimal runs and the spelling of '-'. Keeping those two parts
// separate avoids materialising tokens for candidates that will be discarded.
func plan(s string) (bits int, upper, number bool) {
	lowerCaseBits, upperCaseBits := 0, 0
	lowerMode, upperMode := false, true
	plainBits, numberBits := 0, 0

	for i := 0; i < len(s); {
		c := s[i]
		if isLetter(c) {
			runUpper := isUpper(c)
			j := i + 1
			for j < len(s) && isLetter(s[j]) && isUpper(s[j]) == runUpper {
				j++
			}
			run := j - i
			if runUpper == lowerMode {
				lowerCaseBits += costLetter * run
			} else if run >= 3 {
				lowerCaseBits += costToggleLong + costLetter*run
				lowerMode = runUpper
			} else {
				lowerCaseBits += costLetterCased * run
			}
			if runUpper == upperMode {
				upperCaseBits += costLetter * run
			} else if run >= 3 {
				upperCaseBits += costToggleLong + costLetter*run
				upperMode = runUpper
			} else {
				upperCaseBits += costLetterCased * run
			}
			i = j
			continue
		}

		if c == ' ' {
			plainBits += costSpace
			numberBits += costSpace
			i++
			continue
		}

		if isDigit(c) {
			j := i + 1
			for j < len(s) && isDigit(s[j]) {
				j++
			}
			plainBits += costSimple * (j - i)
			for i < j {
				v, bestLen := 0, 0
				for l := 1; l <= numberMaxDigits && i+l <= j; l++ {
					if l > 1 && s[i] == '0' {
						break
					}
					v = v*10 + int(s[i+l-1]-'0')
					if v > numberMax {
						break
					}
					bestLen = l
				}
				if bestLen == 1 {
					numberBits += costSimple
				} else {
					numberBits += costNumber
				}
				i += bestLen
			}
			continue
		}

		_, width, cost, ok := symbolAt(s, i, false)
		if ok {
			plainBits += cost
			if c == '-' {
				numberBits += costSimple
			} else {
				numberBits += cost
			}
			i += width
			continue
		}

		n := 1
		for n < maxEscapeRun && i+n < len(s) && mustEscape(s, i+n, false) {
			n++
		}
		cost = costEscapeBase + costEscapeByte*n
		plainBits += cost
		numberBits += cost
		i += n
	}

	bits = lowerCaseBits + plainBits
	if candidate := upperCaseBits + plainBits; candidate < bits {
		bits, upper = candidate, true
	}
	if candidate := lowerCaseBits + numberBits; candidate < bits {
		bits, upper, number = candidate, false, true
	}
	if candidate := upperCaseBits + numberBits; candidate < bits {
		bits, upper, number = candidate, true, true
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
	bits, upper, number := plan(s)
	payload := payloadBytes(bits)
	if frameOverhead(payload)+payload >= frameOverhead(len(s))+len(s) {
		return append(appendHeader(out, 0, len(s)), s...)
	}

	flags := byte(flagPacked5)
	if upper {
		flags |= flagUppercase
	}
	if number {
		flags |= flagNumber
	}
	out = appendHeader(out, flags, payload)

	// The stream is exactly payload bytes, so one growth replaces the repeated
	// reallocation an unsized append would do while the tokens are written.
	w := bitWriter{buf: slices.Grow(out, payload)}
	w.writeBits(uint32(payload*8-padBitsWidth-bits), padBitsWidth)
	writeStream(&w, s, upper, number)
	return w.flush()
}

// Size returns the number of bytes Append would add for s, without encoding it.
func Size(s string) int {
	if len(s) == 0 {
		return 1
	}
	bits, _, _ := plan(s)
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
