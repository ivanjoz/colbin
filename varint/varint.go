// Package varint implements a variable-length integer codec for arrays of
// int8, int16, int32 and int64.
//
// A standard varint spends one continuation flag per byte: 7 payload bits out
// of 8. Two array-wide parameters, both stored in the header and both chosen by
// the encoder, reclaim some of those flags:
//
//	k  the declared minimum encoded length, in bytes. Every value occupies at
//	   least k bytes, so bytes 1..k-1 are guaranteed to be followed by another
//	   byte: their continuation flag is always 1 and carries no information.
//	   Those k-1 leading bytes therefore hold 8 payload bits each.
//
//	M  the declared maximum encoded length, in bytes. No value exceeds M bytes,
//	   so byte M is guaranteed to be the last: its flag is always 0 and is also
//	   reclaimed, giving byte M 8 payload bits.
//
// Bytes k..M-1 are the only ones that keep a flag, since they may or may not be
// final. Capacity for a value occupying exactly l bytes is:
//
//	cap(l) = 8(k-1) + 7(l-k+1)   for k <= l < M
//	cap(M) = 7M + k
//
// Both parameters must be *declared*, not derived from the data. Setting M to
// the max value's ordinary varint length ceil(bmax/7) provably gains nothing:
// it yields cap(M) = M = ceil(bmax/7), exactly the plain-varint length. The
// useful choice is the smallest M with cap(M) >= bmax, i.e. ceil((bmax-k)/7),
// which is one byte shorter whenever bmax is just past a multiple of 7. The
// same argument applies to k. AppendArray searches both jointly.
//
// This file holds the single-value primitives; array.go holds the transforms,
// the parameter search, and the exported array codec.
package varint

import (
	"errors"
	"math/bits"
)

var (
	ErrTruncated     = errors.New("colbin: varint array truncated")
	ErrNegativeCount = errors.New("colbin: negative varint array count")
	ErrShortBuffer   = errors.New("colbin: varint array output slice too short")
)

// zigzag maps signed to unsigned so that small magnitudes stay short:
// 0,-1,1,-2,2 -> 0,1,2,3,4.
func zigzag(v int64) uint64 { return uint64(v<<1) ^ uint64(v>>63) }

// unzigzag inverts zigzag.
func unzigzag(u uint64) int64 { return int64(u>>1) ^ -int64(u&1) }

// capBits returns how many payload bits a value occupying exactly l bytes can
// hold under (k, m). Requires k <= l <= m.
func capBits(l, k, m uint8) uint8 {
	if l == m {
		return 7*m + k
	}
	return 8*(k-1) + 7*(l-k+1)
}

// encLen returns the byte length a value of nbits bits occupies under (k, m),
// or 0 if it does not fit in m bytes. nbits is bits.Len64 of the value, so a
// zero value has nbits 0 and still occupies the k-byte floor.
func encLen(nbits, k, m uint8) uint8 {
	for l := k; l <= m; l++ {
		if nbits <= capBits(l, k, m) {
			return l
		}
	}
	return 0
}

// appendKM writes v under (k, m) and returns out. The caller must have
// established that v fits, i.e. bits.Len64(v) <= capBits(m, k, m); the encoder
// guarantees this by rejecting non-fitting parameter pairs during the search.
func appendKM(out []byte, v uint64, k, m uint8) []byte {
	l := encLen(uint8(bits.Len64(v)), k, m)
	for i := uint8(1); i <= l; i++ {
		if i < k || i == m { // flag-free: forced continuation, or forced terminal
			out = append(out, byte(v))
			v >>= 8
			continue
		}
		b := byte(v) & 0x7F
		v >>= 7
		if i < l {
			b |= 0x80
		}
		out = append(out, b)
	}
	return out
}

// getKM reads one value written by appendKM starting at pos, and
// returns it with the position just past its last byte. A shift beyond 64 bits
// (only reachable from corrupt input) yields zero rather than panicking.
func getKM(buf []byte, pos int, k, m uint8) (uint64, int, error) {
	var v uint64
	var shift uint8
	for i := uint8(1); ; i++ {
		if pos >= len(buf) {
			return 0, pos, ErrTruncated
		}
		b := buf[pos]
		pos++
		switch {
		case i < k: // guaranteed continuation: whole byte is payload
			v |= uint64(b) << shift
			shift += 8
		case i == m: // guaranteed terminal: whole byte is payload
			return v | uint64(b)<<shift, pos, nil
		default:
			v |= uint64(b&0x7F) << shift
			shift += 7
			if b&0x80 == 0 {
				return v, pos, nil
			}
		}
	}
}

// kmParams is one candidate parameter pair plus the payload size it produces.
