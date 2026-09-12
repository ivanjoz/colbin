package bench

// colbin against protocol buffers over the corpus, which is the comparison the
// hand-written literals could not make.
//
// The two sides encode the *same generated rows*: the corpus is built once and
// converted into protobuf messages field for field, so any difference in bytes
// or in time is the format rather than the data. The conversion happens outside
// the timed loop; a benchmark that included it would be measuring the converter.
//
// Where the two formats cannot be made identical, protobuf is given the better
// option rather than the matching one:
//
//   - cents are `int64` and not `sint64`, because every amount here is
//     non-negative and int64 is the shorter of the two for those;
//   - `age` and the basis-point fields are uint32 in proto, which has no uint8
//     or uint16 — a varint encodes them in one byte either way;
//   - protobuf marshals through its generated code with a reused buffer, which
//     is its fast path.

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/ivanjoz/colbin"
	"github.com/ivanjoz/colbin/corpus"
)

var (
	corpusData = corpus.Generate(corpus.Seed, corpus.Small)

	corpusUsers    = corpus.Generate(corpus.Seed, corpus.Small).Users
	corpusProducts = corpusData.Products
	corpusSales    = corpusData.Sales
	corpusMetrics  = corpusData.Metrics

	protoUsers    = toProtoUsers(corpusData.Users)
	protoProducts = toProtoProducts(corpusData.Products)
	protoSales    = toProtoSales(corpusData.Sales)
	protoMetrics  = toProtoMetrics(corpusData.Metrics)

	corpusUserCodec    = colbin.MustCodec[corpus.User]()
	corpusProductCodec = colbin.MustCodec[corpus.Product]()
	corpusSaleCodec    = colbin.MustCodec[corpus.Sale]()
	corpusMetricCodec  = colbin.MustCodec[corpus.Metric]()
)

func toProtoUsers(rows []corpus.User) []*CorpusUser {
	out := make([]*CorpusUser, len(rows))
	for index, row := range rows {
		out[index] = &CorpusUser{
			Id: row.ID, Name: row.Name, Email: row.Email, Country: row.Country,
			Age: uint32(row.Age), Active: row.Active, CreatedAt: row.CreatedAt,
		}
	}
	return out
}

func toProtoProducts(rows []corpus.Product) []*CorpusProduct {
	out := make([]*CorpusProduct, len(rows))
	for index, row := range rows {
		out[index] = &CorpusProduct{
			Id: row.ID, Sku: row.SKU, Name: row.Name, PriceCents: row.PriceCents,
			Stock: row.Stock, CategoryId: uint32(row.CategoryID), Tags: row.Tags,
		}
	}
	return out
}

func toProtoSales(rows []corpus.Sale) []*CorpusSale {
	out := make([]*CorpusSale, len(rows))
	for index, row := range rows {
		detail := make([]*CorpusSaleLine, len(row.Detail))
		for line, source := range row.Detail {
			detail[line] = &CorpusSaleLine{
				ProductId: source.ProductID, Quantity: source.Quantity,
				UnitCents: source.UnitCents, DiscountBp: uint32(source.DiscountBP),
				TaxBp: uint32(source.TaxBP), TotalCents: source.TotalCents,
			}
		}
		out[index] = &CorpusSale{
			Id: row.ID, UserId: row.UserID, StoreId: uint32(row.StoreID),
			CreatedAt: row.CreatedAt, SubtotalCents: row.SubtotalCents,
			TaxCents: row.TaxCents, TotalCents: row.TotalCents,
			PaidCents: row.PaidCents, Detail: detail,
		}
	}
	return out
}

func toProtoMetrics(rows []corpus.Metric) []*CorpusMetric {
	out := make([]*CorpusMetric, len(rows))
	for index, row := range rows {
		out[index] = &CorpusMetric{SeriesId: row.SeriesID, At: row.At, Value: row.Value}
	}
	return out
}

// TestCorpusSizes is the headline: total bytes per table, both formats, over
// identical data.
func TestCorpusSizes(t *testing.T) {
	options := proto.MarshalOptions{}
	protoSize := func(messages ...proto.Message) int {
		total := 0
		for _, message := range messages {
			total += options.Size(message)
		}
		return total
	}
	colbinSize := func(encode func(index int) []byte, count int) int {
		total := 0
		for index := range count {
			total += len(encode(index))
		}
		return total
	}

	buffer := make([]byte, 0, 8192)
	type line struct {
		table            string
		rows             int
		protobuf, colbin int
	}
	report := []line{
		{"users", len(corpusUsers),
			protoSize(asMessages(protoUsers)...),
			colbinSize(func(i int) []byte { return corpusUserCodec.Append(buffer[:0], &corpusUsers[i]) }, len(corpusUsers))},
		{"products", len(corpusProducts),
			protoSize(asMessages(protoProducts)...),
			colbinSize(func(i int) []byte { return corpusProductCodec.Append(buffer[:0], &corpusProducts[i]) }, len(corpusProducts))},
		{"sales (nested detail)", len(corpusSales),
			protoSize(asMessages(protoSales)...),
			colbinSize(func(i int) []byte { return corpusSaleCodec.Append(buffer[:0], &corpusSales[i]) }, len(corpusSales))},
		{"metrics", len(corpusMetrics),
			protoSize(asMessages(protoMetrics)...),
			colbinSize(func(i int) []byte { return corpusMetricCodec.Append(buffer[:0], &corpusMetrics[i]) }, len(corpusMetrics))},
	}

	t.Logf("%-22s %7s %10s %10s %8s", "table", "rows", "protobuf", "colbin", "delta")
	var totalProto, totalColbin int
	for _, entry := range report {
		totalProto += entry.protobuf
		totalColbin += entry.colbin
		t.Logf("%-22s %7d %10d %10d %7.1f%%", entry.table, entry.rows,
			entry.protobuf, entry.colbin,
			float64(entry.colbin-entry.protobuf)/float64(entry.protobuf)*100)
	}
	t.Logf("%-22s %7s %10d %10d %7.1f%%", "total", "", totalProto, totalColbin,
		float64(totalColbin-totalProto)/float64(totalProto)*100)
}

func asMessages[T proto.Message](rows []T) []proto.Message {
	out := make([]proto.Message, len(rows))
	for index, row := range rows {
		out[index] = row
	}
	return out
}

// Sales: the nested table, and the one where colbin can transpose.

func BenchmarkCorpusSalesProtobufMarshal(b *testing.B) {
	options := proto.MarshalOptions{}
	buffer := make([]byte, 0, 8192)
	b.ReportAllocs()
	for b.Loop() {
		for _, sale := range protoSales {
			var err error
			buffer, err = options.MarshalAppend(buffer[:0], sale)
			if err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkCorpusSalesColbinAppend(b *testing.B) {
	buffer := make([]byte, 0, 8192)
	b.ReportAllocs()
	for b.Loop() {
		for index := range corpusSales {
			buffer = corpusSaleCodec.Append(buffer[:0], &corpusSales[index])
		}
	}
}

func BenchmarkCorpusSalesProtobufUnmarshal(b *testing.B) {
	encoded := make([][]byte, len(protoSales))
	for index, sale := range protoSales {
		data, err := proto.Marshal(sale)
		if err != nil {
			b.Fatal(err)
		}
		encoded[index] = data
	}
	b.ReportAllocs()
	for b.Loop() {
		for _, data := range encoded {
			into := &CorpusSale{}
			if err := proto.Unmarshal(data, into); err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkCorpusSalesColbinUnmarshal(b *testing.B) {
	encoded := make([][]byte, len(corpusSales))
	for index := range corpusSales {
		encoded[index] = corpusSaleCodec.Encode(&corpusSales[index])
	}
	var into corpus.Sale
	b.ReportAllocs()
	for b.Loop() {
		for _, data := range encoded {
			if err := corpusSaleCodec.Unmarshal(data, &into); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// Users: the ordinary flat record, strings and small integers.

func BenchmarkCorpusUsersProtobufMarshal(b *testing.B) {
	options := proto.MarshalOptions{}
	buffer := make([]byte, 0, 1024)
	b.ReportAllocs()
	for b.Loop() {
		for _, user := range protoUsers {
			var err error
			buffer, err = options.MarshalAppend(buffer[:0], user)
			if err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkCorpusUsersColbinAppend(b *testing.B) {
	buffer := make([]byte, 0, 1024)
	b.ReportAllocs()
	for b.Loop() {
		for index := range corpusUsers {
			buffer = corpusUserCodec.Append(buffer[:0], &corpusUsers[index])
		}
	}
}

func BenchmarkCorpusUsersProtobufUnmarshal(b *testing.B) {
	encoded := make([][]byte, len(protoUsers))
	for index, user := range protoUsers {
		data, err := proto.Marshal(user)
		if err != nil {
			b.Fatal(err)
		}
		encoded[index] = data
	}
	b.ReportAllocs()
	for b.Loop() {
		for _, data := range encoded {
			into := &CorpusUser{}
			if err := proto.Unmarshal(data, into); err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkCorpusUsersColbinUnmarshal(b *testing.B) {
	encoded := make([][]byte, len(corpusUsers))
	for index := range corpusUsers {
		encoded[index] = corpusUserCodec.Encode(&corpusUsers[index])
	}
	var into corpus.User
	b.ReportAllocs()
	for b.Loop() {
		for _, data := range encoded {
			if err := corpusUserCodec.Unmarshal(data, &into); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// Metrics: three integers, the shape a column codec is for.

func BenchmarkCorpusMetricsProtobufMarshal(b *testing.B) {
	options := proto.MarshalOptions{}
	buffer := make([]byte, 0, 256)
	b.ReportAllocs()
	for b.Loop() {
		for _, metric := range protoMetrics {
			var err error
			buffer, err = options.MarshalAppend(buffer[:0], metric)
			if err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkCorpusMetricsColbinAppend(b *testing.B) {
	buffer := make([]byte, 0, 256)
	b.ReportAllocs()
	for b.Loop() {
		for index := range corpusMetrics {
			buffer = corpusMetricCodec.Append(buffer[:0], &corpusMetrics[index])
		}
	}
}
