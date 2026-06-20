package tiering

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"nexus/internal/common"
	"nexus/internal/storage"

	"go.uber.org/zap"
)

type TierDecision struct {
	ObjectKey     string
	Bucket        string
	CurrentTier   common.StorageTier
	TargetTier    common.StorageTier
	HotnessScore  float64
	Reason        string
	MigratedAt    time.Time
}

type AccessHistory struct {
	ObjectKey     string
	Bucket        string
	AccessTimes   []time.Time
	AccessTypes   []string
	UserIDs       []string
	LastAccess    time.Time
	TotalAccesses int
	PriorityWeight int
	FixedTier     bool
}

type TierDecisionModel struct {
	mu            sync.RWMutex
	decayHalfLife time.Duration
	featureWeights map[string]float64
}

func NewTierDecisionModel() *TierDecisionModel {
	return &TierDecisionModel{
		decayHalfLife: 7 * 24 * time.Hour,
		featureWeights: map[string]float64{
			"access_frequency": 0.4,
			"recent_access":    0.3,
			"content_type":     0.1,
			"size":            -0.1,
			"priority":         0.1,
		},
	}
}

func (m *TierDecisionModel) CalculateHotnessScore(history *AccessHistory, metadata *common.ObjectMetadata) float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	now := time.Now()
	
	frequencyScore := m.calculateFrequencyScore(history)
	recentScore := m.calculateRecentScore(history, now)
	contentScore := m.calculateContentScore(metadata)
	sizeScore := m.calculateSizeScore(metadata)
	priorityScore := m.calculatePriorityScore(history)
	
	totalScore := 
		frequencyScore * m.featureWeights["access_frequency"] +
		recentScore * m.featureWeights["recent_access"] +
		contentScore * m.featureWeights["content_type"] +
		sizeScore * m.featureWeights["size"] +
		priorityScore * m.featureWeights["priority"]
	
	return math.Max(0, math.Min(100, totalScore))
}

func (m *TierDecisionModel) calculateFrequencyScore(history *AccessHistory) float64 {
	if len(history.AccessTimes) == 0 {
		return 0
	}
	
	decayFactor := math.Log(2) / m.decayHalfLife.Seconds()
	score := 0.0
	now := time.Now()
	
	for _, t := range history.AccessTimes {
		age := now.Sub(t).Seconds()
		decayedWeight := math.Exp(-decayFactor * age)
		score += decayedWeight
	}
	
	return math.Min(100, score/10)
}

func (m *TierDecisionModel) calculateRecentScore(history *AccessHistory, now time.Time) float64 {
	if history.LastAccess.IsZero() {
		return 0
	}
	
	hoursSinceAccess := now.Sub(history.LastAccess).Hours()
	
	if hoursSinceAccess < 1 {
		return 100
	} else if hoursSinceAccess < 24 {
		return 80
	} else if hoursSinceAccess < 168 {
		return 50
	} else if hoursSinceAccess < 720 {
		return 20
	} else {
		return 0
	}
}

func (m *TierDecisionModel) calculateContentScore(metadata *common.ObjectMetadata) float64 {
	if metadata == nil || metadata.ContentType == "" {
		return 50
	}

	contentType := metadata.ContentType
	switch {
	case len(contentType) >= 5 && contentType[:5] == "image":
		return 80
	case len(contentType) >= 5 && contentType[:5] == "video":
		return 60
	case len(contentType) >= 4 && contentType[:4] == "text":
		return 70
	default:
		return 50
	}
}

func (m *TierDecisionModel) calculateSizeScore(metadata *common.ObjectMetadata) float64 {
	if metadata == nil {
		return 50
	}
	
	size := metadata.Size
	
	if size < 1024*1024 {
		return 80
	} else if size < 100*1024*1024 {
		return 60
	} else if size < 1024*1024*1024 {
		return 40
	} else {
		return 20
	}
}

func (m *TierDecisionModel) calculatePriorityScore(history *AccessHistory) float64 {
	if history == nil {
		return 0
	}
	
	return float64(history.PriorityWeight)
}

type TieringManager struct {
	mu           sync.RWMutex
	store        *storage.TieredObjectStore
	model        *TierDecisionModel
	accessHistories map[string]*AccessHistory
	hotspots     map[string]*HotspotDetection
	config       *TieringConfig
	stopCh       chan struct{}
	migrating    map[string]bool
}

type TieringConfig struct {
	Enabled          bool
	Schedule         string
	HotMaxSize       int64
	WarmMaxSize      int64
	ColdMaxSize      int64
	MigrationWorkers int
	CheckInterval    time.Duration
}

type HotspotDetection struct {
	Prefix         string
	Bucket         string
	AccessCount    int
	BaselineAccess int
	DetectedAt     time.Time
	IsHotspot      bool
}

func NewTieringManager(store *storage.TieredObjectStore, config *TieringConfig) *TieringManager {
	if config == nil {
		config = &TieringConfig{
			Enabled:          true,
			MigrationWorkers: 10,
			CheckInterval:    6 * time.Hour,
			HotMaxSize:       32 << 30,
		}
	}
	
	return &TieringManager{
		store:           store,
		model:           NewTierDecisionModel(),
		accessHistories: make(map[string]*AccessHistory),
		hotspots:        make(map[string]*HotspotDetection),
		config:          config,
		stopCh:          make(chan struct{}),
		migrating:       make(map[string]bool),
	}
}

func (m *TieringManager) RecordAccess(ctx context.Context, bucket, key string, accessType string, userID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	historyKey := bucket + "/" + key
	history, exists := m.accessHistories[bucket+"/"+key]
	if !exists {
		history = &AccessHistory{
			ObjectKey: key,
			Bucket:    bucket,
		}
		m.accessHistories[historyKey] = history
	}
	
	now := time.Now()
	history.AccessTimes = append(history.AccessTimes, now)
	history.AccessTypes = append(history.AccessTypes, accessType)
	history.UserIDs = append(history.UserIDs, userID)
	history.LastAccess = now
	history.TotalAccesses++
	
	if len(history.AccessTimes) > 1000 {
		history.AccessTimes = history.AccessTimes[len(history.AccessTimes)-1000:]
		history.AccessTypes = history.AccessTypes[len(history.AccessTypes)-1000:]
		history.UserIDs = history.UserIDs[len(history.UserIDs)-1000:]
	}
	
	m.updateHotspotDetection(bucket, key)
}

func (m *TieringManager) updateHotspotDetection(bucket, prefix string) {
	hotspotKey := bucket + "/" + prefix
	
	now := time.Now()
	hotspot, exists := m.hotspots[hotspotKey]
	if !exists {
		hotspot = &HotspotDetection{
			Bucket:         bucket,
			Prefix:         prefix,
			BaselineAccess: 1,
			DetectedAt:     now,
		}
		m.hotspots[hotspotKey] = hotspot
	}
	
	hotspot.AccessCount++
	
	windowStart := now.Add(-10 * time.Minute)
	recentAccesses := 0
	historyKey := bucket + "/" + prefix
	if history, ok := m.accessHistories[historyKey]; ok {
		for _, t := range history.AccessTimes {
			if t.After(windowStart) {
				recentAccesses++
			}
		}
	}
	
	if recentAccesses > hotspot.BaselineAccess*5 && recentAccesses > 10 {
		hotspot.IsHotspot = true
		hotspot.DetectedAt = now
	}
}

func (m *TieringManager) RunTieringDecision(ctx context.Context) ([]TierDecision, error) {
	m.mu.Lock()
	
	var objects []*common.ObjectMetadata
	for _, history := range m.accessHistories {
		metadata, err := m.store.Head(ctx, history.Bucket, history.ObjectKey)
		if err != nil {
			zap.L().Warn("tiering: failed to get object metadata",
				zap.String("bucket", history.Bucket),
				zap.String("key", history.ObjectKey),
				zap.Error(err),
			)
			continue
		}
		objects = append(objects, metadata)
	}
	
	m.mu.Unlock()
	
	sort.Slice(objects, func(i, j int) bool {
		return objects[i].LastAccessedAt.After(objects[j].LastAccessedAt)
	})
	
	var decisions []TierDecision
	
	for _, obj := range objects {
		historyKey := obj.Bucket + "/" + obj.Key
		history, exists := m.accessHistories[historyKey]
		if !exists {
			history = &AccessHistory{
				ObjectKey: obj.Key,
				Bucket:    obj.Bucket,
			}
		}
		
		if history.FixedTier {
			continue
		}
		
		score := m.model.CalculateHotnessScore(history, obj)
		targetTier := m.scoreToTier(score)
		
		if targetTier != obj.StorageTier {
			decisions = append(decisions, TierDecision{
				ObjectKey:    obj.Key,
				Bucket:       obj.Bucket,
				CurrentTier:  obj.StorageTier,
				TargetTier:   targetTier,
				HotnessScore: score,
				Reason:       fmt.Sprintf("score %.2f warrants migration from %s to %s", score, obj.StorageTier.String(), targetTier.String()),
				MigratedAt:   time.Now(),
			})
		}
	}
	
	return decisions, nil
}

func (m *TieringManager) scoreToTier(score float64) common.StorageTier {
	switch {
	case score > 80:
		return common.TierHot
	case score > 60:
		return common.TierWarm
	case score > 20:
		return common.TierCold
	default:
		return common.TierArchive
	}
}

func (m *TieringManager) ExecuteMigrations(ctx context.Context, decisions []TierDecision) error {
	m.mu.Lock()
	if len(decisions) == 0 {
		m.mu.Unlock()
		return nil
	}
	
	type migration struct {
		decision TierDecision
		result   chan error
	}
	
	migrationChan := make(chan migration, len(decisions))
	resultChan := make(chan error, len(decisions))
	
	for _, d := range decisions {
		migratingKey := d.Bucket + "/" + d.ObjectKey
		if m.migrating[migratingKey] {
			continue
		}
		m.migrating[migratingKey] = true
		migrationChan <- migration{
			decision: d,
			result:   resultChan,
		}
	}
	
	close(migrationChan)
	m.mu.Unlock()
	
	workerCount := m.config.MigrationWorkers
	if workerCount <= 0 {
		workerCount = 10
	}
	
	var wg sync.WaitGroup
	wg.Add(workerCount)
	
	for i := 0; i < workerCount; i++ {
		go func() {
			defer wg.Done()
			for mig := range migrationChan {
				err := m.store.Migrate(ctx, mig.decision.Bucket, mig.decision.ObjectKey, mig.decision.CurrentTier, mig.decision.TargetTier)
				mig.result <- err
				
				m.mu.Lock()
				migratingKey := mig.decision.Bucket + "/" + mig.decision.ObjectKey
				delete(m.migrating, migratingKey)
				m.mu.Unlock()
			}
		}()
	}
	
	go func() {
		wg.Wait()
		close(resultChan)
	}()
	
	var errs []error
	for err := range resultChan {
		if err != nil {
			errs = append(errs, err)
		}
	}
	
	if len(errs) > 0 {
		return fmt.Errorf("migration errors: %v", errs)
	}
	
	return nil
}

func (m *TieringManager) SetFixedTier(ctx context.Context, bucket, key string, fixed bool, tier *common.StorageTier) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	historyKey := bucket + "/" + key
	history, exists := m.accessHistories[historyKey]
	if !exists {
		history = &AccessHistory{
			ObjectKey: key,
			Bucket:    bucket,
		}
		m.accessHistories[historyKey] = history
	}
	
	history.FixedTier = fixed
	
	if fixed && tier != nil {
		metadata, err := m.store.Head(ctx, bucket, key)
		if err != nil {
			return err
		}
		if metadata.StorageTier != *tier {
			return m.store.Migrate(ctx, bucket, key, metadata.StorageTier, *tier)
		}
	}
	
	return nil
}

func (m *TieringManager) GetAccessHistory(bucket, key string) (*AccessHistory, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	history, exists := m.accessHistories[bucket+"/"+key]
	return history, exists
}

func (m *TieringManager) GetHotspots() []*HotspotDetection {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	var hotspots []*HotspotDetection
	for _, h := range m.hotspots {
		if h.IsHotspot {
			hotspots = append(hotspots, h)
		}
	}
	
	return hotspots
}

func (m *TieringManager) GetHotspotPrefetchCandidates() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	var candidates []string
	for _, h := range m.hotspots {
		if h.IsHotspot {
			candidates = append(candidates, h.Bucket+"/"+h.Prefix)
		}
	}
	
	return candidates
}
