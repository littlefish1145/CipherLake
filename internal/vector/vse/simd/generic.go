package simd

func dotGeneric(a, b []float32) float32 {
	var sum float32
	for i := range a {
		sum += a[i] * b[i]
	}
	return sum
}

func l2SqGeneric(a, b []float32) float32 {
	var sum float32
	for i := range a {
		d := a[i] - b[i]
		sum += d * d
	}
	return sum
}

func dotNEON(a, b []float32) float32 {
	return dotGeneric(a, b)
}

func l2SqNEON(a, b []float32) float32 {
	return l2SqGeneric(a, b)
}

func dotAVX512(a, b []float32) float32 {
	return dotGeneric(a, b)
}

func l2SqAVX512(a, b []float32) float32 {
	return l2SqGeneric(a, b)
}
