package wire

// Grouped tables: a long table cut into groups of rows.
//
//	narrow  [key:4][1 1 1 1]  [length] [rows] [groupRows] group*
//	wide    [key][0xEC | lw]  [length] [rows] [groupRows] group*
//	group   [byteLength:u32]  column run
//
// A group is the column run a table of its rows would hold, keyed at the same
// width, so a reader decodes each one as it would a whole table. Every group
// holds groupRows rows but the last, which holds the rest; there are exactly as
// many as that takes. An absent column is zeros for the group's rows.
//
// The group's length is a fixed four bytes rather than a length form, so the
// writer patches it in place: a group is far past what a one-byte length holds,
// and a backpatch that widens would move it.
//
// What it is for is the writer's memory and cache: a table is transposed a
// group at a time, so its column buffer is a group's, not the table's. A
// reader of a dynamic value (dynamic.go) never meets one — only a table field
// is grouped — and refuses one there.

import (
	"encoding/binary"
	"errors"
)

const (
	// narrowGrouped is a narrow table's f1, set when its rows are in groups.
	narrowGrouped uint8 = 0b10
	// groupedFlag is a wide table's MAP detail bit 2, set when its rows are in
	// groups.
	groupedFlag uint8 = 0b0100
	// groupHeader is the bytes in front of a group's column run.
	groupHeader = 4
)

// ErrBadGroup is a grouped table whose groups do not add up to its rows: no
// rows per group, a group that runs past the table, or groups left over.
var ErrBadGroup = errors.New("wire: a grouped table's groups do not match its rows")

// OpenGroupedTable begins a table of rows rows in groups of groupRows. Write
// each group between OpenGroup and CloseGroup, then Close.
func (w *Writer) OpenGroupedTable(key uint8, rows, groupRows int) Mark {
	mark := w.openNarrowSized(key, narrowTable|narrowGrouped, rows)
	w.Buffer = appendCount(appendCount(w.Buffer, rows), groupRows)
	return mark
}

// OpenGroupedTable begins a table of rows rows in groups of groupRows. Write
// each group between OpenGroup and CloseGroup, then Close.
func (w *Writer8) OpenGroupedTable(key uint8, rows, groupRows int) Mark {
	mark := w.openSized(key, classMap, tableFlag|groupedFlag, rows)
	w.Buffer = appendCount(appendCount(w.Buffer, rows), groupRows)
	return mark
}

// OpenGroup begins a group of a grouped table on buffer, at either key width,
// and returns where CloseGroup patches its length.
func OpenGroup(buffer []byte) ([]byte, int) {
	return append(buffer, 0, 0, 0, 0), len(buffer)
}

// CloseGroup patches the length of the group OpenGroup began at at.
func CloseGroup(buffer []byte, at int) {
	body := len(buffer) - at - groupHeader
	if uint64(body) > 1<<32-1 {
		panic(errFieldTooLarge)
	}
	binary.LittleEndian.PutUint32(buffer[at:], uint32(body))
}

// tableHead reads a table body's row count and, for a grouped one, its rows per
// group, returning what follows: the column run, or the groups.
func tableHead(body []byte, grouped bool) (rows, groupRows int, rest []byte, err error) {
	rows, at, ok := readCount(body)
	if !ok {
		return 0, 0, nil, ErrTruncated
	}
	if !grouped {
		return rows, 0, body[at:], nil
	}
	groupRows, used, ok := readCount(body[at:])
	if !ok {
		return 0, 0, nil, ErrTruncated
	}
	if groupRows == 0 {
		return 0, 0, nil, ErrBadGroup
	}
	return rows, groupRows, body[at+used:], nil
}

// NextGroup splits a grouped table's groups into the first one's column run
// and the groups after it.
func NextGroup(groups []byte) (columns, rest []byte, err error) {
	if len(groups) < groupHeader {
		return nil, nil, ErrBadGroup
	}
	size := binary.LittleEndian.Uint32(groups)
	if uint64(size) > uint64(len(groups)-groupHeader) {
		return nil, nil, ErrBadGroup
	}
	end := groupHeader + int(size)
	return groups[groupHeader:end], groups[end:], nil
}

// GroupCount is how many groups a table of rows rows in groups of groupRows
// holds.
func GroupCount(rows, groupRows int) int {
	count := rows / groupRows
	if rows%groupRows != 0 {
		count++
	}
	return count
}
