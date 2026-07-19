package pluginloader

import (
	"context"
	"testing"
	"time"
)

// Test helpers shared across the pluginloader test files.

func neverExpires() time.Time { return time.Time{} }

func testCallTimeout() time.Duration { return 5 * time.Second }

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}
