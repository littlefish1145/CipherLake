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
//       0 = OK
//       1 = ErrUnauthorized
//       2 = ErrNotFound
//       3 = ErrVersionConflict
//       4 = ErrBufferTooSmall
//       5 = ErrInvalidArgs
//       100 = ErrInternal
package pluginloader

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"strconv"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"go.uber.org/zap"

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

// VectorSearcher is the subset of the vector manager used by host imports.
type VectorSearcher interface {
	Search(ctx context.Context, query vector.Vector, topK int, filters map[string]string) ([]vector.SearchResult, error)
}

// StorageClient is the subset of the storage layer used by host imports.
// Phase 1 leaves GetObject/PutObject as stubs returning ErrUnauthorized;
// the interface is wired now so Phase 3 can plug in real encrypt/decrypt
// forwarding without changing the ABI.
type StorageClient interface {
	GetObject(ctx context.Context, bucket, key, originalUser string) ([]byte, error)
	PutObject(ctx context.Context, bucket, key string, body []byte, originalUser string) error
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
}

// NewHostImports creates a host-imports layer wired to the given Loader and
// capability table. The vector searcher and storage client may be nil in
// Phase 1 (the corresponding imports will return ErrUnauthorized until
// plugged in).
func NewHostImports(loader *Loader, caps *CapabilityTable, vector VectorSearcher, storage StorageClient) *HostImports {
	if loader == nil {
		panic("NewHostImports: loader is nil")
	}
	return &HostImports{
		caps:    caps,
		loader:  loader,
		vector:  vector,
		storage: storage,
		logger:  zap.L(),
	}
}

// SetLogger replaces the default logger. Useful in tests.
func (h *HostImports) SetLogger(l *zap.Logger) {
	h.logger = l
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

	// Route response (spec §3.4).
	fb.WithFunc(h.routeResponseWrite).Export("route.response-write")

	// Request reading (spec §5 ctx_read for req_handle).
	fb.WithFunc(h.requestRead).Export("request.read")
	fb.WithFunc(h.requestBody).Export("request.body")

	return nil
}

// state.get(token, key_ptr, key_len, val_ptr, val_cap, out_status, out_len, out_version)
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

	entry, err := h.loader.StateGet(c.PluginName, string(key))
	if err != nil {
		h.logger.Error("state.get failed", zap.Error(err), zap.String("plugin", c.PluginName))
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
func (h *HostImports) storageGet(ctx context.Context, m api.Module, token uint64, bucketPtr uint32, bucketLen uint32, keyPtr uint32, keyLen uint32, outStatus uint32, outDataPtr uint32, outDataCap uint32, outDataLen uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	writeStatus(mem, outStatus, statusUnauthorized)
}

// storage.put(token, bucket_ptr, bucket_len, key_ptr, key_len, body_ptr, body_len, out_status, out_etag_ptr, out_etag_cap, out_etag_len)
func (h *HostImports) storagePut(ctx context.Context, m api.Module, token uint64, bucketPtr uint32, bucketLen uint32, keyPtr uint32, keyLen uint32, bodyPtr uint32, bodyLen uint32, outStatus uint32, outEtagPtr uint32, outEtagCap uint32, outEtagLen uint32) {
	mem := m.Memory()
	if mem == nil {
		return
	}
	writeStatus(mem, outStatus, statusUnauthorized)
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
	writeStatus(mem, outStatus, statusUnauthorized)
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
	h.logger.Info("plugin log",
		zap.String("plugin", pluginName),
		zap.String("level", level),
		zap.String("payload", payload),
	)
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
	_ = labelsJSON
	h.logger.Debug("plugin metric",
		zap.String("plugin", pluginName),
		zap.String("name", name),
		zap.Float64("value", value),
		zap.String("labels", labelsJSON),
	)
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

// parseLogLevel converts common log-level strings to zap levels.
func parseLogLevel(level string) zap.AtomicLevel {
	switch level {
	case "debug":
		return zap.NewAtomicLevelAt(zap.DebugLevel)
	case "info":
		return zap.NewAtomicLevelAt(zap.InfoLevel)
	case "warn", "warning":
		return zap.NewAtomicLevelAt(zap.WarnLevel)
	case "error":
		return zap.NewAtomicLevelAt(zap.ErrorLevel)
	default:
		lvl := zap.NewAtomicLevel()
		if err := lvl.UnmarshalText([]byte(level)); err != nil {
			return zap.NewAtomicLevelAt(zap.InfoLevel)
		}
		return lvl
	}
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
var _ = parseLogLevel
var _ = time.Now
