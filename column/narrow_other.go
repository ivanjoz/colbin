//go:build !amd64 || purego

package column

// narrowKernels is whether []int32 columns have kernels of their own: only
// amd64's. NEON ones were written and held to the int64 bytes on Graviton3, and
// measured 3% slower on TPC-H normalized encode than the int64 path: the sign
// extension costs what the halved input saves there, so arm64 keeps its columns
// at int64.
const narrowKernels = false

func scanSIMD32(vals []int32) (int64, bool, bool, int, int, int) { return 0, false, false, 0, 0, 0 }

func residualsSIMD32(vals []int32, transform uint8, base int64, zz bool, block []uint64) uint64 {
	return 0
}
