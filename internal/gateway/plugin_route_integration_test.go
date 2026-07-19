package gateway

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cipherlake/internal/config"
	"cipherlake/internal/pluginloader"
	pluginpb "cipherlake/proto/plugin"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ensureMcpMemoryServerWasm returns the paths to the Rust example plugin's
// manifest and wasm artifact. The plugin is a standalone Rust Cargo project at
// examples/plugins/mcp-memory-server-rust. If plugin.wasm is missing, it builds
// it with cargo.
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

// TestPluginRoute_EndToEnd boots a real Loader with the Rust mcp-memory-server
// plugin, starts the Loader gRPC server, and verifies that the Gateway's
// /_plugins/<name>/* route forwards requests end-to-end.
func TestPluginRoute_EndToEnd(t *testing.T) {
	manifestPath, wasmPath := ensureMcpMemoryServerWasm(t)

	manifestRaw, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	wasmRaw, err := os.ReadFile(wasmPath)
	require.NoError(t, err)

	manifest, err := pluginloader.ParseManifest(manifestRaw)
	require.NoError(t, err)

	// Start Loader.
	loader, err := pluginloader.New(&config.PluginLoaderConfig{
		CallTimeout: "1s",
		Raft: &config.RaftConfig{
			Enabled:    true,
			DataDir:    t.TempDir(),
			NodeID:     "gateway-integration-test",
			ListenAddr: "127.0.0.1:0",
		},
	})
	require.NoError(t, err)
	defer loader.Shutdown()

	// Wait for single-node leader election before applying.
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	var installErr error
	for time.Now().Before(deadline) {
		installErr = loader.InstallPlugin(ctx, manifest, wasmRaw, 1)
		if installErr == nil {
			break
		}
		if !strings.Contains(installErr.Error(), "not the leader") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.NoError(t, installErr)

	// Start Loader gRPC server.
	grpcLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcSrv := grpc.NewServer()
	pluginpb.RegisterPluginLoaderServiceServer(grpcSrv, &pluginloader.LoaderGRPCServer{Loader: loader})
	go grpcSrv.Serve(grpcLn)
	defer grpcSrv.Stop()

	// Connect Gateway to Loader.
	conn, err := grpc.NewClient(grpcLn.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()

	gw := &S3Gateway{
		config: &config.Config{
			PluginLoader: config.PluginLoaderConfig{
				CallTimeout: "1s",
			},
		},
		loaderClient: pluginpb.NewPluginLoaderServiceClient(conn),
	}

	// Hit the SSE route through the Gateway handler.
	req := httptest.NewRequest(http.MethodGet, "/_plugins/mcp-memory-server/mcp/sse", nil)
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	require.Contains(t, rec.Body.String(), "event: endpoint")

	// Hit the JSON-RPC route.
	body := strings.NewReader(`{"jsonrpc":"2.0","method":"initialize","id":1}`)
	req = httptest.NewRequest(http.MethodPost, "/_plugins/mcp-memory-server/mcp/messages", body)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "mcp-memory-server")
}
