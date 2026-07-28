package flow

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"
)

func newID() string { return uuid.New().String() }

var (
	ErrWorkflowNotFound   = errors.New("workflow not found")
	ErrPluginNotFound     = errors.New("plugin not found")
	ErrProcessingFailed   = errors.New("processing failed")
	ErrInvalidWorkflow    = errors.New("invalid workflow configuration")
	ErrUnsupportedContent = errors.New("unsupported content type")
)

type TriggerType string

const (
	TriggerOnUpload TriggerType = "on_upload"
	TriggerOnGet    TriggerType = "on_get"
	TriggerOnPutTag TriggerType = "on_put_tag"
	TriggerSchedule TriggerType = "schedule"
)

type ExecutionStatus string

const (
	StatusPending   ExecutionStatus = "pending"
	StatusRunning   ExecutionStatus = "running"
	StatusCompleted ExecutionStatus = "completed"
	StatusFailed    ExecutionStatus = "failed"
	StatusSkipped   ExecutionStatus = "skipped"
)

type Workflow struct {
	Name     string      `yaml:"name"`
	Trigger  TriggerType `yaml:"trigger"`
	Filter   string      `yaml:"filter"`
	Steps    []Step      `yaml:"steps"`
	Enabled  bool        `yaml:"enabled"`
	Priority int         `yaml:"priority"`
}

type Step struct {
	Name           string            `yaml:"name"`
	Plugin         string            `yaml:"plugin"`
	Params         map[string]string `yaml:"params"`
	Output         string            `yaml:"output"`
	Tags           map[string]string `yaml:"tags"`
	OutputMetadata bool              `yaml:"output_metadata"`
	Inline         bool              `yaml:"inline"`
	OnError        string            `yaml:"on_error"`
}

type WorkflowConfig struct {
	Workflows []Workflow `yaml:"pipelines"`
}

type ObjectInput struct {
	Key          string
	Bucket       string
	Content      io.Reader
	Size         int64
	ContentType  string
	UserMetadata map[string]string
	Params       map[string]string
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

type WorkflowPlugin interface {
	Name() string
	Process(ctx context.Context, input *ObjectInput) (*ProcessResult, error)
	CanStream() bool
	SupportedTypes() []string
}

type Execution struct {
	ID          string
	Workflow    string
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
