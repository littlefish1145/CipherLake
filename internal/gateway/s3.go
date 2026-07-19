package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"hash/crc32"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"cipherlake/internal/auth"
	"cipherlake/internal/bootstrap"
	"cipherlake/internal/cache"
	"cipherlake/internal/common"
	"cipherlake/internal/config"
	"cipherlake/internal/events"
	"cipherlake/internal/fts"
	"cipherlake/internal/logger"
	"cipherlake/internal/metadata"
	"cipherlake/internal/observability"
	"cipherlake/internal/pipeline"
	"cipherlake/internal/ratelimit"
	"cipherlake/internal/services"
	"cipherlake/internal/storage"
	"cipherlake/internal/taskqueue"
	"cipherlake/internal/tiering"
	"cipherlake/internal/units"
	"cipherlake/internal/vector"
	pluginpb "cipherlake/proto/plugin"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	bucketNameRegex         = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	pathTraversalPattern    = regexp.MustCompile(`(?:^|/)\.\.(?:$|/)`)
	controlCharPattern      = regexp.MustCompile(`[\x00-\x1f]`)
	encodedTraversalPattern = regexp.MustCompile(`(?i)(?:%2e%2e|%2e\.|\.\.%2e)`)

	maxRequestBodyBytes int64 = 50 * 1024 * 1024
)

type S3Gateway struct {
	mu                sync.RWMutex
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
	multipartCleanup  *MultipartCleanup
	bucketSvc         *BucketService
	objectSvc         *ObjectService
	searchSvc         *SearchService
	multipartSvc      *MultipartService
	taskQueue         *taskqueue.Queue
	writeTasks        objectWriteTaskScheduler
	loaderClient      pluginpb.PluginLoaderServiceClient
	loaderConn        *grpc.ClientConn
	closeOnce         sync.Once
	closeErr          error
}

type BucketState struct {
	Name        string
	CreatedAt   time.Time
	ObjectCount int64
	TotalSize   int64
}

type ListObjectsV2Output struct {
	XMLName               xml.Name               `xml:"ListBucketResult"`
	Name                  string                 `xml:"Name"`
	Prefix                string                 `xml:"Prefix"`
	StartAfter            string                 `xml:"StartAfter,omitempty"`
	ContinuationToken     string                 `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string                 `xml:"NextContinuationToken,omitempty"`
	KeyCount              int                    `xml:"KeyCount"`
	MaxKeys               int                    `xml:"MaxKeys"`
	Delimiter             string                 `xml:"Delimiter,omitempty"`
	IsTruncated           bool                   `xml:"IsTruncated"`
	Contents              []ListObjectsV2Content `xml:"Contents,omitempty"`
	CommonPrefixes        []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes,omitempty"`
}

type ListObjectsV2Content struct {
	Key          string    `xml:"Key"`
	LastModified time.Time `xml:"LastModified"`
	ETag         string    `xml:"ETag"`
	Size         int64     `xml:"Size"`
	StorageClass string    `xml:"StorageClass"`
	Owner        *struct {
		ID          string `xml:"ID"`
		DisplayName string `xml:"DisplayName"`
	} `xml:"Owner,omitempty"`
}

type ListObjectsOutput struct {
	XMLName        xml.Name               `xml:"ListBucketResult"`
	Name           string                 `xml:"Name"`
	Prefix         string                 `xml:"Prefix"`
	Marker         string                 `xml:"Marker"`
	MaxKeys        int                    `xml:"MaxKeys"`
	Delimiter      string                 `xml:"Delimiter,omitempty"`
	IsTruncated    bool                   `xml:"IsTruncated"`
	NextMarker     string                 `xml:"NextMarker,omitempty"`
	Contents       []ListObjectsV2Content `xml:"Contents,omitempty"`
	CommonPrefixes []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes,omitempty"`
}

type ListBucketsOutput struct {
	XMLName xml.Name `xml:"ListAllMyBucketsResult"`
	Owner   struct {
		ID          string `xml:"ID"`
		DisplayName string `xml:"DisplayName"`
	} `xml:"Owner"`
	Buckets []struct {
		Bucket struct {
			Name         string    `xml:"Name"`
			CreationDate time.Time `xml:"CreationDate"`
		} `xml:"Bucket"`
	} `xml:"Buckets>Bucket"`
}

type ErrorResponse struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	RequestID string   `xml:"RequestId"`
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
		return nil, errors.Join(fmt.Errorf("failed to initialize components: %w", err), gateway.Close())
	}

	accessLogDir := cfg.Logging.AccessLogDir
	if accessLogDir == "" {
		accessLogDir = cfg.Node.DataDir + "/logs"
	}
	accessLogger, err := NewAccessLogger(accessLogDir, 0)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("failed to create access logger: %w", err), gateway.Close())
	}
	gateway.accessLog = accessLogger

	return gateway, nil
}

func (g *S3Gateway) initializeStores(cfg *config.Config) error {
	stores, err := newGatewayStores(cfg)
	if err != nil {
		return err
	}
	g.metadata = stores.metadata
	g.store = stores.store
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
		// 全面转向 Milvus:传递 Milvus 配置
		if cfg.Vector.Milvus != nil {
			vectorConfig.Milvus = convertMilvusConfig(cfg.Vector.Milvus)
		}
		vectorManager, err := vector.NewVectorManager(vectorConfig)
		if err != nil {
			return fmt.Errorf("failed to create vector manager: %w", err)
		}
		g.vector = vectorManager
	}

	pipelineExecutor := pipeline.NewPipelineExecutor(cfg.Pipelines.MaxConcurrent)
	if err := pipeline.RegisterDefaultPlugins(pipelineExecutor); err != nil {
		return fmt.Errorf("failed to register default plugins: %w", err)
	}
	if cfg.Pipelines.ConfigFile != "" {
		if err := pipelineExecutor.LoadConfig(cfg.Pipelines.ConfigFile); err != nil {
			logger.Warn("Failed to load pipeline config", zap.String("path", cfg.Pipelines.ConfigFile), zap.Error(err))
		}
	}
	g.pipeline = pipelineExecutor

	authRuntime := newGatewayAuthRuntime(cfg, g.iamBridge)
	g.auth = authRuntime.handler
	g.authProvider = authRuntime.provider
	g.permChecker = authRuntime.permissions

	g.rateLimiter = newGatewayRateLimiter(cfg)

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

	g.writeTasks = newGatewayObjectWriteTaskScheduler(g)

	// Initialize domain services (must be done before task queue,
	// because task queue handlers reference these services).
	g.bucketSvc = NewBucketService(g)
	g.objectSvc = NewObjectService(g)
	g.searchSvc = NewSearchService(g)
	g.multipartSvc = NewMultipartService(g)
	g.multipartCleanup = NewMultipartCleanup(g.multipartSvc.handler, 5*time.Minute)

	// Initialize background task queue.
	if err := g.initializeTaskQueue(cfg); err != nil {
		return fmt.Errorf("failed to initialize task queue: %w", err)
	}
	g.multipartCleanup.Start()

	// Initialize plugin-loader gRPC client if configured.
	if cfg.PluginLoader.GRPCListenAddr != "" {
		conn, err := grpc.NewClient(cfg.PluginLoader.GRPCListenAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return fmt.Errorf("failed to connect to plugin loader: %w", err)
		}
		g.loaderConn = conn
		g.loaderClient = pluginpb.NewPluginLoaderServiceClient(conn)
	}

	return nil
}

func (g *S3Gateway) initializeTaskQueue(cfg *config.Config) error {
	queue, err := newGatewayTaskQueue(cfg, gatewayTaskQueueHandlers{
		vectorize: g.searchSvc.handleVectorizeTask,
		fts:       g.searchSvc.handleFTSTask,
		pipeline:  g.handlePipelineTask,
	})
	if err != nil {
		return err
	}
	g.taskQueue = queue
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

	// Plugin routes: /_plugins/<plugin_name>/* are dispatched to the
	// plugin-loader microservice (spec §3.4).
	if g.loaderClient != nil {
		mux.HandleFunc("/_plugins/", g.handlePluginRoute)
	}

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

func (g *S3Gateway) handleRequest(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	requestID := uuid.New().String()
	ctx := r.Context()
	ctx = common.WithRequestID(ctx, requestID)
	r = r.WithContext(ctx)

	w.Header().Set("x-amz-request-id", requestID)
	w.Header().Set("x-amz-id-2", uuid.New().String()[:16])

	// Detect virtual-hosted-style: bucket name embedded in Host header.
	// e.g. Host: mybucket.example.com:8080 with domain "example.com" → bucket="mybucket"
	var vhostBucket string
	if g.config != nil {
		vhostBucket = extractVirtualHostedBucket(r.Host, g.config.Node.Domain)
	}

	// Determine bucket and rewrite path for virtual-hosted-style requests.
	// For vhost-style, the URL path is the object key (no bucket prefix),
	// so we rewrite r.URL.Path to /<bucket>/<key> so downstream code works unchanged.
	if vhostBucket != "" {
		originalPath := r.URL.Path
		r.URL.Path = "/" + vhostBucket + originalPath
		// Store original path so access log can reference it
		ctx = context.WithValue(r.Context(), ctxKeyOriginalPath{}, originalPath)
		r = r.WithContext(ctx)
	}

	// CORS needs the resolved bucket name
	corsBucket := vhostBucket
	g.setCORSHeaders(w, r, corsBucket)

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	if g.rateLimiter != nil {
		userID := g.getUserID(r)
		ip := extractIP(r)
		bucket := vhostBucket
		if bucket == "" {
			if len(r.URL.Path) > 1 {
				parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
				if len(parts) > 0 {
					bucket = parts[0]
				}
			}
		}
		contentLength, _ := strconv.ParseInt(r.Header.Get("Content-Length"), 10, 64)
		result := g.rateLimiter.Allow(ctx, ip, userID, bucket, r.Method, contentLength)
		if !result.Allowed {
			retryAfter := fmt.Sprintf("%d", result.RetryAfter)
			if result.RetryAfter <= 0 {
				retryAfter = "1"
			}
			w.Header().Set("Retry-After", retryAfter)
			w.Header().Set("X-RateLimit-Limit-Type", result.LimitType)
			g.writeError(w, http.StatusTooManyRequests, "SlowDown",
				fmt.Sprintf("Rate limit exceeded (%s). Please retry after %ss.", result.LimitType, retryAfter))
			return
		}
	}

	path := r.URL.Path
	method := r.Method

	if len(path) > 1024 {
		g.writeError(w, http.StatusBadRequest, "InvalidURI", "Object key too long")
		return
	}

	if pathTraversalPattern.MatchString(path) || controlCharPattern.MatchString(path) || encodedTraversalPattern.MatchString(r.URL.RawPath) || encodedTraversalPattern.MatchString(r.URL.RawQuery) {
		g.writeError(w, http.StatusBadRequest, "InvalidURI", "Invalid characters in path")
		return
	}

	var handler func(w http.ResponseWriter, r *http.Request) error

	if path == "/" {
		if method == "GET" {
			if _, err := g.requireIdentity(r, auth.ActionRead, "", ""); err != nil {
				g.writeError(w, http.StatusUnauthorized, "AccessDenied", err.Error())
				return
			}
			handler = g.bucketSvc.handleListBuckets
		} else if method == "POST" {
			if _, err := g.requireIdentity(r, auth.ActionWrite, "", ""); err != nil {
				g.writeError(w, http.StatusUnauthorized, "AccessDenied", err.Error())
				return
			}
			handler = g.handlePOST
		}
	} else if len(path) > 1 {
		parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)
		bucket := parts[0]

		if !g.validateBucketName(bucket) {
			g.writeError(w, http.StatusBadRequest, "InvalidBucketName", "Bucket name must be between 3-63 characters, lowercase letters, numbers, hyphens, and periods")
			return
		}

		if len(parts) == 1 || (len(parts) == 2 && parts[1] == "") {
			handler = g.handleBucketOperations(bucket, method)
		} else if len(parts) == 2 {
			key := parts[1]
			if !g.validateObjectKey(key) {
				g.writeError(w, http.StatusBadRequest, "InvalidKey", "Object key contains invalid characters")
				return
			}
			handler = g.handleObjectOperations(bucket, key, method)
		}
	}

	if handler == nil {
		g.writeError(w, http.StatusNotFound, "NoSuchResource", "The specified resource does not exist.")
		return
	}

	recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	if err := handler(recorder, r); err != nil {
		// If the handler already wrote a response (e.g. via writeError inside
		// the handler before returning a wrapped error), do not attempt to
		// write another status — just log.
		if !recorder.written {
			status, code, message := g.classifyHandlerError(err)
			g.writeError(w, status, code, message)
			recorder.status = status
			recorder.written = true
		} else {
			zap.L().Warn("handler returned error after writing response",
				zap.String("method", method),
				zap.String("path", path),
				zap.Error(err))
		}
	}

	if g.accessLog != nil {
		parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)
		bucket := ""
		key := ""
		if len(parts) >= 1 {
			bucket = parts[0]
		}
		if len(parts) >= 2 {
			key = parts[1]
		}
		g.accessLog.Log(AccessLogEntry{
			RemoteIP:   getClientIP(r),
			UserID:     g.getUserID(r),
			Operation:  method,
			Bucket:     bucket,
			Key:        key,
			StatusCode: recorder.status,
			BytesSent:  recorder.bytesWritten,
			RequestID:  requestID,
			Latency:    time.Since(startTime),
		})
	}
}

// statusRecorder wraps http.ResponseWriter to capture the status code and
// bytes written for access logging. It is safe for use as the ResponseWriter
// passed into handlers.
type statusRecorder struct {
	http.ResponseWriter
	status       int
	written      bool
	bytesWritten int64
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.written {
		return
	}
	r.status = code
	r.written = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.written {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	if n > 0 {
		r.bytesWritten += int64(n)
	}
	return n, err
}

// classifyHandlerError maps a handler-returned error to the appropriate S3
// HTTP status, error code, and user-visible message. Metadata sentinel
// errors are unwrapped via errors.Is so that handlers can wrap them with
// additional context without losing the semantic classification.
func (g *S3Gateway) classifyHandlerError(err error) (int, string, string) {
	switch {
	case errors.Is(err, metadata.ErrKeyNotFound):
		return http.StatusNotFound, "NoSuchKey", "The specified key does not exist."
	case errors.Is(err, metadata.ErrBucketNotFound):
		return http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist."
	case errors.Is(err, metadata.ErrBucketAlreadyExists):
		return http.StatusConflict, "BucketAlreadyExists", "The requested bucket name is not available."
	case errors.Is(err, metadata.ErrBucketNotEmpty):
		return http.StatusConflict, "BucketNotEmpty", "The bucket you tried to delete is not empty."
	case errors.Is(err, metadata.ErrInvalidKey):
		return http.StatusBadRequest, "InvalidKey", "The specified key is not valid."
	case errors.Is(err, metadata.ErrVersionNotFound):
		return http.StatusNotFound, "NoSuchVersion", "The specified version does not exist."
	case errors.Is(err, metadata.ErrQuotaExceeded):
		return http.StatusTooManyRequests, "QuotaExceeded", "The bucket quota has been exceeded."
	case errors.Is(err, metadata.ErrTransactionFailed):
		return http.StatusServiceUnavailable, "TransactionFailed", "The metadata transaction could not be committed."
	default:
		// Heuristics for handler-returned errors that don't use sentinel
		// wrapping but follow established prefix conventions.
		msg := err.Error()
		if strings.HasPrefix(msg, "access denied") {
			return http.StatusForbidden, "AccessDenied", msg
		}
		if strings.HasPrefix(msg, "method not allowed") {
			return http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed against this resource."
		}
		if strings.HasPrefix(msg, "uploadId is required") || strings.HasPrefix(msg, "missing") {
			return http.StatusBadRequest, "InvalidRequest", msg
		}
		zap.L().Error("unclassified handler error", zap.Error(err))
		return http.StatusInternalServerError, "InternalError", "An internal error occurred"
	}
}

// ctxKeyOriginalPath is the context key for the original URL path before
// virtual-hosted-style rewriting.
type ctxKeyOriginalPath struct{}

func (g *S3Gateway) setCORSHeaders(w http.ResponseWriter, r *http.Request, bucket string) {
	origin := r.Header.Get("Origin")
	if origin != "" {
		bucketInfo, err := g.metadata.GetBucket(r.Context(), bucket)
		if err == nil && bucketInfo != nil && bucketInfo.CORS != nil && len(bucketInfo.CORS.AllowedOrigins) > 0 {
			for _, allowed := range bucketInfo.CORS.AllowedOrigins {
				if allowed == "*" || allowed == origin {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					break
				}
			}
		}
	}
	if w.Header().Get("Access-Control-Allow-Origin") == "" {
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, HEAD, OPTIONS, PATCH")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Content-Length, x-amz-content-sha256, x-amz-date, x-amz-security-token, x-amz-user-agent, x-amz-meta-*, x-amz-acl, x-amz-copy-source, x-amz-tagging, x-amz-server-side-encryption, x-amz-server-side-encryption-customer-algorithm, x-amz-server-side-encryption-customer-key, x-amz-server-side-encryption-customer-key-MD5, x-amz-checksum-crc32c, x-amz-checksum-sha256, x-amz-checksum-md5, x-amz-checksum-mode, X-Amz-Algorithm, X-Amz-Credential, X-Amz-Date, X-Amz-Expires, X-Amz-SignedHeaders, X-Amz-Signature, amz-sdk-invocation-id, amz-sdk-request, amz-sdk-retry, X-CipherLake-Resumable, X-CipherLake-Finalize, Upload-Offset, Upload-Length, Upload-Checksum")
	w.Header().Set("Access-Control-Max-Age", "3600")
	w.Header().Set("Access-Control-Expose-Headers", "ETag, X-Amz-Version-Id, X-Amz-Request-Id, X-Amz-Expiration, X-Amz-Checksum-CRC32C, X-Amz-Checksum-SHA256, X-Amz-Checksum-MD5, x-amz-server-side-encryption-customer-algorithm, x-amz-server-side-encryption-customer-key-MD5, X-CipherLake-Upload-Id, Upload-Offset, Upload-Length, Upload-Checksum")
}

func (g *S3Gateway) validateBucketName(name string) bool {
	if len(name) < 3 || len(name) > 63 {
		return false
	}
	if !bucketNameRegex.MatchString(name) {
		return false
	}
	if strings.HasPrefix(name, "xn--") {
		return false
	}
	if strings.HasSuffix(name, "-s3alias") {
		return false
	}
	return true
}

func (g *S3Gateway) validateObjectKey(key string) bool {
	if len(key) > 1024 {
		return false
	}
	if pathTraversalPattern.MatchString(key) {
		return false
	}
	if controlCharPattern.MatchString(key) {
		return false
	}
	return true
}

// extractVirtualHostedBucket extracts the bucket name from a virtual-hosted-style
// request. Given Host "mybucket.example.com:8080" and domains "example.com,localhost",
// it returns "mybucket". Returns empty string if the Host does not match the
// virtual-hosted pattern. The domain parameter supports comma-separated values.
func extractVirtualHostedBucket(host, domains string) string {
	if domains == "" {
		return ""
	}
	// Strip port
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	h = strings.ToLower(h)

	// Try each domain (comma-separated)
	for _, domain := range strings.Split(domains, ",") {
		domain = strings.TrimSpace(strings.ToLower(domain))
		if domain == "" {
			continue
		}
		if !strings.HasSuffix(h, "."+domain) {
			continue
		}
		bucket := strings.TrimSuffix(h, "."+domain)
		if bucket == "" {
			continue
		}
		if bucketNameRegex.MatchString(bucket) {
			return bucket
		}
	}
	return ""
}

func (g *S3Gateway) validateContentType(contentType string) bool {
	// S3-compatible storage accepts any content type.
	// Only block explicitly dangerous types.
	if contentType == "" {
		return true
	}
	blocked := []string{
		"application/x-msdos-program",
		"application/x-msdownload",
	}
	lower := strings.ToLower(contentType)
	for _, b := range blocked {
		if strings.HasPrefix(lower, b) {
			return false
		}
	}
	return true
}

func (g *S3Gateway) handlePOST(w http.ResponseWriter, r *http.Request) error {
	if r.Method != "POST" {
		return fmt.Errorf("method not allowed")
	}

	query := r.URL.Query()
	if query.Has("vector_search") {
		return g.searchSvc.handleVectorSearch(w, r)
	}

	if query.Has("fts_search") {
		return g.searchSvc.handleFTSSearch(w, r)
	}

	// Resumable upload: POST /{bucket}/{key}?resumable creates a session
	if query.Has("resumable") && g.resumableHandler != nil {
		path := r.URL.Path
		parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)
		bucket := ""
		key := ""
		if len(parts) >= 1 {
			bucket = parts[0]
		}
		if len(parts) >= 2 {
			key = parts[1]
		}
		if bucket == "" || key == "" {
			return fmt.Errorf("bucket and key are required for resumable upload")
		}
		return g.resumableHandler.HandleCreateSession(w, r, bucket, key)
	}

	// Resumable upload finalize: POST /{bucket}/{key}?uploadId=...&finalize=1
	if query.Has("uploadId") && query.Has("finalize") && g.resumableHandler != nil {
		path := r.URL.Path
		parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)
		bucket := ""
		key := ""
		if len(parts) >= 1 {
			bucket = parts[0]
		}
		if len(parts) >= 2 {
			key = parts[1]
		}
		return g.resumableHandler.HandleFinalize(w, r, bucket, key)
	}

	if query.Has("uploads") {
		bucket := query.Get("bucket")
		key := query.Get("key")
		if bucket == "" {
			path := r.URL.Path
			if len(path) > 1 {
				parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)
				if len(parts) >= 1 {
					bucket = parts[0]
				}
				if len(parts) >= 2 {
					key = parts[1]
				}
			}
		}
		return g.multipartSvc.HandleCreateMultipartUpload(w, r, bucket, key)
	}

	return fmt.Errorf("unsupported POST operation")
}

func (g *S3Gateway) handleBucketOperations(bucket, method string) func(w http.ResponseWriter, r *http.Request) error {
	return func(w http.ResponseWriter, r *http.Request) error {
		query := r.URL.Query()

		switch method {
		case "PUT":
			if query.Has("acl") {
				if _, err := g.requireIdentity(r, auth.ActionAdmin, bucket, ""); err != nil {
					return fmt.Errorf("access denied: %w", err)
				}
				return g.bucketSvc.handlePutBucketAcl(w, r, bucket)
			}
			if _, err := g.requireIdentity(r, auth.ActionWrite, bucket, ""); err != nil {
				return fmt.Errorf("access denied: %w", err)
			}
			return g.bucketSvc.handleCreateBucket(w, r, bucket)
		case "DELETE":
			if _, err := g.requireIdentity(r, auth.ActionAdmin, bucket, ""); err != nil {
				return fmt.Errorf("access denied: %w", err)
			}
			return g.bucketSvc.handleDeleteBucket(w, r, bucket)
		case "HEAD":
			if _, err := g.requireIdentity(r, auth.ActionRead, bucket, ""); err != nil {
				return fmt.Errorf("access denied: %w", err)
			}
			return g.bucketSvc.handleHeadBucket(w, r, bucket)
		case "POST":
			if query.Has("delete") {
				if _, err := g.requireIdentity(r, auth.ActionDelete, bucket, ""); err != nil {
					return fmt.Errorf("access denied: %w", err)
				}
				return g.objectSvc.handleDeleteObjects(w, r, bucket)
			}
			return fmt.Errorf("unsupported POST operation on bucket")
		case "GET":
			if query.Has("location") {
				return g.bucketSvc.handleGetBucketLocation(w, r, bucket)
			}
			if query.Has("fts_search") {
				return g.searchSvc.handleFTSSearch(w, r)
			}
			if query.Has("acl") {
				if _, err := g.requireIdentity(r, auth.ActionRead, bucket, ""); err != nil {
					return fmt.Errorf("access denied: %w", err)
				}
				return g.bucketSvc.handleGetBucketAcl(w, r, bucket)
			}
			if query.Has("versioning") {
				return g.bucketSvc.handleGetBucketVersioning(w, r, bucket)
			}
			if query.Has("uploads") {
				return g.multipartSvc.HandleListUploads(w, r, bucket)
			}
			if !g.isBucketPublicRead(r, bucket) {
				if _, err := g.requireIdentity(r, auth.ActionRead, bucket, ""); err != nil {
					return fmt.Errorf("access denied: %w", err)
				}
			}
			return g.bucketSvc.handleListObjects(w, r, bucket)
		default:
			return fmt.Errorf("method not allowed")
		}
	}
}

func (g *S3Gateway) handleObjectOperations(bucket, key, method string) func(w http.ResponseWriter, r *http.Request) error {
	return func(w http.ResponseWriter, r *http.Request) error {
		query := r.URL.Query()

		// Resumable upload routing: when uploadId is present and resumable handler is active,
		// route to resumable flow. When X-CipherLake-Resumable header is set, also use resumable flow.
		// Otherwise, fall through to standard S3 multipart upload flow.
		if query.Has("uploadId") && g.resumableHandler != nil && r.Header.Get("X-CipherLake-Resumable") == "1" {
			switch method {
			case "PATCH":
				return g.resumableHandler.HandlePatch(w, r, bucket, key)
			case "HEAD":
				return g.resumableHandler.HandleHead(w, r, bucket, key)
			case "POST":
				if query.Has("finalize") {
					return g.resumableHandler.HandleFinalize(w, r, bucket, key)
				}
				return fmt.Errorf("unsupported POST operation for resumable upload")
			default:
				return fmt.Errorf("method not allowed for resumable upload")
			}
		}

		if query.Has("uploadId") {
			switch method {
			case "PUT":
				return g.multipartSvc.HandleUploadPart(w, r, bucket, key)
			case "POST":
				return g.multipartSvc.HandleCompleteMultipartUpload(w, r, bucket, key)
			case "DELETE":
				return g.multipartSvc.HandleAbortMultipartUpload(w, r, bucket, key)
			case "GET":
				return g.multipartSvc.HandleListParts(w, r, bucket, key)
			default:
				return fmt.Errorf("method not allowed for multipart upload")
			}
		}

		switch method {
		case "PUT":
			if r.Header.Get("x-amz-copy-source") != "" {
				return g.objectSvc.handleCopyObject(w, r, bucket, key)
			}
			return g.objectSvc.handlePutObject(w, r, bucket, key)
		case "GET":
			return g.objectSvc.handleGetObject(w, r, bucket, key)
		case "HEAD":
			return g.objectSvc.handleHeadObject(w, r, bucket, key)
		case "DELETE":
			return g.objectSvc.handleDeleteObject(w, r, bucket, key)
		case "POST":
			return g.objectSvc.handleObjectPOST(w, r, bucket, key)
		case "PATCH":
			// PATCH method for resumable uploads (with uploadId in query)
			if g.resumableHandler != nil && query.Has("uploadId") {
				return g.resumableHandler.HandlePatch(w, r, bucket, key)
			}
			return fmt.Errorf("method not allowed")
		default:
			return fmt.Errorf("method not allowed")
		}
	}
}

func isClientDisconnected(err error) bool {
	if err == nil {
		return false
	}
	if err == context.Canceled {
		return true
	}
	errMsg := err.Error()
	if strings.Contains(errMsg, "broken pipe") ||
		strings.Contains(errMsg, "connection reset") ||
		strings.Contains(errMsg, "client disconnected") {
		return true
	}
	if strings.Contains(errMsg, "use of closed connection") {
		return true
	}
	return false
}

func parseRangeHeader(rangeHeader string, totalSize int64) (start, end int64, err error) {
	if !strings.HasPrefix(rangeHeader, "bytes=") {
		return 0, 0, fmt.Errorf("invalid range header format")
	}

	rangeSpec := strings.TrimPrefix(rangeHeader, "bytes=")
	parts := strings.Split(rangeSpec, "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid range specification")
	}

	if parts[0] == "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid range end")
		}
		start = totalSize - end
		if start < 0 {
			start = 0
		}
		end = totalSize - 1
	} else {
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid range start")
		}
		if parts[1] == "" {
			end = totalSize - 1
		} else {
			end, err = strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("invalid range end")
			}
		}
	}

	if start > end || start >= totalSize {
		return 0, 0, fmt.Errorf("range not satisfiable")
	}

	if end >= totalSize {
		end = totalSize - 1
	}

	return start, end, nil
}

// DeleteObjectsRequest represents the XML body of a DeleteObjects request.
type DeleteObjectsRequest struct {
	XMLName xml.Name              `xml:"Delete"`
	Objects []DeleteObjectsObject `xml:"Object"`
	Quiet   bool                  `xml:"Quiet,omitempty"`
}

// DeleteObjectsObject represents a single object in a DeleteObjects request.
type DeleteObjectsObject struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId,omitempty"`
}

// DeleteObjectsResult represents the XML response for DeleteObjects.
type DeleteObjectsResult struct {
	XMLName xml.Name               `xml:"DeleteResult"`
	Deleted []DeleteObjectsDeleted `xml:"Deleted"`
	Error   []DeleteObjectsError   `xml:"Error,omitempty"`
}

// DeleteObjectsDeleted represents a successfully deleted object in the response.
type DeleteObjectsDeleted struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId,omitempty"`
}

// DeleteObjectsError represents a failed deletion in the response.
type DeleteObjectsError struct {
	Key     string `xml:"Key"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// CopyObjectResult represents the XML response for CopyObject.
type CopyObjectResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	LastModified string   `xml:"LastModified"`
	ETag         string   `xml:"ETag"`
}

func (g *S3Gateway) submitBackgroundTask(ctx context.Context, kind string, payload any) {
	if g.taskQueue == nil {
		return
	}

	task, err := taskqueue.NewTask(kind, payload,
		taskqueue.WithRetry(g.config.TaskQueue.DefaultRetry),
		taskqueue.WithDeadlineAfter(5*time.Minute),
	)
	if err != nil {
		zap.L().Error("failed to create background task", zap.String("kind", kind), zap.Error(err))
		return
	}

	if err := g.taskQueue.Submit(ctx, task); err != nil {
		zap.L().Error("failed to submit background task",
			zap.String("kind", kind),
			zap.String("task_id", task.ID),
			zap.Error(err))
	}
}

func (g *S3Gateway) writeError(w http.ResponseWriter, status int, code, message string) {
	requestID := uuid.New().String()
	w.Header().Set("x-amz-request-id", requestID)
	w.Header().Set("Content-Type", "application/xml")

	errorResp := ErrorResponse{
		Code:      code,
		Message:   message,
		RequestID: requestID,
	}

	g.writeXML(w, status, errorResp)
}

func (g *S3Gateway) writeXML(w http.ResponseWriter, status int, data interface{}) error {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)

	encoder := xml.NewEncoder(w)
	encoder.Indent("", "  ")
	return encoder.Encode(data)
}

func (g *S3Gateway) writeJSON(w http.ResponseWriter, status int, data interface{}) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	return json.NewEncoder(w).Encode(data)
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
	g.closeOnce.Do(func() {
		g.closeErr = g.close()
	})
	return g.closeErr
}

func (g *S3Gateway) close() error {
	var closeErr error
	if err := stopGatewayTaskQueue(context.Background(), g.taskQueue); err != nil {
		zap.L().Warn("failed to stop task queue gracefully", zap.Error(err))
		closeErr = errors.Join(closeErr, err)
	}
	if g.resumableCleanup != nil {
		g.resumableCleanup.Stop()
	}
	if g.multipartCleanup != nil {
		g.multipartCleanup.Stop()
	}
	if g.cryptoCoordinator != nil {
		if err := g.cryptoCoordinator.Close(); err != nil {
			closeErr = errors.Join(closeErr, err)
		}
	}
	if g.vector != nil {
		if err := g.vector.Close(); err != nil {
			closeErr = errors.Join(closeErr, err)
		}
	}
	if g.tracerShutdown != nil {
		if err := g.tracerShutdown(context.Background()); err != nil {
			closeErr = errors.Join(closeErr, err)
		}
	}
	if g.eventBus != nil {
		g.eventBus.Stop()
	}
	if g.ftsIndex != nil {
		if err := g.ftsIndex.Close(); err != nil {
			closeErr = errors.Join(closeErr, err)
		}
	}
	if g.accessLog != nil {
		if err := g.accessLog.Close(); err != nil {
			closeErr = errors.Join(closeErr, err)
		}
	}
	if g.metadata != nil {
		if err := g.metadata.Close(); err != nil {
			closeErr = errors.Join(closeErr, err)
		}
	}
	return closeErr
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

// getIdentity authenticates the request using the unified auth provider.
func (g *S3Gateway) getIdentity(r *http.Request) (*auth.Identity, error) {
	if g.authProvider == nil {
		return nil, fmt.Errorf("authentication provider not configured")
	}
	return g.authProvider.Authenticate(r)
}

// getUserID returns the authenticated user ID or "anonymous".
func (g *S3Gateway) getUserID(r *http.Request) string {
	identity, err := g.getIdentity(r)
	if err != nil || identity == nil {
		return "anonymous"
	}
	return identity.ID
}

// requireIdentity authenticates and authorizes the request for the given action.
func (g *S3Gateway) requireIdentity(r *http.Request, action, bucket, key string) (*auth.Identity, error) {
	identity, err := g.getIdentity(r)
	if err != nil {
		return nil, err
	}
	if identity == nil {
		return nil, fmt.Errorf("authentication required")
	}
	if g.permChecker == nil {
		return identity, nil
	}
	if err := g.permChecker.Check(r.Context(), identity, action, bucket, key, r); err != nil {
		return nil, err
	}
	return identity, nil
}

func (g *S3Gateway) GetEventBus() *events.EventBus {
	return g.eventBus
}

func (g *S3Gateway) auditLog(r *http.Request, action, bucket, key, userID, result string, details map[string]interface{}) {
	ctx := context.Background()
	clientIP := ""
	userAgent := ""
	if r != nil {
		ctx = r.Context()
		clientIP = r.RemoteAddr
		userAgent = r.UserAgent()
	}
	requestID := common.GetRequestID(ctx)

	entry := map[string]interface{}{
		"timestamp":  time.Now().UTC().Format(time.RFC3339),
		"request_id": requestID,
		"action":     action,
		"bucket":     bucket,
		"key":        key,
		"user":       userID,
		"result":     result,
		"client_ip":  clientIP,
		"user_agent": userAgent,
	}

	for k, v := range details {
		entry[k] = v
	}

	if g.config != nil && g.config.Logging.Format == "json" {
		data, _ := json.Marshal(entry)
		fmt.Printf("%s\n", string(data))
	}
}

func setChecksumResponseHeaders(w http.ResponseWriter, objMeta *metadata.ObjectMetadata, r *http.Request) {
	checksumMode := r.Header.Get("x-amz-checksum-mode")
	if checksumMode != "ENABLED" && (objMeta.Checksum == "" || objMeta.ChecksumType == "") {
		return
	}
	switch objMeta.ChecksumType {
	case "crc32c":
		w.Header().Set("x-amz-checksum-crc32c", objMeta.Checksum)
	case "sha256":
		w.Header().Set("x-amz-checksum-sha256", objMeta.Checksum)
	case "md5":
		w.Header().Set("x-amz-checksum-md5", objMeta.Checksum)
	}
}

func computeCRC32C(data []byte) string {
	h := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	h.Write(data)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func extractIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		ips := strings.Split(xff, ",")
		if len(ips) > 0 {
			return strings.TrimSpace(ips[0])
		}
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	ip := r.RemoteAddr
	if idx := strings.LastIndex(ip, ":"); idx != -1 {
		ip = ip[:idx]
	}
	return ip
}

func (g *S3Gateway) isBucketPublicRead(r *http.Request, bucket string) bool {
	if g.config != nil && g.config.Auth.AnonymousRead {
		return true
	}

	bucketInfo, err := g.metadata.GetBucket(r.Context(), bucket)
	if err != nil {
		return false
	}

	return bucketInfo.ACL == "public-read" || bucketInfo.ACL == "public-read-write"
}

func (g *S3Gateway) validatePresignedURLMethod(r *http.Request, expectedMethod string) error {
	if r.Method != expectedMethod {
		return fmt.Errorf("presigned URL method mismatch: expected %s, got %s", expectedMethod, r.Method)
	}
	return nil
}
