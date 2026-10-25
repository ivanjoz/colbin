//go:build !(amd64 || arm64) || purego

package wire

const useStringElementsKernel = false

func appendStringElementsKernel(buffer []byte, values []string, payload int) []byte {
	return appendStringElementsLoop(buffer, values)
}
