package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	pluginpb "cipherlake/proto/plugin"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// PluginAdminProxy exposes the Loader's PluginAdminService gRPC API over
// HTTP so that cipherlakectl can manage plugins through the Gateway's
// existing /admin/ route (spec §3.11, P3-1 / P3-5).
type PluginAdminProxy struct {
	client pluginpb.PluginAdminServiceClient
}

// NewPluginAdminProxy creates a proxy for the given admin client. If client
// is nil, all handlers return 503.
func NewPluginAdminProxy(client pluginpb.PluginAdminServiceClient) *PluginAdminProxy {
	return &PluginAdminProxy{client: client}
}

func (p *PluginAdminProxy) requireClient(w http.ResponseWriter) bool {
	if p.client == nil {
		http.Error(w, "plugin loader admin client not configured", http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (p *PluginAdminProxy) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

func (p *PluginAdminProxy) writeError(w http.ResponseWriter, err error) {
	if st, ok := status.FromError(err); ok {
		http.Error(w, st.Message(), grpcToHTTPStatus(st.Code()))
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func grpcToHTTPStatus(code codes.Code) int {
	switch code {
	case codes.InvalidArgument:
		return http.StatusBadRequest
	case codes.NotFound:
		return http.StatusNotFound
	case codes.AlreadyExists:
		return http.StatusConflict
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.FailedPrecondition:
		return http.StatusPreconditionFailed
	default:
		return http.StatusInternalServerError
	}
}

func (p *PluginAdminProxy) writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// ListPlugins handles GET /admin/plugin/list.
func (p *PluginAdminProxy) ListPlugins(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !p.requireClient(w) {
		return
	}
	ctx, cancel := p.ctx()
	defer cancel()
	resp, err := p.client.ListPlugins(ctx, &pluginpb.ListPluginsRequest{})
	if err != nil {
		p.writeError(w, err)
		return
	}
	p.writeJSON(w, resp)
}

// InstallPlugin handles POST /admin/plugin/install.
func (p *PluginAdminProxy) InstallPlugin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !p.requireClient(w) {
		return
	}
	var req pluginpb.InstallPluginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	ctx, cancel := p.ctx()
	defer cancel()
	resp, err := p.client.InstallPlugin(ctx, &req)
	if err != nil {
		p.writeError(w, err)
		return
	}
	p.writeJSON(w, resp)
}

// UninstallPlugin handles POST /admin/plugin/uninstall.
func (p *PluginAdminProxy) UninstallPlugin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !p.requireClient(w) {
		return
	}
	var req pluginpb.UninstallPluginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	ctx, cancel := p.ctx()
	defer cancel()
	resp, err := p.client.UninstallPlugin(ctx, &req)
	if err != nil {
		p.writeError(w, err)
		return
	}
	p.writeJSON(w, resp)
}

// ReloadPlugin handles POST /admin/plugin/reload.
func (p *PluginAdminProxy) ReloadPlugin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !p.requireClient(w) {
		return
	}
	var req pluginpb.ReloadPluginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	ctx, cancel := p.ctx()
	defer cancel()
	resp, err := p.client.ReloadPlugin(ctx, &req)
	if err != nil {
		p.writeError(w, err)
		return
	}
	p.writeJSON(w, resp)
}

// GetPluginState handles GET /admin/plugin/state?plugin=&key=.
func (p *PluginAdminProxy) GetPluginState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !p.requireClient(w) {
		return
	}
	pluginName := r.URL.Query().Get("plugin")
	key := r.URL.Query().Get("key")
	if pluginName == "" || key == "" {
		http.Error(w, "plugin and key query parameters are required", http.StatusBadRequest)
		return
	}
	ctx, cancel := p.ctx()
	defer cancel()
	resp, err := p.client.GetPluginState(ctx, &pluginpb.GetPluginStateRequest{PluginName: pluginName, Key: key})
	if err != nil {
		p.writeError(w, err)
		return
	}
	p.writeJSON(w, resp)
}

// DumpPluginState handles GET /admin/plugin/state/dump?plugin=.
func (p *PluginAdminProxy) DumpPluginState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !p.requireClient(w) {
		return
	}
	pluginName := r.URL.Query().Get("plugin")
	if pluginName == "" {
		http.Error(w, "plugin query parameter is required", http.StatusBadRequest)
		return
	}
	ctx, cancel := p.ctx()
	defer cancel()
	resp, err := p.client.DumpPluginState(ctx, &pluginpb.DumpPluginStateRequest{PluginName: pluginName})
	if err != nil {
		p.writeError(w, err)
		return
	}
	p.writeJSON(w, resp)
}

// GetPluginLogs handles GET /admin/plugin/logs?plugin=&limit=.
func (p *PluginAdminProxy) GetPluginLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !p.requireClient(w) {
		return
	}
	pluginName := r.URL.Query().Get("plugin")
	if pluginName == "" {
		http.Error(w, "plugin query parameter is required", http.StatusBadRequest)
		return
	}
	limit := int32(100)
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil && n > 0 {
			limit = int32(n)
		}
	}
	ctx, cancel := p.ctx()
	defer cancel()
	resp, err := p.client.GetPluginLogs(ctx, &pluginpb.GetPluginLogsRequest{PluginName: pluginName, Limit: limit})
	if err != nil {
		p.writeError(w, err)
		return
	}
	p.writeJSON(w, resp)
}

// GetPluginMetrics handles GET /admin/plugin/metrics?plugin=&limit=.
func (p *PluginAdminProxy) GetPluginMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !p.requireClient(w) {
		return
	}
	pluginName := r.URL.Query().Get("plugin")
	if pluginName == "" {
		http.Error(w, "plugin query parameter is required", http.StatusBadRequest)
		return
	}
	limit := int32(100)
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil && n > 0 {
			limit = int32(n)
		}
	}
	ctx, cancel := p.ctx()
	defer cancel()
	resp, err := p.client.GetPluginMetrics(ctx, &pluginpb.GetPluginMetricsRequest{PluginName: pluginName, Limit: limit})
	if err != nil {
		p.writeError(w, err)
		return
	}
	p.writeJSON(w, resp)
}

// SetPluginLogLevel handles POST /admin/plugin/log-level?plugin=&level=.
func (p *PluginAdminProxy) SetPluginLogLevel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !p.requireClient(w) {
		return
	}
	pluginName := r.URL.Query().Get("plugin")
	level := r.URL.Query().Get("level")
	if pluginName == "" || level == "" {
		http.Error(w, "plugin and level query parameters are required", http.StatusBadRequest)
		return
	}
	ctx, cancel := p.ctx()
	defer cancel()
	resp, err := p.client.SetPluginLogLevel(ctx, &pluginpb.SetPluginLogLevelRequest{PluginName: pluginName, Level: level})
	if err != nil {
		p.writeError(w, err)
		return
	}
	p.writeJSON(w, resp)
}

// AddTrustedKey handles POST /admin/plugin/trust/add.
func (p *PluginAdminProxy) AddTrustedKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !p.requireClient(w) {
		return
	}
	var req pluginpb.AddTrustedKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	ctx, cancel := p.ctx()
	defer cancel()
	resp, err := p.client.AddTrustedKey(ctx, &req)
	if err != nil {
		p.writeError(w, err)
		return
	}
	p.writeJSON(w, resp)
}

// RemoveTrustedKey handles POST /admin/plugin/trust/remove.
func (p *PluginAdminProxy) RemoveTrustedKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !p.requireClient(w) {
		return
	}
	var req pluginpb.RemoveTrustedKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	ctx, cancel := p.ctx()
	defer cancel()
	resp, err := p.client.RemoveTrustedKey(ctx, &req)
	if err != nil {
		p.writeError(w, err)
		return
	}
	p.writeJSON(w, resp)
}

// ListTrustedKeys handles GET /admin/plugin/trust/list.
func (p *PluginAdminProxy) ListTrustedKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !p.requireClient(w) {
		return
	}
	ctx, cancel := p.ctx()
	defer cancel()
	resp, err := p.client.ListTrustedKeys(ctx, &pluginpb.ListTrustedKeysRequest{})
	if err != nil {
		p.writeError(w, err)
		return
	}
	p.writeJSON(w, resp)
}

var _ = io.EOF
