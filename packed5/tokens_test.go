package packed5

import (
	"math/rand/v2"
	"sync"
	"testing"
)

// The reference tokeniser. Until the encoder was fused, this was how Append
// worked: scan built the whole token list, then writeToken walked it emitting
// bits. writeStream now does both in one pass, and this pair stays as the
// independent statement of what the greedy walk is supposed to produce —
// TestWriteStreamMatchesTokenWriter holds the two to byte-identical output, and
// the plan tests price tokenisations against it.

// stackLimit is the input length up to which a reference scan runs entirely on
// the stack. Packed-5 targets short strings.
const stackLimit = 64

// A scan emits at most two tokens per byte: a long toggle, then the letter that
// triggered it.
const scratchTokens = 2 * stackLimit

// tokenPool backs strings past stackLimit. Nothing needs clearing on reuse:
// scan truncates the buffer and appends, so it only ever reads back what it
// just wrote.
var tokenPool = sync.Pool{New: func() any { s := make([]token, 0, 2*stackLimit); return &s }}

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

// referenceAppend is Append as it was before the walk and the writer were
// fused: a full tokenisation, then a pass over the tokens.
func referenceAppend(out []byte, s string) []byte {
	if len(s) == 0 {
		return append(out, 0)
	}
	bits, upper, number := plan(s)
	payload := payloadBytes(bits)
	if frameOverhead(payload)+payload >= frameOverhead(len(s))+len(s) {
		return append(appendHeader(out, 0, len(s)), s...)
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

// TestWriteStreamMatchesTokenWriter is the guard on the fusion: the one-pass
// writer must emit the same frame, byte for byte, as building the token list
// and walking it. Random bytes rather than text, so escape runs, invalid UTF-8
// and dense case changes are all covered, and every length up to the point
// where the reference leaves its stack buffer.
func TestWriteStreamMatchesTokenWriter(t *testing.T) {
	rng := rand.New(rand.NewPCG(97, 31))
	pools := [][]string{alphabet, {"a", "B", " ", "1", "2", "\u00f1", "-"}, {"x", "X", "0", "9", "@", "\x00"}}
	for n := 0; n <= 2*stackLimit; n++ {
		for range 8 {
			raw := make([]byte, n)
			for i := range raw {
				raw[i] = byte(rng.Uint32())
			}
			cases := []string{string(raw), randString(rng, pools[n%len(pools)], n)}
			for _, s := range cases {
				got := Append([]byte("prefix"), s)
				want := referenceAppend([]byte("prefix"), s)
				if string(got) != string(want) {
					t.Fatalf("%q: fused = %x, reference = %x", s, got, want)
				}
			}
		}
	}
}

// TestByteClassifiers checks the branchless classifiers against their plain
// definitions over the whole byte range. They fold case and lean on unsigned
// wraparound, so a single stray value would silently mis-tokenise rather than
// fail loudly.
func TestByteClassifiers(t *testing.T) {
	for i := range 256 {
		c := byte(i)
		wantLetter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		if isLetter(c) != wantLetter {
			t.Errorf("isLetter(%d) = %v, want %v", c, isLetter(c), wantLetter)
		}
		if want := c >= '0' && c <= '9'; isDigit(c) != want {
			t.Errorf("isDigit(%d) = %v, want %v", c, isDigit(c), want)
		}
		if !wantLetter {
			continue
		}
		if want := c >= 'A' && c <= 'Z'; isUpper(c) != want {
			t.Errorf("isUpper(%d) = %v, want %v", c, isUpper(c), want)
		}
		want := uint32(c - 'a')
		if c <= 'Z' {
			want = uint32(c - 'A')
		}
		if letterIndex(c) != want {
			t.Errorf("letterIndex(%d) = %d, want %d", c, letterIndex(c), want)
		}
	}
}
