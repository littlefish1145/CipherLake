package gateway

import (
	"context"
	"testing"
	"time"

	"cipherlake/internal/config"
	pluginpb "cipherlake/proto/plugin"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

func TestHookConditionEvaluator(t *testing.T) {
	ev, err := newHookConditionEvaluator()
	require.NoError(t, err)

	t.Run("empty condition matches", func(t *testing.T) {
		ok, err := ev.evaluate("", &HookConditionEnv{Bucket: "memories"})
		require.NoError(t, err)
		require.True(t, ok)
	})

	t.Run("bucket and prefix", func(t *testing.T) {
		env := &HookConditionEnv{
			Operation: "s3:GetObject",
			Bucket:    "memories",
			Key:       "sessions/abc",
			Body:      make([]byte, 512),
		}
		ok, err := ev.evaluate(`bucket == "memories" && key.startsWith("sessions/") && size < 1024`, env)
		require.NoError(t, err)
		require.True(t, ok)
	})

	t.Run("size too large", func(t *testing.T) {
		env := &HookConditionEnv{Bucket: "memories", Body: make([]byte, 2048)}
		ok, err := ev.evaluate(`size < 1024`, env)
		require.NoError(t, err)
		require.False(t, ok)
	})

	t.Run("content type", func(t *testing.T) {
		env := &HookConditionEnv{
			Headers: map[string]string{"Content-Type": "application/json"},
		}
		ok, err := ev.evaluate(`content_type == "application/json"`, env)
		require.NoError(t, err)
		require.True(t, ok)
	})

	t.Run("user metadata", func(t *testing.T) {
		env := &HookConditionEnv{
			UserMetadata: map[string]string{"env": "prod"},
		}
		ok, err := ev.evaluate(`user_metadata["env"] == "prod"`, env)
		require.NoError(t, err)
		require.True(t, ok)
	})

	t.Run("time and source ip", func(t *testing.T) {
		env := &HookConditionEnv{
			SourceIP: "10.0.0.5",
			Time:     time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC),
		}
		ok, err := ev.evaluate(`source_ip.startsWith("10.0.") && time.getHours() == 12`, env)
		require.NoError(t, err)
		require.True(t, ok)
	})

	t.Run("compile error", func(t *testing.T) {
		_, err := ev.evaluate(`bucket = "memories"`, &HookConditionEnv{})
		require.Error(t, err)
	})

	t.Run("non-boolean result", func(t *testing.T) {
		_, err := ev.evaluate(`bucket`, &HookConditionEnv{Bucket: "memories"})
		require.Error(t, err)
	})
}

func TestHookOrchestrator_ConditionSkipsHook(t *testing.T) {
	client := &fakePluginLoaderClient{
		hooks: map[string][]*pluginpb.HookRecord{
			"s3:GetObject": {
				{
					PluginName: "p1",
					Handler:    "on_hook",
					HookType:   "before",
					Priority:   100,
					Condition:  `bucket == "memories"`,
				},
			},
		},
	}
	orch := NewHookOrchestrator(client, configForOrchestrator())

	called := false
	_, err := orch.ExecuteWithEnv(context.Background(), "s3:GetObject", &pluginpb.HookContext{
		Operation: "s3:GetObject",
		Bucket:    "other",
	}, nil, func(ctx *pluginpb.HookContext) (*HookResult, error) {
		called = true
		return &HookResult{Action: pluginpb.HookAction_HOOK_ACTION_CONTINUE}, nil
	})
	require.NoError(t, err)
	require.True(t, called, "inner handler should run when condition skips hook")
	require.False(t, client.invoked, "hook should not be invoked when condition does not match")

	client.invoked = false
	called = false
	_, err = orch.ExecuteWithEnv(context.Background(), "s3:GetObject", &pluginpb.HookContext{
		Operation: "s3:GetObject",
		Bucket:    "memories",
	}, nil, func(ctx *pluginpb.HookContext) (*HookResult, error) {
		called = true
		return &HookResult{Action: pluginpb.HookAction_HOOK_ACTION_CONTINUE}, nil
	})
	require.NoError(t, err)
	require.True(t, called)
	require.True(t, client.invoked, "hook should be invoked when condition matches")
}

// fakePluginLoaderClient is a stub PluginLoaderServiceClient for gateway tests.
type fakePluginLoaderClient struct {
	hooks   map[string][]*pluginpb.HookRecord
	invoked bool
}

func (f *fakePluginLoaderClient) ListHooks(ctx context.Context, in *pluginpb.ListHooksRequest, opts ...grpc.CallOption) (*pluginpb.ListHooksResponse, error) {
	return &pluginpb.ListHooksResponse{Hooks: f.hooks[in.Operation]}, nil
}

func (f *fakePluginLoaderClient) InvokeHook(ctx context.Context, in *pluginpb.InvokeHookRequest, opts ...grpc.CallOption) (*pluginpb.InvokeHookResponse, error) {
	f.invoked = true
	return &pluginpb.InvokeHookResponse{Action: pluginpb.HookAction_HOOK_ACTION_CONTINUE}, nil
}

func (f *fakePluginLoaderClient) InvokeRoute(ctx context.Context, in *pluginpb.InvokeRouteRequest, opts ...grpc.CallOption) (*pluginpb.InvokeRouteResponse, error) {
	return &pluginpb.InvokeRouteResponse{StatusCode: 200}, nil
}

func (f *fakePluginLoaderClient) Health(ctx context.Context, in *pluginpb.HealthRequest, opts ...grpc.CallOption) (*pluginpb.HealthResponse, error) {
	return &pluginpb.HealthResponse{Healthy: true}, nil
}

func configForOrchestrator() config.PluginLoaderConfig {
	return config.PluginLoaderConfig{CallTimeout: "1s"}
}
