package wire

// The dictionary column: a table's string column carried as ids into a
// dictionary the message holds once, ahead of its body.
//
//	wide    [key] [0xB0 | lw] [length] [dict] [column]
//	narrow  [key | 1101]      [length] [dict] [column]
//
// `dict` is a count (one byte up to 254, 0xFF and a u32) naming which of the
// message's dictionaries the ids index, and `column` is the blocked column codec
// over the ids, one per row — so a column of a dozen distinct values costs four
// bits a row rather than the strings.
//
// Neither form was a string column before. A wide string column is a LIST, so a
// COL under a string field's key is unambiguous; a narrow one is the length form
// with clear flags, so `1101` is free. Both are the length form at their width,
// so a reader that does not know the key steps over one like any other field.
//
// What the ids mean is the codec's to resolve: wire only frames them. And
// nothing here is on the path of an ordinary string column — a reader asks
// IsDictColumn only when its message declared dictionaries at the root.

import "github.com/ivanjoz/colbin/column"

// narrowDictColumn is a narrow string column's flags when it holds ids.
const narrowDictColumn uint8 = 0b01

// DictColumn writes a column of dictionary ids under key. Unlike Column it is
// written when every id is zero: the column is a string column, and its absence
// would mean empty strings, not the dictionary's first entry.
func (w *Writer8) DictColumn(key uint8, dict int, ids []int64) {
	mark := w.openSized(key, classCol, 0, len(ids))
	w.Buffer = column.AppendArray(appendCount(w.Buffer, dict), ids)
	w.Close(mark)
}

// DictColumn is Writer8.DictColumn with four key bits.
func (w *Writer) DictColumn(key uint8, dict int, ids []int64) {
	mark := w.openNarrowSized(key, narrowDictColumn, len(ids))
	w.Buffer = column.AppendArray(appendCount(w.Buffer, dict), ids)
	w.Close(mark)
}

// IsDictColumn reports whether the field at the cursor is a COL, which under a
// string column's key is a dictionary column.
func (r *Reader8) IsDictColumn() bool {
	return r.at+1 < len(r.buffer) && r.buffer[r.at+1]>>4 == 0b1000|classCol &&
		r.buffer[r.at+1]&nullableDetail == 0
}

// IsDictColumn reports whether the field at the cursor is the length form with
// the dictionary flag.
func (r *Reader) IsDictColumn() bool {
	return r.at < len(r.buffer) && r.buffer[r.at]&0b1111 == nibbleLength|narrowDictColumn
}

// DictColumn reads a column written by DictColumn: which dictionary, and rows
// ids decoded into dst's capacity.
func (r *Reader8) DictColumn(rows int, dst []int64) (int, []int64, bool) {
	body, ok := r.compositeBody(classCol)
	if !ok {
		return 0, nil, false
	}
	return r.dictIDs(r.buffer[r.at-len(body):], len(body), rows, dst)
}

// DictColumn is Reader8.DictColumn with four key bits.
func (r *Reader) DictColumn(rows int, dst []int64) (int, []int64, bool) {
	body, flags, ok := r.compositeBody()
	if !ok {
		return 0, nil, false
	}
	if flags != narrowDictColumn {
		r.fail(ErrBadEscape)
		return 0, nil, false
	}
	dict, ids, err := dictIDs(r.buffer[r.at-len(body):], len(body), rows, dst)
	if err != nil {
		r.fail(err)
		return 0, nil, false
	}
	return dict, ids, true
}

func (r *Reader8) dictIDs(rest []byte, size, rows int, dst []int64) (int, []int64, bool) {
	dict, ids, err := dictIDs(rest, size, rows, dst)
	if err != nil {
		r.fail(err)
		return 0, nil, false
	}
	return dict, ids, true
}

// dictIDs splits a dictionary column's body, the first size bytes of rest, into
// its dictionary number and its ids. rest runs on to the message's end, for the
// reason decodeColumn is handed it.
func dictIDs(rest []byte, size, rows int, dst []int64) (int, []int64, error) {
	dict, at, ok := readCount(rest[:size])
	if !ok {
		return 0, nil, ErrTruncated
	}
	ids, err := decodeColumn(rest[at:], size-at, rows, dst)
	if err != nil {
		return 0, nil, err
	}
	return dict, ids, nil
}

// The nullable dictionary column: the dictionary column of a pointer field in
// which some rows are nil, laid out as every nullable column is (nullable.go),
// with the dictionary's number in front.
//
//	wide    [key] [0xB4 | lw] [length] [dict] [presence] [column]
//	narrow  [key | 1111]      [length] [dict] [presence] [column]
//
// `column` holds an id for each row present only. Under a string field's key a
// wide COL with the nullable bit is free, as a COL is; a narrow string column's
// flags are 00 for strings, 01 ids, 10 nullable strings, and this is 11 — which
// under a slice's key is a grouped table, so it is read by its key's type, never
// by its flags alone.

// narrowNullableDict is a narrow string column's flags when it holds the ids of
// the rows present.
const narrowNullableDict uint8 = 0b11

// NullableDictColumn writes the dictionary column of a pointer field in which
// some rows are nil: their presence, then the ids of the rows present.
func (w *Writer8) NullableDictColumn(key uint8, dict int, presence []byte, ids []int64) {
	mark := w.openSized(key, classCol, nullableDetail, len(presence)+len(ids))
	w.Buffer = column.AppendArray(append(appendCount(w.Buffer, dict), presence...), ids)
	w.Close(mark)
}

// NullableDictColumn is Writer8.NullableDictColumn with four key bits.
func (w *Writer) NullableDictColumn(key uint8, dict int, presence []byte, ids []int64) {
	mark := w.openNarrowSized(key, narrowNullableDict, len(presence)+len(ids))
	w.Buffer = column.AppendArray(append(appendCount(w.Buffer, dict), presence...), ids)
	w.Close(mark)
}

// IsNullableDictColumn reports whether the field at the cursor is a COL with
// the nullable bit, which under a string column's key is a nullable dictionary
// column.
func (r *Reader8) IsNullableDictColumn() bool {
	return r.at+1 < len(r.buffer) && r.buffer[r.at+1]>>4 == 0b1000|classCol &&
		r.buffer[r.at+1]&nullableDetail != 0
}

// IsNullableDictColumn reports whether the field at the cursor is the length
// form with the nullable dictionary flags.
func (r *Reader) IsNullableDictColumn() bool {
	return r.at < len(r.buffer) && r.buffer[r.at]&0b1111 == nibbleLength|narrowNullableDict
}

// NullableDictColumn reads a column written by NullableDictColumn, of a table
// of rows rows: which dictionary, the presence, a sub-slice of the message, and
// the ids of the rows present, decoded into dst's capacity.
func (r *Reader8) NullableDictColumn(rows int, dst []int64) (int, []byte, []int64, bool) {
	body, ok := r.compositeBody(classCol)
	if !ok {
		return 0, nil, nil, false
	}
	return r.nullableDict(r.buffer[r.at-len(body):], len(body), rows, dst)
}

// NullableDictColumn is Reader8.NullableDictColumn with four key bits.
func (r *Reader) NullableDictColumn(rows int, dst []int64) (int, []byte, []int64, bool) {
	nibble, start, size, ok := r.field()
	if !ok {
		return 0, nil, nil, false
	}
	if nibble != nibbleLength|narrowNullableDict {
		r.fail(ErrBadEscape)
		return 0, nil, nil, false
	}
	dict, presence, ids, err := nullableDictIDs(r.buffer[start:], size, rows, dst)
	if err != nil {
		r.fail(err)
		return 0, nil, nil, false
	}
	return dict, presence, ids, true
}

func (r *Reader8) nullableDict(rest []byte, size, rows int, dst []int64) (int, []byte, []int64, bool) {
	dict, presence, ids, err := nullableDictIDs(rest, size, rows, dst)
	if err != nil {
		r.fail(err)
		return 0, nil, nil, false
	}
	return dict, presence, ids, true
}

// nullableDictIDs splits a nullable dictionary column's body, the first size
// bytes of rest, into its dictionary number, its presence and its ids.
func nullableDictIDs(rest []byte, size, rows int, dst []int64) (int, []byte, []int64, error) {
	dict, at, ok := readCount(rest[:size])
	if !ok {
		return 0, nil, nil, ErrTruncated
	}
	presence, ids, err := nullableColumn(rest[at:], size-at, rows, dst)
	if err != nil {
		return 0, nil, nil, err
	}
	return dict, presence, ids, nil
}
