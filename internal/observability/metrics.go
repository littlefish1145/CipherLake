package observability

import (
	"net/http"
	"runtime"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	namespace = "nexus"
)

// MetricsRegistry holds all Prometheus metrics for the Nexus system.
type MetricsRegistry struct {
	// Original 14 metrics
	ObjectPutTotal          *prometheus.CounterVec
	ObjectGetTotal          *prometheus.CounterVec
	ObjectGetBytesTotal     *prometheus.CounterVec
	EncryptDuration         *prometheus.HistogramVec
	DecryptDuration         *prometheus.HistogramVec
	VectorSearchDuration    *prometheus.HistogramVec
	VectorIndexSizeVectors  prometheus.Gauge
	StorageIOBytesTotal     *prometheus.CounterVec
	ReplicationLagSeconds   *prometheus.GaugeVec
	EventDeliveryTotal      *prometheus.CounterVec
	IAMPolicyEvalDuration   prometheus.Histogram
	HTTPRequestDuration     *prometheus.HistogramVec
	GRPCRequestDuration     *prometheus.HistogramVec
	Up                      prometheus.Gauge

	// Object operations (new)
	ObjectDeleteTotal       *prometheus.CounterVec
	ObjectHeadTotal         *prometheus.CounterVec
	ObjectListTotal         *prometheus.CounterVec
	ObjectPutBytesTotal     *prometheus.CounterVec
	ObjectDeleteBytesTotal  *prometheus.CounterVec
	MultipartUploadTotal    *prometheus.CounterVec
	MultipartCompleteTotal  *prometheus.CounterVec
	MultipartAbortTotal     *prometheus.CounterVec
	CopyObjectTotal         *prometheus.CounterVec

	// Bucket operations (new)
	BucketCreateTotal       *prometheus.CounterVec
	BucketDeleteTotal       *prometheus.CounterVec
	BucketListTotal         *prometheus.CounterVec
	BucketInfoTotal         *prometheus.CounterVec

	// Encryption (new)
	EncryptionKeyGenerationTotal  *prometheus.CounterVec
	EncryptionKeyRotationTotal    *prometheus.CounterVec
	EncryptionOperationTotal      *prometheus.CounterVec
	SSECOperationTotal            *prometheus.CounterVec

	// KMS (new)
	KMSRequestTotal        *prometheus.CounterVec
	KMSRequestDuration     *prometheus.HistogramVec
	KMSErrorTotal          *prometheus.CounterVec
	KMSCacheHitTotal       *prometheus.CounterVec
	KMSCacheMissTotal      *prometheus.CounterVec

	// Raft (new)
	RaftLeaderChangesTotal  prometheus.Counter
	RaftAppliedEntriesTotal prometheus.Counter
	RaftCommitDuration      prometheus.Histogram
	RaftSnapshotDuration    prometheus.Histogram
	RaftPeerCount           prometheus.Gauge
	RaftIsLeader            prometheus.Gauge
	RaftTerm                prometheus.Gauge

	// Storage (new)
	StorageOperationTotal    *prometheus.CounterVec
	StorageOperationDuration *prometheus.HistogramVec
	StorageBackendHealth     *prometheus.GaugeVec
	ErasureRebuildTotal      *prometheus.CounterVec
	ErasureShardHealth       *prometheus.GaugeVec

	// Vector (new)
	VectorIndexTotal           *prometheus.CounterVec
	VectorIndexSizeBytes       *prometheus.GaugeVec
	VectorQuantizationError    *prometheus.HistogramVec
	VectorRebuildTotal         *prometheus.CounterVec
	VectorRebuildDuration      prometheus.Histogram

	// FTS (new)
	FTSIndexTotal             *prometheus.CounterVec
	FTSSearchDuration         *prometheus.HistogramVec
	FTSSegmentCount           prometheus.Gauge
	FTSIndexSizeBytes         prometheus.Gauge
	FTSDocumentCount          prometheus.Gauge

	// Backup (new)
	BackupTotal               *prometheus.CounterVec
	BackupDuration            *prometheus.HistogramVec
	BackupSizeBytes           *prometheus.GaugeVec
	RestoreTotal              *prometheus.CounterVec
	RestoreDuration           prometheus.Histogram

	// IAM (new)
	IAMAuthTotal              *prometheus.CounterVec
	IAMTokenIssuedTotal       *prometheus.CounterVec
	IAMPolicyEvalTotal        *prometheus.CounterVec
	IAMBoundaryEvalTotal      *prometheus.CounterVec
	IAMSCPEvalTotal           *prometheus.CounterVec

	// Resumable (new)
	ResumableSessionTotal     *prometheus.CounterVec
	ResumableUploadBytesTotal *prometheus.CounterVec
	ResumableSessionDuration  prometheus.Histogram

	// Cache (new)
	CacheHitTotal             *prometheus.CounterVec
	CacheMissTotal            *prometheus.CounterVec
	CacheEvictionTotal        *prometheus.CounterVec
	CacheSizeBytes            *prometheus.GaugeVec

	// System (new)
	GoGoroutines              prometheus.Gauge
	GoMemAllocBytes           prometheus.Gauge
	GoMemSysBytes             prometheus.Gauge
	GoGCDurationSeconds       prometheus.Summary
	ProcessOpenFDs            prometheus.Gauge
	ProcessMaxFDs             prometheus.Gauge
	ProcessResidentMemoryBytes prometheus.Gauge
	ProcessCPUSecondsTotal    prometheus.Counter
}

var defaultBuckets = []float64{.005, .01, .025, .05, .1, .25, .5}
var largeBuckets = []float64{.01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300}

// NewMetricsRegistry creates and registers all Prometheus metrics.
func NewMetricsRegistry() *MetricsRegistry {
	r := &MetricsRegistry{}

	r.ObjectPutTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "object_put_total",
			Help:      "Total number of object put operations",
		},
		[]string{"bucket", "status"},
	)

	r.ObjectGetTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "object_get_total",
			Help:      "Total number of object get operations",
		},
		[]string{"bucket", "status"},
	)

	r.ObjectGetBytesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "object_get_bytes_total",
			Help:      "Total bytes retrieved by object get operations",
		},
		[]string{"bucket"},
	)

	r.EncryptDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "encrypt_duration_seconds",
			Help:      "Duration of encryption operations",
			Buckets:   defaultBuckets,
		},
		[]string{"service"},
	)

	r.DecryptDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "decrypt_duration_seconds",
			Help:      "Duration of decryption operations",
			Buckets:   defaultBuckets,
		},
		[]string{"service"},
	)

	r.VectorSearchDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "vector_search_duration_seconds",
			Help:      "Duration of vector search operations",
			Buckets:   defaultBuckets,
		},
		[]string{"index_type"},
	)

	r.VectorIndexSizeVectors = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "vector_index_size_vectors",
			Help:      "Current number of vectors in the index",
		},
	)

	r.StorageIOBytesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "storage_io_bytes_total",
			Help:      "Total bytes of storage I/O operations",
		},
		[]string{"op", "backend"},
	)

	r.ReplicationLagSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "replication_lag_seconds",
			Help:      "Replication lag in seconds per rule",
		},
		[]string{"rule"},
	)

	r.EventDeliveryTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "event_delivery_total",
			Help:      "Total number of event deliveries",
		},
		[]string{"status"},
	)

	r.IAMPolicyEvalDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "iam_policy_eval_duration_seconds",
			Help:      "Duration of IAM policy evaluations",
			Buckets:   defaultBuckets,
		},
	)

	r.HTTPRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "http_request_duration_seconds",
			Help:      "Duration of HTTP requests",
			Buckets:   []float64{.005, .01, .025, .05, .1, .25, .5},
		},
		[]string{"method", "path", "status"},
	)

	r.GRPCRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "grpc_request_duration_seconds",
			Help:      "Duration of gRPC requests",
			Buckets:   defaultBuckets,
		},
		[]string{"method", "status"},
	)

	r.Up = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "up",
			Help:      "Indicates if the Nexus service is up (1 = up)",
		},
	)

	// Object operations (new)
	r.ObjectDeleteTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "object_delete_total",
			Help:      "Total number of object delete operations",
		},
		[]string{"bucket", "status"},
	)

	r.ObjectHeadTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "object_head_total",
			Help:      "Total number of object head operations",
		},
		[]string{"bucket", "status"},
	)

	r.ObjectListTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "object_list_total",
			Help:      "Total number of object list operations",
		},
		[]string{"bucket", "status"},
	)

	r.ObjectPutBytesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "object_put_bytes_total",
			Help:      "Total bytes written by object put operations",
		},
		[]string{"bucket"},
	)

	r.ObjectDeleteBytesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "object_delete_bytes_total",
			Help:      "Total bytes deleted by object delete operations",
		},
		[]string{"bucket"},
	)

	r.MultipartUploadTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "multipart_upload_total",
			Help:      "Total number of multipart upload initiations",
		},
		[]string{"bucket", "status"},
	)

	r.MultipartCompleteTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "multipart_complete_total",
			Help:      "Total number of multipart upload completions",
		},
		[]string{"bucket", "status"},
	)

	r.MultipartAbortTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "multipart_abort_total",
			Help:      "Total number of multipart upload aborts",
		},
		[]string{"bucket", "status"},
	)

	r.CopyObjectTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "copy_object_total",
			Help:      "Total number of copy object operations",
		},
		[]string{"bucket", "status"},
	)

	// Bucket operations (new)
	r.BucketCreateTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "bucket_create_total",
			Help:      "Total number of bucket create operations",
		},
		[]string{"status"},
	)

	r.BucketDeleteTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "bucket_delete_total",
			Help:      "Total number of bucket delete operations",
		},
		[]string{"status"},
	)

	r.BucketListTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "bucket_list_total",
			Help:      "Total number of bucket list operations",
		},
		[]string{"status"},
	)

	r.BucketInfoTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "bucket_info_total",
			Help:      "Total number of bucket info/head operations",
		},
		[]string{"status"},
	)

	// Encryption (new)
	r.EncryptionKeyGenerationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "encryption_key_generation_total",
			Help:      "Total number of encryption key generations",
		},
		[]string{"kms_type"},
	)

	r.EncryptionKeyRotationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "encryption_key_rotation_total",
			Help:      "Total number of encryption key rotations",
		},
		[]string{"kms_type"},
	)

	r.EncryptionOperationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "encryption_operation_total",
			Help:      "Total number of encryption/decryption operations",
		},
		[]string{"operation", "status"},
	)

	r.SSECOperationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "ssec_operation_total",
			Help:      "Total number of SSE-C specific operations",
		},
		[]string{"operation", "status"},
	)

	// KMS (new)
	r.KMSRequestTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "kms_request_total",
			Help:      "Total number of KMS requests",
		},
		[]string{"operation", "kms_type", "status"},
	)

	r.KMSRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "kms_request_duration_seconds",
			Help:      "Duration of KMS requests",
			Buckets:   defaultBuckets,
		},
		[]string{"operation", "kms_type"},
	)

	r.KMSErrorTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "kms_error_total",
			Help:      "Total number of KMS errors",
		},
		[]string{"operation", "kms_type"},
	)

	r.KMSCacheHitTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "kms_cache_hit_total",
			Help:      "Total number of KMS cache hits",
		},
		[]string{"kms_type"},
	)

	r.KMSCacheMissTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "kms_cache_miss_total",
			Help:      "Total number of KMS cache misses",
		},
		[]string{"kms_type"},
	)

	// Raft (new)
	r.RaftLeaderChangesTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "raft_leader_changes_total",
			Help:      "Total number of Raft leader changes",
		},
	)

	r.RaftAppliedEntriesTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "raft_applied_entries_total",
			Help:      "Total number of Raft entries applied to state machine",
		},
	)

	r.RaftCommitDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "raft_commit_duration_seconds",
			Help:      "Duration of Raft commit operations",
			Buckets:   defaultBuckets,
		},
	)

	r.RaftSnapshotDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "raft_snapshot_duration_seconds",
			Help:      "Duration of Raft snapshot operations",
			Buckets:   largeBuckets,
		},
	)

	r.RaftPeerCount = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "raft_peer_count",
			Help:      "Current number of Raft peers",
		},
	)

	r.RaftIsLeader = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "raft_is_leader",
			Help:      "Indicates if this node is the Raft leader (1 = yes)",
		},
	)

	r.RaftTerm = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "raft_term",
			Help:      "Current Raft term",
		},
	)

	// Storage (new)
	r.StorageOperationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "storage_operation_total",
			Help:      "Total number of storage backend operations",
		},
		[]string{"operation", "backend", "status"},
	)

	r.StorageOperationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "storage_operation_duration_seconds",
			Help:      "Duration of storage backend operations",
			Buckets:   defaultBuckets,
		},
		[]string{"operation", "backend"},
	)

	r.StorageBackendHealth = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "storage_backend_health",
			Help:      "Health status of storage backends (1=healthy, 0=unhealthy)",
		},
		[]string{"backend"},
	)

	r.ErasureRebuildTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "erasure_rebuild_total",
			Help:      "Total number of erasure-coded shard rebuilds",
		},
		[]string{"status"},
	)

	r.ErasureShardHealth = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "erasure_shard_health",
			Help:      "Health status of erasure-coded shards",
		},
		[]string{"shard_index"},
	)

	// Vector (new)
	r.VectorIndexTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "vector_index_total",
			Help:      "Total number of vector index operations",
		},
		[]string{"operation", "status"},
	)

	r.VectorIndexSizeBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "vector_index_size_bytes",
			Help:      "Current size of vector index in bytes per bucket",
		},
		[]string{"bucket"},
	)

	r.VectorQuantizationError = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "vector_quantization_error",
			Help:      "Quantization error for vector compression (SQ/PQ)",
			Buckets:   []float64{0.001, 0.005, 0.01, 0.02, 0.05, 0.1, 0.2, 0.5, 1.0},
		},
		[]string{"type"},
	)

	r.VectorRebuildTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "vector_rebuild_total",
			Help:      "Total number of vector index rebuilds",
		},
		[]string{"status"},
	)

	r.VectorRebuildDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "vector_rebuild_duration_seconds",
			Help:      "Duration of vector index rebuilds",
			Buckets:   largeBuckets,
		},
	)

	// FTS (new)
	r.FTSIndexTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "fts_index_total",
			Help:      "Total number of FTS index operations",
		},
		[]string{"operation", "status"},
	)

	r.FTSSearchDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "fts_search_duration_seconds",
			Help:      "Duration of FTS search operations",
			Buckets:   defaultBuckets,
		},
		[]string{"type"},
	)

	r.FTSSegmentCount = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "fts_segment_count",
			Help:      "Current number of FTS segments",
		},
	)

	r.FTSIndexSizeBytes = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "fts_index_size_bytes",
			Help:      "Current size of FTS index in bytes",
		},
	)

	r.FTSDocumentCount = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "fts_document_count",
			Help:      "Current total number of documents in FTS index",
		},
	)

	// Backup (new)
	r.BackupTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "backup_total",
			Help:      "Total number of backup operations",
		},
		[]string{"type", "status"},
	)

	r.BackupDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "backup_duration_seconds",
			Help:      "Duration of backup operations",
			Buckets:   largeBuckets,
		},
		[]string{"type"},
	)

	r.BackupSizeBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "backup_size_bytes",
			Help:      "Size of backups by type",
		},
		[]string{"type"},
	)

	r.RestoreTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "restore_total",
			Help:      "Total number of restore operations",
		},
		[]string{"status"},
	)

	r.RestoreDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "restore_duration_seconds",
			Help:      "Duration of restore operations",
			Buckets:   largeBuckets,
		},
	)

	// IAM (new)
	r.IAMAuthTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "iam_auth_total",
			Help:      "Total number of IAM authentication attempts",
		},
		[]string{"method", "status"},
	)

	r.IAMTokenIssuedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "iam_token_issued_total",
			Help:      "Total number of IAM tokens issued",
		},
		[]string{"type"},
	)

	r.IAMPolicyEvalTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "iam_policy_eval_total",
			Help:      "Total number of IAM policy evaluations",
		},
		[]string{"result"},
	)

	r.IAMBoundaryEvalTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "iam_boundary_eval_total",
			Help:      "Total number of IAM permission boundary evaluations",
		},
		[]string{"result"},
	)

	r.IAMSCPEvalTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "iam_scp_eval_total",
			Help:      "Total number of IAM SCP evaluations",
		},
		[]string{"result"},
	)

	// Resumable (new)
	r.ResumableSessionTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "resumable_session_total",
			Help:      "Total number of resumable upload sessions",
		},
		[]string{"status"},
	)

	r.ResumableUploadBytesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "resumable_upload_bytes_total",
			Help:      "Total bytes uploaded via resumable sessions",
		},
		[]string{"bucket"},
	)

	r.ResumableSessionDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "resumable_session_duration_seconds",
			Help:      "Duration of resumable upload sessions",
			Buckets:   largeBuckets,
		},
	)

	// Cache (new)
	r.CacheHitTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "cache_hit_total",
			Help:      "Total number of cache hits",
		},
		[]string{"tier"},
	)

	r.CacheMissTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "cache_miss_total",
			Help:      "Total number of cache misses",
		},
		[]string{"tier"},
	)

	r.CacheEvictionTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "cache_eviction_total",
			Help:      "Total number of cache evictions",
		},
		[]string{"tier"},
	)

	r.CacheSizeBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "cache_size_bytes",
			Help:      "Current size of cache by tier in bytes",
		},
		[]string{"tier"},
	)

	// System (new)
	r.GoGoroutines = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "go_goroutines",
			Help:      "Number of goroutines",
		},
	)

	r.GoMemAllocBytes = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "go_mem_alloc_bytes",
			Help:      "Number of bytes allocated and still in use",
		},
	)

	r.GoMemSysBytes = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "go_mem_sys_bytes",
			Help:      "Number of bytes obtained from system",
		},
	)

	r.GoGCDurationSeconds = prometheus.NewSummary(
		prometheus.SummaryOpts{
			Namespace:  namespace,
			Name:       "go_gc_duration_seconds",
			Help:       "Summary of GC pause durations",
			Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
		},
	)

	r.ProcessOpenFDs = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "process_open_fds",
			Help:      "Number of open file descriptors",
		},
	)

	r.ProcessMaxFDs = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "process_max_fds",
			Help:      "Maximum file descriptors allowed",
		},
	)

	r.ProcessResidentMemoryBytes = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "process_resident_memory_bytes",
			Help:      "Resident memory size in bytes",
		},
	)

	r.ProcessCPUSecondsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "process_cpu_seconds_total",
			Help:      "Total user and system CPU time spent in seconds",
		},
	)

	prometheus.MustRegister(
		// Original 14
		r.ObjectPutTotal,
		r.ObjectGetTotal,
		r.ObjectGetBytesTotal,
		r.EncryptDuration,
		r.DecryptDuration,
		r.VectorSearchDuration,
		r.VectorIndexSizeVectors,
		r.StorageIOBytesTotal,
		r.ReplicationLagSeconds,
		r.EventDeliveryTotal,
		r.IAMPolicyEvalDuration,
		r.HTTPRequestDuration,
		r.GRPCRequestDuration,
		r.Up,
		// Object operations (9 new)
		r.ObjectDeleteTotal,
		r.ObjectHeadTotal,
		r.ObjectListTotal,
		r.ObjectPutBytesTotal,
		r.ObjectDeleteBytesTotal,
		r.MultipartUploadTotal,
		r.MultipartCompleteTotal,
		r.MultipartAbortTotal,
		r.CopyObjectTotal,
		// Bucket operations (4 new)
		r.BucketCreateTotal,
		r.BucketDeleteTotal,
		r.BucketListTotal,
		r.BucketInfoTotal,
		// Encryption (4 new)
		r.EncryptionKeyGenerationTotal,
		r.EncryptionKeyRotationTotal,
		r.EncryptionOperationTotal,
		r.SSECOperationTotal,
		// KMS (5 new)
		r.KMSRequestTotal,
		r.KMSRequestDuration,
		r.KMSErrorTotal,
		r.KMSCacheHitTotal,
		r.KMSCacheMissTotal,
		// Raft (7 new)
		r.RaftLeaderChangesTotal,
		r.RaftAppliedEntriesTotal,
		r.RaftCommitDuration,
		r.RaftSnapshotDuration,
		r.RaftPeerCount,
		r.RaftIsLeader,
		r.RaftTerm,
		// Storage (5 new)
		r.StorageOperationTotal,
		r.StorageOperationDuration,
		r.StorageBackendHealth,
		r.ErasureRebuildTotal,
		r.ErasureShardHealth,
		// Vector (5 new)
		r.VectorIndexTotal,
		r.VectorIndexSizeBytes,
		r.VectorQuantizationError,
		r.VectorRebuildTotal,
		r.VectorRebuildDuration,
		// FTS (5 new)
		r.FTSIndexTotal,
		r.FTSSearchDuration,
		r.FTSSegmentCount,
		r.FTSIndexSizeBytes,
		r.FTSDocumentCount,
		// Backup (5 new)
		r.BackupTotal,
		r.BackupDuration,
		r.BackupSizeBytes,
		r.RestoreTotal,
		r.RestoreDuration,
		// IAM (5 new)
		r.IAMAuthTotal,
		r.IAMTokenIssuedTotal,
		r.IAMPolicyEvalTotal,
		r.IAMBoundaryEvalTotal,
		r.IAMSCPEvalTotal,
		// Resumable (3 new)
		r.ResumableSessionTotal,
		r.ResumableUploadBytesTotal,
		r.ResumableSessionDuration,
		// Cache (4 new)
		r.CacheHitTotal,
		r.CacheMissTotal,
		r.CacheEvictionTotal,
		r.CacheSizeBytes,
		// System (8 new)
		r.GoGoroutines,
		r.GoMemAllocBytes,
		r.GoMemSysBytes,
		r.GoGCDurationSeconds,
		r.ProcessOpenFDs,
		r.ProcessMaxFDs,
		r.ProcessResidentMemoryBytes,
		r.ProcessCPUSecondsTotal,
	)

	r.Up.Set(1)

	return r
}

// RecordPutObject records a put object operation.
func (m *MetricsRegistry) RecordPutObject(bucket string, status string) {
	if m != nil {
		m.ObjectPutTotal.WithLabelValues(bucket, status).Inc()
	}
}

// RecordGetObject records a get object operation.
func (m *MetricsRegistry) RecordGetObject(bucket string, status string, bytes int64) {
	if m != nil {
		m.ObjectGetTotal.WithLabelValues(bucket, status).Inc()
		m.ObjectGetBytesTotal.WithLabelValues(bucket).Add(float64(bytes))
	}
}

// RecordDeleteObject records a delete object operation.
func (m *MetricsRegistry) RecordDeleteObject(bucket string, status string) {
	if m != nil {
		m.ObjectDeleteTotal.WithLabelValues(bucket, status).Inc()
	}
}

// RecordHeadObject records a head object operation.
func (m *MetricsRegistry) RecordHeadObject(bucket string, status string) {
	if m != nil {
		m.ObjectHeadTotal.WithLabelValues(bucket, status).Inc()
	}
}

// RecordListObjects records a list objects operation.
func (m *MetricsRegistry) RecordListObjects(bucket string, status string) {
	if m != nil {
		m.ObjectListTotal.WithLabelValues(bucket, status).Inc()
	}
}

// RecordCopyObject records a copy object operation.
func (m *MetricsRegistry) RecordCopyObject(bucket string, status string) {
	if m != nil {
		m.CopyObjectTotal.WithLabelValues(bucket, status).Inc()
	}
}

// RecordKMSRequest records a KMS request with duration.
func (m *MetricsRegistry) RecordKMSRequest(operation, kmsType, status string, duration time.Duration) {
	if m != nil {
		m.KMSRequestTotal.WithLabelValues(operation, kmsType, status).Inc()
		m.KMSRequestDuration.WithLabelValues(operation, kmsType).Observe(duration.Seconds())
	}
}

// RecordRaftLeaderChange records a Raft leader change event.
func (m *MetricsRegistry) RecordRaftLeaderChange() {
	if m != nil {
		m.RaftLeaderChangesTotal.Inc()
	}
}

// RecordFTSSearch records an FTS search operation with duration.
func (m *MetricsRegistry) RecordFTSSearch(searchType string, duration time.Duration) {
	if m != nil {
		m.FTSSearchDuration.WithLabelValues(searchType).Observe(duration.Seconds())
	}
}

// RecordBackup records a backup operation with type, status, and duration.
func (m *MetricsRegistry) RecordBackup(backupType, status string, duration time.Duration) {
	if m != nil {
		m.BackupTotal.WithLabelValues(backupType, status).Inc()
		m.BackupDuration.WithLabelValues(backupType).Observe(duration.Seconds())
	}
}

// RecordIAMAuth records an IAM authentication attempt.
func (m *MetricsRegistry) RecordIAMAuth(method, status string) {
	if m != nil {
		m.IAMAuthTotal.WithLabelValues(method, status).Inc()
	}
}

// RecordEncryptDuration records an encryption operation duration.
func (m *MetricsRegistry) RecordEncryptDuration(service string, duration time.Duration) {
	if m != nil {
		m.EncryptDuration.WithLabelValues(service).Observe(duration.Seconds())
	}
}

// RecordDecryptDuration records a decryption operation duration.
func (m *MetricsRegistry) RecordDecryptDuration(service string, duration time.Duration) {
	if m != nil {
		m.DecryptDuration.WithLabelValues(service).Observe(duration.Seconds())
	}
}

// RecordVectorSearch records a vector search operation duration.
func (m *MetricsRegistry) RecordVectorSearch(indexType string, duration time.Duration) {
	if m != nil {
		m.VectorSearchDuration.WithLabelValues(indexType).Observe(duration.Seconds())
	}
}

// RecordStorageIO records a storage I/O operation.
func (m *MetricsRegistry) RecordStorageIO(op, backend string, bytes int64) {
	if m != nil {
		m.StorageIOBytesTotal.WithLabelValues(op, backend).Add(float64(bytes))
	}
}

// RecordEventDelivery records an event delivery.
func (m *MetricsRegistry) RecordEventDelivery(status string) {
	if m != nil {
		m.EventDeliveryTotal.WithLabelValues(status).Inc()
	}
}

// RecordIAMPolicyEval records an IAM policy evaluation duration.
func (m *MetricsRegistry) RecordIAMPolicyEval(duration time.Duration) {
	if m != nil {
		m.IAMPolicyEvalDuration.Observe(duration.Seconds())
	}
}

// UpdateSystemMetrics collects and updates Go runtime / process-level metrics.
func (m *MetricsRegistry) UpdateSystemMetrics() {
	if m == nil {
		return
	}
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	m.GoGoroutines.Set(float64(runtime.NumGoroutine()))
	m.GoMemAllocBytes.Set(float64(memStats.Alloc))
	m.GoMemSysBytes.Set(float64(memStats.Sys))
}

// HTTPMiddleware returns an HTTP middleware that records request duration and status.
func (m *MetricsRegistry) HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(wrapped, r)
		duration := time.Since(start)
		if m != nil {
			m.HTTPRequestDuration.WithLabelValues(
				r.Method,
				r.URL.Path,
				httpStatusClass(wrapped.statusCode),
			).Observe(duration.Seconds())
		}
	})
}

// MetricsHandler returns an HTTP handler that serves Prometheus metrics.
func MetricsHandler() http.Handler {
	return promhttp.Handler()
}

type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func httpStatusClass(code int) string {
	switch {
	case code >= 100 && code < 200:
		return "1xx"
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	default:
		return "5xx"
	}
}
