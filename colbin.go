package colbin

import "github.com/ivanjoz/colbin/codec"

// Marshal encodes v into the colbin format. Structs and slices of structs use
// the columnar records layout; other supported values use the single-value
// layout. Pointers are dereferenced.
func Marshal(v any) ([]byte, error) {
	return codec.Marshal(v)
}

// MarshalForceCompact encodes v in compact mode whenever the type permits it,
// instead of letting Marshal pick the mode on size.
//
// Compact mode is a bit-level layout for one record, or an array of at most
// three, where the columnar layout has nothing to amortise its per-column
// framing over. Marshal takes it when it measures smaller, which is the right
// default; this is for a caller who wants the form itself -- one bitstream, no
// version byte, no columns to walk -- for a size-bounded record such as a token
// or a small blob.
//
// v must be a struct or a slice of one to three structs: compact mode frames
// records, so there is no value layout for a top-level map or scalar. It errors
// rather than falling back, naming the field in the way, and the messages it
// writes are ordinary colbin messages that Unmarshal reads.
//
// Compact mode carries nested structs, arrays of structs, maps and nested
// slices. It cannot carry an interface field at any depth: the concrete type is
// a property of the value and the compact wire has no tag for it. A pointer
// field needs SetOmitEmpty(true), and a self-referential type is out.
func MarshalForceCompact(v any) ([]byte, error) {
	return codec.MarshalForceCompact(v)
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

// MarshalMinimal encodes v in **minimal mode**: a byte-aligned `[key][value]`
// layout for a record of at most sixteen primitive fields, where a zero-valued
// field is not written at all.
//
// It is the mode for one small record on a hot path — a wire frame, a token, a
// row key — where the columnar mode's per-record framing is the dominant cost
// and there is nothing to amortise it over. A ten-field record encodes in 62 ns
// here, 18 ns through [MustMinimalCodec], and 6 ns through the minimal.Writer
// underneath, against compact mode's 92 ns for about the same bytes; see
// minimal/README.md for the format, the measurements and the trade.
//
// Two things it asks of the type, both refused loudly rather than worked around:
//
//   - **Every field carries an explicit `cb:"N"` with N ≤ 15.** Four key bits
//     cannot hold a hashed id.
//   - **Scalars, strings, []byte and slices of those.** No nested struct, map,
//     pointer or interface: a field's whole layout has to follow from its key.
//
// A minimal message is not self-describing and carries no mode byte, so
// [Unmarshal] cannot read one and [UnmarshalMinimal] is the only way back.
func MarshalMinimal(v any) ([]byte, error) {
	return codec.MarshalMinimal(v)
}

// AppendMinimal is MarshalMinimal onto a buffer the caller owns, which is what
// makes encoding one message per call allocation-free.
//
//	buf := make([]byte, 0, 64)
//	for _, record := range records {
//	    buf, _ = colbin.AppendMinimal(buf[:0], &record)
//	    send(buf)
//	}
func AppendMinimal(dst []byte, v any) ([]byte, error) {
	return codec.AppendMinimal(dst, v)
}

// UnmarshalMinimal decodes a minimal message into dst, a non-nil pointer to a
// struct of the same shape. Fields the message omitted are zeroed, since that is
// exactly what their absence means.
//
// A key the type does not declare is an error rather than something to skip: the
// header sizes a field but does not say which layout it is, so there is no way to
// know how far to step.
func UnmarshalMinimal(data []byte, dst any) error {
	return codec.UnmarshalMinimal(data, dst)
}

// MinimalFieldIDs reports the wire key of every field of a type minimal mode
// accepts, for handing to a reader in another language. Reading them out of the
// struct tags by hand is how the two sides drift.
func MinimalFieldIDs(v any) (map[string]uint8, error) {
	return codec.MinimalFieldIDs(v)
}

// MinimalCodec is minimal mode through a cached, typed handle: the per-type plan
// is resolved once instead of on every call, which is what takes a ten-field
// record from 62 ns to 20. Use it wherever the type is known at compile time.
//
//	var chargeCodec = colbin.MustMinimalCodec[Charge]()
//
//	buf, _ := chargeCodec.Append(buf[:0], &charge)
type MinimalCodec[T any] = codec.MinimalCodec[T]

// NewMinimalCodec builds the handle for T, which must be a struct minimal mode
// accepts.
func NewMinimalCodec[T any]() (*MinimalCodec[T], error) { return codec.NewMinimalCodec[T]() }

// MustMinimalCodec is NewMinimalCodec for a package-level variable, where a type
// error is a programming error and there is nobody to return it to.
func MustMinimalCodec[T any]() *MinimalCodec[T] { return codec.MustMinimalCodec[T]() }
