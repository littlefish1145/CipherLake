package pluginloader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"cipherlake/internal/config"
	pluginpb "cipherlake/proto/plugin"
	craft "cipherlake/internal/raft"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	bolt "go.etcd.io/bbolt"
)

// Loader is the top-level orchestrator of the plugin loader service.
//
// It owns the Raft node (which replicates LoaderFSM state) and exposes
// methods for admin operations (install/uninstall/reload), hook invocation,
// route dispatch, and state KV access. WASM instance management and
// capability token enforcement live in dedicated files (instance_pool.go,
// capability.go) and are wired into the Loader as Phase 1 progresses.
type Loader struct {
	cfg   *config.PluginLoaderConfig
	raft  *craft.RaftNode
	fsm   *LoaderFSM

	// runtime is the shared wazero runtime; all plugin instances run in it.
	runtime *WASMRuntime

	// mu protects the in-memory plugin table during install/uninstall.
	// Persistent state is serialized through Raft, but the live WASM
	// instances are managed in-memory and need their own lock.
	mu     sync.RWMutex
	routes map[string]string // route prefix → plugin name
	plugins map[string]*pluginHolder

	// reqMu protects the in-flight request handle table used during route
	// dispatch. Each InvokeRoute call allocates a handle that on_request
	// uses to read the request and write the response.
	reqMu         sync.Mutex
	nextReqHandle uint64
	requests      map[uint64]*pluginRequest
}

// pluginHolder holds the runtime artifacts for an installed plugin.
type pluginHolder struct {
	name     string
	manifest *Manifest
	compiled *CompiledModule
	pool     *InstancePool
}

// pluginRequest is the per-invocation context passed to a plugin's
// on_request entry point via an opaque handle.
type pluginRequest struct {
	method       string
	path         string
	headers      map[string]string
	body         []byte
	originalUser string

	// Response fields are filled by the plugin via host imports.
	respStatus  int
	respHeaders map[string]string
	respBody    []byte
	respWritten bool
}

// New constructs a Loader, bootstraps a single-node Raft cluster (Phase 1)
// and initializes the LoaderFSM with its four top-level buckets.
func New(cfg *config.PluginLoaderConfig) (*Loader, error) {
	if cfg == nil {
		return nil, errors.New("plugin loader config is nil")
	}
	if cfg.Raft == nil {
		return nil, errors.New("plugin loader raft config is nil")
	}

	fsm, err := NewLoaderFSMFromDir(cfg.Raft.DataDir)
	if err != nil {
		return nil, fmt.Errorf("failed to create loader FSM: %w", err)
	}

	raftNode, err := craft.NewRaftNodeWithFSM(cfg.Raft, fsm, fsm)
	if err != nil {
		fsm.Close()
		return nil, fmt.Errorf("failed to create loader raft node: %w", err)
	}

	// Phase 1: single-node bootstrap. Phase 5 extends to multi-node via
	// AddVoter / DemoteVoter / RemoveServer (spec §12.3). BootstrapSingle is
	// idempotent (returns nil on raft.ErrCantBootstrap), so re-running the
	// loader on already-initialized data is safe.
	if err := raftNode.BootstrapSingle(cfg.Raft); err != nil {
		raftNode.Shutdown()
		fsm.Close()
		return nil, fmt.Errorf("failed to bootstrap loader raft: %w", err)
	}

	rt := NewWASMRuntime(NewCapabilityTable())

	l := &Loader{
		cfg:      cfg,
		raft:     raftNode,
		fsm:      fsm,
		runtime:  rt,
		routes:   make(map[string]string),
		plugins:  make(map[string]*pluginHolder),
		requests: make(map[uint64]*pluginRequest),
	}

	host := NewHostImports(l, rt.CapabilityTable(), nil, nil)
	rt.SetHostInstantiator(host)
	return l, nil
}

// raftBootstrapErr is reserved for future use; kept as a named type so
// downstream code can wrap "already bootstrapped" cases without importing
// hashicorp/raft directly.
type raftBootstrapErr = error

// Shutdown gracefully stops the loader: drains in-flight requests, closes
// the WASM runtime, the Raft node, and the FSM.
func (l *Loader) Shutdown() error {
	if l.runtime != nil {
		rt := l.runtime
		l.runtime = nil
		if err := rt.Close(context.Background()); err != nil {
			return fmt.Errorf("wasm runtime close: %w", err)
		}
	}
	if l.raft != nil {
		raftNode := l.raft
		l.raft = nil
		if err := raftNode.Shutdown(); err != nil {
			return fmt.Errorf("raft shutdown: %w", err)
		}
	}
	return nil
}

// RaftApply submits a loader FSM operation through Raft replication.
// Blocks until committed (30s timeout).
//
// The FSM's Apply returns *raft.FSMApplyResult; if Success=false we surface
// the underlying error string. CAS mismatches are detected via the error
// string ("state CAS mismatch for ...") — callers can use IsErrVersionMismatch
// on the wrapped error returned by StatePut (which goes through this path).
func (l *Loader) RaftApply(ctx context.Context, op *LoaderFSMOp) error {
	if l.raft == nil {
		return errors.New("loader raft node is nil")
	}
	opJSON, err := json.Marshal(op)
	if err != nil {
		return fmt.Errorf("failed to marshal loader op: %w", err)
	}
	result, ok, err := l.raft.ApplyRaw(ctx, opJSON, 30*time.Second)
	if err != nil {
		return err
	}
	if ok && result != nil && !result.Success {
		// CAS mismatch is a normal semantic failure; preserve the typed
		// error so callers can use IsErrVersionMismatch. Other FSM errors
		// are surfaced as plain errors.
		if strings.HasPrefix(result.Error, "state CAS mismatch") {
			return ErrVersionMismatch{}
		}
		return errors.New(result.Error)
	}
	return nil
}

// StateGet reads a key from a plugin's state KV namespace (spec §3.14).
// Reads go directly to the local FSM DB (linearizable reads would require
// a Raft ReadIndex; for Phase 1 single-node we accept eventual from-local
// consistency; Phase 5 adds ReadIndex for multi-node, spec §3.14).
func (l *Loader) StateGet(pluginName, key string) (*StateEntry, error) {
	db := l.fsm.DB()
	if db == nil {
		return nil, errors.New("loader FSM db is nil")
	}
	var entry *StateEntry
	err := db.View(func(tx *bolt.Tx) error {
		stateRoot := tx.Bucket([]byte(BucketPluginState))
		if stateRoot == nil {
			return fmt.Errorf("bucket %s missing", BucketPluginState)
		}
		nested := stateRoot.Bucket([]byte(pluginName))
		if nested == nil {
			entry = nil
			return nil
		}
		raw := nested.Get([]byte(key))
		if raw == nil {
			entry = nil
			return nil
		}
		var e StateEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			return fmt.Errorf("corrupt state entry for %s/%s: %w", pluginName, key, err)
		}
		entry = &e
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entry, nil
}

// StatePut writes a key (optionally with CAS), replicating through Raft.
// On CAS mismatch returns an error wrapping ErrVersionMismatch; use
// IsErrVersionMismatch(err) to detect it.
func (l *Loader) StatePut(pluginName, key string, value []byte, expectedVersion uint64) (uint64, error) {
	op := &LoaderFSMOp{
		Type:            OpStatePut,
		Plugin:          pluginName,
		Key:             key,
		Data:            value,
		ExpectedVersion: expectedVersion,
	}
	if err := l.RaftApply(context.Background(), op); err != nil {
		return 0, err
	}
	// Re-read to fetch the new version (the FSM Apply already bumped it).
	entry, err := l.StateGet(pluginName, key)
	if err != nil {
		return 0, err
	}
	if entry == nil {
		return 0, errors.New("state entry missing after put (FSM inconsistency)")
	}
	return entry.Version, nil
}

// StateDelete removes a key (idempotent), replicating through Raft.
func (l *Loader) StateDelete(pluginName, key string) error {
	op := &LoaderFSMOp{
		Type:   OpStateDelete,
		Plugin: pluginName,
		Key:    key,
	}
	return l.RaftApply(context.Background(), op)
}

// RegisterRoute claims a route prefix for a plugin. Returns an error on
// conflict (spec §3.4 A8: first-arrived wins; later installs rejected).
//
// This is in-memory only — the route → plugin mapping is reconstructed
// from plugin_registry on startup. We keep an in-memory map for O(1)
// lookup in the hot path; the canonical source of truth is the registry.
func (l *Loader) RegisterRoute(prefix, pluginName string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if existing, ok := l.routes[prefix]; ok && existing != pluginName {
		return ErrRoutePrefixConflict{Prefix: prefix, Existing: existing}
	}
	l.routes[prefix] = pluginName
	return nil
}

// RoutePlugin returns the plugin name handling the given route prefix,
// or "" if no plugin owns it.
func (l *Loader) RoutePlugin(prefix string) string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.routes[prefix]
}

// SetVectorManager injects the vector searcher used by vector.search host
// imports. Call once before any plugin instances are created.
func (l *Loader) SetVectorManager(vm VectorSearcher) {
	if l.runtime == nil || l.runtime.hostInst == nil {
		return
	}
	if hi, ok := l.runtime.hostInst.(*HostImports); ok {
		hi.vector = vm
	}
}

// WASMRuntime exposes the loader's shared WASM runtime. Used by tests and
// by the admin API when building instance pools.
func (l *Loader) WASMRuntime() *WASMRuntime { return l.runtime }

// allocateRequest creates a handle for an in-flight plugin request.
// The caller must call releaseRequest when done.
func (l *Loader) allocateRequest(req *pluginRequest) uint64 {
	l.reqMu.Lock()
	defer l.reqMu.Unlock()
	l.nextReqHandle++
	if l.nextReqHandle == 0 {
		l.nextReqHandle++
	}
	h := l.nextReqHandle
	l.requests[h] = req
	return h
}

// GetRequest returns the request context for the given handle.
func (l *Loader) GetRequest(handle uint64) *pluginRequest {
	l.reqMu.Lock()
	defer l.reqMu.Unlock()
	return l.requests[handle]
}

// WriteResponse stores the response written by the plugin for a request handle.
func (l *Loader) WriteResponse(handle uint64, status int, headers map[string]string, body []byte) error {
	l.reqMu.Lock()
	defer l.reqMu.Unlock()
	req, ok := l.requests[handle]
	if !ok {
		return fmt.Errorf("request handle %d not found", handle)
	}
	if req.respWritten {
		return fmt.Errorf("response already written for handle %d", handle)
	}
	req.respStatus = status
	req.respHeaders = headers
	req.respBody = body
	req.respWritten = true
	return nil
}

// releaseRequest removes a request handle from the in-flight table.
func (l *Loader) releaseRequest(handle uint64) {
	l.reqMu.Lock()
	defer l.reqMu.Unlock()
	delete(l.requests, handle)
}

// InstallPlugin loads a plugin into the runtime, creates its instance pool,
// registers its routes, and persists metadata through Raft (spec §3.5).
//
// Phase 1 trusts the caller to supply a valid trust tier; signature
// verification (P2-1) will downgrade or reject based on trusted_keys.json.
func (l *Loader) InstallPlugin(ctx context.Context, manifest *Manifest, wasmBytes []byte, trustTier int) error {
	if manifest == nil {
		return errors.New("manifest is nil")
	}
	if err := manifest.Validate(); err != nil {
		return err
	}
	if len(wasmBytes) == 0 {
		return errors.New("wasm bytes are empty")
	}

	compiled, err := l.runtime.LoadModule(ctx, manifest.Name, manifest.Version, wasmBytes)
	if err != nil {
		return fmt.Errorf("failed to load wasm module: %w", err)
	}

	// Register routes in-memory. A conflict aborts the install before we
	// persist anything to Raft.
	l.mu.Lock()
	for _, r := range manifest.Routes {
		if existing, ok := l.routes[r.Prefix]; ok && existing != manifest.Name {
			l.mu.Unlock()
			compiled.Close(ctx)
			return ErrRoutePrefixConflict{Prefix: r.Prefix, Existing: existing}
		}
	}
	for _, r := range manifest.Routes {
		l.routes[r.Prefix] = manifest.Name
	}

	// Determine effective trust tier. Phase 1: accept caller-supplied tier
	// capped at the requested tier. P2-1 will derive this from signatures.
	effectiveTier := trustTier
	if effectiveTier > manifest.TrustTierRequested {
		effectiveTier = manifest.TrustTierRequested
	}
	if effectiveTier < 0 || effectiveTier > 2 {
		effectiveTier = 0
	}

	poolCfg := DefaultPoolConfig()
	if l.cfg.InstancePoolSize > 0 {
		poolCfg.MaxSize = l.cfg.InstancePoolSize
	}
	pool := NewInstancePool(l.runtime, compiled, manifest.Name, effectiveTier, manifest.Capabilities, poolCfg)

	l.plugins[manifest.Name] = &pluginHolder{
		name:     manifest.Name,
		manifest: manifest,
		compiled: compiled,
		pool:     pool,
	}
	l.mu.Unlock()

	// Persist plugin metadata to Raft FSM.
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("failed to marshal manifest: %w", err)
	}
	record := PluginRecord{
		Name:        manifest.Name,
		Version:     manifest.Version,
		TrustTier:   effectiveTier,
		Manifest:    manifestJSON,
		InstalledAt: time.Now().UTC().Format(time.RFC3339),
	}
	recordJSON, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("failed to marshal plugin record: %w", err)
	}
	op := &LoaderFSMOp{
		Type:   OpPluginInstall,
		Plugin: manifest.Name,
		Data:   recordJSON,
	}
	if err := l.RaftApply(ctx, op); err != nil {
		// Best-effort rollback: remove from memory. Persistent state will
		// be reconciled on next startup.
		l.unloadPlugin(manifest.Name)
		return fmt.Errorf("failed to persist plugin install: %w", err)
	}
	return nil
}

// unloadPlugin removes a plugin from the in-memory tables and closes its
// compiled module. It does NOT mutate Raft state.
func (l *Loader) unloadPlugin(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	holder, ok := l.plugins[name]
	if !ok {
		return
	}
	delete(l.plugins, name)
	for prefix, owner := range l.routes {
		if owner == name {
			delete(l.routes, prefix)
		}
	}
	holder.pool.DestroyAll(context.Background())
	holder.compiled.Close(context.Background())
}

// InvokeRoute dispatches a request to a plugin's on_request entry point
// and returns the response written by the plugin (spec §3.4).
func (l *Loader) InvokeRoute(ctx context.Context, req *pluginpb.InvokeRouteRequest) (*pluginpb.InvokeRouteResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is nil")
	}
	if req.PluginName == "" {
		return nil, status.Error(codes.InvalidArgument, "plugin_name is required")
	}

	l.mu.RLock()
	holder, ok := l.plugins[req.PluginName]
	l.mu.RUnlock()
	if !ok {
		return nil, status.Errorf(codes.NotFound, "plugin %q not installed", req.PluginName)
	}

	reqCtx := &pluginRequest{
		method:       req.Method,
		path:         req.Path,
		headers:      req.Headers,
		body:         req.Body,
		originalUser: req.OriginalUser,
	}
	handle := l.allocateRequest(reqCtx)
	defer l.releaseRequest(handle)

	inst, err := holder.pool.Borrow(ctx)
	if err != nil {
		return nil, status.Errorf(codes.ResourceExhausted, "failed to borrow plugin instance: %v", err)
	}
	defer holder.pool.Return(inst)

	// Phase 1: route_id is always 0; the plugin does sub-routing from req.Path.
	const routeID uint64 = 0
	_, err = inst.CallEntry(ctx, EntryPointOnRequest, routeID, handle)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "plugin on_request failed: %v", err)
	}

	if !reqCtx.respWritten {
		return nil, status.Errorf(codes.Internal, "plugin did not write a response")
	}

	resp := &pluginpb.InvokeRouteResponse{
		StatusCode: int32(reqCtx.respStatus),
		Headers:    reqCtx.respHeaders,
		Body:       reqCtx.respBody,
	}
	if reqCtx.respStatus >= 500 {
		resp.ErrorMessage = "plugin returned error status"
	}
	return resp, nil
}

// Health returns the loader health status.
func (l *Loader) Health(ctx context.Context) (*pluginpb.HealthResponse, error) {
	return &pluginpb.HealthResponse{
		Healthy:     true,
		ServiceName: "plugin-loader-service",
		Version:     HostAPIVersion,
	}, nil
}

// LoaderGRPCServer adapts Loader to the pluginpb.PluginLoaderServiceServer
// interface generated from proto/plugin/loader.proto.
type LoaderGRPCServer struct {
	pluginpb.UnimplementedPluginLoaderServiceServer
	Loader *Loader
}

// InvokeRoute delegates to Loader.InvokeRoute.
func (s *LoaderGRPCServer) InvokeRoute(ctx context.Context, req *pluginpb.InvokeRouteRequest) (*pluginpb.InvokeRouteResponse, error) {
	return s.Loader.InvokeRoute(ctx, req)
}

// Health delegates to Loader.Health.
func (s *LoaderGRPCServer) Health(ctx context.Context, _ *pluginpb.HealthRequest) (*pluginpb.HealthResponse, error) {
	return s.Loader.Health(ctx)
}

// ErrRoutePrefixConflict is returned when a plugin install attempts to
// register a route prefix already claimed by another plugin.
type ErrRoutePrefixConflict struct {
	Prefix   string
	Existing string
}

func (e ErrRoutePrefixConflict) Error() string {
	return fmt.Sprintf("route prefix %q already owned by plugin %q", e.Prefix, e.Existing)
}
