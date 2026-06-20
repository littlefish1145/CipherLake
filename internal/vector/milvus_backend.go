package vector

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/milvus-io/milvus-sdk-go/v2/client"
	"github.com/milvus-io/milvus-sdk-go/v2/entity"
)

// MilvusConfig 是 Milvus 后端的配置,深度集成到 VectorConfig 中。
type MilvusConfig struct {
	Address          string            // Milvus 地址,如 "localhost:19530"
	Username         string            // 认证用户名(可空)
	Password         string            // 认证密码(可空)
	DBName           string            // 数据库名(默认 default)
	CollectionName   string            // 集合名(默认 nexus_vectors)
	ShardsNum        int32             // 分片数(默认 2,水平扩展能力)
	IndexType        string            // milvus 索引类型:FLAT/HNSW/IVF_FLAT/IVF_SQ8/DISKANN
	IndexParams      map[string]string // 索引参数,如 HNSW 的 M/efConstruction
	NPROBE           int               // IVF 查询时探查的聚类数
	EF               int               // HNSW 查询时的 ef
	ConsistencyLevel string            // Strong/Bounded/Session/Eventually
	// Tiered Storage(分层存储)配置
	// 通过 ResourceGroup 实现 hot/cold 分离:
	//   - Hot 资源组:节点多,全内存,低延迟
	//   - Cold 资源组:节点少,DISKANN,低成本
	// Milvus 2.6+ 的 Tiered Storage 是服务端透明功能,SDK 无需改动
	TieredStorage    *TieredStorageConfig
	// ReplicaNumber 副本数(默认 1,高可用场景设为 2+)
	ReplicaNumber    int32
	// DISKANN 专用参数(当 IndexType == "DISKANN" 时生效)
	DiskANN          *DiskANNConfig
}

// TieredStorageConfig 配置热冷分层存储。
type TieredStorageConfig struct {
	Enabled       bool   // 是否启用分层存储
	HotResourceGroup  string // 热数据资源组名(默认 "hot")
	ColdResourceGroup string // 冷数据资源组名(默认 "cold")
	HotNodes      int32  // 热资源组节点数(默认 2)
	ColdNodes     int32  // 冷资源组节点数(默认 1)
	// WarmUp 预热策略(Milvus 2.6+ 服务端配置):
	//   sync:   load 前先加载到缓存(高延迟,低查询延迟)
	//   async:  后台异步预热(平衡)
	//   disable: 完全按需加载(低内存,首查询高延迟)
	WarmUp        string // sync | async | disable
}

// DiskANNConfig 配置 DISKANN 磁盘索引参数。
type DiskANNConfig struct {
	MaxDegree               int     // Vamana 图最大度数(默认 56)
	SearchListSize          int     // 候选列表大小(默认 100)
	PQCodeBudgetGBRatio     float64 // PQ 码本内存比例(默认 0.125)
	SearchCacheBudgetGBRatio float64 // 缓存节点数据比例(默认 0.10)
	BeamWidthRatio           float64 // 每次搜索迭代最大 IO 请求数与 CPU 数的比值(默认 4.0)
}

// MilvusBackend 实现 VectorIndex 接口,将向量存储和索引委托给 Milvus。
//
// 深度集成策略:
//   - Milvus 负责向量存储、HNSW/IVF/DISKANN 索引、分片、副本、持久化
//   - 本层保留 embedding 缓存、metadata 过滤、bucket 分区(映射为 Milvus partition key)
//   - Tiered Storage 通过 ResourceGroup 实现 hot/cold 分离
//   - 自研 HNSW/MMap/BucketIndex 在此前提下可退役
type MilvusBackend struct {
	mu          sync.RWMutex
	client      client.Client
	config      *MilvusConfig
	dim         int
	metric      MetricType
	collection  string
	initialized bool
	stats       IndexStats
}

// NewMilvusBackend 创建并连接 Milvus 后端,自动建集合和索引。
func NewMilvusBackend(dim int, metric MetricType, cfg *MilvusConfig) (*MilvusBackend, error) {
	if cfg == nil {
		cfg = &MilvusConfig{}
	}
	if cfg.Address == "" {
		cfg.Address = "localhost:19530"
	}
	if cfg.CollectionName == "" {
		cfg.CollectionName = "nexus_vectors"
	}
	if cfg.ShardsNum == 0 {
		cfg.ShardsNum = 2
	}
	if cfg.IndexType == "" {
		cfg.IndexType = "HNSW"
	}
	if cfg.NPROBE == 0 {
		cfg.NPROBE = 16
	}
	if cfg.EF == 0 {
		cfg.EF = 64
	}
	if cfg.ConsistencyLevel == "" {
		cfg.ConsistencyLevel = "Bounded"
	}
	if cfg.ReplicaNumber == 0 {
		cfg.ReplicaNumber = 1
	}
	// DISKANN 默认参数
	if cfg.IndexType == "DISKANN" && cfg.DiskANN == nil {
		cfg.DiskANN = &DiskANNConfig{
			MaxDegree:               56,
			SearchListSize:          100,
			PQCodeBudgetGBRatio:     0.125,
			SearchCacheBudgetGBRatio: 0.10,
			BeamWidthRatio:           4.0,
		}
	}
	// Tiered Storage 默认配置
	if cfg.TieredStorage != nil && cfg.TieredStorage.Enabled {
		if cfg.TieredStorage.HotResourceGroup == "" {
			cfg.TieredStorage.HotResourceGroup = "hot"
		}
		if cfg.TieredStorage.ColdResourceGroup == "" {
			cfg.TieredStorage.ColdResourceGroup = "cold"
		}
		if cfg.TieredStorage.HotNodes == 0 {
			cfg.TieredStorage.HotNodes = 2
		}
		if cfg.TieredStorage.ColdNodes == 0 {
			cfg.TieredStorage.ColdNodes = 1
		}
		if cfg.TieredStorage.WarmUp == "" {
			cfg.TieredStorage.WarmUp = "async"
		}
	}

	mb := &MilvusBackend{
		config:     cfg,
		dim:        dim,
		metric:     metric,
		collection: cfg.CollectionName,
	}

	if err := mb.connect(); err != nil {
		return nil, fmt.Errorf("milvus connect failed: %w", err)
	}

	// 启用 Tiered Storage 时创建资源组
	if cfg.TieredStorage != nil && cfg.TieredStorage.Enabled {
		if err := mb.setupResourceGroups(); err != nil {
			mb.Close()
			return nil, fmt.Errorf("milvus setup resource groups failed: %w", err)
		}
	}

	if err := mb.ensureCollection(); err != nil {
		mb.Close()
		return nil, fmt.Errorf("milvus ensure collection failed: %w", err)
	}

	mb.initialized = true
	mb.stats = IndexStats{
		Dimension:   dim,
		IndexType:   "Milvus-" + cfg.IndexType,
		LastBuiltAt: time.Now(),
	}
	return mb, nil
}

// connect 建立与 Milvus 的连接。
func (mb *MilvusBackend) connect() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	config := client.Config{
		Address:  mb.config.Address,
		Username: mb.config.Username,
		Password: mb.config.Password,
		DBName:   mb.config.DBName,
	}

	c, err := client.NewClient(ctx, config)
	if err != nil {
		return fmt.Errorf("failed to create milvus client: %w", err)
	}
	mb.client = c
	return nil
}

// setupResourceGroups 创建热冷资源组(Milvus 2.4+ 支持)。
// 热资源组:多节点,全内存,低延迟查询
// 冷资源组:少节点,DISKANN,低成本存储
func (mb *MilvusBackend) setupResourceGroups() error {
	ctx := context.Background()
	ts := mb.config.TieredStorage

	// 创建热资源组
	hotRg, err := mb.client.DescribeResourceGroup(ctx, ts.HotResourceGroup)
	if err != nil || hotRg == nil {
		// 资源组不存在,创建它
		hotCfg := &entity.ResourceGroupConfig{
			Requests: &entity.ResourceGroupLimit{
				NodeNum: ts.HotNodes,
			},
		}
		if err := mb.client.CreateResourceGroup(ctx, ts.HotResourceGroup,
			client.WithCreateResourceGroupConfig(hotCfg)); err != nil {
			return fmt.Errorf("create hot resource group failed: %w", err)
		}
	}

	// 创建冷资源组
	coldRg, err := mb.client.DescribeResourceGroup(ctx, ts.ColdResourceGroup)
	if err != nil || coldRg == nil {
		coldCfg := &entity.ResourceGroupConfig{
			Requests: &entity.ResourceGroupLimit{
				NodeNum: ts.ColdNodes,
			},
		}
		if err := mb.client.CreateResourceGroup(ctx, ts.ColdResourceGroup,
			client.WithCreateResourceGroupConfig(coldCfg)); err != nil {
			return fmt.Errorf("create cold resource group failed: %w", err)
		}
	}

	return nil
}

// ensureCollection 检查集合是否存在,不存在则创建并建索引。
func (mb *MilvusBackend) ensureCollection() error {
	ctx := context.Background()
	has, err := mb.client.HasCollection(ctx, mb.collection)
	if err != nil {
		return fmt.Errorf("has collection check failed: %w", err)
	}
	if has {
		return mb.loadCollection()
	}
	return mb.createCollection()
}

// createCollection 创建集合(含 schema)并建索引。
func (mb *MilvusBackend) createCollection() error {
	ctx := context.Background()

	// 主键:VarChar,使用应用层提供的 string ID
	pkField := entity.NewField().
		WithName("id").
		WithDataType(entity.FieldTypeVarChar).
		WithMaxLength(256).
		WithIsPrimaryKey(true)

	// 向量字段
	vecField := entity.NewField().
		WithName("vector").
		WithDataType(entity.FieldTypeFloatVector).
		WithDim(int64(mb.dim))

	// bucket 字段:作为分区键,实现按 bucket 分片(Milvus 自动分区)
	bucketField := entity.NewField().
		WithName("bucket").
		WithDataType(entity.FieldTypeVarChar).
		WithMaxLength(256).
		WithIsPartitionKey(true)

	// object_key 字段
	objectKeyField := entity.NewField().
		WithName("object_key").
		WithDataType(entity.FieldTypeVarChar).
		WithMaxLength(512)

	// metadata JSON 字段:支持动态过滤
	metadataField := entity.NewField().
		WithName("metadata").
		WithDataType(entity.FieldTypeJSON)

	schema := entity.NewSchema().
		WithName(mb.collection).
		WithDescription("Nexus vector storage backed by Milvus").
		WithField(pkField).
		WithField(vecField).
		WithField(bucketField).
		WithField(objectKeyField).
		WithField(metadataField)

	consistencyLevel := mb.parseConsistencyLevel()
	opts := []client.CreateCollectionOption{
		client.WithConsistencyLevel(consistencyLevel),
	}

	if err := mb.client.CreateCollection(ctx, schema, mb.config.ShardsNum, opts...); err != nil {
		return fmt.Errorf("create collection failed: %w", err)
	}

	return mb.createIndex()
}

// createIndex 在向量字段上创建索引。
func (mb *MilvusBackend) createIndex() error {
	ctx := context.Background()

	idxType := strings.ToUpper(mb.config.IndexType)
	metricType := mb.milvusMetricType()

	var idx entity.Index
	switch idxType {
	case "HNSW":
		m := 16
		efConstruction := 200
		if v, ok := mb.config.IndexParams["M"]; ok {
			if n, err := strconv.Atoi(v); err == nil {
				m = n
			}
		}
		if v, ok := mb.config.IndexParams["efConstruction"]; ok {
			if n, err := strconv.Atoi(v); err == nil {
				efConstruction = n
			}
		}
		idx0, err := entity.NewIndexHNSW(metricType, m, efConstruction)
		if err != nil {
			return fmt.Errorf("new hnsw index failed: %w", err)
		}
		idx = idx0
	case "IVF_FLAT":
		nlist := 1024
		if v, ok := mb.config.IndexParams["nlist"]; ok {
			if n, err := strconv.Atoi(v); err == nil {
				nlist = n
			}
		}
		idx0, err := entity.NewIndexIvfFlat(metricType, nlist)
		if err != nil {
			return fmt.Errorf("new ivf flat index failed: %w", err)
		}
		idx = idx0
	case "IVF_SQ8":
		nlist := 1024
		if v, ok := mb.config.IndexParams["nlist"]; ok {
			if n, err := strconv.Atoi(v); err == nil {
				nlist = n
			}
		}
		idx0, err := entity.NewIndexIvfSQ8(metricType, nlist)
		if err != nil {
			return fmt.Errorf("new ivf sq8 index failed: %w", err)
		}
		idx = idx0
	case "DISKANN":
		idx0, err := entity.NewIndexDISKANN(metricType)
		if err != nil {
			return fmt.Errorf("new diskann index failed: %w", err)
		}
		idx = idx0
	case "FLAT", "AUTO":
		idx0, err := entity.NewIndexFlat(metricType)
		if err != nil {
			return fmt.Errorf("new flat index failed: %w", err)
		}
		idx = idx0
	default:
		return fmt.Errorf("unsupported milvus index type: %s", idxType)
	}

	if err := mb.client.CreateIndex(ctx, mb.collection, "vector", idx, false); err != nil {
		return fmt.Errorf("create index failed: %w", err)
	}
	return nil
}

// loadCollection 加载已存在的集合到内存。
// 启用 Tiered Storage 时,将副本分配到热冷资源组。
func (mb *MilvusBackend) loadCollection() error {
	ctx := context.Background()

	loadOpts := []client.LoadCollectionOption{}

	// 配置副本数
	if mb.config.ReplicaNumber > 1 {
		loadOpts = append(loadOpts, client.WithReplicaNumber(mb.config.ReplicaNumber))
	}

	// 启用 Tiered Storage 时,将副本分配到热冷资源组
	if mb.config.TieredStorage != nil && mb.config.TieredStorage.Enabled {
		rgs := []string{
			mb.config.TieredStorage.HotResourceGroup,
			mb.config.TieredStorage.ColdResourceGroup,
		}
		loadOpts = append(loadOpts, client.WithResourceGroups(rgs))
	}

	return mb.client.LoadCollection(ctx, mb.collection, false, loadOpts...)
}

// --- Tiered Storage 管理 API ---

// LoadToHot 将集合加载到热资源组(全内存,低延迟)。
// 适用于高频查询场景。
func (mb *MilvusBackend) LoadToHot(ctx context.Context) error {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if mb.config.TieredStorage == nil || !mb.config.TieredStorage.Enabled {
		return fmt.Errorf("tiered storage not enabled")
	}

	// 先释放,再加载到热资源组
	if err := mb.client.ReleaseCollection(ctx, mb.collection); err != nil {
		return fmt.Errorf("release before hot load failed: %w", err)
	}

	return mb.client.LoadCollection(ctx, mb.collection, false,
		client.WithResourceGroups([]string{mb.config.TieredStorage.HotResourceGroup}),
	)
}

// LoadToCold 将集合加载到冷资源组(DISKANN,低成本)。
// 适用于低频查询/归档场景。
func (mb *MilvusBackend) LoadToCold(ctx context.Context) error {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if mb.config.TieredStorage == nil || !mb.config.TieredStorage.Enabled {
		return fmt.Errorf("tiered storage not enabled")
	}

	if err := mb.client.ReleaseCollection(ctx, mb.collection); err != nil {
		return fmt.Errorf("release before cold load failed: %w", err)
	}

	return mb.client.LoadCollection(ctx, mb.collection, false,
		client.WithResourceGroups([]string{mb.config.TieredStorage.ColdResourceGroup}),
	)
}

// Release 从内存卸载集合,数据仍保留在对象存储中。
// 适用于完全不再查询的归档数据。
func (mb *MilvusBackend) Release(ctx context.Context) error {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	return mb.client.ReleaseCollection(ctx, mb.collection)
}

// GetLoadState 查询集合加载状态。
// 返回:NotExist / NotLoad / Loading / Loaded
func (mb *MilvusBackend) GetLoadState(ctx context.Context) (string, error) {
	mb.mu.RLock()
	defer mb.mu.RUnlock()

	state, err := mb.client.GetLoadState(ctx, mb.collection, nil)
	if err != nil {
		return "", fmt.Errorf("get load state failed: %w", err)
	}
	switch state {
	case entity.LoadStateNotExist:
		return "NotExist", nil
	case entity.LoadStateNotLoad:
		return "NotLoad", nil
	case entity.LoadStateLoading:
		return "Loading", nil
	case entity.LoadStateLoaded:
		return "Loaded", nil
	default:
		return fmt.Sprintf("Unknown(%d)", int32(state)), nil
	}
}

// GetLoadingProgress 查询集合加载进度(0-100)。
func (mb *MilvusBackend) GetLoadingProgress(ctx context.Context) (int64, error) {
	mb.mu.RLock()
	defer mb.mu.RUnlock()

	return mb.client.GetLoadingProgress(ctx, mb.collection, nil)
}

// --- 分区管理 API ---

// CreatePartition 为指定 bucket 创建独立分区。
// 启用后可按分区粒度加载/卸载,实现更精细的冷热控制。
func (mb *MilvusBackend) CreatePartition(ctx context.Context, bucket string) error {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	return mb.client.CreatePartition(ctx, mb.collection, bucket)
}

// LoadPartition 加载指定 bucket 分区到内存(热)。
func (mb *MilvusBackend) LoadPartition(ctx context.Context, bucket string) error {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	// LoadPartitions 不支持 WithReplicaNumber(选项类型不同),
	// 副本数由集合级配置决定
	return mb.client.LoadPartitions(ctx, mb.collection, []string{bucket}, false)
}

// ReleasePartition 从内存卸载指定 bucket 分区(变冷)。
func (mb *MilvusBackend) ReleasePartition(ctx context.Context, bucket string) error {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	return mb.client.ReleasePartitions(ctx, mb.collection, []string{bucket})
}

// --- 批量导入 API ---

// BulkInsert 从文件批量导入向量数据。
// 支持JSON/Parquet格式,适用于大规模初始数据导入。
// 返回 taskID,可通过 GetBulkInsertState 查询进度。
func (mb *MilvusBackend) BulkInsert(ctx context.Context, files []string) (int64, error) {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	taskID, err := mb.client.BulkInsert(ctx, mb.collection, "", files)
	if err != nil {
		return 0, fmt.Errorf("bulk insert failed: %w", err)
	}
	return taskID, nil
}

// --- VectorIndex 接口实现 ---

// Insert 实现 VectorIndex.Insert,批量写入向量到 Milvus。
func (mb *MilvusBackend) Insert(ctx context.Context, vectors []Vector) error {
	if len(vectors) == 0 {
		return nil
	}

	mb.mu.Lock()
	defer mb.mu.Unlock()

	ids := make([]string, 0, len(vectors))
	vecs := make([][]float32, 0, len(vectors))
	buckets := make([]string, 0, len(vectors))
	objectKeys := make([]string, 0, len(vectors))
	metadatas := make([][]byte, 0, len(vectors))

	for i := range vectors {
		v := &vectors[i]
		if v.ID == "" {
			return fmt.Errorf("vector ID is required for milvus insert")
		}
		if len(v.Values) != mb.dim {
			return fmt.Errorf("vector dimension mismatch: expected %d, got %d", mb.dim, len(v.Values))
		}
		ids = append(ids, v.ID)
		vecs = append(vecs, v.Values)
		buckets = append(buckets, v.Bucket)
		objectKeys = append(objectKeys, v.ObjectKey)
		metadatas = append(metadatas, metadataToJSON(v.Metadata))
	}

	// Insert(ctx, collName, partitionName, columns...)
	// partitionName 为空字符串表示默认分区(Milvus 按 bucket partition key 自动分片)
	_, err := mb.client.Insert(
		ctx,
		mb.collection,
		"",
		entity.NewColumnVarChar("id", ids),
		entity.NewColumnFloatVector("vector", mb.dim, vecs),
		entity.NewColumnVarChar("bucket", buckets),
		entity.NewColumnVarChar("object_key", objectKeys),
		entity.NewColumnJSONBytes("metadata", metadatas),
	)
	if err != nil {
		return fmt.Errorf("milvus insert failed: %w", err)
	}

	mb.stats.TotalVectors += int64(len(vectors))
	return nil
}

// Search 实现 VectorIndex.Search,执行向量相似度搜索。
func (mb *MilvusBackend) Search(ctx context.Context, query Vector, topK int, filters map[string]string) ([]SearchResult, error) {
	if len(query.Values) != mb.dim {
		return nil, ErrInvalidDimension
	}

	mb.mu.RLock()
	defer mb.mu.RUnlock()

	sp, err := mb.buildSearchParams()
	if err != nil {
		return nil, err
	}

	// 构建 metadata 过滤表达式
	expr := mb.buildFilterExpr(filters)
	metricType := mb.milvusMetricType()
	outputFields := []string{"id", "bucket", "object_key", "metadata"}

	resultSets, err := mb.client.Search(
		ctx,
		mb.collection,
		[]string{}, // 所有分区
		expr,        // 过滤表达式
		outputFields,
		[]entity.Vector{entity.FloatVector(query.Values)},
		"vector",
		metricType,
		topK,
		sp,
	)
	if err != nil {
		return nil, fmt.Errorf("milvus search failed: %w", err)
	}

	if len(resultSets) == 0 {
		return nil, nil
	}

	rs := resultSets[0]
	scores := rs.Scores
	fields := rs.Fields

	results := make([]SearchResult, 0, len(scores))
	for i := 0; i < len(scores); i++ {
		sr := SearchResult{Score: scores[i]}

		// 从结果列中提取字段值
		if col := fields.GetColumn("id"); col != nil {
			if v, err := col.GetAsString(i); err == nil {
				sr.ID = v
			}
		}
		if col := fields.GetColumn("bucket"); col != nil {
			if v, err := col.GetAsString(i); err == nil {
				sr.Bucket = v
			}
		}
		if col := fields.GetColumn("object_key"); col != nil {
			if v, err := col.GetAsString(i); err == nil {
				sr.ObjectKey = v
			}
		}
		if col := fields.GetColumn("metadata"); col != nil {
			if v, err := col.GetAsString(i); err == nil {
				sr.Metadata = jsonToMetadata(v)
			}
		}
		results = append(results, sr)
	}

	mb.stats.QueryCount++
	return results, nil
}

// Delete 实现 VectorIndex.Delete,按 ID 删除向量。
func (mb *MilvusBackend) Delete(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}

	mb.mu.Lock()
	defer mb.mu.Unlock()

	// 构建 ID 过滤表达式
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = strconv.Quote(id)
	}
	expr := fmt.Sprintf("id in [%s]", strings.Join(quoted, ","))

	// Delete(ctx, collName, partitionName, expr)
	if err := mb.client.Delete(ctx, mb.collection, "", expr); err != nil {
		return fmt.Errorf("milvus delete failed: %w", err)
	}

	mb.stats.TotalVectors -= int64(len(ids))
	if mb.stats.TotalVectors < 0 {
		mb.stats.TotalVectors = 0
	}
	return nil
}

// Build 实现 VectorIndex.Build。Milvus 的索引在写入时自动构建,此处为 Flush。
func (mb *MilvusBackend) Build(ctx context.Context) error {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if err := mb.client.Flush(ctx, mb.collection, false); err != nil {
		return fmt.Errorf("milvus flush failed: %w", err)
	}
	mb.stats.LastBuiltAt = time.Now()
	return nil
}

// GetStats 实现 VectorIndex.GetStats。
func (mb *MilvusBackend) GetStats() IndexStats {
	mb.mu.RLock()
	defer mb.mu.RUnlock()

	stats := mb.stats
	// 查询 Milvus 获取实际行数(异步,不阻塞)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if statsMap, err := mb.client.GetCollectionStatistics(ctx, mb.collection); err == nil {
		if rowStr, ok := statsMap["row_count"]; ok {
			if n, err := strconv.ParseInt(rowStr, 10, 64); err == nil {
				stats.TotalVectors = n
			}
		}
	}
	return stats
}

// Close 关闭 Milvus 连接。
func (mb *MilvusBackend) Close() error {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if mb.client != nil {
		err := mb.client.Close()
		mb.client = nil
		mb.initialized = false
		return err
	}
	return nil
}

// --- 内部辅助方法 ---

func (mb *MilvusBackend) buildSearchParams() (entity.SearchParam, error) {
	idxType := strings.ToUpper(mb.config.IndexType)
	switch idxType {
	case "HNSW":
		return entity.NewIndexHNSWSearchParam(mb.config.EF)
	case "IVF_FLAT":
		return entity.NewIndexIvfFlatSearchParam(mb.config.NPROBE)
	case "IVF_SQ8":
		return entity.NewIndexIvfSQ8SearchParam(mb.config.NPROBE)
	case "DISKANN":
		// DISKANN 的 search_list 参数,范围 [topK, int32_max]
		// 默认取 max(NPROBE, 16)
		searchList := mb.config.NPROBE
		if searchList < 16 {
			searchList = 16
		}
		sp, err := entity.NewIndexDISKANNSearchParam(searchList)
		if err != nil {
			return nil, fmt.Errorf("new diskann search param failed: %w", err)
		}
		return sp, nil
	case "FLAT", "AUTO":
		return entity.NewIndexFlatSearchParam()
	default:
		return entity.NewIndexHNSWSearchParam(mb.config.EF)
	}
}

// buildFilterExpr 将 metadata filters 转换为 Milvus 表达式。
// Milvus 的 JSON 字段过滤语法:metadata["key"] == "value"
func (mb *MilvusBackend) buildFilterExpr(filters map[string]string) string {
	if len(filters) == 0 {
		return ""
	}
	var parts []string
	for k, v := range filters {
		// 转义 key 中的特殊字符
		k = strings.ReplaceAll(k, "\"", "\\\"")
		v = strings.ReplaceAll(v, "\"", "\\\"")
		parts = append(parts, fmt.Sprintf("metadata[\"%s\"] == \"%s\"", k, v))
	}
	return strings.Join(parts, " and ")
}

func (mb *MilvusBackend) milvusMetricType() entity.MetricType {
	switch mb.metric {
	case MetricCosine:
		return entity.COSINE
	case MetricEuclidean:
		return entity.L2
	case MetricDotProduct:
		return entity.IP
	default:
		return entity.COSINE
	}
}

func (mb *MilvusBackend) parseConsistencyLevel() entity.ConsistencyLevel {
	switch strings.ToLower(mb.config.ConsistencyLevel) {
	case "strong":
		return entity.ClStrong
	case "session":
		return entity.ClSession
	case "eventually":
		return entity.ClEventually
	default:
		return entity.ClBounded
	}
}

// metadataToJSON 将 map[string]string 序列化为 JSON 字节。
func metadataToJSON(m map[string]string) []byte {
	if len(m) == 0 {
		return []byte("{}")
	}
	var sb strings.Builder
	sb.WriteByte('{')
	first := true
	for k, v := range m {
		if !first {
			sb.WriteByte(',')
		}
		first = false
		sb.WriteString(strconv.Quote(k))
		sb.WriteByte(':')
		sb.WriteString(strconv.Quote(v))
	}
	sb.WriteByte('}')
	return []byte(sb.String())
}

// jsonToMetadata 将 JSON 字符串解析为 map[string]string。
func jsonToMetadata(s string) map[string]string {
	if s == "" {
		return nil
	}
	m := make(map[string]string)
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return m
	}
	// 简单解析:去除首尾花括号,按逗号分割
	inner := s[1 : len(s)-1]
	if inner == "" {
		return m
	}
	pairs := splitJSONPairs(inner)
	for _, p := range pairs {
		colon := strings.Index(p, ":")
		if colon < 0 {
			continue
		}
		k := strings.Trim(strings.TrimSpace(p[:colon]), "\"")
		v := strings.Trim(strings.TrimSpace(p[colon+1:]), "\"")
		m[k] = v
	}
	return m
}

// splitJSONPairs 简单地按逗号分割 JSON 键值对(不处理嵌套)。
func splitJSONPairs(s string) []string {
	var pairs []string
	depth := 0
	start := 0
	for i, c := range s {
		switch c {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case ',':
			if depth == 0 {
				pairs = append(pairs, s[start:i])
				start = i + 1
			}
		}
	}
	pairs = append(pairs, s[start:])
	return pairs
}

// 确保 MilvusBackend 在编译期满足 VectorIndex 接口。
var _ VectorIndex = (*MilvusBackend)(nil)
