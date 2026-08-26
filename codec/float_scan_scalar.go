//go:build !goexperiment.simd || !amd64

package codec

func allZeroFloat64s(vals []float64) bool {
	return allZeroFloat64sScalar(vals)
}
