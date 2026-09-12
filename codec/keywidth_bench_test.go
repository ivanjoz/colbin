package codec

// The three framings of a key run, on the same record, straight-line as a
// generator would emit them. This is what decides whether K4 is worth keeping:
// BYTE_ALIGNED_PLAN.md §2.7 leaves it open, and these are the numbers that close
// it.

import (
	"testing"

	"github.com/ivanjoz/colbin/wire"
)

func appendChargeWide(buffer []byte, charge *benchRecord) []byte {
	writer := wire.Writer8{Buffer: buffer}
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

func appendChargeBitmap(buffer []byte, charge *benchRecord) []byte {
	writer := wire.NewBitmapWriter(buffer, 9)
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
	return writer.Buffer()
}

func readChargeWide(message []byte, charge *benchRecord) error {
	*charge = benchRecord{}
	reader := wire.NewReader8(message)
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

func readChargeBitmap(message []byte, charge *benchRecord) error {
	*charge = benchRecord{}
	reader := wire.NewBitmapReader(message)
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

func TestKeyWidthSizes(t *testing.T) {
	narrow := appendChargeByHand(nil, &benchCharge)
	wide := appendChargeWide(nil, &benchCharge)
	bitmap := appendChargeBitmap(nil, &benchCharge)
	t.Logf("five of ten fields set: narrow %d B, wide keys %d B, bitmap %d B",
		len(narrow), len(wide), len(bitmap))

	full := benchCharge
	full.Inference, full.ExtraAllowed = 9, true
	full.Access2, full.Access3, full.Access4 = 11, 12, 13
	t.Logf("ten of ten:             narrow %d B, wide keys %d B, bitmap %d B",
		len(appendChargeByHand(nil, &full)),
		len(appendChargeWide(nil, &full)),
		len(appendChargeBitmap(nil, &full)))

	var back benchRecord
	if err := readChargeWide(wide, &back); err != nil || back != benchCharge {
		t.Fatalf("wide round trip: %+v %v", back, err)
	}
	if err := readChargeBitmap(bitmap, &back); err != nil || back != benchCharge {
		t.Fatalf("bitmap round trip: %+v %v", back, err)
	}
}

func BenchmarkKeyWidthAppendNarrow(b *testing.B) {
	buffer := make([]byte, 0, 64)
	b.ReportAllocs()
	for b.Loop() {
		buffer = appendChargeByHand(buffer[:0], &benchCharge)
	}
}

func BenchmarkKeyWidthAppendWide(b *testing.B) {
	buffer := make([]byte, 0, 64)
	b.ReportAllocs()
	for b.Loop() {
		buffer = appendChargeWide(buffer[:0], &benchCharge)
	}
}

func BenchmarkKeyWidthAppendBitmap(b *testing.B) {
	buffer := make([]byte, 0, 64)
	b.ReportAllocs()
	for b.Loop() {
		buffer = appendChargeBitmap(buffer[:0], &benchCharge)
	}
}

func BenchmarkKeyWidthReadNarrow(b *testing.B) {
	message := appendChargeByHand(nil, &benchCharge)
	var back benchRecord
	b.ReportAllocs()
	for b.Loop() {
		_ = readChargeByHand(message, &back)
	}
}

func BenchmarkKeyWidthReadWide(b *testing.B) {
	message := appendChargeWide(nil, &benchCharge)
	var back benchRecord
	b.ReportAllocs()
	for b.Loop() {
		_ = readChargeWide(message, &back)
	}
}

func BenchmarkKeyWidthReadBitmap(b *testing.B) {
	message := appendChargeBitmap(nil, &benchCharge)
	var back benchRecord
	b.ReportAllocs()
	for b.Loop() {
		_ = readChargeBitmap(message, &back)
	}
}
