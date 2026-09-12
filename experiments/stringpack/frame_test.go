package stringpack

// One framing shared by every experimental codec here, so that measured ratios
// are comparable with packed5's and with each other.
//
//	bit  0     packed        0 = raw bytes follow, 1 = packed unit stream
//	bit  1     drop          1 = the payload's last unit is padding
//	bit  2     upper         the stream starts in uppercase mode
//	bits 3-7   length        payload byte length, 31 = uvarint follows
//
// drop needs only one bit: for a payload of P bytes holding U units of width w,
// w*U <= 8P < w*U+8, so floor(8P/w) is U or U+1 and never more, for w of 5 and
// of 6 alike.
//
// upper is free real estate that fell out of that. u5b spends it on a leading
// CASE_TOGGLE_LONG, hoisting the token into the header; codecs that do not want
// it leave it clear.
//
// That is byte-for-byte the same overhead as packed5's header: one byte for any
// payload up to 30 bytes. What changed is the two flag bits. packed5 spends
// them on UPPERCASE_DOMINANT and ENABLE_NUMBER_0_1023, which is what forces its
// planning pass; here they carry the stream terminator instead, which moves the
// three padBits out of the payload and lets the unit grid start at bit 0 of
// byte 0.

import "errors"

var (
	errTruncated = errors.New("stringpack: truncated")
	errBadLength = errors.New("stringpack: bad length prefix")
	errBadUnit   = errors.New("stringpack: bad unit")
)

const (
	hdrPacked   = 1 << 0
	hdrDropShif = 1
	hdrDropMask = 0x1
	hdrUpper    = 1 << 2
	hdrLenShift = 3
	hdrLenInlin = 30
	hdrLenEscap = 31
)

func uvarintLen(v int) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

func appendUvarint(out []byte, v int) []byte {
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}

// frameOverhead is the framing byte count for a payload of n bytes.
func frameOverhead(n int) int {
	if n <= hdrLenInlin {
		return 1
	}
	return 1 + uvarintLen(n)
}

func appendHeader(out []byte, flags byte, payload int) []byte {
	if payload <= hdrLenInlin {
		return append(out, flags|byte(payload)<<hdrLenShift)
	}
	return appendUvarint(append(out, flags|hdrLenEscap<<hdrLenShift), payload)
}

type frameHdr struct {
	payload []byte
	// wide is the payload extended to the end of the caller's buffer. The
	// group readers load a whole word at the last group's offset, so they need
	// up to slack bytes past the payload; back-to-back frames supply that from
	// the next frame, and the caller must pad the final one.
	wide   []byte
	n      int // total frame bytes
	drop   int
	packed bool
	upper  bool
}

func readFrame(buf []byte) (frameHdr, error) {
	if len(buf) == 0 {
		return frameHdr{}, errTruncated
	}
	h := buf[0]
	pos := 1
	length := int(h >> hdrLenShift)
	if length == hdrLenEscap {
		v, shift := 0, uint(0)
		for {
			if pos >= len(buf) || pos > 9 {
				return frameHdr{}, errBadLength
			}
			b := buf[pos]
			pos++
			v |= int(b&0x7F) << shift
			if b < 0x80 {
				break
			}
			shift += 7
		}
		if v <= hdrLenInlin {
			return frameHdr{}, errBadLength
		}
		length = v
	}
	if length > len(buf)-pos {
		return frameHdr{}, errTruncated
	}
	return frameHdr{
		payload: buf[pos : pos+length],
		wide:    buf[pos:],
		n:       pos + length,
		drop:    int(h>>hdrDropShif) & hdrDropMask,
		packed:  h&hdrPacked != 0,
		upper:   h&hdrUpper != 0,
	}, nil
}
