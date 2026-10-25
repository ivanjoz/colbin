//go:build arm64 && !purego

#include "textflag.h"

// func copyElements(dst *byte, values *string, n int) int
//
// elements_amd64.s in arm64. R1 walks the string headers, a pointer and a
// length each, R0 the output. Every copy is two moves that may overlap, the
// first from the value's start and the second ending at its end, so nothing is
// read or written outside it: 1 to 3 bytes as three single bytes, then 4, 8
// and 16-byte pairs, and past 32 a 16-byte loop with an overlapping last move.
// R7 and R8 are the ends of the value and of its copy.
//
// The prefetch is for the value 32 on, as on amd64. PRFM does not fault, so
// an empty value's nil pointer is as harmless here.
TEXT ·copyElements(SB), NOSPLIT, $0-32
	MOVD dst+0(FP), R0
	MOVD values+8(FP), R1
	MOVD n+16(FP), R2
	MOVD R0, R3                 // the start, for the count of bytes written

next:
	LDP  (R1), (R4, R5)         // the value's bytes and its length
	CMP  $32, R2
	BLE  size
	MOVD 512(R1), R6            // the bytes of the value 32 on
	PRFM (R6), PLDL1KEEP

size:
	CMP  $0xFE, R5
	BHI  escape
	MOVB R5, (R0)
	ADD  $1, R0
	B    copy

escape:
	MOVD $0xFF, R6
	MOVB R6, (R0)
	MOVW R5, 1(R0)
	ADD  $5, R0

copy:
	ADD R4, R5, R7
	ADD R0, R5, R8
	CMP $4, R5
	BLO tiny
	CMP $8, R5
	BLO four
	CMP $16, R5
	BLO eight
	CMP $32, R5
	BHI long
	LDP (R4), (R9, R10)
	LDP -16(R7), (R11, R12)
	STP (R9, R10), (R0)
	STP (R11, R12), -16(R8)
	B   done

eight:
	MOVD (R4), R9
	MOVD -8(R7), R10
	MOVD R9, (R0)
	MOVD R10, -8(R8)
	B    done

four:
	MOVWU (R4), R9
	MOVWU -4(R7), R10
	MOVW  R9, (R0)
	MOVW  R10, -4(R8)
	B     done

tiny:
	CBZ   R5, done
	LSR   $1, R5, R9            // the middle byte: the second of 2 or 3
	MOVBU (R4), R10
	MOVBU (R4)(R9), R11
	MOVBU -1(R7), R12
	MOVB  R10, (R0)
	MOVB  R11, (R0)(R9)
	MOVB  R12, -1(R8)
	B     done

long:
	MOVD R4, R9
	MOVD R0, R10
	SUB  $16, R7, R13           // where the last sixteen start

longLoop:
	LDP.P 16(R9), (R11, R12)
	STP.P (R11, R12), 16(R10)
	CMP   R13, R9
	BLO   longLoop
	LDP   -16(R7), (R11, R12)   // the last sixteen, over what is done
	STP   (R11, R12), -16(R8)

done:
	ADD  R5, R0, R0
	ADD  $16, R1, R1
	SUBS $1, R2, R2
	BNE  next

	SUB  R3, R0, R0
	MOVD R0, ret+24(FP)
	RET
