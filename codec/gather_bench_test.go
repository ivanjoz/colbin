package codec

import (
	"reflect"
	"strconv"
	"testing"
	"unsafe"
)

type gatherLine struct {
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

func BenchmarkGatherTable(b *testing.B) {
	rows := make([]gatherLine, 600_000)
	for i := range rows {
		rows[i] = gatherLine{PartKey: int32(i), SuppKey: int32(i * 7), LineNumber: int32(i % 7),
			ExtendedPrice: int64(i) * 31, ReturnFlag: "N", LineStatus: "O", ShipMode: "TRUCK",
			ShipInstruct: "NONE", Comment: strconv.Itoa(i)}
	}
	plan, err := planFor(reflect.TypeFor[gatherLine]())
	if err != nil {
		b.Fatal(err)
	}
	var buf scratch
	slice := (*sliceHeader)(unsafe.Pointer(&rows))
	b.ResetTimer()
	for b.Loop() {
		gatherTable(plan, slice, unsafe.Sizeof(rows[0]), &buf)
	}
}
