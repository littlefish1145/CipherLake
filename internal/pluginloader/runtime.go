package pluginloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"cipherlake/internal/config"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// Mandatory entry points every WASM module must export (spec §4).
// Modules missing any of these are rejected at install time with
// ErrInvalidEntryPoints.
const (
	EntryPointWasmInit  = "wasm_init"
	EntryPointOnHook    = "on_hook"
	EntryPointOnRequest = "on_request"
)

// Optional entry points. Modules MAY export these; absence is treated
// as graceful degradation (the loader skips the corresponding callback).
var optionalEntryPoints = []string{
	"on_install",
	"on_uninstall",
	"on_suspend",
	"on_resume",
	"on_event",
	"on_sse_close",
	"on_task",
	"on_pipeline_step",
}

// CompiledModule is a wazero-compiled representation of a plugin's
// WASM bytes. Compilation is reused across instances of the same
// plugin (spec §3.13 — instance pool). The compiled form is held by
// the runtime and reused on every borrow from the pool.
type CompiledModule struct {
	name    string
	version string
	src     wazero.CompiledModule
}

// Close releases the compiled module's resources. Callers should call
// this when a plugin is uninstalled.
func (c *CompiledModule) Close(ctx context.Context) error {
	if c == nil || c.src == nil {
		return nil
	}
	return c.src.Close(ctx)
}

// Name returns the plugin name this compiled module belongs to.
func (c *CompiledModule) Name() string { return c.name }

// Version returns the plugin semver from the manifest.
func (c *CompiledModule) Version() string { return c.version }

// WASMRuntime owns the wazero runtime and is shared across all
// plugins. There is one runtime per loader process — creating one
// runtime per plugin would multiply the ~5MB wazero baseline.
//
// The runtime is configured per spec §3.1: the compiler path is
// disabled (interpreter only) to keep the binary under the +15MB
// budget. Performance-sensitive callers (e.g. pipeline steps in
// Phase 4) may opt in to the compiler via a future config flag.
type WASMRuntime struct {
	rt       wazero.Runtime
	caps     *CapabilityTable
	hostInst HostInstantiator
}

// HostInstantiator is implemented by the host-imports layer (P1-4).
// When the runtime instantiates a WASM module, it calls back into the
// host layer to register the per-instance host functions (storage,
// state, vector, etc.) wired to the instance's capability token.
//
// Returning an error from Instantiate aborts the WASM instantiation
// and the module is not added to the pool.
type HostInstantiator interface {
	// Instantiate is called with the freshly-built module + the token
	// that was issued for this instance. Implementations must register
	// all host imports on the provided module builder and return the
	// finalized module.
	Instantiate(ctx context.Context, builder wazero.HostModuleBuilder, token CapabilityToken, pluginName string) error

	// OnModuleInstantiated is called after the plugin module has been
	// instantiated and before it is returned to the pool. Implementations
	// can use this to associate the live module with its capability token
	// for host-import authorization.
	OnModuleInstantiated(mod api.Module, token CapabilityToken)

	// OnModuleClosed is called when the plugin module is closed. The
	// implementation should clean up any module/token associations.
	OnModuleClosed(mod api.Module)
}

// NewWASMRuntime constructs a runtime with the interpreter-only engine
// (spec §3.1 binary budget). The capability table is shared so that
// host imports (which receive a token as their first arg) can resolve
// it through the runtime.
func NewWASMRuntime(caps *CapabilityTable) *WASMRuntime {
	return NewWASMRuntimeWithLimits(caps, NewResourceLimiter(config.PluginLoaderResourceLimits{}))
}

// NewWASMRuntimeWithLimits constructs a runtime with resource limits and
// context-cancel termination enabled (spec §3.6). CloseOnContextDone ensures
// that a WASM call whose context expires is terminated instead of blocking
// forever; the instance is then discarded by the pool.
func NewWASMRuntimeWithLimits(caps *CapabilityTable, limiter *ResourceLimiter) *WASMRuntime {
	ctx := context.Background()
	// Interpreter-only configuration. The compiler would add ~3MB to
	// the binary and a measurable startup latency; we opt out per spec.
	cfg := wazero.NewRuntimeConfigInterpreter().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(limiter.GlobalMemoryPages())
	rt := wazero.NewRuntimeWithConfig(ctx, cfg)
	// WASI Preview 1 is needed by tinygo-compiled plugins (spec §8.6).
	wasi_snapshot_preview1.Instantiate(ctx, rt)
	return &WASMRuntime{rt: rt, caps: caps}
}

// SetHostInstantiator plugs in the host-imports layer (P1-4). Must be
// called before LoadModule / Instantiate.
func (r *WASMRuntime) SetHostInstantiator(h HostInstantiator) {
	r.hostInst = h
}

// CapabilityTable returns the runtime's capability table.
func (r *WASMRuntime) CapabilityTable() *CapabilityTable { return r.caps }

// Runtime exposes the underlying wazero runtime for callers that
// need to register host modules directly (P1-4 host imports do this).
func (r *WASMRuntime) Runtime() wazero.Runtime { return r.rt }

// LoadModule compiles WASM bytes and validates that all mandatory entry
// points are exported (spec §4). The returned CompiledModule can be
// instantiated multiple times (instance pool).
//
// Returns ErrInvalidEntryPoints if any mandatory export is missing.
// Returns the underlying wazero error if compilation fails.
func (r *WASMRuntime) LoadModule(ctx context.Context, name, version string, src []byte) (*CompiledModule, error) {
	if len(src) == 0 {
		return nil, errors.New("wasm source is empty")
	}
	compiled, err := r.rt.CompileModule(ctx, src)
	if err != nil {
		return nil, fmt.Errorf("failed to compile wasm module for plugin %q: %w", name, err)
	}
	if err := validateEntryPoints(compiled); err != nil {
		compiled.Close(ctx)
		return nil, err
	}
	return &CompiledModule{name: name, version: version, src: compiled}, nil
}

// validateEntryPoints checks that all mandatory entry points are exported
// and warns (via log, future) about optional ones that are missing.
func validateEntryPoints(m wazero.CompiledModule) error {
	exports := m.ExportedFunctions()
	if exports == nil {
		return ErrInvalidEntryPoints{Missing: []string{
			EntryPointWasmInit, EntryPointOnHook, EntryPointOnRequest,
		}}
	}
	missing := []string{}
	for _, ep := range []string{
		EntryPointWasmInit,
		EntryPointOnHook,
		EntryPointOnRequest,
	} {
		if _, ok := exports[ep]; !ok {
			missing = append(missing, ep)
		}
	}
	if len(missing) > 0 {
		return ErrInvalidEntryPoints{Missing: missing}
	}
	return nil
}

// Instantiate creates a fresh instance of the compiled module, issues
// a capability token, calls wasm_init(token), and returns the
// resulting Instance ready for hook/request dispatch.
//
// The token's capability set is the manifest-declared capabilities
// (post-validation). The token's expiry is the instance lifetime —
// callers should call Instance.Destroy() to revoke.
func (r *WASMRuntime) Instantiate(
	ctx context.Context,
	compiled *CompiledModule,
	pluginName string,
	trustTier int,
	caps []string,
	callTimeout time.Duration,
) (*Instance, error) {
	if compiled == nil {
		return nil, errors.New("compiled module is nil")
	}

	token, err := r.caps.Issue(pluginName, trustTier, caps, time.Time{})
	if err != nil {
		return nil, fmt.Errorf("failed to issue capability token: %w", err)
	}

	// Build the host module (host imports) for this instance. The host
	// instantiator (P1-4) is responsible for registering all host
	// functions; we just provide the token + plugin name.
	moduleCfg := wazero.NewModuleConfig()
	if r.hostInst != nil {
		// The host instantiator builds a host module named "nexus:host"
		// and registers all imports the plugin declares. We then pass
		// the same config to InstantiateModule.
		builder := r.rt.NewHostModuleBuilder("nexus:host")
		if err := r.hostInst.Instantiate(ctx, builder, token, pluginName); err != nil {
			r.caps.Revoke(token)
			r.caps.Delete(token)
			return nil, fmt.Errorf("failed to register host imports: %w", err)
		}
		// Instantiate the host module so its functions are visible
		// to the plugin module's imports.
		if _, err := builder.Instantiate(ctx); err != nil {
			r.caps.Revoke(token)
			r.caps.Delete(token)
			return nil, fmt.Errorf("failed to instantiate host module: %w", err)
		}
	}

	mod, err := r.rt.InstantiateModule(ctx, compiled.src, moduleCfg)
	if err != nil {
		r.caps.Revoke(token)
		r.caps.Delete(token)
		return nil, fmt.Errorf("failed to instantiate wasm module for plugin %q: %w", pluginName, err)
	}

	// Call wasm_init(token) — spec §4: "实例初始化，传入 capability_token_root"
	initFn := mod.ExportedFunction(EntryPointWasmInit)
	if initFn == nil {
		// Should be caught by validateEntryPoints but defensive.
		mod.Close(ctx)
		r.caps.Revoke(token)
		r.caps.Delete(token)
		return nil, ErrInvalidEntryPoints{Missing: []string{EntryPointWasmInit}}
	}

	initCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	results, err := initFn.Call(initCtx, uint64(token))
	if err != nil {
		mod.Close(ctx)
		r.caps.Revoke(token)
		r.caps.Delete(token)
		return nil, fmt.Errorf("wasm_init returned error: %w", err)
	}
	if len(results) == 0 || results[0] != 0 {
		// spec §4: 0=OK, 1=Err, >=100 custom. Non-zero init means the
		// plugin refused to initialize — we treat it as a load failure.
		mod.Close(ctx)
		r.caps.Revoke(token)
		r.caps.Delete(token)
		return nil, fmt.Errorf("wasm_init returned non-zero status %d", statusFromResults(results))
	}

	inst := &Instance{
		Module:       mod,
		Token:        token,
		PluginName:   pluginName,
		TrustTier:    trustTier,
		callTimeout:  callTimeout,
		runtime:       r,
	}
	if r.hostInst != nil {
		r.hostInst.OnModuleInstantiated(mod, token)
	}
	return inst, nil
}

func statusFromResults(results []uint64) uint64 {
	if len(results) == 0 {
		return 0
	}
	return results[0]
}

// Close releases all runtime resources. Caller should ensure all
// instances are destroyed first.
func (r *WASMRuntime) Close(ctx context.Context) error {
	var err error
	if r.rt != nil {
		if cerr := r.rt.Close(ctx); cerr != nil {
			err = fmt.Errorf("wasm runtime close: %w", cerr)
		}
	}
	return err
}

// Instance is a live WASM module instance. Borrowed from the pool by
// a single request, used, then returned. Each Instance has its own
// capability token; destroying the instance revokes the token.
type Instance struct {
	Module      api.Module
	Token       CapabilityToken
	PluginName  string
	TrustTier   int
	callTimeout time.Duration
	runtime     *WASMRuntime
}

// CallEntry invokes an entry-point function on this instance with the
// given arguments. Used by hook/request/event/task dispatchers.
// The first argument is always the capability token (spec §4).
func (in *Instance) CallEntry(ctx context.Context, name string, args ...uint64) (uint64, error) {
	fn := in.Module.ExportedFunction(name)
	if fn == nil {
		return 0, fmt.Errorf("entry point %q not exported by plugin %q", name, in.PluginName)
	}
	// Prepend the token (spec §4: every entry's first param is the token).
	fullArgs := make([]uint64, 0, len(args)+1)
	fullArgs = append(fullArgs, uint64(in.Token))
	fullArgs = append(fullArgs, args...)
	callCtx, cancel := context.WithTimeout(ctx, in.callTimeout)
	defer cancel()
	results, err := fn.Call(callCtx, fullArgs...)
	if err != nil {
		return 0, fmt.Errorf("wasm call %q failed: %w", name, err)
	}
	if len(results) == 0 {
		return 0, nil
	}
	return results[0], nil
}

// Destroy closes the WASM module and revokes the capability token.
// After Destroy the Instance is no longer usable.
func (in *Instance) Destroy(ctx context.Context) {
	if in.Module != nil {
		if in.runtime != nil && in.runtime.hostInst != nil {
			in.runtime.hostInst.OnModuleClosed(in.Module)
		}
		in.Module.Close(ctx)
	}
	if in.runtime != nil && in.runtime.caps != nil {
		in.runtime.caps.Revoke(in.Token)
		in.runtime.caps.Delete(in.Token)
	}
	in.Module = nil
}

// ErrInvalidEntryPoints is returned when a WASM module is missing
// one or more mandatory entry points (spec §4).
type ErrInvalidEntryPoints struct {
	Missing []string
}

func (e ErrInvalidEntryPoints) Error() string {
	return fmt.Sprintf("wasm module missing mandatory entry points: %v", e.Missing)
}

// IsErrInvalidEntryPoints reports whether err is an entry-point validation failure.
func IsErrInvalidEntryPoints(err error) bool {
	var e ErrInvalidEntryPoints
	return errors.As(err, &e)
}

// Compile-time assertion that we expect readers of these helpers to be
// using io.Reader for source bytes. This keeps the door open for
// stream-based module loading (e.g. directly from OCI blobs) without
// changing the API.
var _ = io.Reader(nil)
