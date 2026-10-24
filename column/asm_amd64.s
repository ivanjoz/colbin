//go:build amd64 && !purego && !goexperiment.simd

#include "textflag.h"

// The vector loops of asm_amd64.go. Four int64 lanes to a YMM register. Every
// loop runs over n values, n a positive multiple of four, and stores its
// accumulators for the Go side to fold. VZEROUPPER on the way out, so that the
// SSE code after them pays no transition.
//
// Operands are in Go's order, the reverse of Intel's: VPSUBQ b, a, d is
// d = a - b, VPCMPGTQ b, a, d is d = a > b, and VPBLENDVB m, b, a, d is
// d = m ? b : a.

// func cpuid(leaf, subleaf uint32) (eax, ebx, ecx, edx uint32)
TEXT ·cpuid(SB), NOSPLIT, $0-24
	MOVL leaf+0(FP), AX
	MOVL subleaf+4(FP), CX
	CPUID
	MOVL AX, eax+8(FP)
	MOVL BX, ebx+12(FP)
	MOVL CX, ecx+16(FP)
	MOVL DX, edx+20(FP)
	RET

// func xgetbv() uint32
TEXT ·xgetbv(SB), NOSPLIT, $0-4
	MOVL $0, CX
	XGETBV
	MOVL AX, ret+0(FP)
	RET

// func orAVX2(values *uint64, n int, out *[4]uint64)
TEXT ·orAVX2(SB), NOSPLIT, $0-24
	MOVQ values+0(FP), SI
	MOVQ n+8(FP), CX
	MOVQ out+16(FP), DI
	VPXOR Y0, Y0, Y0

orLoop:
	VPOR  (SI), Y0, Y0
	ADDQ  $32, SI
	SUBQ  $4, CX
	JNZ   orLoop
	VMOVDQU Y0, (DI)
	VZEROUPPER
	RET

// func statsAVX2(values *int64, n int, out *[12]int64)
//
// Y0 the minimum, Y1 the OR of value^first, Y2 the OR of subOverflows' word,
// Y3 the first value in every lane.
TEXT ·statsAVX2(SB), NOSPLIT, $0-24
	MOVQ values+0(FP), SI
	MOVQ n+8(FP), CX
	MOVQ out+16(FP), DI
	VPBROADCASTQ (SI), Y3
	VMOVDQU Y3, Y0
	VPXOR   Y1, Y1, Y1
	VPXOR   Y2, Y2, Y2

	// The first vector has no value before it, so no differences.
	VMOVDQU   (SI), Y4
	VPCMPGTQ  Y4, Y0, Y5
	VPBLENDVB Y5, Y4, Y0, Y0
	VPXOR     Y3, Y4, Y6
	VPOR      Y6, Y1, Y1
	ADDQ      $32, SI
	SUBQ      $4, CX
	JZ        statsDone

statsLoop:
	VMOVDQU   (SI), Y4       // a
	VMOVDQU   -8(SI), Y7     // b, the values one before
	VPCMPGTQ  Y4, Y0, Y5     // minimum > a
	VPBLENDVB Y5, Y4, Y0, Y0
	VPXOR     Y3, Y4, Y6
	VPOR      Y6, Y1, Y1
	VPSUBQ    Y7, Y4, Y8     // d = a - b
	VPXOR     Y7, Y4, Y9     // a ^ b
	VPXOR     Y8, Y4, Y10    // a ^ d
	VPAND     Y10, Y9, Y9
	VPOR      Y9, Y2, Y2
	ADDQ      $32, SI
	SUBQ      $4, CX
	JNZ       statsLoop

statsDone:
	VMOVDQU Y0, (DI)
	VMOVDQU Y1, 32(DI)
	VMOVDQU Y2, 64(DI)
	VZEROUPPER
	RET

// func blockSetsAVX2(values *int64, n int, base int64, zz bool, out *[8]uint64)
//
// Y0 the OR of the raw residuals, Y1 of the frame ones, Y2 base in every
// lane, Y3 zero.
TEXT ·blockSetsAVX2(SB), NOSPLIT, $0-40
	MOVQ    values+0(FP), SI
	MOVQ    n+8(FP), CX
	MOVQ    base+16(FP), AX
	MOVQ    out+32(FP), DI
	MOVQ    AX, X2
	VPBROADCASTQ X2, Y2
	VPXOR   Y0, Y0, Y0
	VPXOR   Y1, Y1, Y1
	VPXOR   Y3, Y3, Y3
	CMPB    zz+24(FP), $0
	JNE     setsZigzag

setsPlain:
	VMOVDQU (SI), Y4
	VPOR    Y4, Y0, Y0
	VPSUBQ  Y2, Y4, Y5
	VPOR    Y5, Y1, Y1
	ADDQ    $32, SI
	SUBQ    $4, CX
	JNZ     setsPlain
	JMP     setsDone

setsZigzag:
	VMOVDQU  (SI), Y4
	VPSLLQ   $1, Y4, Y6
	VPCMPGTQ Y4, Y3, Y7      // 0 > v: the sign, smeared across the lane
	VPXOR    Y7, Y6, Y6
	VPOR     Y6, Y0, Y0
	VPSUBQ   Y2, Y4, Y5
	VPOR     Y5, Y1, Y1
	ADDQ     $32, SI
	SUBQ     $4, CX
	JNZ      setsZigzag

setsDone:
	VMOVDQU Y0, (DI)
	VMOVDQU Y1, 32(DI)
	VZEROUPPER
	RET

// func residualsAVX2(values *int64, n int, base int64, mode int, dst *uint64, out *[4]uint64)
//
// mode 0 raw, 1 raw zigzagged, 2 frame of reference, 3 delta. Y0 the OR of
// the residuals, Y2 base in every lane, Y3 zero.
TEXT ·residualsAVX2(SB), NOSPLIT, $0-48
	MOVQ    values+0(FP), SI
	MOVQ    n+8(FP), CX
	MOVQ    base+16(FP), AX
	MOVQ    mode+24(FP), BX
	MOVQ    dst+32(FP), DI
	MOVQ    out+40(FP), DX
	MOVQ    AX, X2
	VPBROADCASTQ X2, Y2
	VPXOR   Y0, Y0, Y0
	VPXOR   Y3, Y3, Y3
	CMPQ    BX, $1
	JEQ     residualsZigzag
	CMPQ    BX, $2
	JEQ     residualsFrame
	CMPQ    BX, $3
	JEQ     residualsDelta

residualsRaw:
	VMOVDQU (SI), Y4
	VMOVDQU Y4, (DI)
	VPOR    Y4, Y0, Y0
	ADDQ    $32, SI
	ADDQ    $32, DI
	SUBQ    $4, CX
	JNZ     residualsRaw
	JMP     residualsDone

residualsZigzag:
	VMOVDQU  (SI), Y4
	VPSLLQ   $1, Y4, Y6
	VPCMPGTQ Y4, Y3, Y7
	VPXOR    Y7, Y6, Y6
	VMOVDQU  Y6, (DI)
	VPOR     Y6, Y0, Y0
	ADDQ     $32, SI
	ADDQ     $32, DI
	SUBQ     $4, CX
	JNZ      residualsZigzag
	JMP      residualsDone

residualsFrame:
	VMOVDQU (SI), Y4
	VPSUBQ  Y2, Y4, Y5
	VMOVDQU Y5, (DI)
	VPOR    Y5, Y0, Y0
	ADDQ    $32, SI
	ADDQ    $32, DI
	SUBQ    $4, CX
	JNZ     residualsFrame
	JMP     residualsDone

residualsDelta:
	VMOVDQU  8(SI), Y4
	VMOVDQU  (SI), Y5
	VPSUBQ   Y5, Y4, Y6
	VPSLLQ   $1, Y6, Y7
	VPCMPGTQ Y6, Y3, Y8
	VPXOR    Y8, Y7, Y7
	VMOVDQU  Y7, (DI)
	VPOR     Y7, Y0, Y0
	ADDQ     $32, SI
	ADDQ     $32, DI
	SUBQ     $4, CX
	JNZ      residualsDelta

residualsDone:
	VMOVDQU Y0, (DX)
	VZEROUPPER
	RET

// func stepsAVX2(values *int64, n int, out *[4]uint64)
//
// Y0 the OR of the zigzagged steps, Y3 zero.
TEXT ·stepsAVX2(SB), NOSPLIT, $0-24
	MOVQ    values+0(FP), SI
	MOVQ    n+8(FP), CX
	MOVQ    out+16(FP), DI
	VPXOR   Y0, Y0, Y0
	VPXOR   Y3, Y3, Y3

stepsLoop:
	VMOVDQU  8(SI), Y4       // the next values
	VMOVDQU  (SI), Y5
	VPSUBQ   Y5, Y4, Y6      // step = next - value
	VPSLLQ   $1, Y6, Y7
	VPCMPGTQ Y6, Y3, Y8
	VPXOR    Y8, Y7, Y7
	VPOR     Y7, Y0, Y0
	ADDQ     $32, SI
	SUBQ     $4, CX
	JNZ      stepsLoop
	VMOVDQU  Y0, (DI)
	VZEROUPPER
	RET
