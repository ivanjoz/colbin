//go:build amd64 && !purego

#include "textflag.h"

// ENCODE writes the non-negative value in AX: zero is nothing, one to small−1
// more is the nibble value−1, and the rest the fewest bytes that hold it behind
// a nibble of their count plus three, stored as a whole word that the next
// field lands over. Without a branch: the sized form is always stored, and
// which header and how many of the bytes count is picked by CMOV, because which
// form a value takes is the data's, and a branch on it is mispredicted as often
// as a column of small numbers changes form.
#define ENCODE(small) \
	MOVQ    AX, CX; \
	ORQ     $1, CX; \
	BSRQ    CX, CX; \
	SHRQ    $3, CX; \
	LEAL    4(CX), DX; \
	ORL     R13, DX; \
	LEAQ    2(CX), R14; \
	LEAQ    -1(AX), CX; \
	CMPQ    CX, small; \
	LEAQ    (R13)(CX*1), CX; \
	CMOVQCS CX, DX; \
	MOVL    $1, CX; \
	CMOVQCS CX, R14; \
	XORL    CX, CX; \
	TESTQ   AX, AX; \
	CMOVQEQ CX, R14; \
	MOVB    DX, (DI)(R9*1); \
	MOVQ    AX, 1(DI)(R9*1); \
	ADDQ    R14, R9

// func rowKernel(ops *rowOp, n int, record unsafe.Pointer, out *byte, at, limit, reserve int) (done, end int)
//
// rowProgram.write's loop over the ops it spells, from ops[0] for up to n of
// them: the integer forms, floats, bools and strings, written at out+at. It
// stops at the first op it does not spell — a composite, a field through
// writeFields, a string of more than 0xFFFF bytes — and at a string the room
// up to limit does not hold with reserve to spare, and says how many it did
// and where the bytes end. The caller has reserved reserve bytes at at.
//
// SI the ops, BX n, R8 the record, DI out, R9 at, R10 limit, R11 reserve, R12
// the op; per op DX its kind, R13 its key and R14 the field, then AX the
// value and CX a width or a length.
TEXT ·rowKernel(SB), NOSPLIT, $0-72
	MOVQ ops+0(FP), SI
	MOVQ n+8(FP), BX
	MOVQ record+16(FP), R8
	MOVQ out+24(FP), DI
	MOVQ at+32(FP), R9
	MOVQ limit+40(FP), R10
	MOVQ reserve+48(FP), R11
	XORQ R12, R12

loop:
	CMPQ    R12, BX
	JAE     finish
	MOVBLZX 0(SI)(R12*8), DX
	MOVBLZX 1(SI)(R12*8), R13
	MOVL    4(SI)(R12*8), R14
	ADDQ    R8, R14
	CMPL    DX, $10              // rowBool
	JA      text
	JEQ     boolean
	CMPL    DX, $3               // rowInt64, the last signed
	JBE     signed
	CMPL    DX, $7               // rowUint64, the last unsigned
	JBE     unsigned
	CMPL    DX, $8               // rowFloat32
	JEQ     float32
	MOVQ    (R14), AX
	BSWAPQ  AX
	JMP     uint

float32:
	MOVL   (R14), AX
	BSWAPL AX
	JMP    uint

unsigned:
	CMPL    DX, $5
	JB      uint8
	JEQ     uint16
	CMPL    DX, $6
	JEQ     uint32
	MOVQ    (R14), AX
	JMP     uint

uint8:
	MOVBQZX (R14), AX
	JMP     uint

uint16:
	MOVWQZX (R14), AX
	JMP     uint

uint32:
	MOVL (R14), AX

uint:
	ENCODE($4)
	JMP next

signed:
	CMPL    DX, $1
	JB      int8
	JEQ     int16
	CMPL    DX, $2
	JEQ     int32
	MOVQ    (R14), AX
	JMP     int

int8:
	MOVBQSX (R14), AX
	JMP     int

int16:
	MOVWQSX (R14), AX
	JMP     int

int32:
	MOVLQSX (R14), AX

int:
	// A negative is −1's nibble or its magnitude in the length form.
	TESTQ AX, AX
	JS    negative
	ENCODE($3)
	JMP   next

negative:
	CMPQ AX, $-1
	JEQ  minusOne
	NEGQ AX
	BSRQ AX, CX
	SHRQ $3, CX
	INCQ CX
	MOVL R13, DX
	ORL  $12, DX                 // the length form
	MOVB DX, (DI)(R9*1)
	MOVB CX, 1(DI)(R9*1)
	MOVQ AX, 2(DI)(R9*1)
	LEAQ 2(R9)(CX*1), R9
	JMP  next

minusOne:
	MOVL R13, DX
	ORL  $3, DX
	MOVB DX, (DI)(R9*1)
	INCQ R9
	JMP  next

boolean:
	// The key when true, counted only then: a bool is 0 or 1.
	MOVBLZX (R14), CX
	MOVB    R13, (DI)(R9*1)
	ADDQ    CX, R9
	JMP     next

text:
	CMPL  DX, $11                // rowString; anything past it is the caller's
	JNE   finish
	MOVQ  8(R14), CX             // the length
	TESTQ CX, CX
	JZ    next
	CMPQ  CX, $0xFFFF
	JA    finish
	LEAQ  4(R9)(CX*1), DX        // the header, the value and the room after
	ADDQ  R11, DX
	CMPQ  DX, R10
	JA    finish
	MOVQ  (R14), AX              // the bytes
	LEAQ  (DI)(R9*1), DX         // where the header goes
	CMPQ  CX, $8
	JA    textLength
	LEAL  3(CX), R14             // one to eight: the nibble alone
	ORL   R13, R14
	MOVB  R14, (DX)
	INCQ  DX
	JMP   copy

textLength:
	MOVL R13, R14
	ORL  $12, R14
	MOVB R14, (DX)
	CMPQ CX, $0xFD
	JA   textLength16
	MOVB CX, 1(DX)
	ADDQ $2, DX
	JMP  copy

textLength16:
	MOVB $0xFE, 1(DX)
	MOVW CX, 2(DX)
	ADDQ $4, DX

copy:
	// CX bytes from AX to DX, at least one: two moves that may overlap, as in
	// wire's copyElements, and past 32 a 16-byte loop.
	CMPQ CX, $4
	JB   tiny
	CMPQ CX, $8
	JB   four
	CMPQ CX, $16
	JB   eight
	CMPQ CX, $32
	JA   long
	MOVOU (AX), X0
	MOVOU -16(AX)(CX*1), X1
	MOVOU X0, (DX)
	MOVOU X1, -16(DX)(CX*1)
	JMP   copied

eight:
	MOVQ (AX), R13
	MOVQ -8(AX)(CX*1), R14
	MOVQ R13, (DX)
	MOVQ R14, -8(DX)(CX*1)
	JMP  copied

four:
	MOVL (AX), R13
	MOVL -4(AX)(CX*1), R14
	MOVL R13, (DX)
	MOVL R14, -4(DX)(CX*1)
	JMP  copied

tiny:
	MOVQ    CX, R14
	SHRQ    $1, R14
	MOVBLZX (AX), R13
	MOVB    R13, (DX)
	MOVBLZX (AX)(R14*1), R13
	MOVB    R13, (DX)(R14*1)
	MOVBLZX -1(AX)(CX*1), R13
	MOVB    R13, -1(DX)(CX*1)
	JMP     copied

long:
	XORQ R14, R14

longLoop:
	MOVOU (AX)(R14*1), X0
	MOVOU X0, (DX)(R14*1)
	ADDQ  $16, R14
	LEAQ  16(R14), R13
	CMPQ  R13, CX
	JB    longLoop
	MOVOU -16(AX)(CX*1), X0
	MOVOU X0, -16(DX)(CX*1)

copied:
	LEAQ (DX)(CX*1), R9
	SUBQ DI, R9

next:
	INCQ R12
	JMP  loop

finish:
	MOVQ R12, done+56(FP)
	MOVQ R9, end+64(FP)
	RET

// func rowKernelWide(ops *rowOp, n int, record unsafe.Pointer, out *byte, at, limit, reserve int) (done, end int)
//
// rowKernel at eight key bits: rowProgram.writeWide's loop, as rowKernel is
// write's, with wire's Writer8 forms. Registers as there.
TEXT ·rowKernelWide(SB), NOSPLIT, $0-72
	MOVQ ops+0(FP), SI
	MOVQ n+8(FP), BX
	MOVQ record+16(FP), R8
	MOVQ out+24(FP), DI
	MOVQ at+32(FP), R9
	MOVQ limit+40(FP), R10
	MOVQ reserve+48(FP), R11
	XORQ R12, R12

wloop:
	CMPQ    R12, BX
	JAE     wfinish
	MOVBLZX 0(SI)(R12*8), DX
	MOVBLZX 1(SI)(R12*8), R13
	MOVL    4(SI)(R12*8), R14
	ADDQ    R8, R14
	CMPL    DX, $10
	JA      wtext
	JEQ     wboolean
	CMPL    DX, $3
	JBE     wsigned
	CMPL    DX, $7
	JBE     wunsigned
	CMPL    DX, $8
	JEQ     wfloat32
	MOVQ    (R14), AX
	BSWAPQ  AX
	JMP     wuint

wfloat32:
	MOVL   (R14), AX
	BSWAPL AX
	JMP    wuint

wunsigned:
	CMPL    DX, $5
	JB      wuint8
	JEQ     wuint16
	CMPL    DX, $6
	JEQ     wuint32
	MOVQ    (R14), AX
	JMP     wuint

wuint8:
	MOVBQZX (R14), AX
	JMP     wuint

wuint16:
	MOVWQZX (R14), AX
	JMP     wuint

wuint32:
	MOVL (R14), AX

wuint:
	// Up to 127 is the descriptor itself, and zero nothing. Past that the
	// magnitude is the value, the varint is the value, and the byte-count
	// descriptor is positive.
	CMPQ  AX, $0x80
	JB    wsmall
	MOVQ  AX, DX                 // the varint's value
	MOVL  $0x88, R14             // the byte-count descriptor
	JMP   wnumber

wsigned:
	CMPL    DX, $1
	JB      wint8
	JEQ     wint16
	CMPL    DX, $2
	JEQ     wint32
	MOVQ    (R14), AX
	JMP     wint

wint8:
	MOVBQSX (R14), AX
	JMP     wint

wint16:
	MOVWQSX (R14), AX
	JMP     wint

wint32:
	MOVLQSX (R14), AX

wint:
	CMPQ  AX, $0x80              // unsigned, so a negative is not small
	JB    wsmall
	// The varint is of the zigzagged value; the byte count of the magnitude,
	// negative or positive.
	MOVQ    AX, DX
	SHLQ    $1, DX
	MOVQ    AX, CX
	SARQ    $63, CX
	XORQ    CX, DX
	MOVL    $0x88, R14
	TESTQ   AX, AX
	JNS     wnumber
	NEGQ    AX
	MOVL    $0x80, R14

wnumber:
	// AX the magnitude, DX the varint's value, R14 the descriptor, R13 the key.
	// The width is the magnitude's bytes. The varint is the shorter form when
	// its continuation bytes, seven bits each of what the descriptor's three
	// leave, number fewer than the width: when what is left is zero after
	// 7·(width−1) bits. One byte of magnitude never loses to a varint.
	BSRQ  AX, CX
	SHRQ  $3, CX                 // the width less one
	JZ    wbytes
	CMPQ  CX, $6                 // seven bytes are eight under size code 7
	JB    wshift
	MOVL  $7, CX

wshift:
	IMULQ $7, CX
	MOVQ  DX, BX                 // BX is n: restored from the frame below
	SHRQ  $3, BX
	SHRQ  CX, BX
	TESTQ BX, BX
	MOVQ  n+8(FP), BX
	JNZ   wbytes

	// The varint: the low three bits in the descriptor, then seven at a time.
	MOVB R13, (DI)(R9*1)
	MOVQ DX, CX
	ANDL $7, CX
	ORL  $0xF8, CX
	MOVB CX, 1(DI)(R9*1)
	ADDQ $2, R9
	SHRQ $3, DX

wvarint:
	CMPQ DX, $0x80
	JB   wvarintLast
	MOVL DX, CX
	ORL  $0x80, CX
	MOVB CX, (DI)(R9*1)
	INCQ R9
	SHRQ $7, DX
	JMP  wvarint

wvarintLast:
	MOVB DX, (DI)(R9*1)
	INCQ R9
	JMP  wnext

wbytes:
	// The size code is the width, but 7 for seven or eight bytes, which are
	// then eight.
	BSRQ AX, CX
	SHRQ $3, CX
	INCQ CX
	MOVQ CX, DX
	CMPQ CX, $7
	JB   wcode
	MOVL $7, DX
	MOVL $8, CX

wcode:
	ORL  DX, R14
	MOVB R13, (DI)(R9*1)
	MOVB R14, 1(DI)(R9*1)
	MOVQ AX, 2(DI)(R9*1)
	LEAQ 2(R9)(CX*1), R9
	JMP  wnext

wsmall:
	// The key and the value, counted only when the value is not zero: no
	// branch on which, because a small field's zeros are the data's.
	MOVB    R13, (DI)(R9*1)
	MOVB    AX, 1(DI)(R9*1)
	XORL    CX, CX
	TESTQ   AX, AX
	SETNE   CX
	LEAQ    (R9)(CX*2), R9
	JMP     wnext

wboolean:
	// The key and 1, counted only when true: a bool is 0 or 1.
	MOVBLZX (R14), CX
	MOVB    R13, (DI)(R9*1)
	MOVB    $1, 1(DI)(R9*1)
	LEAQ    (R9)(CX*2), R9
	JMP     wnext

wtext:
	CMPL  DX, $11
	JNE   wfinish
	MOVQ  8(R14), CX
	TESTQ CX, CX
	JZ    wnext
	MOVQ  $0xFFFFFFFF, DX
	CMPQ  CX, DX
	JA    wfinish
	LEAQ  10(R9)(CX*1), DX       // key, descriptor, the length as a word, the value
	ADDQ  R11, DX
	CMPQ  DX, R10
	JA    wfinish
	MOVQ  (R14), AX
	LEAQ  (DI)(R9*1), DX
	MOVB  R13, (DX)
	MOVQ  CX, 2(DX)              // the length, little-endian, as a word
	CMPQ  CX, $0xFF
	JA    wtext16
	MOVB  $0x90, 1(DX)
	ADDQ  $3, DX
	JMP   wcopy

wtext16:
	CMPQ CX, $0xFFFF
	JA   wtext32
	MOVB $0x91, 1(DX)
	ADDQ $4, DX
	JMP  wcopy

wtext32:
	MOVB $0x92, 1(DX)
	ADDQ $6, DX

wcopy:
	CMPQ CX, $4
	JB   wtiny
	CMPQ CX, $8
	JB   wfour
	CMPQ CX, $16
	JB   weight
	CMPQ CX, $32
	JA   wlong
	MOVOU (AX), X0
	MOVOU -16(AX)(CX*1), X1
	MOVOU X0, (DX)
	MOVOU X1, -16(DX)(CX*1)
	JMP   wcopied

weight:
	MOVQ (AX), R13
	MOVQ -8(AX)(CX*1), R14
	MOVQ R13, (DX)
	MOVQ R14, -8(DX)(CX*1)
	JMP  wcopied

wfour:
	MOVL (AX), R13
	MOVL -4(AX)(CX*1), R14
	MOVL R13, (DX)
	MOVL R14, -4(DX)(CX*1)
	JMP  wcopied

wtiny:
	MOVQ    CX, R14
	SHRQ    $1, R14
	MOVBLZX (AX), R13
	MOVB    R13, (DX)
	MOVBLZX (AX)(R14*1), R13
	MOVB    R13, (DX)(R14*1)
	MOVBLZX -1(AX)(CX*1), R13
	MOVB    R13, -1(DX)(CX*1)
	JMP     wcopied

wlong:
	XORQ R14, R14

wlongLoop:
	MOVOU (AX)(R14*1), X0
	MOVOU X0, (DX)(R14*1)
	ADDQ  $16, R14
	LEAQ  16(R14), R13
	CMPQ  R13, CX
	JB    wlongLoop
	MOVOU -16(AX)(CX*1), X0
	MOVOU X0, -16(DX)(CX*1)

wcopied:
	LEAQ (DX)(CX*1), R9
	SUBQ DI, R9

wnext:
	INCQ R12
	JMP  wloop

wfinish:
	MOVQ R12, done+56(FP)
	MOVQ R9, end+64(FP)
	RET
