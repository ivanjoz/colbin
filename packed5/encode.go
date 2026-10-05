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
// It is not size-optimal: the optimum is a shortest path over (offset, case
// mode) nodes, several times slower. encode_test.go keeps that as an oracle and
// bounds the gap, which is zero on realistic strings and small on synthetic
// input where case changes are dense and interleaved with symbols.

// opensUpper picks the case mode the stream starts in, which the container
// carries in one bit.
//
// The rule is the first letter's case, and it is never worse than starting
// lower. Take the leading run of k letters in the opposite case to lower: paying
// for it from lower costs 2k units for k of one or two, and 1+k from three up,
// where starting in that mode costs k plus the one toggle that returns. Those
// are equal at k>=3 and strictly better below it, and after the run both
// encoders are in the same mode with the same string left.
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

// AppendPayload appends the packed unit stream for s onto out and reports the
// payload's byte length and the case mode it opens in. The caller stores both:
// AppendString needs them to read the payload back.
//
// ok is false when the packed form would not be smaller than the raw bytes, in
// which case out is returned with nothing appended and the caller writes s raw.
// The encoder finds that out by writing into a buffer bounded at len(s) and
// noticing when it runs out, so the answer costs a bounds test per group rather
// than a pass of its own.
//
// With eight bytes of spare capacity past len(out)+len(s), it does not allocate.
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
