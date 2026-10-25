//go:build arm64 && !purego

#include "textflag.h"

// The vector loops of vec.go in NEON. Two int64 lanes to a Q register, so a
// vector of four is a pair of them, A and B, each with its own accumulator,
// and a kernel stores both for the four lanes vec.go folds. Every loop runs
// over n values, n a positive multiple of four.
//
// Operands are in Go's order, the reverse of Arm's: VSUB b, a, d is
// d = a - b, VCMGT b, a, d is d = a > b, VUSHL s, a, d is d = a << s, and
// VBIT m, b, d is d = m ? b : d. NEON has no 64-bit minimum, so the minimum is
// a compare and a VBIT; a zigzag's sign is an arithmetic shift by 63.

// func orVec(values *uint64, n int, out *[4]uint64)
TEXT ·orVec(SB), NOSPLIT, $0-24
	MOVD values+0(FP), R0
	MOVD n+8(FP), R1
	MOVD out+16(FP), R2
	VEOR V0.B16, V0.B16, V0.B16
	VEOR V1.B16, V1.B16, V1.B16

orLoop:
	VLD1.P 32(R0), [V4.D2, V5.D2]
	VORR   V4.B16, V0.B16, V0.B16
	VORR   V5.B16, V1.B16, V1.B16
	SUBS   $4, R1, R1
	BNE    orLoop
	VST1   [V0.D2, V1.D2], (R2)
	RET

// func statsVec(values *int64, n int, out *[12]int64)
//
// V0 and V1 the minimum, V16 and V17 the OR of value^first, V18 and V19 the OR
// of subOverflows' word, V3 the first value in both lanes. R3 trails R0 by a
// value.
TEXT ·statsVec(SB), NOSPLIT, $0-24
	MOVD  values+0(FP), R0
	MOVD  n+8(FP), R1
	MOVD  out+16(FP), R2
	VLD1R (R0), [V3.D2]
	VORR  V3.B16, V3.B16, V0.B16
	VORR  V3.B16, V3.B16, V1.B16
	VEOR  V16.B16, V16.B16, V16.B16
	VEOR  V17.B16, V17.B16, V17.B16
	VEOR  V18.B16, V18.B16, V18.B16
	VEOR  V19.B16, V19.B16, V19.B16

	// The first vector has no value before it, so no differences.
	VLD1.P 32(R0), [V4.D2, V5.D2]
	VCMGT  V4.D2, V0.D2, V6.D2
	VBIT   V6.B16, V4.B16, V0.B16
	VCMGT  V5.D2, V1.D2, V7.D2
	VBIT   V7.B16, V5.B16, V1.B16
	VEOR   V3.B16, V4.B16, V6.B16
	VORR   V6.B16, V16.B16, V16.B16
	VEOR   V3.B16, V5.B16, V7.B16
	VORR   V7.B16, V17.B16, V17.B16
	SUBS   $4, R1, R1
	BEQ    statsDone
	SUB    $8, R0, R3

statsLoop:
	VLD1.P 32(R0), [V4.D2, V5.D2]   // a
	VLD1.P 32(R3), [V20.D2, V21.D2] // b, the values one before
	VCMGT  V4.D2, V0.D2, V6.D2      // minimum > a
	VBIT   V6.B16, V4.B16, V0.B16
	VCMGT  V5.D2, V1.D2, V7.D2
	VBIT   V7.B16, V5.B16, V1.B16
	VEOR   V3.B16, V4.B16, V6.B16
	VORR   V6.B16, V16.B16, V16.B16
	VEOR   V3.B16, V5.B16, V7.B16
	VORR   V7.B16, V17.B16, V17.B16
	VSUB   V20.D2, V4.D2, V22.D2    // d = a - b
	VSUB   V21.D2, V5.D2, V23.D2
	VEOR   V20.B16, V4.B16, V24.B16 // a ^ b
	VEOR   V21.B16, V5.B16, V25.B16
	VEOR   V22.B16, V4.B16, V26.B16 // a ^ d
	VEOR   V23.B16, V5.B16, V27.B16
	VAND   V26.B16, V24.B16, V24.B16
	VAND   V27.B16, V25.B16, V25.B16
	VORR   V24.B16, V18.B16, V18.B16
	VORR   V25.B16, V19.B16, V19.B16
	SUBS   $4, R1, R1
	BNE    statsLoop

statsDone:
	VST1.P [V0.D2, V1.D2], 32(R2)
	VST1.P [V16.D2, V17.D2], 32(R2)
	VST1   [V18.D2, V19.D2], (R2)
	RET

// func blockSetsVec(values *int64, n int, base int64, zz bool, out *[8]uint64)
//
// V0 and V1 the OR of the raw residuals, V16 and V17 of the frame ones, V2
// base in both lanes.
TEXT ·blockSetsVec(SB), NOSPLIT, $0-40
	MOVD  values+0(FP), R0
	MOVD  n+8(FP), R1
	MOVD  base+16(FP), R3
	MOVBU zz+24(FP), R4
	MOVD  out+32(FP), R2
	VDUP  R3, V2.D2
	VEOR  V0.B16, V0.B16, V0.B16
	VEOR  V1.B16, V1.B16, V1.B16
	VEOR  V16.B16, V16.B16, V16.B16
	VEOR  V17.B16, V17.B16, V17.B16
	CBNZ  R4, setsZigzag

setsPlain:
	VLD1.P 32(R0), [V4.D2, V5.D2]
	VORR   V4.B16, V0.B16, V0.B16
	VORR   V5.B16, V1.B16, V1.B16
	VSUB   V2.D2, V4.D2, V6.D2
	VSUB   V2.D2, V5.D2, V7.D2
	VORR   V6.B16, V16.B16, V16.B16
	VORR   V7.B16, V17.B16, V17.B16
	SUBS   $4, R1, R1
	BNE    setsPlain
	B      setsDone

setsZigzag:
	VLD1.P 32(R0), [V4.D2, V5.D2]
	VSHL   $1, V4.D2, V6.D2
	VSHL   $1, V5.D2, V7.D2
	VSSHR  $63, V4.D2, V8.D2       // the sign, smeared across the lane
	VSSHR  $63, V5.D2, V9.D2
	VEOR   V8.B16, V6.B16, V6.B16
	VEOR   V9.B16, V7.B16, V7.B16
	VORR   V6.B16, V0.B16, V0.B16
	VORR   V7.B16, V1.B16, V1.B16
	VSUB   V2.D2, V4.D2, V10.D2
	VSUB   V2.D2, V5.D2, V11.D2
	VORR   V10.B16, V16.B16, V16.B16
	VORR   V11.B16, V17.B16, V17.B16
	SUBS   $4, R1, R1
	BNE    setsZigzag

setsDone:
	VST1.P [V0.D2, V1.D2], 32(R2)
	VST1   [V16.D2, V17.D2], (R2)
	RET

// func residualsVec(values *int64, n int, base int64, mode int, dst *uint64, out *[4]uint64)
//
// mode 0 raw, 1 raw zigzagged, 2 frame of reference, 3 delta. V0 and V1 the
// OR of the residuals, V2 base in both lanes.
TEXT ·residualsVec(SB), NOSPLIT, $0-48
	MOVD values+0(FP), R0
	MOVD n+8(FP), R1
	MOVD base+16(FP), R3
	MOVD mode+24(FP), R4
	MOVD dst+32(FP), R5
	MOVD out+40(FP), R2
	VDUP R3, V2.D2
	VEOR V0.B16, V0.B16, V0.B16
	VEOR V1.B16, V1.B16, V1.B16
	CMP  $1, R4
	BEQ  residualsZigzag
	CMP  $2, R4
	BEQ  residualsFrame
	CMP  $3, R4
	BEQ  residualsDelta

residualsRaw:
	VLD1.P 32(R0), [V4.D2, V5.D2]
	VST1.P [V4.D2, V5.D2], 32(R5)
	VORR   V4.B16, V0.B16, V0.B16
	VORR   V5.B16, V1.B16, V1.B16
	SUBS   $4, R1, R1
	BNE    residualsRaw
	B      residualsDone

residualsZigzag:
	VLD1.P 32(R0), [V4.D2, V5.D2]
	VSHL   $1, V4.D2, V6.D2
	VSHL   $1, V5.D2, V7.D2
	VSSHR  $63, V4.D2, V8.D2
	VSSHR  $63, V5.D2, V9.D2
	VEOR   V8.B16, V6.B16, V6.B16
	VEOR   V9.B16, V7.B16, V7.B16
	VST1.P [V6.D2, V7.D2], 32(R5)
	VORR   V6.B16, V0.B16, V0.B16
	VORR   V7.B16, V1.B16, V1.B16
	SUBS   $4, R1, R1
	BNE    residualsZigzag
	B      residualsDone

residualsFrame:
	VLD1.P 32(R0), [V4.D2, V5.D2]
	VSUB   V2.D2, V4.D2, V6.D2
	VSUB   V2.D2, V5.D2, V7.D2
	VST1.P [V6.D2, V7.D2], 32(R5)
	VORR   V6.B16, V0.B16, V0.B16
	VORR   V7.B16, V1.B16, V1.B16
	SUBS   $4, R1, R1
	BNE    residualsFrame
	B      residualsDone

residualsDelta:
	ADD    $8, R0, R3              // the next values
residualsDeltaLoop:
	VLD1.P 32(R3), [V4.D2, V5.D2]
	VLD1.P 32(R0), [V20.D2, V21.D2]
	VSUB   V20.D2, V4.D2, V6.D2
	VSUB   V21.D2, V5.D2, V7.D2
	VSHL   $1, V6.D2, V8.D2
	VSHL   $1, V7.D2, V9.D2
	VSSHR  $63, V6.D2, V10.D2
	VSSHR  $63, V7.D2, V11.D2
	VEOR   V10.B16, V8.B16, V8.B16
	VEOR   V11.B16, V9.B16, V9.B16
	VST1.P [V8.D2, V9.D2], 32(R5)
	VORR   V8.B16, V0.B16, V0.B16
	VORR   V9.B16, V1.B16, V1.B16
	SUBS   $4, R1, R1
	BNE    residualsDeltaLoop

residualsDone:
	VST1 [V0.D2, V1.D2], (R2)
	RET

// func packSmallVec(block *uint64, groups, w int, shifts *[8]uint64, out *byte)
//
// V0 to V3 the shift counts, two lanes each. A group's eight values are
// shifted into place and ORed down to a word, or a word per half; the stores
// are eight or sixteen bytes wide and advance w, so each lands over the zero
// top of the one before.
TEXT ·packSmallVec(SB), NOSPLIT, $0-40
	MOVD block+0(FP), R0
	MOVD groups+8(FP), R1
	MOVD w+16(FP), R2
	MOVD shifts+24(FP), R3
	MOVD out+32(FP), R4
	VLD1 (R3), [V0.D2, V1.D2, V2.D2, V3.D2]
	CMP  $8, R2
	BGT  packHalves

packWord:
	VLD1.P 64(R0), [V4.D2, V5.D2, V6.D2, V7.D2]
	VUSHL  V0.D2, V4.D2, V4.D2
	VUSHL  V1.D2, V5.D2, V5.D2
	VUSHL  V2.D2, V6.D2, V6.D2
	VUSHL  V3.D2, V7.D2, V7.D2
	VORR   V5.B16, V4.B16, V4.B16
	VORR   V7.B16, V6.B16, V6.B16
	VORR   V6.B16, V4.B16, V4.B16
	VMOV   V4.D[0], R5
	VMOV   V4.D[1], R6
	ORR    R6, R5, R5
	MOVD   R5, (R4)
	ADD    R2, R4, R4
	SUBS   $1, R1, R1
	BNE    packWord
	RET

packHalves:
	LSL  $2, R2, R7                // the second half starts at bit 4w
	MOVD $64, R8
	SUB  R7, R8, R8                // and spills past the word from bit 64 - 4w

packHalvesLoop:
	VLD1.P 64(R0), [V4.D2, V5.D2, V6.D2, V7.D2]
	VUSHL  V0.D2, V4.D2, V4.D2
	VUSHL  V1.D2, V5.D2, V5.D2
	VUSHL  V2.D2, V6.D2, V6.D2
	VUSHL  V3.D2, V7.D2, V7.D2
	VORR   V5.B16, V4.B16, V4.B16  // the first half
	VORR   V7.B16, V6.B16, V6.B16  // the second
	VMOV   V4.D[0], R5
	VMOV   V4.D[1], R6
	ORR    R6, R5, R5              // the first half's word
	VMOV   V6.D[0], R9
	VMOV   V6.D[1], R10
	ORR    R10, R9, R9             // the second half's word
	CMP    $64, R7
	BEQ    packSixteen             // at w 16 the halves are whole words
	LSL    R7, R9, R10             // its low bits, over the first half's top
	ORR    R10, R5, R5
	LSR    R8, R9, R9              // and its high bits: R9 >> (64 - 4w)

packSixteen:
	STP    (R5, R9), (R4)
	ADD    R2, R4, R4
	SUBS   $1, R1, R1
	BNE    packHalvesLoop
	RET

// func stepsVec(values *int64, n int, out *[4]uint64)
//
// V0 and V1 the OR of the zigzagged steps. R3 leads R0 by a value.
TEXT ·stepsVec(SB), NOSPLIT, $0-24
	MOVD values+0(FP), R0
	MOVD n+8(FP), R1
	MOVD out+16(FP), R2
	ADD  $8, R0, R3
	VEOR V0.B16, V0.B16, V0.B16
	VEOR V1.B16, V1.B16, V1.B16

stepsLoop:
	VLD1.P 32(R3), [V4.D2, V5.D2]   // the next values
	VLD1.P 32(R0), [V20.D2, V21.D2]
	VSUB   V20.D2, V4.D2, V6.D2     // step = next - value
	VSUB   V21.D2, V5.D2, V7.D2
	VSHL   $1, V6.D2, V8.D2
	VSHL   $1, V7.D2, V9.D2
	VSSHR  $63, V6.D2, V10.D2
	VSSHR  $63, V7.D2, V11.D2
	VEOR   V10.B16, V8.B16, V8.B16
	VEOR   V11.B16, V9.B16, V9.B16
	VORR   V8.B16, V0.B16, V0.B16
	VORR   V9.B16, V1.B16, V1.B16
	SUBS   $4, R1, R1
	BNE    stepsLoop
	VST1   [V0.D2, V1.D2], (R2)
	RET
