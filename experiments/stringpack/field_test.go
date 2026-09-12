package stringpack

// A string as a *field of a record*, rather than as a standalone frame.
//
// The standalone frame in frame_test.go carries a length and an encoding flag.
// So does the BLOB descriptor a colbin record already wraps around it, which
// means an embedded packed string pays for both. This file implements the
// proposal that removes the duplication under each key width, and prices it
// against what the same field costs today.
//
// Notation is BYTE_ALIGNED_PLAN.md's: b.n is bit n of byte b, bytes from 0 and
// bits from 1 with .1 the most significant.
//
// # Today — the frame goes in as BLOB content, verbatim
//
//	K4  0.1-0.4 Key · 0.5 More? · 0.6-0.8 Size high 3 · 1.1-1.8 Size low 8
//	K8  0.1-0.8 Key · 1.1 1 · 1.2-1.4 Class 001 · 1.5-1.6 enc · 1.7-1.8 lw
//	                · 2.1-... Size, lw bytes LE
//
// # Proposed — the frame header's two live bits move into the descriptor
//
//	K4  0.1-0.4 Key
//	    0.5     packed   0 = raw bytes · 1 = u5b unit stream
//	    0.6     upper    the stream starts in uppercase mode
//	    0.7-0.8 Size high 2
//	    1.1-1.8 Size low 8       -> 0..1022, 1023 escapes to a u32
//	    2.1-... Content, Size bytes — the bare unit stream
//
//	K8  0.1-0.8 Key
//	    1.1     1
//	    1.2-1.4 Class 001
//	    1.5-1.6 enc  0 raw · 1 u5b lower · 2 dict ref · 3 u5b upper
//	    1.7-1.8 lw   0->1B · 1->2B · 2->4B · 3->8B
//	    2.1-... Size, lw bytes LE
//	    then    Content, Size bytes — the bare unit stream
//
// Everything the frame header held is then either in the descriptor (packed,
// upper) or derivable from it: the unit count is floor(8*Size/5), because u5b
// pads to the grid with a CASE_TOGGLE_SIMPLE that decodes to nothing.
//
// K4 pays for this by narrowing its inline size from 11 bits to 10 — the plan
// calls the 11-bit form a K4 advantage, and this trades 2047 down to 1022 in two
// header bytes. K8 pays by spending its last reserved enc value.

import (
	"encoding/binary"
	"slices"
	"testing"
)

const (
	classBlob = 1 // BLOB's class in the K8 descriptor

	encRaw       = 0
	encU5BLower  = 1
	encDictRef   = 2 // not produced here; listed so the numbering is the plan's
	encU5BUpper  = 3
	k4SizeEscape = 1023
)

// fieldCodec is one (key width, framing) pair under test.
type fieldCodec struct {
	name   string
	encode func(out []byte, key int, s string) []byte
	decode func(dst, buf []byte) ([]byte, int, error)
}

var fieldCodecs = []fieldCodec{
	{"K4/raw", appendK4Raw, decodeK4},
	{"K4/framed", appendK4Framed, decodeK4Framed},
	{"K4/direct", appendK4, decodeK4},
	{"K8/raw", appendK8Raw, decodeK8},
	{"K8/framed", appendK8Framed, decodeK8Framed},
	{"K8/direct", appendK8, decodeK8},
}

// lwFor is the smallest lw code whose width holds n.
func lwFor(n int) int {
	switch {
	case n <= 0xFF:
		return 0
	case n <= 0xFFFF:
		return 1
	case n <= 0xFFFFFFFF:
		return 2
	}
	return 3
}

func putSize(out []byte, lw, n int) []byte {
	switch lw {
	case 0:
		return append(out, byte(n))
	case 1:
		return binary.LittleEndian.AppendUint16(out, uint16(n))
	case 2:
		return binary.LittleEndian.AppendUint32(out, uint32(n))
	}
	return binary.LittleEndian.AppendUint64(out, uint64(n))
}

func getSize(buf []byte, lw int) (int, int, bool) {
	w := 1 << lw
	if len(buf) < w {
		return 0, 0, false
	}
	switch lw {
	case 0:
		return int(buf[0]), 1, true
	case 1:
		return int(binary.LittleEndian.Uint16(buf)), 2, true
	case 2:
		return int(binary.LittleEndian.Uint32(buf)), 4, true
	}
	v := binary.LittleEndian.Uint64(buf)
	if v > uint64(maxFieldSize) {
		return 0, 0, false
	}
	return int(v), 8, true
}

const maxFieldSize = 1 << 30

// Raw baselines: the same descriptors with the encoding turned off, so a ratio
// above 1 can be read as "a string field costs more than the string" rather than
// mistaken for the packing failing.

func appendK4Raw(out []byte, key int, s string) []byte {
	return append(appendK4Desc(out, key, false, false, len(s)), s...)
}

func appendK8Raw(out []byte, key int, s string) []byte {
	return append(appendK8Desc(out, key, encRaw, len(s)), s...)
}

// ---------------------------------------------------------------------------
// Proposed: the descriptor carries packed and upper, the content is bare units.

func appendK4(out []byte, key int, s string) []byte {
	var stack [u5Scratch]uint8
	units, upper := u5bPrepare(u5Tokenize(stack[:0], s))
	payload := packedLen5(len(units))

	if payload >= len(s) {
		return append(appendK4Desc(out, key, false, false, len(s)), s...)
	}
	out = appendK4Desc(out, key, true, upper, payload)
	return packWide64(out, units)
}

// appendK4Desc writes the key byte, the flag bits and the size.
func appendK4Desc(out []byte, key int, packed, upper bool, size int) []byte {
	b := byte(key) << 4
	if packed {
		b |= 1 << 3
	}
	if upper {
		b |= 1 << 2
	}
	if size >= k4SizeEscape {
		out = append(out, b|byte(k4SizeEscape>>8), byte(k4SizeEscape&0xFF))
		return binary.LittleEndian.AppendUint32(out, uint32(size))
	}
	return append(out, b|byte(size>>8), byte(size))
}

func decodeK4(dst, buf []byte) ([]byte, int, error) {
	if len(buf) < 2 {
		return dst, 0, errTruncated
	}
	b := buf[0]
	packed, upper := b&(1<<3) != 0, b&(1<<2) != 0
	size, pos := int(b&0x03)<<8|int(buf[1]), 2
	if size == k4SizeEscape {
		if len(buf) < 6 {
			return dst, 0, errTruncated
		}
		size = int(binary.LittleEndian.Uint32(buf[2:]))
		if size < k4SizeEscape || size > maxFieldSize {
			return dst, 0, errBadLength // one size has one encoding
		}
		pos = 6
	}
	if size > len(buf)-pos {
		return dst, 0, errTruncated
	}
	if !packed {
		return append(dst, buf[pos:pos+size]...), pos + size, nil
	}
	out, err := u5bExpand(dst, buf[pos:], size*8/5, upper)
	if err != nil {
		return dst, 0, err
	}
	return out, pos + size, nil
}

func appendK8(out []byte, key int, s string) []byte {
	var stack [u5Scratch]uint8
	units, upper := u5bPrepare(u5Tokenize(stack[:0], s))
	payload := packedLen5(len(units))

	if payload >= len(s) {
		return append(appendK8Desc(out, key, encRaw, len(s)), s...)
	}
	enc := encU5BLower
	if upper {
		enc = encU5BUpper
	}
	out = appendK8Desc(out, key, enc, payload)
	return packWide64(out, units)
}

func appendK8Desc(out []byte, key, enc, size int) []byte {
	lw := lwFor(size)
	out = append(out, byte(key), 1<<7|classBlob<<4|byte(enc)<<2|byte(lw))
	return putSize(out, lw, size)
}

func decodeK8(dst, buf []byte) ([]byte, int, error) {
	if len(buf) < 3 {
		return dst, 0, errTruncated
	}
	d := buf[1]
	if d&0x80 == 0 || d>>4&0x07 != classBlob {
		return dst, 0, errBadUnit
	}
	enc, lw := int(d>>2&0x03), int(d&0x03)
	size, k, ok := getSize(buf[2:], lw)
	if !ok {
		return dst, 0, errTruncated
	}
	pos := 2 + k
	if size > len(buf)-pos {
		return dst, 0, errTruncated
	}
	switch enc {
	case encRaw:
		return append(dst, buf[pos:pos+size]...), pos + size, nil
	case encU5BLower, encU5BUpper:
		out, err := u5bExpand(dst, buf[pos:], size*8/5, enc == encU5BUpper)
		if err != nil {
			return dst, 0, err
		}
		return out, pos + size, nil
	}
	return dst, 0, errBadUnit // encDictRef has no meaning outside a column
}

// ---------------------------------------------------------------------------
// Today: the same descriptors, carrying a standalone u5b frame as content.
//
// Both write the descriptor's size field last, because the frame's length is
// not known until it is written — the same reserve-and-patch the proposed
// encoders do, so the benchmark compares framing rather than bookkeeping.

func appendK4Framed(out []byte, key int, s string) []byte {
	start := len(out)
	out = append(out, byte(key)<<4, 0)
	out = u5bAppend(out, s)
	size := len(out) - start - 2
	if size < 2048 {
		out[start] |= byte(size >> 8)
		out[start+1] = byte(size)
		return out
	}
	out = slices.Grow(out, 3)[:len(out)+3]
	copy(out[start+5:], out[start+2:len(out)-3])
	out[start] |= 1 << 3 // More? = 32-bit size
	binary.LittleEndian.PutUint32(out[start+1:], uint32(size))
	return out
}

func decodeK4Framed(dst, buf []byte) ([]byte, int, error) {
	if len(buf) < 2 {
		return dst, 0, errTruncated
	}
	size, pos := int(buf[0]&0x07)<<8|int(buf[1]), 2
	if buf[0]&(1<<3) != 0 {
		if len(buf) < 5 {
			return dst, 0, errTruncated
		}
		size, pos = int(binary.LittleEndian.Uint32(buf[1:])), 5
	}
	if size > len(buf)-pos {
		return dst, 0, errTruncated
	}
	out, n, err := u5bDecode(dst, buf[pos:])
	if err != nil {
		return dst, 0, err
	}
	if n != size {
		return dst, 0, errBadLength
	}
	return out, pos + size, nil
}

func appendK8Framed(out []byte, key int, s string) []byte {
	start := len(out)
	out = append(out, byte(key), 0, 0)
	out = u5bAppend(out, s)
	size := len(out) - start - 3
	// enc names packed5 whether or not this particular value ended up packed;
	// the frame's own PACKED_5 bit is what actually decides, which is the
	// duplication the proposal removes.
	if lw := lwFor(size); lw == 0 {
		out[start+1] = 1<<7 | classBlob<<4 | encU5BLower<<2
		out[start+2] = byte(size)
		return out
	} else {
		k := 1 << lw
		out = slices.Grow(out, k-1)[:len(out)+k-1]
		copy(out[start+2+k:], out[start+3:len(out)-k+1])
		out[start+1] = 1<<7 | classBlob<<4 | encU5BLower<<2 | byte(lw)
		putSize(out[start+2:start+2], lw, size)
		return out
	}
}

func decodeK8Framed(dst, buf []byte) ([]byte, int, error) {
	if len(buf) < 3 {
		return dst, 0, errTruncated
	}
	d := buf[1]
	if d&0x80 == 0 || d>>4&0x07 != classBlob {
		return dst, 0, errBadUnit
	}
	size, k, ok := getSize(buf[2:], int(d&0x03))
	if !ok {
		return dst, 0, errTruncated
	}
	pos := 2 + k
	if size > len(buf)-pos {
		return dst, 0, errTruncated
	}
	out, n, err := u5bDecode(dst, buf[pos:])
	if err != nil {
		return dst, 0, err
	}
	if n != size {
		return dst, 0, errBadLength
	}
	return out, pos + size, nil
}

// ---------------------------------------------------------------------------

func TestFieldsRoundTrip(t *testing.T) {
	inputs := allStrings()
	for _, c := range fieldCodecs {
		t.Run(c.name, func(t *testing.T) {
			for i, s := range inputs {
				buf := c.encode(nil, i%16, s)
				buf = append(buf, make([]byte, slack)...)
				got, n, err := c.decode(nil, buf)
				if err != nil {
					t.Fatalf("%q: %v", s, err)
				}
				if n != len(buf)-slack {
					t.Fatalf("%q: consumed %d of %d", s, n, len(buf)-slack)
				}
				if string(got) != s {
					t.Fatalf("%q: round trip gave %q", s, got)
				}
			}
		})
	}
}

// TestFieldsBackToBack reads a run of fields out of one buffer, which is the
// shape a record has and the one that supplies wide64's read slack from the
// following field rather than from padding.
func TestFieldsBackToBack(t *testing.T) {
	inputs := allStrings()
	for _, c := range fieldCodecs {
		t.Run(c.name, func(t *testing.T) {
			var enc []byte
			for i, s := range inputs {
				enc = c.encode(enc, i%16, s)
			}
			total := len(enc)
			enc = append(enc, make([]byte, slack)...)
			var out []byte
			var bounds []int
			for p := 0; p < total; {
				var n int
				var err error
				out, n, err = c.decode(out, enc[p:])
				if err != nil {
					t.Fatal(err)
				}
				bounds = append(bounds, len(out))
				p += n
			}
			if len(bounds) != len(inputs) {
				t.Fatalf("decoded %d fields, want %d", len(bounds), len(inputs))
			}
			prev := 0
			for i, end := range bounds {
				if string(out[prev:end]) != inputs[i] {
					t.Fatalf("field %d: got %q want %q", i, out[prev:end], inputs[i])
				}
				prev = end
			}
		})
	}
}

// TestFieldKeyRoundTrip pins that the key survives, since the proposed K4 form
// shares its byte with the flags.
func TestFieldKeyRoundTrip(t *testing.T) {
	for key := range 16 {
		for _, s := range []string{"hola", "SKU-4217-hola", "\xff\xfe", ""} {
			if got := appendK4(nil, key, s)[0] >> 4; int(got) != key {
				t.Errorf("K4 key %d: read back %d", key, got)
			}
			if got := appendK8(nil, key, s)[0]; int(got) != key {
				t.Errorf("K8 key %d: read back %d", key, got)
			}
		}
	}
}

// TestFieldSavesTheFrameHeader is the proposal's claim, stated as a test: an
// embedded field costs exactly the standalone frame's own overhead less, and
// never more, on every input.
//
// That is one byte for any payload the frame's five length bits can hold, and
// three for a payload past 30 bytes, where the standalone form escapes to a
// uvarint the descriptor's size field was already carrying.
func TestFieldSavesTheFrameHeader(t *testing.T) {
	byName := func(n string) fieldCodec {
		for _, c := range fieldCodecs {
			if c.name == n {
				return c
			}
		}
		t.Fatalf("no codec %q", n)
		return fieldCodec{}
	}
	for _, pair := range []struct{ framed, direct fieldCodec }{
		{byName("K4/framed"), byName("K4/direct")},
		{byName("K8/framed"), byName("K8/direct")},
	} {
		var packedSeen, short, long int
		for _, s := range allStrings() {
			a := len(pair.framed.encode(nil, 3, s))
			b := len(pair.direct.encode(nil, 3, s))
			if b > a {
				t.Errorf("%s vs %s: %q is %d bytes, was %d", pair.direct.name, pair.framed.name, s, b, a)
			}
			frame := u5bAppend(nil, s)
			if len(frame) >= len(s)+1 {
				continue // this value went raw; framing is the same either way
			}
			packedSeen++
			// The frame's own overhead is what the embedded form stops paying.
			payload := len(frame) - frameOverhead(len(frame))
			if want := frameOverhead(payload); a-b != want {
				t.Errorf("%s: %q (payload %d) saved %d bytes, want %d",
					pair.direct.name, s, payload, a-b, want)
			}
			if payload <= hdrLenInlin {
				short++
			} else {
				long++
			}
		}
		if packedSeen == 0 || short == 0 || long == 0 {
			t.Fatalf("%s: corpus did not cover both length forms (%d packed, %d short, %d long)",
				pair.direct.name, packedSeen, short, long)
		}
	}
}

func BenchmarkField(b *testing.B) {
	for _, c := range fieldCodecs {
		for _, kind := range corpusKinds {
			in, n := corpus(kind)
			b.Run(c.name+"/"+kind, func(b *testing.B) {
				out := make([]byte, 0, 4<<20)
				b.SetBytes(int64(n))
				b.ReportAllocs()
				for b.Loop() {
					out = out[:0]
					for i, s := range in {
						out = c.encode(out, i%16, s)
					}
				}
				b.ReportMetric(float64(len(out))/float64(n), "ratio")
				b.ReportMetric(float64(len(out))/float64(len(in)), "B/field")
				msPerMB(b, n)
			})
		}
	}
}
