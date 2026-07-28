package gateway

import (
	"context"
	"errors"
	"testing"

	"cipherlake/internal/config"
	pluginpb "cipherlake/proto/plugin"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeLoaderClient is a test double for pluginpb.PluginLoaderServiceClient.
type fakeLoaderClient struct {
	listHooksFunc  func(context.Context, *pluginpb.ListHooksRequest, ...grpc.CallOption) (*pluginpb.ListHooksResponse, error)
	invokeHookFunc func(context.Context, *pluginpb.InvokeHookRequest, ...grpc.CallOption) (*pluginpb.InvokeHookResponse, error)
}

func (f *fakeLoaderClient) InvokeHook(ctx context.Context, req *pluginpb.InvokeHookRequest, _ ...grpc.CallOption) (*pluginpb.InvokeHookResponse, error) {
	if f.invokeHookFunc != nil {
		return f.invokeHookFunc(ctx, req)
	}
	return nil, errors.New("InvokeHook not implemented")
}

func (f *fakeLoaderClient) ListHooks(ctx context.Context, req *pluginpb.ListHooksRequest, _ ...grpc.CallOption) (*pluginpb.ListHooksResponse, error) {
	if f.listHooksFunc != nil {
		return f.listHooksFunc(ctx, req)
	}
	return nil, errors.New("ListHooks not implemented")
}

func (f *fakeLoaderClient) InvokeRoute(ctx context.Context, req *pluginpb.InvokeRouteRequest, _ ...grpc.CallOption) (*pluginpb.InvokeRouteResponse, error) {
	return nil, errors.New("InvokeRoute not implemented")
}

func (f *fakeLoaderClient) Health(ctx context.Context, req *pluginpb.HealthRequest, _ ...grpc.CallOption) (*pluginpb.HealthResponse, error) {
	return nil, errors.New("Health not implemented")
}

func TestHookOrchestrator_TimeoutAndBreaker(t *testing.T) {
	listCount := 0
	client := &fakeLoaderClient{
		listHooksFunc: func(ctx context.Context, req *pluginpb.ListHooksRequest, _ ...grpc.CallOption) (*pluginpb.ListHooksResponse, error) {
			listCount++
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	cfg := config.PluginLoaderConfig{
		CallTimeout:              "50ms",
		CriticalBreakerThreshold: 3,
		CriticalBreakerPause:     "200ms",
	}
	orchestrator := NewHookOrchestrator(client, cfg)

	// First 3 listHooks calls time out and count toward the breaker.
	for i := 0; i < 3; i++ {
		_, err := orchestrator.Execute(context.Background(), "s3:GetObject", &pluginpb.HookContext{}, func(ctx *pluginpb.HookContext) (*HookResult, error) {
			return &HookResult{Action: pluginpb.HookAction_HOOK_ACTION_CONTINUE}, nil
		})
		require.NoError(t, err, "non-critical operation should degrade when listHooks times out")
	}
	require.Equal(t, 3, listCount)

	// Breaker is now open: non-critical operation should still degrade without calling Loader.
	listCount = 0
	_, err := orchestrator.Execute(context.Background(), "s3:GetObject", &pluginpb.HookContext{}, func(ctx *pluginpb.HookContext) (*HookResult, error) {
		return &HookResult{Action: pluginpb.HookAction_HOOK_ACTION_CONTINUE}, nil
	})
	require.NoError(t, err)
	require.Equal(t, 0, listCount, "breaker open should skip Loader calls")

	// Critical hook fails-closed while breaker is open.
	orchestrator.criticalHooks["s3:GetObject"] = struct{}{}
	_, err = orchestrator.Execute(context.Background(), "s3:GetObject", &pluginpb.HookContext{}, func(ctx *pluginpb.HookContext) (*HookResult, error) {
		return &HookResult{Action: pluginpb.HookAction_HOOK_ACTION_CONTINUE}, nil
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "critical")
}

func TestHookOrchestrator_OnFailurePolicies(t *testing.T) {
	cases := []struct {
		name      string
		onFailure string
		critical  bool
		wantErr   bool
	}{
		{"deny", "deny", false, true},
		{"allow", "allow", false, false},
		{"log_and_allow", "log_and_allow", false, false},
		{"critical deny", "allow", true, true},
		{"default log_and_allow", "", false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeLoaderClient{
				listHooksFunc: func(ctx context.Context, req *pluginpb.ListHooksRequest, _ ...grpc.CallOption) (*pluginpb.ListHooksResponse, error) {
					return &pluginpb.ListHooksResponse{
						Hooks: []*pluginpb.HookRecord{
							{
								PluginName: "demo",
								HookId:     "demo-0",
								HookType:   "before",
								Operation:  "s3:GetObject",
								Handler:    "on_hook",
								OnFailure:  tc.onFailure,
							},
						},
					}, nil
				},
				invokeHookFunc: func(ctx context.Context, req *pluginpb.InvokeHookRequest, _ ...grpc.CallOption) (*pluginpb.InvokeHookResponse, error) {
					return nil, status.Error(codes.Unavailable, "loader down")
				},
			}
			cfg := config.PluginLoaderConfig{CallTimeout: "50ms"}
			orchestrator := NewHookOrchestrator(client, cfg)
			if tc.critical {
				orchestrator.criticalHooks["s3:GetObject"] = struct{}{}
			}
			_, err := orchestrator.Execute(context.Background(), "s3:GetObject", &pluginpb.HookContext{}, func(ctx *pluginpb.HookContext) (*HookResult, error) {
				return &HookResult{Action: pluginpb.HookAction_HOOK_ACTION_CONTINUE}, nil
			})
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestHookOrchestrator_CriticalListHooksFailure(t *testing.T) {
	client := &fakeLoaderClient{
		listHooksFunc: func(ctx context.Context, req *pluginpb.ListHooksRequest, _ ...grpc.CallOption) (*pluginpb.ListHooksResponse, error) {
			return nil, status.Error(codes.DeadlineExceeded, "timeout")
		},
	}
	cfg := config.PluginLoaderConfig{
		CallTimeout:   "50ms",
		CriticalHooks: []string{"s3:GetObject"},
	}
	orchestrator := NewHookOrchestrator(client, cfg)

	_, err := orchestrator.Execute(context.Background(), "s3:GetObject", &pluginpb.HookContext{}, func(ctx *pluginpb.HookContext) (*HookResult, error) {
		return &HookResult{Action: pluginpb.HookAction_HOOK_ACTION_CONTINUE}, nil
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "critical operation")
}

func TestHookOrchestrator_NonCriticalListHooksFailureDegrades(t *testing.T) {
	client := &fakeLoaderClient{
		listHooksFunc: func(ctx context.Context, req *pluginpb.ListHooksRequest, _ ...grpc.CallOption) (*pluginpb.ListHooksResponse, error) {
			return nil, status.Error(codes.DeadlineExceeded, "timeout")
		},
	}
	cfg := config.PluginLoaderConfig{CallTimeout: "50ms"}
	orchestrator := NewHookOrchestrator(client, cfg)

	innerCalled := false
	_, err := orchestrator.Execute(context.Background(), "s3:GetObject", &pluginpb.HookContext{}, func(ctx *pluginpb.HookContext) (*HookResult, error) {
		innerCalled = true
		return &HookResult{Action: pluginpb.HookAction_HOOK_ACTION_CONTINUE}, nil
	})
	require.NoError(t, err)
	require.True(t, innerCalled)
}
