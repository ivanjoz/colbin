package codec

import (
	"fmt"
	"reflect"
	"sync"
	"unsafe"

	"github.com/ivanjoz/colbin/compact"
)

// Codec[T] is a per-type handle: the struct cache with a typed door on it.
//
// Marshal and Unmarshal work out the same facts on every call -- what type this
// is, what its fields are, where they sit, which mode carries them -- and for a
// message holding one record that lookup is a real share of the cost. A Codec
// resolves all of it once, at construction, and holds it:
//
//	var statsCodec = colbin.MustCodec[SaleOrderProductStats]()
//
//	buf := make([]byte, 0, 64)
//	for _, rec := range records {
//	    buf, _ = statsCodec.Append(buf[:0], &rec)
//	    send(buf)
//	}
//
// The value arrives as *T rather than as any, which is the other half: an
// interface would box it, and a non-addressable struct behind one has to be
// copied before its fields can be addressed at all. A pointer needs neither, so
// the encode path touches no reflection and, with a buffer handed back each
// time, allocates nothing.
//
// A Codec is safe for concurrent use: it is read-only after construction, and
// the writers and readers it borrows come from pools.
//
// Codecs write plain binary mode. MarshalJSON's schema section is a per-type
// cost that a handle would not change, so it stays on the package functions.
type Codec[T any] struct {
	rtype reflect.Type
	ti    *typeInfo
	pl    *compactPlan
}

// NewCodec builds the handle for T, which must be a struct: a Codec encodes
// records, one T per record. Every other supported shape -- maps, scalars,
// slices of pointers -- goes through the package-level Marshal.
//
// The type is described once here, so a Codec built at package scope moves the
// whole cost of understanding T out of the request path.
func NewCodec[T any]() (*Codec[T], error) {
	t := reflect.TypeFor[T]()
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("colbin: NewCodec needs a struct type, got %s", t.Kind())
	}
	ti, err := getTypeInfo(t)
	if err != nil {
		return nil, err
	}
	return &Codec[T]{rtype: t, ti: ti, pl: compactPlanFor(ti)}, nil
}

// MustCodec is NewCodec for a package-level variable, where a type error is a
// programming error and there is nobody to return it to.
func MustCodec[T any]() *Codec[T] {
	c, err := NewCodec[T]()
	if err != nil {
		panic(err)
	}
	return c
}

// Compact reports whether T can use compact mode, and Keys the key width its
// field ids allow. Both are fixed by the type, so a caller can log or assert on
// them once rather than inspecting messages.
func (c *Codec[T]) Compact() bool          { return c.pl.usable }
func (c *Codec[T]) Keys() compact.KeyWidth { return c.pl.keys }

// Marshal encodes one record. It is Append onto a fresh buffer; a caller
// encoding many should use Append and reuse one.
func (c *Codec[T]) Marshal(v *T) ([]byte, error) { return c.Append(nil, v) }

// Append encodes one record onto dst and returns the extended buffer, so a loop
// over many records can pass buf[:0] and allocate nothing at all.
//
// One record always takes compact mode when the type allows it -- the columnar
// header and per-column type bytes have nothing to amortise over -- so the mode
// is decided by the type, not by the value, and this path never builds both.
func (c *Codec[T]) Append(dst []byte, v *T) ([]byte, error) {
	if v == nil {
		return nil, fmt.Errorf("colbin: Codec.Append needs a non-nil *%s", c.rtype)
	}
	p := unsafe.Pointer(v)
	if c.pl.usable {
		return appendCompactTo(dst, c.ti, unsafe.Slice(&p, 1), compact.ShapeStruct), nil
	}
	return appendRecords(dst, binaryPrefix(), c.ti, unsafe.Slice(&p, 1), false, false), nil
}

// AppendSlice encodes vs as a message of len(vs) records, the same layout
// Marshal([]T) produces: compact mode at one to three records when it wins,
// columnar past that.
func (c *Codec[T]) AppendSlice(dst []byte, vs []T) ([]byte, error) {
	return appendRecords(dst, binaryPrefix(), c.ti, c.pointers(vs), true, true), nil
}

// MarshalSlice encodes vs onto a fresh buffer.
func (c *Codec[T]) MarshalSlice(vs []T) ([]byte, error) { return c.AppendSlice(nil, vs) }

// pointers is one pointer per record. The slice's own backing array already has
// the records laid out end to end, so this is arithmetic rather than copying.
func (c *Codec[T]) pointers(vs []T) []unsafe.Pointer {
	if len(vs) == 0 {
		return nil
	}
	ptrs := make([]unsafe.Pointer, len(vs))
	base, size := unsafe.Pointer(&vs[0]), c.ti.size
	for i := range ptrs {
		ptrs[i] = unsafe.Add(base, uintptr(i)*size)
	}
	return ptrs
}

// Unmarshal decodes a one-record message into v, accepting either mode: bit 0 of
// byte 0 discriminates, exactly as the package-level Unmarshal does.
//
// v is overwritten rather than merged into. Compact mode omits a zero-valued
// field entirely, so anything already in v would otherwise survive as a stale
// value under a field the message does not carry.
func (c *Codec[T]) Unmarshal(data []byte, v *T) error {
	if v == nil {
		return fmt.Errorf("colbin: Codec.Unmarshal needs a non-nil *%s", c.rtype)
	}
	if !compact.IsCompact(data) || !c.pl.usable {
		return decodeInto(data, reflect.NewAt(c.rtype, unsafe.Pointer(v)).Elem())
	}
	r := compactReaderPool.Get().(*compact.Reader)
	defer releaseCompactReader(r)
	if err := r.Reset(data); err != nil {
		return err
	}
	if n := r.Records(); n != 1 {
		return fmt.Errorf("colbin: compact message has %d records, cannot decode into a single %s", n, c.rtype)
	}
	var zero T
	*v = zero
	if err := compactReadRecord(r, c.pl, unsafe.Pointer(v)); err != nil {
		return err
	}
	return r.Err()
}

// UnmarshalSlice decodes a message of any record count into dst, replacing
// whatever it held.
func (c *Codec[T]) UnmarshalSlice(data []byte, dst *[]T) error {
	if dst == nil {
		return fmt.Errorf("colbin: Codec.UnmarshalSlice needs a non-nil *[]%s", c.rtype)
	}
	if !compact.IsCompact(data) || !c.pl.usable {
		return decodeInto(data, reflect.ValueOf(dst).Elem())
	}
	r := compactReaderPool.Get().(*compact.Reader)
	defer releaseCompactReader(r)
	if err := r.Reset(data); err != nil {
		return err
	}
	out := make([]T, r.Records())
	for i := range out {
		if err := compactReadRecord(r, c.pl, unsafe.Pointer(&out[i])); err != nil {
			return err
		}
	}
	if err := r.Err(); err != nil {
		return err
	}
	*dst = out
	return nil
}

var (
	compactReaderPool = sync.Pool{New: func() any { return new(compact.Reader) }}
)

// releaseCompactReader drops the message before the reader goes back: a pooled
// object must not keep the buffer it just read alive.
func releaseCompactReader(r *compact.Reader) {
	_ = r.Reset(nil)
	compactReaderPool.Put(r)
}
