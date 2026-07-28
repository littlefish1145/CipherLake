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
	err = loader.InstallPlugin(ctx, manifest, wasmRaw, nil, "")
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
		require.Contains(t, string(resp.Body), "memory_store")
		require.Contains(t, string(resp.Body), "memory_retrieve")
	})

	t.Run("mcp_memory_store_then_retrieve", func(t *testing.T) {
		// 1. memory_store: write a value through the MCP tool.
		storeBody := `{"jsonrpc":"2.0","method":"tools/call","id":3,"params":{"name":"memory_store","arguments":{"key":"sessions/abc","value":"hello-nexus"}}}`
		resp, err := loader.InvokeRoute(ctx, &pluginpb.InvokeRouteRequest{
			PluginName: manifest.Name,
			Method:     "POST",
			Path:       "/mcp/messages",
			Headers:    map[string]string{"Content-Type": "application/json"},
			Body:       []byte(storeBody),
		})
		require.NoError(t, err)
		require.Equal(t, int32(200), resp.StatusCode)
		storeResp := string(resp.Body)
		// text payload is escaped JSON inside outer JSON-RPC envelope, so
		// we match the unescaped substring rather than `"stored"` (which
		// appears as `\"stored\"` in the wire response).
		require.Contains(t, storeResp, `stored`, "memory_store response: %s", storeResp)
		require.Contains(t, storeResp, `sessions/abc`)

		// 2. memory_retrieve: read the value back through the MCP tool.
		retrieveBody := `{"jsonrpc":"2.0","method":"tools/call","id":4,"params":{"name":"memory_retrieve","arguments":{"key":"sessions/abc"}}}`
		resp, err = loader.InvokeRoute(ctx, &pluginpb.InvokeRouteRequest{
			PluginName: manifest.Name,
			Method:     "POST",
			Path:       "/mcp/messages",
			Headers:    map[string]string{"Content-Type": "application/json"},
			Body:       []byte(retrieveBody),
		})
		require.NoError(t, err)
		require.Equal(t, int32(200), resp.StatusCode)
		retrieveResp := string(resp.Body)
		require.Contains(t, retrieveResp, `found`, "memory_retrieve response: %s", retrieveResp)
		require.Contains(t, retrieveResp, `hello-nexus`)

		// 3. memory_retrieve on a missing key returns not_found.
		missingBody := `{"jsonrpc":"2.0","method":"tools/call","id":5,"params":{"name":"memory_retrieve","arguments":{"key":"sessions/missing"}}}`
		resp, err = loader.InvokeRoute(ctx, &pluginpb.InvokeRouteRequest{
			PluginName: manifest.Name,
			Method:     "POST",
			Path:       "/mcp/messages",
			Headers:    map[string]string{"Content-Type": "application/json"},
			Body:       []byte(missingBody),
		})
		require.NoError(t, err)
		require.Equal(t, int32(200), resp.StatusCode)
		missingResp := string(resp.Body)
		require.True(t,
			strings.Contains(missingResp, `not_found`) || strings.Contains(missingResp, `error`),
			"missing key should return not_found or error, got: %s", missingResp)
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
