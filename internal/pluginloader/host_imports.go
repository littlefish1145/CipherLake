// Package pluginloader host imports (spec §5).
//
// This file implements the host side of the WASM plugin ABI using wazero
// native host functions rather than the Component Model / wit-bindgen path.
// That keeps Phase 1 unblocked while the WIT files in sdk/wit/ remain the
// canonical type documentation and the target for future generated bindings.
//
// ABI conventions used here:
//   - Every host function lives in the "nexus:host" module.
//   - Function names mirror the WIT imports, e.g. "state.get".
//   - The first argument is always the capability token (u64).
//   - String/byte buffers are passed as (ptr, len) pairs (i32 each).
//   - Results are written through out-parameters; status is a u32:
//     0 = OK
//     1 = ErrUnauthorized
//     2 = ErrNotFound
//     3 = ErrVersionConflict
//     4 = ErrBufferTooSmall
//     5 = ErrInvalidArgs
//     100 = ErrInternal
package pluginloader

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"

	"cipherlake/internal/events"
	"cipherlake/internal/iam"
	"cipherlake/internal/logger"
	"cipherlake/internal/observability"
	"cipherlake/internal/vector"
)

// Host status codes exposed to WASM guests.
const (
	statusOK              uint32 = 0
	statusUnauthorized    uint32 = 1
	statusNotFound        uint32 = 2
	statusVersionConflict uint32 = 3
	statusBufferTooSmall  uint32 = 4
	statusInvalidArgs     uint32 = 5
	statusInternal        uint32 = 100
)

// ErrNotFound is returned by StorageClient implementations when the
// requested object does not exist. Host imports translate this into the
// statusNotFound status code returned to the WASM guest.
var ErrNotFound = errors.New("pluginloader: object not found")

// VectorSearcher is the subset of the vector manager used by host imports.
type VectorSearcher interface {
	Search(ctx context.Context, query vector.Vector, topK int, filters map[string]string) ([]vector.SearchResult, error)
}

type VectorIndexer interface {
	Insert(ctx context.Context, vectors []vector.Vector) error
}

// StorageClient is the subset of the storage layer used by host imports.
// It forwards reads/writes to the cipherlake gateway over HTTP so that
// the standard auth → EncryptOperation → backend.Put → metadata index
// path applies. Plugins receive plaintext bytes on Get and present
// plaintext bytes on Put; they never handle encryption keys or
// ciphertext.
type StorageClient interface {
	GetObject(ctx context.Context, bucket, key, originalUser string) ([]byte, error)
	// PutObject writes the plaintext body through the cipherlake gateway
	// standard PUT path. Returns the object's ETag on success.
	PutObject(ctx context.Context, bucket, key string, body []byte, originalUser string) (string, error)
}

// PolicyEvaluator is the subset of the IAM policy evaluator used by the
// iam.policy.evaluate host import (spec §3.7 A4, P5-5).
type PolicyEvaluator interface {
	Evaluate(ctx *iam.EvalContext) *iam.EvalResult
}

// DataKeyGenerator is the subset of the KMS/keygen service used by the
// kms.datakey.generate host import (spec §3.7 A4, P5-5).
type DataKeyGenerator interface {
	GenerateDataKey(ctx context.Context, keyID string, length int) (plaintext, encrypted []byte, err error)
	GetPublicKey(ctx context.Context, keyID string) ([]byte, error)
}

// AuditSigner signs audit payloads on behalf of Tier 2 plugins.
type AuditSigner interface {
	Sign(payload []byte) ([]byte, error)
}

// HookRegistrar allows Tier 2 plugins to register hooks dynamically.
type HookRegistrar interface {
	RegisterHook(ctx context.Context, pluginName string, record *HookRecord) error
}

// HostImports implements the HostInstantiator interface. It registers all
// host imports for a single WASM instance and authorizes each call using the
// instance's capability token.
type HostImports struct {
	caps    *CapabilityTable
	loader  *Loader
	vector  VectorSearcher
	storage StorageClient
	logger  *zap.Logger

	// httpFetcher executes outbound requests on behalf of plugins.
	httpFetcher *HTTPFetcher

	// pluginMetrics forwards plugin-emitted metrics to Prometheus with
	// cardinality limits (spec §3.17, P4-4).
	pluginMetrics *PluginMetrics

	// pluginTracer manages OpenTelemetry spans created by plugins
	// (spec §3.17, P4-4).
	pluginTracer *PluginTracer

	// Tier-2-only service integrations (spec §3.7 A4, P5-5).
	policyEvaluator  PolicyEvaluator
	dataKeyGenerator DataKeyGenerator
	auditSigner      AuditSigner
	hookRegistrar    HookRegistrar

	// moduleTokens maps a live plugin module to its capability token so
	// host imports that are not passed the token directly (e.g. hook.*)
	// can still authorize.
	moduleTokens map[api.Module]CapabilityToken

	// moduleValidators maps a live plugin module to its network egress
	// validator. The validator is built from the plugin's effective trust
	// tier and manifest.network_egress patterns (spec §3.10).
	moduleValidators map[api.Module]*NetworkEgressValidator
	modMu            sync.RWMutex
}

// NewHostImports creates a host-imports layer wired to the given Loader and
// capability table. The vector searcher and storage client may be nil in
// Phase 1 (the corresponding imports will return ErrUnauthorized until
// plugged in).
func NewHostImports(loader *Loader, caps *CapabilityTable, vector VectorSearcher, storage StorageClient) *HostImports {
	if loader == nil {
		panic("NewHostImports: loader is nil")
	}
	pm := NewPluginMetrics()
	_ = pm.Register(prometheus.DefaultRegisterer)
	return &HostImports{
		caps:             caps,
		loader:           loader,
		vector:           vector,
		storage:          storage,
		logger:           zap.L(),
		pluginMetrics:    pm,
		pluginTracer:     NewPluginTracer(),
		moduleTokens:     make(map[api.Module]CapabilityToken),
		moduleValidators: make(map[api.Module]*NetworkEgressValidator),
	}
}

// tokenForModule returns the capability token associated with a module.
func (h *HostImports) tokenForModule(m api.Module) CapabilityToken {
	h.modMu.RLock()
	defer h.modMu.RUnlock()
	return h.moduleTokens[m]
}

// validatorForModule returns the network egress validator associated with a module.
func (h *HostImports) validatorForModule(m api.Module) *NetworkEgressValidator {
	h.modMu.RLock()
	defer h.modMu.RUnlock()
	return h.moduleValidators[m]
}

// OnModuleInstantiated records the module→token and module→validator associations.
func (h *HostImports) OnModuleInstantiated(mod api.Module, token CapabilityToken) {
	h.modMu.Lock()
	defer h.modMu.Unlock()
	h.moduleTokens[mod] = token
	if c := h.caps.Lookup(token); c != nil {
		if holder, ok := h.loader.pluginHolder(c.PluginName); ok {
			h.moduleValidators[mod] = NewNetworkEgressValidator(holder.pool.trustTier, holder.manifest.NetworkEgress)
		}
	}
}

// OnModuleClosed removes the module associations.
func (h *HostImports) OnModuleClosed(mod api.Module) {
	h.modMu.Lock()
	defer h.modMu.Unlock()
	delete(h.moduleTokens, mod)
	delete(h.moduleValidators, mod)
}

// SetLogger replaces the default logger. Useful in tests.
func (h *HostImports) SetLogger(l *zap.Logger) {
	h.logger = l
}

// SetHTTPFetcher injects the HTTP fetcher used by http.fetch. If unset,
// http.fetch returns ErrInternal.
func (h *HostImports) SetHTTPFetcher(f *HTTPFetcher) {
	h.httpFetcher = f
}

// SetPluginMetrics injects the Prometheus metrics recorder. If unset,
// metric-record updates the debug buffer but does not expose Prometheus
// metrics.
func (h *HostImports) SetPluginMetrics(pm *PluginMetrics) {
	h.pluginMetrics = pm
}

// SetPluginTracer injects the OpenTelemetry span manager. If unset,
// trace_span_start/end are no-ops.
func (h *HostImports) SetPluginTracer(pt *PluginTracer) {
	h.pluginTracer = pt
}

// SetPolicyEvaluator injects the IAM policy evaluator used by iam.policy.evaluate.
func (h *HostImports) SetPolicyEvaluator(pe PolicyEvaluator) {
	h.policyEvaluator = pe
}

// SetDataKeyGenerator injects the KMS/keygen service used by kms.datakey.generate.
func (h *HostImports) SetDataKeyGenerator(g DataKeyGenerator) {
	h.dataKeyGenerator = g
}

// SetAuditSigner injects the signer used by crypto.audit.sign.
func (h *HostImports) SetAuditSigner(s AuditSigner) {
	h.auditSigner = s
}

// SetHookRegistrar injects the hook registrar used by gateway.hook.register-for-other.
func (h *HostImports) SetHookRegistrar(r HookRegistrar) {
	h.hookRegistrar = r
}

// SetVectorSearcher injects the vector searcher used by the vector.search
// host import (spec §5). When nil, vector.search returns ErrInternal.
// This allows plugin-loader-service to wire in the cipherlake VSE
// (VectorManager) without re-creating the HostImports.
func (h *HostImports) SetVectorSearcher(v VectorSearcher) {
	h.vector = v
}

// SetStorageClient injects the storage client used by the storage.get /
// storage.list host imports (spec §5). When nil, storage.* return
// ErrUnauthorized. Production deployments inject a client that reads
// objects through the cipherlake gateway standard flow (metadata →
// backend.Get → cryptoCoordinator.DecryptOperation) so plugins never
// touch the raw encrypted bytes on disk.
func (h *HostImports) SetStorageClient(s StorageClient) {
	h.storage = s
}

// Instantiate implements HostInstantiator. It registers every host import on
// builder; unauthorized calls are rejected at call time, not link time, so
// missing capabilities surface as deterministic status codes.
func (h *HostImports) Instantiate(ctx context.Context, builder wazero.HostModuleBuilder, token CapabilityToken, pluginName string) error {
	fb := builder.NewFunctionBuilder()

	// State KV (spec §3.14).
	fb.WithFunc(h.stateGet).Export("state.get")
	fb.WithFunc(h.statePut).Export("state.put")
	fb.WithFunc(h.stateCas).Export("state.cas")
	fb.WithFunc(h.stateDelete).Export("state.delete")
	fb.WithFunc(h.stateBatch).Export("state.batch")

	// Storage (spec §3.9) — Phase 1 stubs, real forwarding in Phase 3.
	fb.WithFunc(h.storageGet).Export("storage.get")
	fb.WithFunc(h.storagePut).Export("storage.put")
	fb.WithFunc(h.storageList).Export("storage.list")
	fb.WithFunc(h.storageDelete).Export("storage.delete")

	// Vector (spec §5).
	fb.WithFunc(h.vectorSearch).Export("vector.search")
	fb.WithFunc(h.vectorIndex).Export("vector.index")

	// Observability (spec §3.17).
	fb.WithFunc(h.logEmit).Export("observability.log-emit")
	fb.WithFunc(h.metricRecord).Export("observability.metric-record")
	fb.WithFunc(h.traceSpanStart).Export("observability.trace-span-start")
	fb.WithFunc(h.traceSpanEnd).Export("observability.trace-span-end")

	// Events (spec §3.13, P4-2).
	fb.WithFunc(h.eventSubscribe).Export("event.subscribe")
	fb.WithFunc(h.eventUnsubscribe).Export("event.unsubscribe")
	fb.WithFunc(h.eventRead).Export("event.read")
	fb.WithFunc(h.eventPublish).Export("event.publish")

	// Tasks (spec §3.13, P4-3).
	fb.WithFunc(h.taskSchedule).Export("task.schedule")
	fb.WithFunc(h.taskUnschedule).Export("task.unschedule")
	fb.WithFunc(h.taskRead).Export("task.read")

	// Pipeline steps (spec §3.12, P4-1).
	fb.WithFunc(h.pipelineRead).Export("pipeline.read")
	fb.WithFunc(h.pipelineBody).Export("pipeline.body")
	fb.WithFunc(h.pipelineOutputWrite).Export("pipeline.output.write")

	// HTTP egress (spec §3.10, P2-3).
	fb.WithFunc(h.httpFetch).Export("http.fetch")

	// Tier-2-only advanced capabilities (spec §3.7 A4, P5-5).
	fb.WithFunc(h.iamPolicyEvaluate).Export("iam.policy.evaluate")
	fb.WithFunc(h.kmsDataKeyGenerate).Export("kms.datakey.generate")
	fb.WithFunc(h.cryptoAuditSign).Export("crypto.audit.sign")
	fb.WithFunc(h.gatewayHookRegisterForOther).Export("gateway.hook.register-for-other")

	// Route response (spec §3.4).
	fb.WithFunc(h.routeResponseWrite).Export("route.response-write")

	// SSE event publish (spec §3.13, P3-4).
	fb.WithFunc(h.routeEventPublish).Export("route.event-publish")

	// Request reading (spec §5 ctx_read for req_handle).
	fb.WithFunc(h.requestRead).Export("request.read")
	fb.WithFunc(h.requestBody).Export("request.body")

	// Hook context access (spec §3.2, P2-4).
	fb.WithFunc(h.hookRead).Export("hook.read")
	fb.WithFunc(h.hookBody).Export("hook.body")
	fb.WithFunc(h.hookModify).Export("hook.modify")
	fb.WithFunc(h.hookShortCircuit).Export("hook.short-circuit")

	return nil
}

// state.get(token, key_ptr, key_len, val_ptr, val_cap, out_status, out_len, out_version)
//
// Cross-plugin reads use the key format "<plugin_name>:<key>". The reader
// must hold the "state:cross_plugin:<plugin_name>" capability and the
// target plugin's manifest must grant the reader access to the key prefix
// (spec §3.14 A13, P5-3).
func (h *HostImports) stateGet(ctx context.Context, m api.Module, token uint64, keyPtr uint32, keyLen uint32, valPtr uint32, valCap uint32, outStatus uint32, outLen uint32, outVersion uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}

	if err := h.caps.Authorize(CapabilityToken(token), "state:kv"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	key, ok := readBytes(mem, keyPtr, keyLen)
	if !ok {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}

	c := h.caps.Lookup(CapabilityToken(token))
	if c == nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}

	targetPlugin, actualKey, crossPlugin := h.resolveCrossPluginStateKey(c.PluginName, string(key))
	if crossPlugin {
		capName := "state:cross_plugin:" + targetPlugin
		if err := h.caps.Authorize(CapabilityToken(token), capName); err != nil {
			logger.AuditLogger("state.cross_plugin.get", targetPlugin, c.PluginName, "denied", map[string]interface{}{
				"key":    actualKey,
				"reason": err.Error(),
			})
			writeStatus(mem, outStatus, statusUnauthorized)
			return
		}
		if !h.isCrossPluginStateAllowed(c.PluginName, targetPlugin, actualKey) {
			logger.AuditLogger("state.cross_plugin.get", targetPlugin, c.PluginName, "denied", map[string]interface{}{
				"key":    actualKey,
				"reason": "target plugin manifest does not grant access",
			})
			writeStatus(mem, outStatus, statusUnauthorized)
			return
		}
		logger.AuditLogger("state.cross_plugin.get", targetPlugin, c.PluginName, "allowed", map[string]interface{}{
			"key": actualKey,
		})
	}

	entry, err := h.loader.StateGet(targetPlugin, actualKey)
	if err != nil {
		h.logger.Error("state.get failed", zap.Error(err), zap.String("plugin", targetPlugin), zap.String("source_plugin", c.PluginName))
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	if entry == nil {
		writeStatus(mem, outStatus, statusNotFound)
		return
	}
	if uint32(len(entry.Value)) > valCap {
		writeStatus(mem, outStatus, statusBufferTooSmall)
		writeUint32(mem, outLen, uint32(len(entry.Value)))
		return
	}
	if !mem.Write(valPtr, entry.Value) {
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	writeStatus(mem, outStatus, statusOK)
	writeUint32(mem, outLen, uint32(len(entry.Value)))
	writeUint64(mem, outVersion, entry.Version)
}

// resolveCrossPluginStateKey inspects a state key for the cross-plugin
// prefix "<plugin_name>:<key>". If the prefix names a plugin other than
// the caller, it returns the target plugin, the stripped key, and true.
// Otherwise it returns the caller's own plugin name, the original key,
// and false. The prefix is only recognized when <plugin_name> is a valid
// plugin name, avoiding accidental interpretation of keys that happen to
// contain a colon.
func (h *HostImports) resolveCrossPluginStateKey(callerPlugin, key string) (targetPlugin, actualKey string, crossPlugin bool) {
	idx := strings.IndexByte(key, ':')
	if idx <= 0 {
		return callerPlugin, key, false
	}
	prefix := key[:idx]
	if !pluginNameRe.MatchString(prefix) {
		return callerPlugin, key, false
	}
	if prefix == callerPlugin {
		return callerPlugin, key[idx+1:], false
	}
	return prefix, key[idx+1:], true
}

// isCrossPluginStateAllowed reports whether readerPlugin may read
// targetPlugin's state key according to targetPlugin's manifest grants
// (spec §3.14 A13).
func (h *HostImports) isCrossPluginStateAllowed(readerPlugin, targetPlugin, key string) bool {
	h.loader.mu.RLock()
	holder, ok := h.loader.plugins[targetPlugin]
	h.loader.mu.RUnlock()
	if !ok || holder == nil || holder.manifest == nil {
		return false
	}
	for _, grant := range holder.manifest.Grants {
		if grant.Plugin != readerPlugin {
			continue
		}
		for _, pattern := range grant.Keys {
			if matchGlob(pattern, key) {
				return true
			}
		}
	}
	return false
}

// matchGlob performs a simple glob match. The only special character is
// "*" which matches any sequence of characters (including empty).
func matchGlob(pattern, s string) bool {
	// Fast path: no glob characters.
	if !strings.Contains(pattern, "*") {
		return pattern == s
	}
	// Convert glob to a simple regex, escaping other special chars.
	var b strings.Builder
	for i := 0; i < len(pattern); i++ {
		ch := pattern[i]
		switch ch {
		case '*':
			b.WriteString(".*")
		case '?', '+', '(', ')', '[', ']', '{', '}', '^', '$', '\\', '.', '|':
			b.WriteByte('\\')
			b.WriteByte(ch)
		default:
			b.WriteByte(ch)
		}
	}
	re, err := regexp.Compile("^" + b.String() + "$")
	if err != nil {
		return false
	}
	return re.MatchString(s)
}

// state.put(token, key_ptr, key_len, val_ptr, val_len, expected_version, out_status, out_version)
func (h *HostImports) statePut(ctx context.Context, m api.Module, token uint64, keyPtr uint32, keyLen uint32, valPtr uint32, valLen uint32, expectedVersion uint64, outStatus uint32, outVersion uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	if err := h.caps.Authorize(CapabilityToken(token), "state:kv"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	key, ok := readBytes(mem, keyPtr, keyLen)
	if !ok {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	value, ok := readBytes(mem, valPtr, valLen)
	if !ok {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	c := h.caps.Lookup(CapabilityToken(token))
	if c == nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}

	newVer, err := h.loader.StatePut(c.PluginName, string(key), value, expectedVersion)
	if err != nil {
		if IsErrVersionMismatch(err) {
			writeStatus(mem, outStatus, statusVersionConflict)
			return
		}
		h.logger.Error("state.put failed", zap.Error(err), zap.String("plugin", c.PluginName))
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	writeStatus(mem, outStatus, statusOK)
	writeUint64(mem, outVersion, newVer)
}

// state.cas(token, key_ptr, key_len, val_ptr, val_len, expected_version, out_status, out_version)
func (h *HostImports) stateCas(ctx context.Context, m api.Module, token uint64, keyPtr uint32, keyLen uint32, valPtr uint32, valLen uint32, expectedVersion uint64, outStatus uint32, outVersion uint32) {
	// CAS is just put with a mandatory expected_version > 0.
	if expectedVersion == 0 {
		mem := m.Memory()
		if mem != nil {
			writeStatus(mem, outStatus, statusInvalidArgs)
		}
		return
	}
	h.statePut(ctx, m, token, keyPtr, keyLen, valPtr, valLen, expectedVersion, outStatus, outVersion)
}

// state.delete(token, key_ptr, key_len, out_status)
func (h *HostImports) stateDelete(ctx context.Context, m api.Module, token uint64, keyPtr uint32, keyLen uint32, outStatus uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	if err := h.caps.Authorize(CapabilityToken(token), "state:kv"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	key, ok := readBytes(mem, keyPtr, keyLen)
	if !ok {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	c := h.caps.Lookup(CapabilityToken(token))
	if c == nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	if err := h.loader.StateDelete(c.PluginName, string(key)); err != nil {
		h.logger.Error("state.delete failed", zap.Error(err), zap.String("plugin", c.PluginName))
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	writeStatus(mem, outStatus, statusOK)
}

// state.batch(token, ops_json_ptr, ops_json_len, out_status, out_results_ptr, out_results_cap, out_results_len)
func (h *HostImports) stateBatch(ctx context.Context, m api.Module, token uint64, opsPtr uint32, opsLen uint32, outStatus uint32, outResultsPtr uint32, outResultsCap uint32, outResultsLen uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	if err := h.caps.Authorize(CapabilityToken(token), "state:kv"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	c := h.caps.Lookup(CapabilityToken(token))
	if c == nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	opsJSON, ok := readBytes(mem, opsPtr, opsLen)
	if !ok {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}

	var ops []stateBatchOp
	if err := json.Unmarshal(opsJSON, &ops); err != nil {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}

	results := make([]stateBatchResult, len(ops))
	for i, op := range ops {
		switch op.Op {
		case "put":
			ver, err := h.loader.StatePut(c.PluginName, op.Key, op.Value, op.ExpectedVersion)
			results[i].Version = ver
			if err != nil {
				if IsErrVersionMismatch(err) {
					results[i].Status = statusVersionConflict
				} else {
					results[i].Status = statusInternal
				}
			}
		case "delete":
			if err := h.loader.StateDelete(c.PluginName, op.Key); err != nil {
				results[i].Status = statusInternal
			}
		default:
			results[i].Status = statusInvalidArgs
		}
	}

	out, err := json.Marshal(results)
	if err != nil {
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	if uint32(len(out)) > outResultsCap {
		writeStatus(mem, outStatus, statusBufferTooSmall)
		writeUint32(mem, outResultsLen, uint32(len(out)))
		return
	}
	if !mem.Write(outResultsPtr, out) {
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	writeStatus(mem, outStatus, statusOK)
	writeUint32(mem, outResultsLen, uint32(len(out)))
}

type stateBatchOp struct {
	Op              string          `json:"op"`
	Key             string          `json:"key"`
	Value           json.RawMessage `json:"value,omitempty"`
	ExpectedVersion uint64          `json:"expected_version,omitempty"`
}

type stateBatchResult struct {
	Status  uint32 `json:"status"`
	Version uint64 `json:"version,omitempty"`
}

// storage.get(token, bucket_ptr, bucket_len, key_ptr, key_len, out_status, out_data_ptr, out_data_cap, out_data_len)
//
// Reads an object through the injected StorageClient. The client is
// expected to walk the cipherlake standard read path (metadata →
// backend.Get → cryptoCoordinator.DecryptOperation) so plugins never
// touch raw encrypted bytes on disk. storage.put remains Unauthorized
// (plugins are not allowed to bypass the gateway write path).
func (h *HostImports) storageGet(ctx context.Context, m api.Module, token uint64, bucketPtr uint32, bucketLen uint32, keyPtr uint32, keyLen uint32, outStatus uint32, outDataPtr uint32, outDataCap uint32, outDataLen uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}

	// 1. Base capability: storage:get
	if err := h.caps.Authorize(CapabilityToken(token), "storage:get"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}

	// 2. Read bucket and key from guest memory.
	bucket, ok := readString(mem, bucketPtr, bucketLen)
	if !ok {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	key, ok := readString(mem, keyPtr, keyLen)
	if !ok {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	if bucket == "" || key == "" {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}

	// 3. Scoped capability: storage:get:{bucket}/*
	requestedCap := "storage:get:" + bucket + "/*"
	if err := h.caps.Authorize(CapabilityToken(token), requestedCap); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}

	// 4. Resolve the principal for the audit/ownership check.
	pluginName := h.pluginNameForToken(CapabilityToken(token))

	// 5. No storage backend wired in - return Unauthorized to keep the
	// Phase 1 contract that storage is only available when an
	// administrator explicitly wires in a client.
	if h.storage == nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}

	// 6. Call the storage client (which internally walks the cipherlake
	// standard read path).
	data, err := h.storage.GetObject(ctx, bucket, key, pluginName)
	if err != nil {
		h.logger.Warn("storage.get failed",
			zap.String("plugin", pluginName),
			zap.String("bucket", bucket),
			zap.String("key", key),
			zap.Error(err))
		// Heuristic: NotFound vs Internal.
		if isNotFoundErr(err) {
			writeStatus(mem, outStatus, statusNotFound)
		} else {
			writeStatus(mem, outStatus, statusInternal)
		}
		return
	}

	// 7. Write the plaintext bytes back into guest memory.
	if uint32(len(data)) > outDataCap {
		writeStatus(mem, outStatus, statusBufferTooSmall)
		writeUint32(mem, outDataLen, uint32(len(data)))
		return
	}
	if !mem.Write(outDataPtr, data) {
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	writeStatus(mem, outStatus, statusOK)
	writeUint32(mem, outDataLen, uint32(len(data)))
}

// storage.put(token, bucket_ptr, bucket_len, key_ptr, key_len, body_ptr, body_len, out_status, out_etag_ptr, out_etag_cap, out_etag_len)
//
// Writes an object through the cipherlake gateway standard PUT path
// (auth → EncryptOperation → backend.Put → metadata index). Plugins
// present the plaintext body; the StorageClient forwards it over HTTP
// to the gateway, which performs KMS-based encryption, bucket policy
// enforcement, IAM audit, and metadata indexing. The plugin never
// handles encryption keys or ciphertext.
//
// When no StorageClient is wired in, returns Unauthorized.
func (h *HostImports) storagePut(ctx context.Context, m api.Module, token uint64, bucketPtr uint32, bucketLen uint32, keyPtr uint32, keyLen uint32, bodyPtr uint32, bodyLen uint32, outStatus uint32, outEtagPtr uint32, outEtagCap uint32, outEtagLen uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}

	// 1. Base capability: storage:put
	if err := h.caps.Authorize(CapabilityToken(token), "storage:put"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}

	// 2. Read bucket and key from guest memory.
	bucket, ok := readString(mem, bucketPtr, bucketLen)
	if !ok {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	key, ok := readString(mem, keyPtr, keyLen)
	if !ok {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	if bucket == "" || key == "" {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}

	// 3. Scoped capability: storage:put:{bucket}/*
	requestedCap := "storage:put:" + bucket + "/*"
	if err := h.caps.Authorize(CapabilityToken(token), requestedCap); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}

	// 4. Read the plaintext body from guest memory.
	body, ok := readBytes(mem, bodyPtr, bodyLen)
	if !ok {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}

	// 5. Resolve the principal for audit attribution.
	pluginName := h.pluginNameForToken(CapabilityToken(token))

	// 6. No storage backend wired in - return Unauthorized to keep the
	// contract that storage writes are only available when an
	// administrator explicitly wires in a client.
	if h.storage == nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}

	// 7. Call the storage client (which forwards to the cipherlake
	// gateway standard PUT path: auth → EncryptOperation →
	// backend.Put → metadata index).
	etag, err := h.storage.PutObject(ctx, bucket, key, body, pluginName)
	if err != nil {
		h.logger.Warn("storage.put failed",
			zap.String("plugin", pluginName),
			zap.String("bucket", bucket),
			zap.String("key", key),
			zap.Error(err))
		writeStatus(mem, outStatus, statusInternal)
		return
	}

	// 8. Write the returned ETag back into guest memory.
	etagBytes := []byte(etag)
	if uint32(len(etagBytes)) > outEtagCap {
		// ETag too large for the guest buffer; still report OK because
		// the object was written successfully.
		h.logger.Warn("storage.put: etag truncated",
			zap.Int("etag_len", len(etagBytes)),
			zap.Uint32("etag_cap", outEtagCap))
		writeUint32(mem, outEtagLen, 0)
	} else if outEtagPtr != 0 && len(etagBytes) > 0 {
		if !mem.Write(outEtagPtr, etagBytes) {
			writeStatus(mem, outStatus, statusInternal)
			return
		}
		writeUint32(mem, outEtagLen, uint32(len(etagBytes)))
	} else {
		writeUint32(mem, outEtagLen, 0)
	}

	writeStatus(mem, outStatus, statusOK)
}

// storage.list(token, bucket_ptr, bucket_len, prefix_ptr, prefix_len, out_status, ...)
func (h *HostImports) storageList(ctx context.Context, m api.Module, token uint64, bucketPtr uint32, bucketLen uint32, prefixPtr uint32, prefixLen uint32, outStatus uint32, outPtr uint32, outCap uint32, outLen uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	writeStatus(mem, outStatus, statusUnauthorized)
}

// storage.delete(token, bucket_ptr, bucket_len, key_ptr, key_len, out_status)
func (h *HostImports) storageDelete(ctx context.Context, m api.Module, token uint64, bucketPtr uint32, bucketLen uint32, keyPtr uint32, keyLen uint32, outStatus uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	writeStatus(mem, outStatus, statusUnauthorized)
}

// vector.search(token, bucket_ptr, bucket_len, query_ptr, query_len_floats, top_k, out_status, out_result_ptr, out_result_cap, out_result_len)
func (h *HostImports) vectorSearch(ctx context.Context, m api.Module, token uint64, bucketPtr uint32, bucketLen uint32, queryPtr uint32, queryLenFloats uint32, topK uint32, outStatus uint32, outResultPtr uint32, outResultCap uint32, outResultLen uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	// Base capability check first: a token with no vector:search grant at
	// all should get Unauthorized even if the bucket argument is invalid.
	if err := h.caps.Authorize(CapabilityToken(token), "vector:search"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	bucket, ok := readString(mem, bucketPtr, bucketLen)
	if !ok {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	if bucket == "" {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	requestedCap := "vector:search:" + bucket + "/*"
	if err := h.caps.Authorize(CapabilityToken(token), requestedCap); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	if h.vector == nil {
		writeStatus(mem, outStatus, statusInternal)
		return
	}

	queryBytes, ok := readBytes(mem, queryPtr, queryLenFloats*4)
	if !ok {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	queryVec := make([]float32, queryLenFloats)
	for i := uint32(0); i < queryLenFloats; i++ {
		bits := binary.LittleEndian.Uint32(queryBytes[i*4:])
		queryVec[i] = math.Float32frombits(bits)
	}

	results, err := h.vector.Search(ctx, vector.Vector{
		Bucket:    bucket,
		Values:    queryVec,
		Dimension: int(queryLenFloats),
	}, int(topK), map[string]string{"bucket": bucket})
	if err != nil {
		h.logger.Error("vector.search failed", zap.Error(err), zap.String("bucket", bucket))
		writeStatus(mem, outStatus, statusInternal)
		return
	}

	type resultDTO struct {
		Key   string  `json:"key"`
		Score float32 `json:"score"`
	}
	dtos := make([]resultDTO, len(results))
	for i, r := range results {
		dtos[i] = resultDTO{Key: r.ObjectKey, Score: r.Score}
	}
	out, err := json.Marshal(dtos)
	if err != nil {
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	if uint32(len(out)) > outResultCap {
		writeStatus(mem, outStatus, statusBufferTooSmall)
		writeUint32(mem, outResultLen, uint32(len(out)))
		return
	}
	if !mem.Write(outResultPtr, out) {
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	writeStatus(mem, outStatus, statusOK)
	writeUint32(mem, outResultLen, uint32(len(out)))
}

// vector.index(token, bucket_ptr, bucket_len, key_ptr, key_len, vec_ptr, vec_len_floats, out_status)
func (h *HostImports) vectorIndex(ctx context.Context, m api.Module, token uint64, bucketPtr uint32, bucketLen uint32, keyPtr uint32, keyLen uint32, vecPtr uint32, vecLenFloats uint32, outStatus uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	if err := h.caps.Authorize(CapabilityToken(token), "vector:index"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	bucket, ok := readString(mem, bucketPtr, bucketLen)
	if !ok || bucket == "" {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	key, ok := readString(mem, keyPtr, keyLen)
	if !ok || key == "" {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	requestedCap := "vector:index:" + bucket + "/*"
	if err := h.caps.Authorize(CapabilityToken(token), requestedCap); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	indexer, ok := h.vector.(VectorIndexer)
	if !ok || indexer == nil {
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	vecBytes, ok := readBytes(mem, vecPtr, vecLenFloats*4)
	if !ok || vecLenFloats == 0 {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	values := make([]float32, vecLenFloats)
	for i := uint32(0); i < vecLenFloats; i++ {
		bits := binary.LittleEndian.Uint32(vecBytes[i*4:])
		values[i] = math.Float32frombits(bits)
	}
	if err := indexer.Insert(ctx, []vector.Vector{{
		ID:        bucket + "/" + key,
		Bucket:    bucket,
		ObjectKey: key,
		Values:    values,
		Dimension: int(vecLenFloats),
		Metadata: map[string]string{
			"plugin": h.pluginNameForToken(CapabilityToken(token)),
		},
	}}); err != nil {
		h.logger.Error("vector.index failed", zap.Error(err), zap.String("bucket", bucket), zap.String("key", key))
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	writeStatus(mem, outStatus, statusOK)
}

// observability.log-emit(token, level_ptr, level_len, json_ptr, json_len)
func (h *HostImports) logEmit(ctx context.Context, m api.Module, token uint64, levelPtr uint32, levelLen uint32, jsonPtr uint32, jsonLen uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	if err := h.caps.Authorize(CapabilityToken(token), "log:emit"); err != nil {
		return // silent drop: logging failures should not trap the guest
	}
	level, _ := readString(mem, levelPtr, levelLen)
	payload, _ := readString(mem, jsonPtr, jsonLen)
	c := h.caps.Lookup(CapabilityToken(token))
	pluginName := ""
	if c != nil {
		pluginName = c.PluginName
	}
	// Buffer for nexusctl plugin logs; filtered by per-plugin dynamic level.
	if h.loader != nil {
		h.loader.DebugLog(pluginName, level, payload)
	}
	// Forward to the global slog logger with trace context (spec §3.17).
	logger := observability.ContextLogger(ctx).With(slog.String("plugin", pluginName))
	logger.Log(ctx, parseSlogLevel(level), payload, pluginLogAttrs(payload)...)
}

// observability.metric-record(token, name_ptr, name_len, value f64, labels_ptr, labels_len)
func (h *HostImports) metricRecord(ctx context.Context, m api.Module, token uint64, namePtr uint32, nameLen uint32, value float64, labelsPtr uint32, labelsLen uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	if err := h.caps.Authorize(CapabilityToken(token), "metric:record"); err != nil {
		return
	}
	name, _ := readString(mem, namePtr, nameLen)
	labelsJSON, _ := readString(mem, labelsPtr, labelsLen)
	c := h.caps.Lookup(CapabilityToken(token))
	pluginName := ""
	if c != nil {
		pluginName = c.PluginName
	}

	labels := make(map[string]string)
	if labelsJSON != "" {
		_ = json.Unmarshal([]byte(labelsJSON), &labels) // best-effort
	}

	// Buffer for nexusctl plugin metrics.
	if h.loader != nil {
		h.loader.DebugMetric(pluginName, name, value, labelsJSON)
	}
	// Forward to Prometheus with cardinality limits (spec §3.17, P4-4).
	if h.pluginMetrics != nil {
		h.pluginMetrics.Record(pluginName, name, value, labels)
	}
}

// observability.trace-span-start(token, name_ptr, name_len, attrs_json_ptr, attrs_json_len, out_span_handle)
func (h *HostImports) traceSpanStart(ctx context.Context, m api.Module, token uint64, namePtr uint32, nameLen uint32, attrsPtr uint32, attrsLen uint32, outHandle uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	if err := h.caps.Authorize(CapabilityToken(token), "trace:span"); err != nil {
		return
	}
	name, _ := readString(mem, namePtr, nameLen)
	attrsJSON, _ := readString(mem, attrsPtr, attrsLen)
	c := h.caps.Lookup(CapabilityToken(token))
	pluginName := ""
	if c != nil {
		pluginName = c.PluginName
	}

	var attrs []attribute.KeyValue
	if attrsJSON != "" {
		var kv map[string]string
		if err := json.Unmarshal([]byte(attrsJSON), &kv); err == nil {
			for k, v := range kv {
				attrs = append(attrs, attribute.String(k, v))
			}
		}
	}

	var handle uint64
	if h.pluginTracer != nil {
		var err error
		handle, _, err = h.pluginTracer.Start(ctx, pluginName, name, attrs)
		if err != nil {
			h.logger.Debug("trace_span_start failed", zap.Error(err), zap.String("plugin", pluginName))
		}
	}
	writeUint64(mem, outHandle, handle)
}

// observability.trace-span-end(token, span_handle)
func (h *HostImports) traceSpanEnd(ctx context.Context, m api.Module, token uint64, spanHandle uint64) {
	if err := h.caps.Authorize(CapabilityToken(token), "trace:span"); err != nil {
		return
	}
	if h.pluginTracer != nil {
		_ = h.pluginTracer.End(spanHandle)
	}
}

// http.fetch(token, method_ptr, method_len, url_ptr, url_len, headers_json_ptr,
// headers_json_len, body_ptr, body_len, timeout_ms, out_status, out_resp_ptr,
// out_resp_cap, out_resp_len)
//
// Performs an outbound HTTP request on behalf of the plugin. The plugin must
// hold the net:egress capability and the URL must pass the network egress
// whitelist (spec §3.10, P2-3). The response is written as a JSON object:
//
//	{"status":200,"status_text":"OK","headers":{},"body_b64":"..."}
func (h *HostImports) httpFetch(ctx context.Context, m api.Module, token uint64, methodPtr uint32, methodLen uint32, urlPtr uint32, urlLen uint32, headersPtr uint32, headersLen uint32, bodyPtr uint32, bodyLen uint32, timeoutMs uint32, outStatus uint32, outRespPtr uint32, outRespCap uint32, outRespLen uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	if err := h.caps.Authorize(CapabilityToken(token), "net:egress"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	c := h.caps.Lookup(CapabilityToken(token))
	if c == nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}

	method, ok := readString(mem, methodPtr, methodLen)
	if !ok {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	rawURL, ok := readString(mem, urlPtr, urlLen)
	if !ok || rawURL == "" {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	headersJSON, ok := readString(mem, headersPtr, headersLen)
	if !ok {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	body, ok := readBytes(mem, bodyPtr, bodyLen)
	if !ok {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}

	validator := h.validatorForModule(m)
	if validator == nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	if err := validator.Allow(rawURL); err != nil {
		logger.AuditLogger("http.fetch", rawURL, c.PluginName, "denied", map[string]interface{}{
			"method": method,
			"reason": err.Error(),
		})
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}

	if h.httpFetcher == nil {
		writeStatus(mem, outStatus, statusInternal)
		return
	}

	var headers map[string]string
	if headersJSON != "" {
		if err := json.Unmarshal([]byte(headersJSON), &headers); err != nil {
			writeStatus(mem, outStatus, statusInvalidArgs)
			return
		}
	}

	var timeout time.Duration
	if timeoutMs > 0 {
		timeout = time.Duration(timeoutMs) * time.Millisecond
	}

	result, err := h.httpFetcher.Fetch(method, rawURL, headers, body, timeout)
	if err != nil {
		h.logger.Warn("http.fetch failed",
			zap.String("plugin", c.PluginName),
			zap.String("url", rawURL),
			zap.Error(err),
		)
		writeStatus(mem, outStatus, statusInternal)
		return
	}

	out, err := json.Marshal(result)
	if err != nil {
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	if uint32(len(out)) > outRespCap {
		writeStatus(mem, outStatus, statusBufferTooSmall)
		writeUint32(mem, outRespLen, uint32(len(out)))
		return
	}
	if !mem.Write(outRespPtr, out) {
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	writeStatus(mem, outStatus, statusOK)
	writeUint32(mem, outRespLen, uint32(len(out)))
}

// route.response-write(token, req_handle i64, status_code i32, headers_ptr, headers_len, body_ptr, body_len, out_status)
func (h *HostImports) routeResponseWrite(ctx context.Context, m api.Module, token uint64, reqHandle uint64, statusCode uint32, headersPtr uint32, headersLen uint32, bodyPtr uint32, bodyLen uint32, outStatus uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	if err := h.caps.Authorize(CapabilityToken(token), "http:route"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	body, ok := readBytes(mem, bodyPtr, bodyLen)
	if !ok {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	headersJSON, ok := readString(mem, headersPtr, headersLen)
	if !ok {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}

	var headers map[string]string
	if headersJSON != "" {
		if err := json.Unmarshal([]byte(headersJSON), &headers); err != nil {
			writeStatus(mem, outStatus, statusInvalidArgs)
			return
		}
	}

	if err := h.loader.WriteResponse(reqHandle, int(statusCode), headers, body); err != nil {
		h.logger.Debug("route.response-write failed",
			zap.Uint64("req_handle", reqHandle),
			zap.Error(err),
		)
		writeStatus(mem, outStatus, statusInternal)
		return
	}

	c := h.caps.Lookup(CapabilityToken(token))
	pluginName := ""
	if c != nil {
		pluginName = c.PluginName
	}
	h.logger.Debug("route.response-write",
		zap.String("plugin", pluginName),
		zap.Uint64("req_handle", reqHandle),
		zap.Uint32("status", statusCode),
		zap.Int("body_len", len(body)),
		zap.String("headers", headersJSON),
	)
	writeStatus(mem, outStatus, statusOK)
}

// route.event-publish(token, session_id, event_type_ptr, event_type_len,
// payload_ptr, payload_len, out_status)
//
// Publishes an event to an SSE session. The session must be owned by the
// calling plugin; cross-plugin publish is rejected with ErrUnauthorized
// (spec §3.13 A12).
func (h *HostImports) routeEventPublish(ctx context.Context, m api.Module, token uint64, sessionID uint64, eventTypePtr uint32, eventTypeLen uint32, payloadPtr uint32, payloadLen uint32, outStatus uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	if err := h.caps.Authorize(CapabilityToken(token), "event:publish"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	c := h.caps.Lookup(CapabilityToken(token))
	if c == nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	owner := h.loader.GetSSESessionOwner(sessionID)
	if owner == "" {
		writeStatus(mem, outStatus, statusNotFound)
		return
	}
	if owner != c.PluginName {
		logger.AuditLogger("route.event-publish", c.PluginName, "", "denied", map[string]interface{}{
			"session_id": sessionID,
			"owner":      owner,
		})
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	eventType, _ := readString(mem, eventTypePtr, eventTypeLen)
	payload, _ := readBytes(mem, payloadPtr, payloadLen)
	logger.AuditLogger("route.event-publish", c.PluginName, "", "allowed", map[string]interface{}{
		"session_id":    sessionID,
		"event_type":    eventType,
		"payload_bytes": len(payload),
	})
	writeStatus(mem, outStatus, statusOK)
}

// event.subscribe(token, event_type_ptr, event_type_len, out_handle, out_status)
//
// Subscribes the calling plugin to events matching event_type. Matching
// events are dispatched to the plugin's on_event entry point (or the handler
// declared in the manifest) via a handle that can be read with event.read.
func (h *HostImports) eventSubscribe(ctx context.Context, m api.Module, token uint64, eventTypePtr uint32, eventTypeLen uint32, outHandle uint32, outStatus uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	if err := h.caps.Authorize(CapabilityToken(token), "event:subscribe"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	c := h.caps.Lookup(CapabilityToken(token))
	if c == nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	eventType, ok := readString(mem, eventTypePtr, eventTypeLen)
	if !ok || eventType == "" {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}

	handle, err := h.loader.SubscribePluginEvent(c.PluginName, eventType, "on_event")
	if err != nil {
		h.logger.Error("event.subscribe failed", zap.Error(err), zap.String("plugin", c.PluginName))
		writeStatus(mem, outStatus, statusInternal)
		return
	}

	logger.AuditLogger("event.subscribe", c.PluginName, "", "allowed", map[string]interface{}{
		"event_type": eventType,
		"handle":     handle,
	})
	writeUint64(mem, outHandle, handle)
	writeStatus(mem, outStatus, statusOK)
}

// event.unsubscribe(token, handle, out_status)
func (h *HostImports) eventUnsubscribe(ctx context.Context, m api.Module, token uint64, handle uint64, outStatus uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	if err := h.caps.Authorize(CapabilityToken(token), "event:subscribe"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	c := h.caps.Lookup(CapabilityToken(token))
	if c == nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	if err := h.loader.UnsubscribePluginEvent(c.PluginName, handle); err != nil {
		logger.AuditLogger("event.unsubscribe", c.PluginName, "", "denied", map[string]interface{}{
			"handle": handle,
			"error":  err.Error(),
		})
		writeStatus(mem, outStatus, statusNotFound)
		return
	}
	logger.AuditLogger("event.unsubscribe", c.PluginName, "", "allowed", map[string]interface{}{
		"handle": handle,
	})
	writeStatus(mem, outStatus, statusOK)
}

// event.read(token, event_handle, out_ptr, out_cap, out_len, out_status)
//
// Reads the JSON payload of the event associated with event_handle. The
// handle is passed to the plugin's on_event entry point.
func (h *HostImports) eventRead(ctx context.Context, m api.Module, token uint64, eventHandle uint64, outPtr uint32, outCap uint32, outLen uint32, outStatus uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	if err := h.caps.Authorize(CapabilityToken(token), "event:subscribe"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	c := h.caps.Lookup(CapabilityToken(token))
	if c == nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	inv := h.loader.GetEventContext(eventHandle)
	if inv == nil {
		writeStatus(mem, outStatus, statusNotFound)
		return
	}
	if uint32(len(inv.payload)) > outCap {
		writeStatus(mem, outStatus, statusBufferTooSmall)
		writeUint32(mem, outLen, uint32(len(inv.payload)))
		return
	}
	if !mem.Write(outPtr, inv.payload) {
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	writeStatus(mem, outStatus, statusOK)
	writeUint32(mem, outLen, uint32(len(inv.payload)))
}

// event.publish(token, event_json_ptr, event_json_len, out_status)
//
// Publishes an event to the system event bus. Other plugins subscribed to
// the event type will receive it. Requires the event:publish capability.
func (h *HostImports) eventPublish(ctx context.Context, m api.Module, token uint64, eventJSONPtr uint32, eventJSONLen uint32, outStatus uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	if err := h.caps.Authorize(CapabilityToken(token), "event:publish"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	c := h.caps.Lookup(CapabilityToken(token))
	if c == nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	eventJSON, ok := readBytes(mem, eventJSONPtr, eventJSONLen)
	if !ok {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	var ev events.Event
	if err := json.Unmarshal(eventJSON, &ev); err != nil {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	if ev.EventType == "" {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}

	if err := h.loader.EventBusPublish(ctx, &ev); err != nil {
		h.logger.Error("event.publish failed", zap.Error(err), zap.String("plugin", c.PluginName))
		writeStatus(mem, outStatus, statusInternal)
		return
	}

	logger.AuditLogger("event.publish", c.PluginName, "", "allowed", map[string]interface{}{
		"event_type": ev.EventType,
		"bucket":     ev.Bucket,
		"key":        ev.Key,
	})
	writeStatus(mem, outStatus, statusOK)
}

// task.schedule(token, cron_ptr, cron_len, payload_ptr, payload_len, out_handle, out_status)
//
// Registers a dynamic cron schedule for the calling plugin. On each trigger
// the loader invokes the plugin's on_task entry point with a task handle that
// can be read with task.read.
func (h *HostImports) taskSchedule(ctx context.Context, m api.Module, token uint64, cronPtr uint32, cronLen uint32, payloadPtr uint32, payloadLen uint32, outHandle uint32, outStatus uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	if err := h.caps.Authorize(CapabilityToken(token), "task:schedule"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	c := h.caps.Lookup(CapabilityToken(token))
	if c == nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	cronExpr, ok := readString(mem, cronPtr, cronLen)
	if !ok || cronExpr == "" {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	payload, _ := readBytes(mem, payloadPtr, payloadLen)

	handle, err := h.loader.SchedulePluginTask(c.PluginName, cronExpr, payload, "on_task")
	if err != nil {
		h.logger.Error("task.schedule failed", zap.Error(err), zap.String("plugin", c.PluginName))
		writeStatus(mem, outStatus, statusInternal)
		return
	}

	logger.AuditLogger("task.schedule", c.PluginName, "", "allowed", map[string]interface{}{
		"schedule": cronExpr,
		"handle":   handle,
	})
	writeUint64(mem, outHandle, handle)
	writeStatus(mem, outStatus, statusOK)
}

// task.unschedule(token, handle, out_status)
func (h *HostImports) taskUnschedule(ctx context.Context, m api.Module, token uint64, handle uint64, outStatus uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	if err := h.caps.Authorize(CapabilityToken(token), "task:schedule"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	c := h.caps.Lookup(CapabilityToken(token))
	if c == nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	if err := h.loader.UnschedulePluginTask(c.PluginName, handle); err != nil {
		logger.AuditLogger("task.unschedule", c.PluginName, "", "denied", map[string]interface{}{
			"handle": handle,
			"error":  err.Error(),
		})
		writeStatus(mem, outStatus, statusNotFound)
		return
	}
	logger.AuditLogger("task.unschedule", c.PluginName, "", "allowed", map[string]interface{}{
		"handle": handle,
	})
	writeStatus(mem, outStatus, statusOK)
}

// task.read(token, task_handle, out_ptr, out_cap, out_len, out_status)
//
// Reads the payload of the task associated with task_handle. The handle is
// passed to the plugin's on_task entry point.
func (h *HostImports) taskRead(ctx context.Context, m api.Module, token uint64, taskHandle uint64, outPtr uint32, outCap uint32, outLen uint32, outStatus uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	if err := h.caps.Authorize(CapabilityToken(token), "task:schedule"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	c := h.caps.Lookup(CapabilityToken(token))
	if c == nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	inv := h.loader.GetTaskContext(taskHandle)
	if inv == nil {
		writeStatus(mem, outStatus, statusNotFound)
		return
	}
	if uint32(len(inv.payload)) > outCap {
		writeStatus(mem, outStatus, statusBufferTooSmall)
		writeUint32(mem, outLen, uint32(len(inv.payload)))
		return
	}
	if !mem.Write(outPtr, inv.payload) {
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	writeStatus(mem, outStatus, statusOK)
	writeUint32(mem, outLen, uint32(len(inv.payload)))
}

// pipeline.read(token, step_handle, out_ptr, out_cap, out_len, out_status)
//
// Writes a JSON object describing the pipeline step input:
//
//	{"key":"...","bucket":"...","content_type":"...","user_metadata":{},"body_len":N}
func (h *HostImports) pipelineRead(ctx context.Context, m api.Module, token uint64, stepHandle uint64, outPtr uint32, outCap uint32, outLen uint32, outStatus uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	if err := h.caps.Authorize(CapabilityToken(token), "pipeline:step"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	c := h.caps.Lookup(CapabilityToken(token))
	if c == nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	inv := h.loader.GetPipelineContext(stepHandle)
	if inv == nil {
		writeStatus(mem, outStatus, statusNotFound)
		return
	}
	if c.PluginName != inv.pluginName {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	info := map[string]any{
		"key":           inv.key,
		"bucket":        inv.bucket,
		"content_type":  inv.contentType,
		"user_metadata": inv.userMetadata,
		"body_len":      len(inv.body),
	}
	out, err := json.Marshal(info)
	if err != nil {
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	if uint32(len(out)) > outCap {
		writeStatus(mem, outStatus, statusBufferTooSmall)
		writeUint32(mem, outLen, uint32(len(out)))
		return
	}
	if !mem.Write(outPtr, out) {
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	writeStatus(mem, outStatus, statusOK)
	writeUint32(mem, outLen, uint32(len(out)))
}

// pipeline.body(token, step_handle, out_ptr, out_cap, out_len, out_status)
//
// Writes the raw input body for the pipeline step.
func (h *HostImports) pipelineBody(ctx context.Context, m api.Module, token uint64, stepHandle uint64, outPtr uint32, outCap uint32, outLen uint32, outStatus uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	if err := h.caps.Authorize(CapabilityToken(token), "pipeline:step"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	c := h.caps.Lookup(CapabilityToken(token))
	if c == nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	inv := h.loader.GetPipelineContext(stepHandle)
	if inv == nil {
		writeStatus(mem, outStatus, statusNotFound)
		return
	}
	if c.PluginName != inv.pluginName {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	if uint32(len(inv.body)) > outCap {
		writeStatus(mem, outStatus, statusBufferTooSmall)
		writeUint32(mem, outLen, uint32(len(inv.body)))
		return
	}
	if !mem.Write(outPtr, inv.body) {
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	writeStatus(mem, outStatus, statusOK)
	writeUint32(mem, outLen, uint32(len(inv.body)))
}

// pipeline.output.write(token, step_handle, body_ptr, body_len, content_type_ptr,
// content_type_len, metadata_json_ptr, metadata_json_len, out_status)
//
// Writes the output of a pipeline step. The body becomes the transformed
// object content; content_type and metadata are applied to the resulting
// ObjectOutput.
func (h *HostImports) pipelineOutputWrite(ctx context.Context, m api.Module, token uint64, stepHandle uint64, bodyPtr uint32, bodyLen uint32, contentTypePtr uint32, contentTypeLen uint32, metadataJSONPtr uint32, metadataJSONLen uint32, outStatus uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	if err := h.caps.Authorize(CapabilityToken(token), "pipeline:step"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	c := h.caps.Lookup(CapabilityToken(token))
	if c == nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	inv := h.loader.GetPipelineContext(stepHandle)
	if inv == nil {
		writeStatus(mem, outStatus, statusNotFound)
		return
	}
	if c.PluginName != inv.pluginName {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	body, ok := readBytes(mem, bodyPtr, bodyLen)
	if !ok {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	contentType, _ := readString(mem, contentTypePtr, contentTypeLen)
	metadataJSON, ok := readBytes(mem, metadataJSONPtr, metadataJSONLen)
	if !ok {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	var metadata map[string]string
	if len(metadataJSON) > 0 {
		if err := json.Unmarshal(metadataJSON, &metadata); err != nil {
			writeStatus(mem, outStatus, statusInvalidArgs)
			return
		}
	}
	inv.outputBody = body
	inv.outputContentType = contentType
	inv.outputMetadata = metadata
	writeStatus(mem, outStatus, statusOK)
}

// request.read(req_handle, out_ptr, out_cap, out_len, out_status)
// Writes a JSON object {"method":"...","path":"...","headers":{...},"body_len":N,"original_user":"..."}.
func (h *HostImports) requestRead(ctx context.Context, m api.Module, reqHandle uint64, outPtr uint32, outCap uint32, outLen uint32, outStatus uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	req := h.loader.GetRequest(reqHandle)
	if req == nil {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	info := map[string]any{
		"method":        req.method,
		"path":          req.path,
		"headers":       req.headers,
		"body_len":      len(req.body),
		"original_user": req.originalUser,
	}
	out, err := json.Marshal(info)
	if err != nil {
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	if uint32(len(out)) > outCap {
		writeStatus(mem, outStatus, statusBufferTooSmall)
		writeUint32(mem, outLen, uint32(len(out)))
		return
	}
	if !mem.Write(outPtr, out) {
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	writeStatus(mem, outStatus, statusOK)
	writeUint32(mem, outLen, uint32(len(out)))
}

// request.body(req_handle, out_ptr, out_cap, out_len, out_status)
// Writes the raw request body.
func (h *HostImports) requestBody(ctx context.Context, m api.Module, reqHandle uint64, outPtr uint32, outCap uint32, outLen uint32, outStatus uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	req := h.loader.GetRequest(reqHandle)
	if req == nil {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	if uint32(len(req.body)) > outCap {
		writeStatus(mem, outStatus, statusBufferTooSmall)
		writeUint32(mem, outLen, uint32(len(req.body)))
		return
	}
	if !mem.Write(outPtr, req.body) {
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	writeStatus(mem, outStatus, statusOK)
	writeUint32(mem, outLen, uint32(len(req.body)))
}

// memory helpers.

func readBytes(mem api.Memory, ptr, len uint32) ([]byte, bool) {
	if len == 0 {
		return nil, true
	}
	b, ok := mem.Read(ptr, len)
	if !ok {
		return nil, false
	}
	out := make([]byte, len)
	copy(out, b)
	return out, true
}

func readString(mem api.Memory, ptr, len uint32) (string, bool) {
	b, ok := readBytes(mem, ptr, len)
	if !ok {
		return "", false
	}
	return string(b), true
}

func writeStatus(mem api.Memory, ptr uint32, status uint32) {
	_ = writeUint32(mem, ptr, status)
}

func writeUint32(mem api.Memory, ptr uint32, v uint32) bool {
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, v)
	return mem.Write(ptr, buf)
}

func writeUint64(mem api.Memory, ptr uint32, v uint64) bool {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, v)
	return mem.Write(ptr, buf)
}

// pluginNameForToken resolves the plugin name owning a capability token.
// Used by storage.get to attribute reads to a principal for audit.
func (h *HostImports) pluginNameForToken(tok CapabilityToken) string {
	if h.caps == nil {
		return ""
	}
	if c := h.caps.Lookup(tok); c != nil {
		return c.PluginName
	}
	return ""
}

// isNotFoundErr returns true if err indicates the object does not exist.
// It handles both sentinel errors and common error-message patterns
// produced by the storage layer.
func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNotFound) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "NoSuchKey") ||
		strings.Contains(msg, "does not exist")
}

// float64 -> uint64 bit-cast helper for value parameters.
func float64ToUint64(v float64) uint64 {
	return math.Float64bits(v)
}

// uint64 -> float64 bit-cast helper.
func uint64ToFloat64(v uint64) float64 {
	return math.Float64frombits(v)
}

// intToFloat64 converts an integer to float64. Kept for symmetry with
// value passing conventions; currently unused but part of the ABI toolkit.
func intToFloat64(v int) float64 {
	return float64(v)
}

// parseFloat64 converts a string to float64; used by tests that validate
// metric value encoding. strconv is the source of truth here.
func parseFloat64(s string) (float64, error) {
	return strconv.ParseFloat(s, 64)
}

var _ = parseFloat64
var _ = float64ToUint64
var _ = uint64ToFloat64
var _ = intToFloat64
var _ = time.Now

// --- hook host imports (spec §3.2, P2-4) ---

// hook.read(handle, out_ptr, out_cap, out_len, out_status)
// Writes a JSON object describing the hook invocation context.
func (h *HostImports) hookRead(ctx context.Context, m api.Module, handle uint64, outPtr uint32, outCap uint32, outLen uint32, outStatus uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	if err := h.caps.Authorize(h.tokenForModule(m), "gateway:hook"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	inv := h.loader.GetHookContext(handle)
	if inv == nil {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	info := map[string]any{
		"operation":     inv.ctx.Operation,
		"phase":         inv.ctx.Phase,
		"bucket":        inv.ctx.Bucket,
		"key":           inv.ctx.Key,
		"method":        inv.ctx.Method,
		"headers":       inv.ctx.Headers,
		"body_len":      len(inv.ctx.Body),
		"original_user": inv.ctx.OriginalUser,
		"status_code":   inv.ctx.StatusCode,
	}
	out, err := json.Marshal(info)
	if err != nil {
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	if uint32(len(out)) > outCap {
		writeStatus(mem, outStatus, statusBufferTooSmall)
		writeUint32(mem, outLen, uint32(len(out)))
		return
	}
	if !mem.Write(outPtr, out) {
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	writeStatus(mem, outStatus, statusOK)
	writeUint32(mem, outLen, uint32(len(out)))
}

// hook.body(handle, out_ptr, out_cap, out_len, out_status)
// Writes the raw request/response body from the hook context.
func (h *HostImports) hookBody(ctx context.Context, m api.Module, handle uint64, outPtr uint32, outCap uint32, outLen uint32, outStatus uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	if err := h.caps.Authorize(h.tokenForModule(m), "gateway:hook"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	inv := h.loader.GetHookContext(handle)
	if inv == nil {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	body := inv.ctx.Body
	if uint32(len(body)) > outCap {
		writeStatus(mem, outStatus, statusBufferTooSmall)
		writeUint32(mem, outLen, uint32(len(body)))
		return
	}
	if !mem.Write(outPtr, body) {
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	writeStatus(mem, outStatus, statusOK)
	writeUint32(mem, outLen, uint32(len(body)))
}

// hook.modify(handle, patch_json_ptr, patch_json_len, out_status)
// Patch fields: headers, body_b64, status_code, short_circuit.
func (h *HostImports) hookModify(ctx context.Context, m api.Module, handle uint64, patchPtr uint32, patchLen uint32, outStatus uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	if err := h.caps.Authorize(h.tokenForModule(m), "gateway:hook"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	inv := h.loader.GetHookContext(handle)
	if inv == nil {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	patchJSON, ok := readBytes(mem, patchPtr, patchLen)
	if !ok {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	var patch hookModifyPatch
	if err := json.Unmarshal(patchJSON, &patch); err != nil {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}

	inv.mu.Lock()
	defer inv.mu.Unlock()

	if inv.modified == nil {
		inv.modified = cloneHookContext(inv.ctx)
	}
	if len(patch.Headers) > 0 {
		if inv.modified.Headers == nil {
			inv.modified.Headers = make(map[string]string)
		}
		for k, v := range patch.Headers {
			inv.modified.Headers[k] = v
		}
	}
	if patch.BodyB64 != "" {
		body, err := base64.StdEncoding.DecodeString(patch.BodyB64)
		if err != nil {
			writeStatus(mem, outStatus, statusInvalidArgs)
			return
		}
		inv.modified.Body = body
	}
	if patch.StatusCode != 0 {
		inv.modified.StatusCode = patch.StatusCode
	}
	if patch.ShortCircuit {
		inv.shortCircuit = true
		inv.abortStatus = inv.modified.StatusCode
		inv.abortHeaders = inv.modified.Headers
		inv.abortBody = inv.modified.Body
	}
	writeStatus(mem, outStatus, statusOK)
}

// hook.short-circuit(handle, status_code, headers_json_ptr, headers_json_len, body_ptr, body_len, out_status)
func (h *HostImports) hookShortCircuit(ctx context.Context, m api.Module, handle uint64, statusCode uint32, headersPtr uint32, headersLen uint32, bodyPtr uint32, bodyLen uint32, outStatus uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	if err := h.caps.Authorize(h.tokenForModule(m), "gateway:hook"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	inv := h.loader.GetHookContext(handle)
	if inv == nil {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	headersJSON, ok := readBytes(mem, headersPtr, headersLen)
	if !ok {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	body, ok := readBytes(mem, bodyPtr, bodyLen)
	if !ok {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	var headers map[string]string
	if err := json.Unmarshal(headersJSON, &headers); err != nil {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}

	inv.mu.Lock()
	inv.shortCircuit = true
	inv.abortStatus = int32(statusCode)
	inv.abortHeaders = headers
	inv.abortBody = body
	inv.mu.Unlock()

	writeStatus(mem, outStatus, statusOK)
}

// iam.policy.evaluate(token, req_ptr, req_len, out_ptr, out_cap, out_status, out_len)
//
// Tier-2-only host import that evaluates IAM policies for the supplied
// request context and returns the decision as JSON.
func (h *HostImports) iamPolicyEvaluate(ctx context.Context, m api.Module, token uint64, reqPtr uint32, reqLen uint32, outPtr uint32, outCap uint32, outStatus uint32, outLen uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	if err := h.caps.Authorize(CapabilityToken(token), "iam:policy:evaluate"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	if h.policyEvaluator == nil {
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	reqBytes, ok := readBytes(mem, reqPtr, reqLen)
	if !ok {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	var req struct {
		Principal  string            `json:"principal"`
		Action     string            `json:"action"`
		Resource   string            `json:"resource"`
		SourceIP   string            `json:"source_ip,omitempty"`
		Conditions map[string]string `json:"conditions,omitempty"`
	}
	if err := json.Unmarshal(reqBytes, &req); err != nil {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}

	result := h.policyEvaluator.Evaluate(&iam.EvalContext{
		Principal:  req.Principal,
		Action:     req.Action,
		Resource:   req.Resource,
		SourceIP:   req.SourceIP,
		Conditions: req.Conditions,
		Time:       time.Now(),
	})

	resp := struct {
		Decision   string `json:"decision"`
		MatchedBy  string `json:"matched_by,omitempty"`
		PolicyType string `json:"policy_type,omitempty"`
		Details    string `json:"details,omitempty"`
	}{
		Decision:   result.Decision.String(),
		MatchedBy:  result.MatchedBy,
		PolicyType: result.PolicyType,
		Details:    result.Details,
	}
	respBytes, _ := json.Marshal(resp)
	writeBytesResponse(mem, outPtr, outCap, outStatus, outLen, respBytes)
}

// kms.datakey.generate(token, req_ptr, req_len, out_ptr, out_cap, out_status, out_len)
//
// Tier-2-only host import that generates a data encryption key (DEK),
// encrypts it with the KMS public key, and returns the ciphertext.
func (h *HostImports) kmsDataKeyGenerate(ctx context.Context, m api.Module, token uint64, reqPtr uint32, reqLen uint32, outPtr uint32, outCap uint32, outStatus uint32, outLen uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	if err := h.caps.Authorize(CapabilityToken(token), "kms:datakey:generate"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	if h.dataKeyGenerator == nil {
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	reqBytes, ok := readBytes(mem, reqPtr, reqLen)
	if !ok {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	var req struct {
		KeyID     string `json:"key_id,omitempty"`
		Length    int    `json:"length,omitempty"`
		Bucket    string `json:"bucket,omitempty"`
		ObjectKey string `json:"object_key,omitempty"`
	}
	if err := json.Unmarshal(reqBytes, &req); err != nil {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	if req.Length <= 0 {
		req.Length = 32
	}

	_, encrypted, err := h.dataKeyGenerator.GenerateDataKey(ctx, req.KeyID, req.Length)
	if err != nil {
		h.logger.Error("kms.datakey.generate failed", zap.Error(err))
		writeStatus(mem, outStatus, statusInternal)
		return
	}

	resp := struct {
		EncryptedDEK string `json:"encrypted_dek"`
		Algorithm    string `json:"algorithm"`
		KeyID        string `json:"key_id"`
	}{
		EncryptedDEK: base64.StdEncoding.EncodeToString(encrypted),
		Algorithm:    "ECIES-P256-AES-256-GCM",
		KeyID:        req.KeyID,
	}
	respBytes, _ := json.Marshal(resp)
	writeBytesResponse(mem, outPtr, outCap, outStatus, outLen, respBytes)
}

// crypto.audit.sign(token, payload_ptr, payload_len, out_ptr, out_cap, out_status, out_len)
//
// Tier-2-only host import that signs an audit payload.
func (h *HostImports) cryptoAuditSign(ctx context.Context, m api.Module, token uint64, payloadPtr uint32, payloadLen uint32, outPtr uint32, outCap uint32, outStatus uint32, outLen uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	if err := h.caps.Authorize(CapabilityToken(token), "crypto:audit:sign"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	if h.auditSigner == nil {
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	payload, ok := readBytes(mem, payloadPtr, payloadLen)
	if !ok {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}

	sig, err := h.auditSigner.Sign(payload)
	if err != nil {
		h.logger.Error("crypto.audit.sign failed", zap.Error(err))
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	writeBytesResponse(mem, outPtr, outCap, outStatus, outLen, sig)
}

// gateway.hook.register-for-other(token, req_ptr, req_len, out_status)
//
// Tier-2-only host import that allows a core plugin to dynamically register
// a hook. The hook is associated with the caller plugin and is uninstalled
// when the caller is uninstalled.
func (h *HostImports) gatewayHookRegisterForOther(ctx context.Context, m api.Module, token uint64, reqPtr uint32, reqLen uint32, outStatus uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	if err := h.caps.Authorize(CapabilityToken(token), "gateway:hook:register_for_other"); err != nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	if h.hookRegistrar == nil {
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	c := h.caps.Lookup(CapabilityToken(token))
	if c == nil {
		writeStatus(mem, outStatus, statusUnauthorized)
		return
	}
	reqBytes, ok := readBytes(mem, reqPtr, reqLen)
	if !ok {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	var req struct {
		Operation string `json:"operation"`
		HookType  string `json:"hook_type"`
		Handler   string `json:"handler"`
		Priority  int    `json:"priority,omitempty"`
		Condition string `json:"condition,omitempty"`
		OnFailure string `json:"on_failure,omitempty"`
		Critical  bool   `json:"critical,omitempty"`
	}
	if err := json.Unmarshal(reqBytes, &req); err != nil {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}
	if req.Operation == "" || req.HookType == "" || req.Handler == "" {
		writeStatus(mem, outStatus, statusInvalidArgs)
		return
	}

	record := &HookRecord{
		PluginName: c.PluginName,
		HookID:     fmt.Sprintf("%s-dynamic-%d", c.PluginName, time.Now().UnixNano()),
		HookType:   req.HookType,
		Operation:  req.Operation,
		Condition:  req.Condition,
		Handler:    req.Handler,
		Priority:   req.Priority,
		OnFailure:  req.OnFailure,
		Critical:   req.Critical,
	}
	if err := h.hookRegistrar.RegisterHook(ctx, c.PluginName, record); err != nil {
		h.logger.Error("gateway.hook.register-for-other failed", zap.Error(err), zap.String("plugin", c.PluginName))
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	logger.AuditLogger("gateway.hook.register-for-other", c.PluginName, "", "allowed", map[string]interface{}{
		"operation": req.Operation,
		"handler":   req.Handler,
	})
	writeStatus(mem, outStatus, statusOK)
}

// writeBytesResponse writes a byte response to guest memory with
// ErrBufferTooSmall when the buffer is too small.
func writeBytesResponse(mem api.Memory, outPtr uint32, outCap uint32, outStatus uint32, outLen uint32, data []byte) {
	if uint32(len(data)) > outCap {
		writeStatus(mem, outStatus, statusBufferTooSmall)
		writeUint32(mem, outLen, uint32(len(data)))
		return
	}
	if !mem.Write(outPtr, data) {
		writeStatus(mem, outStatus, statusInternal)
		return
	}
	writeStatus(mem, outStatus, statusOK)
	writeUint32(mem, outLen, uint32(len(data)))
}
