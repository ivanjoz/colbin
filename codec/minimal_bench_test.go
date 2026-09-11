package codec

import (
	"testing"

	"github.com/ivanjoz/colbin/minimal"
)

// The three ways to write the same record, so the cost of each layer is visible:
// the wire package straight-line (what a generated codec emits), the reflection
// façade over a cached plan, and compact mode through its typed handle.

type minimalBenchCharge struct {
	CompanyID    int32  `cb:"0"`
	UserID       int32  `cb:"1"`
	RouteID      uint16 `cb:"2"`
	CPU          uint16 `cb:"3"`
	Inference    uint16 `cb:"4"`
	ExtraAllowed bool   `cb:"5"`
	Access1      uint16 `cb:"6"`
	Access2      uint16 `cb:"7"`
	Access3      uint16 `cb:"8"`
	Access4      uint16 `cb:"9"`
}

var benchCharge = minimalBenchCharge{
	CompanyID: 7, UserID: 42, RouteID: 103, CPU: 5, Access1: 0x0139,
}

// appendChargeByHand is what a macro or a generator would emit: one call per
// field, each picking the writer that matches the field's static Go type.
func appendChargeByHand(buffer []byte, charge *minimalBenchCharge) []byte {
	writer := minimal.Writer{Buffer: buffer}
	writer.U32(0, uint32(charge.CompanyID))
	writer.U32(1, uint32(charge.UserID))
	writer.U16(2, charge.RouteID)
	writer.U16(3, charge.CPU)
	writer.U16(4, charge.Inference)
	writer.Bool(5, charge.ExtraAllowed)
	writer.U16(6, charge.Access1)
	writer.U16(7, charge.Access2)
	writer.U16(8, charge.Access3)
	writer.U16(9, charge.Access4)
	return writer.Buffer
}

func BenchmarkMinimalAppendByHand(b *testing.B) {
	buffer := make([]byte, 0, 64)
	b.ReportAllocs()
	for b.Loop() {
		buffer = appendChargeByHand(buffer[:0], &benchCharge)
	}
}

func BenchmarkMinimalAppendReflected(b *testing.B) {
	buffer := make([]byte, 0, 64)
	b.ReportAllocs()
	for b.Loop() {
		buffer, _ = AppendMinimal(buffer[:0], &benchCharge)
	}
}

func BenchmarkMinimalMarshal(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_, _ = MarshalMinimal(&benchCharge)
	}
}

func BenchmarkMinimalUnmarshal(b *testing.B) {
	message, _ := MarshalMinimal(&benchCharge)
	back := minimalBenchCharge{}
	b.ReportAllocs()
	for b.Loop() {
		_ = UnmarshalMinimal(message, &back)
	}
}

func BenchmarkMinimalCompactForComparison(b *testing.B) {
	codec := MustCodec[minimalBenchCharge]()
	buffer := make([]byte, 0, 64)
	b.ReportAllocs()
	for b.Loop() {
		buffer, _ = codec.Append(buffer[:0], &benchCharge)
	}
}

func BenchmarkMinimalCompactUnmarshalForComparison(b *testing.B) {
	codec := MustCodec[minimalBenchCharge]()
	message, _ := codec.Append(nil, &benchCharge)
	back := minimalBenchCharge{}
	b.ReportAllocs()
	for b.Loop() {
		_ = codec.Unmarshal(message, &back)
	}
}

func TestMinimalBenchSizes(t *testing.T) {
	byHand := appendChargeByHand(nil, &benchCharge)
	reflected, err := MarshalMinimal(&benchCharge)
	if err != nil {
		t.Fatal(err)
	}
	if len(byHand) != len(reflected) {
		t.Fatalf("hand-written wrote %d bytes, reflected wrote %d", len(byHand), len(reflected))
	}
	compact, err := MustCodec[minimalBenchCharge]().Append(nil, &benchCharge)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("minimal %d B · compact %d B", len(reflected), len(compact))
}

func BenchmarkMinimalCodecAppend(b *testing.B) {
	codec := MustMinimalCodec[minimalBenchCharge]()
	buffer := make([]byte, 0, 64)
	b.ReportAllocs()
	for b.Loop() {
		buffer = codec.Append(buffer[:0], &benchCharge)
	}
}

func BenchmarkMinimalCodecUnmarshal(b *testing.B) {
	codec := MustMinimalCodec[minimalBenchCharge]()
	message := codec.Encode(&benchCharge)
	back := minimalBenchCharge{}
	b.ReportAllocs()
	for b.Loop() {
		_ = codec.Unmarshal(message, &back)
	}
}

// readChargeByHand is the decode half of what a generator would emit.
func readChargeByHand(message []byte, charge *minimalBenchCharge) error {
	*charge = minimalBenchCharge{}
	reader := minimal.NewReader(message)
	for reader.More() {
		switch reader.Key() {
		case 0:
			charge.CompanyID = int32(reader.U32())
		case 1:
			charge.UserID = int32(reader.U32())
		case 2:
			charge.RouteID = reader.U16()
		case 3:
			charge.CPU = reader.U16()
		case 4:
			charge.Inference = reader.U16()
		case 5:
			charge.ExtraAllowed = reader.Bool()
		case 6:
			charge.Access1 = reader.U16()
		case 7:
			charge.Access2 = reader.U16()
		case 8:
			charge.Access3 = reader.U16()
		case 9:
			charge.Access4 = reader.U16()
		default:
			reader.Skip()
		}
	}
	return reader.Err()
}

func BenchmarkMinimalReadByHand(b *testing.B) {
	message := appendChargeByHand(nil, &benchCharge)
	back := minimalBenchCharge{}
	b.ReportAllocs()
	for b.Loop() {
		_ = readChargeByHand(message, &back)
	}
}
