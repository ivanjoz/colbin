//go:build arm64 && !purego

#include "textflag.h"

// dictkernel_amd64.s in arm64: the same kernel, step for step. The key is two
// words, so it is an LDP of the string and an LDP of the mask rather than a
// vector load: there is nothing across values for NEON to do, and moving the
// halves out of a vector register would only add to the chain.
//
// Operands are in Go's order: CMP b, a sets the flags of a − b.
//
// R0 the next value, R1 n, R2 the table, R3 mask32, R4 the shift, R5 the next
// id, R6 the masks, R7 the hash's multiplier, R8 the OR of lengths, R9 raw,
// R10 the values left. Per value: R11 the data, R12 the length, R13 lo, R14
// hi, R15 the slot offset, R16 the probes left, R17 the slot, R19 its id+1,
// R20 and R21 scratch.

// func dictKernel(values *string, n int, table *dictShort, mask32 int, shift uint, ids *int64) (done, raw, lengths int)
TEXT ·dictKernel(SB), NOSPLIT, $0-72
	MOVD values+0(FP), R0
	MOVD n+8(FP), R1
	MOVD table+16(FP), R2
	MOVD mask32+24(FP), R3
	MOVD shift+32(FP), R4
	MOVD ids+40(FP), R5
	MOVD $masks<>(SB), R6
	MOVD $0x9e3779b97f4a7c15, R7
	MOVD ZR, R8
	MOVD ZR, R9
	MOVD R1, R10

loop:
	CBZ R10, out
	LDP (R0), (R11, R12)
	CMP $16, R12
	BHI out
	CBZ R12, empty

	// Sixteen bytes from the string's start stay in its page unless it starts
	// in the page's last sixteen; those few go to Go. A page is at least 4 KB.
	AND $0xFFF, R11, R13
	CMP $0xFF0, R13
	BHI out
	LDP (R11), (R13, R14)
	ADD R12<<4, R6, R15
	LDP (R15), (R20, R21)
	AND R20, R13, R13
	AND R21, R14, R14
	B   hash

empty:
	// No bytes, and a pointer that may be nil: the key is zero.
	MOVD ZR, R13
	MOVD ZR, R14

hash:
	// shortHash: (lo ^ rotl(hi, 31) ^ n<<59) * multiplier, and the slot is the
	// product's top bits.
	ROR  $33, R14, R15
	EOR  R13, R15, R15
	LSL  $59, R12, R20
	EOR  R20, R15, R15
	MUL  R7, R15, R15
	LSR  R4, R15, R15
	LSL  $5, R15, R15
	AND  R3, R15, R15
	MOVD $32, R16

probe:
	ADD   R15, R2, R17
	MOVWU 20(R17), R19
	CBZ   R19, out
	LDP   (R17), (R20, R21)
	CMP   R20, R13
	BNE   next
	CMP   R21, R14
	BNE   next
	MOVWU 16(R17), R20
	CMP   R20, R12
	BNE   next

	// Found: the slot holds id+1.
	SUB  $1, R19, R19
	MOVD R19, (R5)
	ORR  R12, R8, R8
	ADD  R12, R9, R9
	ADD  $1, R9, R9
	ADD  $16, R0, R0
	ADD  $8, R5, R5
	SUB  $1, R10, R10
	B    loop

next:
	ADD  $32, R15, R15
	AND  R3, R15, R15
	SUBS $1, R16, R16
	BNE  probe

out:
	SUB  R10, R1, R1
	MOVD R1, done+48(FP)
	MOVD R9, raw+56(FP)
	MOVD R8, lengths+64(FP)
	RET

// masks<> is, for each length 0..16, sixteen bytes of which that many lead
// with 0xFF: the string's sixteen bytes ANDed with it are the string, zero
// past its end.
DATA masks<>+0x000(SB)/8, $0x0000000000000000
DATA masks<>+0x008(SB)/8, $0x0000000000000000
DATA masks<>+0x010(SB)/8, $0x00000000000000ff
DATA masks<>+0x018(SB)/8, $0x0000000000000000
DATA masks<>+0x020(SB)/8, $0x000000000000ffff
DATA masks<>+0x028(SB)/8, $0x0000000000000000
DATA masks<>+0x030(SB)/8, $0x0000000000ffffff
DATA masks<>+0x038(SB)/8, $0x0000000000000000
DATA masks<>+0x040(SB)/8, $0x00000000ffffffff
DATA masks<>+0x048(SB)/8, $0x0000000000000000
DATA masks<>+0x050(SB)/8, $0x000000ffffffffff
DATA masks<>+0x058(SB)/8, $0x0000000000000000
DATA masks<>+0x060(SB)/8, $0x0000ffffffffffff
DATA masks<>+0x068(SB)/8, $0x0000000000000000
DATA masks<>+0x070(SB)/8, $0x00ffffffffffffff
DATA masks<>+0x078(SB)/8, $0x0000000000000000
DATA masks<>+0x080(SB)/8, $0xffffffffffffffff
DATA masks<>+0x088(SB)/8, $0x0000000000000000
DATA masks<>+0x090(SB)/8, $0xffffffffffffffff
DATA masks<>+0x098(SB)/8, $0x00000000000000ff
DATA masks<>+0x0a0(SB)/8, $0xffffffffffffffff
DATA masks<>+0x0a8(SB)/8, $0x000000000000ffff
DATA masks<>+0x0b0(SB)/8, $0xffffffffffffffff
DATA masks<>+0x0b8(SB)/8, $0x0000000000ffffff
DATA masks<>+0x0c0(SB)/8, $0xffffffffffffffff
DATA masks<>+0x0c8(SB)/8, $0x00000000ffffffff
DATA masks<>+0x0d0(SB)/8, $0xffffffffffffffff
DATA masks<>+0x0d8(SB)/8, $0x000000ffffffffff
DATA masks<>+0x0e0(SB)/8, $0xffffffffffffffff
DATA masks<>+0x0e8(SB)/8, $0x0000ffffffffffff
DATA masks<>+0x0f0(SB)/8, $0xffffffffffffffff
DATA masks<>+0x0f8(SB)/8, $0x00ffffffffffffff
DATA masks<>+0x100(SB)/8, $0xffffffffffffffff
DATA masks<>+0x108(SB)/8, $0xffffffffffffffff
GLOBL masks<>(SB), RODATA|NOPTR, $272
