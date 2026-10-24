package codec

// Maps of basic types, without reflection.
//
// maps.go reaches a map through reflection, because Go gives no way to walk
// one through an unsafe pointer, and pays a reflect.Value per key and per value
// for it — on a struct with one small map, most of its encode and decode.
//
// But a map whose key and value are unnamed basic types is one of a few dozen
// Go types, and each of those is compiled into this package below as an
// instantiation. The plan looks the field's type up when it first meets it, and
// from then on the map is walked as the Go type it is: a range, a sort of the
// keys and a typed store, which is what a code generator would have written —
// done once per type at plan time instead of once per build.
//
// Anything else — a named key or value type, a struct, an `any` — stays on the
// reflection path. The bytes are the same either way: the entries in the same
// key order, each written by the same element writer.

import (
	"cmp"
	"math"
	"math/bits"
	"reflect"
	"slices"
	"unsafe"

	"github.com/ivanjoz/colbin/wire"
)

// mapOps is one map type's compiled walks, at both key widths.
type mapOps struct {
	appendNarrow func(writer *wire.Writer, key uint8, at unsafe.Pointer)
	appendWide   func(writer *wire.Writer8, key uint8, at unsafe.Pointer)
	readNarrow   func(entries *wire.Reader, count int, at unsafe.Pointer) error
	readWide     func(entries *wire.Reader8, count int, at unsafe.Pointer) error
}

type (
	typedMapKey interface {
		string | int | int32 | int64 | uint32 | uint64
	}
	typedMapValue interface {
		typedMapKey | float32 | float64 | bool
	}
)

// typedMaps is every compiled map type, keyed by its unnamed Go type.
var typedMaps = map[reflect.Type]*mapOps{}

func init() {
	typedMapKeyed[string]()
	typedMapKeyed[int]()
	typedMapKeyed[int32]()
	typedMapKeyed[int64]()
	typedMapKeyed[uint32]()
	typedMapKeyed[uint64]()
}

func typedMapKeyed[K typedMapKey]() {
	typedMapOf[K, string]()
	typedMapOf[K, int]()
	typedMapOf[K, int32]()
	typedMapOf[K, int64]()
	typedMapOf[K, uint32]()
	typedMapOf[K, uint64]()
	typedMapOf[K, float32]()
	typedMapOf[K, float64]()
	typedMapOf[K, bool]()
}

func typedMapOf[K typedMapKey, V typedMapValue]() {
	typedMaps[reflect.TypeFor[map[K]V]()] = &mapOps{
		appendNarrow: appendTypedMapNarrow[K, V],
		appendWide:   appendTypedMapWide[K, V],
		readNarrow:   readTypedMapNarrow[K, V],
		readWide:     readTypedMapWide[K, V],
	}
}

// typedMapFor is the compiled walks for a map type, or nil for one that is
// not compiled. A named map type is its underlying type here — the same
// memory — but a named key or value type is not: only the basic types are.
func typedMapFor(mapType reflect.Type) *mapOps {
	return typedMaps[reflect.MapOf(mapType.Key(), mapType.Elem())]
}

// entry is a map's key and value, gathered so that the entries sort by key
// without a second lookup for each value.
type entry[K typedMapKey, V typedMapValue] struct {
	key   K
	value V
}

// smallMap is how many entries a map's walk sorts on the stack; a bigger one
// takes an allocation for them.
const smallMap = 16

// sortedEntries appends a map's entries to entries in key order, which is the
// order maps.go writes them in: sortMapKeys compares strings with < and
// integers by value.
func sortedEntries[K typedMapKey, V typedMapValue](entries []entry[K, V], values map[K]V) []entry[K, V] {
	for key, value := range values {
		entries = append(entries, entry[K, V]{key, value})
	}
	slices.SortFunc(entries, func(a, b entry[K, V]) int { return cmp.Compare(a.key, b.key) })
	return entries
}

func appendTypedMapNarrow[K typedMapKey, V typedMapValue](writer *wire.Writer, key uint8, at unsafe.Pointer) {
	var stack [smallMap]entry[K, V]
	entries := sortedEntries(stack[:0], *(*map[K]V)(at))
	mark := writer.OpenMap(key, len(entries))
	for _, entry := range entries {
		putNarrowElement(writer, entry.key)
		putNarrowElement(writer, entry.value)
	}
	writer.Close(mark)
}

func appendTypedMapWide[K typedMapKey, V typedMapValue](writer *wire.Writer8, key uint8, at unsafe.Pointer) {
	var stack [smallMap]entry[K, V]
	entries := sortedEntries(stack[:0], *(*map[K]V)(at))
	mark := writer.OpenMap(key, len(entries))
	for _, entry := range entries {
		putWideElement(writer, entry.key)
		putWideElement(writer, entry.value)
	}
	writer.Close(mark)
}

// putNarrowElement is writeNarrowMapValue for a typed value: the switch is on
// the instantiation's type, which the compiler resolves for each one.
func putNarrowElement[T typedMapValue](writer *wire.Writer, value T) {
	switch value := any(value).(type) {
	case string:
		writer.ElementString(value)
	case int:
		writer.ElementInt(int64(value))
	case int32:
		writer.ElementInt(int64(value))
	case int64:
		writer.ElementInt(value)
	case uint32:
		writer.ElementUint(uint64(value))
	case uint64:
		writer.ElementUint(value)
	case float32:
		writer.ElementUint(uint64(bits.ReverseBytes32(math.Float32bits(value))))
	case float64:
		writer.ElementUint(bits.ReverseBytes64(math.Float64bits(value)))
	case bool:
		if value {
			writer.ElementUint(1)
		} else {
			writer.ElementUint(0)
		}
	}
}

// putWideElement is writeMapValue for a typed value.
func putWideElement[T typedMapValue](writer *wire.Writer8, value T) {
	switch value := any(value).(type) {
	case string:
		writer.ElementString(value)
	case int:
		writer.ElementInt(int64(value))
	case int32:
		writer.ElementInt(int64(value))
	case int64:
		writer.ElementInt(value)
	case uint32:
		writer.ElementUint(uint64(value))
	case uint64:
		writer.ElementUint(value)
	case float32:
		writer.ElementUint(uint64(bits.ReverseBytes32(math.Float32bits(value))))
	case float64:
		writer.ElementUint(bits.ReverseBytes64(math.Float64bits(value)))
	case bool:
		if value {
			writer.ElementUint(1)
		} else {
			writer.ElementUint(0)
		}
	}
}

// readTypedMapNarrow fills a map of count entries, and stores it only once
// every entry has read: a map that fails half way is not left behind, as
// readNarrowMap does not leave one.
func readTypedMapNarrow[K typedMapKey, V typedMapValue](entries *wire.Reader, count int, at unsafe.Pointer) error {
	built := make(map[K]V, count)
	for range count {
		key, err := takeNarrowElement[K](entries)
		if err != nil {
			return err
		}
		value, err := takeNarrowElement[V](entries)
		if err != nil {
			return err
		}
		if err := entries.Err(); err != nil {
			return err
		}
		built[key] = value
	}
	*(*map[K]V)(at) = built
	return nil
}

func readTypedMapWide[K typedMapKey, V typedMapValue](entries *wire.Reader8, count int, at unsafe.Pointer) error {
	built := make(map[K]V, count)
	for range count {
		key, err := takeWideElement[K](entries)
		if err != nil {
			return err
		}
		value, err := takeWideElement[V](entries)
		if err != nil {
			return err
		}
		if err := entries.Err(); err != nil {
			return err
		}
		built[key] = value
	}
	*(*map[K]V)(at) = built
	return nil
}

// takeNarrowElement is readNarrowMapValue for a typed value, refusing an
// integer wider than T as setInt and setUint do.
func takeNarrowElement[T typedMapValue](reader *wire.Reader) (T, error) {
	var out T
	var err error
	switch into := any(&out).(type) {
	case *string:
		*into = reader.ElementString()
	case *int:
		err = narrowInt(into, reader.ElementInt())
	case *int32:
		err = narrowInt(into, reader.ElementInt())
	case *int64:
		*into = reader.ElementInt()
	case *uint32:
		err = narrowUint(into, reader.ElementUint())
	case *uint64:
		*into = reader.ElementUint()
	case *float32:
		*into = math.Float32frombits(bits.ReverseBytes32(uint32(reader.ElementUint())))
	case *float64:
		*into = math.Float64frombits(bits.ReverseBytes64(reader.ElementUint()))
	case *bool:
		*into = reader.ElementUint() == 1
	}
	return out, err
}

func takeWideElement[T typedMapValue](reader *wire.Reader8) (T, error) {
	var out T
	var err error
	switch into := any(&out).(type) {
	case *string:
		*into = reader.ElementString()
	case *int:
		err = narrowInt(into, reader.ElementInt())
	case *int32:
		err = narrowInt(into, reader.ElementInt())
	case *int64:
		*into = reader.ElementInt()
	case *uint32:
		err = narrowUint(into, reader.ElementUint())
	case *uint64:
		*into = reader.ElementUint()
	case *float32:
		*into = math.Float32frombits(bits.ReverseBytes32(uint32(reader.ElementUint())))
	case *float64:
		*into = math.Float64frombits(bits.ReverseBytes64(reader.ElementUint()))
	case *bool:
		*into = reader.ElementUint() == 1
	}
	return out, err
}

func narrowInt[T int | int32](into *T, value int64) error {
	if int64(T(value)) != value {
		return wire.ErrFieldTooWide
	}
	*into = T(value)
	return nil
}

func narrowUint(into *uint32, value uint64) error {
	if uint64(uint32(value)) != value {
		return wire.ErrFieldTooWide
	}
	*into = uint32(value)
	return nil
}
