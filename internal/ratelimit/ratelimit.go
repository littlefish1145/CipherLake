package ratelimit

import (
	"context"
	"errors"
	"hash/fnv"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrRateLimited    = errors.New("rate limited")
	ErrCircuitOpen    = errors.New("circuit breaker is open")
	ErrBandwidthLimit = errors.New("bandwidth limit exceeded")
)

const slidingWindowShards = 64

type slidingWindowShard struct {
	mu       sync.Mutex
	windows  map[string]*windowState
}

type SlidingWindowCounter struct {
	shards    [slidingWindowShards]*slidingWindowShard
	duration  time.Duration
	precision time.Duration
	limit     int64
}

type windowState struct {
	buckets    []int64
	bucketTime int64
	count      int64
}

func NewSlidingWindowCounter(duration time.Duration, precision time.Duration, limit int64) *SlidingWindowCounter {
	sw := &SlidingWindowCounter{
		duration:  duration,
		precision: precision,
		limit:     limit,
	}
	for i := 0; i < slidingWindowShards; i++ {
		sw.shards[i] = &slidingWindowShard{
			windows: make(map[string]*windowState),
		}
	}
	return sw
}

func (s *SlidingWindowCounter) shard(key string) *slidingWindowShard {
	h := fnv.New32a()
	h.Write([]byte(key))
	return s.shards[int(h.Sum32())%slidingWindowShards]
}

func (s *SlidingWindowCounter) Allow(key string) bool {
	shard := s.shard(key)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	now := time.Now()
	nowNano := now.UnixNano()
	bucketIdx := int((nowNano % s.duration.Nanoseconds()) / s.precision.Nanoseconds())
	currentBucket := nowNano / s.precision.Nanoseconds()

	ws, exists := shard.windows[key]
	if !exists || currentBucket-ws.bucketTime > int64(s.duration/s.precision) {
		numBuckets := int(s.duration / s.precision)
		if numBuckets == 0 {
			numBuckets = 1
		}
		ws = &windowState{
			buckets:    make([]int64, numBuckets),
			bucketTime: currentBucket - int64(numBuckets),
			count:      0,
		}
		shard.windows[key] = ws
	}

	for ws.bucketTime < currentBucket {
		ws.bucketTime++
		idx := int(ws.bucketTime % int64(len(ws.buckets)))
		ws.count -= ws.buckets[idx]
		if ws.count < 0 {
			ws.count = 0
		}
		ws.buckets[idx] = 0
	}

	if ws.count >= s.limit {
		return false
	}

	ws.buckets[bucketIdx%len(ws.buckets)]++
	ws.count++
	return true
}

func (s *SlidingWindowCounter) Count(key string) int64 {
	shard := s.shard(key)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	ws, exists := shard.windows[key]
	if !exists {
		return 0
	}
	return ws.count
}

func (s *SlidingWindowCounter) Reset(key string) {
	shard := s.shard(key)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	delete(shard.windows, key)
}

type TokenBucketLimiter struct {
	mu           sync.RWMutex
	buckets      map[string]*tokenBucket
	defaultLimit int
	defaultBurst int
}

type tokenBucket struct {
	tokens    float64
	lastCheck time.Time
	limit     int
	burst     int
	mu        sync.Mutex
}

func NewTokenBucketLimiter(defaultLimit, defaultBurst int) *TokenBucketLimiter {
	return &TokenBucketLimiter{
		buckets:      make(map[string]*tokenBucket),
		defaultLimit: defaultLimit,
		defaultBurst: defaultBurst,
	}
}

func (l *TokenBucketLimiter) Allow(ctx context.Context, key string) error {
	return l.AllowN(ctx, key, 1)
}

func (l *TokenBucketLimiter) AllowN(ctx context.Context, key string, n int) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	l.mu.RLock()
	bucket, exists := l.buckets[key]
	l.mu.RUnlock()

	if !exists {
		l.mu.Lock()
		bucket, exists = l.buckets[key]
		if !exists {
			bucket = &tokenBucket{
				tokens:    float64(l.defaultBurst),
				lastCheck: time.Now(),
				limit:     l.defaultLimit,
				burst:     l.defaultBurst,
			}
			l.buckets[key] = bucket
		}
		l.mu.Unlock()
	}

	bucket.mu.Lock()
	defer bucket.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(bucket.lastCheck).Seconds()
	bucket.lastCheck = now

	bucket.tokens += elapsed * float64(bucket.limit)
	if bucket.tokens > float64(bucket.burst) {
		bucket.tokens = float64(bucket.burst)
	}

	if bucket.tokens >= float64(n) {
		bucket.tokens -= float64(n)
		return nil
	}

	return ErrRateLimited
}

func (l *TokenBucketLimiter) GetLimit(key string) int {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if bucket, exists := l.buckets[key]; exists {
		return bucket.limit
	}
	return l.defaultLimit
}

func (l *TokenBucketLimiter) SetLimit(key string, limit int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	bucket, exists := l.buckets[key]
	if !exists {
		bucket = &tokenBucket{
			tokens:    float64(l.defaultBurst),
			lastCheck: time.Now(),
			limit:     limit,
			burst:     l.defaultBurst,
		}
		l.buckets[key] = bucket
	} else {
		bucket.limit = limit
	}
}

func (l *TokenBucketLimiter) RemoveBucket(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, key)
}

type BandwidthLimiter struct {
	mu          sync.Mutex
	limits      map[string]*bwState
	defaultBPS  int64
	burstBytes  int64
}

type bwState struct {
	tokens     float64
	lastCheck  time.Time
	bps        int64
	burst      int64
	mu         sync.Mutex
}

func NewBandwidthLimiter(defaultBPS, burstBytes int64) *BandwidthLimiter {
	return &BandwidthLimiter{
		limits:     make(map[string]*bwState),
		defaultBPS: defaultBPS,
		burstBytes: burstBytes,
	}
}

func (b *BandwidthLimiter) Allow(key string, bytes int64) bool {
	b.mu.Lock()
	state, exists := b.limits[key]
	if !exists {
		state = &bwState{
			tokens:    float64(b.burstBytes),
			lastCheck: time.Now(),
			bps:       b.defaultBPS,
			burst:     b.burstBytes,
		}
		b.limits[key] = state
	}
	b.mu.Unlock()

	state.mu.Lock()
	defer state.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(state.lastCheck).Seconds()
	state.lastCheck = now

	state.tokens += elapsed * float64(state.bps)
	if state.tokens > float64(state.burst) {
		state.tokens = float64(state.burst)
	}

	if state.tokens >= float64(bytes) {
		state.tokens -= float64(bytes)
		return true
	}

	return false
}

func (b *BandwidthLimiter) Wait(key string, bytes int64) {
	for !b.Allow(key, bytes) {
		time.Sleep(10 * time.Millisecond)
	}
}

func (b *BandwidthLimiter) SetLimit(key string, bps int64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	state, exists := b.limits[key]
	if !exists {
		state = &bwState{
			tokens:    float64(b.burstBytes),
			lastCheck: time.Now(),
			bps:       bps,
			burst:     b.burstBytes,
		}
		b.limits[key] = state
	} else {
		state.bps = bps
	}
}

type MultiLevelLimiter struct {
	globalLimiter   *SlidingWindowCounter
	ipLimiter       *SlidingWindowCounter
	userLimiter     *TokenBucketLimiter
	bucketLimiter   *TokenBucketLimiter
	apiLimiters     map[string]*TokenBucketLimiter
	bwLimiter       *BandwidthLimiter
	whitelist       map[string]bool
	stats           *LimiterStats
	mu              sync.RWMutex
	config          *MultiLevelConfig
}

type MultiLevelConfig struct {
	GlobalRPS          int64
	GlobalBurst        int
	IPRPS              int64
	IPBurst            int
	UserRPS            int
	UserBurst          int
	BucketRPS          int
	BucketBurst        int
	UploadBytesPerSec  int64
	UploadBurstBytes   int64
	APILimits          map[string]APILimit
	Whitelist          []string
	WindowDuration     time.Duration
	WindowPrecision    time.Duration
}

type APILimit struct {
	RPS   int
	Burst int
}

type LimiterStats struct {
	GlobalAllowed   int64
	GlobalRejected  int64
	IPRejected      int64
	UserRejected    int64
	BucketRejected  int64
	APIRejected     int64
	BWRejected      int64
	TotalRequests   int64
}

func NewMultiLevelLimiter(cfg *MultiLevelConfig) *MultiLevelLimiter {
	if cfg.WindowDuration == 0 {
		cfg.WindowDuration = time.Second
	}
	if cfg.WindowPrecision == 0 {
		cfg.WindowPrecision = 100 * time.Millisecond
	}
	if cfg.UploadBurstBytes == 0 {
		cfg.UploadBurstBytes = cfg.UploadBytesPerSec
	}

	ml := &MultiLevelLimiter{
		globalLimiter: NewSlidingWindowCounter(cfg.WindowDuration, cfg.WindowPrecision, cfg.GlobalRPS),
		ipLimiter:     NewSlidingWindowCounter(cfg.WindowDuration, cfg.WindowPrecision, cfg.IPRPS),
		userLimiter:   NewTokenBucketLimiter(cfg.UserRPS, cfg.UserBurst),
		bucketLimiter: NewTokenBucketLimiter(cfg.BucketRPS, cfg.BucketBurst),
		apiLimiters:   make(map[string]*TokenBucketLimiter),
		bwLimiter:     NewBandwidthLimiter(cfg.UploadBytesPerSec, cfg.UploadBurstBytes),
		whitelist:     make(map[string]bool),
		stats:         &LimiterStats{},
		config:        cfg,
	}

	for api, limit := range cfg.APILimits {
		ml.apiLimiters[api] = NewTokenBucketLimiter(limit.RPS, limit.Burst)
	}

	for _, ip := range cfg.Whitelist {
		ml.whitelist[ip] = true
	}

	return ml
}

type RateLimitResult struct {
	Allowed   bool
	Key       string
	LimitType string
	RetryAfter int
}

func (m *MultiLevelLimiter) Allow(ctx context.Context, ip, userID, bucket, apiMethod string, contentLength int64) *RateLimitResult {
	atomic.AddInt64(&m.stats.TotalRequests, 1)

	if m.whitelist[ip] {
		atomic.AddInt64(&m.stats.GlobalAllowed, 1)
		return &RateLimitResult{Allowed: true, Key: ip, LimitType: "whitelist"}
	}

	if !m.globalLimiter.Allow("global") {
		atomic.AddInt64(&m.stats.GlobalRejected, 1)
		return &RateLimitResult{Allowed: false, Key: "global", LimitType: "global", RetryAfter: 1}
	}

	if !m.ipLimiter.Allow("ip:" + ip) {
		atomic.AddInt64(&m.stats.IPRejected, 1)
		return &RateLimitResult{Allowed: false, Key: ip, LimitType: "ip", RetryAfter: 1}
	}

	if userID != "" && userID != "anonymous" {
		if err := m.userLimiter.Allow(ctx, "user:"+userID); err != nil {
			atomic.AddInt64(&m.stats.UserRejected, 1)
			return &RateLimitResult{Allowed: false, Key: userID, LimitType: "user", RetryAfter: 1}
		}
	}

	if bucket != "" {
		if err := m.bucketLimiter.Allow(ctx, "bucket:"+bucket); err != nil {
			atomic.AddInt64(&m.stats.BucketRejected, 1)
			return &RateLimitResult{Allowed: false, Key: bucket, LimitType: "bucket", RetryAfter: 1}
		}
	}

	if apiMethod != "" {
		m.mu.RLock()
		limiter, exists := m.apiLimiters[apiMethod]
		m.mu.RUnlock()

		if exists {
			if err := limiter.Allow(ctx, "api:"+apiMethod+":"+userID); err != nil {
				atomic.AddInt64(&m.stats.APIRejected, 1)
				return &RateLimitResult{Allowed: false, Key: apiMethod, LimitType: "api", RetryAfter: 1}
			}
		}
	}

	if contentLength > 0 && m.config.UploadBytesPerSec > 0 && (apiMethod == "PUT" || apiMethod == "POST") {
		if !m.bwLimiter.Allow("upload:"+userID, contentLength) {
			atomic.AddInt64(&m.stats.BWRejected, 1)
			retryAfter := int(contentLength/m.config.UploadBytesPerSec) + 1
			return &RateLimitResult{Allowed: false, Key: userID, LimitType: "bandwidth", RetryAfter: retryAfter}
		}
	}

	atomic.AddInt64(&m.stats.GlobalAllowed, 1)
	return &RateLimitResult{Allowed: true, Key: "", LimitType: ""}
}

func (m *MultiLevelLimiter) GetStats() *LimiterStats {
	return &LimiterStats{
		GlobalAllowed:  atomic.LoadInt64(&m.stats.GlobalAllowed),
		GlobalRejected: atomic.LoadInt64(&m.stats.GlobalRejected),
		IPRejected:     atomic.LoadInt64(&m.stats.IPRejected),
		UserRejected:   atomic.LoadInt64(&m.stats.UserRejected),
		BucketRejected: atomic.LoadInt64(&m.stats.BucketRejected),
		APIRejected:    atomic.LoadInt64(&m.stats.APIRejected),
		BWRejected:     atomic.LoadInt64(&m.stats.BWRejected),
		TotalRequests:  atomic.LoadInt64(&m.stats.TotalRequests),
	}
}

func (m *MultiLevelLimiter) SetUserLimit(userID string, rps, burst int) {
	m.userLimiter.mu.Lock()
	defer m.userLimiter.mu.Unlock()

	key := "user:" + userID
	bucket, exists := m.userLimiter.buckets[key]
	if exists {
		bucket.limit = rps
		bucket.burst = burst
		if bucket.tokens > float64(burst) {
			bucket.tokens = float64(burst)
		}
	} else {
		m.userLimiter.buckets[key] = &tokenBucket{
			tokens:    float64(burst),
			lastCheck: time.Now(),
			limit:     rps,
			burst:     burst,
		}
	}
}

func (m *MultiLevelLimiter) SetBucketLimit(bucket string, rps, burst int) {
	m.bucketLimiter.SetLimit("bucket:"+bucket, rps)
}

func (m *MultiLevelLimiter) SetAPILimit(api string, rps, burst int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if limiter, exists := m.apiLimiters[api]; exists {
		limiter.SetLimit("api:"+api, rps)
	} else {
		m.apiLimiters[api] = NewTokenBucketLimiter(rps, burst)
	}
}

func (m *MultiLevelLimiter) AddWhitelist(ip string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.whitelist[ip] = true
}

func (m *MultiLevelLimiter) RemoveWhitelist(ip string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.whitelist, ip)
}

func (m *MultiLevelLimiter) CleanupStaleEntries(maxAge time.Duration) {
	m.userLimiter.RemoveBucket("")
	m.bucketLimiter.RemoveBucket("")
}

type AdaptiveLimiter struct {
	limiter        *TokenBucketLimiter
	circuitBreaker *CircuitBreaker
	stats          *AdaptiveStats
	mu             sync.RWMutex
}

type AdaptiveStats struct {
	TotalRequests int64
	Allowed       int64
	Rejected      int64
	CircuitOpen   int64
}

func NewAdaptiveLimiter(defaultLimit, defaultBurst int) *AdaptiveLimiter {
	return &AdaptiveLimiter{
		limiter:        NewTokenBucketLimiter(defaultLimit, defaultBurst),
		circuitBreaker: NewCircuitBreaker(),
		stats:          &AdaptiveStats{},
	}
}

func (a *AdaptiveLimiter) Allow(ctx context.Context, key string) error {
	atomic.AddInt64(&a.stats.TotalRequests, 1)

	if _, err := a.circuitBreaker.Allow("default"); err != nil {
		atomic.AddInt64(&a.stats.CircuitOpen, 1)
		return err
	}

	if err := a.limiter.Allow(ctx, key); err != nil {
		atomic.AddInt64(&a.stats.Rejected, 1)
		return err
	}

	atomic.AddInt64(&a.stats.Allowed, 1)
	return nil
}

func (a *AdaptiveLimiter) GetStats() *AdaptiveStats {
	return &AdaptiveStats{
		TotalRequests: atomic.LoadInt64(&a.stats.TotalRequests),
		Allowed:       atomic.LoadInt64(&a.stats.Allowed),
		Rejected:      atomic.LoadInt64(&a.stats.Rejected),
		CircuitOpen:   atomic.LoadInt64(&a.stats.CircuitOpen),
	}
}

func (a *AdaptiveLimiter) SetLimit(key string, limit int) {
	a.limiter.SetLimit(key, limit)
}

type CircuitBreaker struct {
	mu       sync.RWMutex
	breakers map[string]*circuitBreakerInternal
	configs  map[string]*CircuitBreakerConfig
}

type circuitBreakerInternal struct {
	mu              sync.Mutex
	name            string
	maxRequests     uint32
	interval        time.Duration
	timeout         time.Duration
	errorThreshold  float64
	successThreshold uint32
	state           CircuitState
	failures        int64
	successes       int64
	requests        int64
	lastStateChange time.Time
	halfOpenAllowed uint32
}

type CircuitBreakerConfig struct {
	Name            string
	MaxRequests     uint32
	Interval        time.Duration
	Timeout         time.Duration
	ErrorThreshold  float64
	SuccessThreshold uint32
}

type CircuitState int

const (
	StateClosed   CircuitState = iota
	StateOpen
	StateHalfOpen
)

func NewCircuitBreaker() *CircuitBreaker {
	return &CircuitBreaker{
		breakers: make(map[string]*circuitBreakerInternal),
		configs:  make(map[string]*CircuitBreakerConfig),
	}
}

func (cb *CircuitBreaker) Register(name string, cfg *CircuitBreakerConfig) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.configs[name] = cfg
	cb.breakers[name] = &circuitBreakerInternal{
		name:            cfg.Name,
		maxRequests:     cfg.MaxRequests,
		interval:        cfg.Interval,
		timeout:         cfg.Timeout,
		errorThreshold:  cfg.ErrorThreshold,
		successThreshold: cfg.SuccessThreshold,
		state:           StateClosed,
		lastStateChange: time.Now(),
		halfOpenAllowed: cfg.MaxRequests,
	}
}

func (cb *CircuitBreaker) Allow(name string) (bool, error) {
	cb.mu.RLock()
	breaker, exists := cb.breakers[name]
	cb.mu.RUnlock()

	if !exists {
		return true, nil
	}

	breaker.mu.Lock()
	defer breaker.mu.Unlock()

	switch breaker.state {
	case StateOpen:
		if time.Since(breaker.lastStateChange) > breaker.timeout {
			breaker.state = StateHalfOpen
			breaker.halfOpenAllowed = breaker.maxRequests
			breaker.lastStateChange = time.Now()
			breaker.requests = 0
			breaker.failures = 0
			breaker.successes = 0
		} else {
			return false, ErrCircuitOpen
		}
		fallthrough
	case StateHalfOpen:
		if breaker.halfOpenAllowed == 0 {
			return false, ErrCircuitOpen
		}
		breaker.halfOpenAllowed--
	}

	breaker.requests++
	return true, nil
}

func (cb *CircuitBreaker) RecordSuccess(name string) {
	cb.mu.RLock()
	breaker, exists := cb.breakers[name]
	cb.mu.RUnlock()

	if !exists {
		return
	}

	breaker.mu.Lock()
	defer breaker.mu.Unlock()

	breaker.successes++

	if breaker.state == StateHalfOpen && breaker.successes >= int64(breaker.successThreshold) {
		breaker.state = StateClosed
		breaker.lastStateChange = time.Now()
		breaker.failures = 0
		breaker.requests = 0
	}
}

func (cb *CircuitBreaker) RecordFailure(name string) {
	cb.mu.RLock()
	breaker, exists := cb.breakers[name]
	cb.mu.RUnlock()

	if !exists {
		return
	}

	breaker.mu.Lock()
	defer breaker.mu.Unlock()

	breaker.failures++

	if breaker.state == StateHalfOpen {
		breaker.state = StateOpen
		breaker.lastStateChange = time.Now()
		return
	}

	if breaker.requests > 0 {
		failureRatio := float64(breaker.failures) / float64(breaker.requests)
		if breaker.requests >= int64(breaker.successThreshold) && failureRatio >= breaker.errorThreshold {
			breaker.state = StateOpen
			breaker.lastStateChange = time.Now()
		}
	}
}

func (cb *CircuitBreaker) GetState(name string) CircuitState {
	cb.mu.RLock()
	breaker, exists := cb.breakers[name]
	cb.mu.RUnlock()

	if !exists {
		return StateClosed
	}

	breaker.mu.Lock()
	defer breaker.mu.Unlock()

	if breaker.state == StateOpen && time.Since(breaker.lastStateChange) > breaker.timeout {
		return StateHalfOpen
	}

	return breaker.state
}

type ConcurrencyLimiter struct {
	mu       sync.RWMutex
	limits   map[string]int
	semas    map[string]chan struct{}
	maxTotal int
}

func NewConcurrencyLimiter(maxTotal int) *ConcurrencyLimiter {
	return &ConcurrencyLimiter{
		limits:   make(map[string]int),
		semas:    make(map[string]chan struct{}),
		maxTotal: maxTotal,
	}
}

func (c *ConcurrencyLimiter) getOrCreateSemaphore(key string, limit int) chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()

	if sema, exists := c.semas[key]; exists {
		return sema
	}

	sema := make(chan struct{}, limit)
	// Fill all slots
	for i := 0; i < limit; i++ {
		sema <- struct{}{}
	}
	c.semas[key] = sema
	return sema
}

func (c *ConcurrencyLimiter) Acquire(ctx context.Context, key string) (release func(), err error) {
	c.mu.RLock()
	limit, exists := c.limits[key]
	c.mu.RUnlock()

	if !exists {
		limit = c.maxTotal
	}

	sema := c.getOrCreateSemaphore(key, limit)

	select {
	case <-sema:
		return func() {
			sema <- struct{}{}
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *ConcurrencyLimiter) SetLimit(key string, limit int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.limits[key] = limit

	// Rebuild semaphore with new limit
	if sema, exists := c.semas[key]; exists {
		// Drain the old semaphore
		close(sema)
	}
	newSema := make(chan struct{}, limit)
	for i := 0; i < limit; i++ {
		newSema <- struct{}{}
	}
	c.semas[key] = newSema
}

func (c *ConcurrencyLimiter) GetCount(key string) int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if sema, exists := c.semas[key]; exists {
		return int64(cap(sema) - len(sema))
	}
	return 0
}

type RequestKey struct {
	UserID    string
	APIMethod string
	Bucket    string
}

func (rk *RequestKey) String() string {
	return rk.UserID + ":" + rk.APIMethod + ":" + rk.Bucket
}

type PerUserRateLimiter struct {
	limiter      *TokenBucketLimiter
	defaultLimit int
	defaultBurst int
	mu           sync.RWMutex
	userLimits   map[string]*UserLimit
}

type UserLimit struct {
	Limit     int
	Burst     int
	ExpiresAt time.Time
}

func NewPerUserRateLimiter(defaultLimit, defaultBurst int) *PerUserRateLimiter {
	return &PerUserRateLimiter{
		limiter:      NewTokenBucketLimiter(defaultLimit, defaultBurst),
		defaultLimit: defaultLimit,
		defaultBurst: defaultBurst,
		userLimits:   make(map[string]*UserLimit),
	}
}

func (p *PerUserRateLimiter) Allow(ctx context.Context, key *RequestKey) error {
	p.mu.RLock()
	userLimit, exists := p.userLimits[key.UserID]
	if exists && userLimit.ExpiresAt.After(time.Now()) {
		p.limiter.SetLimit(key.String(), userLimit.Limit)
	} else {
		p.limiter.SetLimit(key.String(), p.defaultLimit)
	}
	p.mu.RUnlock()

	return p.limiter.Allow(ctx, key.String())
}

func (p *PerUserRateLimiter) SetUserLimit(userID string, limit, burst int, duration time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.userLimits[userID] = &UserLimit{
		Limit:     limit,
		Burst:     burst,
		ExpiresAt: time.Now().Add(duration),
	}
}

func (p *PerUserRateLimiter) RemoveUserLimit(userID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.userLimits, userID)
}

type APIType string

const (
	APIPutObject    APIType = "PUT"
	APIGetObject    APIType = "GET"
	APIDeleteObject APIType = "DELETE"
	APIListObjects  APIType = "LIST"
	APIHeadObject   APIType = "HEAD"
	APICreateBucket APIType = "CREATE_BUCKET"
)

type DefaultAPILimits map[APIType]struct {
	Limit int
	Burst int
}

func GetDefaultLimits() DefaultAPILimits {
	return DefaultAPILimits{
		APIPutObject:    {Limit: 100, Burst: 20},
		APIGetObject:    {Limit: 1000, Burst: 100},
		APIDeleteObject: {Limit: 500, Burst: 50},
		APIListObjects:  {Limit: 100, Burst: 20},
		APIHeadObject:   {Limit: 2000, Burst: 200},
		APICreateBucket: {Limit: 10, Burst: 5},
	}
}
