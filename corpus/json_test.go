package corpus

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ivanjoz/colbin"
)

// The schema section over the corpus: what a reader without the Go type gets,
// what it costs, and whether it agrees with encoding/json.
//
// # What the reference is
//
// Every comparison below is against `encoding/json` over the record the *Go*
// decoder produces from the same message — not over the row the generator built.
// The two differ in exactly one place: an empty slice and a nil slice are the
// same absence on this wire, so a row holding `[]string{}` comes back nil and
// renders as null. Pinning the walk to what Unmarshal sees is the stronger
// statement in any case — either both decoders agree, or the test says where.

// agreesWithEncodingJSON runs a sample of rows through the schema walk and
// compares each against encoding/json.
func agreesWithEncodingJSON[T any](t *testing.T, name string, rows []T, sample int) {
	t.Helper()
	schema, err := colbin.SchemaFor[T]()
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	buffer := make([]byte, 0, 4096)
	text := make([]byte, 0, 4096)
	for index := range min(sample, len(rows)) {
		message, err := colbin.Append(buffer[:0], &rows[index])
		if err != nil {
			t.Fatalf("%s[%d]: %v", name, index, err)
		}
		text, err = colbin.AppendJSON(text[:0], schema, message)
		if err != nil {
			t.Fatalf("%s[%d]: %v", name, index, err)
		}
		var decoded T
		if err := colbin.Unmarshal(message, &decoded); err != nil {
			t.Fatalf("%s[%d]: %v", name, index, err)
		}
		want, err := json.Marshal(&decoded)
		if err != nil {
			t.Fatal(err)
		}
		var gotValue, wantValue any
		if err := json.Unmarshal(text, &gotValue); err != nil {
			t.Fatalf("%s[%d] produced invalid JSON: %v\n%s", name, index, err, text)
		}
		if err := json.Unmarshal(want, &wantValue); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(gotValue, wantValue) {
			t.Fatalf("%s[%d]\n got %s\nwant %s", name, index, text, want)
		}
	}
}

// Phase 3 and 4: the flat records and the nested one, over real-shaped data
// rather than over a literal chosen to pass.
func TestJSONAgreesWithEncodingJSONAcrossTheCorpus(t *testing.T) {
	built := Generate(Seed, Small)
	agreesWithEncodingJSON(t, "users", built.Users, 40)
	agreesWithEncodingJSON(t, "products", built.Products, 40)
	agreesWithEncodingJSON(t, "categories", built.Categories, 12)
	agreesWithEncodingJSON(t, "stores", built.Stores, 5)
	agreesWithEncodingJSON(t, "sales", built.Sales, 60)
	agreesWithEncodingJSON(t, "events", built.Events, 40)
	agreesWithEncodingJSON(t, "metrics", built.Metrics, 40)
}

// Phase 5: the same sale type in both layouts. The corpus straddles the table
// threshold on purpose, so this picks one of each rather than hoping.
func TestJSONReadsBothSaleLayouts(t *testing.T) {
	const threshold = 8 // codec.tableThreshold
	built := Generate(Seed, Small)
	var asList, asTable []Sale
	for _, sale := range built.Sales {
		if len(sale.Detail) >= threshold {
			if len(asTable) < 20 {
				asTable = append(asTable, sale)
			}
			continue
		}
		if len(asList) < 20 {
			asList = append(asList, sale)
		}
	}
	if len(asList) == 0 || len(asTable) == 0 {
		t.Fatalf("the corpus gave %d list sales and %d table ones", len(asList), len(asTable))
	}
	agreesWithEncodingJSON(t, "sales (list)", asList, len(asList))
	agreesWithEncodingJSON(t, "sales (table)", asTable, len(asTable))
}

// A message that carries its own schema decodes into the Go type and into JSON,
// which is what makes the section additive rather than a second format.
func TestSelfDescribingSalesAcrossTheCorpus(t *testing.T) {
	built := Generate(Seed, Small)
	for index := range min(30, len(built.Sales)) {
		sale := &built.Sales[index]
		message, err := colbin.MarshalSelfDescribing(sale)
		if err != nil {
			t.Fatal(err)
		}
		var back Sale
		if err := colbin.Unmarshal(message, &back); err != nil {
			t.Fatalf("sales[%d]: %v", index, err)
		}
		if !reflect.DeepEqual(&back, sale) {
			t.Fatalf("sales[%d] round-tripped as %+v", index, back)
		}
		if _, err := colbin.ToJSON(nil, message); err != nil {
			t.Fatalf("sales[%d]: %v", index, err)
		}
	}
}

// Phase 9's size table: what a schema costs, against what it describes.
//
// It prints rather than asserts, because the numbers are the deliverable:
// `go test ./corpus -run ReportSchema -v` is how you see whether a section is
// worth sending inline before deciding to.
func TestReportSchemaSizes(t *testing.T) {
	built := Generate(Seed, Small)

	report := []struct {
		name    string
		schema  int
		message float64
	}{
		{"users", schemaSize[User](t), meanSize(t, built.Users)},
		{"products", schemaSize[Product](t), meanSize(t, built.Products)},
		{"categories", schemaSize[Category](t), meanSize(t, built.Categories)},
		{"stores", schemaSize[Store](t), meanSize(t, built.Stores)},
		{"sales (with detail)", schemaSize[Sale](t), meanSize(t, built.Sales)},
		{"events", schemaSize[Event](t), meanSize(t, built.Events)},
		{"metrics", schemaSize[Metric](t), meanSize(t, built.Metrics)},
	}
	t.Logf("%-22s %8s %10s %10s", "table", "schema", "B/message", "schema/msg")
	for _, entry := range report {
		t.Logf("%-22s %8d %10.1f %9.1fx",
			entry.name, entry.schema, entry.message, float64(entry.schema)/entry.message)
	}
	t.Log("a schema is sent once per connection; a message is sent every time")
}

func schemaSize[T any](t *testing.T) int {
	t.Helper()
	schema, err := colbin.SchemaFor[T]()
	if err != nil {
		t.Fatal(err)
	}
	return schema.Size()
}

func meanSize[T any](t *testing.T, rows []T) float64 {
	t.Helper()
	buffer := make([]byte, 0, 4096)
	total := 0
	for index := range rows {
		data, err := colbin.Append(buffer[:0], &rows[index])
		if err != nil {
			t.Fatal(err)
		}
		total += len(data)
	}
	return float64(total) / float64(len(rows))
}

// Phase 9's benchmarks: the JSON walk against encoding/json, on the same rows.
//
// The comparison is not quite like for like and is worth having anyway.
// encoding/json starts from a Go struct; this starts from bytes on a wire, which
// is the position a browser or a proxy is actually in — the alternative there is
// not "call encoding/json", it is "decode into a struct first, then call it".

func BenchmarkCorpusUsersToJSON(b *testing.B) {
	benchmarkToJSON(b, Generate(Seed, Small).Users)
}

func BenchmarkCorpusUsersEncodingJSON(b *testing.B) {
	benchmarkEncodingJSON(b, Generate(Seed, Small).Users)
}

func BenchmarkCorpusSalesToJSON(b *testing.B) {
	benchmarkToJSON(b, Generate(Seed, Small).Sales)
}

func BenchmarkCorpusSalesEncodingJSON(b *testing.B) {
	benchmarkEncodingJSON(b, Generate(Seed, Small).Sales)
}

func BenchmarkCorpusUsersDecodeAny(b *testing.B) {
	benchmarkDecodeAny(b, Generate(Seed, Small).Users)
}

func benchmarkToJSON[T any](b *testing.B, rows []T) {
	schema, err := colbin.SchemaFor[T]()
	if err != nil {
		b.Fatal(err)
	}
	encoded := encodeAll(b, rows)
	text := make([]byte, 0, 4096)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		for _, message := range encoded {
			if text, err = colbin.AppendJSON(text[:0], schema, message); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ReportMetric(float64(len(rows)), "rows/op")
}

func benchmarkEncodingJSON[T any](b *testing.B, rows []T) {
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		for index := range rows {
			if _, err := json.Marshal(&rows[index]); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ReportMetric(float64(len(rows)), "rows/op")
}

func benchmarkDecodeAny[T any](b *testing.B, rows []T) {
	schema, err := colbin.SchemaFor[T]()
	if err != nil {
		b.Fatal(err)
	}
	encoded := encodeAll(b, rows)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		for _, message := range encoded {
			if _, err := colbin.DecodeAny(schema, message); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ReportMetric(float64(len(rows)), "rows/op")
}

func encodeAll[T any](b *testing.B, rows []T) [][]byte {
	b.Helper()
	encoded := make([][]byte, len(rows))
	for index := range rows {
		message, err := colbin.Marshal(&rows[index])
		if err != nil {
			b.Fatal(err)
		}
		encoded[index] = message
	}
	return encoded
}
