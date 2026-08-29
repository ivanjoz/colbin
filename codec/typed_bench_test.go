package codec

import (
	"testing"
)

// The shape a Codec is for: one record per message, many messages. The type is
// numbered `cb:"1"`.. so it also gets compact mode's narrow keys.
type benchStats struct {
	Quantity                int32 `cb:"1"`
	QuantityPendingDelivery int32 `cb:"2"`
	SubQuantity             int16 `cb:"3"`
	SubQuantityPending      int16 `cb:"4"`
	SubDivisor              int16 `cb:"5"`
	TotalAmount             int32 `cb:"6"`
	TotalDebtAmount         int32 `cb:"7"`
}

var (
	benchStatsVal   = benchStats{480, 120, 12, 3, 24, 145900, 32000}
	benchStatsCodec = MustCodec[benchStats]()
)

// --- one record at a time ------------------------------------------------------

func BenchmarkRecordMarshal(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Marshal(benchStatsVal); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRecordMarshalCodec(b *testing.B) {
	v := benchStatsVal
	b.ReportAllocs()
	for b.Loop() {
		if _, err := benchStatsCodec.Marshal(&v); err != nil {
			b.Fatal(err)
		}
	}
}

// Append onto a buffer the caller keeps: the form with nothing left to allocate.
func BenchmarkRecordAppendCodec(b *testing.B) {
	v, buf := benchStatsVal, make([]byte, 0, 64)
	b.ReportAllocs()
	for b.Loop() {
		var err error
		if buf, err = benchStatsCodec.Append(buf[:0], &v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRecordUnmarshal(b *testing.B) {
	data, _ := Marshal(benchStatsVal)
	var out benchStats
	b.ReportAllocs()
	for b.Loop() {
		if err := Unmarshal(data, &out); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRecordUnmarshalCodec(b *testing.B) {
	data, _ := Marshal(benchStatsVal)
	var out benchStats
	b.ReportAllocs()
	for b.Loop() {
		if err := benchStatsCodec.Unmarshal(data, &out); err != nil {
			b.Fatal(err)
		}
	}
}

// --- a batch of separate messages ----------------------------------------------

// 10k records, each its own message, which is the case a Codec exists for: the
// per-call type lookup and reflection are paid 10k times or once.
const separateRecords = 10_000

func benchBatch(n int) []benchStats {
	recs := make([]benchStats, n)
	for i := range recs {
		recs[i] = benchStats{
			Quantity: int32(i%500 + 1), QuantityPendingDelivery: int32(i % 90),
			SubQuantity: int16(i % 24), SubDivisor: 24,
			TotalAmount: int32(i * 37), TotalDebtAmount: int32(i * 3),
		}
	}
	return recs
}

func BenchmarkSeparateMarshal(b *testing.B) {
	recs := benchBatch(separateRecords)
	b.ReportAllocs()
	for b.Loop() {
		for i := range recs {
			if _, err := Marshal(recs[i]); err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkSeparateAppendCodec(b *testing.B) {
	recs := benchBatch(separateRecords)
	buf := make([]byte, 0, 64)
	b.ReportAllocs()
	for b.Loop() {
		for i := range recs {
			var err error
			if buf, err = benchStatsCodec.Append(buf[:0], &recs[i]); err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkSeparateUnmarshal(b *testing.B) {
	msgs := benchMessages(b)
	var out benchStats
	b.ReportAllocs()
	for b.Loop() {
		for _, m := range msgs {
			if err := Unmarshal(m, &out); err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkSeparateUnmarshalCodec(b *testing.B) {
	msgs := benchMessages(b)
	var out benchStats
	b.ReportAllocs()
	for b.Loop() {
		for _, m := range msgs {
			if err := benchStatsCodec.Unmarshal(m, &out); err != nil {
				b.Fatal(err)
			}
		}
	}
}

func benchMessages(b *testing.B) [][]byte {
	b.Helper()
	recs := benchBatch(separateRecords)
	msgs := make([][]byte, len(recs))
	for i := range recs {
		m, err := Marshal(recs[i])
		if err != nil {
			b.Fatal(err)
		}
		msgs[i] = m
	}
	return msgs
}
