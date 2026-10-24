//go:build amd64 && !purego

package wire

import "slices"

// The element copy in assembly (elements_amd64.s). A string column is mostly
// short values, and where a loader cut them from lines each one is its own
// cache miss at an address no prefetcher can guess; append pays a memmove call
// and a capacity check per value on top. The kernel copies a value with a
// couple of moves picked by its size, and asks for the bytes of the value a
// few places ahead before it needs them, which Go has no way to say.
// Baseline amd64 only: no AVX, so no CPU check.
const useStringElementsKernel = true

// elementsChunk bounds one call, which the scheduler cannot preempt.
const elementsChunk = 4096

func appendStringElementsKernel(buffer []byte, values []string, payload int) []byte {
	start := len(buffer)
	buffer = slices.Grow(buffer, payload)[:start+payload]
	at := start
	for len(values) > 0 {
		chunk := values[:min(len(values), elementsChunk)]
		at += copyElements(&buffer[at], &chunk[0], len(chunk))
		values = values[len(chunk):]
	}
	return buffer[:at]
}

// copyElements writes n values, each behind its size, from dst on, and
// returns how many bytes that took. dst must have room for them all.
//
//go:noescape
func copyElements(dst *byte, values *string, n int) int
