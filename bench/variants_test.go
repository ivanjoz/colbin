package bench

// The three ways a caller can hand colbin a struct, against protobuf on the
// same record each time.
//
//	tagged      every field numbered — four-bit keys, the fast path
//	untagged    no tags at all — ids from fnv8 of the name, eight-bit keys
//	packed5     tagged, with the string packing on — eight-bit keys
//
// They are separate benchmarks rather than one parameterised over a flag,
// because the choice is a property of the type: it decides the key width, which
// decides which half of `wire` runs. packed5 is the exception and genuinely is
// a flag, so it is set around the codec that uses it.
//
// The string-heavy record is a second shape on purpose. packed5 has nothing to
// do on a six-field sensor reading whose only string is "C" — it costs the wide
// key and saves nothing, which is why the sensor record gets *larger* with it
// on. A product with a SKU and category names is what the encoding is for.

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/ivanjoz/colbin"
)

// BareReading is Reading with the tags taken off. Same fields, same values; the
// ids now come from the field names.
type BareReading struct {
	SensorID  uint64
	Timestamp int64
	Value     float64
	Unit      string
	Samples   []int32
	Valid     bool
}

// ColbinProduct is the twin of the Product message, minus the attributes map:
// a map is a different cost centre and would swamp what this is measuring.
type ColbinProduct struct {
	ID         uint64   `cb:"1"`
	SKU        string   `cb:"2"`
	Name       string   `cb:"3"`
	PriceCents int64    `cb:"4"`
	Stock      uint32   `cb:"5"`
	Categories []string `cb:"6"`
}

// WideProduct is ColbinProduct with one id past fifteen, which buys the wide key
// without turning packed5 on.
type WideProduct struct {
	ID         uint64   `cb:"1"`
	SKU        string   `cb:"2"`
	Name       string   `cb:"3"`
	PriceCents int64    `cb:"4"`
	Stock      uint32   `cb:"5"`
	Categories []string `cb:"20"`
}

var (
	bareSample = BareReading{
		SensorID:  9124,
		Timestamp: 1767225600123,
		Value:     21.5,
		Unit:      "C",
		Samples:   []int32{21, 22, 21, 23, 24, 22},
		Valid:     true,
	}
	bareCodec = colbin.MustCodec[BareReading]()

	productSample = ColbinProduct{
		ID:         88214,
		SKU:        "ACME-WDG-4471-XL",
		Name:       "ACME WIDGET LARGE",
		PriceCents: 249900,
		Stock:      1420,
		Categories: []string{"HARDWARE", "FASTENERS", "INDUSTRIAL"},
	}
	protoProduct = &Product{
		Id:         88214,
		Sku:        "ACME-WDG-4471-XL",
		Name:       "ACME WIDGET LARGE",
		PriceCents: 249900,
		Stock:      1420,
		Categories: []string{"HARDWARE", "FASTENERS", "INDUSTRIAL"},
	}
	productCodec = colbin.MustCodec[ColbinProduct]()
)

// withPacked5 builds a codec while the flag is on, which is what bakes the wide
// key into its plan, and puts the flag back afterwards.
func withPacked5[T any](b *testing.B, run func(codec *colbin.Codec[T])) {
	colbin.SetPacked5(true)
	codec := colbin.MustCodec[T]()
	b.Cleanup(func() { colbin.SetPacked5(false) })
	b.ResetTimer()
	run(codec)
}

// Sizes, which is the part the key width and packed5 actually move.

func TestVariantSizes(t *testing.T) {
	pb, err := proto.Marshal(protoSample)
	if err != nil {
		t.Fatal(err)
	}
	tagged := readingCodec.Encode(&sample)
	untagged := bareCodec.Encode(&bareSample)

	colbin.SetPacked5(true)
	packedReading := colbin.MustCodec[Reading]().Encode(&sample)
	packedProduct := colbin.MustCodec[ColbinProduct]().Encode(&productSample)
	colbin.SetPacked5(false)

	t.Logf("sensor reading: protobuf %d B · colbin+tags %d B · colbin untagged %d B · colbin+packed5 %d B",
		len(pb), len(tagged), len(untagged), len(packedReading))

	pbProduct, err := proto.Marshal(protoProduct)
	if err != nil {
		t.Fatal(err)
	}
	plainProduct := productCodec.Encode(&productSample)
	// The wide baseline separates the two effects packed5 has: it packs the
	// strings, and it forces the eight-bit key that costs a byte per field.
	wideProduct := colbin.MustCodec[WideProduct]().Encode(&WideProduct{
		ID: productSample.ID, SKU: productSample.SKU, Name: productSample.Name,
		PriceCents: productSample.PriceCents, Stock: productSample.Stock,
		Categories: productSample.Categories,
	})
	t.Logf("string-heavy product: protobuf %d B · colbin+tags %d B · colbin wide, no packing %d B · colbin+packed5 %d B",
		len(pbProduct), len(plainProduct), len(wideProduct), len(packedProduct))
	t.Logf("  → the wide key costs %d B, the packing saves %d B, net %+d B",
		len(wideProduct)-len(plainProduct),
		len(wideProduct)-len(packedProduct),
		len(packedProduct)-len(plainProduct))

	// Every variant has to survive a round trip, or the sizes mean nothing.
	var backTagged Reading
	if err := readingCodec.Unmarshal(tagged, &backTagged); err != nil {
		t.Fatal(err)
	}
	var backBare BareReading
	if err := bareCodec.Unmarshal(untagged, &backBare); err != nil {
		t.Fatal(err)
	}
	if backBare.SensorID != bareSample.SensorID || backBare.Unit != bareSample.Unit ||
		len(backBare.Samples) != 6 || !backBare.Valid {
		t.Fatalf("untagged round-tripped as %+v", backBare)
	}
	var backProduct ColbinProduct
	if err := productCodec.Unmarshal(plainProduct, &backProduct); err != nil {
		t.Fatal(err)
	}
	if backProduct.SKU != productSample.SKU || len(backProduct.Categories) != 3 {
		t.Fatalf("product round-tripped as %+v", backProduct)
	}
}

// The sensor reading, three ways.

func BenchmarkVariantTaggedAppend(b *testing.B) {
	buffer := make([]byte, 0, 128)
	b.ReportAllocs()
	for b.Loop() {
		buffer = readingCodec.Append(buffer[:0], &sample)
	}
}

func BenchmarkVariantUntaggedAppend(b *testing.B) {
	buffer := make([]byte, 0, 128)
	b.ReportAllocs()
	for b.Loop() {
		buffer = bareCodec.Append(buffer[:0], &bareSample)
	}
}

func BenchmarkVariantPacked5Append(b *testing.B) {
	buffer := make([]byte, 0, 128)
	b.ReportAllocs()
	withPacked5(b, func(codec *colbin.Codec[Reading]) {
		for b.Loop() {
			buffer = codec.Append(buffer[:0], &sample)
		}
	})
}

func BenchmarkVariantTaggedUnmarshal(b *testing.B) {
	data := readingCodec.Encode(&sample)
	var into Reading
	b.ReportAllocs()
	for b.Loop() {
		if err := readingCodec.Unmarshal(data, &into); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkVariantUntaggedUnmarshal(b *testing.B) {
	data := bareCodec.Encode(&bareSample)
	var into BareReading
	b.ReportAllocs()
	for b.Loop() {
		if err := bareCodec.Unmarshal(data, &into); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkVariantPacked5Unmarshal(b *testing.B) {
	var into Reading
	b.ReportAllocs()
	withPacked5(b, func(codec *colbin.Codec[Reading]) {
		data := codec.Encode(&sample)
		for b.Loop() {
			if err := codec.Unmarshal(data, &into); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// The string-heavy product, where packed5 has something to do.

func BenchmarkProductProtobufMarshal(b *testing.B) {
	buffer := make([]byte, 0, 256)
	options := proto.MarshalOptions{}
	b.ReportAllocs()
	for b.Loop() {
		var err error
		buffer, err = options.MarshalAppend(buffer[:0], protoProduct)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkProductProtobufUnmarshal(b *testing.B) {
	data, err := proto.Marshal(protoProduct)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		into := &Product{}
		if err := proto.Unmarshal(data, into); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkProductColbinAppend(b *testing.B) {
	buffer := make([]byte, 0, 256)
	b.ReportAllocs()
	for b.Loop() {
		buffer = productCodec.Append(buffer[:0], &productSample)
	}
}

func BenchmarkProductColbinUnmarshal(b *testing.B) {
	data := productCodec.Encode(&productSample)
	var into ColbinProduct
	b.ReportAllocs()
	for b.Loop() {
		if err := productCodec.Unmarshal(data, &into); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkProductPacked5Append(b *testing.B) {
	buffer := make([]byte, 0, 256)
	b.ReportAllocs()
	withPacked5(b, func(codec *colbin.Codec[ColbinProduct]) {
		for b.Loop() {
			buffer = codec.Append(buffer[:0], &productSample)
		}
	})
}

func BenchmarkProductPacked5Unmarshal(b *testing.B) {
	var into ColbinProduct
	b.ReportAllocs()
	withPacked5(b, func(codec *colbin.Codec[ColbinProduct]) {
		data := codec.Encode(&productSample)
		for b.Loop() {
			if err := codec.Unmarshal(data, &into); err != nil {
				b.Fatal(err)
			}
		}
	})
}
