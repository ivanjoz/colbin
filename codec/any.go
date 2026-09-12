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

// anySink assembles maps and slices as the walk descends.
type anySink struct {
	root  any
	stack []anyLevel
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

func (s *anySink) beginObject() {
	s.stack = append(s.stack, anyLevel{object: map[string]any{}})
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

func (s *anySink) beginArray() {
	// A non-nil empty slice, so that an array of no elements is [] and not null:
	// the wire distinguishes them and so should this.
	s.stack = append(s.stack, anyLevel{array: []any{}})
}

func (s *anySink) endArray() {
	top := len(s.stack) - 1
	if top < 0 {
		return
	}
	array := s.stack[top].array
	s.stack = s.stack[:top]
	s.place(array)
}

func (s *anySink) key(name string) {
	if top := len(s.stack) - 1; top >= 0 {
		s.stack[top].key = name
	}
}

func (s *anySink) null()                 { s.place(nil) }
func (s *anySink) boolean(value bool)    { s.place(value) }
func (s *anySink) signed(value int64)    { s.place(value) }
func (s *anySink) unsigned(value uint64) { s.place(value) }
func (s *anySink) text(value string)     { s.place(value) }

// textBytes copies, because the result outlives the message buffer. That is the
// allocation the JSON sink avoids and this one cannot: a Go string has to own
// its bytes.
func (s *anySink) textBytes(value []byte) { s.place(string(value)) }
func (s *anySink) float(value float64, _ int) error {
	s.place(value)
	return nil
}

// blob copies. The message buffer is usually one the caller reuses, and a value
// pointing into it would change underneath — which is the same reason readField
// copies a []byte rather than aliasing one.
func (s *anySink) blob(value []byte) {
	s.place(append([]byte(nil), value...))
}
