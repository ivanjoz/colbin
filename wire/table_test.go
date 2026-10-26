package wire

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
	"testing"

	"github.com/ivanjoz/colbin/internal/wirefmt"
)

type tableRow struct {
	ID     int32
	UserID int32
	Amount int64
	Name   string
}

func makeRows(n int) []tableRow {
	rows := make([]tableRow, n)
	for index := range rows {
		rows[index] = tableRow{
			ID:     int32(1000 + index),
			UserID: int32(index % 7),
			Amount: int64(index) * 137,
			Name:   fmt.Sprintf("row-%d", index%5),
		}
	}
	return rows
}

func writeTable(buffer []byte, rows []tableRow) []byte {
	writer := Writer8{Buffer: buffer}
	mark := writer.OpenTable(0, len(rows))
	writer.Buffer = writeColumns(writer.Buffer, rows)
	writer.Close(mark)
	return writer.Buffer
}

// writeGroupedTable is writeTable in groups of groupRows.
func writeGroupedTable(buffer []byte, rows []tableRow, groupRows int) []byte {
	writer := Writer8{Buffer: buffer}
	mark := writer.OpenGroupedTable(0, len(rows), groupRows)
	for start := 0; start < len(rows); start += groupRows {
		var at int
		writer.Buffer, at = OpenGroup(writer.Buffer)
		writer.Buffer = writeColumns(writer.Buffer, rows[start:min(start+groupRows, len(rows))])
		CloseGroup(writer.Buffer, at)
	}
	writer.Close(mark)
	return writer.Buffer
}

func writeColumns(buffer []byte, rows []tableRow) []byte {
	ids := make([]int64, len(rows))
	users := make([]int64, len(rows))
	amounts := make([]int64, len(rows))
	names := make([]string, len(rows))
	for index, row := range rows {
		ids[index], users[index] = int64(row.ID), int64(row.UserID)
		amounts[index], names[index] = row.Amount, row.Name
	}
	writer := Writer8{Buffer: buffer}
	writer.Column(0, ids)
	writer.Column(1, users)
	writer.Column(2, amounts)
	writer.StringColumn(3, names)
	return writer.Buffer
}

func writeRowWise(buffer []byte, rows []tableRow) []byte {
	writer := Writer8{Buffer: buffer}
	list := writer.OpenList(0, len(rows))
	for _, row := range rows {
		element := writer.OpenElementStruct()
		writer.I32(0, row.ID)
		writer.I32(1, row.UserID)
		writer.Int(2, row.Amount)
		writer.String(3, row.Name)
		writer.Close(element)
	}
	writer.Close(list)
	return writer.Buffer
}

func readTable(message []byte) ([]tableRow, error) {
	reader := NewReader8(message)
	rows, groupRows, body, ok := reader.Table()
	if !ok {
		return nil, reader.Err()
	}
	out := make([]tableRow, rows)
	if groupRows == 0 {
		return out, readColumns(body, out)
	}
	for start := 0; start < rows; start += groupRows {
		columns, rest, err := NextGroup(body)
		if err != nil {
			return nil, err
		}
		if err := readColumns(columns, out[start:min(start+groupRows, rows)]); err != nil {
			return nil, err
		}
		body = rest
	}
	if len(body) != 0 {
		return nil, ErrBadGroup
	}
	return out, nil
}

func readColumns(body []byte, out []tableRow) error {
	columns := NewReader8(body)
	rows := len(out)
	for columns.More() {
		switch columns.Key() {
		case 0:
			for index, value := range columns.Column(rows, nil) {
				out[index].ID = int32(value)
			}
		case 1:
			for index, value := range columns.Column(rows, nil) {
				out[index].UserID = int32(value)
			}
		case 2:
			for index, value := range columns.Column(rows, nil) {
				out[index].Amount = value
			}
		case 3:
			for index, value := range columns.Strings(nil) {
				out[index].Name = value
			}
		default:
			columns.Skip()
		}
	}
	return columns.Err()
}

func TestTableRoundTrip(t *testing.T) {
	for _, n := range []int{1, 2, 3, 10, 128, 129, 1000} {
		rows := makeRows(n)
		// groupRows 0 is the table written whole.
		for _, groupRows := range []int{0, 1, 7, 128, 1000} {
			message := writeTable(nil, rows)
			if groupRows != 0 {
				message = writeGroupedTable(nil, rows, groupRows)
			}
			back, err := readTable(message)
			if err != nil {
				t.Fatalf("n=%d groups of %d: %v", n, groupRows, err)
			}
			if !slices.Equal(back, rows) {
				t.Fatalf("n=%d groups of %d: read %+v", n, groupRows, back)
			}
		}
	}
}

// A grouped table is refused, never misread, when its groups and its rows
// disagree, and a narrow one is told from a list as a table written whole is.
func TestGroupedTableFraming(t *testing.T) {
	rows := makeRows(10)
	message := writeGroupedTable(nil, rows, 4)
	reader := NewReader8(message)
	if !reader.IsTable() {
		t.Fatal("a grouped table is not a table")
	}
	for cut := 1; cut < len(message); cut++ {
		if _, err := readTable(message[:cut]); err == nil {
			t.Fatalf("a grouped table cut at %d of %d read", cut, len(message))
		}
	}
	// Rows per group of zero.
	zero := slices.Clone(message)
	zero[4] = 0
	if _, err := readTable(zero); err == nil {
		t.Fatal("a table of groups of no rows read")
	}

	narrow := Writer{}
	mark := narrow.OpenGroupedTable(3, 2, 1)
	for range 2 {
		var at int
		narrow.Buffer, at = OpenGroup(narrow.Buffer)
		narrow.Column(0, []int64{5})
		CloseGroup(narrow.Buffer, at)
	}
	narrow.Close(mark)
	if narrow.Buffer[0] != 3<<4|0b1111 {
		t.Fatalf("a narrow grouped table opens %#02x", narrow.Buffer[0])
	}
	read := NewReader(narrow.Buffer)
	if !read.IsTable() {
		t.Fatal("a narrow grouped table is not a table")
	}
	got, groupRows, body, ok := read.Table()
	if !ok || got != 2 || groupRows != 1 {
		t.Fatalf("a narrow grouped table read as %d rows in groups of %d, %v", got, groupRows, read.Err())
	}
	for range 2 {
		columns, rest, err := NextGroup(body)
		if err != nil {
			t.Fatal(err)
		}
		group := NewReader(columns)
		if group.Key() != 0 || group.Column(1, nil)[0] != 5 {
			t.Fatalf("a group read as %x", columns)
		}
		body = rest
	}
}

// A column of nothing but zeros is not written, and its absence is what says so.
// That is the old format's omit-empty version byte, deleted.
func TestAZeroColumnIsAbsent(t *testing.T) {
	rows := make([]tableRow, 500)
	for index := range rows {
		rows[index].ID = int32(index)
	}
	message := writeTable(nil, rows)
	back, err := readTable(message)
	if err != nil {
		t.Fatal(err)
	}
	for index := range rows {
		if back[index].UserID != 0 || back[index].Amount != 0 {
			t.Fatalf("row %d: absent columns read as %+v", index, back[index])
		}
	}
	// Only the id column carries anything: the two zero integer columns and the
	// all-empty string column are all absent.
	reader := NewReader8(message)
	_, _, body, _ := reader.Table()
	columns := NewReader8(body)
	keys := []uint8{}
	for columns.More() {
		keys = append(keys, columns.Key())
		columns.Skip()
	}
	if len(keys) != 1 || keys[0] != 0 {
		t.Fatalf("columns present: %v, want the id column only", keys)
	}
}

// The measurement the mode choice used to be: where a table overtakes a list of
// structs. It is a per-field decision now, so this reports rather than asserts —
// except at the ends, where the answer must not have changed sign.
func TestTableAgainstRowWise(t *testing.T) {
	var tableWins int
	for _, n := range []int{1, 2, 3, 4, 8, 16, 32, 64, 128, 512, 4096} {
		rows := makeRows(n)
		columnar := len(writeTable(nil, rows))
		rowWise := len(writeRowWise(nil, rows))
		t.Logf("n=%-5d table %7d B (%5.1f B/row)   list-of-structs %7d B (%5.1f B/row)",
			n, columnar, float64(columnar)/float64(n),
			rowWise, float64(rowWise)/float64(n))
		if columnar < rowWise {
			tableWins++
		}
	}
	if got := len(writeTable(nil, makeRows(1))); got <= len(writeRowWise(nil, makeRows(1))) {
		t.Errorf("one row: the table should not already be winning (%d B)", got)
	}
	if got := len(writeTable(nil, makeRows(4096))); got >= len(writeRowWise(nil, makeRows(4096))) {
		t.Errorf("4096 rows: the table should be winning by a lot (%d B)", got)
	}
	if tableWins == 0 {
		t.Error("the table never won, which makes the class pointless")
	}
}

func BenchmarkTableWrite(b *testing.B) {
	rows := makeRows(1000)
	buffer := make([]byte, 0, 1<<16)
	b.ReportAllocs()
	for b.Loop() {
		buffer = writeTable(buffer[:0], rows)
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(rows)), "ns/row")
}

func BenchmarkTableRead(b *testing.B) {
	rows := makeRows(1000)
	message := writeTable(nil, rows)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := readTable(message); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(rows)), "ns/row")
}

func BenchmarkRowWiseWrite(b *testing.B) {
	rows := makeRows(1000)
	buffer := make([]byte, 0, 1<<16)
	b.ReportAllocs()
	for b.Loop() {
		buffer = writeRowWise(buffer[:0], rows)
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(rows)), "ns/row")
}

// A table's, a list's and a map's length is reserved by a guess at their size,
// and the guess must never show in the bytes: whatever was reserved, the length
// comes out in the shortest form, as a one-byte placeholder widened would
// write it.
func TestASizedLengthIsTheShortestForm(t *testing.T) {
	sizes := []int{0, 1, 200, 253, 254, 255, 256, 300, 65535, 65536, 70000}
	opens := []struct {
		name   string
		narrow func(*Writer, int) Mark
		flags  uint8
		wide   func(*Writer8, int) Mark
		desc   uint8
	}{
		{"table", func(w *Writer, n int) Mark { return w.OpenTable(3, n) }, narrowTable,
			func(w *Writer8, n int) Mark { return w.OpenTable(3, n) }, descriptor(classMap, tableFlag)},
		{"list", func(w *Writer, n int) Mark { return w.OpenList(3, n) }, 0,
			func(w *Writer8, n int) Mark { return w.OpenList(3, n) }, descriptor(classList, 0)},
		{"map", func(w *Writer, n int) Mark { return w.OpenMap(3, n) }, 0,
			func(w *Writer8, n int) Mark { return w.OpenMap(3, n) }, descriptor(classMap, 0)},
	}
	for _, open := range opens {
		for _, count := range sizes {
			for _, size := range sizes {
				filler := make([]byte, size)
				for index := range filler {
					filler[index] = byte(index)
				}
				body := append(appendCount(nil, count), filler...)

				narrow := Writer{Buffer: []byte{0xAA}}
				mark := open.narrow(&narrow, count)
				narrow.Buffer = append(narrow.Buffer, filler...)
				narrow.Close(mark)
				want := append(wirefmt.AppendLength([]byte{0xAA, 3<<4 | nibbleLength | open.flags}, len(body)), body...)
				if !bytes.Equal(narrow.Buffer, want) {
					t.Fatalf("narrow %s of %d, %d-byte body: the framing differs", open.name, count, size)
				}

				wide := Writer8{Buffer: []byte{0xAA}}
				mark = open.wide(&wide, count)
				wide.Buffer = append(wide.Buffer, filler...)
				wide.Close(mark)
				want = []byte{0xAA, 3, open.desc, byte(len(body))}
				if len(body) >= inlineCompositeLength {
					want = binary.LittleEndian.AppendUint32([]byte{0xAA, 3, open.desc | 2}, uint32(len(body)))
				}
				want = append(want, body...)
				if !bytes.Equal(wide.Buffer, want) {
					t.Fatalf("wide %s of %d, %d-byte body: the framing differs", open.name, count, size)
				}
			}
		}
	}
}

// A column is read with the rest of the message after it, for the codec's
// one-load read, but may not take any of it: one read for more rows than it
// holds fails, as it did when it had only its own bytes, rather than decoding
// the next field as its tail.
func TestAColumnStopsAtItsSize(t *testing.T) {
	values := []int64{5, 900, 17, 33, 2, 41, 600, 7}
	var narrow Writer
	narrow.Column(1, values)
	narrow.String(2, string(make([]byte, 64)))
	reader := NewReader(narrow.Buffer)
	if reader.Column(len(values)+8, nil); reader.Err() == nil {
		t.Fatal("narrow: a column read for more rows than it holds succeeded")
	}
	reader = NewReader(narrow.Buffer)
	if got := reader.Column(len(values), nil); reader.Err() != nil || !slices.Equal(got, values) {
		t.Fatal("narrow:", got, reader.Err())
	}

	var wide Writer8
	wide.Column(1, values)
	wide.String(2, string(make([]byte, 64)))
	reader8 := NewReader8(wide.Buffer)
	if reader8.Column(len(values)+8, nil); reader8.Err() == nil {
		t.Fatal("wide: a column read for more rows than it holds succeeded")
	}
}
