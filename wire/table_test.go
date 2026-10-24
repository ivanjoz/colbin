package wire

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"
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
	ids := make([]int64, len(rows))
	users := make([]int64, len(rows))
	amounts := make([]int64, len(rows))
	names := make([]string, len(rows))
	for index, row := range rows {
		ids[index], users[index] = int64(row.ID), int64(row.UserID)
		amounts[index], names[index] = row.Amount, row.Name
	}
	writer := Writer8{Buffer: buffer}
	mark := writer.OpenTable(0, len(rows))
	writer.Column(0, ids)
	writer.Column(1, users)
	writer.Column(2, amounts)
	writer.StringColumn(3, names)
	writer.Close(mark)
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
	rows, columns, ok := reader.Table()
	if !ok {
		return nil, reader.Err()
	}
	out := make([]tableRow, rows)
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
	return out, columns.Err()
}

func TestTableRoundTrip(t *testing.T) {
	for _, n := range []int{1, 2, 3, 10, 128, 129, 1000} {
		rows := makeRows(n)
		back, err := readTable(writeTable(nil, rows))
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if len(back) != n {
			t.Fatalf("n=%d: read %d rows", n, len(back))
		}
		for index := range rows {
			if back[index] != rows[index] {
				t.Fatalf("n=%d row %d: %+v, want %+v", n, index, back[index], rows[index])
			}
		}
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
	_, columns, _ := reader.Table()
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
				want := append(appendLength([]byte{0xAA, 3<<4 | nibbleLength | open.flags}, len(body)), body...)
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
