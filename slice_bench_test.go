package colbin_test

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/ivanjoz/colbin"
)

func benchSlice[T colbin.Number](b *testing.B, name string, n int, value func(*rand.Rand, int) T) {
	r := rand.New(rand.NewPCG(1, 2))
	vals := fill(n, func(i int) T { return value(r, i) })
	encoded := colbin.EncodeSlice(vals)
	label := fmt.Sprintf("%s/n=%d", name, n)
	b.Run(label+"/encode", func(b *testing.B) {
		buf := make([]byte, 0, len(encoded)*2)
		for b.Loop() {
			buf = colbin.AppendSlice(buf[:0], vals)
		}
		b.ReportMetric(float64(len(encoded)), "bytes")
	})
	b.Run(label+"/decode", func(b *testing.B) {
		for b.Loop() {
			if _, err := colbin.DecodeSlice[T](encoded); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkSlice(b *testing.B) {
	for _, n := range []int{16, 200, 5000} {
		benchSlice(b, "int32 ids", n, func(r *rand.Rand, i int) int32 { return int32(500000 + i*3 + r.IntN(3)) })
		benchSlice(b, "int16", n, func(r *rand.Rand, i int) int16 { return int16(r.IntN(2000)) })
		benchSlice(b, "uint16", n, func(r *rand.Rand, i int) uint16 { return uint16(r.IntN(2000)) })
		benchSlice(b, "float32 prices", n, func(r *rand.Rand, i int) float32 { return float32(r.IntN(100000)) / 100 })
		benchSlice(b, "float64 prices", n, func(r *rand.Rand, i int) float64 { return float64(r.IntN(100000)) / 100 })
	}
}
