package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// OPAClient integrates with Open Policy Agent for policy decisions
type OPAClient struct {
	addr       string
	httpClient *http.Client
	timeout    time.Duration
}

// OPAConfig defines configuration for OPA client
type OPAConfig struct {
	Address string        // OPA server address, e.g., "http://localhost:8181"
	Timeout time.Duration // Request timeout
}

// PolicyRequest represents input for OPA policy evaluation
type PolicyRequest struct {
	User    UserContext    `json:"user"`
	Object  ObjectContext  `json:"object"`
	Action  ActionContext  `json:"action"`
	Context RequestContext `json:"context"`
}

// UserContext contains user-related information
type UserContext struct {
	ID                string              `json:"id"`
	Name              string              `json:"name"`
	Role              string              `json:"role"`
	Permissions       []string            `json:"permissions"`
	BucketPermissions map[string][]string `json:"bucket_permissions,omitempty"`
	Attributes        map[string]string   `json:"attributes,omitempty"`
}

// ObjectContext contains object-related information
type ObjectContext struct {
	Bucket      string            `json:"bucket"`
	Key         string            `json:"key"`
	Size        int64             `json:"size,omitempty"`
	ContentType string            `json:"content_type,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Encrypted   bool              `json:"encrypted,omitempty"`
	Owner       string            `json:"owner,omitempty"`
	CreatedAt   time.Time         `json:"created_at,omitempty"`
}

// ActionContext contains action-related information
type ActionContext struct {
	Type      string `json:"type"`      // read, write, delete, list
	Operation string `json:"operation"` // specific operation like get_object, put_object
}

// RequestContext contains request-related information
type RequestContext struct {
	SourceIP string            `json:"source_ip"`
	Time     time.Time         `json:"time"`
	Method   string            `json:"method"`
	Path     string            `json:"path"`
	Headers  map[string]string `json:"headers,omitempty"`
	TraceID  string            `json:"trace_id,omitempty"`
}

// PolicyResponse represents OPA policy decision
type PolicyResponse struct {
	Result bool   `json:"result"`
	Reason string `json:"reason,omitempty"`
}

// NewOPAClient creates a new OPA client
func NewOPAClient(cfg OPAConfig) *OPAClient {
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}

	return &OPAClient{
		addr:    cfg.Address,
		timeout: cfg.Timeout,
		httpClient: &http.Client{
			Timeout: cfg.Timeout,
		},
	}
}

// Evaluate evaluates a policy against the given input
func (o *OPAClient) Evaluate(ctx context.Context, policyPath string, input PolicyRequest) (*PolicyResponse, error) {
	url := fmt.Sprintf("%s/v1/data/%s", o.addr, policyPath)

	inputJSON, err := json.Marshal(map[string]interface{}{
		"input": input,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal policy input: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(inputJSON))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request to OPA: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("OPA returned status %d: %s", resp.StatusCode, string(body))
	}

	var opaResp struct {
		Result bool   `json:"result"`
		Reason string `json:"reason,omitempty"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&opaResp); err != nil {
		return nil, fmt.Errorf("failed to decode OPA response: %w", err)
	}

	zap.L().Debug("opa policy evaluated",
		zap.String("policy_path", policyPath),
		zap.Bool("result", opaResp.Result),
		zap.String("user_id", input.User.ID),
		zap.String("bucket", input.Object.Bucket),
		zap.String("action", input.Action.Type))

	return &PolicyResponse{
		Result: opaResp.Result,
		Reason: opaResp.Reason,
	}, nil
}

// EvaluateAccess evaluates access policy for a user on an object
func (o *OPAClient) EvaluateAccess(ctx context.Context, user UserContext, object ObjectContext, action ActionContext, requestCtx RequestContext) (bool, error) {
	input := PolicyRequest{
		User:    user,
		Object:  object,
		Action:  action,
		Context: requestCtx,
	}

	resp, err := o.Evaluate(ctx, "cipherlake/access/allow", input)
	if err != nil {
		return false, err
	}

	return resp.Result, nil
}

// EvaluateEncryption evaluates encryption policy for an object
func (o *OPAClient) EvaluateEncryption(ctx context.Context, object ObjectContext) (bool, error) {
	input := PolicyRequest{
		Object: object,
		Action: ActionContext{
			Type:      "encrypt",
			Operation: "encrypt_object",
		},
	}

	resp, err := o.Evaluate(ctx, "cipherlake/encryption/required", input)
	if err != nil {
		return false, err
	}

	return resp.Result, nil
}

// Health checks OPA server health
func (o *OPAClient) Health(ctx context.Context) error {
	url := fmt.Sprintf("%s/health", o.addr)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create health check request: %w", err)
	}

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("OPA health check failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("OPA returned status %d", resp.StatusCode)
	}

	return nil
}
