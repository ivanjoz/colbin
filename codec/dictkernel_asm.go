//go:build (amd64 || arm64) && !purego

package codec

// useDictKernel is whether dictColumn.scan hands its common case to
// dictKernel, in dictkernel_amd64.s or dictkernel_arm64.s. Both are baseline
// instructions only (SSE2 on amd64), so there is nothing to detect; the tests
// turn it off to hold it to the Go loop.
var useDictKernel = true

// setDictKernel turns the kernel on or off, and says whether it was on.
func setDictKernel(on bool) bool {
	was := useDictKernel
	useDictKernel = on
	return was
}

// dictKernel looks up, from values[0], up to n strings of at most sixteen
// bytes that the short table already holds, storing their ids from ids[0]. It
// stops at the first it does not take, and says how many it took, what they
// cost as strings, and the OR of their lengths. See dictkernel_amd64.s and
// dictkernel_arm64.s.
//
//go:noescape
func dictKernel(values *string, n int, table *dictShort, mask32 int, shift uint, ids *int64) (done, raw, lengths int)
