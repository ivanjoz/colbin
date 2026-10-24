//go:build amd64 && !purego

#include "textflag.h"

// func copyElements(dst *byte, values *string, n int) int
//
// SI walks the string headers, a pointer and a length each, DI the output.
// Every copy is two moves that may overlap, the first from the value's start
// and the second ending at its end, so nothing is read or written outside it:
// 1 to 3 bytes as three single bytes, then 4, 8 and 16-byte pairs, and past
// 32 a 16-byte loop with an overlapping last move.
//
// The prefetch is for the value 32 on, which at a few nanoseconds a value is
// about a miss ahead. On TPC-H's lineitem it was 14% of the encode against no
// prefetch and 8 values ahead was short of it; 64 measured the same as 32.
// Prefetching an empty value's nil pointer does not fault.
TEXT ·copyElements(SB), NOSPLIT, $0-32
	MOVQ dst+0(FP), DI
	MOVQ values+8(FP), SI
	MOVQ n+16(FP), BX
	MOVQ DI, R10                // the start, for the count of bytes written

next:
	MOVQ (SI), R8               // the value's bytes
	MOVQ 8(SI), R9              // and its length
	CMPQ BX, $32
	JLE  size
	MOVQ 512(SI), R12           // the bytes of the value 32 on
	PREFETCHT0 (R12)

size:
	CMPQ R9, $0xFE
	JA   escape
	MOVB R9, (DI)
	INCQ DI
	JMP  copy

escape:
	MOVB $0xFF, (DI)
	MOVL R9, 1(DI)
	ADDQ $5, DI

copy:
	CMPQ R9, $4
	JB   tiny
	CMPQ R9, $8
	JB   four
	CMPQ R9, $16
	JB   eight
	CMPQ R9, $32
	JA   long
	MOVOU (R8), X0
	MOVOU -16(R8)(R9*1), X1
	MOVOU X0, (DI)
	MOVOU X1, -16(DI)(R9*1)
	JMP   done

eight:
	MOVQ (R8), AX
	MOVQ -8(R8)(R9*1), CX
	MOVQ AX, (DI)
	MOVQ CX, -8(DI)(R9*1)
	JMP  done

four:
	MOVL (R8), AX
	MOVL -4(R8)(R9*1), CX
	MOVL AX, (DI)
	MOVL CX, -4(DI)(R9*1)
	JMP  done

tiny:
	TESTQ   R9, R9
	JZ      done
	MOVQ    R9, DX
	SHRQ    $1, DX              // the middle byte: the second of 2 or 3
	MOVBLZX (R8), AX
	MOVBLZX (R8)(DX*1), CX
	MOVBLZX -1(R8)(R9*1), R11
	MOVB    AX, (DI)
	MOVB    CX, (DI)(DX*1)
	MOVB    R11, -1(DI)(R9*1)
	JMP     done

long:
	XORQ DX, DX

longLoop:
	MOVOU (R8)(DX*1), X0
	MOVOU X0, (DI)(DX*1)
	ADDQ  $16, DX
	LEAQ  16(DX), CX
	CMPQ  CX, R9
	JB    longLoop
	MOVOU -16(R8)(R9*1), X0     // the last sixteen, over what is done
	MOVOU X0, -16(DI)(R9*1)

done:
	ADDQ R9, DI
	ADDQ $16, SI
	DECQ BX
	JNZ  next

	SUBQ R10, DI
	MOVQ DI, ret+24(FP)
	RET
