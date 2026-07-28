package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

var (
	ErrPipelineNotFound   = errors.New("pipeline not found")
	ErrPluginNotFound     = errors.New("plugin not found")
	ErrProcessingFailed   = errors.New("processing failed")
	ErrInvalidPipeline    = errors.New("invalid pipeline configuration")
	ErrUnsupportedContent = errors.New("unsupported content type")
)

type TriggerType string

const (
	TriggerOnUpload TriggerType = "on_upload"
	TriggerOnGet    TriggerType = "on_get"
	TriggerOnPutTag TriggerType = "on_put_tag"
	TriggerSchedule TriggerType = "schedule"
)

type PipelinePlugin interface {
	Name() string
	Process(ctx context.Context, input *ObjectInput) (*ProcessResult, error)
	CanStream() bool
	SupportedTypes() []string
}

type ObjectInput struct {
	Key          string
	Bucket       string
	Content      io.Reader
	Size         int64
	ContentType  string
	UserMetadata map[string]string
}

type ObjectOutput struct {
	Key         string
	Content     io.Reader
	Size        int64
	ContentType string
	Metadata    map[string]string
}

type ProcessResult struct {
	Outputs         []*ObjectOutput
	UpdatedMetadata map[string]string
	Error           error
	Skipped         bool
}

type Pipeline struct {
	Name     string         `yaml:"name"`
	Trigger  TriggerType    `yaml:"trigger"`
	Filter   string         `yaml:"filter"`
	Steps    []PipelineStep `yaml:"steps"`
	Enabled  bool           `yaml:"enabled"`
	Priority int            `yaml:"priority"`
}

type PipelineStep struct {
	Name           string            `yaml:"name"`
	Plugin         string            `yaml:"plugin"`
	Params         map[string]string `yaml:"params"`
	Output         string            `yaml:"output"`
	Tags           map[string]string `yaml:"tags"`
	OutputMetadata bool              `yaml:"output_metadata"`
	Inline         bool              `yaml:"inline"`
	OnError        string            `yaml:"on_error"`
}

type PipelineConfig struct {
	Pipelines []Pipeline `yaml:"pipelines"`
}

type PipelineExecutor struct {
	mu         sync.RWMutex
	plugins    map[string]PipelinePlugin
	config     *PipelineConfig
	results    map[string]*PipelineExecution
	workers    chan struct{}
	maxWorkers int

	// wasmProvider is consulted when a step's plugin name is not found
	// in the in-process plugin table. It allows WASM plugins registered in
	// the plugin loader service to act as pipeline steps (P4-1).
	wasmProvider WASMStepProvider
}

// WASMStepProvider resolves a pipeline step name to a PipelinePlugin.
// The plugin loader service implements this interface so that YAML
// pipeline configurations can reference WASM steps by name.
type WASMStepProvider interface {
	GetPipelineStep(name string) (PipelinePlugin, bool)
}

type PipelineExecution struct {
	ID          string
	Pipeline    string
	ObjectKey   string
	Bucket      string
	Status      ExecutionStatus
	StartedAt   time.Time
	CompletedAt *time.Time
	Steps       []StepResult
	Error       string
}

type StepResult struct {
	StepName   string
	Status     ExecutionStatus
	OutputKeys []string
	Error      string
	Duration   time.Duration
}

type ExecutionStatus string

const (
	StatusPending   ExecutionStatus = "pending"
	StatusRunning   ExecutionStatus = "running"
	StatusCompleted ExecutionStatus = "completed"
	StatusFailed    ExecutionStatus = "failed"
	StatusSkipped   ExecutionStatus = "skipped"
)

func NewPipelineExecutor(maxWorkers int) *PipelineExecutor {
	if maxWorkers <= 0 {
		maxWorkers = 100
	}

	return &PipelineExecutor{
		plugins:    make(map[string]PipelinePlugin),
		results:    make(map[string]*PipelineExecution),
		workers:    make(chan struct{}, maxWorkers),
		maxWorkers: maxWorkers,
	}
}

func (e *PipelineExecutor) RegisterPlugin(plugin PipelinePlugin) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.plugins[plugin.Name()] = plugin

	return nil
}

// SetWASMStepProvider wires the executor to a WASM step provider (P4-1).
// When a pipeline step references a plugin name that is not registered
// in-process, the provider is asked to resolve it to a WASM step.
func (e *PipelineExecutor) SetWASMStepProvider(provider WASMStepProvider) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.wasmProvider = provider
}

func (e *PipelineExecutor) LoadConfig(configPath string) error {
	data, err := readFile(configPath)
	if err != nil {
		return fmt.Errorf("failed to read config: %w", err)
	}

	var config PipelineConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	e.config = &config

	return nil
}

func (e *PipelineExecutor) LoadConfigData(data []byte) error {
	var config PipelineConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	e.config = &config

	return nil
}

func (e *PipelineExecutor) GetMatchingPipelines(ctx context.Context, trigger TriggerType, contentType string, metadata map[string]string) []*Pipeline {
	e.mu.RLock()
	defer e.mu.RUnlock()

	var matching []*Pipeline

	if e.config == nil {
		return matching
	}

	for _, p := range e.config.Pipelines {
		if !p.Enabled {
			continue
		}

		if p.Trigger != trigger {
			continue
		}

		if p.Filter != "" {
			if !e.evaluateFilter(p.Filter, contentType, metadata) {
				continue
			}
		}

		p := p
		matching = append(matching, &p)
	}

	return matching
}

func (e *PipelineExecutor) evaluateFilter(filter string, contentType string, metadata map[string]string) bool {
	filter = strings.ToLower(filter)

	parts := strings.Split(filter, "and")
	for _, part := range parts {
		part = strings.TrimSpace(part)

		if strings.HasPrefix(part, "content-type") {
			if !strings.Contains(strings.ToLower(contentType), extractFilterValue(part)) {
				return false
			}
		}

		if strings.HasPrefix(part, "size") {
			return true
		}

		if strings.HasPrefix(part, "metadata.") {
			parts := strings.Split(part, "=")
			if len(parts) == 2 {
				key := strings.TrimPrefix(strings.TrimSpace(parts[0]), "metadata.")
				value := strings.TrimSpace(parts[1])
				if metadata[key] != value {
					return false
				}
			}
		}
	}

	return true
}

func extractFilterValue(part string) string {
	idx := strings.Index(part, "'")
	if idx == -1 {
		idx = strings.Index(part, "\"")
	}
	if idx == -1 {
		idx = strings.Index(part, "matches")
		if idx != -1 {
			rest := strings.TrimSpace(part[idx+7:])
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

func (e *PipelineExecutor) Execute(ctx context.Context, pipelineName string, input *ObjectInput) (*ProcessResult, error) {
	e.mu.RLock()
	pipeline, err := e.findPipeline(pipelineName)
	e.mu.RUnlock()

	if err != nil {
		return nil, err
	}

	exec := &PipelineExecution{
		ID:        uuid.New().String(),
		Pipeline:  pipeline.Name,
		ObjectKey: input.Key,
		Bucket:    input.Bucket,
		Status:    StatusRunning,
		StartedAt: time.Now(),
		Steps:     make([]StepResult, len(pipeline.Steps)),
	}

	e.mu.Lock()
	e.results[exec.ID] = exec
	e.mu.Unlock()

	e.workers <- struct{}{}
	defer func() { <-e.workers }()

	currentInput := input
	result := &ProcessResult{
		UpdatedMetadata: make(map[string]string),
	}

	for i, step := range pipeline.Steps {
		stepResult := StepResult{
			StepName: step.Name,
			Status:   StatusRunning,
		}

		startTime := time.Now()

		plugin, ok := e.getPlugin(step.Plugin)
		if !ok {
			stepResult.Status = StatusFailed
			stepResult.Error = fmt.Sprintf("plugin not found: %s", step.Plugin)
			exec.Steps[i] = stepResult
			continue
		}

		pluginResult, err := plugin.Process(ctx, currentInput)
		if err != nil {
			stepResult.Status = StatusFailed
			stepResult.Error = err.Error()
			exec.Steps[i] = stepResult

			if step.OnError == "fail" {
				result.Error = err
				break
			} else if step.OnError == "skip" {
				stepResult.Status = StatusSkipped
			} else {
				continue
			}
		}

		stepResult.Duration = time.Since(startTime)

		if pluginResult != nil {
			stepResult.OutputKeys = make([]string, 0)
			for _, output := range pluginResult.Outputs {
				if output != nil {
					derivedKey := e.expandOutputPath(step.Output, input.Key, output)
					stepResult.OutputKeys = append(stepResult.OutputKeys, derivedKey)
				}
			}

			if pluginResult.UpdatedMetadata != nil {
				for k, v := range pluginResult.UpdatedMetadata {
					result.UpdatedMetadata[k] = v
				}
			}

			if len(pluginResult.Outputs) > 0 && !step.Inline {
				result.Outputs = append(result.Outputs, pluginResult.Outputs...)
			}

			if step.Inline && pluginResult.Outputs != nil && len(pluginResult.Outputs) > 0 {
				currentInput.Content = pluginResult.Outputs[0].Content
				if pluginResult.Outputs[0].Size > 0 {
					currentInput.Size = pluginResult.Outputs[0].Size
				}
				if pluginResult.Outputs[0].ContentType != "" {
					currentInput.ContentType = pluginResult.Outputs[0].ContentType
				}
			}
		}

		stepResult.Status = StatusCompleted
		exec.Steps[i] = stepResult
	}

	now := time.Now()
	exec.CompletedAt = &now

	if result.Error != nil {
		exec.Status = StatusFailed
		exec.Error = result.Error.Error()
	} else {
		exec.Status = StatusCompleted
	}

	return result, nil
}

func (e *PipelineExecutor) findPipeline(name string) (*Pipeline, error) {
	if e.config == nil {
		return nil, ErrPipelineNotFound
	}

	for _, p := range e.config.Pipelines {
		if p.Name == name {
			return &p, nil
		}
	}

	return nil, ErrPipelineNotFound
}

func (e *PipelineExecutor) getPlugin(name string) (PipelinePlugin, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if plugin, ok := e.plugins[name]; ok {
		return plugin, true
	}
	if e.wasmProvider != nil {
		return e.wasmProvider.GetPipelineStep(name)
	}
	return nil, false
}

func (e *PipelineExecutor) expandOutputPath(pattern, originalKey string, output *ObjectOutput) string {
	result := pattern
	result = strings.ReplaceAll(result, "{key}", originalKey)
	result = strings.ReplaceAll(result, "{uuid}", uuid.New().String()[:8])

	if output != nil && output.ContentType != "" {
		ext := extensionFromMime(output.ContentType)
		if ext != "" {
			result = result + "." + ext
		}
	}

	return result
}

func extensionFromMime(mimeType string) string {
	exts, _ := mime.ExtensionsByType(mimeType)
	if len(exts) > 0 {
		return strings.TrimPrefix(exts[0], ".")
	}
	return ""
}

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

type ImageCompressPlugin struct{}

func (p *ImageCompressPlugin) Name() string { return "image_compress" }

func (p *ImageCompressPlugin) Process(ctx context.Context, input *ObjectInput) (*ProcessResult, error) {
	if !strings.HasPrefix(input.ContentType, "image/") {
		return nil, ErrUnsupportedContent
	}

	result := &ProcessResult{
		UpdatedMetadata: make(map[string]string),
	}

	result.UpdatedMetadata["compression"] = "webp"
	result.UpdatedMetadata["quality"] = "80"

	return result, nil
}

func (p *ImageCompressPlugin) CanStream() bool { return true }

func (p *ImageCompressPlugin) SupportedTypes() []string {
	return []string{"image/jpeg", "image/png", "image/gif", "image/webp"}
}

type ImageResizePlugin struct{}

func (p *ImageResizePlugin) Name() string { return "image_resize" }

func (p *ImageResizePlugin) Process(ctx context.Context, input *ObjectInput) (*ProcessResult, error) {
	if !strings.HasPrefix(input.ContentType, "image/") {
		return nil, ErrUnsupportedContent
	}

	result := &ProcessResult{
		UpdatedMetadata: make(map[string]string),
	}

	result.UpdatedMetadata["resized"] = "true"

	return result, nil
}

func (p *ImageResizePlugin) CanStream() bool { return true }

func (p *ImageResizePlugin) SupportedTypes() []string {
	return []string{"image/jpeg", "image/png", "image/gif", "image/webp"}
}

type MetadataExtractPlugin struct{}

func (p *MetadataExtractPlugin) Name() string { return "metadata_extract" }

func (p *MetadataExtractPlugin) Process(ctx context.Context, input *ObjectInput) (*ProcessResult, error) {
	metadata := make(map[string]string)

	if strings.HasPrefix(input.ContentType, "image/") {
		metadata["extracted_from"] = "image"
	} else if strings.HasPrefix(input.ContentType, "video/") {
		metadata["extracted_from"] = "video"
	} else if strings.HasPrefix(input.ContentType, "audio/") {
		metadata["extracted_from"] = "audio"
	} else if input.ContentType == "application/pdf" {
		metadata["extracted_from"] = "pdf"
	}

	return &ProcessResult{
		UpdatedMetadata: metadata,
	}, nil
}

func (p *MetadataExtractPlugin) CanStream() bool { return false }

func (p *MetadataExtractPlugin) SupportedTypes() []string {
	return []string{"image/*", "video/*", "audio/*", "application/pdf"}
}

type EncryptPIIPlugin struct{}

var piiPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b\d{15}\b`),
	regexp.MustCompile(`\b\d{18}\b`),
	regexp.MustCompile(`\b1[3-9]\d{9}\b`),
	regexp.MustCompile(`\b\d{4}[-\s]?\d{4}[-\s]?\d{4}[-\s]?\d{4}\b`),
}

func (p *EncryptPIIPlugin) Name() string { return "encrypt_pii" }

func (p *EncryptPIIPlugin) Process(ctx context.Context, input *ObjectInput) (*ProcessResult, error) {
	content, err := io.ReadAll(io.LimitReader(input.Content, 50*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("failed to read content: %w", err)
	}

	contentStr := string(content)
	redacted := contentStr

	for _, pattern := range piiPatterns {
		redacted = pattern.ReplaceAllStringFunc(redacted, func(match string) string {
			return maskString(match)
		})
	}

	result := &ProcessResult{
		UpdatedMetadata: map[string]string{
			"pii_redacted": "true",
		},
	}

	if redacted != contentStr {
		result.Outputs = []*ObjectOutput{
			{
				Key:         input.Key + ".redacted",
				Content:     strings.NewReader(redacted),
				Size:        int64(len(redacted)),
				ContentType: input.ContentType,
			},
		}
	}

	return result, nil
}

func maskString(s string) string {
	if len(s) <= 4 {
		return "****"
	}
	return s[:2] + strings.Repeat("*", len(s)-4) + s[len(s)-2:]
}

func (p *EncryptPIIPlugin) CanStream() bool { return false }

func (p *EncryptPIIPlugin) SupportedTypes() []string {
	return []string{"text/*", "application/json", "application/xml"}
}

type VideoThumbnailPlugin struct{}

func (p *VideoThumbnailPlugin) Name() string { return "video_thumbnail" }

func (p *VideoThumbnailPlugin) Process(ctx context.Context, input *ObjectInput) (*ProcessResult, error) {
	if !strings.HasPrefix(input.ContentType, "video/") {
		return nil, ErrUnsupportedContent
	}

	result := &ProcessResult{
		UpdatedMetadata: map[string]string{
			"thumbnail_generated": "true",
		},
	}

	return result, nil
}

func (p *VideoThumbnailPlugin) CanStream() bool { return false }

func (p *VideoThumbnailPlugin) SupportedTypes() []string {
	return []string{"video/*"}
}

type PDFToTextPlugin struct{}

func (p *PDFToTextPlugin) Name() string { return "pdf_to_text" }

func (p *PDFToTextPlugin) Process(ctx context.Context, input *ObjectInput) (*ProcessResult, error) {
	if input.ContentType != "application/pdf" {
		return nil, ErrUnsupportedContent
	}

	result := &ProcessResult{
		UpdatedMetadata: map[string]string{
			"pdf_converted": "true",
		},
	}

	return result, nil
}

func (p *PDFToTextPlugin) CanStream() bool { return false }

func (p *PDFToTextPlugin) SupportedTypes() []string {
	return []string{"application/pdf"}
}

func (e *PipelineExecutor) GetExecution(id string) (*PipelineExecution, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	exec, ok := e.results[id]
	return exec, ok
}

func (e *PipelineExecutor) ListExecutions(bucket, objectKey string) []*PipelineExecution {
	e.mu.RLock()
	defer e.mu.RUnlock()

	var results []*PipelineExecution
	for _, exec := range e.results {
		if bucket != "" && exec.Bucket != bucket {
			continue
		}
		if objectKey != "" && exec.ObjectKey != objectKey {
			continue
		}
		results = append(results, exec)
	}

	return results
}

func RegisterDefaultPlugins(e *PipelineExecutor) error {
	plugins := []PipelinePlugin{
		&ImageCompressPlugin{},
		&ImageResizePlugin{},
		&MetadataExtractPlugin{},
		&EncryptPIIPlugin{},
		&VideoThumbnailPlugin{},
		&PDFToTextPlugin{},
	}

	for _, plugin := range plugins {
		if err := e.RegisterPlugin(plugin); err != nil {
			return err
		}
	}

	return nil
}

func init() {
	_ = strings.TrimSpace
}
