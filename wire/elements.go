package wire

import "encoding/binary"

// appendStringElements appends values as a list's elements, each behind its size,
// payload bytes in all, which the caller has already added up. Where there is
// a kernel (elements_asm.go) it does the copying; this loop is the
// reference it is held to.
func appendStringElements(buffer []byte, values []string, payload int) []byte {
	if useStringElementsKernel {
		return appendStringElementsKernel(buffer, values, payload)
	}
	return appendStringElementsLoop(buffer, values)
}

func appendStringElementsLoop(buffer []byte, values []string) []byte {
	for _, value := range values {
		if len(value) <= inlineElementSize {
			buffer = append(buffer, uint8(len(value)))
		} else {
			buffer = binary.LittleEndian.AppendUint32(
				append(buffer, elementSizeEscape), uint32(len(value)))
		}
		buffer = append(buffer, value...)
	}
	return buffer
}
