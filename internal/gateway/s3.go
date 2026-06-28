package gateway

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
<<<<<<< HEAD
=======
	"sync"
>>>>>>> f0a7576 (feat: implement nexus-flow embedded workflow kernel + pipeline fixes)
	"time"

	"go.uber.org/zap"

	"nexus/internal/auth"
	"nexus/internal/bootstrap"
	"nexus/internal/cache"
	"nexus/internal/common"
	"nexus/internal/config"
	"nexus/internal/events"
	"nexus/internal/flow"
	"nexus/internal/fts"
	"nexus/internal/logger"
	"nexus/internal/metadata"
	"nexus/internal/observability"
	"nexus/internal/pipeline"
	"nexus/internal/ratelimit"
	"nexus/internal/services"
	"nexus/internal/storage"
	"nexus/internal/taskqueue"
	"nexus/internal/tiering"
	"nexus/internal/units"
	"nexus/internal/vector"
	"nexus/internal/vector/vse"
)

var (
	bucketNameRegex         = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	pathTraversalPattern    = regexp.MustCompile(`(?:^|/)\.\.(?:$|/)`)
	controlCharPattern      = regexp.MustCompile(`[\x00-\x1f]`)
	encodedTraversalPattern = regexp.MustCompile(`(?i)(?:%2e%2e|%2e\.|\.\.%2e)`)

	maxRequestBodyBytes int64 = 50 * 1024 * 1024
)

type S3Gateway struct {
	config            *config.Config
	metadata          *metadata.BoltDBMetadataStore
	store             *storage.TieredObjectStore
	tiering           *tiering.TieringManager
	cryptoCoordinator *services.EncryptionCoordinator
	vector            *vector.VectorManager
	ftsIndex          *fts.InvertedIndex
	pipeline          *pipeline.PipelineExecutor
	auth              *AuthHandler
	iamBridge         *IAMAuthBridge
	authProvider      auth.Provider
	permChecker       auth.PermissionChecker
	rateLimiter       *ratelimit.MultiLevelLimiter
	objectCache       *cache.ObjectCache
	metaCache         *cache.MetadataCache
	server            *http.Server
	buckets           map[string]*BucketState
	accessLog         *AccessLogger
	eventBus          *events.EventBus
	metrics           *observability.MetricsRegistry
	tracerShutdown    func(context.Context) error
	healthHandler     *observability.HealthHandler
	resumableHandler  *ResumableUploadHandler
	resumableCleanup  *ResumableCleanup
	bucketSvc         *BucketService
	objectSvc         *ObjectService
	searchSvc         *SearchService
	multipartSvc      *MultipartService
	taskQueue         *taskqueue.Queue
}

type BucketState struct {
	Name        string
	CreatedAt   time.Time
	ObjectCount int64
	TotalSize   int64
}

func NewS3Gateway(cfg *config.Config) (*S3Gateway, error) {
	gateway := &S3Gateway{
		config:  cfg,
		buckets: make(map[string]*BucketState),
	}

	if err := gateway.initializeStores(cfg); err != nil {
		return nil, fmt.Errorf("failed to initialize stores: %w", err)
	}

	if err := gateway.initializeComponents(cfg); err != nil {
		return nil, fmt.Errorf("failed to initialize components: %w", err)
	}

	accessLogDir := cfg.Logging.AccessLogDir
	if accessLogDir == "" {
		accessLogDir = cfg.Node.DataDir + "/logs"
	}
	accessLogger, err := NewAccessLogger(accessLogDir, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to create access logger: %w", err)
	}
	gateway.accessLog = accessLogger

	return gateway, nil
}

func (g *S3Gateway) initializeStores(cfg *config.Config) error {
	metadataStore, err := metadata.NewBoltDBMetadataStore(cfg.Node.DataDir + "/metadata.db")
	if err != nil {
		return fmt.Errorf("failed to create metadata store: %w", err)
	}
	g.metadata = metadataStore

	store := storage.NewTieredObjectStore(metadataStore)

	hotDir := cfg.Node.DataDir + "/hot"
	hotBackend, err := storage.NewFileBackend(hotDir)
	if err != nil {
		return fmt.Errorf("failed to create hot storage backend: %w", err)
	}
	store.RegisterTier(common.TierHot, hotBackend, cfg.Tiering.HotMaxBytes)

	warmDir := cfg.Node.DataDir + "/warm"
	warmBackend, err := storage.NewFileBackend(warmDir)
	if err != nil {
		return fmt.Errorf("failed to create warm storage backend: %w", err)
	}
	warmMaxSize := cfg.Tiering.WarmMaxBytes()
	store.RegisterTier(common.TierWarm, warmBackend, warmMaxSize)

	coldDir := cfg.Node.DataDir + "/cold"
	coldBackend, err := storage.NewFileBackend(coldDir)
	if err != nil {
		return fmt.Errorf("failed to create cold storage backend: %w", err)
	}
	coldMaxSize := cfg.Tiering.ColdMaxBytes()
	store.RegisterTier(common.TierCold, coldBackend, coldMaxSize)

	archiveDir := cfg.Node.DataDir + "/archive"
	if cfg.Tiering.ArchivePath != "" {
		archiveDir = cfg.Tiering.ArchivePath
	}
	archiveBackend, err := storage.NewFileBackend(archiveDir)
	if err != nil {
		return fmt.Errorf("failed to create archive storage backend: %w", err)
	}
	archiveMaxSize := cfg.Tiering.ArchiveMaxBytes()
	store.RegisterTier(common.TierArchive, archiveBackend, archiveMaxSize)

	g.store = store

	return nil
}

func (g *S3Gateway) initializeComponents(cfg *config.Config) error {
	// Initialize observability (metrics + tracing)
	if cfg.Observability.MetricsEnabled {
		g.metrics = observability.NewMetricsRegistry()
	}

	if cfg.Observability.TracingEnabled {
		tracingCfg := &observability.TracingConfig{
			Enabled:     true,
			Endpoint:    cfg.Observability.TracingEndpoint,
			ServiceName: cfg.Observability.TracingServiceName,
			Insecure:    cfg.Observability.TracingInsecure,
		}
		shutdown, err := observability.InitTracer(tracingCfg)
		if err != nil {
			return fmt.Errorf("failed to initialize tracer: %w", err)
		}
		g.tracerShutdown = shutdown
	}

	// Initialize health checks
	g.healthHandler = observability.NewHealthHandler()
	g.healthHandler.RegisterCheck("boltdb", func() bool {
		return g.metadata != nil
	})

	// Initialize crypto services with zero-trust architecture
	if cfg.CryptoServices.Enabled {
		coordinator, err := bootstrap.InitializeCryptoServices(cfg)
		if err != nil {
			return fmt.Errorf("failed to initialize crypto services: %w", err)
		}
		g.cryptoCoordinator = coordinator
	}

	tieringConfig := &tiering.TieringConfig{
		Enabled:          cfg.Tiering.Enabled,
		HotMaxSize:       cfg.Tiering.HotMaxBytes,
		MigrationWorkers: 10,
		CheckInterval:    6 * time.Hour,
	}
	tieringManager := tiering.NewTieringManager(g.store, tieringConfig)
	g.tiering = tieringManager

	if cfg.Vector.Enabled {
		vectorConfig := &vector.VectorConfig{
			Enabled:              cfg.Vector.Enabled,
			Dimension:            cfg.Vector.Dimension,
			IndexType:            cfg.Vector.IndexType,
			MetricType:           cfg.Vector.MetricType,
			MaxVectors:           cfg.Vector.MaxVectors,
			QueryCacheTTL:        5 * time.Minute,
			EmbeddingProvider:    cfg.Vector.EmbeddingProvider,
			EmbeddingModelPath:   cfg.Vector.EmbeddingModelPath,
			EmbeddingAPIEndpoint: cfg.Vector.EmbeddingAPIEndpoint,
			EmbeddingAPIKey:      cfg.Vector.EmbeddingAPIKey,
			EmbeddingModelName:   cfg.Vector.EmbeddingModelName,
		}
		// 双后端支持:根据 index_type 传递 Milvus 或 VSE 配置
		if cfg.Vector.Milvus != nil {
			vectorConfig.Milvus = convertMilvusConfig(cfg.Vector.Milvus)
		}
		if cfg.Vector.VSE != nil {
			vectorConfig.VSE = convertVSEConfig(cfg.Vector.VSE)
		}
		vectorManager, err := vector.NewVectorManager(vectorConfig)
		if err != nil {
			return fmt.Errorf("failed to create vector manager: %w", err)
		}
		g.vector = vectorManager
	}

	tele := flow.NewTelemetry(flow.DefaultTelemetryConfig())

	pipelineExecutor := pipeline.NewPipelineExecutorWithTelemetry(cfg.Pipelines.MaxConcurrent, tele)
	if err := pipeline.RegisterDefaultPlugins(pipelineExecutor); err != nil {
		return fmt.Errorf("failed to register default plugins: %w", err)
	}
	if cfg.Pipelines.ConfigFile != "" {
		if err := pipelineExecutor.LoadConfig(cfg.Pipelines.ConfigFile); err != nil {
			logger.Warn("Failed to load pipeline config", zap.String("path", cfg.Pipelines.ConfigFile), zap.Error(err))
		}
	}
	g.pipeline = pipelineExecutor

	authConfig := &AuthConfig{
		RequireAuth:   cfg.Auth.RequireAuth,
		AnonymousRead: cfg.Auth.AnonymousRead,
		JWTSecret:     cfg.Auth.JWTSecret,
	}
	if cfg.Auth.TokenExpiry != "" {
		if d, err := time.ParseDuration(cfg.Auth.TokenExpiry); err == nil {
			authConfig.TokenExpiry = d
		}
	}
	if cfg.Auth.RefreshExpiry != "" {
		if d, err := time.ParseDuration(cfg.Auth.RefreshExpiry); err == nil {
			authConfig.RefreshExpiry = d
		}
	}
	g.auth = NewAuthHandlerWithConfig(authConfig)
	provider := newGatewayAuthProvider(g.auth, g.iamBridge)
	g.authProvider = provider
	g.permChecker = provider

	if cfg.RateLimit.Enabled {
		apiLimits := make(map[string]ratelimit.APILimit)
		for name, limit := range cfg.RateLimit.APILimits {
			apiLimits[name] = ratelimit.APILimit{
				RPS:   limit.RPS,
				Burst: limit.Burst,
			}
		}

		mlCfg := &ratelimit.MultiLevelConfig{
			GlobalRPS:         cfg.RateLimit.GlobalRPS,
			GlobalBurst:       cfg.RateLimit.GlobalBurst,
			IPRPS:             cfg.RateLimit.IPRPS,
			IPBurst:           cfg.RateLimit.IPBurst,
			UserRPS:           cfg.RateLimit.UserRPS,
			UserBurst:         cfg.RateLimit.UserBurst,
			BucketRPS:         cfg.RateLimit.BucketRPS,
			BucketBurst:       cfg.RateLimit.BucketBurst,
			UploadBytesPerSec: cfg.RateLimit.UploadBytesPerSec,
			UploadBurstBytes:  cfg.RateLimit.UploadBurstBytes,
			APILimits:         apiLimits,
			Whitelist:         cfg.RateLimit.Whitelist,
		}
		g.rateLimiter = ratelimit.NewMultiLevelLimiter(mlCfg)
	}

	if cfg.Cache.ObjectMaxBytes > 0 {
		objectCache, err := cache.NewObjectCache(cfg.Cache.ObjectMaxBytes, cfg.Cache.TTL)
		if err != nil {
			return fmt.Errorf("failed to create object cache: %w", err)
		}
		g.objectCache = objectCache
	}

	if cfg.Cache.MetadataMaxBytes > 0 {
		g.metaCache = cache.NewMetadataCache(cfg.Cache.TTL)
	}

	if cfg.Events.Enabled {
		webhookTimeout := 5 * time.Second
		if cfg.Events.WebhookTimeout != "" {
			if d, err := time.ParseDuration(cfg.Events.WebhookTimeout); err == nil {
				webhookTimeout = d
			}
		}
		dlqDir := cfg.Events.DeadLetterDir
		if dlqDir == "" {
			dlqDir = cfg.Node.DataDir + "/deadletter"
		}
		bus := events.NewEventBus(
			cfg.Events.Workers,
			webhookTimeout,
			dlqDir,
			cfg.Events.MaxRetries,
			cfg.Events.RetryBaseMS,
		)
		bus.Start()
		g.eventBus = bus
	}

	// Initialize FTS (Full-Text Search)
	if cfg.FTS.Enabled {
		ftsDataDir := cfg.FTS.DataDir
		if ftsDataDir == "" {
			ftsDataDir = cfg.Node.DataDir + "/fts"
		}
		ftsDBPath := ftsDataDir + "/fts.db"
		ftsIndex, err := fts.NewInvertedIndex(ftsDBPath)
		if err != nil {
			return fmt.Errorf("failed to create FTS index: %w", err)
		}
		// Apply BM25 parameters from config
		if cfg.FTS.BM25K1 > 0 {
			ftsIndex.GetScorer().K1 = cfg.FTS.BM25K1
		}
		if cfg.FTS.BM25B > 0 {
			ftsIndex.GetScorer().B = cfg.FTS.BM25B
		}
		// Apply disk quota
		if cfg.FTS.MaxIndexSize != "" {
			if maxSize, err := units.ParseSize(cfg.FTS.MaxIndexSize); err == nil && maxSize > 0 {
				ftsIndex.SetCompactionMaxIndexSize(maxSize)
			}
		}
		g.ftsIndex = ftsIndex
	}

	// Initialize resumable upload handler
	if cfg.Resumable.Enabled {
		g.resumableHandler = NewResumableUploadHandler(g)
		cleanupInterval := 5 * time.Minute
		if cfg.Resumable.CleanupInterval != "" {
			if d, err := time.ParseDuration(cfg.Resumable.CleanupInterval); err == nil {
				cleanupInterval = d
			}
		}
		g.resumableCleanup = NewResumableCleanup(g.resumableHandler, cleanupInterval)
		g.resumableCleanup.Start()
	}

	// Initialize domain services (must be done before task queue,
	// because task queue handlers reference these services).
	g.bucketSvc = NewBucketService(g)
	g.objectSvc = NewObjectService(g)
	g.searchSvc = NewSearchService(g)
	g.multipartSvc = NewMultipartService(g)

	// Initialize background task queue.
	if err := g.initializeTaskQueue(cfg); err != nil {
		return fmt.Errorf("failed to initialize task queue: %w", err)
	}

	return nil
}

func (g *S3Gateway) initializeTaskQueue(cfg *config.Config) error {
	if !cfg.TaskQueue.Enabled {
		return nil
	}

	storePath := cfg.TaskQueue.StorePath
	if storePath == "" {
		storePath = cfg.Node.DataDir + "/tasks.db"
	}
	if !filepath.IsAbs(storePath) {
		storePath = filepath.Join(cfg.Node.DataDir, storePath)
	}

	store, err := taskqueue.NewBoltStore(storePath)
	if err != nil {
		return fmt.Errorf("failed to create task store: %w", err)
	}

	workers := cfg.TaskQueue.Workers
	if workers <= 0 {
		workers = 4
	}

	queueOpts := []taskqueue.QueueOption{
		taskqueue.WithPollInterval(parseDuration(cfg.TaskQueue.PollInterval, 500*time.Millisecond)),
		taskqueue.WithRetryBase(parseDuration(cfg.TaskQueue.RetryBase, time.Second)),
		taskqueue.WithMaxRetryDelay(parseDuration(cfg.TaskQueue.MaxRetryDelay, 5*time.Minute)),
	}

	g.taskQueue = taskqueue.NewQueue(store, workers, queueOpts...)

	// Register background handlers.
	g.taskQueue.Register(taskqueue.KindVectorize, g.searchSvc.handleVectorizeTask)
	g.taskQueue.Register(taskqueue.KindFTS, g.searchSvc.handleFTSTask)
	g.taskQueue.Register(taskqueue.KindPipeline, g.handlePipelineTask)

	g.taskQueue.Start()
	return nil
}

func parseDuration(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return fallback
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	return fallback
}

// convertMilvusConfig 将 config 层的 MilvusConfig 转换为 vector 层的 MilvusConfig。
func convertMilvusConfig(cfg *config.MilvusConfig) *vector.MilvusConfig {
	if cfg == nil {
		return nil
	}
	mc := &vector.MilvusConfig{
		Address:          cfg.Address,
		Username:         cfg.Username,
		Password:         cfg.Password,
		DBName:           cfg.DBName,
		CollectionName:   cfg.CollectionName,
		ShardsNum:        cfg.ShardsNum,
		IndexType:        cfg.IndexType,
		IndexParams:      cfg.IndexParams,
		NPROBE:           cfg.NPROBE,
		EF:               cfg.EF,
		ConsistencyLevel: cfg.ConsistencyLevel,
		ReplicaNumber:    cfg.ReplicaNumber,
	}
	if cfg.TieredStorage != nil {
		mc.TieredStorage = &vector.TieredStorageConfig{
			Enabled:           cfg.TieredStorage.Enabled,
			HotResourceGroup:  cfg.TieredStorage.HotResourceGroup,
			ColdResourceGroup: cfg.TieredStorage.ColdResourceGroup,
			HotNodes:          cfg.TieredStorage.HotNodes,
			ColdNodes:         cfg.TieredStorage.ColdNodes,
			WarmUp:            cfg.TieredStorage.WarmUp,
		}
	}
	if cfg.DiskANN != nil {
		mc.DiskANN = &vector.DiskANNConfig{
			MaxDegree:                cfg.DiskANN.MaxDegree,
			SearchListSize:           cfg.DiskANN.SearchListSize,
			PQCodeBudgetGBRatio:      cfg.DiskANN.PQCodeBudgetGBRatio,
			SearchCacheBudgetGBRatio: cfg.DiskANN.SearchCacheBudgetGBRatio,
			BeamWidthRatio:           cfg.DiskANN.BeamWidthRatio,
		}
	}
	return mc
}

// convertVSEConfig 将 config 层的 VSEMapConfig 转换为 vector 层的 VSEConfig。
func convertVSEConfig(cfg *config.VSEMapConfig) *vector.VSEConfig {
	if cfg == nil {
		return nil
	}
	vcfg := &vector.VSEConfig{
		DataDir:         cfg.DataDir,
		MaxHotSegments:  cfg.MaxHotSegments,
		MaxColdSegments: cfg.MaxColdSegments,
		HotSegmentSize:  cfg.HotSegmentSize,
		ColdSegmentSize: cfg.ColdSegmentSize,
		IVFCentroids:    cfg.IVFCentroids,
		IVFNProbe:       cfg.IVFNProbe,
		PQSubQuantizers: cfg.PQSubQuantizers,
		PQBits:          cfg.PQBits,
		HNSWM:           cfg.HNSWM,
		HNSWEfSearch:    cfg.HNSWEfSearch,
		SearchWorkers:   cfg.SearchWorkers,
		GPUAccel:        cfg.GPUAccel,
		GPUBatchMin:     cfg.GPUBatchMin,
		QuantizerType:   cfg.QuantizerType,
		CacheSize:       cfg.CacheSize,
		AutoMerge:       cfg.AutoMerge,
		MergeInterval:   cfg.MergeInterval,
		EnableS3:        cfg.EnableS3,
		S3Endpoint:      cfg.S3Endpoint,
		S3Region:        cfg.S3Region,
		S3Bucket:        cfg.S3Bucket,
		S3AccessKey:     cfg.S3AccessKey,
		S3SecretKey:     cfg.S3SecretKey,
	}

	if cfg.MergePolicy != nil {
		mergeInterval, _ := time.ParseDuration(cfg.MergePolicy.MergeInterval)
		if mergeInterval <= 0 {
			mergeInterval = 5 * time.Minute
		}
		idleThreshold, _ := time.ParseDuration(cfg.MergePolicy.IdleThreshold)
		if idleThreshold <= 0 {
			idleThreshold = 30 * time.Minute
		}
		vcfg.MergePolicy = vse.MergePolicy{
			HotTargetSize:   cfg.MergePolicy.HotTargetSize,
			HotMaxSegments:  cfg.MergePolicy.HotMaxSegments,
			ColdTargetSize:  cfg.MergePolicy.ColdTargetSize,
			ColdMaxSegments: cfg.MergePolicy.ColdMaxSegments,
			MinVectorsMerge: cfg.MergePolicy.MinVectorsMerge,
			MergeInterval:   mergeInterval,
			IdleThreshold:   idleThreshold,
		}
	} else {
		vcfg.MergePolicy = vse.DefaultMergePolicy()
	}

	if cfg.WarmupPolicy != nil {
		batchInterval, _ := time.ParseDuration(cfg.WarmupPolicy.BatchInterval)
		if batchInterval <= 0 {
			batchInterval = 100 * time.Millisecond
		}
		vcfg.WarmupPolicy = vse.WarmupPolicy{
			Enabled:          cfg.WarmupPolicy.Enabled,
			BatchSize:        cfg.WarmupPolicy.BatchSize,
			BatchInterval:    batchInterval,
			PreloadCentroids: cfg.WarmupPolicy.PreloadCentroids,
			PreloadPQ:        cfg.WarmupPolicy.PreloadPQ,
			MaxPreloadBytes:  cfg.WarmupPolicy.MaxPreloadBytes,
		}
	} else {
		vcfg.WarmupPolicy = vse.DefaultWarmupPolicy()
	}

	return vcfg
}

func (g *S3Gateway) Handler() http.Handler {
	mux := http.NewServeMux()

	// Health check endpoints
	if g.healthHandler != nil {
		mux.HandleFunc("/healthz", g.healthHandler.HealthzHandler())
		mux.HandleFunc("/readyz", g.healthHandler.ReadyzHandler())
		mux.HandleFunc("/livez", g.healthHandler.LivezHandler())
	}

	// Prometheus metrics endpoint
	metricsPath := "/metrics"
	if g.config != nil && g.config.Observability.MetricsPath != "" {
		metricsPath = g.config.Observability.MetricsPath
	}
	mux.Handle(metricsPath, observability.MetricsHandler())

	// FTS search endpoint
	mux.HandleFunc("/admin/fts/search", g.searchSvc.handleAdminFTSSearch)

	mux.HandleFunc("/", g.handleRequest)

	var handler http.Handler = mux

	// Apply tracing middleware
	if g.config != nil && g.config.Observability.TracingEnabled {
		handler = observability.TracingHTTPMiddleware(handler)
	}

	// Apply metrics HTTP middleware
	if g.metrics != nil {
		handler = g.metrics.HTTPMiddleware(handler)
	}

	return handler
}

func (g *S3Gateway) Start(addr string) error {
	g.server = &http.Server{
		Addr:    addr,
		Handler: g.Handler(),
	}

	return g.server.ListenAndServe()
}

func (g *S3Gateway) Stop() error {
	if g.server != nil {
		return g.server.Close()
	}
	return nil
}

func (g *S3Gateway) Close() error {
	if g.taskQueue != nil {
		if err := g.taskQueue.Stop(context.Background()); err != nil {
			zap.L().Warn("failed to stop task queue gracefully", zap.Error(err))
		}
	}
	if g.resumableCleanup != nil {
		g.resumableCleanup.Stop()
	}
	if g.tracerShutdown != nil {
		g.tracerShutdown(context.Background())
	}
	if g.eventBus != nil {
		g.eventBus.Stop()
	}
	if g.ftsIndex != nil {
		g.ftsIndex.Close()
	}
	if g.accessLog != nil {
		return g.accessLog.Close()
	}
	return nil
}

func (g *S3Gateway) GetMetadataStore() *metadata.BoltDBMetadataStore {
	return g.metadata
}

func (g *S3Gateway) GetStore() *storage.TieredObjectStore {
	return g.store
}

func (g *S3Gateway) GetTieringManager() *tiering.TieringManager {
	return g.tiering
}

func (g *S3Gateway) GetVectorManager() *vector.VectorManager {
	return g.vector
}

func (g *S3Gateway) GetFTSIndex() *fts.InvertedIndex {
	return g.ftsIndex
}

func (g *S3Gateway) GetPipelineExecutor() *pipeline.PipelineExecutor {
	return g.pipeline
}

func (g *S3Gateway) GetAuth() *AuthHandler {
	return g.auth
}

func (g *S3Gateway) SetIAMBridge(bridge *IAMAuthBridge) {
	g.iamBridge = bridge
	provider := newGatewayAuthProvider(g.auth, g.iamBridge)
	g.authProvider = provider
	g.permChecker = provider
}

func (g *S3Gateway) GetIAMBridge() *IAMAuthBridge {
	return g.iamBridge
}

func (g *S3Gateway) GetEventBus() *events.EventBus {
	return g.eventBus
}
