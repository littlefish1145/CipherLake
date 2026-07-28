package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"cipherlake/internal/config"
	"cipherlake/internal/logger"
	pluginpb "cipherlake/proto/plugin"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// HookOrchestrator executes S3 operation hooks via the plugin-loader
// microservice (spec §3.2, P2-4/P2-5). It implements before/after/around
// scheduling with an onion model for around hooks and applies the
// timeout + circuit-breaker + critical_hooks policy from spec §3.12.
type HookOrchestrator struct {
	client pluginpb.PluginLoaderServiceClient
	logger *zap.Logger

	callTimeout      time.Duration
	breakerThreshold int
	breakerPause     time.Duration
	criticalHooks    map[string]struct{}

	conditionEvaluator *hookConditionEvaluator

	breakerMu        sync.Mutex
	breakerFailures  int
	breakerOpenUntil time.Time
}

// NewHookOrchestrator creates an orchestrator backed by the given loader client
// and gateway plugin-loader configuration.
func NewHookOrchestrator(client pluginpb.PluginLoaderServiceClient, cfg config.PluginLoaderConfig) *HookOrchestrator {
	o := &HookOrchestrator{
		client:           client,
		logger:           zap.L(),
		callTimeout:      500 * time.Millisecond,
		breakerThreshold: 5,
		breakerPause:     30 * time.Second,
		criticalHooks:    make(map[string]struct{}),
	}
	if cfg.CallTimeout != "" {
		if d, err := time.ParseDuration(cfg.CallTimeout); err == nil && d > 0 {
			o.callTimeout = d
		}
	}
	if cfg.CriticalBreakerThreshold > 0 {
		o.breakerThreshold = cfg.CriticalBreakerThreshold
	}
	if cfg.CriticalBreakerPause != "" {
		if d, err := time.ParseDuration(cfg.CriticalBreakerPause); err == nil && d > 0 {
			o.breakerPause = d
		}
	}
	for _, op := range cfg.CriticalHooks {
		o.criticalHooks[op] = struct{}{}
	}
	if ce, err := newHookConditionEvaluator(); err == nil {
		o.conditionEvaluator = ce
	} else {
		o.logger.Warn("failed to create hook condition evaluator; CEL conditions will be skipped", zap.Error(err))
	}
	return o
}

// SetLogger replaces the default logger.
func (o *HookOrchestrator) SetLogger(l *zap.Logger) {
	o.logger = l
}

// breakerOpen reports whether the circuit breaker is currently open.
func (o *HookOrchestrator) breakerOpen() bool {
	o.breakerMu.Lock()
	defer o.breakerMu.Unlock()
	return time.Now().Before(o.breakerOpenUntil)
}

// recordTimeout increments the consecutive failure counter and opens the
// breaker if the threshold is reached.
func (o *HookOrchestrator) recordTimeout() {
	o.breakerMu.Lock()
	defer o.breakerMu.Unlock()
	o.breakerFailures++
	if o.breakerFailures >= o.breakerThreshold {
		o.breakerOpenUntil = time.Now().Add(o.breakerPause)
		o.logger.Warn("plugin loader circuit breaker opened",
			zap.Int("consecutive_timeouts", o.breakerFailures),
			zap.Time("open_until", o.breakerOpenUntil),
		)
		logger.AuditLogger("loader.circuit_breaker.open", "plugin-loader", "", "allowed", map[string]interface{}{
			"consecutive_timeouts": o.breakerFailures,
			"threshold":            o.breakerThreshold,
			"pause_seconds":        o.breakerPause.Seconds(),
			"open_until":           o.breakerOpenUntil.Format(time.RFC3339),
		})
	}
}

// recordSuccess resets the breaker failure counter.
func (o *HookOrchestrator) recordSuccess() {
	o.breakerMu.Lock()
	defer o.breakerMu.Unlock()
	if o.breakerFailures > 0 {
		wasOpen := time.Now().Before(o.breakerOpenUntil)
		o.breakerFailures = 0
		o.breakerOpenUntil = time.Time{}
		o.logger.Info("plugin loader circuit breaker reset after success")
		if wasOpen {
			logger.AuditLogger("loader.circuit_breaker.close", "plugin-loader", "", "allowed", map[string]interface{}{
				"reason": "successful loader call",
			})
		}
	}
}

// isCritical reports whether the given hook operation is in the critical
// hooks whitelist.
func (o *HookOrchestrator) isCritical(op string) bool {
	_, ok := o.criticalHooks[op]
	return ok
}

// HookResult is the aggregated outcome of a hook chain.
type HookResult struct {
	Action         pluginpb.HookAction
	StatusCode     int
	Headers        map[string]string
	Body           []byte
	ModifiedRequest *pluginpb.HookContext
}

// Execute runs the full hook chain for an S3 operation. The inner function
// represents the actual S3 handler; it is invoked only if no before/around
// hook aborts the request.
func (o *HookOrchestrator) Execute(ctx context.Context, operation string, reqCtx *pluginpb.HookContext, inner func(*pluginpb.HookContext) (*HookResult, error)) (*HookResult, error) {
	return o.ExecuteWithEnv(ctx, operation, reqCtx, nil, inner)
}

// ExecuteWithEnv is like Execute but evaluates per-hook CEL conditions using
// the supplied environment (spec §3.2, P5-4). If env is nil, conditions are
// evaluated with only the fields available from reqCtx.
func (o *HookOrchestrator) ExecuteWithEnv(ctx context.Context, operation string, reqCtx *pluginpb.HookContext, env *HookConditionEnv, inner func(*pluginpb.HookContext) (*HookResult, error)) (*HookResult, error) {
	if o.client == nil {
		return inner(reqCtx)
	}

	hooks, err := o.listHooks(ctx, operation)
	if err != nil {
		// Loader unreachable: critical operations fail-closed; otherwise
		// degrade to the inner S3 handler (hooks are skipped).
		if o.isCritical(operation) {
			logger.AuditLogger("critical.operation.blocked", operation, "", "denied", map[string]interface{}{
				"reason": err.Error(),
			})
			return nil, fmt.Errorf("critical operation %s: loader unreachable: %w", operation, err)
		}
		o.logger.Warn("failed to list hooks, degrading to inner handler",
			zap.String("operation", operation),
			zap.Error(err),
		)
		return inner(reqCtx)
	}
	if len(hooks) == 0 {
		return inner(reqCtx)
	}

	before, around, after := classifyHooks(hooks)

	currentCtx := cloneHookContext(reqCtx)

	// before hooks: ascending priority.
	for _, h := range before {
		if !o.conditionMatches(h, reqCtx, env) {
			continue
		}
		currentCtx.Phase = "before"
		resp, err := o.invokeHook(ctx, h, currentCtx)
		if err != nil {
			return nil, err
		}
		switch resp.Action {
		case pluginpb.HookAction_HOOK_ACTION_ABORT:
			return abortResult(resp), nil
		case pluginpb.HookAction_HOOK_ACTION_MODIFY:
			currentCtx = applyContext(currentCtx, resp.ModifiedContext)
		}
	}

	// around hooks: onion model. Highest priority is the outermost layer.
	var aroundInner func(layer int, hookCtx *pluginpb.HookContext) (*HookResult, error)
	aroundInner = func(layer int, hookCtx *pluginpb.HookContext) (*HookResult, error) {
		if layer == len(around) {
			return inner(hookCtx)
		}
		h := around[layer]
		if !o.conditionMatches(h, reqCtx, env) {
			return aroundInner(layer+1, hookCtx)
		}

		// around_before
		beforeCtx := cloneHookContext(hookCtx)
		beforeCtx.Phase = "around_before"
		resp, err := o.invokeHook(ctx, h, beforeCtx)
		if err != nil {
			return nil, err
		}
		switch resp.Action {
		case pluginpb.HookAction_HOOK_ACTION_ABORT:
			return abortResult(resp), nil
		case pluginpb.HookAction_HOOK_ACTION_MODIFY:
			beforeCtx = applyContext(beforeCtx, resp.ModifiedContext)
		}

		// Recurse into next layer / inner handler.
		innerResult, err := aroundInner(layer+1, beforeCtx)
		if err != nil {
			return nil, err
		}
		if innerResult.Action == pluginpb.HookAction_HOOK_ACTION_ABORT {
			return innerResult, nil
		}

		// around_after: inner result becomes the response context.
		afterCtx := resultToContext(innerResult)
		afterCtx.Phase = "around_after"
		afterCtx.Operation = reqCtx.Operation
		afterCtx.Bucket = reqCtx.Bucket
		afterCtx.Key = reqCtx.Key
		afterCtx.Method = reqCtx.Method
		afterCtx.OriginalUser = reqCtx.OriginalUser
		afterResp, err := o.invokeHook(ctx, h, afterCtx)
		if err != nil {
			return nil, err
		}
		switch afterResp.Action {
		case pluginpb.HookAction_HOOK_ACTION_ABORT:
			return abortResult(afterResp), nil
		case pluginpb.HookAction_HOOK_ACTION_MODIFY:
			applyResult(innerResult, afterResp.ModifiedContext)
		}
		return innerResult, nil
	}

	result, err := aroundInner(0, currentCtx)
	if err != nil {
		return nil, err
	}
	if result.Action == pluginpb.HookAction_HOOK_ACTION_ABORT {
		return result, nil
	}

	// after hooks: descending priority (reverse order).
	for i := len(after) - 1; i >= 0; i-- {
		h := after[i]
		if !o.conditionMatches(h, reqCtx, env) {
			continue
		}
		afterCtx := resultToContext(result)
		afterCtx.Phase = "after"
		afterCtx.Operation = reqCtx.Operation
		afterCtx.Bucket = reqCtx.Bucket
		afterCtx.Key = reqCtx.Key
		afterCtx.Method = reqCtx.Method
		afterCtx.OriginalUser = reqCtx.OriginalUser
		resp, err := o.invokeHook(ctx, h, afterCtx)
		if err != nil {
			return nil, err
		}
		switch resp.Action {
		case pluginpb.HookAction_HOOK_ACTION_ABORT:
			return abortResult(resp), nil
		case pluginpb.HookAction_HOOK_ACTION_MODIFY:
			applyResult(result, resp.ModifiedContext)
		}
	}

	return result, nil
}

// conditionMatches evaluates a hook's CEL condition against the request
// context. Empty conditions always match. Evaluation failures are logged and
// treated as "does not match" so a misconfigured hook is skipped rather than
// blocking the chain.
func (o *HookOrchestrator) conditionMatches(h *pluginpb.HookRecord, reqCtx *pluginpb.HookContext, env *HookConditionEnv) bool {
	if h.Condition == "" {
		return true
	}
	if o.conditionEvaluator == nil {
		return true
	}
	if env == nil {
		env = &HookConditionEnv{
			Operation: reqCtx.Operation,
			Bucket:    reqCtx.Bucket,
			Key:       reqCtx.Key,
			Method:    reqCtx.Method,
			Body:      reqCtx.Body,
			Headers:   reqCtx.Headers,
			UserARN:   reqCtx.OriginalUser,
			Time:      time.Now(),
		}
	}
	ok, err := o.conditionEvaluator.evaluate(h.Condition, env)
	if err != nil {
		o.logger.Warn("hook condition evaluation failed; skipping hook",
			zap.String("plugin", h.PluginName),
			zap.String("handler", h.Handler),
			zap.String("condition", h.Condition),
			zap.Error(err))
		return false
	}
	return ok
}

// listHooks fetches hooks for the operation from the loader. It applies the
// same timeout and breaker logic as invokeHook.
func (o *HookOrchestrator) listHooks(ctx context.Context, operation string) ([]*pluginpb.HookRecord, error) {
	if o.breakerOpen() {
		return nil, fmt.Errorf("loader unreachable (circuit breaker open)")
	}
	req := &pluginpb.ListHooksRequest{Operation: operation}
	callCtx, cancel := context.WithTimeout(ctx, o.callTimeout)
	defer cancel()
	resp, err := o.client.ListHooks(callCtx, req)
	if err != nil {
		if isTimeout(err) {
			o.recordTimeout()
		} else {
			o.recordSuccess()
		}
		return nil, err
	}
	o.recordSuccess()
	return resp.Hooks, nil
}

// invokeHook calls the loader for a single hook, applying the configured
// timeout. When the call fails, the hook's on_failure policy determines
// whether the error is surfaced (deny), logged and swallowed (log_and_allow),
// or silently swallowed (allow). The circuit breaker state is checked before
// calling the Loader.
func (o *HookOrchestrator) invokeHook(ctx context.Context, h *pluginpb.HookRecord, hookCtx *pluginpb.HookContext) (*pluginpb.InvokeHookResponse, error) {
	// Circuit breaker: if Loader is considered unreachable, critical hooks
	// fail-closed immediately; non-critical hooks follow on_failure.
	if o.breakerOpen() {
		return o.handleHookFailure(h, fmt.Errorf("loader unreachable (circuit breaker open)"))
	}

	req := &pluginpb.InvokeHookRequest{
		PluginName: h.PluginName,
		Handler:    h.Handler,
		Context:    hookCtx,
	}
	callCtx, cancel := context.WithTimeout(ctx, o.callTimeout)
	defer cancel()
	resp, err := o.client.InvokeHook(callCtx, req)
	if err != nil {
		o.logger.Warn("hook invocation failed",
			zap.String("plugin", h.PluginName),
			zap.String("handler", h.Handler),
			zap.Error(err),
		)
		return o.handleHookFailure(h, fmt.Errorf("hook %s/%s failed: %w", h.PluginName, h.Handler, err))
	}
	return resp, nil
}

// handleHookFailure applies the per-hook on_failure policy. It returns an
// error for deny (fail-closed), or a synthetic CONTINUE response for allow/
// log_and_allow.
func (o *HookOrchestrator) handleHookFailure(h *pluginpb.HookRecord, err error) (*pluginpb.InvokeHookResponse, error) {
	if o.isCritical(h.Operation) {
		logger.AuditLogger("critical.hook.blocked", h.PluginName, "", "denied", map[string]interface{}{
			"operation": h.Operation,
			"handler":   h.Handler,
			"reason":    err.Error(),
		})
		return nil, fmt.Errorf("critical hook %s/%s failed: %w", h.PluginName, h.Handler, err)
	}

	onFailure := h.OnFailure
	if onFailure == "" {
		onFailure = "log_and_allow"
	}
	switch onFailure {
	case "deny":
		return nil, err
	case "allow":
		return &pluginpb.InvokeHookResponse{Action: pluginpb.HookAction_HOOK_ACTION_CONTINUE}, nil
	case "log_and_allow":
		o.logger.Warn("hook failure swallowed per on_failure policy",
			zap.String("plugin", h.PluginName),
			zap.String("handler", h.Handler),
			zap.String("policy", onFailure),
		)
		return &pluginpb.InvokeHookResponse{Action: pluginpb.HookAction_HOOK_ACTION_CONTINUE}, nil
	default:
		return nil, err
	}
}

// isTimeout reports whether err is a context deadline / timeout error or a
// gRPC timeout. It treats context.Canceled as non-timeout.
func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	if err == context.DeadlineExceeded {
		return true
	}
	// gRPC codes are wrapped; check for DeadlineExceeded / Unavailable.
	if s, ok := status.FromError(err); ok {
		return s.Code() == codes.DeadlineExceeded || s.Code() == codes.Unavailable
	}
	return false
}

// classifyHooks splits hooks by type and sorts by priority desc.
func classifyHooks(hooks []*pluginpb.HookRecord) (before, around, after []*pluginpb.HookRecord) {
	for _, h := range hooks {
		switch h.HookType {
		case "before":
			before = append(before, h)
		case "around":
			around = append(around, h)
		case "after":
			after = append(after, h)
		}
	}
	sortByPriorityDesc(before)
	sortByPriorityDesc(around)
	sortByPriorityDesc(after)
	return
}

func sortByPriorityDesc(hooks []*pluginpb.HookRecord) {
	sort.SliceStable(hooks, func(i, j int) bool {
		return hooks[i].Priority > hooks[j].Priority
	})
}

func abortResult(resp *pluginpb.InvokeHookResponse) *HookResult {
	return &HookResult{
		Action:     pluginpb.HookAction_HOOK_ACTION_ABORT,
		StatusCode: int(resp.StatusCode),
		Headers:    resp.Headers,
		Body:       resp.Body,
	}
}

func cloneHookContext(ctx *pluginpb.HookContext) *pluginpb.HookContext {
	if ctx == nil {
		return nil
	}
	clone := &pluginpb.HookContext{
		Operation:    ctx.Operation,
		Phase:        ctx.Phase,
		Bucket:       ctx.Bucket,
		Key:          ctx.Key,
		Method:       ctx.Method,
		OriginalUser: ctx.OriginalUser,
		StatusCode:   ctx.StatusCode,
	}
	if len(ctx.Headers) > 0 {
		clone.Headers = make(map[string]string, len(ctx.Headers))
		for k, v := range ctx.Headers {
			clone.Headers[k] = v
		}
	}
	if len(ctx.Body) > 0 {
		clone.Body = make([]byte, len(ctx.Body))
		copy(clone.Body, ctx.Body)
	}
	return clone
}

func applyContext(base, patch *pluginpb.HookContext) *pluginpb.HookContext {
	if patch == nil {
		return base
	}
	if base == nil {
		return cloneHookContext(patch)
	}
	if patch.Bucket != "" {
		base.Bucket = patch.Bucket
	}
	if patch.Key != "" {
		base.Key = patch.Key
	}
	if patch.Method != "" {
		base.Method = patch.Method
	}
	if len(patch.Headers) > 0 {
		if base.Headers == nil {
			base.Headers = make(map[string]string)
		}
		for k, v := range patch.Headers {
			base.Headers[k] = v
		}
	}
	if len(patch.Body) > 0 {
		base.Body = make([]byte, len(patch.Body))
		copy(base.Body, patch.Body)
	}
	if patch.StatusCode != 0 {
		base.StatusCode = patch.StatusCode
	}
	return base
}

func resultToContext(r *HookResult) *pluginpb.HookContext {
	return &pluginpb.HookContext{
		StatusCode: int32(r.StatusCode),
		Headers:    cloneHeaders(r.Headers),
		Body:       r.Body,
	}
}

func applyResult(r *HookResult, ctx *pluginpb.HookContext) {
	if ctx == nil {
		return
	}
	if ctx.StatusCode != 0 {
		r.StatusCode = int(ctx.StatusCode)
	}
	if len(ctx.Headers) > 0 {
		r.Headers = cloneHeaders(ctx.Headers)
	}
	if len(ctx.Body) > 0 {
		r.Body = make([]byte, len(ctx.Body))
		copy(r.Body, ctx.Body)
	}
}

func cloneHeaders(h map[string]string) map[string]string {
	if h == nil {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = v
	}
	return out
}

// hookResponseRecorder captures an S3 handler's response so it can be fed
// into after/around_after hooks and written after the chain completes.
type hookResponseRecorder struct {
	http.ResponseWriter
	statusCode int
	headers    http.Header
	body       []byte
	written    bool
}

func newHookResponseRecorder(w http.ResponseWriter) *hookResponseRecorder {
	return &hookResponseRecorder{
		ResponseWriter: w,
		statusCode:     http.StatusOK,
		headers:        make(http.Header),
	}
}

func (r *hookResponseRecorder) WriteHeader(code int) {
	if r.written {
		return
	}
	r.statusCode = code
	r.written = true
}

func (r *hookResponseRecorder) Write(b []byte) (int, error) {
	if !r.written {
		r.WriteHeader(http.StatusOK)
	}
	r.body = append(r.body, b...)
	return len(b), nil
}

func (r *hookResponseRecorder) Header() http.Header {
	return r.headers
}

func (r *hookResponseRecorder) toResult() *HookResult {
	return &HookResult{
		Action:     pluginpb.HookAction_HOOK_ACTION_CONTINUE,
		StatusCode: r.statusCode,
		Headers:    headerToMap(r.headers),
		Body:       r.body,
	}
}

func headerToMap(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vals := range h {
		if len(vals) == 1 {
			out[k] = vals[0]
		} else {
			out[k] = ""
		}
	}
	return out
}

// writeResult writes the final hook result to the real ResponseWriter.
func writeResult(w http.ResponseWriter, result *HookResult) {
	for k, v := range result.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(result.StatusCode)
	if len(result.Body) > 0 {
		_, _ = w.Write(result.Body)
	}
}

// newHookContext builds a HookContext from an HTTP request.
func newHookContext(operation string, r *http.Request, bucket, key string) *pluginpb.HookContext {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(nil) // body consumed; caller should restore if needed
	return &pluginpb.HookContext{
		Operation:    operation,
		Bucket:       bucket,
		Key:          key,
		Method:       r.Method,
		Headers:      headerToMap(r.Header),
		Body:         body,
		OriginalUser: "", // filled by caller if auth identity is available
	}
}
