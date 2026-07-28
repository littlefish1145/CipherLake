package pipeline

import (
	"context"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wasmStepPlugin is a fake WASM pipeline step used to verify the executor's
// WASM fallback path without pulling in the real plugin loader service.
type wasmStepPlugin struct {
	name string
}

func (p *wasmStepPlugin) Name() string { return p.name }

func (p *wasmStepPlugin) CanStream() bool { return false }

func (p *wasmStepPlugin) SupportedTypes() []string { return []string{"*/*"} }

func (p *wasmStepPlugin) Process(ctx context.Context, input *ObjectInput) (*ProcessResult, error) {
	body, _ := io.ReadAll(input.Content)
	return &ProcessResult{
		UpdatedMetadata: map[string]string{
			"wasm_body_len": strconv.Itoa(len(body)),
			"wasm_key":      input.Key,
		},
	}, nil
}

type fakeWASMStepProvider struct {
	plugin PipelinePlugin
}

func (f *fakeWASMStepProvider) GetPipelineStep(name string) (PipelinePlugin, bool) {
	if f.plugin != nil && f.plugin.Name() == name {
		return f.plugin, true
	}
	return nil, false
}

func TestPipelineExecutor_WASMStepFallback(t *testing.T) {
	exec := NewPipelineExecutor(10)
	require.NoError(t, RegisterDefaultPlugins(exec))

	wasmPlugin := &wasmStepPlugin{name: "wasm_meta"}
	exec.SetWASMStepProvider(&fakeWASMStepProvider{plugin: wasmPlugin})

	configData := []byte(`
pipelines:
  - name: mixed-pipeline
    trigger: on_upload
    steps:
      - name: extract-metadata
        plugin: metadata_extract
      - name: wasm-step
        plugin: wasm_meta
    enabled: true
`)
	require.NoError(t, exec.LoadConfigData(configData))

	input := &ObjectInput{
		Key:          "photos/sunset.jpg",
		Bucket:       "test-bucket",
		Content:      strings.NewReader("fake image data"),
		Size:         15,
		ContentType:  "image/jpeg",
		UserMetadata: map[string]string{},
	}

	result, err := exec.Execute(context.Background(), "mixed-pipeline", input)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, "image", result.UpdatedMetadata["extracted_from"])
	assert.Equal(t, "15", result.UpdatedMetadata["wasm_body_len"])
	assert.Equal(t, "photos/sunset.jpg", result.UpdatedMetadata["wasm_key"])
}

func TestPipelineExecutor_WASMStepNotFound(t *testing.T) {
	exec := NewPipelineExecutor(10)
	require.NoError(t, RegisterDefaultPlugins(exec))
	exec.SetWASMStepProvider(&fakeWASMStepProvider{plugin: nil})

	configData := []byte(`
pipelines:
  - name: wasm-only-pipeline
    trigger: on_upload
    steps:
      - name: missing-step
        plugin: not_registered
    enabled: true
`)
	require.NoError(t, exec.LoadConfigData(configData))

	input := &ObjectInput{
		Key:         "docs/x.txt",
		Bucket:      "test-bucket",
		Content:     strings.NewReader("hello"),
		Size:        5,
		ContentType: "text/plain",
	}

	result, err := exec.Execute(context.Background(), "wasm-only-pipeline", input)
	require.NoError(t, err)
	require.NotNil(t, result)

	execs := exec.ListExecutions("test-bucket", "docs/x.txt")
	require.Len(t, execs, 1)
	require.Len(t, execs[0].Steps, 1)
	assert.Equal(t, StatusFailed, execs[0].Steps[0].Status)
	assert.Contains(t, execs[0].Steps[0].Error, "not_registered")
}
