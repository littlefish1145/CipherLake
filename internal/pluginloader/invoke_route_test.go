package pluginloader

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	pluginpb "cipherlake/proto/plugin"

	"github.com/stretchr/testify/require"
)

// ensureMcpMemoryServerWasm returns the paths to the Rust example plugin's
// manifest and wasm artifact. The plugin is a standalone Rust Cargo project at
// examples/plugins/mcp-memory-server-rust. If plugin.wasm is missing, it tries
// to build it with cargo.
func ensureMcpMemoryServerWasm(t *testing.T) (manifestPath, wasmPath string) {
	t.Helper()
	dir := filepath.Join("..", "..", "examples", "plugins", "mcp-memory-server-rust")
	manifestPath = filepath.Join(dir, "manifest.json")
	wasmPath = filepath.Join(dir, "plugin.wasm")

	if _, err := os.Stat(wasmPath); err == nil {
		return manifestPath, wasmPath
	}

	t.Logf("%s not found, attempting cargo build", wasmPath)
	cmd := exec.Command("cargo", "build", "--target", "wasm32-wasip1", "--release")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "failed to build mcp-memory-server: %s", out)

	built := filepath.Join(dir, "target", "wasm32-wasip1", "release", "mcp-memory-server.wasm")
	require.FileExists(t, built)
	require.NoError(t, os.Rename(built, wasmPath))
	require.FileExists(t, wasmPath)
	return manifestPath, wasmPath
}

func TestLoader_InvokeRoute_MCP(t *testing.T) {
	manifestPath, wasmPath := ensureMcpMemoryServerWasm(t)

	manifestRaw, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	wasmRaw, err := os.ReadFile(wasmPath)
	require.NoError(t, err)

	manifest, err := ParseManifest(manifestRaw)
	require.NoError(t, err)

	loader := newTestLoader(t)
	defer loader.Shutdown()

	ctx := context.Background()
	err = loader.InstallPlugin(ctx, manifest, wasmRaw, 1)
	require.NoError(t, err)

	t.Run("mcp_sse", func(t *testing.T) {
		resp, err := loader.InvokeRoute(ctx, &pluginpb.InvokeRouteRequest{
			PluginName: manifest.Name,
			Method:     "GET",
			Path:       "/mcp/sse",
			Headers:    map[string]string{"Accept": "text/event-stream"},
			Body:       nil,
		})
		require.NoError(t, err)
		require.Equal(t, int32(200), resp.StatusCode)
		require.Contains(t, string(resp.Body), "event: endpoint")
	})

	t.Run("mcp_initialize", func(t *testing.T) {
		resp, err := loader.InvokeRoute(ctx, &pluginpb.InvokeRouteRequest{
			PluginName: manifest.Name,
			Method:     "POST",
			Path:       "/mcp/messages",
			Headers:    map[string]string{"Content-Type": "application/json"},
			Body:       []byte(`{"jsonrpc":"2.0","method":"initialize","id":1}`),
		})
		require.NoError(t, err)
		require.Equal(t, int32(200), resp.StatusCode)
		require.Contains(t, string(resp.Body), `"mcp-memory-server"`)
	})

	t.Run("mcp_tools_list", func(t *testing.T) {
		resp, err := loader.InvokeRoute(ctx, &pluginpb.InvokeRouteRequest{
			PluginName: manifest.Name,
			Method:     "POST",
			Path:       "/mcp/messages",
			Headers:    map[string]string{"Content-Type": "application/json"},
			Body:       []byte(`{"jsonrpc":"2.0","method":"tools/list","id":2}`),
		})
		require.NoError(t, err)
		require.Equal(t, int32(200), resp.StatusCode)
		require.Contains(t, string(resp.Body), "memory_search")
	})

	t.Run("memory_search", func(t *testing.T) {
		resp, err := loader.InvokeRoute(ctx, &pluginpb.InvokeRouteRequest{
			PluginName: manifest.Name,
			Method:     "POST",
			Path:       "/memory/search",
			Headers:    map[string]string{"Content-Type": "application/json"},
			Body:       []byte(`{"query":"nexus plugins"}`),
		})
		require.NoError(t, err)
		require.Equal(t, int32(200), resp.StatusCode)
		body := string(resp.Body)
		require.True(t, strings.Contains(body, `"status":"ok"`) || strings.Contains(body, `"status":"demo"`), "unexpected body: %s", body)
	})
}
