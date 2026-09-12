package stringpack

// u5 is packed5 with two changes and nothing else:
//
//  1. Every token is a whole number of 5-bit units. packed5's 4-bit simple
//     operand, 2-bit escape count and 8-bit raw bytes are what stop its stream
//     from sitting on a unit grid; here the simple table is folded into two
//     32-entry symbol tables, the escape count is one unit, and a raw byte is
//     two. In exchange the payload can be packed eight units at a time.
//  2. There is no planning pass. packed5 prices four flag combinations in a
//     separate scan over the string before it writes anything. u5 has no flags:
//     the case mode always starts lower and is moved by an ordinary
//     CASE_TOGGLE_LONG token, and the number opcode is always a number. One
//     scan, no lookahead beyond the greedy rules themselves.
//
// What that costs in size: a lone digit is 10 bits instead of packed5's 9, and
// '-' is 10 bits instead of the 5 it gets when packed5 turns number mode off.
// What it buys is measured in bench_test.go.
//
// Opcodes:
//
//	0..25  letter a..z, cased by the current mode
//	26     space
//	27     CASE_TOGGLE_SIMPLE   next letter only
//	28     CASE_TOGGLE_LONG     until the next one
//	29     + 1 unit: index into u5SymA
//	30     + 1 unit: index into u5SymB, or 31 = escape
//	31     + 2 units: integer 0..1023
//
// Escape: 30, 31, one unit holding n-1 for n in 1..4, then 2 units per raw
// byte (low five bits, then high three).

import (
	"encoding/binary"
	"slices"
	"strconv"
	"unicode/utf8"
)

const (
	u5Space    = 26
	u5CaseNext = 27
	u5CaseLong = 28
	u5SymA     = 29
	u5SymB     = 30
	u5Number   = 31

	u5Esc          = 31 // u5SymB operand that starts a raw-byte run
	u5MaxEscape    = 4
	u5NumberMax    = 1023
	u5NumberDigits = 4
)

// u5TabA holds what a short record is actually made of after letters and
// spaces: digits first, then the punctuation that survives in names, SKUs and
// sentences.
var u5TabA = [32]byte{
	'0', '1', '2', '3', '4', '5', '6', '7', '8', '9',
	'.', ',', '-', '/', ':', ';', '_', '(', ')', '%',
	'#', '"', '\'', '!', '?', '@', '=', '+', '*', '&', '<', '>',
}

// u5TabB holds the rest, including the accented characters packed5 has to reach
// through its symbol table for. Index 31 is not a character; it is u5Esc.
var u5TabB = [32]string{
	"ñ", "á", "é", "í", "ó", "ú", "ü", "Ñ", "Á", "É", "Í", "Ó", "Ú", "€",
	"$", "~", "`", "\\", "[", "]", "^", "{", "}", "|", "\n", "\t", "\r",
	"°", "ª", "º", "¿",
	"", // 31: escape
}

var (
	u5AsciiA [128]int8
	u5AsciiB [128]int8
	// Every non-ASCII entry of u5TabB is Latin-1 Supplement (C2 or C3 lead)
	// except '€'. Keying on the low six bits of the continuation byte turns the
	// lookup into one array index: continuation bytes are 0x80..0xBF, so within
	// well-formed input those six bits identify the rune outright. That matters
	// because the spanish corpus takes this path on nearly every word, and a
	// utf8.DecodeRuneInString plus a table walk was the whole cost there.
	u5LatinC2 [64]int8
	u5LatinC3 [64]int8
	u5Euro    int8 = -1
)

func init() {
	for i := range u5AsciiA {
		u5AsciiA[i] = -1
		u5AsciiB[i] = -1
	}
	for i := range u5LatinC2 {
		u5LatinC2[i] = -1
		u5LatinC3[i] = -1
	}
	for i, c := range u5TabA {
		u5AsciiA[c] = int8(i)
	}
	for i, s := range u5TabB {
		if i == u5Esc {
			continue
		}
		switch {
		case len(s) == 1:
			u5AsciiB[s[0]] = int8(i)
		case len(s) == 2 && s[0] == 0xC2:
			u5LatinC2[s[1]&0x3F] = int8(i)
		case len(s) == 2 && s[0] == 0xC3:
			u5LatinC3[s[1]&0x3F] = int8(i)
		case s == "€":
			u5Euro = int8(i)
		}
	}
}

// u5Multi returns the u5TabB index of the multi-byte character at i with its
// byte width, or -1.
func u5Multi(s string, i int) (int8, int) {
	if i+1 >= len(s) || s[i+1]&0xC0 != 0x80 {
		return -1, 0
	}
	switch s[i] {
	case 0xC2:
		return u5LatinC2[s[i+1]&0x3F], 2
	case 0xC3:
		return u5LatinC3[s[i+1]&0x3F], 2
	case 0xE2:
		if i+2 < len(s) && s[i+1] == 0x82 && s[i+2] == 0xAC {
			return u5Euro, 3
		}
	}
	return -1, 0
}

func u5Escapes(s string, i int) bool {
	c := s[i]
	if (c|0x20)-'a' < 26 || c == ' ' {
		return false
	}
	if c < utf8.RuneSelf {
		return u5AsciiA[c] < 0 && u5AsciiB[c] < 0
	}
	k, _ := u5Multi(s, i)
	return k < 0
}

// u5Tokenize appends the unit sequence for s. This is the whole encoder except
// for packing: whatever the packing kernel costs, it costs on top of this.
func u5Tokenize(units []uint8, s string) []uint8 {
	cur := false // false = lowercase mode
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case (c|0x20)-'a' < 26:
			idx := (c | 0x20) - 'a'
			if (c&0x20 == 0) == cur {
				units = append(units, idx)
				i++
				continue
			}
			// Two simple toggles cost what a pair of long ones does, so the long
			// form only pays from a run of three.
			run := 0
			for j := i; j < len(s) && (s[j]|0x20)-'a' < 26 && (s[j]&0x20 == 0) != cur; j++ {
				run++
			}
			if run >= 3 {
				units = append(units, u5CaseLong)
				cur = !cur
				continue
			}
			units = append(units, u5CaseNext, idx)
			i++

		case c == ' ':
			units = append(units, u5Space)
			i++

		case c-'0' < 10:
			// The longest decimal prefix ten bits can hold. A leading zero would
			// not survive the round trip through an integer, so only a lone '0'
			// takes the digit path.
			v, best, bestLen := 0, 0, 0
			for l := 1; l <= u5NumberDigits && i+l <= len(s); l++ {
				d := s[i+l-1]
				if d-'0' >= 10 || (l > 1 && s[i] == '0') {
					break
				}
				v = v*10 + int(d-'0')
				if v > u5NumberMax {
					break
				}
				best, bestLen = v, l
			}
			if bestLen == 1 {
				units = append(units, u5SymA, c-'0')
			} else {
				units = append(units, u5Number, uint8(best&31), uint8(best>>5))
			}
			i += bestLen

		default:
			if c < utf8.RuneSelf {
				if k := u5AsciiA[c]; k >= 0 {
					units = append(units, u5SymA, uint8(k))
					i++
					continue
				}
				if k := u5AsciiB[c]; k >= 0 {
					units = append(units, u5SymB, uint8(k))
					i++
					continue
				}
			} else if k, w := u5Multi(s, i); k >= 0 {
				units = append(units, u5SymB, uint8(k))
				i += w
				continue
			}
			n := 1
			for n < u5MaxEscape && i+n < len(s) && u5Escapes(s, i+n) {
				n++
			}
			units = append(units, u5SymB, u5Esc, uint8(n-1))
			for k := range n {
				b := s[i+k]
				units = append(units, b&31, b>>5)
			}
			i += n
		}
	}
	return units
}

// u5Scratch is the stack buffer the encoder tokenises into. Worst case is five
// units per byte (an escaped byte inside a one-byte run), but the strings this
// codec is for do not approach that; a longer string falls back to a heap
// slice rather than making every call carry the worst case on its frame.
const u5Scratch = 1024

// u5Append encodes s as one frame appended to out.
//
// Tokenise into a scratch array, then pack the units in one pass. Two passes
// over the string's worth of data, but the second one is over units rather
// than bytes and is branch-free, which is the trade the whole design is about.
func u5Append(out []byte, s string) []byte {
	if len(s) == 0 {
		return append(out, 0)
	}
	var stack [u5Scratch]uint8
	units := u5Tokenize(stack[:0], s)

	payload := packedLen5(len(units))
	if frameOverhead(payload)+payload >= frameOverhead(len(s))+len(s) {
		return append(appendHeader(out, 0, len(s)), s...)
	}
	drop := payload*8/5 - len(units)
	out = appendHeader(out, hdrPacked|byte(drop)<<hdrDropShif, payload)
	return packWide64(out, units)
}

// u5AppendFused packs as it tokenises, so the units never land in memory. It
// exists to price the scratch array and the second pass against the branchier
// single pass.
func u5AppendFused(out []byte, s string) []byte {
	if len(s) == 0 {
		return append(out, 0)
	}
	// Reserve the header byte. Payloads over 30 bytes need a uvarint too, which
	// is not known until the stream is written; those shift the payload down by
	// one byte at the end. Short records never take that path.
	start := len(out)
	out = append(out, 0)
	var w u5Writer
	w.reset(out, len(s))
	w.tokenize(s)
	out = w.finish()

	payload := len(out) - start - 1
	if frameOverhead(payload)+payload >= frameOverhead(len(s))+len(s) {
		return append(appendHeader(out[:start], 0, len(s)), s...)
	}
	drop := payload*8/5 - w.units
	flags := byte(hdrPacked | byte(drop)<<hdrDropShif)
	if payload <= hdrLenInlin {
		out[start] = flags | byte(payload)<<hdrLenShift
		return out
	}
	// Rare: open a gap for the uvarint and slide the payload up.
	k := uvarintLen(payload)
	out = slices.Grow(out, k)[:len(out)+k]
	copy(out[start+1+k:], out[start+1:len(out)-k])
	out[start] = flags | hdrLenEscap<<hdrLenShift
	appendUvarint(out[start+1:start+1], payload)
	return out
}

// u5Writer is the wide64 kernel with the tokeniser's emit points folded in: a
// unit is a fixed shift into a pending word, and every eighth one stores eight
// bytes and advances five.
type u5Writer struct {
	buf   []byte
	p     int
	acc   uint64
	n     uint8
	units int
}

// reset points the writer at the end of out and reserves the worst case for a
// source of srcLen bytes — five units per byte, which is an escape run of one.
// One growth here takes every bounds-driven reallocation out of the write loop.
func (w *u5Writer) reset(out []byte, srcLen int) {
	out = slices.Grow(out, packedLen5(srcLen*5)+slack)
	w.p = len(out)
	w.buf = out[:cap(out)]
	w.acc, w.n, w.units = 0, 0, 0
}

func (w *u5Writer) put(v uint8) {
	w.acc |= uint64(v) << (5 * w.n)
	w.n++
	if w.n == 8 {
		binary.LittleEndian.PutUint64(w.buf[w.p:], w.acc)
		w.p += 5
		w.acc, w.n = 0, 0
	}
	w.units++
}

func (w *u5Writer) finish() []byte {
	if w.n > 0 {
		binary.LittleEndian.PutUint64(w.buf[w.p:], w.acc)
		w.p += packedLen5(int(w.n))
	}
	return w.buf[:w.p]
}

// tokenize is u5Tokenize with w.put where that one appends. The duplication is
// the point: the two differ only in where a unit goes, so the benchmark gap
// between u5Append and u5AppendFused is the cost of materialising the units.
func (w *u5Writer) tokenize(s string) {
	cur := false
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case (c|0x20)-'a' < 26:
			idx := (c | 0x20) - 'a'
			if (c&0x20 == 0) == cur {
				w.put(idx)
				i++
				continue
			}
			run := 0
			for j := i; j < len(s) && (s[j]|0x20)-'a' < 26 && (s[j]&0x20 == 0) != cur; j++ {
				run++
			}
			if run >= 3 {
				w.put(u5CaseLong)
				cur = !cur
				continue
			}
			w.put(u5CaseNext)
			w.put(idx)
			i++

		case c == ' ':
			w.put(u5Space)
			i++

		case c-'0' < 10:
			v, best, bestLen := 0, 0, 0
			for l := 1; l <= u5NumberDigits && i+l <= len(s); l++ {
				d := s[i+l-1]
				if d-'0' >= 10 || (l > 1 && s[i] == '0') {
					break
				}
				v = v*10 + int(d-'0')
				if v > u5NumberMax {
					break
				}
				best, bestLen = v, l
			}
			if bestLen == 1 {
				w.put(u5SymA)
				w.put(c - '0')
			} else {
				w.put(u5Number)
				w.put(uint8(best & 31))
				w.put(uint8(best >> 5))
			}
			i += bestLen

		default:
			if c < utf8.RuneSelf {
				if k := u5AsciiA[c]; k >= 0 {
					w.put(u5SymA)
					w.put(uint8(k))
					i++
					continue
				}
				if k := u5AsciiB[c]; k >= 0 {
					w.put(u5SymB)
					w.put(uint8(k))
					i++
					continue
				}
			} else if k, wd := u5Multi(s, i); k >= 0 {
				w.put(u5SymB)
				w.put(uint8(k))
				i += wd
				continue
			}
			n := 1
			for n < u5MaxEscape && i+n < len(s) && u5Escapes(s, i+n) {
				n++
			}
			w.put(u5SymB)
			w.put(u5Esc)
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

// u5Decode appends the string held by the frame at the front of buf, returning
// the extended dst and the frame length. buf must carry slack readable bytes
// past the frame.
func u5Decode(dst, buf []byte) ([]byte, int, error) {
	h, err := readFrame(buf)
	if err != nil {
		return dst, 0, err
	}
	if !h.packed {
		return append(dst, h.payload...), h.n, nil
	}
	n := len(h.payload)*8/5 - h.drop
	if n < 0 {
		return dst, 0, errBadLength
	}
	out, err := u5Expand(dst, h.wide, n)
	if err != nil {
		return dst, 0, err
	}
	return out, h.n, nil
}

// u5Expand walks n units out of src and appends their characters to dst.
//
// Units arrive from unpackWide64 eight at a time, so the dispatch below never
// touches the bitstream: it indexes a plain byte slice. That is the decode-side
// half of the alignment argument.
func u5Expand(dst, src []byte, n int) ([]byte, error) {
	var stack [u5Scratch]uint8
	units := unpackWide64(stack[:0], src, n)

	out := dst
	cur, pending := false, false
	for i := 0; i < len(units); i++ {
		u := units[i]
		switch {
		case u < u5Space:
			if cur != pending {
				out = append(out, 'A'+u)
			} else {
				out = append(out, 'a'+u)
			}
			pending = false
		case u == u5Space:
			out = append(out, ' ')
		case u == u5CaseNext:
			pending = true
		case u == u5CaseLong:
			cur = !cur
		case u == u5SymA:
			if i+1 >= len(units) {
				return dst, errTruncated
			}
			i++
			out = append(out, u5TabA[units[i]])
		case u == u5SymB:
			if i+1 >= len(units) {
				return dst, errTruncated
			}
			i++
			k := units[i]
			if k != u5Esc {
				out = append(out, u5TabB[k]...)
				break
			}
			if i+1 >= len(units) {
				return dst, errTruncated
			}
			i++
			cnt := int(units[i]) + 1
			if cnt > u5MaxEscape {
				return dst, errBadUnit
			}
			if i+2*cnt > len(units)-1 {
				return dst, errTruncated
			}
			for range cnt {
				out = append(out, units[i+1]|units[i+2]<<5)
				i += 2
			}
		default: // u5Number
			if i+2 >= len(units) {
				return dst, errTruncated
			}
			v := uint64(units[i+1]) | uint64(units[i+2])<<5
			i += 2
			out = strconv.AppendUint(out, v, 10)
		}
	}
	return out, nil
}
