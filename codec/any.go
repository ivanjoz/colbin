package codec

import (
	"encoding/binary"
	"fmt"
	"math"
	"reflect"
	"unsafe"

	"github.com/ivanjoz/colbin/packed5"
	colvarint "github.com/ivanjoz/colbin/varint"
)

// ftAny support: interface{} values cannot be columnarized because their concrete
// type is unknown at build time and may differ per value. So an ftAny column is a
// self-describing, per-value tagged encoding — a compact escape hatch living inside
// the columnar frame, while every other column stays columnar.
//
// An ftAny column is [flags:1=ftAny] followed by N tagged values (read
// sequentially; each value's span is self-describing, like ftArray/ftMap already
// are). Decode normalizes to the canonical JSON-ish set — nil / bool / int64 /
// uint64 / float64 / string / []byte / []any / map[string]any.

// value tags (1 byte prefixing every any-value). Bool is folded into the tag so it
// costs zero payload bytes.
const (
	aNil   uint8 = 0
	aFalse uint8 = 1
	aTrue  uint8 = 2
	aInt   uint8 = 3 // singleton varint array -> int64
	aUint  uint8 = 4 // singleton varint array -> uint64 bit pattern
	aFloat uint8 = 5 // 8 bytes LE     -> float64
	aStr   uint8 = 6 // one self-delimiting packed5 frame
	aBytes uint8 = 7 // uvarint len + bytes
	aArr   uint8 = 8 // uvarint count + value*             -> []any
	aMap   uint8 = 9 // uvarint count + (packed5 key, value)* -> map[string]any
)

// encodeError is panicked by the any encoder for dynamic types it cannot represent
// and recovered by Marshal into a normal error. The rest of the encode path can't
// fail (types are validated at build time), so this keeps its signatures clean.
type encodeError struct{ err error }

// --- encode ---

// encodeAnyColumn writes N self-describing values, one per slot. slotPtrs point at
// the interface{} headers (struct field, array element, or map value backing).
func encodeAnyColumn(out []byte, fm *fieldMeta, slotPtrs []unsafe.Pointer) []byte {
	for _, p := range slotPtrs {
		out = encodeAnyValue(out, reflect.NewAt(fm.ifaceType, p).Elem())
	}
	return out
}

// encodeAnyValue appends one tagged value, recursing into slices/arrays/maps.
// Dispatch is by reflect.Kind so any Go numeric/native type is accepted.
func encodeAnyValue(out []byte, v reflect.Value) []byte {
	// Unwrap interfaces (and nil interfaces) to the concrete value.
	for v.Kind() == reflect.Interface || v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return append(out, aNil)
		}
		v = v.Elem()
	}
	if !v.IsValid() {
		return append(out, aNil)
	}
	switch v.Kind() {
	case reflect.Bool:
		if v.Bool() {
			return append(out, aTrue)
		}
		return append(out, aFalse)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return appendAnyInt(append(out, aInt), v.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return appendAnyInt(append(out, aUint), int64(v.Uint()))
	case reflect.Float32, reflect.Float64:
		return binary.LittleEndian.AppendUint64(append(out, aFloat), math.Float64bits(v.Float()))
	case reflect.String:
		return packed5.Append(append(out, aStr), v.String())
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 && v.Kind() == reflect.Slice { // []byte
			b := v.Bytes()
			out = binary.AppendUvarint(append(out, aBytes), uint64(len(b)))
			return append(out, b...)
		}
		out = binary.AppendUvarint(append(out, aArr), uint64(v.Len()))
		for i := 0; i < v.Len(); i++ {
			out = encodeAnyValue(out, v.Index(i))
		}
		return out
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			panic(encodeError{fmt.Errorf("colbin: any-held map must have string keys, got %s", v.Type().Key())})
		}
		out = binary.AppendUvarint(append(out, aMap), uint64(v.Len()))
		it := v.MapRange()
		for it.Next() {
			out = packed5.Append(out, it.Key().String())
			out = encodeAnyValue(out, it.Value())
		}
		return out
	default:
		panic(encodeError{fmt.Errorf("colbin: cannot encode %s inside an interface{}", v.Type())})
	}
}

func appendAnyInt(out []byte, value int64) []byte {
	values := [1]int64{value}
	return colvarint.AppendArray(out, values[:])
}

// --- decode ---

// decodeAnyColumn reverses encodeAnyColumn, setting each decoded value into its slot.
func (dec *decoder) decodeAnyColumn(fm *fieldMeta, n int, slotPtrs []unsafe.Pointer) error {
	dec.readByte() // ftAny flags
	for i := range n {
		val, err := dec.decodeAnyValue()
		if err != nil {
			return err
		}
		slot := reflect.NewAt(fm.ifaceType, slotPtrs[i]).Elem()
		if val == nil {
			slot.Set(reflect.Zero(fm.ifaceType)) // nil interface
		} else {
			slot.Set(reflect.ValueOf(val))
		}
	}
	return nil
}

// decodeAnyValue reads one tagged value into the canonical Go type.
func (dec *decoder) decodeAnyValue() (any, error) {
	switch dec.readByte() {
	case aNil:
		return nil, nil
	case aFalse:
		return false, nil
	case aTrue:
		return true, nil
	case aInt:
		return dec.readAnyInt()
	case aUint:
		v, err := dec.readAnyInt()
		return uint64(v), err
	case aFloat:
		bits := binary.LittleEndian.Uint64(dec.data[dec.pos : dec.pos+8])
		dec.pos += 8
		return math.Float64frombits(bits), nil
	case aStr:
		return dec.readPacked5String()
	case aBytes:
		l := int(dec.readUvarint())
		b := cloneBytes(dec.data[dec.pos : dec.pos+l])
		dec.pos += l
		return b, nil
	case aArr:
		arr := make([]any, int(dec.readUvarint()))
		for i := range arr {
			v, err := dec.decodeAnyValue()
			if err != nil {
				return nil, err
			}
			arr[i] = v
		}
		return arr, nil
	case aMap:
		n := int(dec.readUvarint())
		m := make(map[string]any, n)
		for range n {
			key, err := dec.readPacked5String()
			if err != nil {
				return nil, err
			}
			value, err := dec.decodeAnyValue()
			if err != nil {
				return nil, err
			}
			m[key] = value
		}
		return m, nil
	default:
		return nil, fmt.Errorf("colbin: bad any-value tag at pos %d", dec.pos-1)
	}
}

func (dec *decoder) readUvarint() uint64 {
	x, m := binary.Uvarint(dec.data[dec.pos:])
	dec.pos += m
	return x
}

func (dec *decoder) readAnyInt() (int64, error) {
	var out [1]int64
	consumed, err := colvarint.DecodeArray(dec.data[dec.pos:], 1, out[:])
	if err != nil {
		return 0, err
	}
	dec.pos += consumed
	return out[0], nil
}
