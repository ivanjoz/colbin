package colbin

// EncodeSlice and DecodeSlice: a slice of numbers on its own, for a caller that
// stores one in a column of its own — a database blob — and wants it small.
//
// This is not a colbin message. It is Go only, has no counterpart in the Rust
// or JavaScript ports, and Unmarshal does not read it; it is the column codec
// (package column) with a count in front of it:
//
//	[uvarint n] [column of n values]     (n = 0: the count alone)
//
// There is no header byte and no type tag, so the bytes do not say what they
// hold, and a reader has to know. In particular they do not say integer from
// float, or float32 from float64: decode into the kind that was encoded. An
// integer slice reads back into any integer type that holds its values (a
// value outside it is an error, not a truncation); a float slice reads back as
// the same type only.
//
// A float32 or float64 slice of decimals, the usual kind, is stored as
// integers, losslessly: see column.AppendFloat32 and column.AppendFloat64.

import (
	"encoding/binary"
	"errors"
	"reflect"
	"sync"
	"unsafe"

	"github.com/ivanjoz/colbin/column"
)

// Number is the element types EncodeSlice and DecodeSlice take. int, uint and
// uint64 are left out: int and uint differ in size between platforms, so what
// one wrote another could not always read, and a uint64 does not fit the
// column codec's int64. Convert to a fixed-width type at the call site.
type Number interface {
	~int8 | ~int16 | ~int32 | ~int64 | ~uint8 | ~uint16 | ~uint32 | ~float32 | ~float64
}

// MaxSliceLen is the longest slice DecodeSlice reads. A constant or decimal
// column is a handful of bytes whatever its length, so the count alone cannot
// be checked against the input, and a corrupt one would otherwise ask for
// gigabytes.
const MaxSliceLen = 1 << 26

var (
	errSliceCount    = errors.New("colbin: slice count is missing or past MaxSliceLen")
	errSliceTrailing = errors.New("colbin: bytes after the slice")
	errSliceRange    = errors.New("colbin: slice value out of range for the element type")
)

// EncodeSlice encodes vals: a count and the column codec's bytes for them.
func EncodeSlice[T Number](vals []T) []byte { return AppendSlice(nil, vals) }

// AppendSlice is EncodeSlice onto dst, which may be nil.
func AppendSlice[T Number](dst []byte, vals []T) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(vals)))
	if len(vals) == 0 {
		return dst
	}
	switch kindOf[T]() {
	case reflect.Int8:
		return appendWidened(dst, view[T, int8](vals))
	case reflect.Int16:
		return appendWidened(dst, view[T, int16](vals))
	case reflect.Uint8:
		return appendWidened(dst, view[T, uint8](vals))
	case reflect.Uint16:
		return appendWidened(dst, view[T, uint16](vals))
	case reflect.Int32:
		return column.AppendArray(dst, view[T, int32](vals))
	case reflect.Int64:
		return column.AppendArray(dst, view[T, int64](vals))
	case reflect.Float32:
		return column.AppendFloat32(dst, view[T, float32](vals))
	case reflect.Float64:
		// A float64's bit patterns are an int64 column, as the codec takes them.
		return column.AppendFloat64(dst, view[T, int64](vals))
	}
	// Unsigned: widened to int64, which holds every value, and which costs
	// nothing on the wire since the element type never reaches it.
	wide := make([]int64, len(vals))
	for index, v := range vals {
		wide[index] = int64(v)
	}
	return column.AppendArray(dst, wide)
}

// DecodeSlice decodes what EncodeSlice wrote into a new slice of T. A
// zero-length slice decodes to an empty one, not nil. It is an error for the
// input to hold anything after the slice.
//
// T must be of the kind that was encoded: an integer type for integers, and
// float32 or float64 for the same of floats. See the file comment.
func DecodeSlice[T Number](data []byte) ([]T, error) {
	n, used := binary.Uvarint(data)
	if used <= 0 || n > MaxSliceLen {
		return nil, errSliceCount
	}
	data = data[used:]
	out := make([]T, n)
	if n == 0 {
		if len(data) != 0 {
			return nil, errSliceTrailing
		}
		return out, nil
	}
	var read int
	var err error
	switch kindOf[T]() {
	case reflect.Int8:
		read, err = column.DecodeArray(data, int(n), view[T, int8](out))
	case reflect.Int16:
		read, err = column.DecodeArray(data, int(n), view[T, int16](out))
	case reflect.Int32:
		read, err = column.DecodeArray(data, int(n), view[T, int32](out))
	case reflect.Int64:
		read, err = column.DecodeArray(data, int(n), view[T, int64](out))
	case reflect.Float32:
		read, err = column.DecodeFloat32(data, int(n), view[T, float32](out))
	case reflect.Float64:
		read, err = column.DecodeArray(data, int(n), view[T, int64](out))
	default:
		read, err = decodeUnsigned(data, out)
	}
	if err != nil {
		return nil, err
	}
	if read != len(data) {
		return nil, errSliceTrailing
	}
	return out, nil
}

// widenPool holds the int32 columns appendWidened encodes from.
var widenPool sync.Pool // *[]int32

// appendWidened encodes a column of a 8- or 16-bit type through an int32 one.
// The bytes are the same, but the column codec has no kernels for the narrow
// widths, and the copy and the int32 kernels together are about four times
// faster than its scalar path at a few hundred values. Unsigned values fit
// int32 too, and so are not allocated for.
func appendWidened[V int8 | int16 | uint8 | uint16](dst []byte, vals []V) []byte {
	holder, _ := widenPool.Get().(*[]int32)
	if holder == nil {
		holder = new([]int32)
	}
	if cap(*holder) < len(vals) {
		*holder = make([]int32, len(vals))
	}
	wide := (*holder)[:len(vals)]
	for index, v := range vals {
		wide[index] = int32(v)
	}
	dst = column.AppendArray(dst, wide)
	widenPool.Put(holder)
	return dst
}

// decodeUnsigned reads an unsigned slice through int64, refusing what T does
// not hold.
func decodeUnsigned[T Number](data []byte, out []T) (int, error) {
	wide := make([]int64, len(out))
	read, err := column.DecodeArray(data, len(out), wide)
	if err != nil {
		return 0, err
	}
	for index, v := range wide {
		// A negative value or one past T wraps, so it does not convert back.
		if int64(T(v)) != v {
			return 0, errSliceRange
		}
		out[index] = T(v)
	}
	return read, nil
}

// kindOf is T's underlying kind, which a type switch on the value cannot give
// for a named type.
func kindOf[T Number]() reflect.Kind { return reflect.TypeFor[T]().Kind() }

// view is vals as a slice of the type of the same size and layout, which
// kindOf has said it is: a float64 as an int64 holds its bit pattern, and a
// named int32 type is an int32.
func view[T Number, V any](vals []T) []V {
	return unsafe.Slice((*V)(unsafe.Pointer(unsafe.SliceData(vals))), len(vals))
}
