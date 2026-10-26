//go:build arm64 && !purego

#include "textflag.h"

// rows_amd64.s in arm64: the same two kernels, op for op, with CLZ for BSR
// and CSEL for CMOV. Scalar throughout; there is nothing here for NEON.
//
// Operands are in Go's order: CMP b, a sets the flags of a − b, and
// CSEL cond, x, y, d is d = cond ? x : y.
//
// R0 the ops, R1 n, R2 the record, R3 out, R4 at, R5 limit, R6 reserve, R7
// the op; per op R8 its kind, R9 its key and R10 the field, then R11 the value
// and R12 to R17, R19 and R20 scratch.

// ENCODE writes the non-negative value in R11 at four key bits: zero is
// nothing, one to small−1 more the nibble value−1, and the rest the fewest
// bytes that hold it behind a nibble of their count plus three, stored as a
// whole word that the next field lands over. Without a branch, as on amd64.
#define ENCODE(small) \
	ORR  $1, R11, R12; \
	CLZ  R12, R12; \
	MOVD $63, R13; \
	SUB  R12, R13, R12; \
	LSR  $3, R12, R12; \
	ADD  $4, R12, R13; \
	ORR  R9, R13, R13; \
	ADD  $2, R12, R14; \
	SUB  $1, R11, R15; \
	ORR  R9, R15, R16; \
	CMP  small, R15; \
	CSEL LO, R16, R13, R13; \
	MOVD $1, R17; \
	CSEL LO, R17, R14, R14; \
	CMP  $0, R11; \
	CSEL EQ, ZR, R14, R14; \
	ADD  R4, R3, R16; \
	MOVB R13, (R16); \
	MOVD R11, 1(R16); \
	ADD  R14, R4, R4

// COPY copies R12 bytes, at least one, from R11 to R16, and leaves at in R4
// past them: two moves that may overlap, as in wire's copyElements, and past
// 32 a 16-byte loop. The labels are the caller's, prefixed.
#define COPY(tiny, four, eight, long, loop, copied) \
	CMP  $4, R12; \
	BLO  tiny; \
	CMP  $8, R12; \
	BLO  four; \
	CMP  $16, R12; \
	BLO  eight; \
	CMP  $32, R12; \
	BHI  long; \
	ADD  R12, R11, R13; \
	ADD  R12, R16, R14; \
	LDP  (R11), (R15, R17); \
	LDP  -16(R13), (R19, R20); \
	STP  (R15, R17), (R16); \
	STP  (R19, R20), -16(R14); \
	B    copied; \
eight: \
	ADD  R12, R11, R13; \
	ADD  R12, R16, R14; \
	MOVD (R11), R15; \
	MOVD -8(R13), R17; \
	MOVD R15, (R16); \
	MOVD R17, -8(R14); \
	B    copied; \
four: \
	ADD   R12, R11, R13; \
	ADD   R12, R16, R14; \
	MOVWU (R11), R15; \
	MOVWU -4(R13), R17; \
	MOVW  R15, (R16); \
	MOVW  R17, -4(R14); \
	B     copied; \
tiny: \
	LSR   $1, R12, R13; \
	MOVBU (R11), R15; \
	MOVB  R15, (R16); \
	ADD   R13, R11, R14; \
	MOVBU (R14), R15; \
	ADD   R13, R16, R14; \
	MOVB  R15, (R14); \
	ADD   R12, R11, R14; \
	MOVBU -1(R14), R15; \
	ADD   R12, R16, R14; \
	MOVB  R15, -1(R14); \
	B     copied; \
long: \
	MOVD ZR, R13; \
loop: \
	ADD  R13, R11, R14; \
	LDP  (R14), (R15, R17); \
	ADD  R13, R16, R14; \
	STP  (R15, R17), (R14); \
	ADD  $16, R13; \
	ADD  $16, R13, R14; \
	CMP  R12, R14; \
	BLO  loop; \
	ADD  R12, R11, R13; \
	LDP  -16(R13), (R15, R17); \
	ADD  R12, R16, R14; \
	STP  (R15, R17), -16(R14); \
copied: \
	ADD  R12, R16, R4; \
	SUB  R3, R4, R4

// LOAD reads the op at R7: its kind into R8, its key into R9, and the field's
// address into R10.
#define LOAD \
	ADD   R7<<3, R0, R12; \
	MOVBU (R12), R8; \
	MOVBU 1(R12), R9; \
	MOVWU 4(R12), R10; \
	ADD   R2, R10, R10

// func rowKernel(ops *rowOp, n int, record unsafe.Pointer, out *byte, at, limit, reserve int) (done, end int)
TEXT ·rowKernel(SB), NOSPLIT, $0-72
	MOVD ops+0(FP), R0
	MOVD n+8(FP), R1
	MOVD record+16(FP), R2
	MOVD out+24(FP), R3
	MOVD at+32(FP), R4
	MOVD limit+40(FP), R5
	MOVD reserve+48(FP), R6
	MOVD ZR, R7

loop:
	CMP  R1, R7
	BHS  finish
	LOAD
	CMP  $10, R8                 // rowBool
	BHI  text
	BEQ  boolean
	ADD  $10, R4, R12            // the most a number stores, its whole word too
	CMP  R5, R12
	BHI  finish
	CMP  $3, R8                  // rowInt64, the last signed
	BLS  signed
	CMP  $7, R8                  // rowUint64, the last unsigned
	BLS  unsigned
	CMP  $8, R8                  // rowFloat32
	BEQ  float32
	MOVD (R10), R11
	REV  R11, R11
	B    uint

float32:
	MOVWU (R10), R11
	REVW  R11, R11
	B     uint

unsigned:
	CMP  $5, R8
	BLO  uint8
	BEQ  uint16
	CMP  $6, R8
	BEQ  uint32
	MOVD (R10), R11
	B    uint

uint8:
	MOVBU (R10), R11
	B     uint

uint16:
	MOVHU (R10), R11
	B     uint

uint32:
	MOVWU (R10), R11

uint:
	ENCODE($4)
	B next

signed:
	CMP  $1, R8
	BLO  int8
	BEQ  int16
	CMP  $2, R8
	BEQ  int32
	MOVD (R10), R11
	B    int

int8:
	MOVB (R10), R11
	B    int

int16:
	MOVH (R10), R11
	B    int

int32:
	MOVW (R10), R11

int:
	// A negative is −1's nibble or its magnitude in the length form.
	CMP $0, R11
	BLT negative
	ENCODE($3)
	B   next

negative:
	CMN  $1, R11
	BEQ  minusOne
	NEG  R11, R11
	CLZ  R11, R12
	MOVD $63, R13
	SUB  R12, R13, R12
	LSR  $3, R12, R12
	ADD  $1, R12, R12            // the width
	ORR  $12, R9, R13            // the length form
	ADD  R4, R3, R16
	MOVB R13, (R16)
	MOVB R12, 1(R16)
	MOVD R11, 2(R16)
	ADD  $2, R12, R12
	ADD  R12, R4, R4
	B    next

minusOne:
	ORR  $3, R9, R13
	ADD  R4, R3, R16
	MOVB R13, (R16)
	ADD  $1, R4, R4
	B    next

boolean:
	// The key when true, counted only then: a bool is 0 or 1.
	CMP   R5, R4
	BHS   finish
	MOVBU (R10), R12
	ADD   R4, R3, R16
	MOVB  R9, (R16)
	ADD   R12, R4, R4
	B     next

text:
	CMP  $11, R8                 // rowString
	BHI  list
	MOVD 8(R10), R12             // the length
	CBZ  R12, next
	MOVD $0xFFFF, R13
	CMP  R13, R12
	BHI  finish
	ADD  R4, R12, R13            // the header, the value and the room after
	ADD  $4, R13, R13
	ADD  R6, R13, R13
	CMP  R5, R13
	BHI  finish
	MOVD (R10), R11              // the bytes
	ADD  R4, R3, R16             // where the header goes
	CMP  $8, R12
	BHI  textLength
	ADD  $3, R12, R13            // one to eight: the nibble alone
	ORR  R9, R13, R13
	MOVB R13, (R16)
	ADD  $1, R16, R16
	B    copy

textLength:
	ORR  $12, R9, R13
	MOVB R13, (R16)
	CMP  $0xFD, R12
	BHI  textLength16
	MOVB R12, 1(R16)
	ADD  $2, R16, R16
	B    copy

textLength16:
	MOVD $0xFE, R13
	MOVB R13, 1(R16)
	MOVH R12, 2(R16)
	ADD  $4, R16, R16

copy:
	COPY(tiny, four, eight, long, longLoop, copied)

next:
	ADD $1, R7, R7
	B   loop

finish:
	MOVD R7, done+56(FP)
	MOVD R4, end+64(FP)
	RET

list:
	// An []int32, as on amd64: the width from the OR of the values folded to
	// their magnitude, the length form, the length in bytes, the elements.
	CMP  $16, R8                 // rowPointer: a nil one is nothing, any
	BNE  int32s                  // other the caller's
	MOVD (R10), R12
	CBZ  R12, next
	B    finish

int32s:
	CMP  $12, R8                 // rowInt32s; anything past it is the caller's
	BNE  finish
	MOVD 8(R10), R12             // the count
	CBZ  R12, next
	MOVD (R10), R11              // the elements
	MOVD ZR, R13
	MOVD R11, R14
	MOVD R12, R15

fold:
	MOVW (R14), R16
	EOR  R16->63, R16, R16
	ORR  R16, R13, R13
	ADD  $4, R14, R14
	SUB  $1, R15, R15
	CBNZ R15, fold
	LSL  $1, R13, R13            // the sign bit back
	MOVD R12, R14                // the length in bytes
	MOVD $12, R15                // the length form, one-byte elements
	CMP  $0xFF, R13
	BLS  listHead
	ADD  R12, R14, R14
	MOVD $13, R15                // two-byte elements
	MOVD $0xFFFF, R16
	CMP  R16, R13
	BLS  listHead
	ADD  R14, R14, R14
	MOVD $14, R15                // four-byte elements

listHead:
	// R14 the length in bytes, R15 the header's low nibble.
	MOVD $0xFFFF, R16
	CMP  R16, R14
	BHI  finish
	ADD  R4, R14, R16
	ADD  $4, R16, R16
	ADD  R6, R16, R16
	CMP  R5, R16
	BHI  finish
	ORR  R9, R15, R17
	ADD  R4, R3, R16
	MOVB R17, (R16)
	CMP  $0xFD, R14
	BHI  listLength16
	MOVB R14, 1(R16)
	ADD  $2, R16, R16
	B    listBody

listLength16:
	MOVD $0xFE, R19
	MOVB R19, 1(R16)
	MOVH R14, 2(R16)
	ADD  $4, R16, R16

listBody:
	// R11 the elements, R12 the count, R16 where they go, R15 the width's nibble.
	CMP $13, R15
	BLO list1
	BEQ list2

list4:
	MOVWU (R11), R17
	MOVW  R17, (R16)
	ADD   $4, R11, R11
	ADD   $4, R16, R16
	SUB   $1, R12, R12
	CBNZ  R12, list4
	B     listDone

list2:
	MOVWU (R11), R17
	MOVH  R17, (R16)
	ADD   $4, R11, R11
	ADD   $2, R16, R16
	SUB   $1, R12, R12
	CBNZ  R12, list2
	B     listDone

list1:
	MOVWU (R11), R17
	MOVB  R17, (R16)
	ADD   $4, R11, R11
	ADD   $1, R16, R16
	SUB   $1, R12, R12
	CBNZ  R12, list1

listDone:
	SUB R3, R16, R4
	B   next

// func rowKernelWide(ops *rowOp, n int, record unsafe.Pointer, out *byte, at, limit, reserve int) (done, end int)
//
// rowKernel at eight key bits, as in rows_amd64.s.
TEXT ·rowKernelWide(SB), NOSPLIT, $0-72
	MOVD ops+0(FP), R0
	MOVD n+8(FP), R1
	MOVD record+16(FP), R2
	MOVD out+24(FP), R3
	MOVD at+32(FP), R4
	MOVD limit+40(FP), R5
	MOVD reserve+48(FP), R6
	MOVD ZR, R7

wloop:
	CMP  R1, R7
	BHS  wfinish
	LOAD
	CMP  $10, R8
	BHI  wtext
	BEQ  wboolean
	CMP  $3, R8
	BLS  wsigned
	CMP  $7, R8
	BLS  wunsigned
	CMP  $8, R8
	BEQ  wfloat32
	MOVD (R10), R11
	REV  R11, R11
	B    wuint

wfloat32:
	MOVWU (R10), R11
	REVW  R11, R11
	B     wuint

wunsigned:
	CMP  $5, R8
	BLO  wuint8
	BEQ  wuint16
	CMP  $6, R8
	BEQ  wuint32
	MOVD (R10), R11
	B    wuint

wuint8:
	MOVBU (R10), R11
	B     wuint

wuint16:
	MOVHU (R10), R11
	B     wuint

wuint32:
	MOVWU (R10), R11

wuint:
	// Up to 127 is the descriptor itself, and zero nothing. Past that the
	// magnitude is the value, the varint is the value, and the byte-count
	// descriptor is positive.
	CMP  $0x80, R11
	BLO  wsmall
	MOVD R11, R19                // the varint's value
	MOVD $0x88, R20              // the byte-count descriptor
	B    wnumber

wsigned:
	CMP  $1, R8
	BLO  wint8
	BEQ  wint16
	CMP  $2, R8
	BEQ  wint32
	MOVD (R10), R11
	B    wint

wint8:
	MOVB (R10), R11
	B    wint

wint16:
	MOVH (R10), R11
	B    wint

wint32:
	MOVW (R10), R11

wint:
	CMP  $0x80, R11              // unsigned, so a negative is not small
	BLO  wsmall
	// The varint is of the zigzagged value; the byte count of the magnitude,
	// negative or positive.
	LSL  $1, R11, R19
	ASR  $63, R11, R12
	EOR  R12, R19, R19
	MOVD $0x88, R20
	CMP  $0, R11
	BGE  wnumber
	NEG  R11, R11
	MOVD $0x80, R20

wnumber:
	// R11 the magnitude, R19 the varint's value, R20 the descriptor. The
	// varint is the shorter form when what it leaves past the descriptor's
	// three bits is zero after 7·(width−1) bits; seven bytes of magnitude are
	// eight, and one byte never loses to a varint.
	CLZ  R11, R12
	MOVD $63, R13
	SUB  R12, R13, R12
	LSR  $3, R12, R12            // the width less one
	CBZ  R12, wbytes
	MOVD $7, R13
	CMP  $6, R12
	CSEL HS, R13, R12, R12
	MUL  R13, R12, R12           // 7·(width−1)
	LSR  $3, R19, R14
	LSR  R12, R14, R14
	CBNZ R14, wbytes

	// The varint: the low three bits in the descriptor, then seven at a time.
	ADD  R4, R3, R16
	MOVB R9, (R16)
	AND  $7, R19, R13
	ORR  $0xF8, R13, R13
	MOVB R13, 1(R16)
	ADD  $2, R4, R4
	LSR  $3, R19, R19

wvarint:
	CMP  $0x80, R19
	BLO  wvarintLast
	ORR  $0x80, R19, R13
	ADD  R4, R3, R16
	MOVB R13, (R16)
	ADD  $1, R4, R4
	LSR  $7, R19, R19
	B    wvarint

wvarintLast:
	ADD  R4, R3, R16
	MOVB R19, (R16)
	ADD  $1, R4, R4
	B    wnext

wbytes:
	// The size code is the width, but 7 for seven or eight bytes, which are
	// then eight.
	CLZ  R11, R12
	MOVD $63, R13
	SUB  R12, R13, R12
	LSR  $3, R12, R12
	ADD  $1, R12, R12            // the width
	MOVD $7, R13
	CMP  $7, R12
	CSEL HS, R13, R12, R14       // the code
	MOVD $8, R13
	CSEL HS, R13, R12, R12       // and the width it means
	ORR  R14, R20, R20
	ADD  R4, R3, R16
	MOVB R9, (R16)
	MOVB R20, 1(R16)
	MOVD R11, 2(R16)
	ADD  $2, R12, R12
	ADD  R12, R4, R4
	B    wnext

wsmall:
	// The key and the value, counted only when the value is not zero.
	ADD  R4, R3, R16
	MOVB R9, (R16)
	MOVB R11, 1(R16)
	CMP  $0, R11
	CSET NE, R12
	ADD  R12<<1, R4, R4
	B    wnext

wboolean:
	// The key and 1, counted only when true: a bool is 0 or 1.
	MOVBU (R10), R12
	ADD   R4, R3, R16
	MOVB  R9, (R16)
	MOVD  $1, R13
	MOVB  R13, 1(R16)
	ADD   R12<<1, R4, R4
	B     wnext

wtext:
	CMP  $16, R8                 // rowPointer: a nil one is nothing
	BNE  wstring
	MOVD (R10), R12
	CBZ  R12, wnext
	B    wfinish

wstring:
	CMP  $11, R8
	BNE  wfinish
	MOVD 8(R10), R12
	CBZ  R12, wnext
	MOVD $0xFFFFFFFF, R13
	CMP  R13, R12
	BHI  wfinish
	ADD  R4, R12, R13            // key, descriptor, the length as a word, the value
	ADD  $10, R13, R13
	ADD  R6, R13, R13
	CMP  R5, R13
	BHI  wfinish
	MOVD (R10), R11
	ADD  R4, R3, R16
	MOVB R9, (R16)
	MOVD R12, 2(R16)             // the length, little-endian, as a word
	CMP  $0xFF, R12
	BHI  wtext16
	MOVD $0x90, R13
	MOVB R13, 1(R16)
	ADD  $3, R16, R16
	B    wcopy

wtext16:
	MOVD $0xFFFF, R13
	CMP  R13, R12
	BHI  wtext32
	MOVD $0x91, R13
	MOVB R13, 1(R16)
	ADD  $4, R16, R16
	B    wcopy

wtext32:
	MOVD $0x92, R13
	MOVB R13, 1(R16)
	ADD  $6, R16, R16

wcopy:
	COPY(wtiny, wfour, weight, wlong, wlongLoop, wcopied)

wnext:
	ADD $1, R7, R7
	B   wloop

wfinish:
	MOVD R7, done+56(FP)
	MOVD R4, end+64(FP)
	RET
