package wire

// Composites: a nested key run, a list of them, and a map.
//
// Three of the wide format's classes are made of values rather than bytes, and
// they recurse through the same descriptors. None of them borrows anything from
// a columnar layout: a nested struct is another key run, not a sub-table.
//
//	struct   [key][desc][bytelen]                ( [key][desc][payload] )*
//	list     [key][desc][bytelen][count]         ( [desc][payload] )*
//	map      [key][desc][bytelen][count]         ( [desc][payload] [desc][payload] )*
//
// Every one carries a byte length, so every one is skippable without its
// sub-schema — which is what `compact.ErrSkipComposite` exists to say cannot be
// done, and is the reason to spend the length.
//
// # Lengths are written forward and patched
//
// A composite's length is not known until its body is. The writer reserves one
// byte, writes the body, and patches it; a body past 255 bytes makes room for
// four by shifting what follows, which is a memmove on a buffer already in cache
// and only happens for the composites that are large enough not to care.
//
// The alternative — walking the value twice to size it first — costs a pass over
// every nested value on every message, to save a rare copy. This way the common
// composite is one reserved byte and one store.

import "encoding/binary"

// Mark records where a composite's length placeholder sits, so Close can patch
// it. It is a value, so nesting costs no allocation.
type Mark struct{ at int }

// inlineCompositeLength is what the reserved byte holds before the body grows
// past it.
const inlineCompositeLength = 0xFF

// openComposite writes a key, a descriptor and a one-byte length placeholder.
func (w *Writer8) openComposite(key, class, detail uint8) Mark {
	w.Buffer = append(w.Buffer, key, descriptor(class, detail), 0)
	return Mark{at: len(w.Buffer) - 1}
}

// OpenStruct begins a nested key run under key. Close it with Close.
//
//	mark := w.OpenStruct(3)
//	w.U32(0, inner.ID)
//	w.String(1, inner.Name)
//	w.Close(mark)
func (w *Writer8) OpenStruct(key uint8) Mark {
	return w.openComposite(key, classStruct, 0)
}

// structNarrowKeys is the STRUCT descriptor's k8 bit, clear when the key run
// inside uses four-bit keys.
const structWideKeys uint8 = 0b1000

// OpenStructWide is OpenStruct for a nested run that needs eight-bit keys. The
// width is per scope, so a narrow-keyed struct can hold a wide-keyed one and the
// other way round — which is the whole reason the bit is in the descriptor that
// opens the run rather than in a header.
func (w *Writer8) OpenStructWide(key uint8) Mark {
	return w.openComposite(key, classStruct, structWideKeys)
}

// OpenElementStructWide is OpenElementStruct for a wide-keyed element.
func (w *Writer8) OpenElementStructWide() Mark {
	w.Buffer = append(w.Buffer, descriptor(classStruct, structWideKeys), 0)
	return Mark{at: len(w.Buffer) - 1}
}

// OpenList begins a list of count values under key. Each element is written as a
// bare descriptor and payload — OpenElementStruct for a struct, or one of the
// element writers below.
func (w *Writer8) OpenList(key uint8, count int) Mark {
	mark := w.openComposite(key, classList, 0)
	w.Buffer = appendCount(w.Buffer, count)
	return mark
}

// OpenMap begins a map of count entries under key. Each entry is a key value
// then a value value, both written as a bare descriptor and payload.
func (w *Writer8) OpenMap(key uint8, count int) Mark {
	mark := w.openComposite(key, classMap, 0)
	w.Buffer = appendCount(w.Buffer, count)
	return mark
}

// countBytes is what appendCount will occupy.
func countBytes(count int) int {
	if count <= inlineElementSize {
		return 1
	}
	return 5
}

// appendCount writes an element count the same way an element length is written:
// one byte, escaping to four. A composite with more than 254 elements is rare
// enough that the escape costs nothing on average, and the common one is a byte.
func appendCount(buffer []byte, count int) []byte {
	if count <= inlineElementSize {
		return append(buffer, uint8(count))
	}
	return binary.LittleEndian.AppendUint32(append(buffer, elementSizeEscape), uint32(count))
}

// AppendLength writes a length the way this format writes every count and every
// element length: one byte, escaping to four behind 0xFF. There is no varint
// here or anywhere else, so reading one back is a compare and a load.
//
// It is exported for the schema section, which is a byte layout of colbin's own
// outside any field framing and should not invent a second rule for a length —
// see codec/schema.go.
func AppendLength(buffer []byte, value int) []byte { return appendCount(buffer, value) }

// ReadLength reverses AppendLength, returning the value and how many bytes it
// occupied.
func ReadLength(buffer []byte) (value, width int, ok bool) { return readCount(buffer) }

// Close patches a composite's length. Every Open must have exactly one Close,
// and they must nest.
func (w *Writer8) Close(mark Mark) {
	body := len(w.Buffer) - (mark.at + 1)
	if body < inlineCompositeLength {
		w.Buffer[mark.at] = uint8(body)
		return
	}
	w.widenLength(mark, body)
}

// widenLength turns a one-byte length placeholder into four, shifting the body
// up to make room. Out of line because it is the rare path and inlining it would
// cost every composite the code size.
func (w *Writer8) widenLength(mark Mark, body int) {
	w.Buffer = append(w.Buffer, 0, 0, 0)
	copy(w.Buffer[mark.at+4:], w.Buffer[mark.at+1:len(w.Buffer)-3])
	binary.LittleEndian.PutUint32(w.Buffer[mark.at:], uint32(body))
	// lw code 2 is a four-byte length, in the descriptor's low two bits.
	w.Buffer[mark.at-1] |= 2
}

// Element writers: a value with a descriptor but no key, which is what a list
// element and a map key or value are.

// OpenElementStruct begins a struct as a list element or a map value.
func (w *Writer8) OpenElementStruct() Mark {
	w.Buffer = append(w.Buffer, descriptor(classStruct, 0), 0)
	return Mark{at: len(w.Buffer) - 1}
}

// ElementUint writes an integer element.
func (w *Writer8) ElementUint(value uint64) {
	if value <= maxInlineValue {
		w.Buffer = append(w.Buffer, uint8(value))
		return
	}
	code, width := sizeCodeFor(value)
	w.Buffer = appendMagnitude(
		append(w.Buffer, descriptor(classInt, intPositiveFlag|code)), value, width)
}

// ElementInt writes a signed integer element.
func (w *Writer8) ElementInt(value int64) {
	if value >= 0 {
		w.ElementUint(uint64(value))
		return
	}
	magnitude := -uint64(value)
	code, width := sizeCodeFor(magnitude)
	w.Buffer = appendMagnitude(
		append(w.Buffer, descriptor(classInt, code)), magnitude, width)
}

// ElementString writes a string element.
func (w *Writer8) ElementString(value string) {
	code, width := lengthCodeFor(uint64(len(value)))
	w.Buffer = appendMagnitude(
		append(w.Buffer, descriptor(classBlob, code)), uint64(len(value)), width)
	w.Buffer = append(w.Buffer, value...)
}

// Reader side.

// Struct returns a reader over a nested key run and advances past it. The sub
// reader is a value, so descending costs no allocation.
//
// It is only correct for a run the descriptor says is wide-keyed; StructBody is
// what a caller that handles both widths uses.
func (r *Reader8) Struct() (Reader8, bool) {
	body, ok := r.compositeBody(classStruct)
	if !ok {
		return Reader8{}, false
	}
	return Reader8{buffer: body}, true
}

// StructBody returns a nested run's bytes and the key width it uses, advancing
// past the whole field. A caller reads the body with NewReader or NewReader8
// accordingly — which is the per-scope key width, spent where it is decided.
func (r *Reader8) StructBody() (body []byte, wideKeys, ok bool) {
	if r.at+2 > len(r.buffer) {
		r.fail(ErrTruncated)
		return nil, false, false
	}
	wideKeys = r.buffer[r.at+1]&structWideKeys != 0
	body, ok = r.compositeBody(classStruct)
	return body, wideKeys, ok
}

// ElementStructBody is StructBody for a key-less element.
func (r *Reader8) ElementStructBody() (body []byte, wideKeys, ok bool) {
	r.at--
	return r.StructBody()
}

// MoreElements reports whether another key-less value follows. It differs from
// More because a key-less value is one byte rather than two at its shortest.
func (r *Reader8) MoreElements() bool { return r.err == nil && r.at < len(r.buffer) }

// List returns the element count and a reader over a list's elements.
func (r *Reader8) List() (int, Reader8, bool) {
	return r.countedBody(classList)
}

// Map returns the entry count and a reader over a map's entries, which alternate
// key and value.
func (r *Reader8) Map() (int, Reader8, bool) {
	return r.countedBody(classMap)
}

// compositeBody checks the class, reads the length and returns the body as a
// sub-slice, advancing the cursor past the whole field.
func (r *Reader8) compositeBody(want uint8) ([]byte, bool) {
	length, start, ok := r.lengthOf(want)
	if !ok {
		return nil, false
	}
	if length > len(r.buffer)-start {
		r.fail(ErrTruncated)
		return nil, false
	}
	r.at = start + length
	return r.buffer[start : start+length], true
}

// countedBody is compositeBody for the classes whose body begins with a count.
func (r *Reader8) countedBody(want uint8) (int, Reader8, bool) {
	body, ok := r.compositeBody(want)
	if !ok {
		return 0, Reader8{}, false
	}
	count, at, ok := readCount(body)
	if !ok {
		r.fail(ErrTruncated)
		return 0, Reader8{}, false
	}
	// The element reader starts one byte in, so that each read can step the
	// cursor back to find its descriptor — a key-less value is one byte shorter
	// than a keyed one, and backing up is what lets both share Reader8's value
	// handling instead of a third copy of it.
	return count, Reader8{buffer: body[at-1:], at: 1}, true
}

// readCount reverses appendCount.
func readCount(body []byte) (count, at int, ok bool) {
	if len(body) < 1 {
		return 0, 0, false
	}
	if value := body[0]; value != elementSizeEscape {
		return int(value), 1, true
	}
	if len(body) < 5 {
		return 0, 0, false
	}
	value := binary.LittleEndian.Uint32(body[1:])
	if uint64(value) > uint64(maxInt) {
		return 0, 0, false
	}
	return int(value), 5, true
}

// Element readers: a value with a descriptor but no key. They are Reader8's own
// reads with the cursor stepped back one byte, exactly as BitmapReader's are,
// because a key-less value is the same shape as a bitmap run's.

// ElementUint reads an integer element.
func (r *Reader8) ElementUint() uint64 {
	r.at--
	return r.Uint()
}

// ElementInt reads a signed integer element.
func (r *Reader8) ElementInt() int64 {
	r.at--
	return r.Int()
}

// ElementString reads a string element.
func (r *Reader8) ElementString() string {
	r.at--
	return r.String()
}

// ElementStruct returns a reader over a struct element.
func (r *Reader8) ElementStruct() (Reader8, bool) {
	r.at--
	return r.Struct()
}

// IsTable reports whether the field at the cursor is a table rather than a list.
// A writer chooses between them on the row count, so a reader has to ask.
func (r *Reader8) IsTable() bool {
	if r.at+2 > len(r.buffer) {
		return false
	}
	desc := r.buffer[r.at+1]
	return desc >= descExplicit && (desc>>4)&0b111 == classMap && desc&tableFlag != 0
}
