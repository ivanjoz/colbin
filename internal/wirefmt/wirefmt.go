// Package wirefmt holds the narrow field format's writers as functions over a
// byte slice: what wire.Writer's methods do, with the buffer passed in and
// handed back rather than held behind a pointer.
//
// It exists for codec's field walks, which keep the buffer in a local across a
// record's fields. Through a *Writer, every field loads the buffer from memory
// and stores it back; over a slice it stays in registers. The format itself is
// documented in wire/narrow.go, and wire's methods are these functions, so
// there is one implementation of each.
//
// The functions are shaped for the inliner as wire's methods were: the common
// case inline, and the rest one call away in a //go:noinline cold half, so that
// the hot half stays inside the budget wherever it is called from.
package wirefmt

import (
	"encoding/binary"
	"errors"
	"math"
	"math/bits"
)

// The nibble. See wire/narrow.go.
const (
	// NibbleSized is the first nibble with a payload, which is
	// nibble−SizedBias bytes long, up to MaxSized at nibble 11.
	NibbleSized = 4
	SizedBias   = 3
	MaxSized    = 8
	// NibbleLength is the length form, 0b11xx. Its low two bits are flags whose
	// meaning is the type's: they never change the size.
	NibbleLength uint8 = 0b1100

	// An unsigned field's nibbles 0..3 are the values 1..4; a signed field's
	// are 1, 2 and 3, and then −1.
	InlineValues   = 4
	SignedInline   = 3
	SignedMinusOne = 3

	// A length: one byte up to 253, and two escapes naming the width of the
	// length that follows.
	InlineLength = 0xFD
	Length16     = 0xFE
	Length32     = 0xFF

	// FlagRaw is a raw string's or blob's length-form flags.
	FlagRaw uint8 = 0b00
)

// ErrFieldTooLarge is a writer's one refusal, and a panic rather than an error:
// a narrow length is at most four bytes, and a payload past that is a wide
// field's job.
var ErrFieldTooLarge = errors.New("wire: a narrow field holds at most 2^32-1 bytes")

// TableGroupRows is the rows past which codec writes a table field in groups,
// and the rows each group holds (wire/groups.go). A reader takes the group size
// from the message, so this is the writer's choice and not the format's; it is
// a variable only so that the vector generators can write a grouped table small
// enough to check in.
var TableGroupRows = 4096

// AppendMagnitude writes a magnitude's low width bytes, little-endian.
//
// It stores all eight and cuts back rather than switching on the width: two
// instructions against a jump table. Storing exactly `width` bytes through a
// switch was tried and measured 30.5 ns against 23.0 on a six-field record — the
// jump table costs more than the discarded stores, and the extra capacity is
// wanted anyway on the buffer this is usually appending to.
func AppendMagnitude(buffer []byte, magnitude uint64, width int) []byte {
	buffer = binary.LittleEndian.AppendUint64(buffer, magnitude)
	return buffer[:len(buffer)-(8-width)]
}

// AppendSized writes a header whose nibble sizes a magnitude of the fewest
// bytes that hold it, and the magnitude. The nibble is the width plus
// SizedBias, so the byte count is exact at every width from one to eight.
func AppendSized(buffer []byte, key uint8, value uint64) []byte {
	width := (bits.Len64(value) + 7) / 8
	return AppendMagnitude(append(buffer, key<<4|uint8(width+SizedBias)), value, width)
}

// AppendLength writes a length in the shortest of its three forms.
//
// A length past four bytes is a programming error rather than data: the value
// is already in memory, and a narrow field cannot say how long it is.
func AppendLength(buffer []byte, length int) []byte {
	switch {
	case length <= InlineLength:
		return append(buffer, uint8(length))
	case length <= 0xFFFF:
		return append(buffer, Length16, uint8(length), uint8(length>>8))
	case uint64(length) <= math.MaxUint32:
		return binary.LittleEndian.AppendUint32(append(buffer, Length32), uint32(length))
	}
	panic(ErrFieldTooLarge)
}

// AppendUint writes an unsigned integer, and nothing at all when it is zero.
//
// The field that fits its own nibble is inline and everything else is a call,
// deliberately: a value of one to four is what a flag, a small count or a bool
// holds, and keeping that path inside the inliner is worth more than the branch
// it costs the rest. One compare covers both the inline range and the
// omit-zero rule, because zero wraps: decremented, it is not below four, so a
// zero falls through to the cold half and is dropped there. The decrement is
// spelled in place, rather than as `value-1` in the compare and again in the
// append, because the inliner charges for each. A Uint that did not inline
// put every field of a ten-field record through a call: 18.9 ns against 7.0.
func AppendUint(buffer []byte, key uint8, value uint64) []byte {
	if value--; value < InlineValues {
		return append(buffer, key<<4|uint8(value))
	}
	return AppendUintWide(buffer, key, value)
}

// AppendUintWide is AppendUint's cold half, out of line so that it does not
// fold back in and push the hot half out of the budget. It takes the value
// AppendUint decremented, which saves a node undoing it.
//
//go:noinline
func AppendUintWide(buffer []byte, key uint8, less uint64) []byte {
	if value := less + 1; value != 0 {
		return AppendSized(buffer, key, value)
	}
	return buffer
}

// AppendInt writes a signed integer: a positive value as an unsigned one would
// be written, past the three that fit the nibble, and a negative one as its
// magnitude in the length form — except −1, which has a nibble of its own.
//
// It does not fit the inliner whichever way it is spelled — the signed range
// test costs a conversion the unsigned one does not — so it is one call that
// keeps every value up to 255 inside it, rather than an inline nibble case in
// front of a second call. The one-byte magnitude is tested first: it is the
// commonest value a signed field holds that is not zero, and putting it ahead
// of the nibble case measured a third of a nanosecond a field.
func AppendInt(buffer []byte, key uint8, value int64) []byte {
	if value > SignedInline && value <= 0xFF {
		return append(buffer, key<<4|NibbleSized, uint8(value))
	}
	if uint64(value)-1 < SignedInline {
		return append(buffer, key<<4|uint8(value-1))
	}
	// A zero is the commonest value a field holds, and it is dropped here
	// rather than one call further in.
	if value != 0 {
		return AppendIntWide(buffer, key, value)
	}
	return buffer
}

// AppendIntWide is every signed value AppendInt does not keep inline: past 255,
// and below −1.
func AppendIntWide(buffer []byte, key uint8, value int64) []byte {
	switch {
	case value > 0:
		return AppendSized(buffer, key, uint64(value))
	case value == -1:
		return append(buffer, key<<4|SignedMinusOne)
	case value < 0:
		// Negating through uint64 rather than int64 keeps math.MinInt64, whose
		// positive counterpart does not exist as an int64.
		magnitude := -uint64(value)
		width := (bits.Len64(magnitude) + 7) / 8
		return AppendMagnitude(append(buffer, key<<4|NibbleLength, uint8(width)), magnitude, width)
	}
	return buffer
}

// AppendI32 writes a signed field no wider than four bytes, as AppendInt does.
// It is AppendInt spelled again at 32 bits rather than AppendInt behind a
// widening, which inlined and still measured slower: the same one call, with
// the comparisons at the width the field already has.
func AppendI32(buffer []byte, key uint8, value int32) []byte {
	if value > SignedInline && value <= 0xFF {
		return append(buffer, key<<4|NibbleSized, uint8(value))
	}
	if uint32(value)-1 < SignedInline {
		return append(buffer, key<<4|uint8(value-1))
	}
	if value != 0 {
		return AppendIntWide(buffer, key, int64(value))
	}
	return buffer
}

// AppendU16 writes a field whose type cannot exceed two bytes: a uint16 is one
// byte or two and never three, so it is two appends with no call under them.
func AppendU16(buffer []byte, key uint8, value uint16) []byte {
	if value == 0 {
		return buffer
	}
	if value <= InlineValues {
		return append(buffer, key<<4|uint8(value-1))
	}
	if value <= 0xFF {
		return append(buffer, key<<4|NibbleSized, uint8(value))
	}
	return append(buffer, key<<4|NibbleSized+1, uint8(value), uint8(value>>8))
}

// AppendU32 writes a field whose type cannot exceed four bytes. It is shaped
// like AppendUint and for the same reason: one compare, one append and one
// call is all the inliner's budget holds.
func AppendU32(buffer []byte, key uint8, value uint32) []byte {
	if value--; value < InlineValues {
		return append(buffer, key<<4|uint8(value))
	}
	return AppendU32Wide(buffer, key, value)
}

// AppendU32Wide takes the value AppendU32 decremented, as AppendUintWide takes
// AppendUint's.
//
//go:noinline
func AppendU32Wide(buffer []byte, key uint8, less uint32) []byte {
	switch value := less + 1; {
	case value == 0:
		return buffer
	case value <= 0xFF:
		return append(buffer, key<<4|NibbleSized, uint8(value))
	case value <= 0xFFFF:
		return append(buffer, key<<4|NibbleSized+1, uint8(value), uint8(value>>8))
	case value <= 0xFF_FFFF:
		return append(buffer, key<<4|NibbleSized+2,
			uint8(value), uint8(value>>8), uint8(value>>16))
	default:
		return append(buffer, key<<4|NibbleSized+3,
			uint8(value), uint8(value>>8), uint8(value>>16), uint8(value>>24))
	}
}

// AppendF32 writes a float32 as its bit pattern with the bytes reversed, in the
// unsigned shape, so that its low zero mantissa bytes land where the trim
// removes them. See wire/narrow.go.
func AppendF32(buffer []byte, key uint8, value float32) []byte {
	return AppendUint(buffer, key, uint64(bits.ReverseBytes32(math.Float32bits(value))))
}

// AppendF64 writes a float64 as AppendF32 writes a float32.
func AppendF64(buffer []byte, key uint8, value float64) []byte {
	return AppendUint(buffer, key, bits.ReverseBytes64(math.Float64bits(value)))
}

// AppendBool writes one byte when true and nothing when false: true is the
// unsigned value 1, nibble 0, which is a whole field in one byte.
func AppendBool(buffer []byte, key uint8, value bool) []byte {
	if !value {
		return buffer
	}
	return append(buffer, key<<4)
}

// AppendBytes writes a raw blob, and nothing when it is empty.
func AppendBytes(buffer []byte, key uint8, value []byte) []byte {
	if len(value) == 0 {
		return buffer
	}
	return append(AppendBlobHeader(buffer, key, len(value)), value...)
}

// AppendString is AppendBytes for a string, which Go appends without copying
// it first.
func AppendString(buffer []byte, key uint8, value string) []byte {
	if len(value) == 0 {
		return buffer
	}
	return append(AppendBlobHeader(buffer, key, len(value)), value...)
}

// AppendBlobHeader writes a raw string's header: the nibble alone for one to
// eight bytes — a code, a short name, a currency — and the length form past
// that. It is out of line so that AppendString and AppendBytes stay inside the
// inliner's budget themselves: an append and a call is what fits.
//
//go:noinline
func AppendBlobHeader(buffer []byte, key uint8, size int) []byte {
	if size <= MaxSized {
		return append(buffer, key<<4|uint8(size+SizedBias))
	}
	return AppendLength(append(buffer, key<<4|NibbleLength|FlagRaw), size)
}
