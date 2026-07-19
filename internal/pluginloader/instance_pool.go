package pluginloader

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
)

// InstancePool manages a pool of WASM instances for a single plugin
// (spec §3.13). The pool size caps concurrent in-flight requests per
// plugin — a Borrow beyond the cap blocks (with timeout) until an
// instance is returned.
//
// Pool semantics:
//   - Borrow(): returns a healthy instance, creating a new one if the
//     pool is empty. Blocks up to borrowTimeout when at capacity.
//   - Return(): returns the instance to the free list for reuse.
//   - Discard(): called when an instance trapped or is otherwise
//     unhealthy; the instance is destroyed and not returned to the
//     pool. The next Borrow will create a replacement.
//   - DestroyAll(): called on uninstall/reload; drains the pool and
//     revokes every token.
type InstancePool struct {
	mu              sync.Mutex
	runtime         *WASMRuntime
	compiled        *CompiledModule
	pluginName      string
	trustTier       int
	caps            []string
	callTimeout     time.Duration
	borrowTimeout   time.Duration
	maxSize         int
	free            []*Instance
	inUse           int
	closed          bool
}

// PoolConfig configures a single plugin's instance pool.
type PoolConfig struct {
	// MaxSize is the maximum number of concurrent instances. Default
	// 16 (spec §3.13). The effective limit is min(MaxSize, tier-level
	// Concurrent cap, manifest-requested Concurrent) per spec §3.6.
	MaxSize int

	// BorrowTimeout is how long Borrow waits when the pool is
	// exhausted before returning ErrPoolExhausted. Default 5s.
	BorrowTimeout time.Duration

	// CallTimeout is the per-WASM-call timeout passed to each
	// instance's entry-point invocation. Default taken from the
	// loader config (cfg.CallTimeout).
	CallTimeout time.Duration
}

// DefaultPoolConfig returns the spec defaults for a Tier-0 plugin.
// Phase 2 (P2-2) overrides these with the three-layer resource model.
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		MaxSize:       16,
		BorrowTimeout: 5 * time.Second,
		CallTimeout:   1 * time.Second,
	}
}

// NewInstancePool constructs a pool for a single plugin. The pool is
// lazy — no instances are created until Borrow is called.
func NewInstancePool(rt *WASMRuntime, compiled *CompiledModule, pluginName string, trustTier int, caps []string, cfg PoolConfig) *InstancePool {
	if cfg.MaxSize <= 0 {
		cfg = DefaultPoolConfig()
	}
	p := &InstancePool{
		runtime:       rt,
		compiled:      compiled,
		pluginName:    pluginName,
		trustTier:     trustTier,
		caps:          caps,
		callTimeout:   cfg.CallTimeout,
		borrowTimeout: cfg.BorrowTimeout,
		maxSize:       cfg.MaxSize,
		free:          make([]*Instance, 0, cfg.MaxSize),
	}
	return p
}

// Borrow acquires an instance for the duration of a request. The
// caller must call Return (or Discard) when done. Failing to return
// leaks the slot until the pool is closed.
//
// If the pool is empty but under capacity, a new instance is created.
// If the pool is at capacity, Borrow blocks up to BorrowTimeout.
func (p *InstancePool) Borrow(ctx context.Context) (*Instance, error) {
	deadline, hasDeadline := ctx.Deadline()
	if err := p.waitFree(ctx, deadline, hasDeadline); err != nil {
		return nil, err
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrPoolClosed{Plugin: p.pluginName}
	}
	if len(p.free) > 0 {
		inst := p.free[len(p.free)-1]
		p.free = p.free[:len(p.free)-1]
		p.inUse++
		p.mu.Unlock()
		return inst, nil
	}
	if p.inUse >= p.maxSize {
		p.mu.Unlock()
		return nil, ErrPoolExhausted{Plugin: p.pluginName, Max: p.maxSize}
	}
	p.inUse++
	p.mu.Unlock()

	// Create a new instance outside the lock — instantiation can take
	// tens of milliseconds and we don't want to block other borrowers.
	inst, err := p.runtime.Instantiate(ctx, p.compiled, p.pluginName, p.trustTier, p.caps, p.callTimeout)
	if err != nil {
		p.mu.Lock()
		p.inUse--
		p.mu.Unlock()
		return nil, fmt.Errorf("failed to instantiate wasm for plugin %q: %w", p.pluginName, err)
	}
	return inst, nil
}

// waitFree blocks until an instance may be available (caller must
// re-check under lock) or the pool is closed / wait times out.
//
// Implementation note: we use a simple ticker poll instead of sync.Cond.
// sync.Cond does not support context cancellation cleanly, and the
// ticker poll is cheap (1ms) for the typical borrow-wait duration
// (sub-millisecond under low load, 5s max under contention).
func (p *InstancePool) waitFree(ctx context.Context, deadline time.Time, hasDeadline bool) error {
	// Fast path: if there's a free instance or under capacity, skip waiting.
	if p.tryAcquireSlot() {
		return nil
	}
	if p.isClosed() {
		return ErrPoolClosed{Plugin: p.pluginName}
	}

	// Determine the effective wait timeout.
	waitTimeout := p.borrowTimeout
	if hasDeadline {
		remaining := time.Until(deadline)
		if remaining < waitTimeout {
			waitTimeout = remaining
		}
	}
	if waitTimeout <= 0 {
		return ErrPoolExhausted{Plugin: p.pluginName, Max: p.maxSize}
	}

	deadline2 := time.Now().Add(waitTimeout)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if p.isClosed() {
				return ErrPoolClosed{Plugin: p.pluginName}
			}
			if p.tryAcquireSlot() {
				return nil
			}
			if time.Now().After(deadline2) {
				return ErrPoolExhausted{Plugin: p.pluginName, Max: p.maxSize}
			}
		}
	}
}

// tryAcquireSlot returns true if a slot is likely free (caller must
// re-check under lock). This is the fast-path pre-check before the
// poll loop in waitFree.
func (p *InstancePool) tryAcquireSlot() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.free) > 0 || p.inUse < p.maxSize
}

func (p *InstancePool) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// Return returns a borrowed instance to the free list for reuse. If
// the instance has been Destroy'd (Module == nil) it is discarded
// rather than returned. If the pool is closed, the instance is destroyed.
func (p *InstancePool) Return(inst *Instance) {
	if inst == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inUse--
	if p.inUse < 0 {
		p.inUse = 0
	}

	if p.closed || inst.Module == nil {
		// Pool is shutting down or instance is unhealthy — destroy.
		inst.Destroy(context.Background())
		return
	}
	p.free = append(p.free, inst)
}

// Discard drops an unhealthy instance without returning it to the pool.
// The slot is freed so the next Borrow can create a replacement.
func (p *InstancePool) Discard(inst *Instance) {
	if inst == nil {
		return
	}
	p.mu.Lock()
	p.inUse--
	if p.inUse < 0 {
		p.inUse = 0
	}
	p.mu.Unlock()

	inst.Destroy(context.Background())
}

// DestroyAll drains the pool: every free instance is destroyed, every
// in-use instance is marked for destruction on its next Return. The
// capability tokens for all currently-free instances are revoked
// immediately. Used on uninstall/reload (spec §3.5).
func (p *InstancePool) DestroyAll(ctx context.Context) {
	p.mu.Lock()
	p.closed = true
	free := p.free
	p.free = nil
	p.mu.Unlock()

	for _, inst := range free {
		inst.Destroy(ctx)
	}
}

// Stats returns the current pool counters (for metrics).
type PoolStats struct {
	Free  int
	InUse int
	Max   int
}

// Stats returns the current pool counters.
func (p *InstancePool) Stats() PoolStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return PoolStats{Free: len(p.free), InUse: p.inUse, Max: p.maxSize}
}

// ErrPoolExhausted is returned when Borrow cannot acquire an instance
// within BorrowTimeout.
type ErrPoolExhausted struct {
	Plugin string
	Max    int
}

func (e ErrPoolExhausted) Error() string {
	return fmt.Sprintf("plugin %q instance pool exhausted (max=%d)", e.Plugin, e.Max)
}

// ErrPoolClosed is returned when Borrow is called on a destroyed pool.
type ErrPoolClosed struct {
	Plugin string
}

func (e ErrPoolClosed) Error() string {
	return fmt.Sprintf("plugin %q instance pool is closed", e.Plugin)
}

// Compile-time assertion: InstancePool uses wazero types indirectly
// via WASMRuntime, keeping this file decoupled from wazero API drift.
var _ wazero.Runtime = (wazero.Runtime)(nil)
var _ error = ErrPoolExhausted{}
var _ error = errors.New("")
