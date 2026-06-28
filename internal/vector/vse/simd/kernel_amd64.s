//go:build amd64

#include "textflag.h"

#define NOSPLIT_NOFRAME NOSPLIT|NOFRAME

// func dotAVX2(aptr, bptr unsafe.Pointer, n int) float32
// AVX2 implementation — processes 8 float32 per iteration with 4× unroll.
TEXT ·dotAVX2(SB), NOSPLIT_NOFRAME, $0-28
	MOVQ  aptr+0(FP), AX
	MOVQ  bptr+8(FP), BX
	MOVQ  n+16(FP), CX

	CMPQ  CX, $8
	JL    scalar_avx2

	VXORPS Y0, Y0, Y0
	VXORPS Y1, Y1, Y1
	VXORPS Y2, Y2, Y2
	VXORPS Y3, Y3, Y3

	MOVQ  CX, R8
	SHRQ  $5, R8
	JZ    avx2_8

avx2_32:
	VMOVUPS  0(AX), Y4
	VMOVUPS  0(BX), Y5
	VMULPS   Y5, Y4, Y4
	VADDPS   Y4, Y0, Y0

	VMOVUPS  32(AX), Y6
	VMOVUPS  32(BX), Y7
	VMULPS   Y7, Y6, Y6
	VADDPS   Y6, Y1, Y1

	VMOVUPS  64(AX), Y4
	VMOVUPS  64(BX), Y5
	VMULPS   Y5, Y4, Y4
	VADDPS   Y4, Y2, Y2

	VMOVUPS  96(AX), Y6
	VMOVUPS  96(BX), Y7
	VMULPS   Y7, Y6, Y6
	VADDPS   Y6, Y3, Y3

	ADDQ    $128, AX
	ADDQ    $128, BX
	DECQ    R8
	JNZ     avx2_32

	VADDPS  Y1, Y0, Y0
	VADDPS  Y3, Y2, Y2
	VADDPS  Y2, Y0, Y0

avx2_8:
	MOVQ  CX, R8
	ANDQ  $24, R8
	JZ    reduce_avx2

avx2_8_loop:
	VMOVUPS  0(AX), Y4
	VMOVUPS  0(BX), Y5
	VMULPS   Y5, Y4, Y4
	VADDPS   Y4, Y0, Y0
	ADDQ    $32, AX
	ADDQ    $32, BX
	SUBQ    $8, R8
	JNZ     avx2_8_loop

reduce_avx2:
	VEXTRACTF128 $1, Y0, X1
	VADDPS      X1, X0, X0
	VHADDPS     X0, X0, X2
	VHADDPS     X2, X2, X0

	MOVQ  CX, R8
	ANDQ  $7, R8
	JZ    done_avx2

avx2_scalar:
	MOVSS 0(AX), X1
	MOVSS 0(BX), X2
	MULSS X2, X1
	ADDSS X1, X0
	ADDQ  $4, AX
	ADDQ  $4, BX
	SUBQ  $1, R8
	JNZ   avx2_scalar

done_avx2:
	VZEROUPPER
	MOVSS X0, ret+24(FP)
	RET

scalar_avx2:
	VXORPS X0, X0, X0
	MOVQ   CX, R8
scalar_avx2_loop:
	MOVSS 0(AX), X1
	MOVSS 0(BX), X2
	MULSS X2, X1
	ADDSS X1, X0
	ADDQ  $4, AX
	ADDQ  $4, BX
	SUBQ  $1, R8
	JNZ   scalar_avx2_loop
	MOVSS X0, ret+24(FP)
	RET

// func dotSSE4(aptr, bptr unsafe.Pointer, n int) float32
// SSE4.2 implementation — processes 4 float32 per iteration with 4× unroll.
TEXT ·dotSSE4(SB), NOSPLIT_NOFRAME, $0-28
	MOVQ  aptr+0(FP), AX
	MOVQ  bptr+8(FP), BX
	MOVQ  n+16(FP), CX

	CMPQ  CX, $4
	JL    scalar_sse

	XORPS X0, X0
	XORPS X1, X1
	XORPS X2, X2
	XORPS X3, X3

	MOVQ  CX, R8
	SHRQ  $4, R8
	JZ    sse_4

sse_16:
	MOVUPS  0(AX), X4
	MOVUPS  0(BX), X5
	MULPS   X5, X4
	ADDPS   X4, X0

	MOVUPS  16(AX), X6
	MOVUPS  16(BX), X7
	MULPS   X7, X6
	ADDPS   X6, X1

	MOVUPS  32(AX), X4
	MOVUPS  32(BX), X5
	MULPS   X5, X4
	ADDPS   X4, X2

	MOVUPS  48(AX), X6
	MOVUPS  48(BX), X7
	MULPS   X7, X6
	ADDPS   X6, X3

	ADDQ    $64, AX
	ADDQ    $64, BX
	DECQ    R8
	JNZ     sse_16

	ADDPS   X1, X0
	ADDPS   X3, X2
	ADDPS   X2, X0

sse_4:
	MOVQ  CX, R8
	ANDQ  $12, R8
	JZ    reduce_sse

sse_4_loop:
	MOVUPS  0(AX), X4
	MOVUPS  0(BX), X5
	MULPS   X5, X4
	ADDPS   X4, X0
	ADDQ    $16, AX
	ADDQ    $16, BX
	SUBQ    $4, R8
	JNZ     sse_4_loop

reduce_sse:
	MOVHLPS X0, X1
	ADDPS   X1, X0
	MOVSLDUP X0, X1
	ADDSS   X1, X0

	MOVQ  CX, R8
	ANDQ  $3, R8
	JZ    done_sse

sse_scalar:
	MOVSS 0(AX), X1
	MOVSS 0(BX), X2
	MULSS X2, X1
	ADDSS X1, X0
	ADDQ  $4, AX
	ADDQ  $4, BX
	SUBQ  $1, R8
	JNZ   sse_scalar

done_sse:
	MOVSS X0, ret+24(FP)
	RET

scalar_sse:
	XORPS X0, X0
	MOVQ  CX, R8
scalar_sse_loop:
	MOVSS 0(AX), X1
	MOVSS 0(BX), X2
	MULSS X2, X1
	ADDSS X1, X0
	ADDQ  $4, AX
	ADDQ  $4, BX
	SUBQ  $1, R8
	JNZ   scalar_sse_loop
	MOVSS X0, ret+24(FP)
	RET

// func l2SqAVX2(aptr, bptr unsafe.Pointer, n int) float32
TEXT ·l2SqAVX2(SB), NOSPLIT_NOFRAME, $0-28
	MOVQ  aptr+0(FP), AX
	MOVQ  bptr+8(FP), BX
	MOVQ  n+16(FP), CX

	CMPQ  CX, $8
	JL    scalar_l2_avx2

	VXORPS Y0, Y0, Y0
	VXORPS Y1, Y1, Y1
	VXORPS Y2, Y2, Y2
	VXORPS Y3, Y3, Y3

	MOVQ  CX, R8
	SHRQ  $5, R8
	JZ    l2_avx2_8

l2_avx2_32:
	VMOVUPS  0(AX), Y4
	VMOVUPS  0(BX), Y5
	VSUBPS   Y5, Y4, Y4
	VMULPS   Y4, Y4, Y4
	VADDPS   Y4, Y0, Y0

	VMOVUPS  32(AX), Y6
	VMOVUPS  32(BX), Y7
	VSUBPS   Y7, Y6, Y6
	VMULPS   Y6, Y6, Y6
	VADDPS   Y6, Y1, Y1

	VMOVUPS  64(AX), Y4
	VMOVUPS  64(BX), Y5
	VSUBPS   Y5, Y4, Y4
	VMULPS   Y4, Y4, Y4
	VADDPS   Y4, Y2, Y2

	VMOVUPS  96(AX), Y6
	VMOVUPS  96(BX), Y7
	VSUBPS   Y7, Y6, Y6
	VMULPS   Y6, Y6, Y6
	VADDPS   Y6, Y3, Y3

	ADDQ    $128, AX
	ADDQ    $128, BX
	DECQ    R8
	JNZ     l2_avx2_32

	VADDPS  Y1, Y0, Y0
	VADDPS  Y3, Y2, Y2
	VADDPS  Y2, Y0, Y0

l2_avx2_8:
	MOVQ  CX, R8
	ANDQ  $24, R8
	JZ    reduce_l2_avx2

l2_avx2_8_loop:
	VMOVUPS  0(AX), Y4
	VMOVUPS  0(BX), Y5
	VSUBPS   Y5, Y4, Y4
	VMULPS   Y4, Y4, Y4
	VADDPS   Y4, Y0, Y0
	ADDQ    $32, AX
	ADDQ    $32, BX
	SUBQ    $8, R8
	JNZ     l2_avx2_8_loop

reduce_l2_avx2:
	VEXTRACTF128 $1, Y0, X1
	VADDPS      X1, X0, X0
	VHADDPS     X0, X0, X2
	VHADDPS     X2, X2, X0

	MOVQ  CX, R8
	ANDQ  $7, R8
	JZ    done_l2_avx2

l2_avx2_scalar:
	MOVSS 0(AX), X1
	MOVSS 0(BX), X2
	SUBSS X2, X1
	MULSS X1, X1
	ADDSS X1, X0
	ADDQ  $4, AX
	ADDQ  $4, BX
	SUBQ  $1, R8
	JNZ   l2_avx2_scalar

done_l2_avx2:
	VZEROUPPER
	MOVSS X0, ret+24(FP)
	RET

scalar_l2_avx2:
	VXORPS X0, X0, X0
	MOVQ   CX, R8
scalar_l2_avx2_loop:
	MOVSS 0(AX), X1
	MOVSS 0(BX), X2
	SUBSS X2, X1
	MULSS X1, X1
	ADDSS X1, X0
	ADDQ  $4, AX
	ADDQ  $4, BX
	SUBQ  $1, R8
	JNZ   scalar_l2_avx2_loop
	MOVSS X0, ret+24(FP)
	RET

// func l2SqSSE4(aptr, bptr unsafe.Pointer, n int) float32
TEXT ·l2SqSSE4(SB), NOSPLIT_NOFRAME, $0-28
	MOVQ  aptr+0(FP), AX
	MOVQ  bptr+8(FP), BX
	MOVQ  n+16(FP), CX

	CMPQ  CX, $4
	JL    scalar_l2_sse

	XORPS X0, X0
	XORPS X1, X1
	XORPS X2, X2
	XORPS X3, X3

	MOVQ  CX, R8
	SHRQ  $4, R8
	JZ    l2_sse_4

l2_sse_16:
	MOVUPS  0(AX), X4
	MOVUPS  0(BX), X5
	SUBPS   X5, X4
	MULPS   X4, X4
	ADDPS   X4, X0

	MOVUPS  16(AX), X6
	MOVUPS  16(BX), X7
	SUBPS   X7, X6
	MULPS   X6, X6
	ADDPS   X6, X1

	MOVUPS  32(AX), X4
	MOVUPS  32(BX), X5
	SUBPS   X5, X4
	MULPS   X4, X4
	ADDPS   X4, X2

	MOVUPS  48(AX), X6
	MOVUPS  48(BX), X7
	SUBPS   X7, X6
	MULPS   X6, X6
	ADDPS   X6, X3

	ADDQ    $64, AX
	ADDQ    $64, BX
	DECQ    R8
	JNZ     l2_sse_16

	ADDPS   X1, X0
	ADDPS   X3, X2
	ADDPS   X2, X0

l2_sse_4:
	MOVQ  CX, R8
	ANDQ  $12, R8
	JZ    reduce_l2_sse

l2_sse_4_loop:
	MOVUPS  0(AX), X4
	MOVUPS  0(BX), X5
	SUBPS   X5, X4
	MULPS   X4, X4
	ADDPS   X4, X0
	ADDQ    $16, AX
	ADDQ    $16, BX
	SUBQ    $4, R8
	JNZ     l2_sse_4_loop

reduce_l2_sse:
	MOVHLPS X0, X1
	ADDPS   X1, X0
	MOVSLDUP X0, X1
	ADDSS   X1, X0

	MOVQ  CX, R8
	ANDQ  $3, R8
	JZ    done_l2_sse

l2_sse_scalar:
	MOVSS 0(AX), X1
	MOVSS 0(BX), X2
	SUBSS X2, X1
	MULSS X1, X1
	ADDSS X1, X0
	ADDQ  $4, AX
	ADDQ  $4, BX
	SUBQ  $1, R8
	JNZ   l2_sse_scalar

done_l2_sse:
	MOVSS X0, ret+24(FP)
	RET

scalar_l2_sse:
	XORPS X0, X0
	MOVQ  CX, R8
scalar_l2_sse_loop:
	MOVSS 0(AX), X1
	MOVSS 0(BX), X2
	SUBSS X2, X1
	MULSS X1, X1
	ADDSS X1, X0
	ADDQ  $4, AX
	ADDQ  $4, BX
	SUBQ  $1, R8
	JNZ   scalar_l2_sse_loop
	MOVSS X0, ret+24(FP)
	RET

// Prefetch — software prefetch at various levels.
// func prefetchL0(addr unsafe.Pointer)
TEXT ·prefetchL0(SB), NOSPLIT, $0-8
	MOVQ addr+0(FP), AX
	PREFETCHT0 (AX)
	RET

// func prefetchL1(addr unsafe.Pointer)
TEXT ·prefetchL1(SB), NOSPLIT, $0-8
	MOVQ addr+0(FP), AX
	PREFETCHT1 (AX)
	RET

// func prefetchL2(addr unsafe.Pointer)
TEXT ·prefetchL2(SB), NOSPLIT, $0-8
	MOVQ addr+0(FP), AX
	PREFETCHT2 (AX)
	RET

// func prefetchNTA(addr unsafe.Pointer)
TEXT ·prefetchNTA(SB), NOSPLIT, $0-8
	MOVQ addr+0(FP), AX
	PREFETCHNTA (AX)
	RET
