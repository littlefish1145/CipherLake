//go:build amd64

#include "textflag.h"

#define NOSPLIT_NOFRAME NOSPLIT|NOFRAME

// func dotFMA(aptr, bptr unsafe.Pointer, n int) float32
// AVX2+FMA implementation — processes 8 float32 per iteration with 4× unroll.
TEXT ·dotFMA(SB), NOSPLIT_NOFRAME, $0-28
	MOVQ  aptr+0(FP), AX
	MOVQ  bptr+8(FP), BX
	MOVQ  n+16(FP), CX

	CMPQ  CX, $8
	JL    scalar_fma

	VXORPS Y0, Y0, Y0
	VXORPS Y1, Y1, Y1
	VXORPS Y2, Y2, Y2
	VXORPS Y3, Y3, Y3

	MOVQ  CX, R8
	SHRQ  $5, R8
	JZ    fma_8

fma_32:
	VMOVUPS     0(AX), Y4
	VMOVUPS     0(BX), Y5
	VFMADD231PS Y5, Y4, Y0

	VMOVUPS     32(AX), Y6
	VMOVUPS     32(BX), Y7
	VFMADD231PS Y7, Y6, Y1

	VMOVUPS     64(AX), Y4
	VMOVUPS     64(BX), Y5
	VFMADD231PS Y5, Y4, Y2

	VMOVUPS     96(AX), Y6
	VMOVUPS     96(BX), Y7
	VFMADD231PS Y7, Y6, Y3

	ADDQ    $128, AX
	ADDQ    $128, BX
	DECQ    R8
	JNZ     fma_32

	VADDPS  Y1, Y0, Y0
	VADDPS  Y3, Y2, Y2
	VADDPS  Y2, Y0, Y0

fma_8:
	MOVQ  CX, R8
	ANDQ  $24, R8
	JZ    reduce_fma

fma_8_loop:
	VMOVUPS     0(AX), Y4
	VMOVUPS     0(BX), Y5
	VFMADD231PS Y5, Y4, Y0
	ADDQ        $32, AX
	ADDQ        $32, BX
	SUBQ        $8, R8
	JNZ         fma_8_loop

reduce_fma:
	VEXTRACTF128 $1, Y0, X1
	VADDPS       X1, X0, X0
	VHADDPS      X0, X0, X2
	VHADDPS      X2, X2, X0

	MOVQ  CX, R8
	ANDQ  $7, R8
	JZ    done_fma

fma_scalar:
	MOVSS 0(AX), X1
	MOVSS 0(BX), X2
	MULSS X2, X1
	ADDSS X1, X0
	ADDQ  $4, AX
	ADDQ  $4, BX
	SUBQ  $1, R8
	JNZ   fma_scalar

done_fma:
	VZEROUPPER
	MOVSS X0, ret+24(FP)
	RET

scalar_fma:
	VXORPS X0, X0, X0
	MOVQ   CX, R8
scalar_fma_loop:
	MOVSS 0(AX), X1
	MOVSS 0(BX), X2
	MULSS X2, X1
	ADDSS X1, X0
	ADDQ  $4, AX
	ADDQ  $4, BX
	SUBQ  $1, R8
	JNZ   scalar_fma_loop
	MOVSS X0, ret+24(FP)
	RET

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

// func l2SqFMA(aptr, bptr unsafe.Pointer, n int) float32
TEXT ·l2SqFMA(SB), NOSPLIT_NOFRAME, $0-28
	MOVQ  aptr+0(FP), AX
	MOVQ  bptr+8(FP), BX
	MOVQ  n+16(FP), CX

	CMPQ  CX, $8
	JL    scalar_l2_fma

	VXORPS Y0, Y0, Y0
	VXORPS Y1, Y1, Y1
	VXORPS Y2, Y2, Y2
	VXORPS Y3, Y3, Y3

	MOVQ  CX, R8
	SHRQ  $5, R8
	JZ    l2_fma_8

l2_fma_32:
	VMOVUPS     0(AX), Y4
	VMOVUPS     0(BX), Y5
	VSUBPS      Y5, Y4, Y4
	VFMADD231PS Y4, Y4, Y0

	VMOVUPS     32(AX), Y6
	VMOVUPS     32(BX), Y7
	VSUBPS      Y7, Y6, Y6
	VFMADD231PS Y6, Y6, Y1

	VMOVUPS     64(AX), Y4
	VMOVUPS     64(BX), Y5
	VSUBPS      Y5, Y4, Y4
	VFMADD231PS Y4, Y4, Y2

	VMOVUPS     96(AX), Y6
	VMOVUPS     96(BX), Y7
	VSUBPS      Y7, Y6, Y6
	VFMADD231PS Y6, Y6, Y3

	ADDQ    $128, AX
	ADDQ    $128, BX
	DECQ    R8
	JNZ     l2_fma_32

	VADDPS  Y1, Y0, Y0
	VADDPS  Y3, Y2, Y2
	VADDPS  Y2, Y0, Y0

l2_fma_8:
	MOVQ  CX, R8
	ANDQ  $24, R8
	JZ    reduce_l2_fma

l2_fma_8_loop:
	VMOVUPS     0(AX), Y4
	VMOVUPS     0(BX), Y5
	VSUBPS      Y5, Y4, Y4
	VFMADD231PS Y4, Y4, Y0
	ADDQ        $32, AX
	ADDQ        $32, BX
	SUBQ        $8, R8
	JNZ         l2_fma_8_loop

reduce_l2_fma:
	VEXTRACTF128 $1, Y0, X1
	VADDPS       X1, X0, X0
	VHADDPS      X0, X0, X2
	VHADDPS      X2, X2, X0

	MOVQ  CX, R8
	ANDQ  $7, R8
	JZ    done_l2_fma

l2_fma_scalar:
	MOVSS 0(AX), X1
	MOVSS 0(BX), X2
	SUBSS X2, X1
	MULSS X1, X1
	ADDSS X1, X0
	ADDQ  $4, AX
	ADDQ  $4, BX
	SUBQ  $1, R8
	JNZ   l2_fma_scalar

done_l2_fma:
	VZEROUPPER
	MOVSS X0, ret+24(FP)
	RET

scalar_l2_fma:
	VXORPS X0, X0, X0
	MOVQ   CX, R8
scalar_l2_fma_loop:
	MOVSS 0(AX), X1
	MOVSS 0(BX), X2
	SUBSS X2, X1
	MULSS X1, X1
	ADDSS X1, X0
	ADDQ  $4, AX
	ADDQ  $4, BX
	SUBQ  $1, R8
	JNZ   scalar_l2_fma_loop
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

// func l2SqBatch4StrideFMA(query, vectors unsafe.Pointer, dim, stride int, out unsafe.Pointer)
// AVX2+FMA — computes 4 L2² distances with arbitrary stride (bytes) between rows.
TEXT ·l2SqBatch4StrideFMA(SB), NOSPLIT|NOFRAME, $0-40
	MOVQ query+0(FP), AX
	MOVQ vectors+8(FP), BX
	MOVQ dim+16(FP), CX
	MOVQ stride+24(FP), DI
	MOVQ out+32(FP), DX

	MOVQ CX, SI
	MOVQ BX, R8
	ADDQ DI, R8
	MOVQ R8, R9
	ADDQ DI, R9
	MOVQ R9, R10
	ADDQ DI, R10

	VXORPS Y0, Y0, Y0
	VXORPS Y1, Y1, Y1
	VXORPS Y2, Y2, Y2
	VXORPS Y3, Y3, Y3

	MOVQ CX, R11
	SHRQ $3, R11
	JZ   stride4_fma_reduce

stride4_fma_loop:
	VMOVUPS 0(AX), Y4

	VMOVUPS     0(BX), Y5
	VSUBPS      Y4, Y5, Y5
	VFMADD231PS Y5, Y5, Y0

	VMOVUPS     0(R8), Y6
	VSUBPS      Y4, Y6, Y6
	VFMADD231PS Y6, Y6, Y1

	VMOVUPS     0(R9), Y7
	VSUBPS      Y4, Y7, Y7
	VFMADD231PS Y7, Y7, Y2

	VMOVUPS     0(R10), Y8
	VSUBPS      Y4, Y8, Y8
	VFMADD231PS Y8, Y8, Y3

	ADDQ $32, AX
	ADDQ $32, BX
	ADDQ $32, R8
	ADDQ $32, R9
	ADDQ $32, R10
	DECQ R11
	JNZ  stride4_fma_loop

stride4_fma_reduce:
	VEXTRACTF128 $1, Y0, X12
	VADDPS       X12, X0, X0
	VHADDPS      X0, X0, X12
	VHADDPS      X12, X12, X0

	VEXTRACTF128 $1, Y1, X12
	VADDPS       X12, X1, X1
	VHADDPS      X1, X1, X12
	VHADDPS      X12, X12, X1

	VEXTRACTF128 $1, Y2, X12
	VADDPS       X12, X2, X2
	VHADDPS      X2, X2, X12
	VHADDPS      X12, X12, X2

	VEXTRACTF128 $1, Y3, X12
	VADDPS       X12, X3, X3
	VHADDPS      X3, X3, X12
	VHADDPS      X12, X12, X3

	ANDQ $7, SI
	JZ   stride4_fma_done

stride4_fma_tail:
	MOVSS 0(AX), X12

	MOVSS 0(BX), X13
	SUBSS X12, X13
	MULSS X13, X13
	ADDSS X13, X0

	MOVSS 0(R8), X13
	SUBSS X12, X13
	MULSS X13, X13
	ADDSS X13, X1

	MOVSS 0(R9), X13
	SUBSS X12, X13
	MULSS X13, X13
	ADDSS X13, X2

	MOVSS 0(R10), X13
	SUBSS X12, X13
	MULSS X13, X13
	ADDSS X13, X3

	ADDQ $4, AX
	ADDQ $4, BX
	ADDQ $4, R8
	ADDQ $4, R9
	ADDQ $4, R10
	DECQ SI
	JNZ  stride4_fma_tail

stride4_fma_done:
	VZEROUPPER
	MOVSS X0, 0(DX)
	MOVSS X1, 4(DX)
	MOVSS X2, 8(DX)
	MOVSS X3, 12(DX)
	RET

// func l2SqBatch4StrideAVX2(query, vectors unsafe.Pointer, dim, stride int, out unsafe.Pointer)
// AVX2 (no FMA) — computes 4 L2² distances with arbitrary stride between rows.
TEXT ·l2SqBatch4StrideAVX2(SB), NOSPLIT|NOFRAME, $0-40
	MOVQ query+0(FP), AX
	MOVQ vectors+8(FP), BX
	MOVQ dim+16(FP), CX
	MOVQ stride+24(FP), DI
	MOVQ out+32(FP), DX

	MOVQ CX, SI
	MOVQ BX, R8
	ADDQ DI, R8
	MOVQ R8, R9
	ADDQ DI, R9
	MOVQ R9, R10
	ADDQ DI, R10

	VXORPS Y0, Y0, Y0
	VXORPS Y1, Y1, Y1
	VXORPS Y2, Y2, Y2
	VXORPS Y3, Y3, Y3

	MOVQ CX, R11
	SHRQ $3, R11
	JZ   stride4_avx2_reduce

stride4_avx2_loop:
	VMOVUPS 0(AX), Y4

	VMOVUPS 0(BX), Y5
	VSUBPS  Y4, Y5, Y5
	VMULPS  Y5, Y5, Y5
	VADDPS  Y5, Y0, Y0

	VMOVUPS 0(R8), Y6
	VSUBPS  Y4, Y6, Y6
	VMULPS  Y6, Y6, Y6
	VADDPS  Y6, Y1, Y1

	VMOVUPS 0(R9), Y7
	VSUBPS  Y4, Y7, Y7
	VMULPS  Y7, Y7, Y7
	VADDPS  Y7, Y2, Y2

	VMOVUPS 0(R10), Y8
	VSUBPS  Y4, Y8, Y8
	VMULPS  Y8, Y8, Y8
	VADDPS  Y8, Y3, Y3

	ADDQ $32, AX
	ADDQ $32, BX
	ADDQ $32, R8
	ADDQ $32, R9
	ADDQ $32, R10
	DECQ R11
	JNZ  stride4_avx2_loop

stride4_avx2_reduce:
	VEXTRACTF128 $1, Y0, X12
	VADDPS       X12, X0, X0
	VHADDPS      X0, X0, X12
	VHADDPS      X12, X12, X0

	VEXTRACTF128 $1, Y1, X12
	VADDPS       X12, X1, X1
	VHADDPS      X1, X1, X12
	VHADDPS      X12, X12, X1

	VEXTRACTF128 $1, Y2, X12
	VADDPS       X12, X2, X2
	VHADDPS      X2, X2, X12
	VHADDPS      X12, X12, X2

	VEXTRACTF128 $1, Y3, X12
	VADDPS       X12, X3, X3
	VHADDPS      X3, X3, X12
	VHADDPS      X12, X12, X3

	ANDQ $7, SI
	JZ   stride4_avx2_done

stride4_avx2_tail:
	MOVSS 0(AX), X12

	MOVSS 0(BX), X13
	SUBSS X12, X13
	MULSS X13, X13
	ADDSS X13, X0

	MOVSS 0(R8), X13
	SUBSS X12, X13
	MULSS X13, X13
	ADDSS X13, X1

	MOVSS 0(R9), X13
	SUBSS X12, X13
	MULSS X13, X13
	ADDSS X13, X2

	MOVSS 0(R10), X13
	SUBSS X12, X13
	MULSS X13, X13
	ADDSS X13, X3

	ADDQ $4, AX
	ADDQ $4, BX
	ADDQ $4, R8
	ADDQ $4, R9
	ADDQ $4, R10
	DECQ SI
	JNZ  stride4_avx2_tail

stride4_avx2_done:
	VZEROUPPER
	MOVSS X0, 0(DX)
	MOVSS X1, 4(DX)
	MOVSS X2, 8(DX)
	MOVSS X3, 12(DX)
	RET

// func l2SqBatch4ContigFMA(query, vectors unsafe.Pointer, n int, out unsafe.Pointer)
TEXT ·l2SqBatch4ContigFMA(SB), NOSPLIT|NOFRAME, $0-32
	MOVQ query+0(FP), AX
	MOVQ vectors+8(FP), BX
	MOVQ n+16(FP), CX
	MOVQ out+24(FP), DX

	MOVQ CX, SI
	MOVQ CX, DI
	SHLQ $2, DI
	MOVQ BX, R8
	ADDQ DI, R8
	MOVQ R8, R9
	ADDQ DI, R9
	MOVQ R9, R10
	ADDQ DI, R10

	VXORPS Y0, Y0, Y0
	VXORPS Y1, Y1, Y1
	VXORPS Y2, Y2, Y2
	VXORPS Y3, Y3, Y3

	MOVQ CX, R11
	SHRQ $3, R11
	JZ   batch4_fma_reduce

batch4_fma_loop:
	VMOVUPS 0(AX), Y4

	VMOVUPS     0(BX), Y5
	VSUBPS      Y4, Y5, Y5
	VFMADD231PS Y5, Y5, Y0

	VMOVUPS     0(R8), Y6
	VSUBPS      Y4, Y6, Y6
	VFMADD231PS Y6, Y6, Y1

	VMOVUPS     0(R9), Y7
	VSUBPS      Y4, Y7, Y7
	VFMADD231PS Y7, Y7, Y2

	VMOVUPS     0(R10), Y8
	VSUBPS      Y4, Y8, Y8
	VFMADD231PS Y8, Y8, Y3

	ADDQ $32, AX
	ADDQ $32, BX
	ADDQ $32, R8
	ADDQ $32, R9
	ADDQ $32, R10
	DECQ R11
	JNZ  batch4_fma_loop

batch4_fma_reduce:
	VEXTRACTF128 $1, Y0, X12
	VADDPS       X12, X0, X0
	VHADDPS      X0, X0, X12
	VHADDPS      X12, X12, X0

	VEXTRACTF128 $1, Y1, X12
	VADDPS       X12, X1, X1
	VHADDPS      X1, X1, X12
	VHADDPS      X12, X12, X1

	VEXTRACTF128 $1, Y2, X12
	VADDPS       X12, X2, X2
	VHADDPS      X2, X2, X12
	VHADDPS      X12, X12, X2

	VEXTRACTF128 $1, Y3, X12
	VADDPS       X12, X3, X3
	VHADDPS      X3, X3, X12
	VHADDPS      X12, X12, X3

	ANDQ $7, SI
	JZ   batch4_fma_done

batch4_fma_tail:
	MOVSS 0(AX), X12

	MOVSS 0(BX), X13
	SUBSS X12, X13
	MULSS X13, X13
	ADDSS X13, X0

	MOVSS 0(R8), X13
	SUBSS X12, X13
	MULSS X13, X13
	ADDSS X13, X1

	MOVSS 0(R9), X13
	SUBSS X12, X13
	MULSS X13, X13
	ADDSS X13, X2

	MOVSS 0(R10), X13
	SUBSS X12, X13
	MULSS X13, X13
	ADDSS X13, X3

	ADDQ $4, AX
	ADDQ $4, BX
	ADDQ $4, R8
	ADDQ $4, R9
	ADDQ $4, R10
	DECQ SI
	JNZ  batch4_fma_tail

batch4_fma_done:
	VZEROUPPER
	MOVSS X0, 0(DX)
	MOVSS X1, 4(DX)
	MOVSS X2, 8(DX)
	MOVSS X3, 12(DX)
	RET

// func l2SqBatch4ContigAVX2(query, vectors unsafe.Pointer, n int, out unsafe.Pointer)
TEXT ·l2SqBatch4ContigAVX2(SB), NOSPLIT|NOFRAME, $0-32
	MOVQ query+0(FP), AX
	MOVQ vectors+8(FP), BX
	MOVQ n+16(FP), CX
	MOVQ out+24(FP), DX

	MOVQ CX, SI
	MOVQ CX, DI
	SHLQ $2, DI
	MOVQ BX, R8
	ADDQ DI, R8
	MOVQ R8, R9
	ADDQ DI, R9
	MOVQ R9, R10
	ADDQ DI, R10

	VXORPS Y0, Y0, Y0
	VXORPS Y1, Y1, Y1
	VXORPS Y2, Y2, Y2
	VXORPS Y3, Y3, Y3

	MOVQ CX, R11
	SHRQ $3, R11
	JZ   batch4_avx2_reduce

batch4_avx2_loop:
	VMOVUPS 0(AX), Y4

	VMOVUPS 0(BX), Y5
	VSUBPS  Y4, Y5, Y5
	VMULPS  Y5, Y5, Y5
	VADDPS  Y5, Y0, Y0

	VMOVUPS 0(R8), Y6
	VSUBPS  Y4, Y6, Y6
	VMULPS  Y6, Y6, Y6
	VADDPS  Y6, Y1, Y1

	VMOVUPS 0(R9), Y7
	VSUBPS  Y4, Y7, Y7
	VMULPS  Y7, Y7, Y7
	VADDPS  Y7, Y2, Y2

	VMOVUPS 0(R10), Y8
	VSUBPS  Y4, Y8, Y8
	VMULPS  Y8, Y8, Y8
	VADDPS  Y8, Y3, Y3

	ADDQ $32, AX
	ADDQ $32, BX
	ADDQ $32, R8
	ADDQ $32, R9
	ADDQ $32, R10
	DECQ R11
	JNZ  batch4_avx2_loop

batch4_avx2_reduce:
	VEXTRACTF128 $1, Y0, X12
	VADDPS       X12, X0, X0
	VHADDPS      X0, X0, X12
	VHADDPS      X12, X12, X0

	VEXTRACTF128 $1, Y1, X12
	VADDPS       X12, X1, X1
	VHADDPS      X1, X1, X12
	VHADDPS      X12, X12, X1

	VEXTRACTF128 $1, Y2, X12
	VADDPS       X12, X2, X2
	VHADDPS      X2, X2, X12
	VHADDPS      X12, X12, X2

	VEXTRACTF128 $1, Y3, X12
	VADDPS       X12, X3, X3
	VHADDPS      X3, X3, X12
	VHADDPS      X12, X12, X3

	ANDQ $7, SI
	JZ   batch4_avx2_done

batch4_avx2_tail:
	MOVSS 0(AX), X12

	MOVSS 0(BX), X13
	SUBSS X12, X13
	MULSS X13, X13
	ADDSS X13, X0

	MOVSS 0(R8), X13
	SUBSS X12, X13
	MULSS X13, X13
	ADDSS X13, X1

	MOVSS 0(R9), X13
	SUBSS X12, X13
	MULSS X13, X13
	ADDSS X13, X2

	MOVSS 0(R10), X13
	SUBSS X12, X13
	MULSS X13, X13
	ADDSS X13, X3

	ADDQ $4, AX
	ADDQ $4, BX
	ADDQ $4, R8
	ADDQ $4, R9
	ADDQ $4, R10
	DECQ SI
	JNZ  batch4_avx2_tail

batch4_avx2_done:
	VZEROUPPER
	MOVSS X0, 0(DX)
	MOVSS X1, 4(DX)
	MOVSS X2, 8(DX)
	MOVSS X3, 12(DX)
	RET
