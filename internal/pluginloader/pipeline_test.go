package pluginloader

import (
	"io"
	"strings"
	"testing"

	"cipherlake/internal/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// minimalPipelinePluginBytes is a hand-built WASM module that exports the
// mandatory entry points plus on_pipeline_step. All functions return i32 0.
//
// Type section:
//   t0 (i64) -> i32
//   t1 (i64, i64) -> i32
//   t2 (i64, i64, i64) -> i32
// Function section: [t0, t1, t2, t1, t1]
// Exports: memory, wasm_init(0), on_hook(1), on_request(2), on_task(3), on_pipeline_step(4)
var minimalPipelinePluginBytes = []byte{
	// WASM magic + version
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,

	// Type section (id=1, size=19)
	0x01, 0x13,
	0x03, // 3 types
	// t0: (i64) -> i32
	0x60, 0x01, 0x7e, 0x01, 0x7f,
	// t1: (i64, i64) -> i32
	0x60, 0x02, 0x7e, 0x7e, 0x01, 0x7f,
	// t2: (i64, i64, i64) -> i32
	0x60, 0x03, 0x7e, 0x7e, 0x7e, 0x01, 0x7f,

	// Function section (id=3, size=6)
	0x03, 0x06,
	0x05, 0x00, 0x01, 0x02, 0x01, 0x01,

	// Memory section (id=5, size=3)
	0x05, 0x03,
	0x01, 0x00, 0x01,

	// Export section (id=7, size=74)
	0x07, 0x4a,
	0x06, // 6 exports
	// "memory"
	0x06, 0x6d, 0x65, 0x6d, 0x6f, 0x72, 0x79, 0x02, 0x00,
	// "wasm_init"
	0x09, 0x77, 0x61, 0x73, 0x6d, 0x5f, 0x69, 0x6e, 0x69, 0x74, 0x00, 0x00,
	// "on_hook"
	0x07, 0x6f, 0x6e, 0x5f, 0x68, 0x6f, 0x6f, 0x6b, 0x00, 0x01,
	// "on_request"
	0x0a, 0x6f, 0x6e, 0x5f, 0x72, 0x65, 0x71, 0x75, 0x65, 0x73, 0x74, 0x00, 0x02,
	// "on_task"
	0x07, 0x6f, 0x6e, 0x5f, 0x74, 0x61, 0x73, 0x6b, 0x00, 0x03,
	// "on_pipeline_step"
	0x10, 0x6f, 0x6e, 0x5f, 0x70, 0x69, 0x70, 0x65, 0x6c, 0x69, 0x6e, 0x65, 0x5f, 0x73, 0x74, 0x65, 0x70, 0x00, 0x04,

	// Code section (id=10, size=26)
	0x0a, 0x1a,
	0x05, // 5 function bodies
	// body for wasm_init
	0x04, 0x00, 0x41, 0x00, 0x0b,
	// body for on_hook
	0x04, 0x00, 0x41, 0x00, 0x0b,
	// body for on_request
	0x04, 0x00, 0x41, 0x00, 0x0b,
	// body for on_task
	0x04, 0x00, 0x41, 0x00, 0x0b,
	// body for on_pipeline_step
	0x04, 0x00, 0x41, 0x00, 0x0b,
}

func TestManifest_PipelineStepValidation(t *testing.T) {
	m := &Manifest{
		ManifestVersion:    SupportedManifestVersion,
		APIVersion:         ">=1.0.0 <2.0.0",
		Name:               "pipeline-demo",
		Version:            "1.0.0",
		TrustTierRequested: 0,
		Capabilities:       []string{"pipeline:step:uppercase"},
		PipelineSteps:      []ManifestPipelineStep{{Name: "uppercase"}},
	}
	require.NoError(t, m.Validate())

	// Missing capability.
	bad := *m
	bad.Capabilities = nil
	require.Error(t, bad.Validate())

	// Invalid step name.
	bad = *m
	bad.PipelineSteps = []ManifestPipelineStep{{Name: "Upper Case"}}
	require.Error(t, bad.Validate())

	// Duplicate step names.
	bad = *m
	bad.PipelineSteps = []ManifestPipelineStep{{Name: "uppercase"}, {Name: "uppercase"}}
	require.Error(t, bad.Validate())
}

func TestLoader_RegisterPipelineStep(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	manifest := &Manifest{
		ManifestVersion:    SupportedManifestVersion,
		APIVersion:         ">=1.0.0 <2.0.0",
		Name:               "pipeline-demo",
		Version:            "1.0.0",
		TrustTierRequested: 0,
		Capabilities:       []string{"pipeline:step:uppercase"},
		PipelineSteps:      []ManifestPipelineStep{{Name: "uppercase"}},
	}
	require.NoError(t, manifest.Validate())

	ctx := testCtx(t)
	require.NoError(t, l.InstallPlugin(ctx, manifest, minimalPipelinePluginBytes, nil, ""))

	plugin, ok := l.GetPipelineStep("uppercase")
	require.True(t, ok)
	assert.Equal(t, "uppercase", plugin.Name())

	_, ok = l.GetPipelineStep("missing")
	assert.False(t, ok)
}

func TestLoader_InvokePipelineStep(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	manifest := &Manifest{
		ManifestVersion:    SupportedManifestVersion,
		APIVersion:         ">=1.0.0 <2.0.0",
		Name:               "pipeline-demo",
		Version:            "1.0.0",
		TrustTierRequested: 0,
		Capabilities:       []string{"pipeline:step:uppercase"},
		PipelineSteps:      []ManifestPipelineStep{{Name: "uppercase"}},
	}
	require.NoError(t, manifest.Validate())

	ctx := testCtx(t)
	require.NoError(t, l.InstallPlugin(ctx, manifest, minimalPipelinePluginBytes, nil, ""))

	input := &pipeline.ObjectInput{
		Key:          "docs/hello.txt",
		Bucket:       "test-bucket",
		Content:      strings.NewReader("hello wasm"),
		Size:         10,
		ContentType:  "text/plain",
		UserMetadata: map[string]string{"author": "test"},
	}
	result, err := l.InvokePipelineStep(ctx, "pipeline-demo", "uppercase", input)
	require.NoError(t, err)
	require.NotNil(t, result)

	// The stub plugin does not write output, so there should be no outputs.
	assert.Empty(t, result.Outputs)
	assert.Empty(t, result.UpdatedMetadata)
}

func TestLoader_PipelineStep_UninstallRemovesStep(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	manifest := &Manifest{
		ManifestVersion:    SupportedManifestVersion,
		APIVersion:         ">=1.0.0 <2.0.0",
		Name:               "pipeline-demo",
		Version:            "1.0.0",
		TrustTierRequested: 0,
		Capabilities:       []string{"pipeline:step:uppercase"},
		PipelineSteps:      []ManifestPipelineStep{{Name: "uppercase"}},
	}
	require.NoError(t, manifest.Validate())

	ctx := testCtx(t)
	require.NoError(t, l.InstallPlugin(ctx, manifest, minimalPipelinePluginBytes, nil, ""))
	require.True(t, func() bool { _, ok := l.GetPipelineStep("uppercase"); return ok }())

	l.unloadPlugin("pipeline-demo")
	_, ok := l.GetPipelineStep("uppercase")
	assert.False(t, ok)
}

func TestHostImports_PipelineReadAndOutput(t *testing.T) {
	l := newTestLoader(t)
	defer l.Shutdown()

	manifest := &Manifest{
		ManifestVersion:    SupportedManifestVersion,
		APIVersion:         ">=1.0.0 <2.0.0",
		Name:               "pipeline-host-test",
		Version:            "1.0.0",
		TrustTierRequested: 0,
		Capabilities:       []string{"pipeline:step:uppercase"},
		PipelineSteps:      []ManifestPipelineStep{{Name: "uppercase"}},
	}
	require.NoError(t, manifest.Validate())
	ctx := testCtx(t)
	require.NoError(t, l.InstallPlugin(ctx, manifest, minimalPipelinePluginBytes, nil, ""))

	h := NewHostImports(l, l.WASMRuntime().CapabilityTable(), nil, nil)
	mod := newTestMemoryModule(t, ctx, l.WASMRuntime().Runtime())
	defer mod.Close(ctx)
	mem := mod.Memory()

	tok, err := l.WASMRuntime().CapabilityTable().Issue("pipeline-host-test", 0, []string{"pipeline:step:uppercase"}, neverExpires())
	require.NoError(t, err)

	inv := &pipelineInvocation{
		pluginName:   "pipeline-host-test",
		stepName:     "uppercase",
		key:          "docs/x.txt",
		bucket:       "b",
		contentType:  "text/plain",
		userMetadata: map[string]string{"k": "v"},
		body:         []byte("hello"),
	}
	handle := l.allocatePipelineContext(inv)
	defer l.releasePipelineContext(handle)

	const base uint32 = 64
	h.pipelineRead(ctx, mod, uint64(tok), handle, base, 1024, base+1024, base+1028)
	require.Equal(t, statusOK, readTestUint32(t, mem, base+1028), "pipeline.read status")

	outLen := readTestUint32(t, mem, base+1024)
	outBytes, _ := mem.Read(base, outLen)
	require.Contains(t, string(outBytes), `"body_len":5`)

	// Write output.
	body := []byte("WORLD")
	ct := []byte("text/plain")
	meta := []byte(`{"converted":"true"}`)
	writeTestMemory(t, mem, base, body)
	writeTestMemory(t, mem, base+64, ct)
	writeTestMemory(t, mem, base+128, meta)
	h.pipelineOutputWrite(ctx, mod, uint64(tok), handle,
		base, uint32(len(body)),
		base+64, uint32(len(ct)),
		base+128, uint32(len(meta)),
		base+256,
	)
	require.Equal(t, statusOK, readTestUint32(t, mem, base+256), "pipeline.output.write status")
	assert.Equal(t, "WORLD", string(inv.outputBody))
	assert.Equal(t, "text/plain", inv.outputContentType)
	assert.Equal(t, "true", inv.outputMetadata["converted"])
}

var _ = io.EOF
