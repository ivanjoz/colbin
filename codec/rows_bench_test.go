package codec

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/ivanjoz/colbin/wire"
)

// The row path at the shape TPC-H's nested case has: orders, each with a short
// list of line items, under the table threshold, so every line is a list
// element walked field by field. The by-hand writer is the ceiling a
// specialised walk can reach: the same bytes, straight-line.

type rowsOrder struct {
	OrderKey      int32          `cb:"1"`
	CustKey       int32          `cb:"2"`
	OrderStatus   string         `cb:"3"`
	TotalPrice    int64          `cb:"4"`
	OrderDate     int32          `cb:"5"`
	OrderPriority string         `cb:"6"`
	Clerk         string         `cb:"7"`
	ShipPriority  int32          `cb:"8"`
	Comment       string         `cb:"9"`
	Lines         []rowsLineItem `cb:"10"`
}

type rowsLineItem struct {
	PartKey       int32  `cb:"1"`
	SuppKey       int32  `cb:"2"`
	LineNumber    int32  `cb:"3"`
	Quantity      int32  `cb:"4"`
	ExtendedPrice int64  `cb:"5"`
	Discount      int32  `cb:"6"`
	Tax           int32  `cb:"7"`
	ReturnFlag    string `cb:"8"`
	LineStatus    string `cb:"9"`
	ShipDate      int32  `cb:"10"`
	CommitDate    int32  `cb:"11"`
	ReceiptDate   int32  `cb:"12"`
	ShipInstruct  string `cb:"13"`
	ShipMode      string `cb:"14"`
	Comment       string `cb:"15"`
}

type rowsNested struct {
	Orders []rowsOrder `cb:"1"`
}

// makeRowsNested builds orders with TPC-H's spread of values: one to seven
// lines each, keys in the hundreds of thousands, short codes and comments.
func makeRowsNested(orders int) *rowsNested {
	random := rand.New(rand.NewPCG(11, 12))
	words := []string{"carefully", "final", "deposits", "sleep", "quickly", "ironic", "packages", "blithely"}
	comment := func(n int) string {
		var out bytes.Buffer
		for range n {
			out.WriteString(words[random.IntN(len(words))])
			out.WriteByte(' ')
		}
		return out.String()
	}
	value := &rowsNested{Orders: make([]rowsOrder, orders)}
	for index := range value.Orders {
		order := &value.Orders[index]
		*order = rowsOrder{
			OrderKey: int32(index*4 + 1), CustKey: random.Int32N(150_000),
			OrderStatus: []string{"O", "F", "P"}[random.IntN(3)], TotalPrice: random.Int64N(50_000_000),
			OrderDate: 8000 + random.Int32N(2400), OrderPriority: "3-MEDIUM",
			Clerk: fmt.Sprintf("Clerk#%09d", random.IntN(1000)), Comment: comment(5),
		}
		order.Lines = make([]rowsLineItem, 1+random.IntN(7))
		for line := range order.Lines {
			order.Lines[line] = rowsLineItem{
				PartKey: random.Int32N(200_000), SuppKey: random.Int32N(10_000), LineNumber: int32(line + 1),
				Quantity: 1 + random.Int32N(50), ExtendedPrice: random.Int64N(10_000_000),
				Discount: random.Int32N(11), Tax: random.Int32N(9),
				ReturnFlag: []string{"R", "A", "N"}[random.IntN(3)], LineStatus: []string{"O", "F"}[random.IntN(2)],
				ShipDate: 8000 + random.Int32N(2500), CommitDate: 8000 + random.Int32N(2500),
				ReceiptDate: 8000 + random.Int32N(2500), ShipInstruct: "DELIVER IN PERSON",
				ShipMode: []string{"AIR", "MAIL", "SHIP", "TRUCK"}[random.IntN(4)], Comment: comment(3),
			}
		}
	}
	return value
}

func appendRowsByHand(buffer []byte, value *rowsNested) []byte {
	writer := wire.Writer{Buffer: append(buffer, rootStructNarrow)}
	if len(value.Orders) == 0 {
		return writer.Buffer
	}
	orders := writer.OpenList(0, len(value.Orders))
	for index := range value.Orders {
		order := &value.Orders[index]
		element := writer.OpenElement()
		writer.I32(0, order.OrderKey)
		writer.I32(1, order.CustKey)
		writer.String(2, order.OrderStatus)
		writer.Int(3, order.TotalPrice)
		writer.I32(4, order.OrderDate)
		writer.String(5, order.OrderPriority)
		writer.String(6, order.Clerk)
		writer.I32(7, order.ShipPriority)
		writer.String(8, order.Comment)
		if len(order.Lines) > 0 {
			lines := writer.OpenList(9, len(order.Lines))
			for line := range order.Lines {
				item := &order.Lines[line]
				element := writer.OpenElement()
				writer.I32(0, item.PartKey)
				writer.I32(1, item.SuppKey)
				writer.I32(2, item.LineNumber)
				writer.I32(3, item.Quantity)
				writer.Int(4, item.ExtendedPrice)
				writer.I32(5, item.Discount)
				writer.I32(6, item.Tax)
				writer.String(7, item.ReturnFlag)
				writer.String(8, item.LineStatus)
				writer.I32(9, item.ShipDate)
				writer.I32(10, item.CommitDate)
				writer.I32(11, item.ReceiptDate)
				writer.String(12, item.ShipInstruct)
				writer.String(13, item.ShipMode)
				writer.String(14, item.Comment)
				writer.CloseElement(element)
			}
			writer.Close(lines)
		}
		writer.CloseElement(element)
	}
	writer.Close(orders)
	return writer.Buffer
}

// The by-hand writer is the row path, which this shape no longer takes: an
// order holding its lines is a table with a list column (listcolumn.go). Its
// bytes still decode to the same value, and the table's are fewer.
func TestRowsByHandIsTheFormat(t *testing.T) {
	value := makeRowsNested(300)
	table := must(MustCodec[rowsNested]().Encode(value))
	rows := appendRowsByHand(nil, value)
	if len(table) >= len(rows) {
		t.Fatalf("by hand %d bytes, codec %d bytes", len(rows), len(table))
	}
	var back rowsNested
	if err := Unmarshal(rows, &back); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(must(Marshal(&back)), table) {
		t.Fatal("the rows decode to another value")
	}
}

func BenchmarkRowsAppend(b *testing.B) {
	value := makeRowsNested(2000)
	handle := MustCodec[rowsNested]()
	buffer := make([]byte, 0, 1<<20)
	b.ReportAllocs()
	for b.Loop() {
		buffer, _ = handle.Append(buffer[:0], value)
	}
	b.SetBytes(int64(len(buffer)))
}

func BenchmarkRowsAppendByHand(b *testing.B) {
	value := makeRowsNested(2000)
	buffer := make([]byte, 0, 1<<20)
	b.ReportAllocs()
	for b.Loop() {
		buffer = appendRowsByHand(buffer[:0], value)
	}
	b.SetBytes(int64(len(buffer)))
}

// The three walks side by side on the same rows: writeFields, the row program
// in Go, and the program with amd64's kernel. Off amd64 the last is the second.
func BenchmarkRowsVariants(b *testing.B) {
	value := makeRowsNested(2000)
	handle := MustCodec[rowsNested]()
	for _, variant := range []struct {
		name            string
		program, kernel bool
	}{{"interpreter", false, false}, {"program", true, false}, {"kernel", true, true}} {
		b.Run(variant.name, func(b *testing.B) {
			useRowPrograms = variant.program
			was := setRowKernel(variant.kernel)
			defer func() { useRowPrograms = true; setRowKernel(was) }()
			buffer := make([]byte, 0, 1<<20)
			for b.Loop() {
				buffer, _ = handle.Append(buffer[:0], value)
			}
		})
	}
}

// The same rows declared with no tags, as a program that says nothing to
// colbin declares them: keys from the field names, so eight-bit keys throughout.

type rowsOrderUntagged struct {
	OrderKey      int32
	CustKey       int32
	OrderStatus   string
	TotalPrice    int64
	OrderDate     int32
	OrderPriority string
	Clerk         string
	ShipPriority  int32
	Comment       string
	Lines         []rowsLineItemUntagged
}

type rowsLineItemUntagged struct {
	PartKey       int32
	SuppKey       int32
	LineNumber    int32
	Quantity      int32
	ExtendedPrice int64
	Discount      int32
	Tax           int32
	ReturnFlag    string
	LineStatus    string
	ShipDate      int32
	CommitDate    int32
	ReceiptDate   int32
	ShipInstruct  string
	ShipMode      string
	Comment       string
}

type rowsNestedUntagged struct {
	Orders []rowsOrderUntagged
}

func makeRowsNestedUntagged(orders int) *rowsNestedUntagged {
	tagged := makeRowsNested(orders)
	value := &rowsNestedUntagged{Orders: make([]rowsOrderUntagged, len(tagged.Orders))}
	for index, order := range tagged.Orders {
		lines := make([]rowsLineItemUntagged, len(order.Lines))
		for line, item := range order.Lines {
			lines[line] = rowsLineItemUntagged(item)
		}
		value.Orders[index] = rowsOrderUntagged{order.OrderKey, order.CustKey, order.OrderStatus,
			order.TotalPrice, order.OrderDate, order.OrderPriority, order.Clerk, order.ShipPriority,
			order.Comment, lines}
	}
	return value
}

func BenchmarkRowsWideVariants(b *testing.B) {
	value := makeRowsNestedUntagged(2000)
	handle := MustCodec[rowsNestedUntagged]()
	if !handle.plan.isWide {
		b.Fatal("the untagged rows are not wide")
	}
	for _, variant := range []struct {
		name            string
		program, kernel bool
	}{{"interpreter", false, false}, {"program", true, false}, {"kernel", true, true}} {
		b.Run(variant.name, func(b *testing.B) {
			useRowPrograms = variant.program
			was := setRowKernel(variant.kernel)
			defer func() { useRowPrograms = true; setRowKernel(was) }()
			buffer := make([]byte, 0, 1<<20)
			for b.Loop() {
				buffer, _ = handle.Append(buffer[:0], value)
			}
		})
	}
}

// The read side: readNarrowRun and the program reading the same rows, narrow
// and wide.
func BenchmarkRowsDecodeVariants(b *testing.B) {
	narrow := must(MustCodec[rowsNested]().Encode(makeRowsNested(2000)))
	wide := must(MustCodec[rowsNestedUntagged]().Encode(makeRowsNestedUntagged(2000)))
	for _, variant := range []struct {
		name    string
		program bool
	}{{"interpreter", false}, {"program", true}} {
		b.Run("narrow/"+variant.name, func(b *testing.B) {
			useRowPrograms = variant.program
			defer func() { useRowPrograms = true }()
			b.ReportAllocs()
			for b.Loop() {
				var value rowsNested
				if err := Unmarshal(narrow, &value); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("wide/"+variant.name, func(b *testing.B) {
			useRowPrograms = variant.program
			defer func() { useRowPrograms = true }()
			b.ReportAllocs()
			for b.Loop() {
				var value rowsNestedUntagged
				if err := Unmarshal(wide, &value); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
