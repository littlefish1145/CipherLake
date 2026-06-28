package arena

import "sync"

type BufferPool struct {
	distPool sync.Pool
	idxPool  sync.Pool
}

var Pool = &BufferPool{
	distPool: sync.Pool{
		New: func() interface{} { return make([]float32, 0, 4096) },
	},
	idxPool: sync.Pool{
		New: func() interface{} { return make([]int32, 0, 4096) },
	},
}

func (bp *BufferPool) GetDists(capacity int) []float32 {
	b := bp.distPool.Get().([]float32)
	if cap(b) < capacity {
		b = make([]float32, capacity)
	}
	return b[:capacity]
}

func (bp *BufferPool) PutDists(b []float32) {
	bp.distPool.Put(b[:0])
}

func (bp *BufferPool) GetIdxs(capacity int) []int32 {
	b := bp.idxPool.Get().([]int32)
	if cap(b) < capacity {
		b = make([]int32, capacity)
	}
	return b[:capacity]
}

func (bp *BufferPool) PutIdxs(b []int32) {
	bp.idxPool.Put(b[:0])
}
