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

// Compact reports whether compact mode can carry T at all, Composite whether T
// holds a nested struct, a map or an array of structs, and Keys the key width
// its field ids allow. All three are fixed by the type, so a caller can log or
// assert on them once rather than inspecting messages.
//
// The pair is what says which form Append writes: compact for a type that is
// Compact and not Composite, and columnar otherwise -- see Append for why.
func (c *Codec[T]) Compact() bool          { return c.pl.usable }
func (c *Codec[T]) Composite() bool        { return c.pl.hasComposite }
func (c *Codec[T]) Keys() compact.KeyWidth { return c.pl.keys }

// Marshal encodes one record. It is Append onto a fresh buffer; a caller
// encoding many should use Append and reuse one.
func (c *Codec[T]) Marshal(v *T) ([]byte, error) { return c.Append(nil, v) }

// Append encodes one record onto dst and returns the extended buffer, so a loop
// over many records can pass buf[:0] and allocate nothing at all.
//
// One record of a flat struct always takes compact mode -- the columnar header
// and per-column type bytes have nothing to amortise over -- so the mode is
// decided by the type, not by the value, and this path never builds both.
//
// A type with a nested struct, a map or an array of structs keeps the columnar
// form here. Compact mode can carry those, but whether it is smaller depends on
// how much sub-record data the value holds, and answering that means encoding
// twice -- which is the one thing a Codec exists not to do. Marshal, which does
// build both, is the entry point for a composite type that wants the smaller of
// the two; MarshalForceCompact is the one that wants compact regardless.
func (c *Codec[T]) Append(dst []byte, v *T) ([]byte, error) {
	if v == nil {
		return nil, fmt.Errorf("colbin: Codec.Append needs a non-nil *%s", c.rtype)
	}
	p := unsafe.Pointer(v)
	if c.pl.usable && !c.pl.hasComposite {
		return appendCompactTo(dst, c.ti, unsafe.Slice(&p, 1), compact.ShapeStruct), nil
	}
	// A composite type goes through the ordinary record path, which builds both
	// forms and keeps the smaller -- so a Codec writes byte for byte what Marshal
	// writes for the same value, which is the property the flat fast path above
	// gets for free.
	return appendRecords(dst, binaryPrefix(), c.ti, unsafe.Slice(&p, 1), false, true), nil
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
	if !compact.IsCompact(data) {
		// Columnar. The handle already holds T's layout, so this goes straight to
		// the sub-table rather than back through reflect and the type cache.
		var zero T
		*v = zero
		return c.decodeColumnar(data, 1, unsafe.Pointer(v))
	}
	if !c.pl.usable {
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

// reuseRecords is the n-record slice to decode into, reusing dst's backing array
// when it is already large enough -- so a loop that hoists its destination
// allocates for the records once rather than per message. The reused elements
// are zeroed for the reason reuseSlice gives: an omitted field, in either mode,
// is simply not written, and the previous message's value would otherwise
// survive underneath it.
func reuseRecords[T any](dst []T, n int) []T {
	if cap(dst) < n {
		return make([]T, n)
	}
	out := dst[:n:n]
	clear(out)
	return out
}

// UnmarshalSlice decodes a message of any record count into dst, replacing
// whatever it held.
//
// dst's backing array is reused when it has the capacity, so the slice handed in
// must not be one the caller still needs: a decode overwrites its elements.
// Passing a fresh or nil slice opts out.
func (c *Codec[T]) UnmarshalSlice(data []byte, dst *[]T) error {
	if dst == nil {
		return fmt.Errorf("colbin: Codec.UnmarshalSlice needs a non-nil *[]%s", c.rtype)
	}
	if !compact.IsCompact(data) {
		// Columnar, which is every message past three records. This used to fall
		// back to the reflect path and re-resolve a type the handle was built to
		// remember; now it reads the count itself and decodes into a []T.
		dec := &decoder{data: data}
		if err := dec.header(); err != nil {
			return err
		}
		n, err := dec.recordCount()
		if err != nil {
			return err
		}
		out := reuseRecords(*dst, n)
		if n > 0 {
			ptrs := spreadPtrs(unsafe.Pointer(&out[0]), n, c.ti.size)
			err = dec.decodeSubTable(c.ti, n, *ptrs)
			putPtrs(ptrs)
			if err != nil {
				return err
			}
		}
		*dst = out
		return nil
	}
	if !c.pl.usable {
		return decodeInto(data, reflect.ValueOf(dst).Elem())
	}
	r := compactReaderPool.Get().(*compact.Reader)
	defer releaseCompactReader(r)
	if err := r.Reset(data); err != nil {
		return err
	}
	out := reuseRecords(*dst, r.Records())
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

// decodeColumnar decodes a columnar records message of exactly n records into
// records laid out contiguously from base, using the layout the handle already
// holds.
func (c *Codec[T]) decodeColumnar(data []byte, want int, base unsafe.Pointer) error {
	dec := &decoder{data: data}
	if err := dec.header(); err != nil {
		return err
	}
	n, err := dec.recordCount()
	if err != nil {
		return err
	}
	if n != want {
		return fmt.Errorf("colbin: message has %d records, cannot decode into a single %s", n, c.rtype)
	}
	ptrs := spreadPtrs(base, n, c.ti.size)
	err = dec.decodeSubTable(c.ti, n, *ptrs)
	putPtrs(ptrs)
	return err
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
