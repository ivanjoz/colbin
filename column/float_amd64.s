//go:build amd64 && !purego

#include "textflag.h"

// The decimal column's conversions (float.go), four float64 lanes to a YMM
// register. AVX2 has no conversion between int64 and float64, so both go
// through decimalMagic, 2^52 + 2^51: a float64 in [2^52, 2^53) is that
// constant plus an integer, in its bits as in its value. The multiply, the
// add and the divide are separate instructions, never fused, which is what
// makes these the same arithmetic as decimalOf.
//
// Operands are in Go's order: VSUBPD b, a, d is d = a - b and VDIVPD b, a, d
// is d = a / b.

#define MAGIC $0x4338000000000000
#define SPAN $0x0008000000000000
#define HIGH $0xFFF0000000000000

// func decimalEncodeVec(vals *int64, n int, power float64, ints *int64, misses *byte)
//
// Y15 the power, Y14 the magic, Y13 2^51, Y12 the bits past the 52nd, Y11
// zero. Per four values: the sum v*power + magic, the integer its bits less
// the magic's, and two tests — that the integer is inside the span, and that
// (sum - magic) / power is v's bits again — whose failures go out as a nibble.
TEXT ·decimalEncodeVec(SB), NOSPLIT, $0-40
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
	// At exponent 0 the multiply and the divide are by one, and change nothing.
	MOVQ         $0x3FF0000000000000, AX
	MOVQ         AX, X10
	UCOMISD      X10, X15
	JEQ          integerEncodeLoop

encodeLoop:
	VMOVDQU   (SI), Y0
	VMULPD    Y15, Y0, Y1
	VADDPD    Y14, Y1, Y2
	VPSUBQ    Y14, Y2, Y3
	VMOVDQU   Y3, (DI)
	VSUBPD    Y14, Y2, Y4
	VDIVPD    Y15, Y4, Y4
	VPCMPEQQ  Y0, Y4, Y5
	VPADDQ    Y13, Y3, Y6
	VPAND     Y12, Y6, Y6
	VPCMPEQQ  Y11, Y6, Y6
	VPAND     Y6, Y5, Y5
	VMOVMSKPD Y5, AX
	XORL      $15, AX
	MOVB      AX, (DX)
	ADDQ      $32, SI
	ADDQ      $32, DI
	INCQ      DX
	SUBQ      $4, CX
	JNZ       encodeLoop
	VZEROUPPER
	RET

integerEncodeLoop:
	VMOVDQU   (SI), Y0
	VADDPD    Y14, Y0, Y2
	VPSUBQ    Y14, Y2, Y3
	VMOVDQU   Y3, (DI)
	VSUBPD    Y14, Y2, Y4
	VPCMPEQQ  Y0, Y4, Y5
	VPADDQ    Y13, Y3, Y6
	VPAND     Y12, Y6, Y6
	VPCMPEQQ  Y11, Y6, Y6
	VPAND     Y6, Y5, Y5
	VMOVMSKPD Y5, AX
	XORL      $15, AX
	MOVB      AX, (DX)
	ADDQ      $32, SI
	ADDQ      $32, DI
	INCQ      DX
	SUBQ      $4, CX
	JNZ       integerEncodeLoop
	VZEROUPPER
	RET

// func decimalDecodeVec(values *int64, n int, power float64) uint64
//
// In place, four integers a turn: magic plus the integer, read as a float64,
// less the magic, over the power. Y8 ORs i + 2^51 together for the span check
// the caller makes.
TEXT ·decimalDecodeVec(SB), NOSPLIT, $0-32
	MOVQ         values+0(FP), SI
	MOVQ         n+8(FP), CX
	VBROADCASTSD power+16(FP), Y15
	MOVQ         MAGIC, AX
	MOVQ         AX, X14
	VPBROADCASTQ X14, Y14
	MOVQ         SPAN, AX
	MOVQ         AX, X13
	VPBROADCASTQ X13, Y13
	VPXOR        Y8, Y8, Y8
	// At exponent 0 the division is by one, which changes nothing: the loop
	// below leaves it out, and the column of integers decodes at the speed of
	// the conversion.
	MOVQ         $0x3FF0000000000000, AX
	MOVQ         AX, X9
	UCOMISD      X9, X15
	JEQ          integerLoop

decodeLoop:
	VMOVDQU (SI), Y0
	VPADDQ  Y13, Y0, Y1
	VPOR    Y1, Y8, Y8
	VPADDQ  Y14, Y0, Y2
	VSUBPD  Y14, Y2, Y2
	VDIVPD  Y15, Y2, Y2
	VMOVUPD Y2, (SI)
	ADDQ    $32, SI
	SUBQ    $4, CX
	JNZ     decodeLoop
	JMP     decodeFold

integerLoop:
	VMOVDQU (SI), Y0
	VPADDQ  Y13, Y0, Y1
	VPOR    Y1, Y8, Y8
	VPADDQ  Y14, Y0, Y2
	VSUBPD  Y14, Y2, Y2
	VMOVUPD Y2, (SI)
	ADDQ    $32, SI
	SUBQ    $4, CX
	JNZ     integerLoop

decodeFold:
	VEXTRACTI128 $1, Y8, X9
	VPOR         X9, X8, X8
	VPSHUFD      $0x4E, X8, X9
	VPOR         X9, X8, X8
	MOVQ         X8, AX
	MOVQ         AX, ret+24(FP)
	VZEROUPPER
	RET
