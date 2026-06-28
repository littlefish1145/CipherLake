package flow

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type Executor struct {
	mu       sync.RWMutex
	plugins  map[string]WorkflowPlugin
	compiled []*CompiledWorkflow
	results  map[string]*Execution
	events   chan struct{}
	active   int32
	maxWorkers int32
	telemetry *Telemetry
}

func NewExecutor(maxWorkers int, telemetry *Telemetry) *Executor {
	if maxWorkers <= 0 {
		maxWorkers = 100
	}
	return &Executor{
		plugins:    make(map[string]WorkflowPlugin),
		results:    make(map[string]*Execution),
		maxWorkers: int32(maxWorkers),
		telemetry:  telemetry,
	}
}

func (e *Executor) RegisterPlugin(plugin WorkflowPlugin) error {
	name := plugin.Name()
	if name == "" {
		return fmt.Errorf("plugin name cannot be empty")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.plugins[name] = plugin
		if e.telemetry != nil {
		e.telemetry.EventBus.Publish(Event{
			ID:       newID(),
			Type:     EventPluginLoaded,
			Time:     time.Now(),
			Workflow: name,
			Metadata: map[string]string{"type": fmt.Sprintf("%T", plugin)},
		})
	}
	return nil
}

func (e *Executor) LoadConfig(path string) error {
	cfg, err := LoadConfigFile(path)
	if err != nil {
		return err
	}
	return e.LoadConfigDataRaw(cfg)
}

func (e *Executor) LoadConfigData(data []byte) error {
	cfg, err := LoadConfigData(data)
	if err != nil {
		return err
	}
	return e.LoadConfigDataRaw(cfg)
}

func (e *Executor) LoadConfigDataRaw(cfg *WorkflowConfig) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	compiler := NewCompiler(e.plugins)
	compiled, err := compiler.Compile(cfg)
	if err != nil {
		return err
	}
	e.compiled = compiled
	return nil
}

func (e *Executor) GetMatchingPipelines(ctx context.Context, trigger TriggerType, contentType string, metadata map[string]string) []*CompiledWorkflow {
	e.mu.RLock()
	defer e.mu.RUnlock()

	var matching []*CompiledWorkflow
	for _, cw := range e.compiled {
		if cw.Workflow.Trigger != trigger {
			continue
		}
		if !MatchFilter(cw.Workflow.Filter, contentType, metadata) {
			continue
		}
		matching = append(matching, cw)
	}
	return matching
}

func (e *Executor) GetMatching(ctx context.Context, trigger TriggerType, contentType string, metadata map[string]string) []*CompiledWorkflow {
	return e.GetMatchingPipelines(ctx, trigger, contentType, metadata)
}

func (e *Executor) Execute(ctx context.Context, workflowName string, input *ObjectInput) (*ProcessResult, error) {
	e.mu.RLock()
	var cw *CompiledWorkflow
	for _, c := range e.compiled {
		if c.Workflow.Name == workflowName {
			cw = c
			break
		}
	}
	e.mu.RUnlock()

	if cw == nil {
		return nil, ErrWorkflowNotFound
	}

	exec := &Execution{
		ID:        newID(),
		Workflow:  cw.Workflow.Name,
		ObjectKey: input.Key,
		Bucket:    input.Bucket,
		Status:    StatusRunning,
		StartedAt: time.Now(),
		Steps:     make([]StepResult, len(cw.Steps)),
	}

	e.mu.Lock()
	e.results[exec.ID] = exec
	e.mu.Unlock()

	tele := e.telemetry

	if tele != nil {
		tele.EventBus.Publish(Event{
			ID:        newID(),
			Type:      EventWorkflowStarted,
			Time:      time.Now(),
			Workflow:  exec.Workflow,
			Execution: exec.ID,
			Metadata:  input.UserMetadata,
		})
		tele.Metrics.GaugeInc("workflow_running", "workflow", exec.Workflow)
	}

	currentInput := input
	result := &ProcessResult{
		UpdatedMetadata: make(map[string]string),
	}

	for i, cs := range cw.Steps {
		stepResult := StepResult{
			StepName: cs.Step.Name,
			Status:   StatusRunning,
		}

		if tele != nil {
			tele.EventBus.Publish(Event{
				ID:        newID(),
				Type:      EventStepStarted,
				Time:      time.Now(),
				Workflow:  exec.Workflow,
				Step:      cs.Step.Name,
				Execution: exec.ID,
			})
		}

		startTime := time.Now()
		currentInput.Params = cs.Step.Params

		pluginResult, err := cs.Plugin.Process(ctx, currentInput)
		duration := time.Since(startTime)

		if err != nil {
			stepResult.Status = StatusFailed
			stepResult.Error = err.Error()
			exec.Steps[i] = stepResult

			if tele != nil {
				tele.EventBus.Publish(Event{
					ID:        newID(),
					Type:      EventStepFailed,
					Time:      time.Now(),
					Workflow:  exec.Workflow,
					Step:      cs.Step.Name,
					Execution: exec.ID,
					Duration:  duration,
					Error:     err.Error(),
				})
			}

			if cs.Step.OnError == "fail" {
				result.Error = err
				break
			} else if cs.Step.OnError == "skip" {
				stepResult.Status = StatusSkipped
				if tele != nil {
					tele.EventBus.Publish(Event{
						ID:   newID(),
						Type: EventStepSkipped,
						Time: time.Now(),
					})
				}
			}
			continue
		}

		stepResult.Duration = duration

		if pluginResult != nil {
			stepResult.OutputKeys = make([]string, 0)
			for _, output := range pluginResult.Outputs {
				if output != nil {
					derivedKey := expandOutputPath(cs.Step.Output, input.Key, output)
					stepResult.OutputKeys = append(stepResult.OutputKeys, derivedKey)
				}
			}

			if pluginResult.UpdatedMetadata != nil {
				for k, v := range pluginResult.UpdatedMetadata {
					result.UpdatedMetadata[k] = v
				}
			}

			if len(pluginResult.Outputs) > 0 && !cs.Step.Inline {
				result.Outputs = append(result.Outputs, pluginResult.Outputs...)
			}

			if cs.Step.Inline && len(pluginResult.Outputs) > 0 {
				out := pluginResult.Outputs[0]
				currentInput.Content = out.Content
				if out.Size > 0 {
					currentInput.Size = out.Size
				}
				if out.ContentType != "" {
					currentInput.ContentType = out.ContentType
				}
			}
		}

		stepResult.Status = StatusCompleted
		exec.Steps[i] = stepResult

		if tele != nil {
			tele.EventBus.Publish(Event{
				ID:        newID(),
				Type:      EventStepCompleted,
				Time:      time.Now(),
				Workflow:  exec.Workflow,
				Step:      cs.Step.Name,
				Execution: exec.ID,
				Duration:  duration,
			})
		}
	}

	now := time.Now()
	exec.CompletedAt = &now

	if result.Error != nil {
		exec.Status = StatusFailed
		exec.Error = result.Error.Error()
		if tele != nil {
			tele.EventBus.Publish(Event{
				ID:        newID(),
				Type:      EventWorkflowFailed,
				Time:      now,
				Workflow:  exec.Workflow,
				Execution: exec.ID,
				Error:     exec.Error,
			})
			tele.Metrics.GaugeDec("workflow_running", "workflow", exec.Workflow)
			tele.Metrics.CounterInc("workflow_failed", "workflow", exec.Workflow)
		}
	} else {
		exec.Status = StatusCompleted
		if tele != nil {
			totalDuration := now.Sub(exec.StartedAt)
			tele.EventBus.Publish(Event{
				ID:        newID(),
				Type:      EventWorkflowCompleted,
				Time:      now,
				Workflow:  exec.Workflow,
				Execution: exec.ID,
				Duration:  totalDuration,
			})
			tele.Metrics.GaugeDec("workflow_running", "workflow", exec.Workflow)
			tele.Metrics.CounterInc("workflow_completed", "workflow", exec.Workflow)
			tele.Metrics.HistogramObserve("workflow_duration", totalDuration.Seconds(), "workflow", exec.Workflow)
		}
	}

	return result, nil
}

func (e *Executor) GetExecution(id string) (*Execution, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	exec, ok := e.results[id]
	return exec, ok
}

func (e *Executor) ListExecutions(bucket, objectKey string) []*Execution {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var result []*Execution
	for _, exec := range e.results {
		if bucket != "" && exec.Bucket != bucket {
			continue
		}
		if objectKey != "" && exec.ObjectKey != objectKey {
			continue
		}
		result = append(result, exec)
	}
	return result
}

func (e *Executor) Plugins() map[string]WorkflowPlugin {
	e.mu.RLock()
	defer e.mu.RUnlock()
	cp := make(map[string]WorkflowPlugin, len(e.plugins))
	for k, v := range e.plugins {
		cp[k] = v
	}
	return cp
}
