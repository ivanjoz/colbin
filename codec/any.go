package codec

// The sink that builds Go values instead of text.
//
// It is the same walk as the JSON one — that is the point of there being a sink
// at all — and it is the slower half. Every object is a map allocation and every
// array is a slice of interfaces, which is where all the cost of this decode
// path is. A caller whose answer is going out as text should use AppendJSON and
// never build the map at all.
//
// # What the shapes are
//
//	struct, map    map[string]any
//	array, list    []any
//	integer        int64 or uint64, by the field's signedness
//	float          float64, at either width
//	string         string
//	[]byte         []byte, copied out of the message
//	bool           bool
//	absent slice   nil
//
// An unsigned field is a uint64 and not an int64, because a uint64 past 2^63 is
// a value the format carries and int64 is not where it fits. A float32 is a
// float64 because `any` holding a float32 is a shape nothing downstream expects.
//
// Unlike the JSON sink this one is faithful to a NaN or an infinity: it has
// somewhere to put them.

import (
	"unsafe"

	"github.com/ivanjoz/colbin/wire"
)

// anySink assembles maps and slices as the walk descends.
type anySink struct {
	root  any
	stack []anyLevel
	// strings is where the keys and strings are copied, a chunk at a time
	// rather than one allocation each: a document's are most of what it holds.
	strings *wire.Arena
	boxes   anyBoxes
}

// anyBoxes is where the values the sink puts in an `any` are kept. An any
// holding a float64, a string or a slice points at a copy of it, which the
// conversion allocates a value at a time: a document's numbers were a quarter
// of its decode's allocations. These are the copies a chunk at a time, at the
// price wire.Arena's chunks have: a value kept alive keeps its chunk.
type anyBoxes struct {
	floats  []float64
	ints    []int64
	strings []string
	arrays  [][]any
}

// The type words boxIn gives the values it boxes.
var (
	floatKind  any = float64(0)
	intKind    any = int64(0)
	stringKind any = ""
	arrayKind  any = []any(nil)
)

// boxIn is value as an any, for kind an any holding the zero of T: the same
// any the conversion makes, with the copy it points at made in chunk.
func boxIn[T any](chunk *[]T, value T, kind any) any {
	if len(*chunk) == cap(*chunk) {
		*chunk = make([]T, 0, max(8, min(2*cap(*chunk), 128)))
	}
	*chunk = append(*chunk, value)
	out := kind
	(*[2]unsafe.Pointer)(unsafe.Pointer(&out))[1] = unsafe.Pointer(&(*chunk)[len(*chunk)-1])
	return out
}

// anyLevel is one open container. Exactly one of object and array is in use; the
// key is the object's pending one.
type anyLevel struct {
	object map[string]any
	array  []any
	key    string
}

// place puts a finished value where the current container wants it, or makes it
// the result when there is no container left.
func (s *anySink) place(value any) {
	top := len(s.stack) - 1
	if top < 0 {
		s.root = value
		return
	}
	if s.stack[top].object != nil {
		s.stack[top].object[s.stack[top].key] = value
		return
	}
	s.stack[top].array = append(s.stack[top].array, value)
}

// beginObject makes the map at the size the message declares, which wire has
// bounded by the bytes that hold the entries: grown from empty instead, a map
// was rehashed on its way to that size, a fifth of a document's decode.
func (s *anySink) beginObject(size int) {
	s.stack = append(s.stack, anyLevel{object: make(map[string]any, size)})
}

func (s *anySink) endObject() {
	top := len(s.stack) - 1
	if top < 0 {
		return
	}
	object := s.stack[top].object
	s.stack = s.stack[:top]
	s.place(object)
}

func (s *anySink) beginArray(size int) {
	// A non-nil empty slice, so that an array of no elements is [] and not null:
	// the wire distinguishes them and so should this.
	s.stack = append(s.stack, anyLevel{array: make([]any, 0, size)})
}

func (s *anySink) endArray() {
	top := len(s.stack) - 1
	if top < 0 {
		return
	}
	array := s.stack[top].array
	s.stack = s.stack[:top]
	s.place(boxIn(&s.boxes.arrays, array, arrayKind))
}

func (s *anySink) key(name string) {
	if top := len(s.stack) - 1; top >= 0 {
		s.stack[top].key = name
	}
}

// keyBytes copies, for the reason textBytes does: a Go map key has to own its
// bytes and the message buffer is one the caller reuses.
func (s *anySink) keyBytes(name []byte) { s.key(s.copied(name)) }

// copied is a string of its own for bytes of the message: in the arena when
// there is one.
func (s *anySink) copied(value []byte) string {
	if s.strings == nil {
		return string(value)
	}
	return s.strings.String(value)
}

func (s *anySink) null()                 { s.place(nil) }
func (s *anySink) boolean(value bool)    { s.place(value) }
func (s *anySink) unsigned(value uint64) { s.place(value) }
func (s *anySink) text(value string)     { s.place(value) }

// signed boxes a value past 255 itself: one below is an any the runtime makes
// without an allocation.
func (s *anySink) signed(value int64) {
	if uint64(value) < 256 {
		s.place(value)
		return
	}
	s.place(boxIn(&s.boxes.ints, value, intKind))
}

// textBytes copies, because the result outlives the message buffer. That is the
// allocation the JSON sink avoids and this one cannot: a Go string has to own
// its bytes.
func (s *anySink) textBytes(value []byte) {
	s.place(boxIn(&s.boxes.strings, s.copied(value), stringKind))
}

func (s *anySink) float(value float64, _ int) error {
	s.place(boxIn(&s.boxes.floats, value, floatKind))
	return nil
}

// blob copies. The message buffer is usually one the caller reuses, and a value
// pointing into it would change underneath — which is the same reason readField
// copies a []byte rather than aliasing one.
func (s *anySink) blob(value []byte) {
	s.place(append([]byte(nil), value...))
}
