package common

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type ContextKey string

const (
	ContextKeyRequestID ContextKey = "request_id"
	ContextKeyUserID    ContextKey = "user_id"
	ContextKeyBucket    ContextKey = "bucket"
	ContextKeyObjectKey ContextKey = "object_key"
	ContextKeyTraceID   ContextKey = "trace_id"
)

func GetRequestID(ctx context.Context) string {
	if id, ok := ctx.Value(ContextKeyRequestID).(string); ok {
		return id
	}
	return uuid.New().String()
}

func GetUserID(ctx context.Context) string {
	if id, ok := ctx.Value(ContextKeyUserID).(string); ok {
		return id
	}
	return ""
}

func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, ContextKeyRequestID, requestID)
}

func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, ContextKeyUserID, userID)
}

type ObjectMetadata struct {
	Key             string            `json:"key"`
	Bucket          string            `json:"bucket"`
	Size            int64             `json:"size"`
	ContentType     string            `json:"content_type"`
	ContentEncoding string            `json:"content_encoding"`
	ETag            string            `json:"etag"`
	UserMetadata    map[string]string `json:"user_metadata"`
	StorageTier     StorageTier       `json:"storage_tier"`
	CreatedAt       time.Time         `json:"created_at"`
	ModifiedAt      time.Time         `json:"modified_at"`
	AccessCount     int64             `json:"access_count"`
	LastAccessedAt  time.Time         `json:"last_accessed_at"`
	Encrypted       bool              `json:"encrypted"`
	Vectorized      bool              `json:"vectorized"`
	VersionID       string            `json:"version_id"`
}

type StorageTier int

const (
	TierHot StorageTier = iota
	TierWarm
	TierCold
	TierArchive
)

func (t StorageTier) String() string {
	switch t {
	case TierHot:
		return "HOT"
	case TierWarm:
		return "WARM"
	case TierCold:
		return "COLD"
	case TierArchive:
		return "ARCHIVE"
	default:
		return "UNKNOWN"
	}
}

func ParseStorageTier(s string) StorageTier {
	switch s {
	case "HOT", "hot", "Hot":
		return TierHot
	case "WARM", "warm", "Warm":
		return TierWarm
	case "COLD", "cold", "Cold":
		return TierCold
	case "ARCHIVE", "archive", "Archive":
		return TierArchive
	default:
		return TierCold
	}
}

type AccessPattern struct {
	ObjectKey        string      `json:"object_key"`
	Bucket           string      `json:"bucket"`
	AccessTimestamps []time.Time `json:"access_timestamps"`
	AccessTypes      []string    `json:"access_types"`
	UserIDs          []string    `json:"user_ids"`
	HotnessScore     float64     `json:"hotness_score"`
	LastMigratedAt   time.Time   `json:"last_migrated_at"`
	PriorityWeight   int         `json:"priority_weight"`
	FixedTier        bool        `json:"fixed_tier"`
}

type VectorMetadata struct {
	ObjectKey string    `json:"object_key"`
	Bucket    string    `json:"bucket"`
	Dimension int       `json:"dimension"`
	IndexType string    `json:"index_type"`
	CreatedAt time.Time `json:"created_at"`
	ModelName string    `json:"model_name"`
	Checksum  string    `json:"checksum"`
}

type PipelineMetadata struct {
	ObjectKey      string            `json:"object_key"`
	Bucket         string            `json:"bucket"`
	PipelineName   string            `json:"pipeline_name"`
	DerivedObjects []string          `json:"derived_objects"`
	Status         PipelineStatus    `json:"status"`
	TriggeredAt    time.Time         `json:"triggered_at"`
	CompletedAt    *time.Time        `json:"completed_at,omitempty"`
	Error          string            `json:"error,omitempty"`
	InputHash      string            `json:"input_hash"`
	OutputMetadata map[string]string `json:"output_metadata"`
}

type PipelineStatus string

const (
	PipelineStatusPending   PipelineStatus = "pending"
	PipelineStatusRunning   PipelineStatus = "running"
	PipelineStatusCompleted PipelineStatus = "completed"
	PipelineStatusFailed    PipelineStatus = "failed"
	PipelineStatusSkipped   PipelineStatus = "skipped"
)

type TokenType string

const (
	TokenTypeWrite  TokenType = "write"
	TokenTypeRead   TokenType = "read"
	TokenTypeDelete TokenType = "delete"
)

type KMSDelegationToken struct {
	TokenID     string    `json:"token_id"`
	TokenType   TokenType `json:"token_type"`
	UserID      string    `json:"user_id"`
	Bucket      string    `json:"bucket"`
	ObjectKey   string    `json:"object_key,omitempty"`
	Expiry      time.Time `json:"expiry"`
	CreatedAt   time.Time `json:"created_at"`
	Operations  []string  `json:"operations"`
	ContentHash string    `json:"content_hash,omitempty"`
	Signature   []byte    `json:"signature,omitempty"`
}

func (t *KMSDelegationToken) IsExpired() bool {
	return time.Now().After(t.Expiry)
}

func (t *KMSDelegationToken) IsValidOperation(op string) bool {
	for _, allowed := range t.Operations {
		if allowed == op || allowed == "*" {
			return true
		}
	}
	return false
}

func (t *KMSDelegationToken) Sign(privKey ed25519.PrivateKey) error {
	clone := *t
	clone.Signature = nil
	data, err := json.Marshal(&clone)
	if err != nil {
		return fmt.Errorf("failed to marshal token for signing: %w", err)
	}
	sig := ed25519.Sign(privKey, data)
	t.Signature = sig
	return nil
}

func (t *KMSDelegationToken) Verify(pubKey ed25519.PublicKey) error {
	if time.Now().After(t.Expiry) {
		return fmt.Errorf("token expired")
	}
	sig := t.Signature
	if sig == nil {
		return fmt.Errorf("token has no signature")
	}
	t.Signature = nil
	data, err := json.Marshal(t)
	t.Signature = sig
	if err != nil {
		return fmt.Errorf("failed to marshal token for verification: %w", err)
	}
	if !ed25519.Verify(pubKey, data, sig) {
		return fmt.Errorf("invalid signature")
	}
	return nil
}
