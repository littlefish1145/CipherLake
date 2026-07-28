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
	"cipherlake/internal/events"
	"cipherlake/internal/scheduler"
	pluginpb "cipherlake/proto/plugin"
	craft "cipherlake/internal/raft"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
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

	// trust verifies Ed25519 plugin signatures and derives effective trust
	// tiers from config/trusted_keys.json (spec §3.7, P2-1).
	trust *TrustStore

	// packager pulls/pushes plugin OCI artifacts (spec §3.16, P3-3).
	packager *OCIPackager

	// resourceLimiter resolves the three-layer resource model (spec §3.6, P2-2).
	resourceLimiter *ResourceLimiter

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

	// hooks is the in-memory S3 hook registry (spec §3.2, P2-4).
	hooks *hookRegistry

	// hookMu protects the in-flight hook context table used during hook
	// invocation. Each InvokeHook call allocates a handle that on_hook
	// uses to read/mutate the hook context.
	hookMu         sync.Mutex
	nextHookHandle uint64
	hookContexts   map[uint64]*hookInvocation

	// sse tracks Server-Sent Events sessions owned by plugins (spec §3.13, P3-4).
	sse *sseRegistry

	// debugBuffers holds recent logs/metrics emitted by plugins for the
	// nexusctl plugin logs/metrics debug tools (spec §3.17, P3-5).
	debugBuffers *debugBuffers

	// logLevels holds dynamically-adjustable per-plugin log levels.
	logLevels *pluginLogLevels

	// eventBus is the system event bus (P4-2). Plugins subscribe to events
	// and the loader forwards matching events to their on_event handlers.
	eventBus *events.EventBus

	// events holds runtime event subscriptions for all plugins.
	events *eventRegistry

	// eventCh carries events from the event bus callback to the loader's
	// dispatch workers.
	eventCh chan *events.Event

	// eventWorkerCount is the number of goroutines that dispatch events to
	// plugin handlers. Default 4.
	eventWorkerCount int

	// eventMu protects the in-flight event context table.
	eventMu sync.Mutex
	nextEventHandle uint64
	eventContexts   map[uint64]*eventInvocation

	// eventStopCh signals event dispatch workers to stop.
	eventStopCh chan struct{}

	// eventDispatchWg waits for event dispatch workers to exit.
	eventDispatchWg sync.WaitGroup

	// scheduler is the system cron scheduler (P4-3). The loader registers
	// plugin schedules and receives triggers through it.
	scheduler *scheduler.Scheduler

	// tasks holds runtime task schedules for all plugins.
	tasks *taskRegistry

	// taskMu protects the in-flight task context table.
	taskMu sync.Mutex
	nextTaskHandle uint64
	taskContexts   map[uint64]*taskInvocation

	// pipelineSteps holds the pipeline step name -> plugin mapping (P4-1).
	pipelineSteps *pipelineStepRegistry

	// pipelineMu protects the in-flight pipeline step context table.
	pipelineMu sync.Mutex
	nextPipelineHandle uint64
	pipelineContexts   map[uint64]*pipelineInvocation

	// peerAdminAddrs maps a Raft peer address to its gRPC admin/invocation
	// address. Used by followers to forward admin writes to the leader
	// (spec §12.3, P5-1). If empty, the leader's Raft address is used.
	peerAdminAddrs map[string]string

	// host is the host-imports layer, retained so production wiring
	// (SetPolicyEvaluator / SetDataKeyGenerator / SetAuditSigner /
	// SetHookRegistrar) can inject real service implementations after
	// construction (spec §3.7 A4, P5-5, F5-1).
	host *HostImports
}

// pluginHolder holds the runtime artifacts for an installed plugin.
type pluginHolder struct {
	name      string
	manifest  *Manifest
	compiled  *CompiledModule
	pool      *InstancePool
	reloading bool
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
	// BootstrapStatic (spec §12.3). Both bootstrap paths are idempotent
	// (return nil on raft.ErrCantBootstrap), so re-running the loader on
	// already-initialized data is safe.
	if len(cfg.Raft.EffectiveClusterPeers()) > 0 {
		if err := raftNode.BootstrapStatic(cfg.Raft); err != nil {
			raftNode.Shutdown()
			fsm.Close()
			return nil, fmt.Errorf("failed to bootstrap loader raft cluster: %w", err)
		}
	} else {
		if err := raftNode.BootstrapSingle(cfg.Raft); err != nil {
			raftNode.Shutdown()
			fsm.Close()
			return nil, fmt.Errorf("failed to bootstrap loader raft: %w", err)
		}
	}

	trustStore, err := LoadTrustStore(cfg.TrustedKeysPath)
	if err != nil {
		raftNode.Shutdown()
		fsm.Close()
		return nil, fmt.Errorf("failed to load trust store: %w", err)
	}
	if sha := FileSHA256(cfg.TrustedKeysPath); sha != "" {
		zap.L().Info("plugin loader trust store loaded",
			zap.String("path", cfg.TrustedKeysPath),
			zap.String("sha256", sha),
			zap.Strings("keys", trustStore.KeyIDs()),
		)
	}

	resourceLimiter := NewResourceLimiter(cfg.ResourceLimits)
	rt := NewWASMRuntimeWithLimits(NewCapabilityTable(), resourceLimiter)

	l := &Loader{
		cfg:             cfg,
		raft:            raftNode,
		fsm:             fsm,
		runtime:         rt,
		trust:           trustStore,
		packager:        NewOCIPackager(),
		resourceLimiter: resourceLimiter,
		routes:          make(map[string]string),
		plugins:         make(map[string]*pluginHolder),
		requests:        make(map[uint64]*pluginRequest),
		hooks:           newHookRegistry(),
		hookContexts:    make(map[uint64]*hookInvocation),
		sse:              newSSERegistry(),
		debugBuffers:     newDebugBuffers(),
		logLevels:        newPluginLogLevels(),
		events:           newEventRegistry(),
		eventCh:          make(chan *events.Event, 4096),
		eventWorkerCount: 4,
		eventContexts:    make(map[uint64]*eventInvocation),
		tasks:            newTaskRegistry(),
		taskContexts:     make(map[uint64]*taskInvocation),
		pipelineSteps:    newPipelineStepRegistry(),
		pipelineContexts: make(map[uint64]*pipelineInvocation),
		peerAdminAddrs:   buildPeerAdminAddrs(cfg),
	}

	host := NewHostImports(l, rt.CapabilityTable(), nil, nil)
	host.SetHTTPFetcher(NewHTTPFetcher())
	_ = host.pluginMetrics.Register(prometheus.DefaultRegisterer)
	// The Loader implements HookRegistrar by default (Tier-2-only
	// gateway.hook.register-for-other host import, spec §3.7 A4, P5-5).
	host.SetHookRegistrar(l)
	l.host = host
	rt.SetHostInstantiator(host)

	// Restore hook registry from replicated FSM so followers see the same hooks.
	if err := l.restoreHooks(); err != nil {
		raftNode.Shutdown()
		fsm.Close()
		return nil, fmt.Errorf("failed to restore hook registry: %w", err)
	}
	l.refreshHookOrder()

	return l, nil
}

// raftBootstrapErr is reserved for future use; kept as a named type so
// downstream code can wrap "already bootstrapped" cases without importing
// hashicorp/raft directly.
type raftBootstrapErr = error

// HostImportsLayer returns the host-imports layer, allowing production code
// to inject Tier-2-only service implementations (PolicyEvaluator,
// DataKeyGenerator, AuditSigner, HookRegistrar) after the Loader is
// constructed (spec §3.7 A4, P5-5, F5-1).
func (l *Loader) HostImportsLayer() *HostImports {
	return l.host
}

// SetPolicyEvaluator injects the IAM policy evaluator used by the
// iam.policy.evaluate Tier-2-only host import.
func (l *Loader) SetPolicyEvaluator(pe PolicyEvaluator) {
	if l.host != nil {
		l.host.SetPolicyEvaluator(pe)
	}
}

// SetDataKeyGenerator injects the KMS service used by kms.datakey.generate.
func (l *Loader) SetDataKeyGenerator(g DataKeyGenerator) {
	if l.host != nil {
		l.host.SetDataKeyGenerator(g)
	}
}

// SetAuditSigner injects the signer used by crypto.audit.sign.
func (l *Loader) SetAuditSigner(s AuditSigner) {
	if l.host != nil {
		l.host.SetAuditSigner(s)
	}
}

// SetHookRegistrar injects the hook registrar used by
// gateway.hook.register-for-other. By default the Loader itself implements
// HookRegistrar; this method exists to allow overriding for testing.
func (l *Loader) SetHookRegistrar(r HookRegistrar) {
	if l.host != nil {
		l.host.SetHookRegistrar(r)
	}
}

// SetVectorSearcher injects the vector searcher used by the vector.search
// host import (spec §5). Production deployments wire in the cipherlake
// VSE (VectorManager) so plugins can run real vector searches. When nil,
// vector.search returns ErrInternal.
func (l *Loader) SetVectorSearcher(v VectorSearcher) {
	if l.host != nil {
		l.host.SetVectorSearcher(v)
	}
}

// SetStorageClient injects the storage client used by the storage.get /
// storage.list host imports (spec §5). Production deployments wire in a
// client that reads objects through the cipherlake gateway standard flow
// (metadata → backend.Get → decrypt) so plugins never touch raw
// encrypted bytes on disk. storage.put/list/delete remain Unauthorized.
func (l *Loader) SetStorageClient(s StorageClient) {
	if l.host != nil {
		l.host.SetStorageClient(s)
	}
}

// Shutdown gracefully stops the loader: drains in-flight requests, closes
// the WASM runtime, the Raft node, and the FSM.
func (l *Loader) Shutdown() error {
	// Stop event dispatch workers first to prevent new plugin calls.
	if l.eventStopCh != nil {
		close(l.eventStopCh)
		l.eventDispatchWg.Wait()
		l.eventStopCh = nil
	}

	// Unregister plugin schedules so the scheduler does not call back into
	// a runtime that is about to be closed.
	l.stopAllPluginTasks()

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

// IsLeader reports whether this Loader is the current Raft leader.
func (l *Loader) IsLeader() bool {
	if l.raft == nil {
		return false
	}
	return l.raft.IsLeader()
}

// LeaderAddr returns the address of the current Raft leader, or an empty
// string if unknown.
func (l *Loader) LeaderAddr() string {
	if l.raft == nil {
		return ""
	}
	return l.raft.GetLeaderAddr()
}

// LeaderAdminAddr returns the gRPC admin/invocation address of the current
// Raft leader. If a peer-to-admin mapping was configured it is used;
// otherwise the leader's Raft address is returned as a fallback
// (spec §12.3, P5-1).
func (l *Loader) LeaderAdminAddr() string {
	addr := l.LeaderAddr()
	if addr == "" {
		return ""
	}
	if l.peerAdminAddrs != nil {
		if adminAddr, ok := l.peerAdminAddrs[addr]; ok && adminAddr != "" {
			return adminAddr
		}
	}
	return addr
}

// leaderStateGet forwards a state read to the current Raft leader's admin
// gRPC service and returns the entry. This lets followers serve linearized
// reads without a native follower ReadIndex API (spec §12.3, P5-2).
func (l *Loader) leaderStateGet(ctx context.Context, pluginName, key string) (*StateEntry, error) {
	addr := l.LeaderAdminAddr()
	if addr == "" {
		return nil, errors.New("no known leader for state read forwarding")
	}
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(dialCtx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("failed to dial leader %s for state read: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	client := pluginpb.NewPluginAdminServiceClient(conn)
	resp, err := client.GetPluginState(ctx, &pluginpb.GetPluginStateRequest{
		PluginName: pluginName,
		Key:        key,
	})
	if err != nil {
		return nil, fmt.Errorf("leader state get failed: %w", err)
	}
	if !resp.Found {
		return nil, nil
	}
	return &StateEntry{Value: resp.Value, Version: resp.Version}, nil
}

// buildPeerAdminAddrs builds a map from Raft peer address to gRPC admin
// address using PluginLoaderConfig.ClusterGRPCAddrs (parallel to
// Raft.ClusterPeers). If ClusterGRPCAddrs is empty the map is nil and
// LeaderAdminAddr falls back to the Raft address.
func buildPeerAdminAddrs(cfg *config.PluginLoaderConfig) map[string]string {
	if cfg == nil || cfg.Raft == nil {
		return nil
	}
	peers := cfg.Raft.EffectiveClusterPeers()
	addrs := cfg.ClusterGRPCAddrs
	if len(addrs) == 0 {
		return nil
	}
	m := make(map[string]string, len(peers))
	for i, peer := range peers {
		if peer == "" {
			continue
		}
		if i < len(addrs) && addrs[i] != "" {
			m[peer] = addrs[i]
		}
	}
	// Always include the local node so a node can resolve its own address.
	if cfg.Raft.ListenAddr != "" && cfg.GRPCListenAddr != "" {
		m[cfg.Raft.ListenAddr] = cfg.GRPCListenAddr
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// StateGet reads a key from a plugin's state KV namespace (spec §3.14).
// In multi-node deployments the read is linearized via Raft ReadIndex
// before reading the local FSM DB (spec §12.3, P5-2).
func (l *Loader) StateGet(pluginName, key string) (*StateEntry, error) {
	return l.StateGetWithContext(context.Background(), pluginName, key)
}

// StateGetWithContext is like StateGet but accepts a context for the
// Raft ReadIndex linearization step.
func (l *Loader) StateGetWithContext(ctx context.Context, pluginName, key string) (*StateEntry, error) {
	// Linearize the read so followers do not serve stale data. If this node
	// is not the leader, forward the read to the leader (hashicorp/raft
	// v1.7.x does not expose a follower-usable ReadIndex API).
	if l.raft != nil && !l.raft.IsLeader() {
		return l.leaderStateGet(ctx, pluginName, key)
	}
	if l.raft != nil {
		if err := l.raft.LinearizableRead(ctx); err != nil {
			return nil, fmt.Errorf("linearizable read failed: %w", err)
		}
	}
	return l.stateGetDirect(pluginName, key)
}

// stateGetDirect reads from the local FSM DB without ReadIndex. Used
// internally after a local Raft apply where linearization is already
// guaranteed.
func (l *Loader) stateGetDirect(pluginName, key string) (*StateEntry, error) {
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
	// Use the direct local read because this node just committed the write.
	entry, err := l.stateGetDirect(pluginName, key)
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

// DebugLog appends a log entry to the in-memory debug buffer for a plugin.
func (l *Loader) DebugLog(pluginName, level, payload string) {
	if l.debugBuffers == nil {
		return
	}
	if !l.logLevels.enabled(pluginName, zapLevelFromString(level)) {
		return
	}
	l.debugBuffers.buffer(pluginName).appendLog(level, payload)
}

// DebugMetric appends a metric sample to the in-memory debug buffer.
func (l *Loader) DebugMetric(pluginName, name string, value float64, labels string) {
	if l.debugBuffers == nil {
		return
	}
	l.debugBuffers.buffer(pluginName).appendMetric(name, value, labels)
}

// RecentDebugLogs returns recent log entries for a plugin.
func (l *Loader) RecentDebugLogs(pluginName string, limit int) []pluginLogEntry {
	if l.debugBuffers == nil {
		return nil
	}
	return l.debugBuffers.recentLogs(pluginName, limit)
}

// RecentDebugMetrics returns recent metric samples for a plugin.
func (l *Loader) RecentDebugMetrics(pluginName string, limit int) []pluginMetricSample {
	if l.debugBuffers == nil {
		return nil
	}
	return l.debugBuffers.recentMetrics(pluginName, limit)
}

// SetPluginLogLevel sets the dynamic log level for a plugin.
func (l *Loader) SetPluginLogLevel(pluginName, level string) string {
	if l.logLevels == nil {
		return "info"
	}
	return l.logLevels.set(pluginName, level).Level().String()
}

// refreshHookOrder recomputes the dependency-based hook ordering and applies
// it to the in-memory hook registry (spec §3.11).
func (l *Loader) refreshHookOrder() {
	order, err := l.dependencyOrder()
	if err != nil {
		zap.L().Warn("failed to compute plugin dependency order", zap.Error(err))
		return
	}
	idx := make(map[string]int, len(order))
	for i, name := range order {
		idx[name] = i
	}
	l.hooks.setPluginOrder(func(name string) int {
		if i, ok := idx[name]; ok {
			return i
		}
		return len(order) + 1
	})
}

// StateDump exports the entire state:kv namespace of a plugin as a map from
// key to state entry.
func (l *Loader) StateDump(pluginName string) (map[string]*StateEntry, error) {
	db := l.fsm.DB()
	if db == nil {
		return nil, errors.New("loader FSM db is nil")
	}
	out := make(map[string]*StateEntry)
	err := db.View(func(tx *bolt.Tx) error {
		stateRoot := tx.Bucket([]byte(BucketPluginState))
		if stateRoot == nil {
			return fmt.Errorf("bucket %s missing", BucketPluginState)
		}
		nested := stateRoot.Bucket([]byte(pluginName))
		if nested == nil {
			return nil
		}
		return nested.ForEach(func(k, v []byte) error {
			var e StateEntry
			if err := json.Unmarshal(v, &e); err != nil {
				return fmt.Errorf("corrupt state entry for %s/%s: %w", pluginName, string(k), err)
			}
			out[string(k)] = &e
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

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
// Signature verification is performed over the SHA-256 digest of
// manifestJSON || wasmBytes using the Ed25519 key identified by keyID. An
// empty signature/keyID installs the plugin at Tier 0. The effective tier
// is the minimum of the requested tier, the signing key's tier, and the
// manifest's declared trust_tier_requested.
func (l *Loader) InstallPlugin(ctx context.Context, manifest *Manifest, wasmBytes []byte, signature []byte, keyID string) error {
	if manifest == nil {
		return errors.New("manifest is nil")
	}
	if err := manifest.Validate(); err != nil {
		return err
	}
	if len(wasmBytes) == 0 {
		return errors.New("wasm bytes are empty")
	}

	// P4-5: dependencies must already be installed.
	if err := l.checkDependenciesInstalled(manifest.Name, manifest.DependsOn); err != nil {
		return err
	}

	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("failed to marshal manifest: %w", err)
	}

	// Determine effective trust tier from signature and manifest request.
	effectiveTier, signingKeyID, err := l.trust.DetermineEffectiveTier(manifest.TrustTierRequested, manifestJSON, wasmBytes, signature, keyID)
	if err != nil {
		return ErrInvalidSignature{Reason: err.Error()}
	}
	_ = signingKeyID

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

	// Resolve the three-layer resource envelope for this plugin.
	effectiveLimits := l.resourceLimiter.ForTier(effectiveTier, manifest.Resources)
	poolCfg := PoolConfig{
		MaxSize:       effectiveLimits.Concurrent,
		BorrowTimeout: 5 * time.Second,
		CallTimeout:   effectiveLimits.CallTimeout,
	}
	if l.cfg.InstancePoolSize > 0 && l.cfg.InstancePoolSize < poolCfg.MaxSize {
		// Legacy top-level cap still applies if it is stricter.
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
	manifestJSON, err = json.Marshal(manifest)
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

	// Register hooks declared in the manifest. If this fails, uninstall the
	// plugin to keep in-memory and persistent state consistent.
	if err := l.registerHooksFromManifest(manifest); err != nil {
		l.unloadPlugin(manifest.Name)
		return fmt.Errorf("failed to register plugin hooks: %w", err)
	}
	l.refreshHookOrder()

	// P4-2: register event subscriptions declared in the manifest.
	l.registerEventSubscriptionsFromManifest(manifest)

	// P4-3: register task schedules declared in the manifest.
	l.registerTaskSchedulesFromManifest(manifest)

	// P4-1: register pipeline steps declared in the manifest.
	if err := l.registerPipelineStepsFromManifest(manifest); err != nil {
		l.unloadPlugin(manifest.Name)
		return fmt.Errorf("failed to register pipeline steps: %w", err)
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
	l.unregisterPluginHooks(name)
	l.unregisterPluginEventSubscriptions(name)
	l.unregisterPluginTaskSchedules(name)
	l.unregisterPluginPipelineSteps(name)
	holder.pool.DestroyAll(context.Background())
	holder.compiled.Close(context.Background())
}

// pluginHolder returns the runtime holder for an installed plugin.
func (l *Loader) pluginHolder(name string) (*pluginHolder, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	h, ok := l.plugins[name]
	return h, ok
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
	reloading := holder != nil && holder.reloading
	l.mu.RUnlock()
	if !ok {
		return nil, status.Errorf(codes.NotFound, "plugin %q not installed", req.PluginName)
	}
	if reloading {
		return nil, status.Errorf(codes.Unavailable, "plugin %q is reloading", req.PluginName)
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

// restoreHooks rebuilds the in-memory hook registry from the FSM.
func (l *Loader) restoreHooks() error {
	db := l.fsm.DB()
	if db == nil {
		return errors.New("loader FSM db is nil")
	}
	return db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketHookRegistry))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var record HookRecord
			if err := json.Unmarshal(v, &record); err != nil {
				return fmt.Errorf("corrupt hook record %s: %w", k, err)
			}
			l.hooks.add(&record)
			return nil
		})
	})
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

// InvokeHook delegates to Loader.InvokeHook.
func (s *LoaderGRPCServer) InvokeHook(ctx context.Context, req *pluginpb.InvokeHookRequest) (*pluginpb.InvokeHookResponse, error) {
	return s.Loader.InvokeHook(ctx, req)
}

// ListHooks delegates to Loader.ListHooks.
func (s *LoaderGRPCServer) ListHooks(ctx context.Context, req *pluginpb.ListHooksRequest) (*pluginpb.ListHooksResponse, error) {
	return s.Loader.ListHooks(ctx, req)
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

// ErrDependencyUnavailable is returned when a plugin is temporarily
// unavailable because it is being reloaded (spec §3.5).
type ErrDependencyUnavailable struct {
	Plugin string
}

func (e ErrDependencyUnavailable) Error() string {
	return fmt.Sprintf("plugin %q is temporarily unavailable (reload in progress)", e.Plugin)
}

// IsErrDependencyUnavailable reports whether err is an ErrDependencyUnavailable.
func IsErrDependencyUnavailable(err error) bool {
	var e ErrDependencyUnavailable
	return errors.As(err, &e)
}
