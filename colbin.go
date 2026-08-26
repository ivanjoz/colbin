package colbin

import "github.com/ivanjoz/colbin/codec"

// Marshal encodes v into the colbin format. Structs and slices of structs use
// the columnar records layout; other supported values use the single-value
// layout. Pointers are dereferenced.
func Marshal(v any) ([]byte, error) {
	return codec.Marshal(v)
}

// Unmarshal decodes a colbin message into dst, which must be a non-nil pointer
// to a compatible Go value.
func Unmarshal(data []byte, dst any) error {
	return codec.Unmarshal(data, dst)
}

// MarshalJSON encodes v like Marshal and prefixes a schema section: the field
// names (from the json tag, else the cb name, else the Go field name) plus the
// type facts the columnar body does not carry. The body itself is byte for byte
// what Marshal writes, so nothing about the plain binary mode changes — the
// schema is the only difference, it is built once per type and cached, and
// Unmarshal accepts either form.
//
// Use it when the reader has no matching Go type and wants JSON: see DecodeJSON
// and DecodeAny.
func MarshalJSON(v any) ([]byte, error) {
	return codec.MarshalJSON(v)
}

// DecodeJSON turns a message written by MarshalJSON into JSON text, using only
// the message's own schema.
func DecodeJSON(data []byte) ([]byte, error) {
	return codec.DecodeJSON(data)
}

// DecodeAny decodes a message written by MarshalJSON into plain Go values —
// []any of map[string]any for records, or the value itself in value mode —
// without needing the original Go type.
func DecodeAny(data []byte) (any, error) {
	return codec.DecodeAny(data)
}
