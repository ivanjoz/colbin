package stringpack

import (
	"strconv"
	"testing"
)

// u5b is u5 with two changes to the frame, and none to the tokens.
//
// u5 lost 5.5% to packed5 on the sku corpus and nowhere else. The dash was the
// obvious suspect and it is not the cause. Dumping the units for
// "SKU-4217-hola" settles it: the stream opens with 28, CASE_TOGGLE_LONG,
// because the string starts uppercase. Those five bits are what packed5 gets
// free from its UPPERCASE_DOMINANT header flag, and they are essentially the
// whole gap.
//
//  1. Hoist a leading CASE_TOGGLE_LONG into the header's spare upper bit. One
//     comparison on a slice that has already been built — nothing is planned and
//     nothing is rescanned.
//  2. Pad to the unit grid with a trailing CASE_TOGGLE_SIMPLE instead of a
//     header flag. A simple toggle with no letter after it decodes to nothing,
//     so it is a NOP the alphabet already had, and it costs no bytes. That
//     retires the drop bit.
//
// Together those leave a header whose only live bits are "packed" and "upper" —
// which is what makes an embedded frame nearly free; see the README on K4/K8.
//
// A third change was tried and reverted: a fixed-width three-digit number token,
// so that leading zeros ("0042", which no value-ranged token can carry) cost
// five bits per digit instead of ten. It works per string — "SKU-0042-hola"
// goes from 14 bytes to 13 — but the corpus draws SKU-%04d uniformly, so only
// one SKU in ten has a leading zero and the corpus-wide effect was 0.06%.
// Against that it is a real regression wherever digit runs are one or two long:
// u5 spends 15 bits on "12" through its variable-width number, u5b's fixed form
// had to spend 20. It is gone; u5b tokenises with u5Tokenize verbatim.
//
// What remains here besides the frame is u5bTokenizeTab, which classifies with a
// 256-entry table instead of u5's comparison chain. The README's claim was that
// per-byte classification is the bottleneck left after the planner and the
// packing are dealt with, and the gap between the two encoders tests it.

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
				units = append(units, u5SymA, uint8(e))
			} else {
				units = append(units, u5Number, uint8(best&31), uint8(best>>5))
			}
			i += bestLen

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

// u5bPrepare turns a raw unit stream into the one a frame actually carries: a
// leading CASE_TOGGLE_LONG hoisted out into upper, and the tail padded to the
// unit grid with a CASE_TOGGLE_SIMPLE that decodes to nothing.
//
// Both are frame-level concerns rather than token-level ones, which is why they
// live here and not in the tokeniser — the embedded K4/K8 field encoders in
// field_test.go reuse this and then put upper somewhere else entirely.
func u5bPrepare(units []uint8) ([]uint8, bool) {
	upper := false
	if len(units) > 0 && units[0] == u5CaseLong {
		units = units[1:]
		upper = true
	}
	if packedLen5(len(units))*8/5 > len(units) {
		units = append(units, u5CaseNext)
	}
	return units, upper
}

func u5bAppend(out []byte, s string) []byte {
	if len(s) == 0 {
		return append(out, 0)
	}
	var stack [u5Scratch]uint8
	units, upper := u5bPrepare(u5Tokenize(stack[:0], s))

	payload := packedLen5(len(units))
	if frameOverhead(payload)+payload >= frameOverhead(len(s))+len(s) {
		return append(appendHeader(out, 0, len(s)), s...)
	}
	flags := byte(hdrPacked)
	if upper {
		flags |= hdrUpper
	}
	out = appendHeader(out, flags, payload)
	return packWide64(out, units)
}

func u5bAppendTab(out []byte, s string) []byte {
	if len(s) == 0 {
		return append(out, 0)
	}
	var stack [u5Scratch]uint8
	units, upper := u5bPrepare(u5bTokenizeTab(stack[:0], s))

	payload := packedLen5(len(units))
	if frameOverhead(payload)+payload >= frameOverhead(len(s))+len(s) {
		return append(appendHeader(out, 0, len(s)), s...)
	}
	flags := byte(hdrPacked)
	if upper {
		flags |= hdrUpper
	}
	out = appendHeader(out, flags, payload)
	return packWide64(out, units)
}

// TestU5BWireFormat pins the exact bytes of the frames the README walks
// through, so the bit-level table there cannot drift away from the code.
func TestU5BWireFormat(t *testing.T) {
	for _, c := range []struct {
		s     string
		frame string
		units []uint8
		upper bool
	}{
		{
			"SKU-4217-hola",
			"5D 52 D1 CE 7E 69 FD 74 C6 8F 5B 00",
			[]uint8{18, 10, 20, 29, 12, 31, 5, 13, 29, 7, 29, 12, 28, 7, 14, 11, 0},
			true,
		},
		{
			"Lima norte",
			"39 7B 21 06 74 73 71 12",
			[]uint8{27, 11, 8, 12, 0, 26, 13, 14, 17, 19, 4},
			false,
		},
		{"hola", "19 C7 2D 00", []uint8{7, 14, 11, 0}, false},
	} {
		enc := u5bAppend(nil, c.s)
		if got := hex(enc); got != c.frame {
			t.Errorf("%q: frame % s, want % s", c.s, got, c.frame)
		}
		units := u5Tokenize(nil, c.s)
		if units[0] == u5CaseLong {
			units = units[1:]
		}
		if !equalUnits(units, c.units) {
			t.Errorf("%q: units %v, want %v", c.s, units, c.units)
		}
		h, err := readFrame(append(enc, make([]byte, slack)...))
		if err != nil {
			t.Fatal(err)
		}
		if h.upper != c.upper || h.drop != 0 || !h.packed {
			t.Errorf("%q: packed=%v drop=%d upper=%v", c.s, h.packed, h.drop, h.upper)
		}
		// The unit count the decoder derives must match what the encoder wrote.
		if n := len(h.payload)*8/5 - h.drop; n != len(c.units) {
			t.Errorf("%q: payload implies %d units, encoder wrote %d", c.s, n, len(c.units))
		}
	}
}

func hex(b []byte) string {
	const digits = "0123456789ABCDEF"
	out := make([]byte, 0, 3*len(b))
	for i, c := range b {
		if i > 0 {
			out = append(out, ' ')
		}
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}

func equalUnits(a, b []uint8) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
	out, err := u5bExpand(dst, h.wide, n, h.upper)
	if err != nil {
		return dst, 0, err
	}
	return out, h.n, nil
}

// u5bExpand walks n units out of src and appends their characters to dst. src
// must have slack bytes readable past the units.
//
// The case mode arrives as an argument rather than being read from a header,
// because where it is stored is the caller's business: a standalone frame keeps
// it in a header bit, an embedded K8 field keeps it in the BLOB descriptor's
// enc field, and an embedded K4 field keeps it in the key byte.
func u5bExpand(dst, src []byte, n int, upper bool) ([]byte, error) {
	var stack [u5Scratch]uint8
	units := unpackWide64(stack[:0], src, n)

	out := dst
	cur, pending := upper, false
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

// TestU5BDropAlwaysZero pins the claim that the drop flag is now dead weight:
// padding with CASE_TOGGLE_SIMPLE lands every frame exactly on the unit grid,
// at no cost in bytes.
func TestU5BDropAlwaysZero(t *testing.T) {
	for _, s := range allStrings() {
		enc := u5bAppend(nil, s)
		h, err := readFrame(append(enc, make([]byte, slack)...))
		if err != nil {
			t.Fatal(err)
		}
		if h.packed && h.drop != 0 {
			t.Fatalf("%q: drop = %d", s, h.drop)
		}
		// And the padding must not have cost a byte.
		if n := len(u5Append(nil, s)); h.packed && len(enc) > n {
			t.Fatalf("%q: u5b %d bytes vs u5 %d", s, len(enc), n)
		}
	}
}
