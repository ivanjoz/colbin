package wire

import (
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
	ids := make([]int32, len(rows))
	users := make([]int32, len(rows))
	amounts := make([]int64, len(rows))
	names := make([]string, len(rows))
	for index, row := range rows {
		ids[index], users[index] = row.ID, row.UserID
		amounts[index], names[index] = row.Amount, row.Name
	}
	writer := Writer8{Buffer: buffer}
	mark := writer.OpenTable(0, len(rows))
	writer.Column32(0, ids)
	writer.Column32(1, users)
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
			for index, value := range columns.Column32(rows, nil) {
				out[index].ID = value
			}
		case 1:
			for index, value := range columns.Column32(rows, nil) {
				out[index].UserID = value
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
