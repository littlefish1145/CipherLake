package pluginloader

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTopologicalOrder(t *testing.T) {
	records := []PluginRecord{
		{Name: "c", Manifest: rawManifestWithDeps("c", []string{"b"})},
		{Name: "a", Manifest: rawManifestWithDeps("a", nil)},
		{Name: "b", Manifest: rawManifestWithDeps("b", []string{"a"})},
	}
	order, err := topologicalOrder(records)
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b", "c"}, order)
}

func TestTopologicalOrder_DependencyOnMissingPluginIgnored(t *testing.T) {
	records := []PluginRecord{
		{Name: "a", Manifest: rawManifestWithDeps("a", []string{"missing"})},
	}
	order, err := topologicalOrder(records)
	require.NoError(t, err)
	require.Equal(t, []string{"a"}, order)
}

func TestTopologicalOrder_CycleDetected(t *testing.T) {
	records := []PluginRecord{
		{Name: "a", Manifest: rawManifestWithDeps("a", []string{"b"})},
		{Name: "b", Manifest: rawManifestWithDeps("b", []string{"a"})},
	}
	_, err := topologicalOrder(records)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cycle")
}

func TestInstallPlugin_MissingDependencyRejected(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	wasm := minimalValidWASM(t)
	dep := &Manifest{
		ManifestVersion:      SupportedManifestVersion,
		APIVersion:           ">=1.0.0 <2.0.0",
		Name:                 "child",
		Version:              "1.0.0",
		TrustTierRequested:   0,
		DependsOn:            []string{"parent"},
	}
	err := l.InstallPlugin(context.Background(), dep, wasm, nil, "")
	require.Error(t, err)
	require.True(t, IsErrMissingDependency(err))
}

func TestInstallPlugin_DependencyOrderHonored(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	wasm := minimalValidWASM(t)
	parent := &Manifest{
		ManifestVersion:      SupportedManifestVersion,
		APIVersion:           ">=1.0.0 <2.0.0",
		Name:                 "parent",
		Version:              "1.0.0",
		TrustTierRequested:   0,
		Hooks:                []ManifestHook{{Type: "before", Operation: "s3:GetObject", Handler: "on_hook", Priority: 99}},
	}
	child := &Manifest{
		ManifestVersion:      SupportedManifestVersion,
		APIVersion:           ">=1.0.0 <2.0.0",
		Name:                 "child",
		Version:              "1.0.0",
		TrustTierRequested:   0,
		DependsOn:            []string{"parent"},
		Hooks:                []ManifestHook{{Type: "before", Operation: "s3:GetObject", Handler: "on_hook", Priority: 1}},
	}

	require.NoError(t, l.InstallPlugin(context.Background(), parent, wasm, nil, ""))
	require.NoError(t, l.InstallPlugin(context.Background(), child, wasm, nil, ""))

	// Parent has lower priority (99) but is a dependency, so it must run first.
	hooks := l.hooks.forOperation("s3:GetObject")
	require.Len(t, hooks, 2)
	require.Equal(t, "parent", hooks[0].PluginName)
	require.Equal(t, "child", hooks[1].PluginName)
}

func rawManifestWithDeps(name string, deps []string) []byte {
	m := &Manifest{
		ManifestVersion:    SupportedManifestVersion,
		APIVersion:         ">=1.0.0 <2.0.0",
		Name:               name,
		Version:            "1.0.0",
		TrustTierRequested: 0,
		DependsOn:          deps,
	}
	b, _ := json.Marshal(m)
	return b
}
