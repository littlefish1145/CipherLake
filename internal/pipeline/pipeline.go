package pipeline

import "nexus/internal/flow"

type PipelineExecutor = flow.Executor
type PipelinePlugin = flow.WorkflowPlugin
type ObjectInput = flow.ObjectInput
type ObjectOutput = flow.ObjectOutput
type ProcessResult = flow.ProcessResult
type Pipeline = flow.Workflow
type PipelineStep = flow.Step
type PipelineConfig = flow.WorkflowConfig
type PipelineExecution = flow.Execution
type StepResult = flow.StepResult
type ExecutionStatus = flow.ExecutionStatus

const (
	TriggerOnUpload = flow.TriggerOnUpload
	TriggerOnGet    = flow.TriggerOnGet
	TriggerOnPutTag = flow.TriggerOnPutTag
	TriggerSchedule = flow.TriggerSchedule

	StatusPending   = flow.StatusPending
	StatusRunning   = flow.StatusRunning
	StatusCompleted = flow.StatusCompleted
	StatusFailed    = flow.StatusFailed
	StatusSkipped   = flow.StatusSkipped
)

var (
	ErrPipelineNotFound   = flow.ErrWorkflowNotFound
	ErrPluginNotFound     = flow.ErrPluginNotFound
	ErrProcessingFailed   = flow.ErrProcessingFailed
	ErrInvalidPipeline    = flow.ErrInvalidWorkflow
	ErrUnsupportedContent = flow.ErrUnsupportedContent
)

func NewPipelineExecutor(maxWorkers int) *PipelineExecutor {
	return flow.NewExecutor(maxWorkers, nil)
}

func NewPipelineExecutorWithTelemetry(maxWorkers int, tele *flow.Telemetry) *PipelineExecutor {
	return flow.NewExecutor(maxWorkers, tele)
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
	for _, p := range plugins {
		if err := e.RegisterPlugin(p); err != nil {
			return err
		}
	}
	return nil
}
