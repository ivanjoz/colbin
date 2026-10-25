//go:build !(amd64 || arm64) || purego

package codec

import "unsafe"

// useRowKernel: rowKernel is amd64's and arm64's. Everywhere else the Go walk
// is all there is.
const useRowKernel = false

func rowKernel(ops *rowOp, n int, record unsafe.Pointer, out *byte, at, limit, reserve int) (done, end int) {
	return 0, at
}

// setRowKernel: there is no kernel to turn on.
func setRowKernel(bool) bool { return false }

func rowKernelWide(ops *rowOp, n int, record unsafe.Pointer, out *byte, at, limit, reserve int) (done, end int) {
	return 0, at
}
