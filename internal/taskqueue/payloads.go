package taskqueue

// Well-known task kinds used by the gateway background pipeline.
const (
	KindVectorize = "vectorize"
	KindFTS       = "fts"
	KindPipeline  = "pipeline"
)

// VectorizePayload carries the parameters for a vector-indexing task.
type VectorizePayload struct {
	Bucket      string            `json:"bucket"`
	Key         string            `json:"key"`
	ContentType string            `json:"content_type"`
	Metadata    map[string]string `json:"metadata"`
	UserID      string            `json:"user_id"`
}

// FTSPayload carries the parameters for a full-text indexing task.
type FTSPayload struct {
	Bucket      string `json:"bucket"`
	Key         string `json:"key"`
	ContentType string `json:"content_type"`
	VersionID   string `json:"version_id"`
}

// PipelinePayload carries the parameters for a pipeline execution task.
type PipelinePayload struct {
	Bucket      string            `json:"bucket"`
	Key         string            `json:"key"`
	ContentType string            `json:"content_type"`
	Metadata    map[string]string `json:"metadata"`
}
