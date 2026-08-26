package comparison_test

import (
	jsonv2 "encoding/json/v2"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/ivanjoz/colbin"
	"github.com/ivanjoz/colbin/comparison"
	"google.golang.org/protobuf/proto"
)

const (
	testRecordsPerType      = 24
	benchmarkRecordsPerType = 64
	testSeed                = uint64(0xC01B1A)
)

type wireCodec struct {
	name      string
	marshal   func(*comparison.BenchmarkCorpus) ([]byte, error)
	unmarshal func([]byte, *comparison.BenchmarkCorpus) error
}

var wireCodecs = []wireCodec{
	{
		name: "Colbin",
		marshal: func(v *comparison.BenchmarkCorpus) ([]byte, error) {
			return colbin.Marshal(v)
		},
		unmarshal: func(data []byte, v *comparison.BenchmarkCorpus) error {
			return colbin.Unmarshal(data, v)
		},
	},
	{
		name: "Protobuf",
		marshal: func(v *comparison.BenchmarkCorpus) ([]byte, error) {
			return proto.Marshal(v)
		},
		unmarshal: func(data []byte, v *comparison.BenchmarkCorpus) error {
			return proto.Unmarshal(data, v)
		},
	},
	{
		name: "JSONv2",
		marshal: func(v *comparison.BenchmarkCorpus) ([]byte, error) {
			return jsonv2.Marshal(v)
		},
		unmarshal: func(data []byte, v *comparison.BenchmarkCorpus) error {
			return jsonv2.Unmarshal(data, v)
		},
	},
	{
		name: "CBOR",
		marshal: func(v *comparison.BenchmarkCorpus) ([]byte, error) {
			return cbor.Marshal(v)
		},
		unmarshal: func(data []byte, v *comparison.BenchmarkCorpus) error {
			return cbor.Unmarshal(data, v)
		},
	},
}

func TestCorpusGeneratorIsDeterministic(t *testing.T) {
	first := comparison.GenerateCorpus(testSeed, 3)
	second := comparison.GenerateCorpus(testSeed, 3)
	if !proto.Equal(first, second) {
		t.Fatal("same seed generated different corpora")
	}
	if proto.Equal(first, comparison.GenerateCorpus(testSeed+1, 3)) {
		t.Fatal("different seeds generated identical corpora")
	}
}

func TestExampleBatchesCoverEveryModel(t *testing.T) {
	batches := exampleBatches(comparison.GenerateCorpus(testSeed, 1))
	if got, want := len(batches), comparison.ExampleTypeCount; got != want {
		t.Fatalf("example batch count = %d, want %d", got, want)
	}
	for _, batch := range batches {
		if batch.message == nil {
			t.Fatalf("%s batch is nil", batch.name)
		}
	}
}

func TestRoundTripBenchmarkCorpus(t *testing.T) {
	want := comparison.GenerateCorpus(testSeed, testRecordsPerType)
	for _, codec := range wireCodecs {
		t.Run(codec.name, func(t *testing.T) {
			data, err := codec.marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			var got comparison.BenchmarkCorpus
			if err := codec.unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(want, &got) {
				t.Fatalf("%s corpus round-trip mismatch", codec.name)
			}
		})
	}
}

// TestPayloadSizeComparison reports wire bytes for identical batches. It does
// not assert that either format wins: the result legitimately depends on the
// data shape, batch size, and codec changes.
func TestPayloadSizeComparison(t *testing.T) {
	corpus := comparison.GenerateCorpus(testSeed, testRecordsPerType)
	reportPayloadSize(t, "all models", corpus)
	for _, batch := range exampleBatches(corpus) {
		reportPayloadSize(t, batch.name, batch.message)
	}
}

func reportPayloadSize(t *testing.T, name string, message *comparison.BenchmarkCorpus) {
	t.Helper()
	var sizes [4]int
	for i, codec := range wireCodecs {
		data, err := codec.marshal(message)
		if err != nil {
			t.Fatalf("%s marshal %s: %v", codec.name, name, err)
		}
		if len(data) == 0 {
			t.Fatalf("%s produced an empty %s payload", name, codec.name)
		}
		sizes[i] = len(data)
	}
	t.Logf("%-20s colbin=%8d B  protobuf=%8d B  jsonv2=%8d B  cbor=%8d B",
		name, sizes[0], sizes[1], sizes[2], sizes[3])
}

type exampleBatch struct {
	name    string
	message *comparison.BenchmarkCorpus
}

// exampleBatches projects one generated corpus into equivalent repeated-message
// Protobuf wrappers, allowing all twenty-one domain types to be compared alone.
func exampleBatches(c *comparison.BenchmarkCorpus) []exampleBatch {
	return []exampleBatch{
		{"Address", &comparison.BenchmarkCorpus{Addresses: c.Addresses}},
		{"Person", &comparison.BenchmarkCorpus{People: c.People}},
		{"Company", &comparison.BenchmarkCorpus{Companies: c.Companies}},
		{"Product", &comparison.BenchmarkCorpus{Products: c.Products}},
		{"OrderLine", &comparison.BenchmarkCorpus{OrderLines: c.OrderLines}},
		{"Order", &comparison.BenchmarkCorpus{Orders: c.Orders}},
		{"Invoice", &comparison.BenchmarkCorpus{Invoices: c.Invoices}},
		{"SensorReading", &comparison.BenchmarkCorpus{SensorReadings: c.SensorReadings}},
		{"WeatherSample", &comparison.BenchmarkCorpus{WeatherSamples: c.WeatherSamples}},
		{"GeoPoint", &comparison.BenchmarkCorpus{GeoPoints: c.GeoPoints}},
		{"Route", &comparison.BenchmarkCorpus{Routes: c.Routes}},
		{"BlogPost", &comparison.BenchmarkCorpus{BlogPosts: c.BlogPosts}},
		{"Comment", &comparison.BenchmarkCorpus{Comments: c.Comments}},
		{"UserProfile", &comparison.BenchmarkCorpus{UserProfiles: c.UserProfiles}},
		{"GamePlayer", &comparison.BenchmarkCorpus{GamePlayers: c.GamePlayers}},
		{"GameMatch", &comparison.BenchmarkCorpus{GameMatches: c.GameMatches}},
		{"MetricPoint", &comparison.BenchmarkCorpus{MetricPoints: c.MetricPoints}},
		{"MetricSeries", &comparison.BenchmarkCorpus{MetricSeries: c.MetricSeries}},
		{"LogEvent", &comparison.BenchmarkCorpus{LogEvents: c.LogEvents}},
		{"PortfolioPosition", &comparison.BenchmarkCorpus{PortfolioPositions: c.PortfolioPositions}},
		{"Portfolio", &comparison.BenchmarkCorpus{Portfolios: c.Portfolios}},
	}
}

// benchCodec is what the benchmarks drive: encode the corpus, then read the
// result back. Colbin's JSON mode belongs here but not in wireCodecs above,
// because its readers do not target the corpus type at all — that is the point
// of the mode — so it has no place in the round-trip test or the payload report.
type benchCodec struct {
	name   string
	encode func(*comparison.BenchmarkCorpus) ([]byte, error)
	decode func([]byte) error
}

// benchCodecs is the round-trip formats plus the two JSON-mode readers: one
// producing JSON text, one producing Go values. Both work from the schema the
// message carries, with no Go type involved.
//
// The two JSON-mode entries share an encode call, so BenchmarkEncode measures
// MarshalJSON twice. That is left in deliberately: the two figures are the same
// work, so the gap between them is the benchmark's own noise floor, measured
// alongside the numbers it applies to.
var benchCodecs = func() []benchCodec {
	out := make([]benchCodec, 0, len(wireCodecs)+2)
	for _, codec := range wireCodecs {
		out = append(out, benchCodec{
			name:   codec.name,
			encode: codec.marshal,
			decode: func(data []byte) error {
				var decoded comparison.BenchmarkCorpus
				err := codec.unmarshal(data, &decoded)
				benchmarkCorpus = &decoded
				return err
			},
		})
	}
	return append(out,
		benchCodec{
			name:   "ColbinJSONText",
			encode: func(v *comparison.BenchmarkCorpus) ([]byte, error) { return colbin.MarshalJSON(v) },
			decode: func(data []byte) (err error) {
				benchmarkBytes, err = colbin.DecodeJSON(data)
				return err
			},
		},
		benchCodec{
			name:   "ColbinJSONValues",
			encode: func(v *comparison.BenchmarkCorpus) ([]byte, error) { return colbin.MarshalJSON(v) },
			decode: func(data []byte) (err error) {
				benchmarkValue, err = colbin.DecodeAny(data)
				return err
			},
		},
	)
}()

var (
	benchmarkBytes  []byte
	benchmarkCorpus *comparison.BenchmarkCorpus
	benchmarkValue  any
)

func BenchmarkEncode(b *testing.B) {
	corpus := comparison.GenerateCorpus(testSeed, benchmarkRecordsPerType)
	for _, codec := range benchCodecs {
		b.Run(codec.name, func(b *testing.B) {
			encoded, err := codec.encode(corpus)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				benchmarkBytes, err = codec.encode(corpus)
				if err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(len(encoded)), "B/payload")
			b.ReportMetric(float64(comparison.ExampleTypeCount*benchmarkRecordsPerType), "models/op")
		})
	}
}

func BenchmarkDecode(b *testing.B) {
	corpus := comparison.GenerateCorpus(testSeed, benchmarkRecordsPerType)
	for _, codec := range benchCodecs {
		b.Run(codec.name, func(b *testing.B) {
			data, err := codec.encode(corpus)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if err := codec.decode(data); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(len(data)), "B/payload")
			b.ReportMetric(float64(comparison.ExampleTypeCount*benchmarkRecordsPerType), "models/op")
		})
	}
}
