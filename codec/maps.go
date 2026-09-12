package codec

// Maps, over wire's MAP composite.
//
//	map  [key][desc][bytelen][count]  ( [desc][key] [desc][value] )*
//
// A map's keys are *values*, not field ids, so there is no key width to choose
// and nothing to look up: the entries are pairs of ordinary key-less values, the
// same shape a list's elements are.
//
// # Reflection per entry, deliberately
//
// Every other field kind is reached by offset with no reflection left at encode
// time. A map cannot be: Go gives no way to walk or fill one through an unsafe
// pointer, so this uses reflect.MapRange and reflect.Value.SetMapIndex and pays
// for them.
//
// That is the honest cost of the kind rather than an oversight. A map field is
// already a hash lookup per entry on both sides; the reflection sits on top of
// something that was never going to be a strided store. A caller with a hot path
// and a fixed key set should use a struct, which is what the rest of this format
// is for.
//
// # What a key and a value may be
//
// Strings and integers as keys, and those plus floats and bools as values.
// Anything else is refused at plan time with the field named, because a map of
// structs is a shape this has no form for yet and silently dropping it would be
// worse than saying so.

import (
	"fmt"
	"math"
	"math/bits"
	"reflect"
	"unsafe"

	"github.com/ivanjoz/colbin/wire"
)

// mapKind is what a map's key or value is, narrowed to the set the wire carries
// as a bare element.
//
// Like fieldOp, these values go on the wire in a schema section and must not be
// reordered. The two float widths are separate kinds for that reason and no
// other: a Go decoder reads the width off the destination field, but a reader
// working from a section has no destination, and a 32-bit reversed bit pattern
// read as a 64-bit one is silent nonsense rather than an error.
type mapKind uint8

const (
	mapString mapKind = iota
	mapInt
	mapUint
	mapFloat64
	mapBool
	mapFloat32

	// mapKindCount bounds the block, so a section naming a kind this version
	// does not assign is refused.
	mapKindCount
)

// mapKindOf narrows a type to a map kind, or refuses it.
func mapKindOf(t reflect.Type, what string) (mapKind, error) {
	switch t.Kind() {
	case reflect.String:
		return mapString, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return mapInt, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return mapUint, nil
	case reflect.Float32:
		if what == "key" {
			return 0, fmt.Errorf("a float is not a map key this format carries")
		}
		return mapFloat32, nil
	case reflect.Float64:
		if what == "key" {
			return 0, fmt.Errorf("a float is not a map key this format carries")
		}
		return mapFloat64, nil
	case reflect.Bool:
		if what == "key" {
			return 0, fmt.Errorf("a bool is not a map key this format carries")
		}
		return mapBool, nil
	}
	return 0, fmt.Errorf("a map %s of %s is not carried", what, t)
}

// mapOpFor resolves a map field, or reports that the type is not one.
func mapOpFor(fieldType reflect.Type) (keyKind, valueKind mapKind, err error) {
	if fieldType.Kind() != reflect.Map {
		return 0, 0, fmt.Errorf("not a map")
	}
	if keyKind, err = mapKindOf(fieldType.Key(), "key"); err != nil {
		return 0, 0, err
	}
	if valueKind, err = mapKindOf(fieldType.Elem(), "value"); err != nil {
		return 0, 0, err
	}
	return keyKind, valueKind, nil
}

// appendMapField writes a map, and nothing at all when it is empty — an absent
// key means an empty map, exactly as it means a zero scalar.
func appendMapField(writer *wire.Writer8, field *planField, at unsafe.Pointer) {
	value := reflect.NewAt(field.sliceType, at).Elem()
	count := value.Len()
	if count == 0 {
		return
	}
	mark := writer.OpenMap(field.key, count)
	for entries := value.MapRange(); entries.Next(); {
		writeMapValue(writer, field.keyKind, entries.Key())
		writeMapValue(writer, field.valueKind, entries.Value())
	}
	writer.Close(mark)
}

func writeMapValue(writer *wire.Writer8, kind mapKind, value reflect.Value) {
	switch kind {
	case mapString:
		writer.ElementString(value.String())
	case mapInt:
		writer.ElementInt(value.Int())
	case mapUint:
		writer.ElementUint(value.Uint())
	case mapFloat32, mapFloat64:
		// A float rides in the integer shape with its bytes reversed, the same
		// way a scalar float field does.
		writer.ElementUint(reverseFloatBits(value))
	case mapBool:
		if value.Bool() {
			writer.ElementUint(1)
		} else {
			writer.ElementUint(0)
		}
	}
}

// readMapField reads a map, allocating it at the entry count the message
// declares.
func readMapField(reader *wire.Reader8, field *planField, at unsafe.Pointer) {
	count, entries, ok := reader.Map()
	if !ok {
		return
	}
	target := reflect.NewAt(field.sliceType, at).Elem()
	built := reflect.MakeMapWithSize(field.sliceType, count)
	key := reflect.New(field.sliceType.Key()).Elem()
	value := reflect.New(field.sliceType.Elem()).Elem()
	for range count {
		readMapValue(&entries, field.keyKind, key)
		readMapValue(&entries, field.valueKind, value)
		if entries.Err() != nil {
			reader.Fail(entries.Err())
			return
		}
		built.SetMapIndex(key, value)
	}
	target.Set(built)
}

func readMapValue(reader *wire.Reader8, kind mapKind, into reflect.Value) {
	switch kind {
	case mapString:
		into.SetString(reader.ElementString())
	case mapInt:
		into.SetInt(reader.ElementInt())
	case mapUint:
		into.SetUint(reader.ElementUint())
	case mapFloat32, mapFloat64:
		setFloatFromReversed(into, reader.ElementUint())
	case mapBool:
		into.SetBool(reader.ElementUint() == 1)
	}
}

// reverseFloatBits and setFloatFromReversed put a float through the same byte
// reversal a scalar float field goes through, so that a round value costs two
// bytes here as well. See wire's F64.
func reverseFloatBits(value reflect.Value) uint64 {
	if value.Type().Bits() == 32 {
		return uint64(bits.ReverseBytes32(math.Float32bits(float32(value.Float()))))
	}
	return bits.ReverseBytes64(math.Float64bits(value.Float()))
}

func setFloatFromReversed(into reflect.Value, raw uint64) {
	if into.Type().Bits() == 32 {
		into.SetFloat(float64(math.Float32frombits(bits.ReverseBytes32(uint32(raw)))))
		return
	}
	into.SetFloat(math.Float64frombits(bits.ReverseBytes64(raw)))
}
