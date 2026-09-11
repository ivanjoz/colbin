package bytealigned

// Does splitting the key run's decoder by key width actually pay?
//
// The claim is the same one §1 makes about column widths — take the variable out
// of the loop — but the situation is not the same. A column width branches once
// per *element*, thousands of times; a key width branches once per *field*, five
// to twenty times, and always the same way within a record. A branch predictor
// eats that after the first iteration.
//
// So this measures the two arrangements directly:
//
//	branchy   one decoder carrying `k8 bool`, tested per field
//	split     two decoders, neither of which contains the other's width
//
// against the same record, five of ten fields set.

import (
	"encoding/binary"
	"testing"
)

type rec struct {
	CompanyID uint32 // key 0
	UserID    uint32 // key 1
	RouteID   uint16 // key 2
	CPU       uint16 // key 3
	Inference uint16 // key 4
	Extra     bool   // key 5
	Access1   uint16 // key 6
	Access2   uint16 // key 7
	Access3   uint16 // key 8
	Access4   uint16 // key 9
}

var benchRec = rec{CompanyID: 7, UserID: 42, RouteID: 103, CPU: 5, Access1: 0x0139}

// magnitudeBytes is how many little-endian bytes a value needs.
func magnitudeBytes(v uint64) int {
	n := 0
	for v != 0 {
		n++
		v >>= 8
	}
	return n
}

func appendMagnitude(out []byte, v uint64, n int) []byte {
	for range n {
		out = append(out, byte(v))
		v >>= 8
	}
	return out
}

// encodeK4 writes [key:4][pos:1][n:3] then n magnitude bytes.
func encodeK4(out []byte, r *rec) []byte {
	put := func(out []byte, key uint8, v uint64) []byte {
		if v == 0 {
			return out
		}
		n := magnitudeBytes(v)
		out = append(out, key<<4|1<<3|uint8(n))
		return appendMagnitude(out, v, n)
	}
	out = put(out, 0, uint64(r.CompanyID))
	out = put(out, 1, uint64(r.UserID))
	out = put(out, 2, uint64(r.RouteID))
	out = put(out, 3, uint64(r.CPU))
	out = put(out, 4, uint64(r.Inference))
	if r.Extra {
		out = append(out, 5<<4|1<<3|0)
	}
	out = put(out, 6, uint64(r.Access1))
	out = put(out, 7, uint64(r.Access2))
	out = put(out, 8, uint64(r.Access3))
	out = put(out, 9, uint64(r.Access4))
	return out
}

// encodeK8 writes [key:8] then either [0][value:7] or [1][class:3][pos:1][n:3]
// and n magnitude bytes.
func encodeK8(out []byte, r *rec) []byte {
	put := func(out []byte, key uint8, v uint64) []byte {
		if v == 0 {
			return out
		}
		if v <= 127 {
			return append(out, key, uint8(v))
		}
		n := magnitudeBytes(v)
		out = append(out, key, 0x80|1<<3|uint8(n))
		return appendMagnitude(out, v, n)
	}
	out = put(out, 0, uint64(r.CompanyID))
	out = put(out, 1, uint64(r.UserID))
	out = put(out, 2, uint64(r.RouteID))
	out = put(out, 3, uint64(r.CPU))
	out = put(out, 4, uint64(r.Inference))
	if r.Extra {
		out = append(out, 5, 1)
	}
	out = put(out, 6, uint64(r.Access1))
	out = put(out, 7, uint64(r.Access2))
	out = put(out, 8, uint64(r.Access3))
	out = put(out, 9, uint64(r.Access4))
	return out
}

// magnitudeMask[n] keeps the low n bytes of an 8-byte load, which is how a
// variable-width little-endian integer is read without a loop. It is why the
// buffers below are padded: the load reads 8 bytes for as few as one.
var magnitudeMask = [9]uint64{
	0, 0xFF, 0xFFFF, 0xFF_FFFF, 0xFFFF_FFFF,
	0xFF_FFFF_FFFF, 0xFFFF_FFFF_FFFF, 0xFF_FFFF_FFFF_FFFF, ^uint64(0),
}

func loadMagnitude(buf []byte, n int) uint64 {
	return binary.LittleEndian.Uint64(buf) & magnitudeMask[n]
}

// store is the part that is identical at both key widths, and so is shared
// rather than duplicated. Keeping this out of the split is the whole point of
// the seam: the framing differs, the value handling does not.
func (r *rec) store(key uint8, v uint64) {
	switch key {
	case 0:
		r.CompanyID = uint32(v)
	case 1:
		r.UserID = uint32(v)
	case 2:
		r.RouteID = uint16(v)
	case 3:
		r.CPU = uint16(v)
	case 4:
		r.Inference = uint16(v)
	case 5:
		r.Extra = v == 1
	case 6:
		r.Access1 = uint16(v)
	case 7:
		r.Access2 = uint16(v)
	case 8:
		r.Access3 = uint16(v)
	case 9:
		r.Access4 = uint16(v)
	}
}

// decodeBranchy carries the key width as data and tests it per field.
func decodeBranchy(buf []byte, n int, k8 bool, r *rec) {
	at := 0
	for at < n {
		var key, desc uint8
		if k8 {
			key, desc = buf[at], buf[at+1]
			at += 2
		} else {
			key, desc = buf[at]>>4, buf[at]&0x0F
			at++
		}
		var v uint64
		if k8 && desc < 0x80 {
			v = uint64(desc)
		} else {
			w := int(desc & 7)
			if w == 0 {
				v = 1
			} else {
				v = loadMagnitude(buf[at:], w)
				at += w
			}
		}
		r.store(key, v)
	}
}

// decodeK4 and decodeK8 are the split pair: neither mentions the other's width.
func decodeK4(buf []byte, n int, r *rec) {
	at := 0
	for at < n {
		header := buf[at]
		at++
		key := header >> 4
		var v uint64
		if w := int(header & 7); w == 0 {
			v = 1
		} else {
			v = loadMagnitude(buf[at:], w)
			at += w
		}
		r.store(key, v)
	}
}

func decodeK8(buf []byte, n int, r *rec) {
	at := 0
	for at < n {
		key, desc := buf[at], buf[at+1]
		at += 2
		var v uint64
		if desc < 0x80 {
			v = uint64(desc)
		} else if w := int(desc & 7); w == 0 {
			v = 1
		} else {
			v = loadMagnitude(buf[at:], w)
			at += w
		}
		r.store(key, v)
	}
}

func TestKeyRunRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name   string
		buf    []byte
		decode func([]byte, int, *rec)
		k8     bool
	}{
		{"k4", encodeK4(nil, &benchRec), decodeK4, false},
		{"k8", encodeK8(nil, &benchRec), decodeK8, true},
	} {
		padded := pad(tc.buf)
		var split, branchy rec
		tc.decode(padded, len(tc.buf), &split)
		decodeBranchy(padded, len(tc.buf), tc.k8, &branchy)
		if split != benchRec || branchy != benchRec {
			t.Fatalf("%s: split %+v branchy %+v want %+v", tc.name, split, branchy, benchRec)
		}
		t.Logf("%s: %d bytes", tc.name, len(tc.buf))
	}
}

func BenchmarkKeyRun(b *testing.B) {
	k4 := encodeK4(nil, &benchRec)
	k8 := encodeK8(nil, &benchRec)
	// The read buffer carries the slack the masked load needs; the message
	// length is passed separately, which is what a real decoder has anyway.
	k4p, k8p := pad(k4), pad(k8)
	n4, n8 := len(k4), len(k8)

	var out rec
	b.Run("k4/split", func(b *testing.B) {
		for b.Loop() {
			decodeK4(k4p, n4, &out)
		}
	})
	b.Run("k4/branchy", func(b *testing.B) {
		for b.Loop() {
			decodeBranchy(k4p, n4, false, &out)
		}
	})
	b.Run("k8/split", func(b *testing.B) {
		for b.Loop() {
			decodeK8(k8p, n8, &out)
		}
	})
	b.Run("k8/branchy", func(b *testing.B) {
		for b.Loop() {
			decodeBranchy(k8p, n8, true, &out)
		}
	})
}
