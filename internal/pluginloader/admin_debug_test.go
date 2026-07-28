package pluginloader

import (
	"context"
	"testing"

	pluginpb "cipherlake/proto/plugin"

	"github.com/stretchr/testify/require"
)

func TestLoaderAdminGRPCServer_DebugTools(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	srv := &LoaderAdminGRPCServer{Loader: l}
	ctx := context.Background()

	// Seed state and debug data.
	_, err := l.StatePut("demo", "k1", []byte("v1"), 0)
	require.NoError(t, err)
	l.DebugLog("demo", "info", "hello")
	l.DebugMetric("demo", "cpu", 0.5, `{"core":"0"}`)
	l.SetPluginLogLevel("demo", "warn")

	dump, err := srv.DumpPluginState(ctx, &pluginpb.DumpPluginStateRequest{PluginName: "demo"})
	require.NoError(t, err)
	require.Equal(t, "demo", dump.PluginName)
	require.Len(t, dump.Entries, 1)
	require.Equal(t, "k1", dump.Entries[0].Key)
	require.Equal(t, []byte("v1"), dump.Entries[0].Value)

	logs, err := srv.GetPluginLogs(ctx, &pluginpb.GetPluginLogsRequest{PluginName: "demo", Limit: 10})
	require.NoError(t, err)
	require.Len(t, logs.Entries, 1)
	require.Equal(t, "info", logs.Entries[0].Level)
	require.Equal(t, "hello", logs.Entries[0].Payload)

	metrics, err := srv.GetPluginMetrics(ctx, &pluginpb.GetPluginMetricsRequest{PluginName: "demo", Limit: 10})
	require.NoError(t, err)
	require.Len(t, metrics.Samples, 1)
	require.Equal(t, "cpu", metrics.Samples[0].Name)
	require.InDelta(t, 0.5, metrics.Samples[0].Value, 0.001)

	levelResp, err := srv.SetPluginLogLevel(ctx, &pluginpb.SetPluginLogLevelRequest{PluginName: "demo", Level: "error"})
	require.NoError(t, err)
	require.Equal(t, "demo", levelResp.PluginName)
	require.Equal(t, "error", levelResp.Level)
}

func TestLoaderAdminGRPCServer_DebugToolsEmptyPluginName(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()
	srv := &LoaderAdminGRPCServer{Loader: l}
	ctx := context.Background()

	_, err := srv.DumpPluginState(ctx, &pluginpb.DumpPluginStateRequest{PluginName: ""})
	require.Error(t, err)
	_, err = srv.GetPluginLogs(ctx, &pluginpb.GetPluginLogsRequest{PluginName: ""})
	require.Error(t, err)
	_, err = srv.GetPluginMetrics(ctx, &pluginpb.GetPluginMetricsRequest{PluginName: ""})
	require.Error(t, err)
	_, err = srv.SetPluginLogLevel(ctx, &pluginpb.SetPluginLogLevelRequest{PluginName: "", Level: "info"})
	require.Error(t, err)
}
