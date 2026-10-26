//go:build !(amd64 || arm64) || purego

package codec

// useDictKernel: dictKernel is amd64's and arm64's. Everywhere else the Go
// loop is all there is.
const useDictKernel = false

// setDictKernel: there is no kernel to turn on.
func setDictKernel(bool) bool { return false }

func dictKernel(values *string, n int, table *dictShort, mask32 int, shift uint, ids *int64) (done, raw, lengths int) {
	return 0, 0, 0
}
