//go:build amd64 && !purego

#include "textflag.h"

// The vector loops of vec.go. Four int64 lanes to a YMM register. Every
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

// func orVec(values *uint64, n int, out *[4]uint64)
TEXT ·orVec(SB), NOSPLIT, $0-24
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

// func statsVec(values *int64, n int, out *[12]int64)
//
// Y0 the minimum, Y1 the OR of value^first, Y2 the OR of subOverflows' word,
// Y3 the first value in every lane. The minimum is a compare and a blend, five
// cycles' latency a vector, so the loop takes two vectors to a turn with a
// second minimum in Y11, folded into Y0 at the end; the ORs are a cycle and
// share theirs.
TEXT ·statsVec(SB), NOSPLIT, $0-24
	MOVQ values+0(FP), SI
	MOVQ n+8(FP), CX
	MOVQ out+16(FP), DI
	VPBROADCASTQ (SI), Y3
	VMOVDQU Y3, Y0
	VMOVDQU Y3, Y11
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
	CMPQ      CX, $8
	JB        statsLoop

statsPairs:
	VMOVDQU   (SI), Y4       // a
	VMOVDQU   32(SI), Y12    // and the vector after it
	VMOVDQU   -8(SI), Y7     // b, the values one before
	VMOVDQU   24(SI), Y13
	VPCMPGTQ  Y4, Y0, Y5     // minimum > a
	VPCMPGTQ  Y12, Y11, Y14
	VPBLENDVB Y5, Y4, Y0, Y0
	VPBLENDVB Y14, Y12, Y11, Y11
	VPXOR     Y3, Y4, Y6
	VPXOR     Y3, Y12, Y15
	VPOR      Y15, Y6, Y6
	VPOR      Y6, Y1, Y1
	VPSUBQ    Y7, Y4, Y8     // d = a - b
	VPSUBQ    Y13, Y12, Y14
	VPXOR     Y7, Y4, Y9     // a ^ b
	VPXOR     Y13, Y12, Y15
	VPXOR     Y8, Y4, Y10    // a ^ d
	VPXOR     Y14, Y12, Y5
	VPAND     Y10, Y9, Y9
	VPAND     Y5, Y15, Y15
	VPOR      Y15, Y9, Y9
	VPOR      Y9, Y2, Y2
	ADDQ      $64, SI
	SUBQ      $8, CX
	CMPQ      CX, $8
	JAE       statsPairs
	TESTQ     CX, CX
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
	VPCMPGTQ  Y11, Y0, Y5
	VPBLENDVB Y5, Y11, Y0, Y0
	VMOVDQU Y0, (DI)
	VMOVDQU Y1, 32(DI)
	VMOVDQU Y2, 64(DI)
	VZEROUPPER
	RET

// func blockSetsVec(values *int64, n int, base int64, zz bool, out *[8]uint64)
//
// Y0 the OR of the raw residuals, Y1 of the frame ones, Y2 base in every
// lane, Y3 zero.
TEXT ·blockSetsVec(SB), NOSPLIT, $0-40
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

// func residualsVec(values *int64, n int, base int64, mode int, dst *uint64, out *[4]uint64)
//
// mode 0 raw, 1 raw zigzagged, 2 frame of reference, 3 delta. Y0 the OR of
// the residuals, Y2 base in every lane, Y3 zero.
TEXT ·residualsVec(SB), NOSPLIT, $0-48
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

// func packSmallVec(block *uint64, groups, w int, shifts *[8]uint64, out *byte)
//
// Y2 the first four values' shift counts, Y3 the last four's. A group's eight
// values are shifted into place and ORed down to a word, or a word per half;
// the stores are eight or sixteen bytes wide and advance w, so each lands over
// the zero top of the one before.
TEXT ·packSmallVec(SB), NOSPLIT, $0-40
	MOVQ    block+0(FP), SI
	MOVQ    groups+8(FP), BX
	MOVQ    w+16(FP), R10
	MOVQ    shifts+24(FP), AX
	MOVQ    out+32(FP), DI
	VMOVDQU (AX), Y2
	VMOVDQU 32(AX), Y3
	CMPQ    R10, $8
	JGT     packHalves

packWord:
	VMOVDQU      (SI), Y4
	VMOVDQU      32(SI), Y5
	VPSLLVQ      Y2, Y4, Y4
	VPSLLVQ      Y3, Y5, Y5
	VPOR         Y5, Y4, Y4
	VEXTRACTI128 $1, Y4, X5
	VPOR         X5, X4, X4
	VPSHUFD      $0x4E, X4, X5
	VPOR         X5, X4, X4
	VMOVQ        X4, (DI)
	ADDQ         $64, SI
	ADDQ         R10, DI
	DECQ         BX
	JNZ          packWord
	JMP          packDone

packHalves:
	MOVQ R10, CX
	SHLQ $2, CX                 // the second half starts at bit 4w

packHalvesLoop:
	VMOVDQU      (SI), Y4
	VMOVDQU      32(SI), Y5
	VPSLLVQ      Y2, Y4, Y4
	VPSLLVQ      Y3, Y5, Y5
	VEXTRACTI128 $1, Y4, X6
	VPOR         X6, X4, X4
	VPSHUFD      $0x4E, X4, X6
	VPOR         X6, X4, X4
	VMOVQ        X4, AX         // the first half's word
	VEXTRACTI128 $1, Y5, X6
	VPOR         X6, X5, X5
	VPSHUFD      $0x4E, X5, X6
	VPOR         X6, X5, X5
	VMOVQ        X5, DX         // the second half's word
	CMPQ         CX, $64
	JEQ          packSixteen
	MOVQ         DX, R8
	SHLQ         CX, R8         // its low bits, over the first half's top
	ORQ          R8, AX
	XORQ         R11, R11
	SHLQ         CX, DX, R11    // and its high bits: DX >> (64 - 4w)
	MOVQ         AX, (DI)
	MOVQ         R11, 8(DI)
	JMP          packHalvesNext

packSixteen:
	MOVQ AX, (DI)               // at w 16 the halves are whole words
	MOVQ DX, 8(DI)

packHalvesNext:
	ADDQ $64, SI
	ADDQ R10, DI
	DECQ BX
	JNZ  packHalvesLoop

packDone:
	VZEROUPPER
	RET

// func scanVec(values *int64, blocks, size int, out *int64)
//
// The one pass of scanSIMD. For each of blocks runs of size values, size a
// positive multiple of four, it stores three words at out: the run's minimum,
// its maximum, and the OR of its zigzagged steps to the value after each, so
// values holds blocks*size+1.
//
// Y0 and Y10 the minimum, Y1 and Y11 the maximum, two of each so that the
// compare-and-blend chains of the two vectors a turn run side by side; Y2 the
// OR of the steps, Y3 zero.
TEXT ·scanVec(SB), NOSPLIT, $0-32
	MOVQ    values+0(FP), SI
	MOVQ    blocks+8(FP), BX
	MOVQ    size+16(FP), R10
	MOVQ    out+24(FP), DI
	VPXOR   Y3, Y3, Y3

scanBlock:
	VPBROADCASTQ (SI), Y0
	VMOVDQU      Y0, Y1
	VMOVDQU      Y0, Y10
	VMOVDQU      Y0, Y11
	VPXOR        Y2, Y2, Y2
	MOVQ         R10, CX
	CMPQ         CX, $8
	JB           scanSingle

scanPairs:
	VMOVDQU   (SI), Y4       // a
	VMOVDQU   32(SI), Y6     // and the vector after a
	VMOVDQU   8(SI), Y8      // the values after a
	VMOVDQU   40(SI), Y9
	VPCMPGTQ  Y4, Y0, Y5     // minimum > a
	VPCMPGTQ  Y6, Y10, Y7
	VPBLENDVB Y5, Y4, Y0, Y0
	VPBLENDVB Y7, Y6, Y10, Y10
	VPCMPGTQ  Y1, Y4, Y5     // a > maximum
	VPCMPGTQ  Y11, Y6, Y7
	VPBLENDVB Y5, Y4, Y1, Y1
	VPBLENDVB Y7, Y6, Y11, Y11
	VPSUBQ    Y4, Y8, Y8     // d = next - a
	VPSUBQ    Y6, Y9, Y9
	VPSLLQ    $1, Y8, Y12    // zigzag(d)
	VPSLLQ    $1, Y9, Y13
	VPCMPGTQ  Y8, Y3, Y8
	VPCMPGTQ  Y9, Y3, Y9
	VPXOR     Y8, Y12, Y12
	VPXOR     Y9, Y13, Y13
	VPOR      Y13, Y12, Y12
	VPOR      Y12, Y2, Y2
	ADDQ      $64, SI
	SUBQ      $8, CX
	CMPQ      CX, $8
	JAE       scanPairs

scanSingle:
	TESTQ     CX, CX
	JZ        scanFold
	VMOVDQU   (SI), Y4
	VMOVDQU   8(SI), Y8
	VPCMPGTQ  Y4, Y0, Y5
	VPBLENDVB Y5, Y4, Y0, Y0
	VPCMPGTQ  Y1, Y4, Y5
	VPBLENDVB Y5, Y4, Y1, Y1
	VPSUBQ    Y4, Y8, Y8
	VPSLLQ    $1, Y8, Y12
	VPCMPGTQ  Y8, Y3, Y8
	VPXOR     Y8, Y12, Y12
	VPOR      Y12, Y2, Y2
	ADDQ      $32, SI

scanFold:
	// The two minimums and maximums into one, then their four lanes into
	// one, a half and then a quarter at a time; the steps' OR likewise.
	VPCMPGTQ     Y10, Y0, Y8
	VPBLENDVB    Y8, Y10, Y0, Y0
	VPCMPGTQ     Y1, Y11, Y8
	VPBLENDVB    Y8, Y11, Y1, Y1
	VEXTRACTI128 $1, Y0, X4
	VPCMPGTQ     X4, X0, X8
	VPBLENDVB    X8, X4, X0, X0
	VPSHUFD      $0x4E, X0, X4
	VPCMPGTQ     X4, X0, X8
	VPBLENDVB    X8, X4, X0, X0
	VMOVQ        X0, (DI)
	VEXTRACTI128 $1, Y1, X4
	VPCMPGTQ     X1, X4, X8
	VPBLENDVB    X8, X4, X1, X1
	VPSHUFD      $0x4E, X1, X4
	VPCMPGTQ     X1, X4, X8
	VPBLENDVB    X8, X4, X1, X1
	VMOVQ        X1, 8(DI)
	VEXTRACTI128 $1, Y2, X4
	VPOR         X4, X2, X2
	VPSHUFD      $0x4E, X2, X4
	VPOR         X4, X2, X2
	VMOVQ        X2, 16(DI)
	ADDQ         $24, DI
	DECQ         BX
	JNZ          scanBlock

	VZEROUPPER
	RET

// func stepsVec(values *int64, n int, out *[4]uint64)
//
// Y0 the OR of the zigzagged steps, Y3 zero.
TEXT ·stepsVec(SB), NOSPLIT, $0-24
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
