package config

import (
	"fmt"
	"os"
	"time"

	"nexus/internal/units"

	"github.com/spf13/viper"
)

type RaftConfig struct {
	Enabled         bool     `mapstructure:"enabled"`
	DataDir         string   `mapstructure:"data_dir"`
	NodeID          string   `mapstructure:"node_id"`
	ListenAddr      string   `mapstructure:"listen_addr"`
	Peers           []string `mapstructure:"peers"`
	SnapshotCount   int      `mapstructure:"snapshot_count"`
	Heartbeat       string   `mapstructure:"heartbeat"`
	ElectionTimeout string   `mapstructure:"election_timeout"`
}

// FTSConfig defines the configuration for full-text search.
type FTSConfig struct {
	Enabled      bool    `mapstructure:"enabled"`
	DataDir      string  `mapstructure:"data_dir"`
	MaxIndexSize string  `mapstructure:"max_index_size"`
	SegmentSize  int     `mapstructure:"segment_size"`
	BM25K1       float64 `mapstructure:"bm25_k1"`
	BM25B        float64 `mapstructure:"bm25_b"`
}

// BackupConfig defines the configuration for backup and recovery.
type BackupConfig struct {
	Enabled         bool   `mapstructure:"enabled"`
	Dir             string `mapstructure:"dir"`
	Interval        string `mapstructure:"interval"`
	RetentionDays   []int  `mapstructure:"retention_days"`
	RemoteType      string `mapstructure:"remote_type"`
	RemoteEndpoint  string `mapstructure:"remote_endpoint"`
	RemoteBucket    string `mapstructure:"remote_bucket"`
	RemotePrefix    string `mapstructure:"remote_prefix"`
	EncryptionKeyID string `mapstructure:"encryption_key_id"`
}

// StorageClassConfig defines the backend configuration for a storage class.
type StorageClassConfig struct {
	BackendType string `mapstructure:"backend_type"` // "file", "s3", "azure", "erasure"
	BackendPath string `mapstructure:"backend_path"` // for file backend
	// S3 configuration
	S3Endpoint       string `mapstructure:"s3_endpoint"`
	S3Region         string `mapstructure:"s3_region"`
	S3Bucket         string `mapstructure:"s3_bucket"`
	S3AccessKey      string `mapstructure:"s3_access_key"`
	S3SecretKey      string `mapstructure:"s3_secret_key"`
	S3ForcePathStyle bool   `mapstructure:"s3_force_path_style"`
	// Azure configuration
	AzureAccountName string `mapstructure:"azure_account_name"`
	AzureAccountKey  string `mapstructure:"azure_account_key"`
	AzureContainer   string `mapstructure:"azure_container"`
	AzureEndpoint    string `mapstructure:"azure_endpoint"`
	// Erasure coding configuration
	ErasureDataShards   int `mapstructure:"erasure_data_shards"`
	ErasureParityShards int `mapstructure:"erasure_parity_shards"`
}

type ObservabilityConfig struct {
	MetricsEnabled     bool   `mapstructure:"metrics_enabled"`
	MetricsPath        string `mapstructure:"metrics_path"`
	TracingEnabled     bool   `mapstructure:"tracing_enabled"`
	TracingEndpoint    string `mapstructure:"tracing_endpoint"`
	TracingServiceName string `mapstructure:"tracing_service_name"`
	TracingInsecure    bool   `mapstructure:"tracing_insecure"`
}

type ResumableConfig struct {
	Enabled         bool   `mapstructure:"enabled"`
	UploadDir       string `mapstructure:"upload_dir"`
	DefaultExpiry   string `mapstructure:"default_expiry"`
	CleanupInterval string `mapstructure:"cleanup_interval"`
	MaxSessionSize  string `mapstructure:"max_session_size"`
}

type TaskQueueConfig struct {
	Enabled       bool   `mapstructure:"enabled"`
	StorePath     string `mapstructure:"store_path"`
	Workers       int    `mapstructure:"workers"`
	PollInterval  string `mapstructure:"poll_interval"`
	RetryBase     string `mapstructure:"retry_base"`
	MaxRetryDelay string `mapstructure:"max_retry_delay"`
	DefaultRetry  int    `mapstructure:"default_retry"`
}

type Config struct {
	Version        string                        `mapstructure:"version"`
	Node           NodeConfig                    `mapstructure:"node"`
	Tiering        TieringConfig                 `mapstructure:"tiering"`
	Encryption     EncryptionConfig              `mapstructure:"encryption"`
	CryptoServices CryptoServicesConfig          `mapstructure:"crypto_services"`
	Vector         VectorConfig                  `mapstructure:"vector"`
	Pipelines      PipelineConfig                `mapstructure:"pipelines"`
	Cache          CacheConfig                   `mapstructure:"cache"`
	Performance    PerformanceConfig             `mapstructure:"performance"`
	Logging        LoggingConfig                 `mapstructure:"logging"`
	Auth           AuthConfig                    `mapstructure:"auth"`
	IAM            IAMConfig                     `mapstructure:"iam"`
	TLS            TLSConfig                     `mapstructure:"tls"`
	RateLimit      RateLimitConfig               `mapstructure:"ratelimit"`
	Replication    ReplicationConfig             `mapstructure:"replication"`
	Events         EventsConfig                  `mapstructure:"events"`
	Observability  ObservabilityConfig           `mapstructure:"observability"`
	StorageClasses map[string]StorageClassConfig `mapstructure:"storage_classes"`
	Raft           RaftConfig                    `mapstructure:"raft"`
	Backup         BackupConfig                  `mapstructure:"backup"`
	FTS            FTSConfig                     `mapstructure:"fts"`
	Resumable      ResumableConfig               `mapstructure:"resumable"`
	TaskQueue      TaskQueueConfig               `mapstructure:"task_queue"`
}

type ReplicationConfig struct {
	AllowPrivateEndpoint bool `mapstructure:"allow_private_endpoint"`
}

type EventsConfig struct {
	Enabled        bool   `mapstructure:"enabled"`
	Workers        int    `mapstructure:"workers"`
	MaxRetries     int    `mapstructure:"max_retries"`
	RetryBaseMS    int    `mapstructure:"retry_base_ms"`
	WebhookTimeout string `mapstructure:"webhook_timeout"`
	DeadLetterDir  string `mapstructure:"dead_letter_dir"`
}

type NodeConfig struct {
	Role         string   `mapstructure:"role"`
	ListenAddr   string   `mapstructure:"listen_addr"`
	DataDir      string   `mapstructure:"data_dir"`
	ClusterPeers []string `mapstructure:"cluster_peers"`
	Domain       string   `mapstructure:"domain"`
}

type AuthConfig struct {
	RequireAuth   bool   `mapstructure:"require_auth"`
	AnonymousRead bool   `mapstructure:"anonymous_read"`
	JWTSecret     string `mapstructure:"jwt_secret"`
	TokenExpiry   string `mapstructure:"token_expiry"`
	RefreshExpiry string `mapstructure:"refresh_expiry"`
}

// IAMConfig for the new IAM system
type IAMConfig struct {
	Enabled        bool   `mapstructure:"enabled"`
	DBPath         string `mapstructure:"db_path"`
	MasterKeyPath  string `mapstructure:"master_key_path"`
	STSServiceAddr string `mapstructure:"sts_service_addr"`
}

type TLSConfig struct {
	Enabled      bool   `mapstructure:"enabled"`
	CertFile     string `mapstructure:"cert_file"`
	KeyFile      string `mapstructure:"key_file"`
	AutoCert     bool   `mapstructure:"auto_cert"`
	AutoCertHost string `mapstructure:"auto_cert_host"`
	RedirectAddr string `mapstructure:"redirect_addr"`
	MinVersion   string `mapstructure:"min_version"`
}

type RateLimitConfig struct {
	Enabled           bool                      `mapstructure:"enabled"`
	GlobalRPS         int64                     `mapstructure:"global_rps"`
	GlobalBurst       int                       `mapstructure:"global_burst"`
	IPRPS             int64                     `mapstructure:"ip_rps"`
	IPBurst           int                       `mapstructure:"ip_burst"`
	UserRPS           int                       `mapstructure:"user_rps"`
	UserBurst         int                       `mapstructure:"user_burst"`
	BucketRPS         int                       `mapstructure:"bucket_rps"`
	BucketBurst       int                       `mapstructure:"bucket_burst"`
	UploadBytesPerSec int64                     `mapstructure:"upload_bytes_per_sec"`
	UploadBurstBytes  int64                     `mapstructure:"upload_burst_bytes"`
	APILimits         map[string]APILimitConfig `mapstructure:"api_limits"`
	Whitelist         []string                  `mapstructure:"whitelist"`
}

type APILimitConfig struct {
	RPS   int `mapstructure:"rps"`
	Burst int `mapstructure:"burst"`
}

type TieringConfig struct {
	Enabled        bool     `mapstructure:"enabled"`
	Schedule       string   `mapstructure:"schedule"`
	HotMaxSize     string   `mapstructure:"hot_max_size"`
	WarmDevices    []string `mapstructure:"warm_devices"`
	ColdDevices    []string `mapstructure:"cold_devices"`
	ArchivePath    string   `mapstructure:"archive_path"`
	HotMaxBytes    int64    `mapstructure:"-"`
	WarmEnabled    bool     `mapstructure:"warm_enabled"`
	ColdEnabled    bool     `mapstructure:"cold_enabled"`
	ArchiveEnabled bool     `mapstructure:"archive_enabled"`
}

// Tier size multipliers relative to HotMaxBytes.
const (
	WarmTierMultiplier    = 5
	ColdTierMultiplier    = 20
	ArchiveTierMultiplier = 100
)

// WarmMaxBytes returns the warm tier capacity limit.
func (c TieringConfig) WarmMaxBytes() int64 { return c.HotMaxBytes * WarmTierMultiplier }

// ColdMaxBytes returns the cold tier capacity limit.
func (c TieringConfig) ColdMaxBytes() int64 { return c.HotMaxBytes * ColdTierMultiplier }

// ArchiveMaxBytes returns the archive tier capacity limit.
func (c TieringConfig) ArchiveMaxBytes() int64 { return c.HotMaxBytes * ArchiveTierMultiplier }

type EncryptionConfig struct {
	KMSType            string `mapstructure:"kms_type"`
	VaultAddr          string `mapstructure:"vault_addr"`
	VaultTokenFile     string `mapstructure:"vault_token_file"`
	VaultTransitKey    string `mapstructure:"vault_transit_key"`
	AWSKMSKeyID        string `mapstructure:"aws_kms_key_id"`
	AWSRegion          string `mapstructure:"aws_region"`
	KMSDegradationMode string `mapstructure:"kms_degradation_mode"` // "reject_writes" or "read_only"
	EnableDedup        bool   `mapstructure:"enable_dedup"`
	MasterKeyPath      string `mapstructure:"master_key_path"`
}

// CryptoServicesConfig for distributed crypto microservices
type CryptoServicesConfig struct {
	Enabled         bool   `mapstructure:"enabled"`
	DistributedMode bool   `mapstructure:"distributed_mode"`
	KeyPath         string `mapstructure:"key_path"`
	KeyStorePath    string `mapstructure:"keystore_path"`
	OPAAddress      string `mapstructure:"opa_address"`
	ConsulAddress   string `mapstructure:"consul_address"`
	MTLSCertFile    string `mapstructure:"mtls_cert_file"`
	MTLSKeyFile     string `mapstructure:"mtls_key_file"`
	MTLSCAFile      string `mapstructure:"mtls_ca_file"`
	AuditSize       int    `mapstructure:"audit_size"`
	// gRPC service addresses for distributed mode
	TokenServiceAddr     string `mapstructure:"token_service_addr"`
	KeyGenServiceAddr    string `mapstructure:"keygen_service_addr"`
	KeyUnwrapServiceAddr string `mapstructure:"keyunwrap_service_addr"`
	EncryptServiceAddr   string `mapstructure:"encrypt_service_addr"`
	DecryptServiceAddr   string `mapstructure:"decrypt_service_addr"`
	KeyStoreServiceAddr  string `mapstructure:"keystore_service_addr"`
}

type VectorConfig struct {
	Enabled              bool          `mapstructure:"enabled"`
	Dimension            int           `mapstructure:"dim"`
	IndexType            string       `mapstructure:"index_type"`
	MetricType           string       `mapstructure:"metric_type"`
	MaxVectors           int64         `mapstructure:"max_vectors"`
	EmbeddingProvider    string       `mapstructure:"embedding_provider"`
	EmbeddingModelPath   string       `mapstructure:"embedding_model_path"`
	EmbeddingAPIEndpoint string       `mapstructure:"embedding_api_endpoint"`
	EmbeddingAPIKey      string       `mapstructure:"embedding_api_key"`
	EmbeddingModelName   string       `mapstructure:"embedding_model_name"`
	AutoIndex            bool          `mapstructure:"auto_index"`
	MaxSearchTopK        int           `mapstructure:"max_search_top_k"`
	MaxQueryLength       int           `mapstructure:"max_query_length"`
	RequireAuth          bool          `mapstructure:"require_auth"`
	AllowedContentTypes  []string     `mapstructure:"allowed_content_types"`
	MaxIndexContentSize  int64         `mapstructure:"max_index_content_size"`
	// Milvus 后端配置(当 index_type = "milvus" 时使用)
	Milvus               *MilvusConfig `mapstructure:"milvus"`
	// VSE 后端配置(当 index_type = "vse" 时使用)
	VSE                  *VSEMapConfig `mapstructure:"vse"`
}

type VSEMapConfig struct {
	DataDir         string `mapstructure:"data_dir"`
	MaxHotSegments  int    `mapstructure:"max_hot_segments"`
	MaxColdSegments int    `mapstructure:"max_cold_segments"`
	HotSegmentSize  int    `mapstructure:"hot_segment_size"`
	ColdSegmentSize int    `mapstructure:"cold_segment_size"`
	IVFCentroids    int    `mapstructure:"ivf_centroids"`
	IVFNProbe       int    `mapstructure:"ivf_nprobe"`
	PQSubQuantizers int    `mapstructure:"pq_sub_quantizers"`
	PQBits          int    `mapstructure:"pq_bits"`
	HNSWM           int    `mapstructure:"hnsw_m"`
	HNSWEfSearch    int    `mapstructure:"hnsw_ef_search"`
	SearchWorkers   int    `mapstructure:"search_workers"`
	GPUAccel        bool   `mapstructure:"gpu_accel"`
	GPUBatchMin     int    `mapstructure:"gpu_batch_min"`
	QuantizerType   string `mapstructure:"quantizer_type"`
	CacheSize       int    `mapstructure:"cache_size"`
	AutoMerge       bool   `mapstructure:"auto_merge"`
	MergeInterval   string `mapstructure:"merge_interval"`
	EnableS3        bool   `mapstructure:"enable_s3"`
	S3Endpoint      string `mapstructure:"s3_endpoint"`
	S3Region        string `mapstructure:"s3_region"`
	S3Bucket        string `mapstructure:"s3_bucket"`
	S3AccessKey     string `mapstructure:"s3_access_key"`
	S3SecretKey     string `mapstructure:"s3_secret_key"`
	MergePolicy     *MergePolicyConfig     `mapstructure:"merge_policy"`
	WarmupPolicy    *WarmupPolicyConfig    `mapstructure:"warmup_policy"`
}

type MergePolicyConfig struct {
	HotTargetSize   int    `mapstructure:"hot_target_size"`
	HotMaxSegments  int    `mapstructure:"hot_max_segments"`
	ColdTargetSize  int    `mapstructure:"cold_target_size"`
	ColdMaxSegments int    `mapstructure:"cold_max_segments"`
	MinVectorsMerge int    `mapstructure:"min_vectors_merge"`
	MergeInterval   string `mapstructure:"merge_interval"`
	IdleThreshold   string `mapstructure:"idle_threshold"`
}

type WarmupPolicyConfig struct {
	Enabled          bool   `mapstructure:"enabled"`
	BatchSize        int    `mapstructure:"batch_size"`
	BatchInterval    string `mapstructure:"batch_interval"`
	PreloadCentroids bool   `mapstructure:"preload_centroids"`
	PreloadPQ        bool   `mapstructure:"preload_pq"`
	MaxPreloadBytes  int64  `mapstructure:"max_preload_bytes"`
}

// MilvusConfig 是 config 层的 Milvus 配置,映射到 vector.MilvusConfig。
type MilvusConfig struct {
	Address          string            `mapstructure:"address"`
	Username         string            `mapstructure:"username"`
	Password         string            `mapstructure:"password"`
	DBName           string            `mapstructure:"db_name"`
	CollectionName   string            `mapstructure:"collection_name"`
	ShardsNum        int32             `mapstructure:"shards_num"`
	IndexType        string            `mapstructure:"index_type"`
	IndexParams      map[string]string `mapstructure:"index_params"`
	NPROBE           int               `mapstructure:"nprobe"`
	EF               int               `mapstructure:"ef"`
	ConsistencyLevel string            `mapstructure:"consistency_level"`
	ReplicaNumber    int32             `mapstructure:"replica_number"`
	TieredStorage    *TieredStorageConfig `mapstructure:"tiered_storage"`
	DiskANN          *DiskANNConfig    `mapstructure:"diskann"`
}

type TieredStorageConfig struct {
	Enabled            bool   `mapstructure:"enabled"`
	HotResourceGroup   string `mapstructure:"hot_resource_group"`
	ColdResourceGroup  string `mapstructure:"cold_resource_group"`
	HotNodes           int32  `mapstructure:"hot_nodes"`
	ColdNodes          int32  `mapstructure:"cold_nodes"`
	WarmUp             string `mapstructure:"warm_up"`
}

type DiskANNConfig struct {
	MaxDegree                int     `mapstructure:"max_degree"`
	SearchListSize           int     `mapstructure:"search_list_size"`
	PQCodeBudgetGBRatio      float64 `mapstructure:"pq_code_budget_gb_ratio"`
	SearchCacheBudgetGBRatio float64 `mapstructure:"search_cache_budget_gb_ratio"`
	BeamWidthRatio           float64 `mapstructure:"beam_width_ratio"`
}

type PipelineConfig struct {
	ConfigFile    string `mapstructure:"config_file"`
	MaxConcurrent int    `mapstructure:"max_concurrent"`
	Enabled       bool   `mapstructure:"enabled"`
}

type CacheConfig struct {
	MetadataMaxSize  string        `mapstructure:"metadata_max_size"`
	ObjectMaxSize    string        `mapstructure:"object_max_size"`
	Policy           string        `mapstructure:"policy"`
	MetadataMaxBytes int64         `mapstructure:"-"`
	ObjectMaxBytes   int64         `mapstructure:"-"`
	TTL              time.Duration `mapstructure:"ttl"`
}

type PerformanceConfig struct {
	MaxUploadSize        string `mapstructure:"max_upload_size"`
	MaxConcurrentUploads int    `mapstructure:"max_concurrent_uploads"`
	IOWorkersPerCore     int    `mapstructure:"io_workers_per_core"`
	MaxUploadBytes       int64  `mapstructure:"-"`
	UseDirectIO          bool   `mapstructure:"use_direct_io"`
	EnableHTTP2          bool   `mapstructure:"enable_http2"`
	ReadBufferSize       int    `mapstructure:"read_buffer_size"`
	WriteBufferSize      int    `mapstructure:"write_buffer_size"`
}

type LoggingConfig struct {
	Level        string `mapstructure:"level"`
	Format       string `mapstructure:"format"`
	OutputPath   string `mapstructure:"output_path"`
	AccessLogDir string `mapstructure:"access_log_dir"`
}

func Load(configPath string) (*Config, error) {
	viper.SetConfigFile(configPath)
	viper.SetConfigType("yaml")

	viper.SetDefault("version", "2.0")
	viper.SetDefault("node.role", "all")
	viper.SetDefault("node.listen_addr", ":8080")
	viper.SetDefault("node.data_dir", "/var/lib/nexus")
	viper.SetDefault("tiering.enabled", true)
	viper.SetDefault("tiering.hot_max_size", "32GB")
	viper.SetDefault("encryption.enable_dedup", true)
	viper.SetDefault("encryption.vault_transit_key", "nexus")
	viper.SetDefault("encryption.kms_degradation_mode", "reject_writes")
	viper.SetDefault("crypto_services.enabled", false)
	viper.SetDefault("crypto_services.distributed_mode", false)
	viper.SetDefault("crypto_services.audit_size", 10000)
	viper.SetDefault("vector.enabled", true)
	viper.SetDefault("vector.dim", 768)
	viper.SetDefault("vector.hot_index_size", "10GB")
	viper.SetDefault("vector.auto_index", true)
	viper.SetDefault("vector.max_search_top_k", 100)
	viper.SetDefault("vector.max_query_length", 10000)
	viper.SetDefault("vector.require_auth", true)
	viper.SetDefault("vector.max_index_content_size", 1048576) // 1MB
	viper.SetDefault("vector.mmap_enabled", false)
	viper.SetDefault("vector.quantization_type", "none")
	viper.SetDefault("vector.pq_subquantizers", 8)
	viper.SetDefault("vector.index_data_dir", "data/vector")
	viper.SetDefault("vector.rebuild_interval", "1h")
	viper.SetDefault("vector.fallback_minutes", 5)
	viper.SetDefault("vector.vse.data_dir", "data/vector")
	viper.SetDefault("vector.vse.max_hot_segments", 10)
	viper.SetDefault("vector.vse.max_cold_segments", 100)
	viper.SetDefault("vector.vse.hot_segment_size", 10000)
	viper.SetDefault("vector.vse.cold_segment_size", 100000)
	viper.SetDefault("vector.vse.ivf_centroids", 256)
	viper.SetDefault("vector.vse.ivf_nprobe", 16)
	viper.SetDefault("vector.vse.pq_sub_quantizers", 8)
	viper.SetDefault("vector.vse.pq_bits", 8)
	viper.SetDefault("vector.vse.hnsw_m", 16)
	viper.SetDefault("vector.vse.hnsw_ef_search", 64)
viper.SetDefault("vector.vse.search_workers", 4)
viper.SetDefault("vector.vse.gpu_accel", false)
viper.SetDefault("vector.vse.gpu_batch_min", 50000)
	viper.SetDefault("vector.vse.quantizer_type", "sq")
	viper.SetDefault("vector.vse.cache_size", 10000)
	viper.SetDefault("vector.vse.auto_merge", true)
	viper.SetDefault("vector.vse.merge_interval", "10m")
	viper.SetDefault("vector.vse.enable_s3", false)
	viper.SetDefault("vector.vse.merge_policy.hot_target_size", 10000)
	viper.SetDefault("vector.vse.merge_policy.hot_max_segments", 10)
	viper.SetDefault("vector.vse.merge_policy.cold_target_size", 100000)
	viper.SetDefault("vector.vse.merge_policy.cold_max_segments", 50)
	viper.SetDefault("vector.vse.merge_policy.min_vectors_merge", 100)
	viper.SetDefault("vector.vse.merge_policy.merge_interval", "5m")
	viper.SetDefault("vector.vse.merge_policy.idle_threshold", "30m")
	viper.SetDefault("vector.vse.warmup_policy.enabled", true)
	viper.SetDefault("vector.vse.warmup_policy.batch_size", 5)
	viper.SetDefault("vector.vse.warmup_policy.batch_interval", "100ms")
	viper.SetDefault("vector.vse.warmup_policy.preload_centroids", true)
	viper.SetDefault("vector.vse.warmup_policy.preload_pq", false)
	viper.SetDefault("vector.vse.warmup_policy.max_preload_bytes", 536870912)
	viper.SetDefault("cache.policy", "tinyLFU")
	viper.SetDefault("cache.metadata_max_size", "10GB")
	viper.SetDefault("cache.object_max_size", "30GB")
	viper.SetDefault("performance.max_upload_size", "100GB")
	viper.SetDefault("performance.max_concurrent_uploads", 500)
	viper.SetDefault("performance.enable_http2", true)
	viper.SetDefault("logging.level", "info")
	viper.SetDefault("logging.format", "json")
	viper.SetDefault("auth.require_auth", false)
	viper.SetDefault("auth.anonymous_read", true)
	viper.SetDefault("auth.token_expiry", "24h")
	viper.SetDefault("auth.refresh_expiry", "168h")
	viper.SetDefault("tls.enabled", false)
	viper.SetDefault("tls.min_version", "1.2")
	viper.SetDefault("ratelimit.enabled", false)
	viper.SetDefault("ratelimit.global_rps", 1000)
	viper.SetDefault("ratelimit.global_burst", 100)
	viper.SetDefault("ratelimit.ip_rps", 100)
	viper.SetDefault("ratelimit.ip_burst", 20)
	viper.SetDefault("ratelimit.user_rps", 50)
	viper.SetDefault("ratelimit.user_burst", 10)
	viper.SetDefault("ratelimit.bucket_rps", 200)
	viper.SetDefault("ratelimit.bucket_burst", 30)
	viper.SetDefault("ratelimit.upload_bytes_per_sec", 52428800)
	viper.SetDefault("ratelimit.upload_burst_bytes", 104857600)
	viper.SetDefault("replication.allow_private_endpoint", false)
	viper.SetDefault("events.enabled", false)
	viper.SetDefault("events.workers", 16)
	viper.SetDefault("events.max_retries", 3)
	viper.SetDefault("events.retry_base_ms", 1000)
	viper.SetDefault("events.webhook_timeout", "5s")
	viper.SetDefault("events.dead_letter_dir", "data/deadletter")
	viper.SetDefault("observability.metrics_enabled", true)
	viper.SetDefault("observability.metrics_path", "/metrics")
	viper.SetDefault("observability.tracing_enabled", false)
	viper.SetDefault("observability.tracing_endpoint", "localhost:4317")
	viper.SetDefault("observability.tracing_service_name", "nexus")
	viper.SetDefault("observability.tracing_insecure", true)
	viper.SetDefault("backup.enabled", false)
	viper.SetDefault("backup.dir", "data/backups")
	viper.SetDefault("backup.interval", "24h")
	viper.SetDefault("backup.retention_days", []int{1, 7, 30})
	viper.SetDefault("backup.remote_type", "local")
	viper.SetDefault("backup.encryption_key_id", "nexus-backup-key")
	viper.SetDefault("raft.enabled", false)
	viper.SetDefault("raft.data_dir", "/var/lib/nexus/raft")
	viper.SetDefault("raft.node_id", "node1")
	viper.SetDefault("raft.listen_addr", ":9090")
	viper.SetDefault("raft.snapshot_count", 8192)
	viper.SetDefault("raft.heartbeat", "1s")
	viper.SetDefault("raft.election_timeout", "1s")
	viper.SetDefault("fts.enabled", false)
	viper.SetDefault("fts.data_dir", "/var/lib/nexus/fts")
	viper.SetDefault("fts.max_index_size", "10GB")
	viper.SetDefault("fts.segment_size", 1024)
	viper.SetDefault("fts.bm25_k1", 1.2)
	viper.SetDefault("fts.bm25_b", 0.75)
	viper.SetDefault("resumable.enabled", false)
	viper.SetDefault("resumable.upload_dir", "data/uploads")
	viper.SetDefault("resumable.default_expiry", "24h")
	viper.SetDefault("resumable.cleanup_interval", "5m")
	viper.SetDefault("resumable.max_session_size", "100GB")
	viper.SetDefault("task_queue.enabled", true)
	viper.SetDefault("task_queue.store_path", "data/tasks.db")
	viper.SetDefault("task_queue.workers", 4)
	viper.SetDefault("task_queue.poll_interval", "500ms")
	viper.SetDefault("task_queue.retry_base", "1s")
	viper.SetDefault("task_queue.max_retry_delay", "5m")
	viper.SetDefault("task_queue.default_retry", 3)
	viper.SetDefault("pipelines.config_file", "pipelines.yaml")
	viper.SetDefault("pipelines.max_concurrent", 100)
	viper.SetDefault("pipelines.enabled", false)

	if err := viper.ReadInConfig(); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("failed to read config: %w", err)
		}
	}

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	if err := cfg.normalize(); err != nil {
		return nil, fmt.Errorf("failed to normalize config: %w", err)
	}

	return &cfg, nil
}

func (c *Config) normalize() error {
	var err error

	if c.Tiering.HotMaxSize != "" {
		c.Tiering.HotMaxBytes, err = units.ParseSize(c.Tiering.HotMaxSize)
		if err != nil {
			return fmt.Errorf("invalid hot_max_size: %w", err)
		}
	}

	if c.Cache.MetadataMaxSize != "" {
		c.Cache.MetadataMaxBytes, err = units.ParseSize(c.Cache.MetadataMaxSize)
		if err != nil {
			return fmt.Errorf("invalid metadata_max_size: %w", err)
		}
	}

	if c.Cache.ObjectMaxSize != "" {
		c.Cache.ObjectMaxBytes, err = units.ParseSize(c.Cache.ObjectMaxSize)
		if err != nil {
			return fmt.Errorf("invalid object_max_size: %w", err)
		}
	}

	if c.Performance.MaxUploadSize != "" {
		c.Performance.MaxUploadBytes, err = units.ParseSize(c.Performance.MaxUploadSize)
		if err != nil {
			return fmt.Errorf("invalid max_upload_size: %w", err)
		}
	}

	if c.Cache.TTL == 0 {
		c.Cache.TTL = 5 * time.Minute
	}

	if c.Performance.IOWorkersPerCore == 0 {
		c.Performance.IOWorkersPerCore = 2
	}

	if c.Performance.ReadBufferSize == 0 {
		c.Performance.ReadBufferSize = 32 * 1024
	}

	if c.Performance.WriteBufferSize == 0 {
		c.Performance.WriteBufferSize = 32 * 1024
	}

	return nil
}
