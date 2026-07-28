package pluginloader

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	"cipherlake/internal/logger"
	"cipherlake/internal/pipeline"
)

// pipelineStepRegistry maps pipeline step names to the plugin that owns them.
// Step names are global within a Loader process and are referenced by YAML
// pipeline configurations (spec §3.12, P4-1).
type pipelineStepRegistry struct {
	mu       sync.RWMutex
	steps    map[string]string // step name -> plugin name
	byPlugin map[string]map[string]struct{}
}

func newPipelineStepRegistry() *pipelineStepRegistry {
	return &pipelineStepRegistry{
		steps:    make(map[string]string),
		byPlugin: make(map[string]map[string]struct{}),
	}
}

func (r *pipelineStepRegistry) register(stepName, pluginName string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.steps[stepName]; ok && existing != pluginName {
		return ErrPipelineStepConflict{Step: stepName, ExistingPlugin: existing}
	}
	r.steps[stepName] = pluginName
	if r.byPlugin[pluginName] == nil {
		r.byPlugin[pluginName] = make(map[string]struct{})
	}
	r.byPlugin[pluginName][stepName] = struct{}{}
	return nil
}

func (r *pipelineStepRegistry) lookup(stepName string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	pluginName, ok := r.steps[stepName]
	return pluginName, ok
}

func (r *pipelineStepRegistry) unregisterPlugin(pluginName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for stepName := range r.byPlugin[pluginName] {
		delete(r.steps, stepName)
	}
	delete(r.byPlugin, pluginName)
}

// pipelineInvocation is the per-on_pipeline_step call context.
type pipelineInvocation struct {
	pluginName   string
	stepName     string
	key          string
	bucket       string
	contentType  string
	userMetadata map[string]string
	body         []byte

	outputBody        []byte
	outputContentType string
	outputMetadata    map[string]string
}

// ErrPipelineStepConflict is returned when two plugins try to register the
// same pipeline step name.
type ErrPipelineStepConflict struct {
	Step           string
	ExistingPlugin string
}

func (e ErrPipelineStepConflict) Error() string {
	return fmt.Sprintf("pipeline step %q already registered by plugin %q", e.Step, e.ExistingPlugin)
}

// GetPipelineStep returns a pipeline.PipelinePlugin wrapper for a WASM
// pipeline step registered with the loader. It satisfies the
// pipeline.WASMStepProvider interface used by pipeline.PipelineExecutor.
func (l *Loader) GetPipelineStep(name string) (pipeline.PipelinePlugin, bool) {
	pluginName, ok := l.pipelineSteps.lookup(name)
	if !ok {
		return nil, false
	}
	return &wasmPipelinePlugin{
		loader:     l,
		pluginName: pluginName,
		stepName:   name,
	}, true
}

// wasmPipelinePlugin adapts a WASM pipeline step to the pipeline executor's
// PipelinePlugin interface.
type wasmPipelinePlugin struct {
	loader     *Loader
	pluginName string
	stepName   string
}

func (p *wasmPipelinePlugin) Name() string { return p.stepName }

func (p *wasmPipelinePlugin) CanStream() bool { return false }

func (p *wasmPipelinePlugin) SupportedTypes() []string { return []string{"*/*"} }

func (p *wasmPipelinePlugin) Process(ctx context.Context, input *pipeline.ObjectInput) (*pipeline.ProcessResult, error) {
	return p.loader.InvokePipelineStep(ctx, p.pluginName, p.stepName, input)
}

// InvokePipelineStep dispatches a single pipeline step to a plugin's
// on_pipeline_step entry point. The input body is passed through host imports
// (pipeline.read / pipeline.body) and the plugin writes its output via
// pipeline.output.write.
func (l *Loader) InvokePipelineStep(ctx context.Context, pluginName, stepName string, input *pipeline.ObjectInput) (*pipeline.ProcessResult, error) {
	holder, ok := l.pluginHolder(pluginName)
	if !ok {
		return nil, fmt.Errorf("plugin %q not installed", pluginName)
	}

	var body []byte
	if input.Content != nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(input.Content, 64<<20))
		if err != nil {
			return nil, fmt.Errorf("failed to read pipeline input body: %w", err)
		}
	}

	inv := &pipelineInvocation{
		pluginName:   pluginName,
		stepName:     stepName,
		key:          input.Key,
		bucket:       input.Bucket,
		contentType:  input.ContentType,
		userMetadata: input.UserMetadata,
		body:         body,
	}
	handle := l.allocatePipelineContext(inv)
	defer l.releasePipelineContext(handle)

	inst, err := holder.pool.Borrow(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to borrow plugin instance: %w", err)
	}
	defer holder.pool.Return(inst)

	logger.AuditLogger("plugin.pipeline.dispatch", pluginName, "", "allowed", map[string]interface{}{
		"step":       stepName,
		"key":        input.Key,
		"bucket":     input.Bucket,
		"body_bytes": len(body),
	})

	if _, err := inst.CallEntry(ctx, "on_pipeline_step", handle); err != nil {
		return nil, fmt.Errorf("pipeline step %q failed: %w", stepName, err)
	}

	result := &pipeline.ProcessResult{
		UpdatedMetadata: make(map[string]string),
	}
	if inv.outputMetadata != nil {
		for k, v := range inv.outputMetadata {
			result.UpdatedMetadata[k] = v
		}
	}
	if len(inv.outputBody) > 0 {
		result.Outputs = []*pipeline.ObjectOutput{
			{
				Key:         input.Key,
				Content:     bytes.NewReader(inv.outputBody),
				Size:        int64(len(inv.outputBody)),
				ContentType: inv.outputContentType,
				Metadata:    inv.outputMetadata,
			},
		}
	}
	return result, nil
}

// registerPipelineStepsFromManifest registers the pipeline steps declared in
// the plugin manifest. The plugin must already have been added to the in-memory
// plugin table. Errors are surfaced so that install can be rolled back.
func (l *Loader) registerPipelineStepsFromManifest(manifest *Manifest) error {
	for _, ps := range manifest.PipelineSteps {
		if err := l.pipelineSteps.register(ps.Name, manifest.Name); err != nil {
			return err
		}
	}
	return nil
}

// unregisterPluginPipelineSteps removes all pipeline step registrations for a
// plugin. Caller must hold l.mu (write lock); unloadPlugin is the only caller.
func (l *Loader) unregisterPluginPipelineSteps(pluginName string) {
	l.pipelineSteps.unregisterPlugin(pluginName)
}

// --- pipeline step handle management ---

func (l *Loader) allocatePipelineContext(inv *pipelineInvocation) uint64 {
	l.pipelineMu.Lock()
	defer l.pipelineMu.Unlock()
	l.nextPipelineHandle++
	if l.nextPipelineHandle == 0 {
		l.nextPipelineHandle++
	}
	h := l.nextPipelineHandle
	l.pipelineContexts[h] = inv
	return h
}

// GetPipelineContext returns the pipeline invocation for the given handle.
func (l *Loader) GetPipelineContext(handle uint64) *pipelineInvocation {
	l.pipelineMu.Lock()
	defer l.pipelineMu.Unlock()
	return l.pipelineContexts[handle]
}

func (l *Loader) releasePipelineContext(handle uint64) {
	l.pipelineMu.Lock()
	defer l.pipelineMu.Unlock()
	delete(l.pipelineContexts, handle)
}

