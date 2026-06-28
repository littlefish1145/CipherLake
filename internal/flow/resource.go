package flow

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrResourceExhausted = errors.New("resource exhausted")
	ErrShuttingDown      = errors.New("resource manager shutting down")
)

type ResourceManager struct {
	mu           sync.RWMutex
	slots        chan struct{}
	maxWorkers   int32
	active       int32
	queueSize    int32
	maxQueue     int32
	shutdown     atomic.Bool
	tokenBucket  chan struct{}
	bucketSize   int
	bucketRate   time.Duration
	telemetry    *Telemetry
}

type ResourceConfig struct {
	MaxWorkers int
	MaxQueue   int
	RateLimit  int
	BurstSize  int
}

func DefaultResourceConfig() ResourceConfig {
	return ResourceConfig{
		MaxWorkers: 100,
		MaxQueue:   1000,
		RateLimit:  0,
		BurstSize:  0,
	}
}

func NewResourceManager(cfg ResourceConfig, telemetry *Telemetry) *ResourceManager {
	rm := &ResourceManager{
		slots:      make(chan struct{}, cfg.MaxWorkers),
		maxWorkers: int32(cfg.MaxWorkers),
		maxQueue:   int32(cfg.MaxQueue),
		telemetry:  telemetry,
	}

	if cfg.RateLimit > 0 {
		rm.bucketSize = cfg.BurstSize
		if rm.bucketSize <= 0 {
			rm.bucketSize = cfg.MaxWorkers
		}
		rm.tokenBucket = make(chan struct{}, rm.bucketSize)
		for i := 0; i < rm.bucketSize; i++ {
			rm.tokenBucket <- struct{}{}
		}
		interval := time.Second / time.Duration(cfg.RateLimit)
		go rm.refillTokens(interval)
	}

	return rm
}

func (rm *ResourceManager) refillTokens(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		if rm.shutdown.Load() {
			return
		}
		select {
		case rm.tokenBucket <- struct{}{}:
		default:
		}
	}
}

func (rm *ResourceManager) Acquire(ctx context.Context) error {
	if rm.shutdown.Load() {
		return ErrShuttingDown
	}

	if rm.tokenBucket != nil {
		select {
		case <-rm.tokenBucket:
		case <-ctx.Done():
			return ctx.Err()
		default:
			return ErrResourceExhausted
		}
	}

	if q := atomic.LoadInt32(&rm.queueSize); q >= rm.maxQueue {
		if rm.telemetry != nil {
			rm.telemetry.EventBus.Publish(Event{
				ID:       newID(),
				Type:     EventQueueBacklog,
				Time:     time.Now(),
				Metadata: map[string]string{"queue_size": string(rune(q))},
			})
		}
		return ErrResourceExhausted
	}

	atomic.AddInt32(&rm.queueSize, 1)
	defer atomic.AddInt32(&rm.queueSize, -1)

	select {
	case rm.slots <- struct{}{}:
		atomic.AddInt32(&rm.active, 1)
		if rm.telemetry != nil {
			rm.telemetry.EventBus.Publish(Event{
				ID:     newID(),
				Type:   EventWorkerBusy,
				Time:   time.Now(),
				Worker: int(atomic.LoadInt32(&rm.active)),
			})
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (rm *ResourceManager) Release() {
	<-rm.slots
	atomic.AddInt32(&rm.active, -1)
	if rm.telemetry != nil {
		rm.telemetry.EventBus.Publish(Event{
			ID:     newID(),
			Type:   EventWorkerIdle,
			Time:   time.Now(),
			Worker: int(atomic.LoadInt32(&rm.active)),
		})
	}
}

func (rm *ResourceManager) Active() int32  { return atomic.LoadInt32(&rm.active) }
func (rm *ResourceManager) QueueSize() int32 { return atomic.LoadInt32(&rm.queueSize) }
func (rm *ResourceManager) MaxWorkers() int32 { return rm.maxWorkers }

func (rm *ResourceManager) Shutdown() {
	rm.shutdown.Store(true)
}

func (rm *ResourceManager) Stats() ResourceStats {
	return ResourceStats{
		Active:    rm.Active(),
		Queued:    rm.QueueSize(),
		MaxWorkers: rm.MaxWorkers(),
	}
}

type ResourceStats struct {
	Active     int32
	Queued     int32
	MaxWorkers int32
}
