package codec

import (
	"testing"

	"github.com/ivanjoz/colbin/wire"
)

// The ways to write the same record, so the cost of each layer is visible: the
// wire package straight-line (what a generated codec emits), the reflection
// façade, and the façade through a Codec handle with its plan already held.

type benchRecord struct {
	CompanyID    int32  `cb:"1"`
	UserID       int32  `cb:"2"`
	RouteID      uint16 `cb:"3"`
	CPU          uint16 `cb:"4"`
	Inference    uint16 `cb:"5"`
	ExtraAllowed bool   `cb:"6"`
	Access1      uint16 `cb:"7"`
	Access2      uint16 `cb:"8"`
	Access3      uint16 `cb:"9"`
	Access4      uint16 `cb:"10"`
}

var benchCharge = benchRecord{
	CompanyID: 7, UserID: 42, RouteID: 103, CPU: 5, Access1: 0x0139,
}

// appendChargeByHand is what a macro or a generator would emit: one call per
// field, each picking the writer that matches the field's static Go type, and
// the same bytes Marshal writes (TestTheHandWrittenBaselineIsTheFormat).
func appendChargeByHand(buffer []byte, charge *benchRecord) []byte {
	writer := wire.Writer{Buffer: append(buffer, rootStructNarrow)}
	writer.I32(0, charge.CompanyID)
	writer.I32(1, charge.UserID)
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

// A baseline that wrote some other encoding would be measuring something else.
func TestTheHandWrittenBaselineIsTheFormat(t *testing.T) {
	reflected, err := Marshal(&benchCharge)
	if err != nil {
		t.Fatal(err)
	}
	if byHand := appendChargeByHand(nil, &benchCharge); string(byHand) != string(reflected) {
		t.Fatalf("by hand %x, Marshal %x", byHand, reflected)
	}
	var back benchRecord
	if err := readChargeByHand(reflected, &back); err != nil || back != benchCharge {
		t.Fatalf("read by hand: %+v, %v", back, err)
	}
}

func BenchmarkAppendByHand(b *testing.B) {
	buffer := make([]byte, 0, 64)
	b.ReportAllocs()
	for b.Loop() {
		buffer = appendChargeByHand(buffer[:0], &benchCharge)
	}
}

func BenchmarkAppendReflected(b *testing.B) {
	buffer := make([]byte, 0, 64)
	b.ReportAllocs()
	for b.Loop() {
		buffer, _ = Append(buffer[:0], &benchCharge)
	}
}

func BenchmarkMarshal(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_, _ = Marshal(&benchCharge)
	}
}

func BenchmarkUnmarshal(b *testing.B) {
	message, _ := Marshal(&benchCharge)
	back := benchRecord{}
	b.ReportAllocs()
	for b.Loop() {
		_ = Unmarshal(message, &back)
	}
}

func BenchmarkCodecAppend(b *testing.B) {
	codec := MustCodec[benchRecord]()
	buffer := make([]byte, 0, 64)
	b.ReportAllocs()
	for b.Loop() {
		buffer, _ = codec.Append(buffer[:0], &benchCharge)
	}
}

func BenchmarkCodecUnmarshal(b *testing.B) {
	codec := MustCodec[benchRecord]()
	message := must(codec.Encode(&benchCharge))
	back := benchRecord{}
	b.ReportAllocs()
	for b.Loop() {
		_ = codec.Unmarshal(message, &back)
	}
}

// readChargeByHand is the decode half of what a generator would emit.
func readChargeByHand(message []byte, charge *benchRecord) error {
	if len(message) == 0 || message[0] != rootStructNarrow {
		return wire.ErrBadDescriptor
	}
	*charge = benchRecord{}
	reader := wire.NewReader(message[1:])
	for reader.More() {
		switch reader.Key() {
		case 0:
			charge.CompanyID = reader.I32()
		case 1:
			charge.UserID = reader.I32()
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

func BenchmarkReadByHand(b *testing.B) {
	message := appendChargeByHand(nil, &benchCharge)
	back := benchRecord{}
	b.ReportAllocs()
	for b.Loop() {
		_ = readChargeByHand(message, &back)
	}
}
