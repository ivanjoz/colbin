package bench

// colbin against protocol buffers, on the shape both are for: one flat record,
// encoded and decoded a great many times.
//
// The comparison is deliberately narrow. It is the same fields, the same values
// and the same field numbers on both sides, with protobuf driven through its
// generated code — which is the fast path, not the reflective one — and colbin
// driven three ways: through the reflection façade, through a held Codec handle,
// and through the straight-line calls a generated codec emits.
//
// What it is measuring is the framing. Both formats omit a zero field, both
// write a key per present field, and neither compresses across fields. The
// difference is that protobuf's key and length are varints and colbin's are
// byte-aligned, and that is the whole of what these numbers are about.

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/ivanjoz/colbin"
	"github.com/ivanjoz/colbin/wire"
)

// Reading is the colbin twin of SensorReading: the same six fields with the same
// numbers, so the two encoders are describing the same record.
type Reading struct {
	SensorID  uint64  `cb:"1"`
	Timestamp int64   `cb:"2"`
	Value     float64 `cb:"3"`
	Unit      string  `cb:"4"`
	Samples   []int32 `cb:"5"`
	Valid     bool    `cb:"6"`
}

var (
	sample = Reading{
		SensorID:  9124,
		Timestamp: 1767225600123,
		Value:     21.5,
		Unit:      "C",
		Samples:   []int32{21, 22, 21, 23, 24, 22},
		Valid:     true,
	}
	protoSample = &SensorReading{
		SensorId:        9124,
		TimestampUnixMs: 1767225600123,
		Value:           21.5,
		Unit:            "C",
		Samples:         []int32{21, 22, 21, 23, 24, 22},
		Valid:           true,
	}
	readingCodec = colbin.MustCodec[Reading]()
)

// appendReading is what codec.Generate emits, and what a hot path should run.
func appendReading(dst []byte, v *Reading) []byte {
	w := wire.Writer{Buffer: append(dst, 0xD0)}
	w.Uint(1, v.SensorID)
	w.Int(2, v.Timestamp)
	w.F64(3, v.Value)
	w.String(4, v.Unit)
	w.Int32s(5, v.Samples)
	w.Bool(6, v.Valid)
	return w.Buffer
}

func readReading(data []byte, v *Reading) error {
	*v = Reading{}
	r := wire.NewReader(data[1:])
	for r.More() {
		switch r.Key() {
		case 1:
			v.SensorID = r.Uint()
		case 2:
			v.Timestamp = r.Int()
		case 3:
			v.Value = r.F64()
		case 4:
			v.Unit = r.String()
		case 5:
			v.Samples = r.Int32s(nil)
		case 6:
			v.Valid = r.Bool()
		default:
			r.Skip()
		}
	}
	return r.Err()
}

func TestSizesAndRoundTrip(t *testing.T) {
	pb, err := proto.Marshal(protoSample)
	if err != nil {
		t.Fatal(err)
	}
	viaFacade, err := colbin.Marshal(&sample)
	if err != nil {
		t.Fatal(err)
	}
	straight := appendReading(nil, &sample)
	if string(viaFacade) != string(straight) {
		t.Fatalf("façade %x, straight-line %x", viaFacade, straight)
	}

	colbin.SetPacked5(true)
	packed, err := colbin.Marshal(&sample)
	colbin.SetPacked5(false)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("one sensor reading: protobuf %d B, colbin %d B, colbin+packed5 %d B",
		len(pb), len(viaFacade), len(packed))

	var back Reading
	if err := readReading(straight, &back); err != nil {
		t.Fatal(err)
	}
	if back.SensorID != sample.SensorID || back.Unit != sample.Unit ||
		back.Value != sample.Value || len(back.Samples) != 6 || !back.Valid {
		t.Fatalf("round-tripped as %+v", back)
	}
}

// Encode.

func BenchmarkProtobufMarshal(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := proto.Marshal(protoSample); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkProtobufMarshalReuse(b *testing.B) {
	buffer := make([]byte, 0, 128)
	options := proto.MarshalOptions{}
	b.ReportAllocs()
	for b.Loop() {
		var err error
		buffer, err = options.MarshalAppend(buffer[:0], protoSample)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkColbinMarshal(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := colbin.Marshal(&sample); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkColbinCodecAppend(b *testing.B) {
	buffer := make([]byte, 0, 128)
	b.ReportAllocs()
	for b.Loop() {
		buffer = readingCodec.Append(buffer[:0], &sample)
	}
}

func BenchmarkColbinStraightLine(b *testing.B) {
	buffer := make([]byte, 0, 128)
	b.ReportAllocs()
	for b.Loop() {
		buffer = appendReading(buffer[:0], &sample)
	}
}

// Decode.

func BenchmarkProtobufUnmarshal(b *testing.B) {
	message, _ := proto.Marshal(protoSample)
	back := &SensorReading{}
	b.ReportAllocs()
	for b.Loop() {
		if err := proto.Unmarshal(message, back); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkColbinUnmarshal(b *testing.B) {
	message, _ := colbin.Marshal(&sample)
	var back Reading
	b.ReportAllocs()
	for b.Loop() {
		if err := colbin.Unmarshal(message, &back); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkColbinCodecUnmarshal(b *testing.B) {
	message := readingCodec.Encode(&sample)
	var back Reading
	b.ReportAllocs()
	for b.Loop() {
		if err := readingCodec.Unmarshal(message, &back); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkColbinStraightLineRead(b *testing.B) {
	message := appendReading(nil, &sample)
	var back Reading
	b.ReportAllocs()
	for b.Loop() {
		if err := readReading(message, &back); err != nil {
			b.Fatal(err)
		}
	}
}

// Nested records, which is the shape protobuf is most often used for and the one
// colbin's composites were built to reach.

type ColbinLine struct {
	Discounts []int32 `cb:"1"`
	Quantity  uint32  `cb:"2"`
	Price     int64   `cb:"3"`
}

type ColbinOrder struct {
	OrderID uint64       `cb:"1"`
	Status  string       `cb:"5"`
	Lines   []ColbinLine `cb:"3"`
	Created int64        `cb:"6"`
}

var (
	nestedSample = ColbinOrder{
		OrderID: 918273,
		Status:  "ACME SA",
		Lines: []ColbinLine{
			{Quantity: 2, Price: 1999, Discounts: []int32{1}},
			{Quantity: 1, Price: 450, Discounts: []int32{2}},
			{Quantity: 12, Price: 75, Discounts: []int32{3}},
		},
		Created: 1767225600,
	}
	// The same shape on the protobuf side: an id, a string, three nested lines
	// each with a string, an integer and a money value, and a total.
	protoNested = &Order{
		Id:     918273,
		Status: "ACME SA",
		Lines: []*OrderLine{
			{Quantity: 2, UnitPriceCents: 1999, DiscountBps: []int32{1}},
			{Quantity: 1, UnitPriceCents: 450, DiscountBps: []int32{2}},
			{Quantity: 12, UnitPriceCents: 75, DiscountBps: []int32{3}},
		},
		CreatedUnix: 1767225600,
	}
	orderCodec = colbin.MustCodec[ColbinOrder]()
)

func TestNestedSizes(t *testing.T) {
	pb, err := proto.Marshal(protoNested)
	if err != nil {
		t.Fatal(err)
	}
	cb := orderCodec.Encode(&nestedSample)
	t.Logf("one order with three lines: protobuf %d B, colbin %d B", len(pb), len(cb))

	var back ColbinOrder
	if err := orderCodec.Unmarshal(cb, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Lines) != 3 || back.Lines[2].Quantity != 12 || back.Status != "ACME SA" {
		t.Fatalf("round-tripped as %+v", back)
	}
}

func BenchmarkProtobufNestedMarshal(b *testing.B) {
	buffer := make([]byte, 0, 256)
	options := proto.MarshalOptions{}
	b.ReportAllocs()
	for b.Loop() {
		var err error
		buffer, err = options.MarshalAppend(buffer[:0], protoNested)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkColbinNestedAppend(b *testing.B) {
	buffer := make([]byte, 0, 256)
	b.ReportAllocs()
	for b.Loop() {
		buffer = orderCodec.Append(buffer[:0], &nestedSample)
	}
}

func BenchmarkProtobufNestedUnmarshal(b *testing.B) {
	message, _ := proto.Marshal(protoNested)
	back := &Order{}
	b.ReportAllocs()
	for b.Loop() {
		if err := proto.Unmarshal(message, back); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkColbinNestedUnmarshal(b *testing.B) {
	message := orderCodec.Encode(&nestedSample)
	var back ColbinOrder
	b.ReportAllocs()
	for b.Loop() {
		if err := orderCodec.Unmarshal(message, &back); err != nil {
			b.Fatal(err)
		}
	}
}
