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
