package replication

import (
	"context"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

type ReplicationRule struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	SourceBucket string            `json:"source_bucket"`
	TargetBucket string            `json:"target_bucket"`
	TargetEndpoint string          `json:"target_endpoint"`
	TargetRegion string            `json:"target_region"`
	PrefixFilter []string          `json:"prefix_filter,omitempty"`
	Status       ReplicationStatus `json:"status"`
	Priority     int               `json:"priority"`
	Enabled      bool              `json:"enabled"`
	ACLDisabled  bool             `json:"acl_disabled"`
	DeleteMarker bool             `json:"delete_marker"`
	CreateNewVersions bool        `json:"create_new_versions"`
	BandwidthLimit int64          `json:"bandwidth_limit_bytes_per_sec"`
	Schedule     string            `json:"schedule"`
	LastSyncAt   time.Time        `json:"last_sync_at"`
	SyncInterval time.Duration     `json:"sync_interval"`
}

type ReplicationStatus string

const (
	StatusPending   ReplicationStatus = "pending"
	StatusSyncing  ReplicationStatus = "syncing"
	StatusComplete ReplicationStatus = "complete"
	StatusFailed   ReplicationStatus = "failed"
	StatusDisabled ReplicationStatus = "disabled"
)

type ReplicationJob struct {
	JobID       string            `json:"job_id"`
	RuleID      string            `json:"rule_id"`
	ObjectKey   string            `json:"object_key"`
	Bucket      string            `json:"bucket"`
	Status      ReplicationStatus `json:"status"`
	Attempt     int64             `json:"attempt"`
	MaxAttempts int               `json:"max_attempts"`
	StartedAt   time.Time        `json:"started_at"`
	CompletedAt *time.Time       `json:"completed_at,omitempty"`
	Error       string           `json:"error,omitempty"`
	ETag        string           `json:"etag"`
	Size        int64            `json:"size"`
	Checksum    string           `json:"checksum"`
}

type ReplicationManager struct {
	mu           sync.RWMutex
	rules        map[string]*ReplicationRule
	jobs         map[string]*ReplicationJob
	wal          *ReplicationWAL
	client       *ReplicationClient
	stats        *ReplicationStats
	workers      map[string]*ReplicationWorker
	stopCh       chan struct{}
	allowPrivate bool
	started      bool
}

type ReplicationStats struct {
	TotalSyncs        int64
	SuccessfulSyncs   int64
	FailedSyncs      int64
	BytesTransferred  int64
	PendingJobs      int64
	AvgLatencyMs     float64
}

type ReplicationWAL struct {
	mu      sync.RWMutex
	entries []WALEntry
	maxSize int
}

type WALEntry struct {
	ID        string    `json:"id"`
	ObjectKey string    `json:"object_key"`
	Bucket    string    `json:"bucket"`
	Operation string    `json:"operation"`
	Status    string    `json:"status"`
	Timestamp time.Time `json:"timestamp"`
	Checksum  uint32   `json:"checksum"`
	Retries   int      `json:"retries"`
}

type ReplicationWorker struct {
	rule      *ReplicationRule
	client    *ReplicationClient
	jobs      chan *ReplicationJob
	stopCh    chan struct{}
	bandwidth int64
}

func NewReplicationManager(cfg *ReplicationConfig) *ReplicationManager {
	allowPrivate := false
	if cfg != nil {
		allowPrivate = cfg.AllowPrivateEndpoint
	}
	return &ReplicationManager{
		rules:        make(map[string]*ReplicationRule),
		jobs:         make(map[string]*ReplicationJob),
		wal:          NewReplicationWAL(10000),
		stats:        &ReplicationStats{},
		workers:      make(map[string]*ReplicationWorker),
		stopCh:       make(chan struct{}),
		allowPrivate: allowPrivate,
	}
}

func NewReplicationWAL(maxSize int) *ReplicationWAL {
	return &ReplicationWAL{
		entries: make([]WALEntry, 0, maxSize),
		maxSize: maxSize,
	}
}

func (w *ReplicationWAL) Append(entry WALEntry) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	entry.Checksum = crc32.ChecksumIEEE([]byte(entry.ObjectKey + entry.Bucket + entry.Operation))
	entry.Timestamp = time.Now()

	w.entries = append(w.entries, entry)

	if len(w.entries) > w.maxSize {
		w.entries = w.entries[len(w.entries)-w.maxSize:]
	}

	return nil
}

func (w *ReplicationWAL) GetPending() []WALEntry {
	w.mu.RLock()
	defer w.mu.RUnlock()

	var pending []WALEntry
	for _, e := range w.entries {
		if e.Status == "pending" || e.Status == "failed" {
			pending = append(pending, e)
		}
	}
	return pending
}

func (w *ReplicationWAL) MarkCompleted(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for i := range w.entries {
		if w.entries[i].ID == id {
			w.entries[i].Status = "completed"
			break
		}
	}
}

type ReplicationClient struct {
	client   *http.Client
	endpoint string
	auth     *ReplicationAuth
}

type ReplicationAuth struct {
	AccessKey string
	SecretKey string
	Region    string
}

func NewReplicationClient(endpoint string, auth *ReplicationAuth, allowPrivate bool) (*ReplicationClient, error) {
	if err := validateEndpoint(endpoint, allowPrivate); err != nil {
		return nil, fmt.Errorf("invalid replication endpoint: %w", err)
	}
	return &ReplicationClient{
		client: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 3 {
					return fmt.Errorf("too many redirects")
				}
				return validateEndpoint(req.URL.String(), allowPrivate)
			},
		},
		endpoint: endpoint,
		auth:     auth,
	}, nil
}

func validateEndpoint(rawURL string, allowPrivate bool) error {
	if rawURL == "" {
		return fmt.Errorf("endpoint URL is empty")
	}
	if !strings.HasPrefix(rawURL, "https://") && !strings.HasPrefix(rawURL, "http://") {
		return fmt.Errorf("endpoint must use http or https scheme")
	}
	hostPort := rawURL
	for _, prefix := range []string{"https://", "http://"} {
		hostPort = strings.TrimPrefix(hostPort, prefix)
	}
	if idx := strings.Index(hostPort, "/"); idx != -1 {
		hostPort = hostPort[:idx]
	}
	if idx := strings.Index(hostPort, "?"); idx != -1 {
		hostPort = hostPort[:idx]
	}
	host := hostPort
	if strings.Contains(host, ":") {
		h, _, err := net.SplitHostPort(host)
		if err != nil {
			return fmt.Errorf("invalid host:port in endpoint")
		}
		host = h
	}
	ip := net.ParseIP(host)
	if ip != nil {
		if ip.IsLoopback() || ip.IsUnspecified() {
			return fmt.Errorf("endpoint must not point to a loopback address")
		}
		if !allowPrivate && (ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()) {
			return fmt.Errorf("endpoint must not point to a private or link-local address")
		}
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("failed to resolve endpoint host: %w", err)
	}
	for _, resolvedIP := range ips {
		if resolvedIP.IsLoopback() || resolvedIP.IsUnspecified() {
			return fmt.Errorf("endpoint resolves to a loopback address")
		}
		if !allowPrivate && (resolvedIP.IsPrivate() || resolvedIP.IsLinkLocalUnicast() || resolvedIP.IsLinkLocalMulticast()) {
			return fmt.Errorf("endpoint resolves to a private or link-local address")
		}
	}
	return nil
}

func (c *ReplicationClient) PutObject(ctx context.Context, bucket, key string, body io.Reader, size int64, metadata map[string]string) error {
	url := fmt.Sprintf("%s/%s/%s", c.endpoint, bucket, key)

	req, err := http.NewRequestWithContext(ctx, "PUT", url, body)
	if err != nil {
		return err
	}

	req.ContentLength = size
	for k, v := range metadata {
		req.Header.Set(k, v)
	}

	if c.auth != nil {
		req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s", c.auth.AccessKey, c.auth.Region))
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("replication failed with status: %d", resp.StatusCode)
	}

	return nil
}

func (c *ReplicationClient) DeleteObject(ctx context.Context, bucket, key string) error {
	url := fmt.Sprintf("%s/%s/%s", c.endpoint, bucket, key)

	req, err := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	if err != nil {
		return err
	}

	if c.auth != nil {
		req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s/%s", c.auth.AccessKey, c.auth.Region, "s3"))
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 && resp.StatusCode != 404 {
		return fmt.Errorf("replication delete failed with status: %d", resp.StatusCode)
	}

	return nil
}

func (m *ReplicationManager) AddRule(rule *ReplicationRule) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if rule.ID == "" {
		rule.ID = uuid.New().String()
	}

	m.rules[rule.ID] = rule

	auth := &ReplicationAuth{
		Region: rule.TargetRegion,
	}
	client, err := NewReplicationClient(rule.TargetEndpoint, auth, m.allowPrivate)
	if err != nil {
		delete(m.rules, rule.ID)
		return fmt.Errorf("failed to create replication client: %w", err)
	}

	worker := &ReplicationWorker{
		rule:      rule,
		client:    client,
		jobs:      make(chan *ReplicationJob, 100),
		stopCh:    make(chan struct{}),
		bandwidth: rule.BandwidthLimit,
	}
	m.workers[rule.ID] = worker

	if m.started {
		go worker.run()
	}

	return nil
}

func (m *ReplicationManager) RemoveRule(ruleID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if worker, ok := m.workers[ruleID]; ok {
		close(worker.stopCh)
		delete(m.workers, ruleID)
	}

	delete(m.rules, ruleID)
	return nil
}

func (w *ReplicationWorker) run() {
	for {
		select {
		case <-w.stopCh:
			return
		case job := <-w.jobs:
			w.processJob(job)
		}
	}
}

func (w *ReplicationWorker) processJob(job *ReplicationJob) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	switch job.Status {
	case StatusPending:
		w.syncObject(ctx, job)
	default:
	}

	atomic.AddInt64(&job.Attempt, 1)
}

func (w *ReplicationWorker) syncObject(ctx context.Context, job *ReplicationJob) error {
	if w.client == nil {
		return fmt.Errorf("replication client not configured")
	}

	switch job.Status {
	case StatusPending:
		job.Status = StatusSyncing
		job.StartedAt = time.Now()
	}

	if job.Attempt >= int64(job.MaxAttempts) {
		job.Status = StatusFailed
		job.Error = "max attempts exceeded"
		return fmt.Errorf("max attempts exceeded for job %s", job.JobID)
	}

	err := w.client.PutObject(ctx, w.rule.TargetBucket, job.ObjectKey, nil, job.Size, map[string]string{
		"X-Replication-Source": job.Bucket,
		"X-Replication-Rule":  job.RuleID,
	})

	if err != nil {
		job.Status = StatusFailed
		job.Error = err.Error()
		return err
	}

	job.Status = StatusComplete
	now := time.Now()
	job.CompletedAt = &now
	job.Checksum = fmt.Sprintf("%x", crc32.ChecksumIEEE([]byte(job.ObjectKey+job.Bucket)))

	return nil
}

func (m *ReplicationManager) SyncObject(ctx context.Context, ruleID, bucket, key string, operation string) error {
	m.mu.RLock()
	rule, exists := m.rules[ruleID]
	m.mu.RUnlock()

	if !exists || !rule.Enabled {
		return fmt.Errorf("rule not found or disabled")
	}

	job := &ReplicationJob{
		JobID:       uuid.New().String(),
		RuleID:      ruleID,
		ObjectKey:   key,
		Bucket:      bucket,
		Status:      StatusPending,
		Attempt:     0,
		MaxAttempts: 3,
		StartedAt:   time.Now(),
	}

	m.wal.Append(WALEntry{
		ID:        job.JobID,
		ObjectKey: key,
		Bucket:    bucket,
		Operation: operation,
		Status:    "pending",
	})

	if worker, ok := m.workers[ruleID]; ok {
		select {
		case worker.jobs <- job:
		default:
			return fmt.Errorf("job queue is full")
		}
	}

	atomic.AddInt64(&m.stats.TotalSyncs, 1)
	atomic.AddInt64(&m.stats.PendingJobs, 1)

	return nil
}

func (m *ReplicationManager) GetRule(ruleID string) (*ReplicationRule, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	rule, exists := m.rules[ruleID]
	return rule, exists
}

func (m *ReplicationManager) ListRules() []*ReplicationRule {
	m.mu.RLock()
	defer m.mu.RUnlock()

	rules := make([]*ReplicationRule, 0, len(m.rules))
	for _, rule := range m.rules {
		rules = append(rules, rule)
	}
	return rules
}

func (m *ReplicationManager) GetStats() *ReplicationStats {
	return &ReplicationStats{
		TotalSyncs:       atomic.LoadInt64(&m.stats.TotalSyncs),
		SuccessfulSyncs: atomic.LoadInt64(&m.stats.SuccessfulSyncs),
		FailedSyncs:     atomic.LoadInt64(&m.stats.FailedSyncs),
		BytesTransferred: atomic.LoadInt64(&m.stats.BytesTransferred),
		PendingJobs:     atomic.LoadInt64(&m.stats.PendingJobs),
		AvgLatencyMs:    m.stats.AvgLatencyMs,
	}
}

func (m *ReplicationManager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.started {
		return nil
	}
	m.started = true

	for _, worker := range m.workers {
		go worker.run()
	}

	return nil
}

func (m *ReplicationManager) Stop() error {
	close(m.stopCh)

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, worker := range m.workers {
		close(worker.stopCh)
	}

	return nil
}

type ReplicationConfig struct {
	AllowPrivateEndpoint bool              `json:"allow_private_endpoint"`
	Rules                []ReplicationRule `json:"rules"`
}

func (m *ReplicationManager) LoadConfig(config *ReplicationConfig) error {
	for _, rule := range config.Rules {
		if err := m.AddRule(&rule); err != nil {
			return fmt.Errorf("failed to add rule %s: %w", rule.ID, err)
		}
	}
	return nil
}

func (m *ReplicationManager) ExportConfig() (*ReplicationConfig, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	config := &ReplicationConfig{
		Rules: make([]ReplicationRule, 0, len(m.rules)),
	}

	for _, rule := range m.rules {
		config.Rules = append(config.Rules, *rule)
	}

	return config, nil
}

type ConflictResolution string

const (
	ConflictLastWriteWins ConflictResolution = "last_write_wins"
	ConflictSourceWins   ConflictResolution = "source_wins"
	ConflictFail         ConflictResolution = "fail"
)

type SyncStatus struct {
	Bucket       string    `json:"bucket"`
	PendingCount int       `json:"pending_count"`
	FailedCount  int       `json:"failed_count"`
	LastSync     time.Time `json:"last_sync"`
	LagMs        int64     `json:"lag_ms"`
}

func (m *ReplicationManager) GetSyncStatus(bucket string) *SyncStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	status := &SyncStatus{
		Bucket: bucket,
	}

	pending := m.wal.GetPending()
	for _, p := range pending {
		if p.Bucket == bucket {
			if p.Status == "pending" {
				status.PendingCount++
			} else if p.Status == "failed" {
				status.FailedCount++
			}
		}
	}

	return status
}
