package packed5

import "slices"

// The encoder is one greedy left-to-right scan, fused with the packer:
//
//   - an opposite-case run of one or two letters takes a CASE_TOGGLE_SIMPLE
//     each; three or more takes a CASE_TOGGLE_LONG.
//   - a decimal run takes the longest prefix the number token can legally carry,
//     except that a lone digit takes the symbol table instead, since ten bits
//     beat fifteen.
//   - a byte with no token of its own is escaped, merged with the following
//     unrepresentable bytes up to the escape's four-byte limit.
//
// There is no planning pass. The format it replaced had two behavioural header
// flags — a default case and a number mode — which changed what the scan emitted
// and so had to be priced before it ran, in a second full walk of the string
// that measured 32% to 46% of encode time. Both are gone: the number token is
// unconditional, and the case mode is carried by an ordinary CASE_TOGGLE_LONG
// that opensUpper hoists into the header bit when it would have landed first.
//
// This is not a size-optimal encoder. The optimum is a shortest path over
// (offset, case mode) nodes, which is exact but several times slower; measured
// against it, this scan is byte-for-byte identical on every corpus of realistic
// short strings tried, and loses a little on synthetic input where case changes
// are dense and interleaved with symbols. encode_test.go keeps a brute-force
// reference and pins that gap.

// opensUpper picks the case mode the stream starts in, which the header carries
// for free in one bit.
//
// The rule is the first letter's case, and it is never worse than starting
// lower. Take the leading run of k letters in the opposite case to lower: paying
// for it from lower costs 2k units for k of one or two, and 1+k from three up,
// where starting in that mode costs k plus the one toggle that returns. Those
// are equal at k>=3 and strictly better below it, and after the run both
// encoders are in the same mode with the same string left.
//
// What it replaces is a header flag whose value could only be found by pricing
// the whole string twice. A scan to the first letter is a few bytes on anything
// this codec is for.
func opensUpper(s string) bool {
	for i := range len(s) {
		if isLetter(s[i]) {
			return isUpper(s[i])
		}
	}
	return false
}

// escapes reports whether the byte at i has no token of its own.
func escapes(s string, i int) bool {
	c := s[i]
	if isLetter(c) || c == ' ' {
		return false
	}
	if c < 0x80 {
		return asciiSym[c] < 0 && asciiExt[c] < 0
	}
	k, _ := extMulti(s, i)
	return k < 0
}

// tokenize walks s and emits its units into w.
func (w *writer) tokenize(s string, upper bool) {
	cur := upper
	for i := 0; i < len(s) && !w.over; {
		c := s[i]
		switch {
		case isLetter(c):
			idx := letterIndex(c)
			if isUpper(c) == cur {
				w.put(idx)
				i++
				continue
			}
			// Two simple toggles cost what a pair of long ones does, so the long
			// form only pays from a run of three.
			run := 0
			for j := i; j < len(s) && isLetter(s[j]) && isUpper(s[j]) != cur; j++ {
				run++
			}
			if run >= 3 {
				w.put(opCaseLong)
				cur = !cur
				continue // re-read the letter, now in the matching mode
			}
			w.put(opCaseSimple)
			w.put(idx)
			i++

		case c == ' ':
			w.put(opSpace)
			i++

		case isDigit(c):
			// The longest prefix ten bits can hold. A token may not carry a
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
				w.put(opSymbol)
				w.put(c - '0')
			} else {
				w.put(opNumber)
				w.put(uint8(best & 31))
				w.put(uint8(best >> 5))
			}
			i += bestLen

		default:
			if c < 0x80 {
				if k := asciiSym[c]; k >= 0 {
					w.put(opSymbol)
					w.put(uint8(k))
					i++
					continue
				}
				if k := asciiExt[c]; k >= 0 {
					w.put(opExt)
					w.put(uint8(k))
					i++
					continue
				}
			} else if k, width := extMulti(s, i); k >= 0 {
				w.put(opExt)
				w.put(uint8(k))
				i += width
				continue
			}
			n := 1
			for n < maxEscapeRun && i+n < len(s) && escapes(s, i+n) {
				n++
			}
			w.put(opExt)
			w.put(extEscape)
			w.put(uint8(n - 1))
			for k := range n {
				b := s[i+k]
				w.put(b & 31)
				w.put(b >> 5)
			}
			i += n
		}
	}
}

// AppendPayload appends the packed unit stream for s onto out — no header, no
// length — and reports the payload's byte length and the case mode it opens in.
//
// ok is false when the packed form would not be smaller than the raw bytes, in
// which case out is returned with nothing appended. The encoder discovers that
// by writing into a buffer bounded at len(s) and noticing when it runs out, so
// the answer costs a bounds test per group rather than a pass of its own.
//
// It exists for a container that already carries a length and a place to record
// the case mode — a colbin BLOB descriptor does both — so that an embedded
// string need not repeat them in a frame header of its own. Append is this plus
// that header.
func AppendPayload(out []byte, s string) (buf []byte, n int, upper, ok bool) {
	if len(s) == 0 {
		return out, 0, false, false
	}
	upper = opensUpper(s)

	// A payload that reaches len(s) has already lost to the raw bytes, so that
	// is both the buffer bound and the point the writer gives up at.
	start := len(out)
	out = slices.Grow(out, len(s)+8)
	w := writer{buf: out[:cap(out)], p: start, limit: start + len(s)}
	w.tokenize(s, upper)
	end, fit := w.finish()
	if !fit || end-start >= len(s) {
		return out, 0, false, false
	}
	return w.buf[:end], end - start, upper, true
}

// Append encodes s as one self-delimiting frame appended to out and returns the
// extended slice.
//
// The packed form is used only when its frame is strictly smaller than the raw
// frame, so Append never inflates a string: the result is never longer than
// len(s) plus the framing bytes.
func Append(out []byte, s string) []byte {
	if len(s) == 0 {
		return append(out, 0) // raw, empty payload
	}
	// One byte of header is reserved before the payload so that the stream can
	// be written straight into the caller's slice. Reserving is what a fused
	// encoder trades for the planning pass it does not run: the length is known
	// after the walk rather than before it, and the rare payload that outgrows
	// the header's five length bits pays a memmove for the uvarint.
	start := len(out)
	out = append(out, 0)
	// ok already means the payload is strictly shorter than the raw bytes, and
	// frameOverhead never shrinks as a length grows, so the packed frame is
	// smaller whenever the payload is.
	out, n, upper, ok := AppendPayload(out, s)
	if !ok {
		return append(appendHeader(out[:start], 0, len(s)), s...)
	}

	flags := byte(flagPacked5)
	if upper {
		flags |= flagUppercase
	}
	if n <= lenInline {
		out[start] = flags | byte(n)<<lenShift
		return out
	}
	k := uvarintLen(n)
	out = slices.Grow(out, k)[:len(out)+k]
	copy(out[start+1+k:], out[start+1:len(out)-k])
	out[start] = flags | lenEscape<<lenShift
	appendUvarint(out[start+1:start+1], n)
	return out
}

// Size returns the number of bytes Append would add for s.
//
// The walk is the encoding, so this packs into a scratch buffer and keeps only
// the length. A caller that wants the size and then the bytes should use
// AppendPayload, which produces both from one pass.
func Size(s string) int {
	if len(s) == 0 {
		return 1
	}
	raw := frameOverhead(len(s)) + len(s)
	// The walk is the encoding, so this packs the payload and keeps only its
	// length. It builds its own writer rather than calling AppendPayload so that
	// the scratch stays on the stack: a buffer handed to a function that returns
	// it escapes, and a 256-byte allocation per call is not what Size is for.
	var stack [sizeScratchBytes]byte
	buf := stack[:]
	if need := len(s) + 8; need > len(buf) {
		buf = make([]byte, need)
	}
	w := writer{buf: buf, limit: len(s)}
	w.tokenize(s, opensUpper(s))
	n, ok := w.finish()
	if !ok {
		return raw
	}
	if packed := frameOverhead(n) + n; packed < raw {
		return packed
	}
	return raw
}

// sizeScratchBytes is the stack buffer Size packs into. A payload is never
// larger than the string, so this covers every string up to 248 bytes.
const sizeScratchBytes = 256

// appendHeader writes the header byte and, when the payload length outgrows the
// header's five length bits, the uvarint that carries it.
func appendHeader(out []byte, flags byte, payloadLen int) []byte {
	if payloadLen <= lenInline {
		return append(out, flags|byte(payloadLen)<<lenShift)
	}
	out = append(out, flags|lenEscape<<lenShift)
	return appendUvarint(out, payloadLen)
}
