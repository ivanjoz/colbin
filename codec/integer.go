package codec

import colvarint "github.com/ivanjoz/colbin/varint"

// appendIntColumn adapts colbin's normalized int64 scratch values to the native
// signed width expected by varint. Converting unsigned Go values through the
// same-width signed type preserves their bit pattern while retaining the
// codec's narrow fixed-width fallback.
func appendIntColumn(out []byte, values []int64, width uint8) []byte {
	// A column of nothing but zeros is the type byte and no payload. This is the
	// whole of omit-empty for integers, and through the length sub-columns that
	// frame them, for bytes, arrays and maps as well.
	if omitEmpty.Load() && allZero(values) {
		return append(out, ftInt|emptyColumnBit)
	}
	out = append(out, ftInt)
	switch width {
	case 8:
		buf := getI8(len(values))
		for i, value := range values {
			(*buf)[i] = int8(value)
		}
		out = colvarint.AppendArray(out, *buf)
		putI8(buf)
	case 16:
		buf := getI16(len(values))
		for i, value := range values {
			(*buf)[i] = int16(value)
		}
		out = colvarint.AppendArray(out, *buf)
		putI16(buf)
	case 32:
		buf := getI32(len(values))
		for i, value := range values {
			(*buf)[i] = int32(value)
		}
		out = colvarint.AppendArray(out, *buf)
		putI32(buf)
	default:
		out = colvarint.AppendArray(out, values)
	}
	return out
}

// decodeIntColumn reverses appendIntColumn and widens values for colbin's
// existing typed setters. It returns the varint frame's exact byte span.
func decodeIntColumn(buf []byte, n int, width uint8, out []int64) (int, error) {
	switch width {
	case 8:
		values := getI8(n)
		consumed, err := colvarint.DecodeArray(buf, n, *values)
		if err == nil {
			for i, value := range *values {
				out[i] = int64(value)
			}
		}
		putI8(values)
		return consumed, err
	case 16:
		values := getI16(n)
		consumed, err := colvarint.DecodeArray(buf, n, *values)
		if err == nil {
			for i, value := range *values {
				out[i] = int64(value)
			}
		}
		putI16(values)
		return consumed, err
	case 32:
		values := getI32(n)
		consumed, err := colvarint.DecodeArray(buf, n, *values)
		if err == nil {
			for i, value := range *values {
				out[i] = int64(value)
			}
		}
		putI32(values)
		return consumed, err
	default:
		return colvarint.DecodeArray(buf, n, out)
	}
}
