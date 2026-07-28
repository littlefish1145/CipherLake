package pluginloader

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"cipherlake/internal/logger"
	pluginpb "cipherlake/proto/plugin"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// hookRegistry holds the in-memory view of registered S3 hooks.
// It is reconstructed from the FSM on startup and updated on install/uninstall.
type hookRegistry struct {
	mu          sync.RWMutex
	hooks       []*HookRecord
	pluginOrder func(string) int
}

func newHookRegistry() *hookRegistry {
	return &hookRegistry{
		hooks:       make([]*HookRecord, 0),
		pluginOrder: func(string) int { return 0 },
	}
}

// setPluginOrder sets the function used to determine plugin dependency order.
// Hooks are sorted by dependency order ascending, then priority descending.
func (r *hookRegistry) setPluginOrder(fn func(string) int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pluginOrder = fn
	r.resortLocked()
}

// add inserts a hook and keeps the registry sorted by (dependency order asc, priority desc).
func (r *hookRegistry) add(h *HookRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hooks = append(r.hooks, h)
	r.resortLocked()
}

// resortLocked re-sorts hooks using the current pluginOrder. Caller must hold lock.
func (r *hookRegistry) resortLocked() {
	order := r.pluginOrder
	sort.SliceStable(r.hooks, func(i, j int) bool {
		oi := order(r.hooks[i].PluginName)
		oj := order(r.hooks[j].PluginName)
		if oi != oj {
			return oi < oj
		}
		return r.hooks[i].Priority > r.hooks[j].Priority
	})
}

// remove deletes a hook by key (plugin_name:hook_id).
func (r *hookRegistry) remove(pluginName, hookID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := pluginName + ":" + hookID
	out := r.hooks[:0]
	for _, h := range r.hooks {
		if h.PluginName+":"+h.HookID != key {
			out = append(out, h)
		}
	}
	r.hooks = out
}

// clear removes all hooks for a plugin.
func (r *hookRegistry) clear(pluginName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.hooks[:0]
	for _, h := range r.hooks {
		if h.PluginName != pluginName {
			out = append(out, h)
		}
	}
	r.hooks = out
}

// forOperation returns a copy of hooks matching the operation, sorted by priority desc.
func (r *hookRegistry) forOperation(op string) []*HookRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*HookRecord
	for _, h := range r.hooks {
		if h.Operation == op {
			out = append(out, h)
		}
	}
	return out
}

// all returns a copy of all hooks.
func (r *hookRegistry) all() []*HookRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*HookRecord, len(r.hooks))
	copy(out, r.hooks)
	return out
}

// hookInvocation is the per-on_hook call context.
type hookInvocation struct {
	pluginName string
	handler    string
	ctx        *pluginpb.HookContext

	mu           sync.Mutex
	modified     *pluginpb.HookContext
	shortCircuit bool
	abortStatus  int32
	abortHeaders map[string]string
	abortBody    []byte
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

// hookModifyPatch is the JSON payload accepted by hook.modify.
type hookModifyPatch struct {
	Headers      map[string]string `json:"headers,omitempty"`
	BodyB64      string            `json:"body_b64,omitempty"`
	StatusCode   int32             `json:"status_code,omitempty"`
	ShortCircuit bool              `json:"short_circuit,omitempty"`
}

// InstallPlugin registers hooks declared in the manifest.
func (l *Loader) registerHooksFromManifest(manifest *Manifest) error {
	for i, h := range manifest.Hooks {
		record := HookRecord{
			PluginName: manifest.Name,
			HookID:     fmt.Sprintf("%s-%d", manifest.Name, i),
			HookType:   h.Type,
			Operation:  h.Operation,
			Condition:  h.Condition,
			Handler:    h.Handler,
			Priority:   h.Priority,
			OnFailure:  h.OnFailure,
			Critical:   h.Critical,
		}
		recordJSON, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("failed to marshal hook record: %w", err)
		}
		op := &LoaderFSMOp{
			Type:   OpHookRegister,
			Plugin: manifest.Name,
			Key:    record.HookID,
			Data:   recordJSON,
		}
		if err := l.RaftApply(context.Background(), op); err != nil {
			return fmt.Errorf("failed to register hook %s: %w", record.HookID, err)
		}
		l.hooks.add(&record)
	}
	return nil
}

// registerHookInMemory adds a hook record to the in-memory registry.
func (l *Loader) registerHookInMemory(record *HookRecord) {
	l.hooks.add(record)
}

// unregisterPluginHooks removes all hooks for a plugin from the in-memory registry.
func (l *Loader) unregisterPluginHooks(pluginName string) {
	l.hooks.clear(pluginName)
}

// RegisterHook registers a single hook dynamically. It persists the hook
// through Raft and adds it to the in-memory registry. It is used by the
// gateway.hook.register-for-other Tier-2-only host import (spec §3.7 A4, P5-5).
func (l *Loader) RegisterHook(ctx context.Context, pluginName string, record *HookRecord) error {
	if record == nil {
		return fmt.Errorf("record is nil")
	}
	record.PluginName = pluginName
	if record.HookID == "" {
		record.HookID = fmt.Sprintf("%s-dynamic-%d", pluginName, time.Now().UnixNano())
	}
	recordJSON, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("failed to marshal hook record: %w", err)
	}
	op := &LoaderFSMOp{
		Type:   OpHookRegister,
		Plugin: pluginName,
		Key:    record.HookID,
		Data:   recordJSON,
	}
	if err := l.RaftApply(ctx, op); err != nil {
		return fmt.Errorf("failed to apply hook registration: %w", err)
	}
	l.hooks.add(record)
	return nil
}

// InvokeHook dispatches a single hook call to a plugin's on_hook entry point.
//
// The plugin receives a hook handle via the second argument. It reads the
// context with hook.read / hook.body and can mutate it with hook.modify.
// The on_hook return value is interpreted as:
//   0 = CONTINUE
//   1 = ABORT (plugin must have set short_circuit via hook.modify)
//   2 = MODIFY (context mutated, continue chain)
//   >=100 = error
func (l *Loader) InvokeHook(ctx context.Context, req *pluginpb.InvokeHookRequest) (*pluginpb.InvokeHookResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is nil")
	}
	if req.PluginName == "" {
		return nil, status.Error(codes.InvalidArgument, "plugin_name is required")
	}
	if req.Handler == "" {
		return nil, status.Error(codes.InvalidArgument, "handler is required")
	}

	l.mu.RLock()
	holder, ok := l.plugins[req.PluginName]
	reloading := holder != nil && holder.reloading
	l.mu.RUnlock()
	if !ok {
		logger.AuditLogger("hook.invoke", req.PluginName, "", "denied", map[string]interface{}{
			"handler": req.Handler,
			"reason":  "plugin not installed",
		})
		return nil, status.Errorf(codes.NotFound, "plugin %q not installed", req.PluginName)
	}
	if reloading {
		logger.AuditLogger("hook.invoke", req.PluginName, "", "denied", map[string]interface{}{
			"handler": req.Handler,
			"reason":  "plugin reloading",
		})
		return nil, status.Errorf(codes.Unavailable, "plugin %q is reloading", req.PluginName)
	}

	inv := &hookInvocation{
		pluginName: req.PluginName,
		handler:    req.Handler,
		ctx:        cloneHookContext(req.Context),
	}
	handle := l.allocateHookContext(inv)
	defer l.releaseHookContext(handle)

	inst, err := holder.pool.Borrow(ctx)
	if err != nil {
		logger.AuditLogger("hook.invoke", req.PluginName, "", "denied", map[string]interface{}{
			"handler": req.Handler,
			"reason":  fmt.Sprintf("borrow instance: %v", err),
		})
		return nil, status.Errorf(codes.ResourceExhausted, "failed to borrow plugin instance: %v", err)
	}
	defer holder.pool.Return(inst)

	// on_hook(token, hook_handle)
	statusCode, err := inst.CallEntry(ctx, req.Handler, handle)
	if err != nil {
		logger.AuditLogger("hook.invoke", req.PluginName, "", "denied", map[string]interface{}{
			"handler": req.Handler,
			"reason":  fmt.Sprintf("on_hook error: %v", err),
		})
		return nil, status.Errorf(codes.Internal, "plugin on_hook failed: %v", err)
	}

	inv.mu.Lock()
	modified := inv.modified
	shortCircuit := inv.shortCircuit
	abortStatus := inv.abortStatus
	abortHeaders := inv.abortHeaders
	abortBody := inv.abortBody
	inv.mu.Unlock()

	action := pluginpb.HookAction_HOOK_ACTION_CONTINUE
	resp := &pluginpb.InvokeHookResponse{}

	switch statusCode {
	case 0:
		if modified != nil {
			action = pluginpb.HookAction_HOOK_ACTION_MODIFY
			resp.ModifiedContext = modified
		} else {
			action = pluginpb.HookAction_HOOK_ACTION_CONTINUE
		}
	case 1:
		action = pluginpb.HookAction_HOOK_ACTION_ABORT
		resp.StatusCode = abortStatus
		resp.Headers = abortHeaders
		resp.Body = abortBody
	case 2:
		action = pluginpb.HookAction_HOOK_ACTION_MODIFY
		if modified == nil {
			modified = cloneHookContext(req.Context)
		}
		resp.ModifiedContext = modified
	default:
		return nil, status.Errorf(codes.Internal, "plugin on_hook returned invalid status %d", statusCode)
	}

	// If the plugin called hook.short-circuit, treat as abort regardless of return code.
	if shortCircuit {
		action = pluginpb.HookAction_HOOK_ACTION_ABORT
		resp.StatusCode = abortStatus
		resp.Headers = abortHeaders
		resp.Body = abortBody
	}

	resp.Action = action

	switch action {
	case pluginpb.HookAction_HOOK_ACTION_ABORT:
		logger.AuditLogger("hook.abort", req.PluginName, "", "allowed", map[string]interface{}{
			"handler":     req.Handler,
			"operation":   req.Context.Operation,
			"status_code": resp.StatusCode,
		})
	case pluginpb.HookAction_HOOK_ACTION_MODIFY:
		logger.AuditLogger("hook.modify", req.PluginName, "", "allowed", map[string]interface{}{
			"handler":   req.Handler,
			"operation": req.Context.Operation,
		})
	}

	// F2.1: enforce trust-tier restrictions on short-circuit / abort bodies.
	sanitizeHookAbortResponse(resp, holder.pool.trustTier, req.PluginName, req.Handler, req.Context.Operation)

	return resp, nil
}

// sanitizeHookAbortResponse enforces spec §3.2 A11: only Tier 2 plugins may
// return a forged body on abort/short-circuit. Tier 0/1 aborts keep their
// status/headers but the body is stripped and defaulted to 403 when no status
// was provided. Audit records are emitted for both allow and deny paths.
func sanitizeHookAbortResponse(resp *pluginpb.InvokeHookResponse, tier int, pluginName, handler, operation string) {
	if resp == nil || resp.Action != pluginpb.HookAction_HOOK_ACTION_ABORT || len(resp.Body) == 0 {
		return
	}
	if tier < TierCore {
		logger.AuditLogger("hook.short-circuit.body.rejected", pluginName, "", "denied", map[string]interface{}{
			"hook":       handler,
			"operation":  operation,
			"trust_tier": tier,
			"body_bytes": len(resp.Body),
		})
		resp.Body = nil
		if resp.StatusCode == 0 {
			resp.StatusCode = 403
		}
		return
	}
	logger.AuditLogger("hook.short-circuit.body.allowed", pluginName, "", "allowed", map[string]interface{}{
		"hook":       handler,
		"operation":  operation,
		"trust_tier": tier,
		"body_bytes": len(resp.Body),
	})
}

// ListHooks returns the registered S3 hooks, optionally filtered by operation.
func (l *Loader) ListHooks(ctx context.Context, req *pluginpb.ListHooksRequest) (*pluginpb.ListHooksResponse, error) {
	var records []*HookRecord
	if req != nil && req.Operation != "" {
		records = l.hooks.forOperation(req.Operation)
	} else {
		records = l.hooks.all()
	}

	out := make([]*pluginpb.HookRecord, 0, len(records))
	for _, r := range records {
		out = append(out, &pluginpb.HookRecord{
			PluginName: r.PluginName,
			HookId:     r.HookID,
			HookType:   r.HookType,
			Operation:  r.Operation,
			Condition:  r.Condition,
			Handler:    r.Handler,
			Priority:   int32(r.Priority),
			OnFailure:  r.OnFailure,
			Critical:   r.Critical,
		})
	}
	return &pluginpb.ListHooksResponse{Hooks: out}, nil
}

// --- hook handle management ---

func (l *Loader) allocateHookContext(inv *hookInvocation) uint64 {
	l.hookMu.Lock()
	defer l.hookMu.Unlock()
	l.nextHookHandle++
	if l.nextHookHandle == 0 {
		l.nextHookHandle++
	}
	h := l.nextHookHandle
	l.hookContexts[h] = inv
	return h
}

func (l *Loader) GetHookContext(handle uint64) *hookInvocation {
	l.hookMu.Lock()
	defer l.hookMu.Unlock()
	return l.hookContexts[handle]
}

func (l *Loader) releaseHookContext(handle uint64) {
	l.hookMu.Lock()
	defer l.hookMu.Unlock()
	delete(l.hookContexts, handle)
}

// logHookResult logs the result of a hook invocation.
func logHookResult(l *zap.Logger, pluginName, handler string, action pluginpb.HookAction) {
	l.Debug("hook invocation result",
		zap.String("plugin", pluginName),
		zap.String("handler", handler),
		zap.String("action", action.String()),
	)
}
