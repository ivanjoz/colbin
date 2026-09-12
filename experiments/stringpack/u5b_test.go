package stringpack

// u5b is u5 with one token changed.
//
// u5 lost 5.5% to packed5 on the sku corpus, and the dash was the obvious
// suspect. It is not. "SKU-0042-hola" costs both codecs two dashes, but it also
// costs them "0042", and a leading zero cannot go through a number token at all:
// the token decodes as a decimal integer, so "0042" would come back as "42".
// Both codecs therefore shred it into lone digits — packed5 pays 9+9+15 = 33
// bits for those four characters, u5 pays 10+10+15 = 35.
//
// So make the number token fixed-width instead of value-ranged: exactly three
// digits, zero padding included, in one opcode plus two units.
//
//	31 + 2 units: three digits, 000..999
//
// Fifteen bits for three digits is five bits per digit — the same as a letter,
// and a flat rate that leading zeros do not disturb. A run of L digits takes
// L/3 of these plus L%3 lone digits. "0042" becomes "004" + "2" = 25 bits
// against packed5's 33.
//
// It costs a bit where packed5's variable-width number wins: "1234" is 15+10 =
// 25 here against 15+9 = 24 there. The measurement decides whether that matters.
//
// Two encoders produce the identical stream. u5bTokenize classifies with the
// same comparison chain u5 uses; u5bTokenizeTab classifies with a 256-entry
// table. The README's closing claim was that per-byte classification is the
// bottleneck left after the planner and the packing are dealt with, and the gap
// between these two is the test of it.

// u5bNumber is the opcode. It is u5Number's slot; only the operand's meaning
// changed.
const (
	u5bNumber      = u5Number
	u5bNumberMax   = 999
	u5bNumberWidth = 3
)

// u5bDigits is the number of leading digits at s[i], capped at what the caller
// can use.
func u5bDigitRun(s string, i int) int {
	n := 0
	for i+n < len(s) && s[i+n]-'0' < 10 {
		n++
	}
	return n
}

func u5bTokenize(units []uint8, s string) []uint8 {
	cur := false
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
			n := u5bDigitRun(s, i)
			for ; n >= u5bNumberWidth; n -= u5bNumberWidth {
				v := int(s[i]-'0')*100 + int(s[i+1]-'0')*10 + int(s[i+2]-'0')
				units = append(units, u5bNumber, uint8(v&31), uint8(v>>5))
				i += u5bNumberWidth
			}
			for ; n > 0; n-- {
				units = append(units, u5SymA, s[i]-'0')
				i++
			}

		default:
			if c < 0x80 {
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
			for n < u5MaxEscape && i+n < len(s) && u5bEscapes(s, i+n) {
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

// u5bEscapes is u5Escapes; named separately so the two codecs stay readable
// side by side.
func u5bEscapes(s string, i int) bool { return u5Escapes(s, i) }

// ---------------------------------------------------------------------------
// Table-driven classification.

// Classes, in the high byte of u5bClass. The low byte is the operand the class
// needs: a letter index, a digit value, or a symbol table index.
const (
	clsEscape uint16 = iota << 8 // must be 0, so the table's zero value is "escape"
	clsLower
	clsUpper
	clsSpace
	clsDigit
	clsSymA
	clsSymB
	clsMulti
)

// u5bClass collapses the comparison chain above into one load. Every branch the
// chain makes on a byte's value — letter, case, space, digit, which symbol
// table — is already decided here.
var u5bClass [256]uint16

func init() {
	for i := range 26 {
		u5bClass['a'+i] = clsLower | uint16(i)
		u5bClass['A'+i] = clsUpper | uint16(i)
	}
	u5bClass[' '] = clsSpace
	for i := range 10 {
		u5bClass['0'+i] = clsDigit | uint16(i)
	}
	for c := range 128 {
		if u5bClass[c] != clsEscape {
			continue
		}
		if k := u5AsciiA[c]; k >= 0 {
			u5bClass[c] = clsSymA | uint16(k)
		} else if k := u5AsciiB[c]; k >= 0 {
			u5bClass[c] = clsSymB | uint16(k)
		}
	}
	// Only these three leads can begin a character u5TabB holds; every other
	// high byte goes straight to the escape.
	u5bClass[0xC2] = clsMulti
	u5bClass[0xC3] = clsMulti
	u5bClass[0xE2] = clsMulti
}

func u5bTokenizeTab(units []uint8, s string) []uint8 {
	cur := false
	for i := 0; i < len(s); {
		e := u5bClass[s[i]]
		switch e & 0xFF00 {
		case clsLower, clsUpper:
			idx := uint8(e)
			if (e&0xFF00 == clsUpper) == cur {
				units = append(units, idx)
				i++
				continue
			}
			run := 0
			for j := i; j < len(s); j++ {
				k := u5bClass[s[j]] & 0xFF00
				if k != clsLower && k != clsUpper {
					break
				}
				if (k == clsUpper) == cur {
					break
				}
				run++
			}
			if run >= 3 {
				units = append(units, u5CaseLong)
				cur = !cur
				continue
			}
			units = append(units, u5CaseNext, idx)
			i++

		case clsSpace:
			units = append(units, u5Space)
			i++

		case clsDigit:
			n := u5bDigitRun(s, i)
			for ; n >= u5bNumberWidth; n -= u5bNumberWidth {
				v := int(s[i]-'0')*100 + int(s[i+1]-'0')*10 + int(s[i+2]-'0')
				units = append(units, u5bNumber, uint8(v&31), uint8(v>>5))
				i += u5bNumberWidth
			}
			for ; n > 0; n-- {
				units = append(units, u5SymA, s[i]-'0')
				i++
			}

		case clsSymA:
			units = append(units, u5SymA, uint8(e))
			i++

		case clsSymB:
			units = append(units, u5SymB, uint8(e))
			i++

		default: // clsMulti or clsEscape
			if e == clsMulti {
				if k, w := u5Multi(s, i); k >= 0 {
					units = append(units, u5SymB, uint8(k))
					i += w
					continue
				}
			}
			n := 1
			for n < u5MaxEscape && i+n < len(s) && u5bEscapes(s, i+n) {
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

// ---------------------------------------------------------------------------

// The two entry points are spelled out rather than sharing a body through a
// func value: an indirect call per string would handicap both equally, but it
// would also put them below u5 in the same table for a reason that has nothing
// to do with what is being compared.

func u5bAppend(out []byte, s string) []byte {
	if len(s) == 0 {
		return append(out, 0)
	}
	var stack [u5Scratch]uint8
	units := u5bTokenize(stack[:0], s)

	flags := byte(hdrPacked)
	// A stream whose first token is a case toggle is a string that opens in
	// uppercase — "SKU-4217-hola" and every ALL CAPS value. Hoisting that one
	// token into the spare header bit recovers packed5's UPPERCASE_DOMINANT for
	// the case that actually occurs, and costs a single comparison on a slice
	// that has already been built. Nothing is planned and nothing is rescanned.
	if units[0] == u5CaseLong {
		units = units[1:]
		flags |= hdrUpper
	}

	payload := packedLen5(len(units))
	if frameOverhead(payload)+payload >= frameOverhead(len(s))+len(s) {
		return append(appendHeader(out, 0, len(s)), s...)
	}
	out = appendHeader(out, flags|byte(payload*8/5-len(units))<<hdrDropShif, payload)
	return packWide64(out, units)
}

func u5bAppendTab(out []byte, s string) []byte {
	if len(s) == 0 {
		return append(out, 0)
	}
	var stack [u5Scratch]uint8
	units := u5bTokenizeTab(stack[:0], s)

	flags := byte(hdrPacked)
	// A stream whose first token is a case toggle is a string that opens in
	// uppercase — "SKU-4217-hola" and every ALL CAPS value. Hoisting that one
	// token into the spare header bit recovers packed5's UPPERCASE_DOMINANT for
	// the case that actually occurs, and costs a single comparison on a slice
	// that has already been built. Nothing is planned and nothing is rescanned.
	if units[0] == u5CaseLong {
		units = units[1:]
		flags |= hdrUpper
	}

	payload := packedLen5(len(units))
	if frameOverhead(payload)+payload >= frameOverhead(len(s))+len(s) {
		return append(appendHeader(out, 0, len(s)), s...)
	}
	out = appendHeader(out, flags|byte(payload*8/5-len(units))<<hdrDropShif, payload)
	return packWide64(out, units)
}

func u5bDecode(dst, buf []byte) ([]byte, int, error) {
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
	var stack [u5Scratch]uint8
	units := unpackWide64(stack[:0], h.wide, n)

	out := dst
	cur, pending := h.upper, false
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
				return dst, 0, errTruncated
			}
			i++
			out = append(out, u5TabA[units[i]])
		case u == u5SymB:
			if i+1 >= len(units) {
				return dst, 0, errTruncated
			}
			i++
			k := units[i]
			if k != u5Esc {
				out = append(out, u5TabB[k]...)
				break
			}
			if i+1 >= len(units) {
				return dst, 0, errTruncated
			}
			i++
			cnt := int(units[i]) + 1
			if cnt > u5MaxEscape {
				return dst, 0, errBadUnit
			}
			if i+2*cnt > len(units)-1 {
				return dst, 0, errTruncated
			}
			for range cnt {
				out = append(out, units[i+1]|units[i+2]<<5)
				i += 2
			}
		default: // u5bNumber: three digits, zero padded
			if i+2 >= len(units) {
				return dst, 0, errTruncated
			}
			v := int(units[i+1]) | int(units[i+2])<<5
			i += 2
			if v > u5bNumberMax {
				return dst, 0, errBadUnit
			}
			out = append(out, byte('0'+v/100), byte('0'+v/10%10), byte('0'+v%10))
		}
	}
	return out, h.n, nil
}
