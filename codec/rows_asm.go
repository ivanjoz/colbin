//go:build (amd64 || arm64) && !purego

package codec

import "unsafe"

// useRowKernel is whether rowProgram.write hands its runs of spelled ops to
// rowKernel, in rows_amd64.s or rows_arm64.s. Both are baseline instructions
// only, so there is nothing to detect; the tests turn it off to hold it to the
// Go walk.
var useRowKernel = true

// rowKernel runs ops from ops[0], up to n of them, onto out from at, stopping
// at the first it does not spell or whose string the room to limit does not
// hold with reserve after it. It returns how many it ran and where the bytes
// end. See rows_amd64.s and rows_arm64.s.
//
//go:noescape
func rowKernel(ops *rowOp, n int, record unsafe.Pointer, out *byte, at, limit, reserve int) (done, end int)

// rowKernelWide is rowKernel at eight key bits, for rowProgram.writeWide.
//
//go:noescape
func rowKernelWide(ops *rowOp, n int, record unsafe.Pointer, out *byte, at, limit, reserve int) (done, end int)

// setRowKernel turns the kernel on or off, and says whether it was on.
func setRowKernel(on bool) bool {
	was := useRowKernel
	useRowKernel = on
	return was
}
