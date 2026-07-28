package pluginloader

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// sseSession tracks a Server-Sent Events session owned by a plugin.
// The host holds the long-lived HTTP connection; the plugin receives
// events via the route.event-publish host import and is notified of
// disconnects via the on_sse_close entry point (spec §3.13 A7, A12).
type sseSession struct {
	id            uint64
	pluginName    string
	createdAt     time.Time
	lastHeartbeat time.Time
}

// sseRegistry holds the live SSE sessions for all plugins.
type sseRegistry struct {
	mu       sync.RWMutex
	sessions map[uint64]*sseSession
	nextID   uint64
}

func newSSERegistry() *sseRegistry {
	return &sseRegistry{
		sessions: make(map[uint64]*sseSession),
	}
}

// CreateSSESession registers a new SSE session owned by pluginName and
// returns its opaque session ID.
func (r *sseRegistry) CreateSSESession(pluginName string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	if r.nextID == 0 {
		r.nextID++
	}
	id := r.nextID
	now := time.Now()
	r.sessions[id] = &sseSession{
		id:            id,
		pluginName:    pluginName,
		createdAt:     now,
		lastHeartbeat: now,
	}
	return id
}

// Owner returns the plugin that owns sessionID, or "" if unknown.
func (r *sseRegistry) Owner(sessionID uint64) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.sessions[sessionID]
	if !ok {
		return ""
	}
	return s.pluginName
}

// Heartbeat updates the last-seen timestamp for a session. Returns false
// if the session does not exist.
func (r *sseRegistry) Heartbeat(sessionID uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[sessionID]
	if !ok {
		return false
	}
	s.lastHeartbeat = time.Now()
	return true
}

// Close removes a session from the registry. Returns the owner plugin
// name so the caller can invoke on_sse_close.
func (r *sseRegistry) Close(sessionID uint64) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[sessionID]
	if !ok {
		return ""
	}
	delete(r.sessions, sessionID)
	return s.pluginName
}

// CloseAll removes every session owned by pluginName and returns the
// list of session IDs that were closed.
func (r *sseRegistry) CloseAll(pluginName string) []uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []uint64
	for id, s := range r.sessions {
		if s.pluginName == pluginName {
			ids = append(ids, id)
			delete(r.sessions, id)
		}
	}
	return ids
}

// Stale returns sessions whose last heartbeat is older than maxAge.
func (r *sseRegistry) Stale(maxAge time.Duration) []uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cutoff := time.Now().Add(-maxAge)
	var ids []uint64
	for id, s := range r.sessions {
		if s.lastHeartbeat.Before(cutoff) {
			ids = append(ids, id)
		}
	}
	return ids
}

// CreateSSESession creates a new SSE session for pluginName and returns
// its opaque handle. The Gateway calls this when a client opens an SSE
// connection on a route with sse_sessions=true.
func (l *Loader) CreateSSESession(pluginName string) uint64 {
	return l.sse.CreateSSESession(pluginName)
}

// GetSSESessionOwner returns the plugin that owns sessionID, or "" if
// the session does not exist.
func (l *Loader) GetSSESessionOwner(sessionID uint64) string {
	return l.sse.Owner(sessionID)
}

// HeartbeatSSESession updates the last-seen timestamp for a session.
func (l *Loader) HeartbeatSSESession(sessionID uint64) bool {
	return l.sse.Heartbeat(sessionID)
}

// SSECloseReason codes passed to the plugin's on_sse_close entry point.
// WASM entry points receive numeric arguments, so reasons are encoded as
// small integers (spec §4).
const (
	SSECloseReasonUnknown          uint64 = 0
	SSECloseReasonClientDisconnect uint64 = 1
	SSECloseReasonTimeout          uint64 = 2
	SSECloseReasonUninstall        uint64 = 3
)

// CloseSSESession closes a single SSE session and invokes the owning
// plugin's on_sse_close entry point with the session ID and reason code.
func (l *Loader) CloseSSESession(ctx context.Context, sessionID uint64, reason uint64) error {
	pluginName := l.sse.Close(sessionID)
	if pluginName == "" {
		return fmt.Errorf("session %d not found", sessionID)
	}
	l.mu.RLock()
	holder, ok := l.plugins[pluginName]
	l.mu.RUnlock()
	if !ok || holder == nil || holder.pool == nil {
		return nil
	}
	inst, err := holder.pool.Borrow(ctx)
	if err != nil {
		return fmt.Errorf("failed to borrow instance for on_sse_close: %w", err)
	}
	defer holder.pool.Return(inst)
	_, _ = inst.CallEntry(ctx, "on_sse_close", sessionID, reason)
	return nil
}

// CloseAllSSESessions closes every SSE session owned by pluginName and
// notifies the plugin via on_sse_close for each one. Used during
// uninstall to prevent leaked sessions (spec §3.13 A7).
func (l *Loader) CloseAllSSESessions(ctx context.Context, pluginName string, reason uint64) {
	ids := l.sse.CloseAll(pluginName)
	if len(ids) == 0 {
		return
	}
	l.mu.RLock()
	holder, ok := l.plugins[pluginName]
	l.mu.RUnlock()
	if !ok || holder == nil || holder.pool == nil {
		return
	}
	for _, id := range ids {
		inst, err := holder.pool.Borrow(ctx)
		if err != nil {
			continue
		}
		_, _ = inst.CallEntry(ctx, "on_sse_close", id, reason)
		holder.pool.Return(inst)
	}
}

// ErrSessionNotOwned is returned when a plugin attempts to publish to an
// SSE session it does not own (spec §3.13 A12).
type ErrSessionNotOwned struct {
	SessionID  uint64
	PluginName string
	Owner      string
}

func (e ErrSessionNotOwned) Error() string {
	return fmt.Sprintf("session %d is owned by %q, not %q", e.SessionID, e.Owner, e.PluginName)
}

// IsErrSessionNotOwned reports whether err is an ErrSessionNotOwned.
func IsErrSessionNotOwned(err error) bool {
	var e ErrSessionNotOwned
	return errors.As(err, &e)
}
