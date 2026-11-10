//go:build amd64 && !purego

#include "textflag.h"

// The float32 decimal column's conversions (float32.go), four values a turn.
// The arithmetic is float_amd64.s's, done in float64 on the widened float32s,
// with the one difference decimalOf32 has from decimalOf: the check narrows the
// quotient to float32 and compares 32-bit patterns. The multiply, the add, the
// divide and the narrowing are separate instructions, never fused, and
// VCVTPD2PS rounds to nearest even as Go's float32(f) does, so these are the
// same arithmetic as the scalar twins.

#define MAGIC $0x4338000000000000
#define SPAN $0x0008000000000000
#define HIGH $0xFFF0000000000000

// func decimalEncodeVec32(vals *float32, n int, power float64, ints *int64, misses *byte)
//
// Y15 the power, Y14 the magic, Y13 2^51, Y12 the bits past the 52nd, Y11 zero.
// Per four values: the four float32s widened to float64, the sum v*power +
// magic, the integer its bits less the magic's, and two tests — that the
// integer is inside the span, and that float32((sum - magic) / power) is v's
// bits again — whose failures go out as a nibble.
TEXT ·decimalEncodeVec32(SB), NOSPLIT, $0-40
	MOVQ         vals+0(FP), SI
	MOVQ         n+8(FP), CX
	VBROADCASTSD power+16(FP), Y15
	MOVQ         ints+24(FP), DI
	MOVQ         misses+32(FP), DX
	MOVQ         MAGIC, AX
	MOVQ         AX, X14
	VPBROADCASTQ X14, Y14
	MOVQ         SPAN, AX
	MOVQ         AX, X13
	VPBROADCASTQ X13, Y13
	MOVQ         HIGH, AX
	MOVQ         AX, X12
	VPBROADCASTQ X12, Y12
	VPXOR        Y11, Y11, Y11

encodeLoop32:
	VMOVDQU    (SI), X0
	VCVTPS2PD  X0, Y1
	VMULPD     Y15, Y1, Y1
	VADDPD     Y14, Y1, Y2
	VPSUBQ     Y14, Y2, Y3
	VMOVDQU    Y3, (DI)
	VSUBPD     Y14, Y2, Y4
	VDIVPD     Y15, Y4, Y4
	VCVTPD2PSY Y4, X4
	VPMOVZXDQ  X4, Y4
	VPMOVZXDQ  X0, Y0
	VPCMPEQQ   Y0, Y4, Y5
	VPADDQ     Y13, Y3, Y6
	VPAND      Y12, Y6, Y6
	VPCMPEQQ   Y11, Y6, Y6
	VPAND      Y6, Y5, Y5
	VMOVMSKPD  Y5, AX
	XORL       $15, AX
	MOVB       AX, (DX)
	ADDQ       $16, SI
	ADDQ       $32, DI
	INCQ       DX
	SUBQ       $4, CX
	JNZ        encodeLoop32
	VZEROUPPER
	RET

// func decimalNarrowVec(src *int64, n int, dst *float32) uint64
//
// Four float64 bit patterns a turn, narrowed to float32. X7 is the float32
// exponent's mask and X8 collects the lanes whose result has all of it set:
// the NaNs and infinities, which the caller handles itself.
TEXT ·decimalNarrowVec(SB), NOSPLIT, $0-32
	MOVQ         src+0(FP), SI
	MOVQ         n+8(FP), CX
	MOVQ         dst+16(FP), DI
	MOVL         $0x7F800000, AX
	MOVQ         AX, X7
	VPBROADCASTD X7, X7
	VPXOR        X8, X8, X8

narrowLoop:
	VMOVUPD    (SI), Y0
	VCVTPD2PSY Y0, X1
	VMOVDQU    X1, (DI)
	VPAND      X7, X1, X2
	VPCMPEQD   X7, X2, X2
	VPOR       X2, X8, X8
	ADDQ       $32, SI
	ADDQ       $16, DI
	SUBQ       $4, CX
	JNZ        narrowLoop
	VMOVMSKPS  X8, AX
	MOVQ       AX, ret+24(FP)
	VZEROUPPER
	RET
