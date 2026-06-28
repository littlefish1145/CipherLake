package replication

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testEndpoint = "https://s3.amazonaws.com"

func TestNewReplicationManager(t *testing.T) {
	mgr := NewReplicationManager(nil)
	assert.NotNil(t, mgr)
	assert.NotNil(t, mgr.rules)
	assert.NotNil(t, mgr.wal)
}

func TestReplicationManager_AddRule(t *testing.T) {
	mgr := NewReplicationManager(nil)

	rule := &ReplicationRule{
		ID:             "rule-001",
		Name:           "test-rule",
		SourceBucket:   "source-bucket",
		TargetBucket:   "target-bucket",
		TargetEndpoint: testEndpoint,
		TargetRegion:   "us-west-2",
		Status:         StatusPending,
		Priority:       1,
		Enabled:        true,
	}

	err := mgr.AddRule(rule)
	assert.NoError(t, err)

	rule2, ok := mgr.GetRule("rule-001")
	assert.True(t, ok)
	assert.Equal(t, "rule-001", rule2.ID)
	assert.Equal(t, "source-bucket", rule2.SourceBucket)
}

func TestReplicationManager_GetRule_NotFound(t *testing.T) {
	mgr := NewReplicationManager(nil)

	_, ok := mgr.GetRule("nonexistent")
	assert.False(t, ok)
}

func TestReplicationManager_RemoveRule(t *testing.T) {
	mgr := NewReplicationManager(nil)

	rule := &ReplicationRule{
		ID:             "rule-001",
		Name:           "test-rule",
		SourceBucket:   "source-bucket",
		TargetBucket:   "target-bucket",
		TargetEndpoint: testEndpoint,
		Status:         StatusPending,
	}

	err := mgr.AddRule(rule)
	require.NoError(t, err)

	err = mgr.RemoveRule("rule-001")
	assert.NoError(t, err)

	_, ok := mgr.GetRule("rule-001")
	assert.False(t, ok)
}

func TestReplicationManager_ListRules(t *testing.T) {
	mgr := NewReplicationManager(nil)

	rules := []ReplicationRule{
		{ID: "rule-1", Name: "rule1", SourceBucket: "bucket1", TargetBucket: "bucket2", TargetEndpoint: testEndpoint},
		{ID: "rule-2", Name: "rule2", SourceBucket: "bucket1", TargetBucket: "bucket3", TargetEndpoint: testEndpoint},
		{ID: "rule-3", Name: "rule3", SourceBucket: "bucket2", TargetBucket: "bucket4", TargetEndpoint: testEndpoint},
	}

	for _, r := range rules {
		err := mgr.AddRule(&r)
		require.NoError(t, err)
	}

	allRules := mgr.ListRules()
	assert.Equal(t, 3, len(allRules))
}

func TestReplicationManager_StartStop(t *testing.T) {
	mgr := NewReplicationManager(nil)

	err := mgr.Start()
	assert.NoError(t, err)

	time.Sleep(100 * time.Millisecond)

	err = mgr.Stop()
	assert.NoError(t, err)
}

func TestReplicationRule(t *testing.T) {
	rule := &ReplicationRule{
		ID:             "rule-001",
		Name:           "test-rule",
		SourceBucket:   "source-bucket",
		TargetBucket:   "target-bucket",
		TargetEndpoint: testEndpoint,
		TargetRegion:   "us-west-2",
		Status:         StatusPending,
		Priority:       1,
		Enabled:        true,
		ACLDisabled:    false,
		DeleteMarker:   true,
	}

	assert.Equal(t, "rule-001", rule.ID)
	assert.Equal(t, "source-bucket", rule.SourceBucket)
	assert.True(t, rule.Enabled)
}

func TestReplicationJob(t *testing.T) {
	job := &ReplicationJob{
		JobID:       "job-001",
		RuleID:      "rule-001",
		ObjectKey:   "test/key.txt",
		Bucket:      "bucket",
		Status:      StatusPending,
		Attempt:     0,
		MaxAttempts: 3,
		StartedAt:   time.Now(),
		ETag:        "etag123",
		Size:        1024,
	}

	assert.Equal(t, "job-001", job.JobID)
	assert.Equal(t, StatusPending, job.Status)
	assert.Equal(t, int64(0), job.Attempt)
}

func TestReplicationStatus(t *testing.T) {
	assert.Equal(t, ReplicationStatus("pending"), StatusPending)
	assert.Equal(t, ReplicationStatus("failed"), StatusFailed)
}

func TestReplicationWAL_Append(t *testing.T) {
	wal := NewReplicationWAL(1000)

	entry := WALEntry{
		ID:        "entry-001",
		ObjectKey: "test/key.txt",
		Bucket:    "bucket",
		Operation: "PUT",
	}

	err := wal.Append(entry)
	assert.NoError(t, err)
	assert.Equal(t, 1, len(wal.entries))
}

func TestReplicationWAL_GetPending(t *testing.T) {
	wal := NewReplicationWAL(1000)

	entries := []WALEntry{
		{ID: "1", ObjectKey: "key1", Bucket: "bucket", Operation: "PUT", Status: "pending"},
		{ID: "2", ObjectKey: "key2", Bucket: "bucket", Operation: "DELETE", Status: "completed"},
	}

	for _, e := range entries {
		wal.Append(e)
	}

	pending := wal.GetPending()
	assert.Equal(t, 1, len(pending))
}

func TestReplicationWAL_MarkCompleted(t *testing.T) {
	wal := NewReplicationWAL(1000)

	wal.Append(WALEntry{ID: "entry-1", ObjectKey: "key", Bucket: "bucket", Operation: "PUT", Status: "pending"})

	wal.MarkCompleted("entry-1")

	pending := wal.GetPending()
	assert.Equal(t, 0, len(pending))
}

func TestReplicationManager_GetSyncStatus(t *testing.T) {
	mgr := NewReplicationManager(nil)

	rule := &ReplicationRule{
		ID:             "rule-001",
		Name:           "test-rule",
		SourceBucket:   "source-bucket",
		TargetBucket:   "target-bucket",
		TargetEndpoint: testEndpoint,
		Enabled:        true,
	}

	err := mgr.AddRule(rule)
	require.NoError(t, err)

	status := mgr.GetSyncStatus("source-bucket")
	assert.NotNil(t, status)
}

func TestReplicationManager_LoadConfig(t *testing.T) {
	mgr := NewReplicationManager(nil)

	config := &ReplicationConfig{
		Rules: []ReplicationRule{
			{ID: "rule-1", Name: "rule1", SourceBucket: "src", TargetBucket: "dst", TargetEndpoint: testEndpoint},
		},
	}

	err := mgr.LoadConfig(config)
	assert.NoError(t, err)

	rules := mgr.ListRules()
	assert.Equal(t, 1, len(rules))
}

func TestReplicationManager_ExportConfig(t *testing.T) {
	mgr := NewReplicationManager(nil)

	rule := &ReplicationRule{
		ID:             "rule-001",
		Name:           "test-rule",
		SourceBucket:   "source-bucket",
		TargetBucket:   "target-bucket",
		TargetEndpoint: testEndpoint,
	}

	err := mgr.AddRule(rule)
	require.NoError(t, err)

	config, err := mgr.ExportConfig()
	assert.NoError(t, err)
	assert.NotNil(t, config)
}

func TestReplicationStats(t *testing.T) {
	stats := &ReplicationStats{
		TotalSyncs:       100,
		SuccessfulSyncs:  95,
		FailedSyncs:      5,
		BytesTransferred: 1024 * 1024,
		PendingJobs:      10,
		AvgLatencyMs:     50.5,
	}

	assert.Equal(t, int64(100), stats.TotalSyncs)
	assert.Equal(t, int64(95), stats.SuccessfulSyncs)
	assert.Equal(t, int64(5), stats.FailedSyncs)
	assert.Equal(t, int64(10), stats.PendingJobs)
}

func TestValidateEndpoint_PrivateBlocked(t *testing.T) {
	err := validateEndpoint("http://192.168.1.1:8080", false)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "private")
}

func TestValidateEndpoint_LoopbackBlocked(t *testing.T) {
	err := validateEndpoint("http://127.0.0.1:8080", false)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "loopback")
}

func TestValidateEndpoint_LoopbackBlockedEvenWithAllowPrivate(t *testing.T) {
	err := validateEndpoint("http://127.0.0.1:8080", true)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "loopback")
}

func TestValidateEndpoint_PrivateAllowedWithFlag(t *testing.T) {
	err := validateEndpoint("http://192.168.1.1:8080", true)
	assert.NoError(t, err)
}

func TestValidateEndpoint_PublicAlwaysAllowed(t *testing.T) {
	err := validateEndpoint("https://s3.amazonaws.com", false)
	assert.NoError(t, err)
}

func TestNewReplicationManager_AllowPrivateConfig(t *testing.T) {
	mgr := NewReplicationManager(&ReplicationConfig{AllowPrivateEndpoint: true})
	assert.True(t, mgr.allowPrivate)

	mgr2 := NewReplicationManager(&ReplicationConfig{AllowPrivateEndpoint: false})
	assert.False(t, mgr2.allowPrivate)

	mgr3 := NewReplicationManager(nil)
	assert.False(t, mgr3.allowPrivate)
}

func TestNewReplicationClient_AllowPrivate(t *testing.T) {
	_, err := NewReplicationClient("http://10.0.0.1:9000", nil, false)
	assert.Error(t, err)

	_, err = NewReplicationClient("http://10.0.0.1:9000", nil, true)
	assert.NoError(t, err)
}
