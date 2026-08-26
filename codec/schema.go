package codec

import (
	"encoding/binary"
	"fmt"
	"reflect"
	"sync"

	"github.com/ivanjoz/colbin/packed5"
)

// Schema section (JSON mode only). The columnar body says almost nothing about
// itself: a column carries a 3-bit ft* class, and everything else — a field's
// name, an integer column's width and signedness, whether a column is nullable,
// whether an empty column was elided — is derived from the Go type on both
// sides. A reader without that type needs all of it, so MarshalJSON writes it
// out once, ahead of the body:
//
//	schema    := [flags:1] [structCount:uvarint] structDef{structCount} rootDesc
//	structDef := [fieldCount:1] ( [id:1] packed5(jsonName) desc )*
//	desc      := [descFlags:1] extra
//
// descFlags holds the ft* class in its low three bits plus descNullable and
// descCyclic. extra depends on the class:
//
//	ftInt, ftFloat            [scalarKind:1]
//	ftString, ftBytes, ftAny  -
//	ftArray                   desc              (the element)
//	ftStruct                  [structIndex:uvarint]
//	ftMap                     desc desc         (key, then value)
//
// Struct types are hoisted into an indexed table rather than inlined so that a
// self-referential type (`type Node struct{ Kids []Node }`) describes itself in
// finite space: the index is reserved before the fields are walked, so a
// back-edge resolves to an already-assigned index.
//
// The section is built once per root type and cached, so the marshal path only
// copies bytes.

var schemaCache sync.Map // reflect.Type (root) -> []byte

// MarshalJSON encodes v the way Marshal does and prefixes the schema section, so
// the result can be turned into JSON by a reader that does not have the Go type
// (see DecodeAny and DecodeJSON). The body after the section is byte for byte
// what Marshal produces, and Unmarshal accepts either form.
func MarshalJSON(v any) (out []byte, err error) {
	defer recoverEncode(&out, &err)
	rv, err := marshalRoot(v)
	if err != nil {
		return nil, err
	}
	schema, err := schemaFor(rv.Type())
	if err != nil {
		return nil, err
	}
	prefix := make([]byte, 0, 12+len(schema))
	prefix = append(prefix, jsonFormatVersion)
	prefix = binary.AppendUvarint(prefix, uint64(len(schema)))
	prefix = append(prefix, schema...)
	return appendMessage(prefix, rv)
}

// schemaFor returns the cached schema section describing a root marshal type.
func schemaFor(t reflect.Type) ([]byte, error) {
	if cached, ok := schemaCache.Load(t); ok {
		return cached.([]byte), nil
	}
	b := schemaBuilder{index: make(map[reflect.Type]int, 4)}
	var flags byte
	var root []byte

	if topLevelIsRecords(t) {
		flags |= schRecords
		elemType := t
		if t.Kind() == reflect.Slice {
			elemType = t.Elem()
		} else {
			flags |= schSingleStruct // Marshal(struct) writes one record; render an object
		}
		ti, err := getTypeInfo(elemType)
		if err != nil {
			return nil, err
		}
		idx, err := b.structIndex(ti)
		if err != nil {
			return nil, err
		}
		// The root struct is never elided or nullable, so its descriptor is the
		// bare class plus its table index.
		root = binary.AppendUvarint([]byte{ftStruct}, uint64(idx))
	} else {
		fm, err := describeType(t) // value mode: one element column, N = 1
		if err != nil {
			return nil, err
		}
		if root, err = b.appendDesc(nil, &fm); err != nil {
			return nil, err
		}
	}

	out := append(make([]byte, 0, 64), flags)
	out = binary.AppendUvarint(out, uint64(len(b.defs)))
	for _, def := range b.defs {
		out = append(out, def...)
	}
	out = append(out, root...)
	schemaCache.Store(t, out)
	return out, nil
}

// schemaBuilder collects the struct table while descriptors are emitted.
type schemaBuilder struct {
	index map[reflect.Type]int
	defs  [][]byte
}

// structIndex returns ti's slot in the struct table, describing it on first use.
// The slot is reserved before the fields are walked so a field that refers back
// to ti resolves to this index instead of recursing forever.
func (b *schemaBuilder) structIndex(ti *typeInfo) (int, error) {
	if idx, ok := b.index[ti.rtype]; ok {
		return idx, nil
	}
	idx := len(b.defs)
	b.index[ti.rtype] = idx
	b.defs = append(b.defs, nil) // reserve the slot; filled in below

	def := []byte{byte(len(ti.fields))}
	for i := range ti.fields {
		fm := &ti.fields[i]
		def = append(def, fm.id)
		def = packed5.Append(def, fm.jsonName)
		var err error
		if def, err = b.appendDesc(def, fm); err != nil {
			return 0, err
		}
	}
	b.defs[idx] = def
	return idx, nil
}

// appendDesc writes one descriptor. For a nullable field the pointee descriptor
// supplies the class and the extras — a *[]T field keeps its slice details there,
// not on the pointer itself — while the pointer contributes the nullable bit.
func (b *schemaBuilder) appendDesc(out []byte, fm *fieldMeta) ([]byte, error) {
	d, nullBit := fm, byte(0)
	if fm.nullable {
		d, nullBit = fm.elem, descNullable
	}
	// cyclic travels with the descriptor whose column may be elided, so the
	// reader tests exactly the bit the encoder tested (see elideEmpty).
	out = append(out, d.fType|nullBit|boolBit(d.cyclic, 4))
	switch d.fType {
	case ftInt, ftFloat:
		kind, err := scalarKindOf(d.goKind)
		if err != nil {
			return nil, err
		}
		return append(out, kind), nil
	case ftArray:
		return b.appendDesc(out, d.elem)
	case ftStruct:
		idx, err := b.structIndex(d.sub)
		if err != nil {
			return nil, err
		}
		return binary.AppendUvarint(out, uint64(idx)), nil
	case ftMap:
		out, err := b.appendDesc(out, d.mapKey)
		if err != nil {
			return nil, err
		}
		return b.appendDesc(out, d.mapVal)
	}
	return out, nil // ftString, ftBytes, ftAny carry no extras
}

// scalarKindOf maps a Go scalar kind to its wire code. Width and signedness are
// both recovered from it: the width decides how many bytes the varint frame
// spans, the signedness how the value is rendered.
func scalarKindOf(k reflect.Kind) (uint8, error) {
	switch k {
	case reflect.Bool:
		return skBool, nil
	case reflect.Int8:
		return skInt8, nil
	case reflect.Int16:
		return skInt16, nil
	case reflect.Int32:
		return skInt32, nil
	case reflect.Int64:
		return skInt64, nil
	case reflect.Int:
		return skInt, nil
	case reflect.Uint8:
		return skUint8, nil
	case reflect.Uint16:
		return skUint16, nil
	case reflect.Uint32:
		return skUint32, nil
	case reflect.Uint64:
		return skUint64, nil
	case reflect.Uint:
		return skUint, nil
	case reflect.Float32:
		return skFloat32, nil
	case reflect.Float64:
		return skFloat64, nil
	}
	return 0, fmt.Errorf("colbin: cannot describe scalar kind %s", k)
}

// bitWidthOfKind is the native width of a scalar kind, in bits.
func bitWidthOfKind(kind uint8) uint8 {
	switch kind {
	case skBool, skInt8, skUint8:
		return 8
	case skInt16, skUint16:
		return 16
	case skInt32, skUint32, skFloat32:
		return 32
	}
	return 64
}
