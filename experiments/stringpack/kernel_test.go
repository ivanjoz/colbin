package stringpack

// The packing kernels, isolated from any tokeniser.
//
// A "unit" here is one 5-bit value, 0..31 — the packed5 alphabet's opcode or
// operand. Every kernel below takes the same []uint8 of units and produces the
// same bit-for-bit LSB-first stream; they differ only in how many units they
// handle per store, and therefore in how much shift/branch work falls on each
// unit. triple16 is the exception: it deliberately wastes one bit in sixteen.
//
//	dense     packed5's own bitWriter shape: accumulate, drain 32 bits when full.
//	wide64    8 units = 40 bits = 5 bytes exactly. Build one uint64 with fixed
//	          shifts, store 8 bytes, advance 5. Zero size cost; needs 8 bytes of
//	          slack past the payload, because the final store writes 3 bytes
//	          past the last real one.
//	triple16  3 units = 15 bits in a uint16, one bit spare. Fixed shifts and a
//	          2-byte store, at a cost of 1 bit per 3 units (6.67%).
//
// The unpack halves mirror them, so a pack/unpack benchmark pair prices the
// encode and decode side of the same choice.

import (
	"encoding/binary"
	"math/rand/v2"
	"slices"
	"testing"
)

// packedLen5 is the payload size of n five-bit units.
func packedLen5(n int) int { return (n*5 + 7) / 8 }

// packedLen16 is the payload size of n five-bit units in uint16 triples.
func packedLen16(n int) int { return (n + 2) / 3 * 2 }

// slack is the number of bytes wide64 and pack6 may touch past the end of the
// payload. Their final store is a full word landing on the last partial group.
const slack = 8

// ---------------------------------------------------------------------------
// dense: packed5's accumulator, reproduced here so the comparison is in-package.

func packDense(dst []byte, units []uint8) []byte {
	var acc uint64
	var n uint8
	for _, u := range units {
		acc |= uint64(u) << n
		n += 5
		if n >= 32 {
			dst = binary.LittleEndian.AppendUint32(dst, uint32(acc))
			acc >>= 32
			n -= 32
		}
	}
	for n > 0 {
		dst = append(dst, byte(acc))
		acc >>= 8
		if n <= 8 {
			break
		}
		n -= 8
	}
	return dst
}

func unpackDense(dst []uint8, src []byte, n int) []uint8 {
	var acc uint64
	var have uint8
	pos := 0
	for range n {
		for have < 5 {
			acc |= uint64(src[pos]) << have
			pos++
			have += 8
		}
		dst = append(dst, uint8(acc&31))
		acc >>= 5
		have -= 5
	}
	return dst
}

// ---------------------------------------------------------------------------
// wide64: 8 units -> uint64 -> 8 bytes stored, 5 bytes advanced.

// packWide64 appends units to dst. The returned slice has len = payload end,
// but the caller's array is written up to slack bytes further; anything already
// in dst past its length is clobbered.
func packWide64(dst []byte, units []uint8) []byte {
	start := len(dst)
	dst = slices.Grow(dst, packedLen5(len(units))+slack)
	buf := dst[:cap(dst)]

	p := start
	i := 0
	for ; i+8 <= len(units); i += 8 {
		u := units[i : i+8 : i+8]
		w := uint64(u[0]) | uint64(u[1])<<5 | uint64(u[2])<<10 | uint64(u[3])<<15 |
			uint64(u[4])<<20 | uint64(u[5])<<25 | uint64(u[6])<<30 | uint64(u[7])<<35
		binary.LittleEndian.PutUint64(buf[p:], w)
		p += 5
	}
	if r := len(units) - i; r > 0 {
		var w uint64
		for k, u := range units[i:] {
			w |= uint64(u) << (5 * k)
		}
		binary.LittleEndian.PutUint64(buf[p:], w)
		p += packedLen5(r)
	}
	return buf[:p]
}

// unpackWide64 reads n units from src, which must have slack bytes readable
// past its payload.
func unpackWide64(dst []uint8, src []byte, n int) []uint8 {
	p := 0
	i := 0
	for ; i+8 <= n; i += 8 {
		w := binary.LittleEndian.Uint64(src[p:])
		dst = append(dst,
			uint8(w)&31, uint8(w>>5)&31, uint8(w>>10)&31, uint8(w>>15)&31,
			uint8(w>>20)&31, uint8(w>>25)&31, uint8(w>>30)&31, uint8(w>>35)&31)
		p += 5
	}
	if i < n {
		w := binary.LittleEndian.Uint64(src[p:])
		for ; i < n; i++ {
			dst = append(dst, uint8(w)&31)
			w >>= 5
		}
	}
	return dst
}

// ---------------------------------------------------------------------------
// triple16: three units per uint16, one bit spare.

func packTriple16(dst []byte, units []uint8) []byte {
	start := len(dst)
	dst = slices.Grow(dst, packedLen16(len(units))+2)
	buf := dst[:cap(dst)]

	p := start
	i := 0
	for ; i+3 <= len(units); i += 3 {
		u := units[i : i+3 : i+3]
		binary.LittleEndian.PutUint16(buf[p:], uint16(u[0])|uint16(u[1])<<5|uint16(u[2])<<10)
		p += 2
	}
	if i < len(units) {
		var w uint16
		for k, u := range units[i:] {
			w |= uint16(u) << (5 * k)
		}
		binary.LittleEndian.PutUint16(buf[p:], w)
		p += 2
	}
	return buf[:p]
}

func unpackTriple16(dst []uint8, src []byte, n int) []uint8 {
	p := 0
	i := 0
	for ; i+3 <= n; i += 3 {
		w := binary.LittleEndian.Uint16(src[p:])
		dst = append(dst, uint8(w)&31, uint8(w>>5)&31, uint8(w>>10)&31)
		p += 2
	}
	if i < n {
		w := binary.LittleEndian.Uint16(src[p:])
		for ; i < n; i++ {
			dst = append(dst, uint8(w)&31)
			w >>= 5
		}
	}
	return dst
}

// ---------------------------------------------------------------------------

func randUnits(n int) []uint8 {
	rng := rand.New(rand.NewPCG(7, 11))
	u := make([]uint8, n)
	for i := range u {
		u[i] = uint8(rng.IntN(32))
	}
	return u
}

func TestKernelsRoundTrip(t *testing.T) {
	kernels := []struct {
		name   string
		pack   func([]byte, []uint8) []byte
		unpack func([]uint8, []byte, int) []uint8
		size   func(int) int
	}{
		{"dense", packDense, unpackDense, packedLen5},
		{"wide64", packWide64, unpackWide64, packedLen5},
		{"triple16", packTriple16, unpackTriple16, packedLen16},
	}
	for _, k := range kernels {
		t.Run(k.name, func(t *testing.T) {
			for n := range 200 {
				units := randUnits(n)
				// A prefix byte proves the kernels honour a non-empty dst.
				enc := k.pack([]byte{0xAA}, units)
				if enc[0] != 0xAA {
					t.Fatalf("n=%d: prefix clobbered", n)
				}
				if got, want := len(enc)-1, k.size(n); got != want {
					t.Fatalf("n=%d: payload %d bytes, want %d", n, got, want)
				}
				// Readers may touch slack bytes past the payload.
				src := append(slices.Clone(enc[1:]), make([]byte, slack)...)
				got := k.unpack(nil, src, n)
				if !slices.Equal(got, units) {
					t.Fatalf("n=%d: round trip mismatch", n)
				}
			}
		})
	}
}

// TestWide64IsDense pins the property that makes wide64 free: it produces the
// exact same bytes as the accumulator, so the grouping is a pure speed change.
func TestWide64IsDense(t *testing.T) {
	for n := range 200 {
		units := randUnits(n)
		if a, b := packDense(nil, units), packWide64(nil, units); !slices.Equal(a, b) {
			t.Fatalf("n=%d: wide64 stream differs from dense", n)
		}
	}
}

const kernelUnits = 1 << 16

func BenchmarkPack(b *testing.B) {
	units := randUnits(kernelUnits)
	kernels := []struct {
		name string
		pack func([]byte, []uint8) []byte
	}{{"dense", packDense}, {"wide64", packWide64}, {"triple16", packTriple16}}
	for _, k := range kernels {
		b.Run(k.name, func(b *testing.B) {
			dst := make([]byte, 0, kernelUnits+slack)
			b.SetBytes(kernelUnits)
			for b.Loop() {
				dst = k.pack(dst[:0], units)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/kernelUnits, "ns/unit")
		})
	}
}

func BenchmarkUnpack(b *testing.B) {
	units := randUnits(kernelUnits)
	kernels := []struct {
		name   string
		pack   func([]byte, []uint8) []byte
		unpack func([]uint8, []byte, int) []uint8
	}{
		{"dense", packDense, unpackDense},
		{"wide64", packWide64, unpackWide64},
		{"triple16", packTriple16, unpackTriple16},
	}
	for _, k := range kernels {
		b.Run(k.name, func(b *testing.B) {
			src := append(k.pack(nil, units), make([]byte, slack)...)
			dst := make([]uint8, 0, kernelUnits)
			b.SetBytes(kernelUnits)
			for b.Loop() {
				dst = k.unpack(dst[:0], src, kernelUnits)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/kernelUnits, "ns/unit")
		})
	}
}
