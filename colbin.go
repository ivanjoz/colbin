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

// Codec is a handle for one struct type: the field layout, the accessors and the
// mode decision resolved once and held, so encoding a record costs neither the
// type lookup nor the reflection that Marshal repeats on every call.
//
// It is what to use for many small messages -- one record per message, thousands
// of them -- where that per-call work is a real share of the cost. Build it once,
// usually at package scope, and reuse it; it is safe for concurrent use.
//
//	var statsCodec = colbin.MustCodec[SaleOrderProductStats]()
//
//	buf := make([]byte, 0, 64)
//	for _, rec := range records {
//	    buf, _ = statsCodec.Append(buf[:0], &rec)   // no allocation per record
//	    send(buf)
//	}
//
// Messages it writes are ordinary colbin messages: Unmarshal reads them, and a
// Codec reads what Marshal wrote.
type Codec[T any] = codec.Codec[T]

// NewCodec builds the handle for T, which must be a struct.
func NewCodec[T any]() (*Codec[T], error) { return codec.NewCodec[T]() }

// MustCodec is NewCodec for a package-level variable, where a type error is a
// programming error and there is nobody to return it to.
func MustCodec[T any]() *Codec[T] { return codec.MustCodec[T]() }

// SetOmitEmpty turns omit-empty encoding on or off, globally.
//
// A colbin column is positional: N records, N values, and a value that happens
// to be zero still takes its slot — the integer codec floors at a byte per
// element, so a thousand records of an untouched field cost a thousand bytes to
// say nothing. With omit-empty on, a column holding nothing but empty values
// (zero, "", empty slice, nil) is written as its type byte alone. On a sparse
// ten-field struct at a thousand records that is 9034 B down to 1028 B.
//
// It also lets compact mode carry pointer fields, which it otherwise cannot:
// compact mode says a field is present by naming it, so nil has to mean absent —
// and then a pointer to the zero value, equally absent, comes back nil. That is
// the one thing the flag costs, and the reason it is opt-in:
//
//	a *T pointing at T's zero value decodes back as nil.
//
// Decoding needs no configuration: both forms are self-describing, so a reader
// never has to be set to match its writer. A message written with the flag on
// does carry its own version byte, so a decoder that predates the flag rejects
// it rather than misreading it.
//
// Set it once at startup, before the first Marshal.
func SetOmitEmpty(on bool) { codec.SetOmitEmpty(on) }

// OmitEmpty reports whether omit-empty encoding is on.
func OmitEmpty() bool { return codec.OmitEmpty() }

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
