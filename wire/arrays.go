package wire

// Arrays of floats, of bools and of blobs.
//
// None of them is a new shape on the wire. An array of floats is an integer
// array's framing at the float's own width — a VEC at eight key bits, the
// length form at four — holding each element's IEEE bits, little-endian and in
// full; an array of bools is the same at one byte an element, 0 or 1; an array
// of blobs is the bytes of an array of strings. What says which is the field's
// op, which a reader has from the Go type or from the schema section.
//
// An element is not trimmed the way a scalar float is (float.go): an array has
// no size per element to trim into, and at a fixed width the whole array is one
// copy in each direction, which is the point of it.
//
// A long array of floats is a column instead, when that is smaller: its count
// and then what a table's float column is (column.AppendFloat64, AppendFloat32),
// under COL at eight key bits and under the length form's width 1 at four. A
// reader takes whichever it finds.

import (
	"encoding/binary"
	"math"
	"unsafe"

	"github.com/ivanjoz/colbin/column"
	"github.com/ivanjoz/colbin/internal/wirefmt"
)

// floatColumnFrom is the length from which an array of floats is tried as a
// column. Below it the column's header and the scoring are too large a share:
// on the Public BI float columns a column of 128 is a third smaller than its
// bits, at what a table's float column costs to write.
const floatColumnFrom = 128

// floatColumnValuesPerByte is the most values a float column may hold for each
// byte of it. A column of identical values is a few bytes whatever its length,
// so its count is the one number here the bytes do not bound, and a reader
// allocates for it; a writer keeps an array denser than this as its bits, and a
// reader refuses one. It is the cells a byte of message buys a table
// (codec's rowBudget).
const floatColumnValuesPerByte = 64

// keepsColumn says a float column of size bytes is written for count values
// that are plain bytes at their bits: it is smaller, and not so much smaller
// that a reader would refuse its count.
func keepsColumn(size, count, plain int) bool {
	return size < plain && count <= size*floatColumnValuesPerByte
}

// float64Bits is values as the bit patterns a float column is held as.
func float64Bits(values []float64) []int64 {
	return unsafe.Slice((*int64)(unsafe.Pointer(unsafe.SliceData(values))), len(values))
}

// floatColumnBody splits a float column's body into its count and its column,
// refusing a count the column is too short to be trusted with.
func floatColumnBody(body []byte) (count int, encoded []byte, ok bool) {
	count, at, ok := readCount(body)
	if !ok || count > (len(body)-at)*floatColumnValuesPerByte {
		return 0, nil, false
	}
	return count, body[at:], true
}

// readFloat64Column appends a float column's values to dst.
func readFloat64Column(dst []float64, body []byte) ([]float64, error) {
	count, encoded, ok := floatColumnBody(body)
	if !ok {
		return dst, ErrTruncated
	}
	if count == 0 {
		return dst, nil
	}
	start := len(dst)
	dst = append(dst, make([]float64, count)...)
	used, err := column.DecodeArray(encoded, count, float64Bits(dst[start:]))
	if err == nil && used > len(encoded) {
		err = ErrTruncated
	}
	if err != nil {
		return dst[:start], err
	}
	return dst, nil
}

func readFloat32Column(dst []float32, body []byte) ([]float32, error) {
	count, encoded, ok := floatColumnBody(body)
	if !ok {
		return dst, ErrTruncated
	}
	if count == 0 {
		return dst, nil
	}
	start := len(dst)
	dst = append(dst, make([]float32, count)...)
	used, err := column.DecodeFloat32(encoded, count, dst[start:])
	if err == nil && used > len(encoded) {
		err = ErrTruncated
	}
	if err != nil {
		return dst[:start], err
	}
	return dst, nil
}

// isFloatColumn says the field at the cursor is a COL, which under an array of
// floats' key is its column.
func (r *Reader8) isFloatColumn() bool {
	return r.at+1 < len(r.buffer) && r.buffer[r.at+1]>>4 == 0b1000|classCol &&
		r.buffer[r.at+1]&(nullableDetail|listColumnDetail) == 0
}

// nativeLittleEndian says the platform stores a float as the wire does, so an
// array of them is copied rather than converted.
var nativeLittleEndian = func() bool {
	probe := uint16(1)
	return *(*byte)(unsafe.Pointer(&probe)) == 1
}()

// fixedElement is every element a fixed-width array holds.
type fixedElement interface {
	~float32 | ~float64 | ~bool
}

// elementBytes is values as the bytes they occupy in memory.
func elementBytes[T fixedElement](values []T) []byte {
	var zero T
	return unsafe.Slice((*byte)(unsafe.Pointer(unsafe.SliceData(values))), len(values)*int(unsafe.Sizeof(zero)))
}

// appendFloat64Elements and appendFloat32Elements append the elements' bits.
func appendFloat64Elements(buffer []byte, values []float64) []byte {
	if nativeLittleEndian {
		return append(buffer, elementBytes(values)...)
	}
	for _, value := range values {
		buffer = binary.LittleEndian.AppendUint64(buffer, math.Float64bits(value))
	}
	return buffer
}

func appendFloat32Elements(buffer []byte, values []float32) []byte {
	if nativeLittleEndian {
		return append(buffer, elementBytes(values)...)
	}
	for _, value := range values {
		buffer = binary.LittleEndian.AppendUint32(buffer, math.Float32bits(value))
	}
	return buffer
}

// readFloat64Elements appends the floats elements holds to dst, which the
// caller has checked is a whole number of them.
func readFloat64Elements(dst []float64, elements []byte) []float64 {
	count := len(elements) / 8
	if count == 0 {
		return dst
	}
	start := len(dst)
	dst = append(dst, make([]float64, count)...)
	if nativeLittleEndian {
		copy(elementBytes(dst[start:]), elements)
		return dst
	}
	for index := range count {
		dst[start+index] = math.Float64frombits(binary.LittleEndian.Uint64(elements[index*8:]))
	}
	return dst
}

func readFloat32Elements(dst []float32, elements []byte) []float32 {
	count := len(elements) / 4
	if count == 0 {
		return dst
	}
	start := len(dst)
	dst = append(dst, make([]float32, count)...)
	if nativeLittleEndian {
		copy(elementBytes(dst[start:]), elements)
		return dst
	}
	for index := range count {
		dst[start+index] = math.Float32frombits(binary.LittleEndian.Uint32(elements[index*4:]))
	}
	return dst
}

// validBools reports whether every byte is 0 or 1, which is all a Go bool may
// hold: a message is untrusted, and a bool of 2 is not a value the language
// defines. It ORs eight bytes at a time rather than testing each.
func validBools(elements []byte) bool {
	var seen uint64
	for len(elements) >= 8 {
		seen |= binary.LittleEndian.Uint64(elements)
		elements = elements[8:]
	}
	for _, raw := range elements {
		seen |= uint64(raw)
	}
	return seen&^0x0101_0101_0101_0101 == 0
}

// readBoolElements appends the bools elements holds to dst, or reports that one
// of them is not a bool.
func readBoolElements(dst []bool, elements []byte) ([]bool, bool) {
	if !validBools(elements) {
		return dst, false
	}
	if len(elements) == 0 {
		return dst, true
	}
	start := len(dst)
	dst = append(dst, make([]bool, len(elements))...)
	copy(elementBytes(dst[start:]), elements)
	return dst, true
}

// appendFixedVec opens a VEC of byteLength bytes at the width widthCode names.
// The sign flag is set, as it is for an array of magnitudes: there is nothing
// to extend.
func appendFixedVec(buffer []byte, key, widthCode uint8, byteLength int) []byte {
	lengthCode, lengthBytes := uint8(0), 1
	if byteLength > 0xFF {
		lengthCode, lengthBytes = 1, 4
	}
	return wirefmt.AppendMagnitude(
		append(buffer, key, descriptor(classVec, widthCode<<2|0b10|lengthCode)), uint64(byteLength), lengthBytes)
}

// Float64s writes an array of float64 at eight bytes an element, and nothing
// when the array is empty.
//
//go:noinline
func (w *Writer8) Float64s(key uint8, values []float64) {
	if len(values) == 0 {
		return
	}
	if len(values) >= floatColumnFrom {
		start := len(w.Buffer)
		mark := w.openSized(key, classCol, 0, len(values))
		w.Buffer = appendCount(w.Buffer, len(values))
		body := len(w.Buffer)
		w.Buffer = column.AppendFloat64(w.Buffer, float64Bits(values))
		if keepsColumn(len(w.Buffer)-body, len(values), len(values)*8) {
			w.Close(mark)
			return
		}
		w.Buffer = w.Buffer[:start]
	}
	w.Buffer = appendFloat64Elements(appendFixedVec(w.Buffer, key, widthCode8Bytes, len(values)*8), values)
}

// Float32s is Float64s at four bytes an element.
//
//go:noinline
func (w *Writer8) Float32s(key uint8, values []float32) {
	if len(values) == 0 {
		return
	}
	if len(values) >= floatColumnFrom {
		start := len(w.Buffer)
		mark := w.openSized(key, classCol, 0, len(values))
		w.Buffer = appendCount(w.Buffer, len(values))
		body := len(w.Buffer)
		w.Buffer = column.AppendFloat32(w.Buffer, values)
		if keepsColumn(len(w.Buffer)-body, len(values), len(values)*4) {
			w.Close(mark)
			return
		}
		w.Buffer = w.Buffer[:start]
	}
	w.Buffer = appendFloat32Elements(appendFixedVec(w.Buffer, key, widthCode4Bytes, len(values)*4), values)
}

// Bools writes an array of bools at a byte an element.
//
//go:noinline
func (w *Writer8) Bools(key uint8, values []bool) {
	if len(values) == 0 {
		return
	}
	w.Buffer = append(appendFixedVec(w.Buffer, key, widthCode1Byte, len(values)), elementBytes(values)...)
}

// The narrow writers, in the length form an integer array takes. They are not
// inlined for the reason Ints is not.

//go:noinline
func (w *Writer) Float64s(key uint8, values []float64) {
	if len(values) == 0 {
		return
	}
	if len(values) >= floatColumnFrom {
		// The length form at width 1, which no array of floats is at: a column.
		start := len(w.Buffer)
		mark := w.openNarrowSized(key, widthCode1Byte, len(values))
		w.Buffer = appendCount(w.Buffer, len(values))
		body := len(w.Buffer)
		w.Buffer = column.AppendFloat64(w.Buffer, float64Bits(values))
		if keepsColumn(len(w.Buffer)-body, len(values), len(values)*8) {
			w.Close(mark)
			return
		}
		w.Buffer = w.Buffer[:start]
	}
	w.Buffer = appendFloat64Elements(
		wirefmt.AppendLength(append(w.Buffer, key<<4|nibbleLength|widthCode8Bytes), len(values)*8), values)
}

//go:noinline
func (w *Writer) Float32s(key uint8, values []float32) {
	if len(values) == 0 {
		return
	}
	if len(values) >= floatColumnFrom {
		start := len(w.Buffer)
		mark := w.openNarrowSized(key, widthCode1Byte, len(values))
		w.Buffer = appendCount(w.Buffer, len(values))
		body := len(w.Buffer)
		w.Buffer = column.AppendFloat32(w.Buffer, values)
		if keepsColumn(len(w.Buffer)-body, len(values), len(values)*4) {
			w.Close(mark)
			return
		}
		w.Buffer = w.Buffer[:start]
	}
	w.Buffer = appendFloat32Elements(
		wirefmt.AppendLength(append(w.Buffer, key<<4|nibbleLength|widthCode4Bytes), len(values)*4), values)
}

//go:noinline
func (w *Writer) Bools(key uint8, values []bool) {
	if len(values) == 0 {
		return
	}
	w.Buffer = append(
		wirefmt.AppendLength(append(w.Buffer, key<<4|nibbleLength|widthCode1Byte), len(values)), elementBytes(values)...)
}

// The readers. An array at another width than its type's is refused rather
// than converted: nothing writes one. An array of floats may be a column.

// Float64s appends the array's elements to dst, which may be nil.
func (r *Reader8) Float64s(dst []float64) []float64 {
	if r.isFloatColumn() {
		body, ok := r.compositeBody(classCol)
		if !ok {
			return dst
		}
		dst, err := readFloat64Column(dst, body)
		r.Fail(err)
		return dst
	}
	elements, width, _, ok := r.vecBody()
	if !ok {
		return dst
	}
	if width != 8 {
		r.fail(ErrBadDescriptor)
		return dst
	}
	return readFloat64Elements(dst, elements)
}

func (r *Reader8) Float32s(dst []float32) []float32 {
	if r.isFloatColumn() {
		body, ok := r.compositeBody(classCol)
		if !ok {
			return dst
		}
		dst, err := readFloat32Column(dst, body)
		r.Fail(err)
		return dst
	}
	elements, width, _, ok := r.vecBody()
	if !ok {
		return dst
	}
	if width != 4 {
		r.fail(ErrBadDescriptor)
		return dst
	}
	return readFloat32Elements(dst, elements)
}

func (r *Reader8) Bools(dst []bool) []bool {
	elements, width, _, ok := r.vecBody()
	if !ok {
		return dst
	}
	dst, valid := readBoolElements(dst, elements)
	if width != 1 || !valid {
		r.fail(ErrBadDescriptor)
	}
	return dst
}

func (r *Reader) Float64s(dst []float64) []float64 {
	elements, width, ok := r.arrayElements()
	if !ok {
		return dst
	}
	if width == 1 {
		dst, err := readFloat64Column(dst, elements)
		r.Fail(err)
		return dst
	}
	if width != 8 {
		r.fail(ErrBadEscape)
		return dst
	}
	return readFloat64Elements(dst, elements)
}

func (r *Reader) Float32s(dst []float32) []float32 {
	elements, width, ok := r.arrayElements()
	if !ok {
		return dst
	}
	if width == 1 {
		dst, err := readFloat32Column(dst, elements)
		r.Fail(err)
		return dst
	}
	if width != 4 {
		r.fail(ErrBadEscape)
		return dst
	}
	return readFloat32Elements(dst, elements)
}

func (r *Reader) Bools(dst []bool) []bool {
	elements, width, ok := r.arrayElements()
	if !ok {
		return dst
	}
	dst, valid := readBoolElements(dst, elements)
	if width != 1 || !valid {
		r.fail(ErrBadEscape)
	}
	return dst
}

// Arrays of blobs: an array of strings' bytes, element for element, from a
// slice whose elements are slices.

// blobsPayload is what the elements take, each behind its size.
func blobsPayload(values [][]byte) (payload int) {
	for _, value := range values {
		payload += StringPayload(len(value))
	}
	return payload
}

// appendBlobElements is appendStringElementsLoop over blobs.
func appendBlobElements(buffer []byte, values [][]byte) []byte {
	for _, value := range values {
		if len(value) <= inlineElementSize {
			buffer = append(buffer, uint8(len(value)))
		} else {
			buffer = binary.LittleEndian.AppendUint32(
				append(buffer, elementSizeEscape), uint32(len(value)))
		}
		buffer = append(buffer, value...)
	}
	return buffer
}

// BytesList writes an array of blobs as Strings writes an array of strings.
//
//go:noinline
func (w *Writer8) BytesList(key uint8, values [][]byte) {
	if len(values) == 0 {
		return
	}
	counted := blobsPayload(values) + countBytes(len(values))
	code, width := lengthCodeFor(uint64(counted))
	w.Buffer = append(w.Buffer, key, descriptor(classList, listHomogeneous)|code)
	w.Buffer = wirefmt.AppendMagnitude(w.Buffer, uint64(counted), width)
	w.Buffer = appendBlobElements(appendCount(w.Buffer, len(values)), values)
}

//go:noinline
func (w *Writer) BytesList(key uint8, values [][]byte) {
	if len(values) == 0 {
		return
	}
	if len(values) == 1 {
		w.Buffer = wirefmt.AppendLength(append(w.Buffer, key<<4|nibbleLength|narrowSingle), len(values[0]))
		w.Buffer = append(w.Buffer, values[0]...)
		return
	}
	w.Buffer = wirefmt.AppendLength(append(w.Buffer, key<<4|nibbleLength), blobsPayload(values))
	w.Buffer = appendBlobElements(w.Buffer, values)
}

// ownBlobs replaces the elements appended to dst from index first on, which are
// sub-slices of the message, with copies of them: a message is usually a read
// buffer the caller reuses. The copies share one array, cut so that appending
// to one cannot reach the next.
func ownBlobs(dst [][]byte, first int) [][]byte {
	total := 0
	for _, element := range dst[first:] {
		total += len(element)
	}
	owned := make([]byte, total)
	at := 0
	for index := first; index < len(dst); index++ {
		size := copy(owned[at:], dst[index])
		dst[index] = owned[at : at+size : at+size]
		at += size
	}
	return dst
}

// BytesList appends a copy of each element to dst, which may be nil.
func (r *Reader8) BytesList(dst [][]byte) [][]byte {
	first := len(dst)
	dst = r.StringsBytes(dst)
	if r.err != nil {
		return dst[:first]
	}
	return ownBlobs(dst, first)
}

func (r *Reader) BytesList(dst [][]byte) [][]byte {
	first := len(dst)
	dst = r.StringsBytes(dst)
	if r.err != nil {
		return dst[:first]
	}
	return ownBlobs(dst, first)
}

// ElementBlob returns a string element's bytes as a sub-slice of the message,
// which is what a blob element is: ElementString without the copy.
func (r *Reader) ElementBlob() []byte {
	if r.at >= len(r.buffer) {
		r.fail(ErrTruncated)
		return nil
	}
	width := lengthWidth[r.buffer[r.at]&0b11]
	rest := r.buffer[r.at+1:]
	if len(rest) < width {
		r.fail(ErrTruncated)
		return nil
	}
	size := leUint(rest, width)
	start := r.at + 1 + width
	if size > uint64(maxInt) || int(size) > len(r.buffer)-start {
		r.fail(ErrTruncated)
		return nil
	}
	r.at = start + int(size)
	return r.buffer[start : start+int(size)]
}
