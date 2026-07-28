package vector

import (
	"context"
	"errors"
	"testing"
)

type closeFailingEmbeddingProvider struct {
	closed bool
}

func (p *closeFailingEmbeddingProvider) GenerateEmbedding(context.Context, string) ([]float32, error) {
	return nil, nil
}

func (p *closeFailingEmbeddingProvider) GenerateEmbeddingBatch(context.Context, []string) ([][]float32, error) {
	return nil, nil
}

func (p *closeFailingEmbeddingProvider) Dimension() int {
	return 0
}

func (p *closeFailingEmbeddingProvider) Close() error {
	p.closed = true
	return errors.New("embedding provider close failed")
}

func TestVectorManagerCloseReturnsEmbeddingProviderError(t *testing.T) {
	provider := &closeFailingEmbeddingProvider{}
	manager := &VectorManager{embeddingProvider: provider}

	err := manager.Close()
	if err == nil {
		t.Fatal("Close() error = nil, want embedding provider close error")
	}
	if !provider.closed {
		t.Fatal("embedding provider was not closed")
	}
}

// TestVectorManager_CloseIdempotent verifies that calling Close() multiple
// times is safe and does not re-invoke the embedding provider's Close.
func TestVectorManager_CloseIdempotent(t *testing.T) {
	provider := &closeFailingEmbeddingProvider{}
	manager := &VectorManager{embeddingProvider: provider}

	firstErr := manager.Close()
	secondErr := manager.Close()

	if firstErr == nil {
		t.Fatal("first Close() should return provider error")
	}
	if firstErr != secondErr {
		t.Fatalf("second Close() returned different error: first=%v second=%v", firstErr, secondErr)
	}
}

// closeCountingProvider counts how many times Close is called.
type closeCountingProvider struct {
	closes int
}

func (p *closeCountingProvider) GenerateEmbedding(context.Context, string) ([]float32, error) {
	return nil, nil
}
func (p *closeCountingProvider) GenerateEmbeddingBatch(context.Context, []string) ([][]float32, error) {
	return nil, nil
}
func (p *closeCountingProvider) Dimension() int { return 0 }
func (p *closeCountingProvider) Close() error {
	p.closes++
	return nil
}

// TestVectorManager_CloseInvokesProviderOnce verifies that the underlying
// provider is closed exactly once even when Close() is called repeatedly.
func TestVectorManager_CloseInvokesProviderOnce(t *testing.T) {
	provider := &closeCountingProvider{}
	manager := &VectorManager{embeddingProvider: provider}

	_ = manager.Close()
	_ = manager.Close()
	_ = manager.Close()

	if provider.closes != 1 {
		t.Fatalf("provider.Close invoked %d times, want 1", provider.closes)
	}
}
