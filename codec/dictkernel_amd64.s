//go:build amd64 && !purego

#include "textflag.h"

// func dictKernel(values *string, n int, table *dictShort, mask32 int, shift uint, ids *int64) (done, raw, lengths int)
//
// dictColumn.scan's loop for the case that is nearly every row: a string of up
// to sixteen bytes that the dictionary already holds. For each of up to n
// values from values[0] it makes the shortKey — one sixteen-byte load, masked
// to the length — hashes it as shortHash does, probes the short table and
// stores the id in ids. It stops, and leaves the value to Go, at the first one
// it does not take: longer than sixteen bytes, within sixteen bytes of the end
// of a page (where the load could run onto one that is not mapped), not in the
// table, or not found within maxDictProbe slots.
//
// mask32 is the table's slot mask times 32, a slot's size: the probe keeps a
// slot's offset rather than its index. done is how many values it took, raw
// what they cost as strings (each one length byte and its bytes), and lengths
// the OR of their lengths, which is non-zero when any was not empty.
//
// Registers: SI the next value, R8 the values left, R9 the table, R10 mask32,
// CX the shift, DI the next id, R13 the OR of lengths, R14 raw, X2 the hash's
// multiplier. Per value: AX lo, DX hi, BX the length, R11 the slot offset,
// R12 the probes left.
TEXT ·dictKernel(SB), NOSPLIT, $0-72
	MOVQ values+0(FP), SI
	MOVQ n+8(FP), R8
	MOVQ table+16(FP), R9
	MOVQ mask32+24(FP), R10
	MOVQ shift+32(FP), CX
	MOVQ ids+40(FP), DI
	XORQ R13, R13
	XORQ R14, R14
	MOVQ $0x9e3779b97f4a7c15, AX
	MOVQ AX, X2

loop:
	TESTQ R8, R8
	JEQ   out
	MOVQ  0(SI), AX
	MOVQ  8(SI), BX
	CMPQ  BX, $16
	JA    out
	TESTQ BX, BX
	JEQ   empty

	// Sixteen bytes from the string's start stay in its page unless it starts
	// in the page's last sixteen; those few go to Go.
	MOVQ  AX, DX
	ANDQ  $0xFFF, DX
	CMPQ  DX, $0xFF0
	JA    out
	MOVOU (AX), X0
	MOVQ  BX, DX
	SHLQ  $4, DX
	LEAQ  masks<>(SB), R11
	MOVOU (R11)(DX*1), X1
	PAND  X1, X0
	MOVQ  X0, AX
	PSRLDQ $8, X0
	MOVQ  X0, DX
	JMP   hash

empty:
	// No bytes, and a pointer that may be nil: the key is zero.
	XORQ AX, AX
	XORQ DX, DX

hash:
	// shortHash: (lo ^ rotl(hi, 31) ^ n<<59) * multiplier, and the slot is the
	// product's top bits.
	MOVQ  DX, R11
	ROLQ  $31, R11
	XORQ  AX, R11
	MOVQ  BX, R12
	SHLQ  $59, R12
	XORQ  R12, R11
	MOVQ  X2, R12
	IMULQ R12, R11
	SHRQ  CX, R11
	SHLQ  $5, R11
	ANDQ  R10, R11
	MOVQ  $32, R12

probe:
	CMPL 20(R9)(R11*1), $0
	JEQ  out
	CMPQ 0(R9)(R11*1), AX
	JNE  next
	CMPQ 8(R9)(R11*1), DX
	JNE  next
	CMPL 16(R9)(R11*1), BX
	JNE  next

	// Found: the slot holds id+1.
	MOVL 20(R9)(R11*1), R11
	DECL R11
	MOVQ R11, (DI)
	ORQ  BX, R13
	LEAQ 1(R14)(BX*1), R14
	ADDQ $16, SI
	ADDQ $8, DI
	DECQ R8
	JMP  loop

next:
	ADDQ $32, R11
	ANDQ R10, R11
	DECQ R12
	JNE  probe

out:
	MOVQ n+8(FP), AX
	SUBQ R8, AX
	MOVQ AX, done+48(FP)
	MOVQ R14, raw+56(FP)
	MOVQ R13, lengths+64(FP)
	RET

// masks<> is, for each length 0..16, sixteen bytes of which that many lead
// with 0xFF: a sixteen-byte load ANDed with it is the string, zero past its end.
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
