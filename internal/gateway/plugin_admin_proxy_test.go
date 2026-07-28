package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pluginpb "cipherlake/proto/plugin"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakePluginAdminClient struct {
	listPluginsFunc      func(ctx context.Context, req *pluginpb.ListPluginsRequest) (*pluginpb.ListPluginsResponse, error)
	getPluginLogsFunc    func(ctx context.Context, req *pluginpb.GetPluginLogsRequest) (*pluginpb.GetPluginLogsResponse, error)
	getPluginMetricsFunc func(ctx context.Context, req *pluginpb.GetPluginMetricsRequest) (*pluginpb.GetPluginMetricsResponse, error)
	setPluginLogLevelFunc func(ctx context.Context, req *pluginpb.SetPluginLogLevelRequest) (*pluginpb.SetPluginLogLevelResponse, error)
}

func (f *fakePluginAdminClient) InstallPlugin(ctx context.Context, req *pluginpb.InstallPluginRequest, opts ...grpc.CallOption) (*pluginpb.InstallPluginResponse, error) {
	return nil, nil
}

func (f *fakePluginAdminClient) UninstallPlugin(ctx context.Context, req *pluginpb.UninstallPluginRequest, opts ...grpc.CallOption) (*pluginpb.UninstallPluginResponse, error) {
	return nil, nil
}

func (f *fakePluginAdminClient) ReloadPlugin(ctx context.Context, req *pluginpb.ReloadPluginRequest, opts ...grpc.CallOption) (*pluginpb.ReloadPluginResponse, error) {
	return nil, nil
}

func (f *fakePluginAdminClient) ListPlugins(ctx context.Context, req *pluginpb.ListPluginsRequest, opts ...grpc.CallOption) (*pluginpb.ListPluginsResponse, error) {
	if f.listPluginsFunc != nil {
		return f.listPluginsFunc(ctx, req)
	}
	return &pluginpb.ListPluginsResponse{}, nil
}

func (f *fakePluginAdminClient) GetPluginState(ctx context.Context, req *pluginpb.GetPluginStateRequest, opts ...grpc.CallOption) (*pluginpb.GetPluginStateResponse, error) {
	return nil, nil
}

func (f *fakePluginAdminClient) DumpPluginState(ctx context.Context, req *pluginpb.DumpPluginStateRequest, opts ...grpc.CallOption) (*pluginpb.DumpPluginStateResponse, error) {
	return nil, nil
}

func (f *fakePluginAdminClient) GetPluginLogs(ctx context.Context, req *pluginpb.GetPluginLogsRequest, opts ...grpc.CallOption) (*pluginpb.GetPluginLogsResponse, error) {
	if f.getPluginLogsFunc != nil {
		return f.getPluginLogsFunc(ctx, req)
	}
	return &pluginpb.GetPluginLogsResponse{}, nil
}

func (f *fakePluginAdminClient) GetPluginMetrics(ctx context.Context, req *pluginpb.GetPluginMetricsRequest, opts ...grpc.CallOption) (*pluginpb.GetPluginMetricsResponse, error) {
	if f.getPluginMetricsFunc != nil {
		return f.getPluginMetricsFunc(ctx, req)
	}
	return &pluginpb.GetPluginMetricsResponse{}, nil
}

func (f *fakePluginAdminClient) SetPluginLogLevel(ctx context.Context, req *pluginpb.SetPluginLogLevelRequest, opts ...grpc.CallOption) (*pluginpb.SetPluginLogLevelResponse, error) {
	if f.setPluginLogLevelFunc != nil {
		return f.setPluginLogLevelFunc(ctx, req)
	}
	return &pluginpb.SetPluginLogLevelResponse{}, nil
}

func (f *fakePluginAdminClient) AddTrustedKey(ctx context.Context, req *pluginpb.AddTrustedKeyRequest, opts ...grpc.CallOption) (*pluginpb.AddTrustedKeyResponse, error) {
	return &pluginpb.AddTrustedKeyResponse{KeyId: req.KeyId, Added: true}, nil
}

func (f *fakePluginAdminClient) RemoveTrustedKey(ctx context.Context, req *pluginpb.RemoveTrustedKeyRequest, opts ...grpc.CallOption) (*pluginpb.RemoveTrustedKeyResponse, error) {
	return &pluginpb.RemoveTrustedKeyResponse{KeyId: req.KeyId, Removed: true}, nil
}

func (f *fakePluginAdminClient) ListTrustedKeys(ctx context.Context, req *pluginpb.ListTrustedKeysRequest, opts ...grpc.CallOption) (*pluginpb.ListTrustedKeysResponse, error) {
	return &pluginpb.ListTrustedKeysResponse{}, nil
}

func TestPluginAdminProxy_ListPlugins(t *testing.T) {
	client := &fakePluginAdminClient{
		listPluginsFunc: func(ctx context.Context, req *pluginpb.ListPluginsRequest) (*pluginpb.ListPluginsResponse, error) {
			return &pluginpb.ListPluginsResponse{
				Plugins: []*pluginpb.PluginInfo{{Name: "demo", Version: "1.0.0"}},
			}, nil
		},
	}
	proxy := NewPluginAdminProxy(client)
	req := httptest.NewRequest(http.MethodGet, "/admin/plugin/list", nil)
	rec := httptest.NewRecorder()
	proxy.ListPlugins(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp pluginpb.ListPluginsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Plugins, 1)
	require.Equal(t, "demo", resp.Plugins[0].Name)
}

func TestPluginAdminProxy_GetPluginLogs(t *testing.T) {
	client := &fakePluginAdminClient{
		getPluginLogsFunc: func(ctx context.Context, req *pluginpb.GetPluginLogsRequest) (*pluginpb.GetPluginLogsResponse, error) {
			return &pluginpb.GetPluginLogsResponse{
				PluginName: req.PluginName,
				Entries:    []*pluginpb.PluginLogEntry{{Level: "info", Payload: "hello"}},
			}, nil
		},
	}
	proxy := NewPluginAdminProxy(client)
	req := httptest.NewRequest(http.MethodGet, "/admin/plugin/logs?plugin=demo&limit=10", nil)
	rec := httptest.NewRecorder()
	proxy.GetPluginLogs(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp pluginpb.GetPluginLogsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "demo", resp.PluginName)
	require.Len(t, resp.Entries, 1)
}

func TestPluginAdminProxy_GetPluginMetrics_MissingPlugin(t *testing.T) {
	proxy := NewPluginAdminProxy(&fakePluginAdminClient{})
	req := httptest.NewRequest(http.MethodGet, "/admin/plugin/metrics", nil)
	rec := httptest.NewRecorder()
	proxy.GetPluginMetrics(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPluginAdminProxy_SetPluginLogLevel_GRPCErrorMapping(t *testing.T) {
	client := &fakePluginAdminClient{
		setPluginLogLevelFunc: func(ctx context.Context, req *pluginpb.SetPluginLogLevelRequest) (*pluginpb.SetPluginLogLevelResponse, error) {
			return nil, status.Error(codes.InvalidArgument, "bad level")
		},
	}
	proxy := NewPluginAdminProxy(client)
	req := httptest.NewRequest(http.MethodPost, "/admin/plugin/log-level?plugin=demo&level=bad", nil)
	rec := httptest.NewRecorder()
	proxy.SetPluginLogLevel(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	body, _ := io.ReadAll(rec.Body)
	require.Contains(t, string(body), "bad level")
}

func TestPluginAdminProxy_AddTrustedKey(t *testing.T) {
	proxy := NewPluginAdminProxy(&fakePluginAdminClient{})
	body := strings.NewReader(`{"key_id":"k1","public_key":"cHVibGlj","is_core":true}`)
	req := httptest.NewRequest(http.MethodPost, "/admin/plugin/trust/add", body)
	rec := httptest.NewRecorder()
	proxy.AddTrustedKey(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp pluginpb.AddTrustedKeyResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "k1", resp.KeyId)
	require.True(t, resp.Added)
}

func TestPluginAdminProxy_RemoveTrustedKey(t *testing.T) {
	proxy := NewPluginAdminProxy(&fakePluginAdminClient{})
	body := strings.NewReader(`{"key_id":"k1"}`)
	req := httptest.NewRequest(http.MethodPost, "/admin/plugin/trust/remove", body)
	rec := httptest.NewRecorder()
	proxy.RemoveTrustedKey(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp pluginpb.RemoveTrustedKeyResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "k1", resp.KeyId)
	require.True(t, resp.Removed)
}

func TestPluginAdminProxy_ListTrustedKeys(t *testing.T) {
	proxy := NewPluginAdminProxy(&fakePluginAdminClient{})
	req := httptest.NewRequest(http.MethodGet, "/admin/plugin/trust/list", nil)
	rec := httptest.NewRecorder()
	proxy.ListTrustedKeys(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp pluginpb.ListTrustedKeysResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Empty(t, resp.Keys)
}

func TestPluginAdminProxy_NoClient(t *testing.T) {
	proxy := NewPluginAdminProxy(nil)
	req := httptest.NewRequest(http.MethodGet, "/admin/plugin/list", nil)
	rec := httptest.NewRecorder()
	proxy.ListPlugins(rec, req)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestGrpcToHTTPStatus(t *testing.T) {
	require.Equal(t, http.StatusBadRequest, grpcToHTTPStatus(codes.InvalidArgument))
	require.Equal(t, http.StatusInternalServerError, grpcToHTTPStatus(codes.Internal))
}

var _ pluginpb.PluginAdminServiceClient = (*fakePluginAdminClient)(nil)
var _ = errors.New
