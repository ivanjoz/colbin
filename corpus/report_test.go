package corpus

import (
	"testing"

	"github.com/ivanjoz/colbin"
)

// A size report over the corpus, per table.
//
// It is a test rather than a benchmark because it measures bytes, and it prints
// rather than asserts because the numbers are the deliverable: `go test
// ./corpus -run Report -v` is how you see what a shape costs before changing it.

func encodedSize[T any](t *testing.T, rows []T) (total int) {
	t.Helper()
	buffer := make([]byte, 0, 4096)
	for index := range rows {
		data, err := colbin.Append(buffer[:0], &rows[index])
		if err != nil {
			t.Fatal(err)
		}
		total += len(data)
	}
	return total
}

func TestReportTableSizes(t *testing.T) {
	built := Generate(Seed, Small)

	type row struct {
		name  string
		rows  int
		bytes int
	}
	report := []row{
		{"users", len(built.Users), encodedSize(t, built.Users)},
		{"products", len(built.Products), encodedSize(t, built.Products)},
		{"categories", len(built.Categories), encodedSize(t, built.Categories)},
		{"stores", len(built.Stores), encodedSize(t, built.Stores)},
		{"sales (with detail)", len(built.Sales), encodedSize(t, built.Sales)},
		{"events", len(built.Events), encodedSize(t, built.Events)},
		{"metrics", len(built.Metrics), encodedSize(t, built.Metrics)},
	}
	t.Logf("%-22s %8s %10s %10s", "table", "rows", "bytes", "B/row")
	grandTotal := 0
	for _, entry := range report {
		grandTotal += entry.bytes
		t.Logf("%-22s %8d %10d %10.1f",
			entry.name, entry.rows, entry.bytes, float64(entry.bytes)/float64(entry.rows))
	}
	t.Logf("%-22s %8s %10d", "whole corpus", "", grandTotal)
}

// TestReportSaleLayouts is the one the corpus exists for: the same table, split
// by which layout the encoder chose, so the columnar saving is visible rather
// than averaged away.
func TestReportSaleLayouts(t *testing.T) {
	const threshold = 8 // codec.tableThreshold
	built := Generate(Seed, Small)
	buffer := make([]byte, 0, 4096)

	var listSales, tableSales, listLines, tableLines, listBytes, tableBytes int
	for index := range built.Sales {
		sale := &built.Sales[index]
		data, err := colbin.Append(buffer[:0], sale)
		if err != nil {
			t.Fatal(err)
		}
		if len(sale.Detail) >= threshold {
			tableSales++
			tableLines += len(sale.Detail)
			tableBytes += len(data)
			continue
		}
		listSales++
		listLines += len(sale.Detail)
		listBytes += len(data)
	}
	t.Logf("%-28s %6s %7s %9s %9s", "layout", "sales", "lines", "bytes", "B/line")
	t.Logf("%-28s %6d %7d %9d %9.1f", "list of structs (<8 lines)",
		listSales, listLines, listBytes, float64(listBytes)/float64(listLines))
	t.Logf("%-28s %6d %7d %9d %9.1f", "table, transposed (>=8)",
		tableSales, tableLines, tableBytes, float64(tableBytes)/float64(tableLines))
}

// Benchmarks over the corpus, which is what the hand-written literals could not
// give: a realistic mix rather than one record repeated.

func BenchmarkCorpusSalesAppend(b *testing.B) {
	sales := Generate(Seed, Small).Sales
	codec := colbin.MustCodec[Sale]()
	buffer := make([]byte, 0, 4096)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		for index := range sales {
			buffer = codec.Append(buffer[:0], &sales[index])
		}
	}
	b.ReportMetric(float64(len(sales)), "sales/op")
}

func BenchmarkCorpusSalesUnmarshal(b *testing.B) {
	sales := Generate(Seed, Small).Sales
	codec := colbin.MustCodec[Sale]()
	encoded := make([][]byte, len(sales))
	for index := range sales {
		encoded[index] = codec.Encode(&sales[index])
	}
	var into Sale
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		for _, data := range encoded {
			if err := codec.Unmarshal(data, &into); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ReportMetric(float64(len(sales)), "sales/op")
}

func BenchmarkCorpusUsersAppend(b *testing.B) {
	users := Generate(Seed, Small).Users
	codec := colbin.MustCodec[User]()
	buffer := make([]byte, 0, 512)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		for index := range users {
			buffer = codec.Append(buffer[:0], &users[index])
		}
	}
	b.ReportMetric(float64(len(users)), "users/op")
}

func BenchmarkCorpusMetricsAppend(b *testing.B) {
	metrics := Generate(Seed, Small).Metrics
	codec := colbin.MustCodec[Metric]()
	buffer := make([]byte, 0, 512)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		for index := range metrics {
			buffer = codec.Append(buffer[:0], &metrics[index])
		}
	}
	b.ReportMetric(float64(len(metrics)), "rows/op")
}
