package codec

import (
	"bytes"
	"math"
	"math/rand/v2"
	"testing"
)

// A short table gathered with its scans (gatherScanned) must write gatherTable's
// bytes, over every op a column holds and every row count around the short
// path's end.

type scannedRow struct {
	B   bool    `cb:"1"`
	I8  int8    `cb:"2"`
	I16 int16   `cb:"3"`
	I32 int32   `cb:"4"`
	I64 int64   `cb:"5"`
	U8  uint8   `cb:"6"`
	U16 uint16  `cb:"7"`
	U32 uint32  `cb:"8"`
	U64 uint64  `cb:"9"`
	F32 float32 `cb:"10"`
	F64 float64 `cb:"11"`
	S   string  `cb:"12"`
}

type scannedPage struct {
	Rows []scannedRow `cb:"1"`
}

func TestScannedTablesWriteGatheredBytes(t *testing.T) {
	codec, err := NewCodec[scannedPage]()
	if err != nil {
		t.Fatal(err)
	}
	random := rand.New(rand.NewPCG(7, 25))
	value := func(shape int) int64 {
		switch shape {
		case 0:
			return 0
		case 1:
			return int64(random.Uint64N(4))
		case 2:
			return []int64{math.MinInt64, math.MaxInt64, -1, 0, 1}[random.IntN(5)]
		}
		return int64(random.Uint64())
	}
	for rows := tableThreshold; rows <= 40; rows++ {
		for round := range 40 {
			page := scannedPage{Rows: make([]scannedRow, rows)}
			shape := round % 4
			for index := range page.Rows {
				x := value(shape)
				page.Rows[index] = scannedRow{
					B: x&1 == 1, I8: int8(x), I16: int16(x), I32: int32(x), I64: x,
					U8: uint8(x), U16: uint16(x), U32: uint32(x), U64: uint64(x),
					F32: math.Float32frombits(uint32(x)), F64: math.Float64frombits(uint64(x)),
				}
				if shape == 3 {
					page.Rows[index].S = "s"
				}
			}
			scanned, err := codec.Encode(&page)
			if err != nil {
				t.Fatal(err)
			}
			useScannedTables = false
			gathered, err := codec.Encode(&page)
			useScannedTables = true
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(scanned, gathered) {
				t.Fatalf("%d rows, shape %d: scanned\n%x\ngathered\n%x", rows, shape, scanned, gathered)
			}
		}
	}
}
