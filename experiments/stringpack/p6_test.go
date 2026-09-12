package stringpack

// p6 is the other end of the design space: give up on being clever and spend
// six bits on every character.
//
// Sixty-four slots are enough for both letter cases, the space and the ten
// digits outright. That deletes the case toggles, the case mode, both of
// packed5's header flags and therefore the whole planning pass. Encoding is one
// table lookup per source byte with no state and no lookahead; decoding is one
// table lookup per unit. Eight units are 48 bits, so the packer is the same
// shape as wide64: build a word, store eight bytes, advance six.
//
// Flat cost is 0.75 bytes per character against packed5's 0.625 floor, but
// packed5 only reaches its floor on unbroken lowercase. Every capital costs it
// a toggle and every symbol an operand, which is why its measured ratios sit at
// 0.73..0.89. Whether 0.75-and-no-thinking beats that is the question the
// benchmark answers.
//
//	0..25   'a'..'z'
//	26..51  'A'..'Z'
//	52      ' '
//	53..62  '0'..'9'
//	63      escape + 1 unit: index into p6Tab2, or 63 = raw byte + 2 units

import (
	"encoding/binary"
	"slices"
	"unicode/utf8"
)

const (
	p6Space = 52
	p6Digit = 53
	p6Esc   = 63
	p6Raw   = 63 // p6Tab2 index that introduces a raw byte
)

var p6Tab2 = [64]string{
	".", ",", "-", "/", ":", ";", "_", "(", ")", "%",
	"#", "\"", "'", "!", "?", "@", "=", "+", "*", "&",
	"<", ">", "[", "]", "{", "}", "|", "\\", "^", "~",
	"`", "$", "\n", "\t", "\r",
	"ñ", "á", "é", "í", "ó", "ú", "ü", "Ñ", "Á", "É", "Í", "Ó", "Ú",
	"€", "°", "ª", "º", "¿", "¡",
	// 54..62 unassigned, 63 is p6Raw.
}

// p6Direct maps a byte to its unit, or 0xFF when it needs the escape. It is the
// encoder's entire state machine.
var p6Direct [256]uint8

// p6Ascii2 maps a byte to its p6Tab2 index, or -1. p6LatinC2/C3 do the same for
// the two-byte runes, keyed on the low six bits of the continuation byte; see
// u5Multi for why that is enough.
var (
	p6Ascii2  [128]int8
	p6LatinC2 [64]int8
	p6LatinC3 [64]int8
	p6Euro    int8 = -1
)

// p6Unit maps a unit back to the bytes it stands for. Two-byte entries and the
// escape are handled outside it.
var p6Chars [64]byte

func init() {
	for i := range p6Direct {
		p6Direct[i] = 0xFF
	}
	for i := range 26 {
		p6Direct['a'+i] = uint8(i)
		p6Direct['A'+i] = uint8(26 + i)
		p6Chars[i] = byte('a' + i)
		p6Chars[26+i] = byte('A' + i)
	}
	p6Direct[' '] = p6Space
	p6Chars[p6Space] = ' '
	for i := range 10 {
		p6Direct['0'+i] = uint8(p6Digit + i)
		p6Chars[p6Digit+i] = byte('0' + i)
	}
	for i := range p6Ascii2 {
		p6Ascii2[i] = -1
	}
	for i := range p6LatinC2 {
		p6LatinC2[i] = -1
		p6LatinC3[i] = -1
	}
	for i, s := range p6Tab2 {
		if i == p6Raw {
			continue
		}
		switch {
		case len(s) == 1:
			p6Ascii2[s[0]] = int8(i)
		case len(s) == 2 && s[0] == 0xC2:
			p6LatinC2[s[1]&0x3F] = int8(i)
		case len(s) == 2 && s[0] == 0xC3:
			p6LatinC3[s[1]&0x3F] = int8(i)
		case s == "€":
			p6Euro = int8(i)
		}
	}
}

func p6Multi(s string, i int) (int8, int) {
	if i+1 >= len(s) || s[i+1]&0xC0 != 0x80 {
		return -1, 0
	}
	switch s[i] {
	case 0xC2:
		return p6LatinC2[s[i+1]&0x3F], 2
	case 0xC3:
		return p6LatinC3[s[i+1]&0x3F], 2
	case 0xE2:
		if i+2 < len(s) && s[i+1] == 0x82 && s[i+2] == 0xAC {
			return p6Euro, 3
		}
	}
	return -1, 0
}

func packedLen6(n int) int { return (n*6 + 7) / 8 }

// p6Writer packs eight 6-bit units into 48 bits: store eight bytes, advance
// six.
type p6Writer struct {
	buf   []byte
	p     int
	acc   uint64
	n     uint8
	units int
}

func (w *p6Writer) reset(out []byte, srcLen int) {
	// Three units per source byte is the raw-escape worst case.
	out = slices.Grow(out, packedLen6(srcLen*3)+slack)
	w.p = len(out)
	w.buf = out[:cap(out)]
	w.acc, w.n, w.units = 0, 0, 0
}

func (w *p6Writer) put(v uint8) {
	w.acc |= uint64(v) << (6 * w.n)
	w.n++
	if w.n == 8 {
		binary.LittleEndian.PutUint64(w.buf[w.p:], w.acc)
		w.p += 6
		w.acc, w.n = 0, 0
	}
	w.units++
}

func (w *p6Writer) finish() []byte {
	if w.n > 0 {
		binary.LittleEndian.PutUint64(w.buf[w.p:], w.acc)
		w.p += packedLen6(int(w.n))
	}
	return w.buf[:w.p]
}

func p6Append(out []byte, s string) []byte {
	if len(s) == 0 {
		return append(out, 0)
	}
	start := len(out)
	out = append(out, 0)
	var w p6Writer
	w.reset(out, len(s))

	for i := 0; i < len(s); {
		c := s[i]
		if u := p6Direct[c]; u != 0xFF {
			w.put(u)
			i++
			continue
		}
		if c < utf8.RuneSelf {
			if k := p6Ascii2[c]; k >= 0 {
				w.put(p6Esc)
				w.put(uint8(k))
				i++
				continue
			}
		} else if k, wd := p6Multi(s, i); k >= 0 {
			w.put(p6Esc)
			w.put(uint8(k))
			i += wd
			continue
		}
		w.put(p6Esc)
		w.put(p6Raw)
		w.put(c & 63)
		w.put(c >> 6)
		i++
	}
	out = w.finish()

	payload := len(out) - start - 1
	if frameOverhead(payload)+payload >= frameOverhead(len(s))+len(s) {
		return append(appendHeader(out[:start], 0, len(s)), s...)
	}
	drop := payload*8/6 - w.units
	flags := byte(hdrPacked | byte(drop)<<hdrDropShif)
	if payload <= hdrLenInlin {
		out[start] = flags | byte(payload)<<hdrLenShift
		return out
	}
	k := uvarintLen(payload)
	out = slices.Grow(out, k)[:len(out)+k]
	copy(out[start+1+k:], out[start+1:len(out)-k])
	out[start] = flags | hdrLenEscap<<hdrLenShift
	appendUvarint(out[start+1:start+1], payload)
	return out
}

func p6Unpack(dst []uint8, src []byte, n int) []uint8 {
	p, i := 0, 0
	for ; i+8 <= n; i += 8 {
		w := binary.LittleEndian.Uint64(src[p:])
		dst = append(dst,
			uint8(w)&63, uint8(w>>6)&63, uint8(w>>12)&63, uint8(w>>18)&63,
			uint8(w>>24)&63, uint8(w>>30)&63, uint8(w>>36)&63, uint8(w>>42)&63)
		p += 6
	}
	if i < n {
		w := binary.LittleEndian.Uint64(src[p:])
		for ; i < n; i++ {
			dst = append(dst, uint8(w)&63)
			w >>= 6
		}
	}
	return dst
}

func p6Decode(dst, buf []byte) ([]byte, int, error) {
	h, err := readFrame(buf)
	if err != nil {
		return dst, 0, err
	}
	if !h.packed {
		return append(dst, h.payload...), h.n, nil
	}
	n := len(h.payload)*8/6 - h.drop
	if n < 0 {
		return dst, 0, errBadLength
	}
	var stack [u5Scratch]uint8
	units := p6Unpack(stack[:0], h.wide, n)

	out := dst
	for i := 0; i < len(units); i++ {
		u := units[i]
		if u != p6Esc {
			out = append(out, p6Chars[u])
			continue
		}
		if i+1 >= len(units) {
			return dst, 0, errTruncated
		}
		i++
		k := units[i]
		if k != p6Raw {
			out = append(out, p6Tab2[k]...)
			continue
		}
		if i+2 >= len(units) {
			return dst, 0, errTruncated
		}
		out = append(out, units[i+1]|units[i+2]<<6)
		i += 2
	}
	return out, h.n, nil
}
