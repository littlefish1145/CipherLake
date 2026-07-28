package ratelimit

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestSlidingWindowCounter(t *testing.T) {
	counter := NewSlidingWindowCounter(time.Second, 100*time.Millisecond, 5)

	for i := 0; i < 5; i++ {
		if !counter.Allow("test") {
			t.Fatalf("request %d should be allowed", i)
		}
	}

	if counter.Allow("test") {
		t.Fatal("6th request should be rejected")
	}
}

func TestSlidingWindowCounter_DifferentKeys(t *testing.T) {
	counter := NewSlidingWindowCounter(time.Second, 100*time.Millisecond, 3)

	for i := 0; i < 3; i++ {
		if !counter.Allow("key1") {
			t.Fatalf("key1 request %d should be allowed", i)
		}
		if !counter.Allow("key2") {
			t.Fatalf("key2 request %d should be allowed", i)
		}
	}

	if counter.Allow("key1") {
		t.Fatal("key1 4th request should be rejected")
	}
	if counter.Allow("key2") {
		t.Fatal("key2 4th request should be rejected")
	}
}

func TestSlidingWindowCounter_Reset(t *testing.T) {
	counter := NewSlidingWindowCounter(time.Second, 100*time.Millisecond, 2)

	counter.Allow("test")
	counter.Allow("test")

	if counter.Allow("test") {
		t.Fatal("should be rejected after limit")
	}

	counter.Reset("test")

	if !counter.Allow("test") {
		t.Fatal("should be allowed after reset")
	}
}

func TestTokenBucketLimiter(t *testing.T) {
	limiter := NewTokenBucketLimiter(100, 50)
	if limiter.defaultLimit != 100 {
		t.Errorf("expected limit 100, got %d", limiter.defaultLimit)
	}
	if limiter.defaultBurst != 50 {
		t.Errorf("expected burst 50, got %d", limiter.defaultBurst)
	}
}

func TestTokenBucketLimiter_Allow(t *testing.T) {
	limiter := NewTokenBucketLimiter(10, 5)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if err := limiter.Allow(ctx, "user1"); err != nil {
			t.Fatalf("request %d should be allowed", i)
		}
	}

	if err := limiter.Allow(ctx, "user1"); err == nil {
		t.Fatal("6th request should be rejected")
	}
}

func TestTokenBucketLimiter_Allow_DifferentKeys(t *testing.T) {
	limiter := NewTokenBucketLimiter(10, 5)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if err := limiter.Allow(ctx, "user1"); err != nil {
			t.Fatalf("user1 request %d should be allowed", i)
		}
		if err := limiter.Allow(ctx, "user2"); err != nil {
			t.Fatalf("user2 request %d should be allowed", i)
		}
	}
}

func TestTokenBucketLimiter_GetLimit(t *testing.T) {
	limiter := NewTokenBucketLimiter(100, 50)

	if limit := limiter.GetLimit("user1"); limit != 100 {
		t.Errorf("expected limit 100, got %d", limit)
	}

	limiter.SetLimit("user1", 200)
	if limit := limiter.GetLimit("user1"); limit != 200 {
		t.Errorf("expected limit 200, got %d", limit)
	}
}

func TestTokenBucketLimiter_Concurrent(t *testing.T) {
	limiter := NewTokenBucketLimiter(1000, 100)
	ctx := context.Background()

	var wg sync.WaitGroup
	successCount := 0
	var mu sync.Mutex

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if err := limiter.Allow(ctx, "user1"); err == nil {
					mu.Lock()
					successCount++
					mu.Unlock()
				}
			}
		}()
	}

	wg.Wait()
	if successCount > 100 {
		t.Errorf("expected at most 100 successes, got %d", successCount)
	}
}

func TestBandwidthLimiter(t *testing.T) {
	limiter := NewBandwidthLimiter(1000, 5000)

	if !limiter.Allow("user1", 3000) {
		t.Fatal("first request within burst should be allowed")
	}

	if !limiter.Allow("user1", 1000) {
		t.Fatal("second request within burst should be allowed")
	}

	if limiter.Allow("user1", 2000) {
		t.Fatal("third request exceeding burst should be rejected")
	}
}

func TestMultiLevelLimiter(t *testing.T) {
	cfg := &MultiLevelConfig{
		GlobalRPS:         100,
		GlobalBurst:       20,
		IPRPS:             50,
		IPBurst:           10,
		UserRPS:           10,
		UserBurst:         5,
		BucketRPS:         20,
		BucketBurst:       10,
		UploadBytesPerSec: 1024 * 1024,
		UploadBurstBytes:  5 * 1024 * 1024,
		APILimits: map[string]APILimit{
			"PUT": {RPS: 10, Burst: 5},
			"GET": {RPS: 100, Burst: 20},
		},
		Whitelist: []string{"127.0.0.1"},
	}

	ml := NewMultiLevelLimiter(cfg)

	result := ml.Allow(context.Background(), "127.0.0.1", "user1", "bucket1", "GET", 0)
	if !result.Allowed {
		t.Fatal("whitelisted IP should be allowed")
	}
	if result.LimitType != "whitelist" {
		t.Errorf("expected whitelist limit type, got %s", result.LimitType)
	}

	result = ml.Allow(context.Background(), "10.0.0.1", "user2", "bucket1", "GET", 0)
	if !result.Allowed {
		t.Fatal("normal request should be allowed")
	}
}

func TestMultiLevelLimiter_IPRateLimit(t *testing.T) {
	cfg := &MultiLevelConfig{
		GlobalRPS:   10000,
		GlobalBurst: 1000,
		IPRPS:       3,
		IPBurst:     3,
		UserRPS:     1000,
		UserBurst:   500,
		BucketRPS:   1000,
		BucketBurst: 500,
		APILimits:   map[string]APILimit{},
	}

	ml := NewMultiLevelLimiter(cfg)

	for i := 0; i < 3; i++ {
		result := ml.Allow(context.Background(), "10.0.0.1", "user1", "bucket1", "GET", 0)
		if !result.Allowed {
			t.Fatalf("request %d should be allowed", i)
		}
	}

	result := ml.Allow(context.Background(), "10.0.0.1", "user1", "bucket1", "GET", 0)
	if result.Allowed {
		t.Fatal("4th request should be IP rate limited")
	}
	if result.LimitType != "ip" {
		t.Errorf("expected ip limit type, got %s", result.LimitType)
	}
}

func TestMultiLevelLimiter_BandwidthLimit(t *testing.T) {
	cfg := &MultiLevelConfig{
		GlobalRPS:         10000,
		GlobalBurst:       1000,
		IPRPS:             10000,
		IPBurst:           1000,
		UserRPS:           1000,
		UserBurst:         500,
		BucketRPS:         1000,
		BucketBurst:       500,
		UploadBytesPerSec: 100,
		UploadBurstBytes:  500,
		APILimits:         map[string]APILimit{},
	}

	ml := NewMultiLevelLimiter(cfg)

	result := ml.Allow(context.Background(), "10.0.0.1", "user1", "bucket1", "PUT", 400)
	if !result.Allowed {
		t.Fatal("upload within burst should be allowed")
	}

	result = ml.Allow(context.Background(), "10.0.0.1", "user1", "bucket1", "PUT", 200)
	if result.Allowed {
		t.Fatal("upload exceeding burst should be rejected")
	}
	if result.LimitType != "bandwidth" {
		t.Errorf("expected bandwidth limit type, got %s", result.LimitType)
	}
}

func TestMultiLevelLimiter_Stats(t *testing.T) {
	cfg := &MultiLevelConfig{
		GlobalRPS:   10000,
		GlobalBurst: 1000,
		IPRPS:       10000,
		IPBurst:     1000,
		UserRPS:     1000,
		UserBurst:   500,
		BucketRPS:   1000,
		BucketBurst: 500,
		APILimits:   map[string]APILimit{},
	}

	ml := NewMultiLevelLimiter(cfg)

	for i := 0; i < 10; i++ {
		ml.Allow(context.Background(), "10.0.0.1", "user1", "bucket1", "GET", 0)
	}

	stats := ml.GetStats()
	if stats.TotalRequests != 10 {
		t.Errorf("expected 10 total requests, got %d", stats.TotalRequests)
	}
	if stats.GlobalAllowed != 10 {
		t.Errorf("expected 10 allowed, got %d", stats.GlobalAllowed)
	}
}

func TestMultiLevelLimiter_SetUserLimit(t *testing.T) {
	cfg := &MultiLevelConfig{
		GlobalRPS:   10000,
		GlobalBurst: 1000,
		IPRPS:       10000,
		IPBurst:     1000,
		UserRPS:     2,
		UserBurst:   2,
		BucketRPS:   1000,
		BucketBurst: 500,
		APILimits:   map[string]APILimit{},
	}

	ml := NewMultiLevelLimiter(cfg)

	ml.SetUserLimit("user1", 100, 50)

	for i := 0; i < 5; i++ {
		result := ml.Allow(context.Background(), "10.0.0.1", "user1", "bucket1", "GET", 0)
		if !result.Allowed {
			t.Fatalf("request %d should be allowed with higher limit", i)
		}
	}
}

func TestCircuitBreaker(t *testing.T) {
	cb := NewCircuitBreaker()
	if cb == nil {
		t.Fatal("circuit breaker should not be nil")
	}
}

func TestCircuitBreaker_Register(t *testing.T) {
	cb := NewCircuitBreaker()

	cfg := &CircuitBreakerConfig{
		Name:             "test",
		MaxRequests:      3,
		Interval:         10 * time.Second,
		Timeout:          30 * time.Second,
		ErrorThreshold:   0.5,
		SuccessThreshold: 2,
	}

	cb.Register("test", cfg)

	state := cb.GetState("test")
	if state != StateClosed {
		t.Errorf("expected StateClosed, got %d", state)
	}
}

func TestCircuitBreaker_Allow(t *testing.T) {
	cb := NewCircuitBreaker()

	cfg := &CircuitBreakerConfig{
		Name:             "test",
		MaxRequests:      3,
		Interval:         10 * time.Second,
		Timeout:          30 * time.Second,
		ErrorThreshold:   0.5,
		SuccessThreshold: 2,
	}

	cb.Register("test", cfg)

	allowed, err := cb.Allow("test")
	if !allowed || err != nil {
		t.Fatal("should be allowed in closed state")
	}
}

func TestCircuitBreaker_Allow_NotExists(t *testing.T) {
	cb := NewCircuitBreaker()

	allowed, err := cb.Allow("nonexistent")
	if !allowed || err != nil {
		t.Fatal("non-existent breaker should allow")
	}
}

func TestCircuitBreaker_OpenClose(t *testing.T) {
	cb := NewCircuitBreaker()

	cfg := &CircuitBreakerConfig{
		Name:             "test",
		MaxRequests:      1,
		Interval:         10 * time.Second,
		Timeout:          100 * time.Millisecond,
		ErrorThreshold:   0.5,
		SuccessThreshold: 3,
	}

	cb.Register("test", cfg)

	for i := 0; i < 3; i++ {
		cb.Allow("test")
		cb.RecordFailure("test")
	}

	state := cb.GetState("test")
	if state != StateOpen {
		t.Errorf("expected StateOpen after failures, got %d", state)
	}

	_, err := cb.Allow("test")
	if err == nil {
		t.Fatal("should be rejected when circuit is open")
	}

	time.Sleep(150 * time.Millisecond)

	state = cb.GetState("test")
	if state != StateHalfOpen {
		t.Errorf("expected StateHalfOpen after timeout, got %d", state)
	}
}

func TestCircuitBreaker_RecordSuccess(t *testing.T) {
	cb := NewCircuitBreaker()

	cfg := &CircuitBreakerConfig{
		Name:             "test",
		MaxRequests:      1,
		Interval:         10 * time.Second,
		Timeout:          100 * time.Millisecond,
		ErrorThreshold:   0.5,
		SuccessThreshold: 2,
	}

	cb.Register("test", cfg)

	for i := 0; i < 3; i++ {
		cb.Allow("test")
		cb.RecordFailure("test")
	}

	time.Sleep(150 * time.Millisecond)

	cb.Allow("test")
	cb.RecordSuccess("test")
	cb.Allow("test")
	cb.RecordSuccess("test")

	state := cb.GetState("test")
	if state != StateClosed {
		t.Errorf("expected StateClosed after successes, got %d", state)
	}
}

func TestAdaptiveLimiter(t *testing.T) {
	adaptive := NewAdaptiveLimiter(100, 50)
	if adaptive == nil {
		t.Fatal("adaptive limiter should not be nil")
	}
}

func TestAdaptiveLimiter_Allow(t *testing.T) {
	adaptive := NewAdaptiveLimiter(10, 5)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if err := adaptive.Allow(ctx, "user1"); err != nil {
			t.Fatalf("request %d should be allowed", i)
		}
	}

	if err := adaptive.Allow(ctx, "user1"); err == nil {
		t.Fatal("6th request should be rejected")
	}
}

func TestAdaptiveLimiter_GetStats(t *testing.T) {
	adaptive := NewAdaptiveLimiter(10, 5)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		adaptive.Allow(ctx, "user1")
	}

	stats := adaptive.GetStats()
	if stats.TotalRequests < 10 {
		t.Errorf("expected at least 10 total requests, got %d", stats.TotalRequests)
	}
}

func TestAdaptiveLimiter_SetLimit(t *testing.T) {
	adaptive := NewAdaptiveLimiter(10, 5)

	adaptive.SetLimit("user1", 100)

	limit := adaptive.limiter.GetLimit("user1")
	if limit != 100 {
		t.Errorf("expected limit 100, got %d", limit)
	}
}

func TestCircuitState(t *testing.T) {
	if StateClosed != 0 {
		t.Errorf("expected StateClosed = 0, got %d", StateClosed)
	}
	if StateOpen != 1 {
		t.Errorf("expected StateOpen = 1, got %d", StateOpen)
	}
	if StateHalfOpen != 2 {
		t.Errorf("expected StateHalfOpen = 2, got %d", StateHalfOpen)
	}
}

func TestConcurrencyLimiter(t *testing.T) {
	limiter := NewConcurrencyLimiter(5)
	ctx := context.Background()

	var releases []func()
	for i := 0; i < 5; i++ {
		release, err := limiter.Acquire(ctx, "test")
		if err != nil {
			t.Fatalf("acquire %d should succeed", i)
		}
		releases = append(releases, release)
	}

	for _, release := range releases {
		release()
	}

	if count := limiter.GetCount("test"); count != 0 {
		t.Errorf("expected count 0 after release, got %d", count)
	}
}

func BenchmarkSlidingWindowCounter(b *testing.B) {
	counter := NewSlidingWindowCounter(time.Second, 100*time.Millisecond, 10000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		counter.Allow("bench-key")
	}
}

func BenchmarkTokenBucketLimiter_Allow(b *testing.B) {
	limiter := NewTokenBucketLimiter(10000, 1000)
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		limiter.Allow(ctx, "benchmark-user")
	}
}

func BenchmarkMultiLevelLimiter(b *testing.B) {
	cfg := &MultiLevelConfig{
		GlobalRPS:   100000,
		GlobalBurst: 10000,
		IPRPS:       50000,
		IPBurst:     5000,
		UserRPS:     10000,
		UserBurst:   1000,
		BucketRPS:   20000,
		BucketBurst: 2000,
		APILimits: map[string]APILimit{
			"GET": {RPS: 50000, Burst: 5000},
		},
	}
	ml := NewMultiLevelLimiter(cfg)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ml.Allow(context.Background(), "10.0.0.1", "user1", "bucket1", "GET", 0)
	}
}
