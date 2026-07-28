package pluginloader

import (
	"context"
	"testing"

	pluginpb "cipherlake/proto/plugin"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestSanitizeHookAbortResponse_Tier0StripsBody(t *testing.T) {
	resp := &pluginpb.InvokeHookResponse{
		Action:     pluginpb.HookAction_HOOK_ACTION_ABORT,
		StatusCode: 401,
		Headers:    map[string]string{"x-reason": "denied"},
		Body:       []byte("forged body"),
	}
	sanitizeHookAbortResponse(resp, TierUntrusted, "demo", "on_hook", "s3:GetObject")

	require.Empty(t, resp.Body)
	require.Equal(t, int32(401), resp.StatusCode)
	require.Equal(t, "denied", resp.Headers["x-reason"])
}

func TestSanitizeHookAbortResponse_Tier1StripsBodyDefaults403(t *testing.T) {
	resp := &pluginpb.InvokeHookResponse{
		Action: pluginpb.HookAction_HOOK_ACTION_ABORT,
		Body:   []byte("forged body"),
	}
	sanitizeHookAbortResponse(resp, TierTrusted, "demo", "on_hook", "s3:GetObject")

	require.Empty(t, resp.Body)
	require.Equal(t, int32(403), resp.StatusCode)
}

func TestSanitizeHookAbortResponse_Tier2AllowsBody(t *testing.T) {
	resp := &pluginpb.InvokeHookResponse{
		Action:     pluginpb.HookAction_HOOK_ACTION_ABORT,
		StatusCode: 200,
		Body:       []byte("trusted body"),
	}
	sanitizeHookAbortResponse(resp, TierCore, "demo", "on_hook", "s3:GetObject")

	require.Equal(t, []byte("trusted body"), resp.Body)
	require.Equal(t, int32(200), resp.StatusCode)
}

func TestSanitizeHookAbortResponse_NonAbortIgnored(t *testing.T) {
	resp := &pluginpb.InvokeHookResponse{
		Action: pluginpb.HookAction_HOOK_ACTION_MODIFY,
		Body:   []byte("modified body"),
	}
	sanitizeHookAbortResponse(resp, TierUntrusted, "demo", "on_hook", "s3:GetObject")

	require.Equal(t, []byte("modified body"), resp.Body)
}

func TestSanitizeHookAbortResponse_EmptyBodyIgnored(t *testing.T) {
	resp := &pluginpb.InvokeHookResponse{
		Action:     pluginpb.HookAction_HOOK_ACTION_ABORT,
		StatusCode: 204,
	}
	sanitizeHookAbortResponse(resp, TierUntrusted, "demo", "on_hook", "s3:GetObject")

	require.Equal(t, int32(204), resp.StatusCode)
}

func TestHookRegistry(t *testing.T) {
	reg := newHookRegistry()

	h1 := &HookRecord{PluginName: "a", HookID: "a-0", Operation: "s3:GetObject", Priority: 10}
	h2 := &HookRecord{PluginName: "b", HookID: "b-0", Operation: "s3:GetObject", Priority: 5}
	reg.add(h1)
	reg.add(h2)

	require.Len(t, reg.forOperation("s3:GetObject"), 2)
	require.Len(t, reg.forOperation("s3:PutObject"), 0)
	require.Len(t, reg.all(), 2)

	reg.remove("a", "a-0")
	require.Len(t, reg.all(), 1)

	reg.clear("b")
	require.Len(t, reg.all(), 0)
}

func TestHookRegistry_SetPluginOrder(t *testing.T) {
	reg := newHookRegistry()
	reg.add(&HookRecord{PluginName: "b", HookID: "b-0", Operation: "s3:GetObject", Priority: 10})
	reg.add(&HookRecord{PluginName: "a", HookID: "a-0", Operation: "s3:GetObject", Priority: 5})
	reg.setPluginOrder(func(name string) int {
		if name == "a" {
			return 0
		}
		return 1
	})
	all := reg.all()
	require.Equal(t, "a", all[0].PluginName)
	require.Equal(t, "b", all[1].PluginName)
}

func TestCloneHookContext(t *testing.T) {
	ctx := &pluginpb.HookContext{
		Operation:    "s3:GetObject",
		Bucket:       "b",
		Key:          "k",
		Headers:      map[string]string{"x": "1"},
		Body:         []byte("body"),
		OriginalUser: "u",
	}
	clone := cloneHookContext(ctx)
	require.Equal(t, ctx.Operation, clone.Operation)
	require.Equal(t, ctx.Body, clone.Body)
	require.Equal(t, ctx.Headers, clone.Headers)

	// Mutating clone does not affect original.
	clone.Headers["x"] = "2"
	clone.Body[0] = 'B'
	require.Equal(t, "1", ctx.Headers["x"])
	require.Equal(t, byte('b'), ctx.Body[0])

	require.Nil(t, cloneHookContext(nil))
}

func TestLoader_ListHooks(t *testing.T) {
	loader := newTestLoader(t)
	defer loader.Shutdown()

	loader.hooks.add(&HookRecord{PluginName: "p", HookID: "p-0", Operation: "s3:GetObject", Handler: "on_hook"})

	resp, err := loader.ListHooks(context.Background(), &pluginpb.ListHooksRequest{})
	require.NoError(t, err)
	require.Len(t, resp.Hooks, 1)

	resp, err = loader.ListHooks(context.Background(), &pluginpb.ListHooksRequest{Operation: "s3:PutObject"})
	require.NoError(t, err)
	require.Len(t, resp.Hooks, 0)
}

func TestLoader_HookContextLifecycle(t *testing.T) {
	loader := newTestLoader(t)
	defer loader.Shutdown()

	inv := &hookInvocation{pluginName: "p", ctx: &pluginpb.HookContext{Operation: "s3:GetObject"}}
	handle := loader.allocateHookContext(inv)
	require.NotZero(t, handle)
	require.Equal(t, inv, loader.GetHookContext(handle))

	loader.releaseHookContext(handle)
	require.Nil(t, loader.GetHookContext(handle))
}

func TestLoader_RegisterHooksFromManifest(t *testing.T) {
	loader := newTestLoader(t)
	defer loader.Shutdown()

	manifest := &Manifest{
		ManifestVersion: SupportedManifestVersion,
		APIVersion:      HostAPICompatRange,
		Name:            "hooky",
		Version:         "1.0.0",
		Capabilities:    []string{"gateway:hook"},
		Hooks: []ManifestHook{
			{Type: "before", Operation: "s3:GetObject", Handler: "on_hook"},
		},
	}
	require.NoError(t, loader.registerHooksFromManifest(manifest))
	require.Len(t, loader.hooks.all(), 1)
}

func TestLoader_InvokeHookValidation(t *testing.T) {
	loader := newTestLoader(t)
	defer loader.Shutdown()

	ctx := context.Background()
	_, err := loader.InvokeHook(ctx, nil)
	require.Error(t, err)

	_, err = loader.InvokeHook(ctx, &pluginpb.InvokeHookRequest{PluginName: "", Handler: "on_hook"})
	require.Error(t, err)

	_, err = loader.InvokeHook(ctx, &pluginpb.InvokeHookRequest{PluginName: "missing", Handler: "on_hook"})
	require.Error(t, err)
}

func TestLogHookResult(t *testing.T) {
	logHookResult(zap.L(), "p", "on_hook", pluginpb.HookAction_HOOK_ACTION_CONTINUE)
}
