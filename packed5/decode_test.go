package packed5

import (
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
)

// packFrame builds a packed frame by hand so the decoder's rejection paths can
// be reached with streams the encoder would never produce.
func packFrame(flags byte, bits int, write func(*bitWriter)) []byte {
	payload := payloadBytes(bits)
	var w bitWriter
	w.writeBits(uint32(payload*8-padBitsWidth-bits), padBitsWidth)
	write(&w)
	return append(appendHeader(nil, flags|flagPacked5, payload), w.flush()...)
}

func TestDecodeErrors(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want error
	}{
		{"empty buffer", nil, ErrTruncated},
		{"reserved flag, raw", []byte{flagReserved, 'a'}, ErrReservedFlag},
		{"reserved flag, packed", []byte{flagReserved | flagPacked5 | 1<<lenShift, 0}, ErrReservedFlag},
		{"raw payload truncated", []byte{5 << lenShift, 'a', 'b'}, ErrTruncated},
		{"raw payload missing", []byte{1 << lenShift}, ErrTruncated},
		{"uvarint truncated", []byte{lenEscape << lenShift, 0x80}, ErrTruncated},
		{"uvarint missing", []byte{lenEscape << lenShift}, ErrTruncated},
		{"uvarint overlong bytes", []byte{lenEscape << lenShift,
			0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01}, ErrBadLength},
		// A length the header nibble could have held must not use the escape:
		// one length, one encoding.
		{"uvarint non-canonical", []byte{lenEscape << lenShift, 14, 'a'}, ErrBadLength},
		{"uvarint zero", []byte{lenEscape << lenShift, 0}, ErrBadLength},
		{"escaped length truncated", []byte{lenEscape << lenShift, 20, 'a'}, ErrTruncated},
		// Pad count larger than the bits that follow it.
		{"pad past end", []byte{flagPacked5 | 1<<lenShift, 0x07}, ErrBadPadding},
		// Opcode 29 with no room for its 5-bit index.
		{"symbol operand truncated", []byte{flagPacked5 | 1<<lenShift, opSymbol << padBitsWidth}, ErrTruncated},
		// Opcode 30 with no room for its 4-bit index.
		{"simple operand truncated", []byte{flagPacked5 | 1<<lenShift, opSimple << padBitsWidth}, ErrTruncated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, n, err := Decode(tc.in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got (%q, %d, %v), want error %v", s, n, err, tc.want)
			}
		})
	}
}

// TestDecodeReservedSymbol checks the two unassigned symbol slots are refused
// rather than decoded as empty strings, so assigning them later is a clean
// format change.
func TestDecodeReservedSymbol(t *testing.T) {
	for idx := symReserved; idx < 32; idx++ {
		buf := packFrame(0, 10, func(w *bitWriter) {
			w.writeBits(opSymbol, 5)
			w.writeBits(uint32(idx), 5)
		})
		if _, _, err := Decode(buf); !errors.Is(err, ErrReservedSymbol) {
			t.Errorf("symbol index %d: err = %v, want ErrReservedSymbol", idx, err)
		}
	}
	// The assigned neighbour must still work, so the boundary is exact.
	buf := packFrame(0, 10, func(w *bitWriter) {
		w.writeBits(opSymbol, 5)
		w.writeBits(symReserved-1, 5)
	})
	got, _, err := Decode(buf)
	if err != nil || got != symTable[symReserved-1] {
		t.Fatalf("index %d: got %q, %v", symReserved-1, got, err)
	}
}

// TestDecodeEscapeTruncated walks an escape that promises more bytes than the
// stream holds.
func TestDecodeEscapeTruncated(t *testing.T) {
	for declared := 1; declared <= maxEscapeRun; declared++ {
		for present := range declared {
			buf := packFrame(0, 11+8*present, func(w *bitWriter) {
				w.writeBits(opSimple, 5)
				w.writeBits(escapeCode, 4)
				w.writeBits(uint32(declared-1), 2)
				for range present {
					w.writeBits(0x41, 8)
				}
			})
			if _, _, err := Decode(buf); err == nil {
				t.Errorf("escape declaring %d bytes with %d present decoded cleanly", declared, present)
			}
		}
	}
}

// TestDecodeTrailingBits rejects a stream whose pad count does not land on a
// token boundary, which would otherwise silently drop or invent a token.
func TestDecodeTrailingBits(t *testing.T) {
	// One 9-bit simple-symbol token declared as if it were 13 bits long.
	buf := packFrame(0, 13, func(w *bitWriter) {
		w.writeBits(opSimple, 5)
		w.writeBits(3, 4) // '3'
	})
	if _, _, err := Decode(buf); !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
}

// TestRawModeIgnoresCaseFlags checks the spec's rule that the case and number
// flags are meaningless when PACKED_5 is clear.
func TestRawModeIgnoresCaseFlags(t *testing.T) {
	for _, flags := range []byte{0, flagUppercase, flagNumber, flagUppercase | flagNumber} {
		buf := append([]byte{flags | 3<<lenShift}, "abc"...)
		got, n, err := Decode(buf)
		if err != nil || got != "abc" || n != 4 {
			t.Fatalf("flags %02x: got (%q, %d, %v)", flags, got, n, err)
		}
	}
}

// TestDecodeIgnoresTrailingBytes checks a frame is read by its own length and
// leaves the rest of the buffer alone.
func TestDecodeIgnoresTrailingBytes(t *testing.T) {
	for _, s := range []string{"hello world", "\xff\xfe", "", "product123"} {
		buf := append(Append(nil, s), "TRAILING GARBAGE"...)
		want := len(Append(nil, s))
		got, n, err := Decode(buf)
		if err != nil || got != s || n != want {
			t.Fatalf("%q: got (%q, %d, %v), want (%q, %d, nil)", s, got, n, err, s, want)
		}
	}
}

// TestDecodeTruncatedAtEveryPrefix checks every short read of a valid frame is
// reported rather than returning a partial string.
func TestDecodeTruncatedAtEveryPrefix(t *testing.T) {
	inputs := []string{"hello world", "helloWORLDagain", "product123", "el niño comió jamón",
		"\xff\xfe\xfd", strings.Repeat("abc ", 60)}
	for _, s := range inputs {
		full := Append(nil, s)
		for cut := range len(full) {
			got, n, err := Decode(full[:cut])
			if err == nil {
				t.Fatalf("%q cut to %d/%d bytes decoded as %q (n=%d)", s, cut, len(full), got, n)
			}
		}
	}
}

// TestDecodeBitFlipsDoNotPanic mutates every single bit of a valid frame. The
// decoder may succeed or fail, but it must never panic or read out of bounds.
func TestDecodeBitFlipsDoNotPanic(t *testing.T) {
	inputs := []string{"hello world", "helloWORLDagain", "product123", "SKU-00042-XL",
		"el niño comió jamón", "€1023.45", strings.Repeat("mixed Case 123 ", 12)}
	for _, s := range inputs {
		base := Append(nil, s)
		for i := range base {
			for bit := range 8 {
				buf := append([]byte(nil), base...)
				buf[i] ^= 1 << bit
				got, n, err := Decode(buf)
				if err == nil && (n < 1 || n > len(buf)) {
					t.Fatalf("%q flip %d.%d: n = %d out of range (got %q)", s, i, bit, n, got)
				}
			}
		}
	}
}

// TestDecodeGarbageDoesNotPanic feeds random bytes straight to the decoder.
func TestDecodeGarbageDoesNotPanic(t *testing.T) {
	rng := rand.New(rand.NewPCG(17, 18))
	buf := make([]byte, 64)
	for range 200000 {
		n := rng.IntN(len(buf) + 1)
		for i := range n {
			buf[i] = byte(rng.UintN(256))
		}
		s, consumed, err := Decode(buf[:n])
		if err != nil {
			continue
		}
		if consumed < 1 || consumed > n {
			t.Fatalf("%x: consumed %d of %d", buf[:n], consumed, n)
		}
		// Whatever comes out must itself be encodable and survive a roundtrip.
		if back, _, err := Decode(Append(nil, s)); err != nil || back != s {
			t.Fatalf("re-encode of %q failed: %q, %v", s, back, err)
		}
	}
}

// TestMaxDecodedLenIsSound drives the decoder's output-size bound with the two
// densest tokens the format has: the three-byte '€' symbol at 3 bytes per 10
// bits, and NUMBER_0_1023 at 4 digits per 15 bits. If the bound were wrong the
// decode buffer would silently grow, so this pins the constant that keeps
// Decode to one allocation.
func TestMaxDecodedLenIsSound(t *testing.T) {
	dense := []struct {
		name string
		bits int
		emit func(*bitWriter)
	}{
		{"euro", 10, func(w *bitWriter) { w.writeBits(opSymbol, 5); w.writeBits(15, 5) }},
		{"number", 15, func(w *bitWriter) { w.writeBits(opNumber, 5); w.writeBits(numberMax, 10) }},
	}
	for _, d := range dense {
		for n := 1; n <= 300; n++ {
			flags := byte(0)
			if d.name == "number" {
				flags = flagNumber
			}
			buf := packFrame(flags, d.bits*n, func(w *bitWriter) {
				for range n {
					d.emit(w)
				}
			})
			got, consumed, err := Decode(buf)
			if err != nil || consumed != len(buf) {
				t.Fatalf("%s n=%d: %v (consumed %d/%d)", d.name, n, err, consumed, len(buf))
			}
			payload := len(buf) - frameOverhead(payloadBytes(d.bits*n))
			if bound := maxDecodedLen(payload); len(got) > bound {
				t.Fatalf("%s n=%d: decoded %d bytes from %d payload bytes, bound is %d",
					d.name, n, len(got), payload, bound)
			}
		}
	}
}

// TestDecodeExpansionIsBounded guards against a decompression bomb: no frame
// can expand past the decoder's own maxDecodedLen bound.
func TestDecodeExpansionIsBounded(t *testing.T) {
	rng := rand.New(rand.NewPCG(19, 20))
	buf := make([]byte, 128)
	for range 50000 {
		n := rng.IntN(len(buf) + 1)
		for i := range n {
			buf[i] = byte(rng.UintN(256))
		}
		s, _, err := Decode(buf[:n])
		if err != nil {
			continue
		}
		if bound := maxDecodedLen(n); len(s) > bound {
			t.Fatalf("%d input bytes expanded to %d, bound is %d", n, len(s), bound)
		}
	}
}

func FuzzDecode(f *testing.F) {
	for _, s := range []string{"hello world", "product123", "ñandú", "\xff\xfe", ""} {
		f.Add(Append(nil, s))
	}
	f.Add([]byte{flagPacked5 | 1<<lenShift, 0xFF})
	f.Add([]byte{lenEscape << lenShift, 0xFF, 0xFF})
	f.Fuzz(func(t *testing.T, buf []byte) {
		s, n, err := Decode(buf)
		if err != nil {
			return
		}
		if n < 1 || n > len(buf) {
			t.Fatalf("consumed %d of %d", n, len(buf))
		}
		back, _, err := Decode(Append(nil, s))
		if err != nil || back != s {
			t.Fatalf("re-encode of %q gave %q, %v", s, back, err)
		}
	})
}

// ------------------------------------------------------------------ bitstream

// TestBitstreamRoundtrip checks the LSB-first writer and reader agree for every
// width the codec uses, at every bit offset within a byte.
func TestBitstreamRoundtrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(21, 22))
	widths := []uint8{2, 3, 4, 5, 8, 10}
	for range 5000 {
		var vals []uint32
		var ws []uint8
		total := 0
		for range rng.IntN(20) + 1 {
			w := widths[rng.IntN(len(widths))]
			ws = append(ws, w)
			vals = append(vals, uint32(rng.UintN(1<<w)))
			total += int(w)
		}
		bw := bitWriter{}
		for i, v := range vals {
			bw.writeBits(v, ws[i])
		}
		buf := bw.flush()
		if want := (total + 7) / 8; len(buf) != want {
			t.Fatalf("wrote %d bytes for %d bits, want %d", len(buf), total, want)
		}
		br := bitReader{buf: buf, limit: total}
		for i, want := range vals {
			got, ok := br.read(ws[i])
			if !ok || got != want {
				t.Fatalf("field %d width %d: got %d ok=%v, want %d", i, ws[i], got, ok, want)
			}
		}
		if br.remaining() != 0 {
			t.Fatalf("%d bits left after reading %d fields", br.remaining(), len(vals))
		}
		if _, ok := br.read(1); ok {
			t.Fatal("read past limit succeeded")
		}
	}
}

// TestBitReaderRespectsLimit checks reads are refused at the limit even when
// the backing buffer has more bytes.
func TestBitReaderRespectsLimit(t *testing.T) {
	buf := []byte{0xFF, 0xFF, 0xFF}
	for limit := range 24 {
		r := bitReader{buf: buf, limit: limit}
		for r.remaining() >= 5 {
			if _, ok := r.read(5); !ok {
				t.Fatalf("limit %d: read failed with %d bits remaining", limit, r.remaining())
			}
		}
		if _, ok := r.read(5); ok {
			t.Fatalf("limit %d: read succeeded with %d bits remaining", limit, r.remaining())
		}
	}
}

// TestUvarintRoundtrip covers the length prefix's own codec at its byte
// boundaries.
func TestUvarintRoundtrip(t *testing.T) {
	vals := []int{0, 1, 126, 127, 128, 129, 16383, 16384, 1 << 20, 1 << 27, maxLen}
	for _, v := range vals {
		buf := appendUvarint(nil, v)
		if len(buf) != uvarintLen(v) {
			t.Fatalf("%d: wrote %d bytes, uvarintLen says %d", v, len(buf), uvarintLen(v))
		}
		got, n, err := readUvarint(buf)
		if err != nil || got != v || n != len(buf) {
			t.Fatalf("%d: got (%d, %d, %v)", v, got, n, err)
		}
	}
	if _, _, err := readUvarint(nil); !errors.Is(err, ErrTruncated) {
		t.Fatalf("empty uvarint: %v", err)
	}
}
