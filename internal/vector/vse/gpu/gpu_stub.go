//go:build !cuda

package gpu

import (
	"nexus/internal/vector/vse"
	"nexus/internal/vector/vse/engine/quantizer"
)

type cudaManager struct{}

func newGPUImpl(Config) gpuImpl { return &cudaManager{} }

func (*cudaManager) init() error                                      { return ErrNoGPU }
func (*cudaManager) close()                                           {}
func (*cudaManager) enabled() bool                                    { return false }
func (*cudaManager) pinSegment(*vse.SegmentMeta, []byte, *quantizer.PQQuantizer, *quantizer.IVF) error {
	return ErrNoGPU
}
func (*cudaManager) unpinSegment(vse.SegmentID) error                     { return ErrNoGPU }
func (*cudaManager) search(vse.SegmentID, SearchRequest) ([]vse.SearchResult, error) {
	return nil, ErrNoGPU
}
func (*cudaManager) batchSearch(vse.SegmentID, []SearchRequest) []BatchResult { return nil }
