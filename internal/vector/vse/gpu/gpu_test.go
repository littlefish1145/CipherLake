//go:build cuda

package gpu

import (
	"testing"
)

func TestGPUInit(t *testing.T) {
	mgr := NewManager(DefaultConfig())
	if err := mgr.Init(); err != nil {
		t.Fatalf("GPU init failed: %v", err)
	}
	defer mgr.Close()

	if !mgr.Enabled() {
		t.Fatal("GPU should be enabled after init")
	}
	t.Logf("GPU enabled: %v", mgr.Enabled())
}
