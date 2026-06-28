# Performance Tuning

Optimize Nexus for your workload with these tuning strategies.

## Gateway Tuning

### Connection Pooling

Configure the gateway connection pool for high-throughput workloads:

```yaml
gateway:
  max_connections: 10000
  read_timeout: "30s"
  write_timeout: "30s"
  idle_timeout: "120s"
```

### Rate Limiting

Adjust rate limits based on your expected traffic:

```yaml
gateway:
  rate_limit:
    enabled: true
    requests_per_second: 1000
    burst: 2000
```

For internal services with no rate limiting:

```yaml
gateway:
  rate_limit:
    enabled: false
```

### Access Logging

Disable access logging for maximum throughput:

```yaml
gateway:
  access_log:
    enabled: false
```

Or use async logging:

```yaml
gateway:
  access_log:
    enabled: true
    format: "json"
    async: true
    buffer_size: 4096
```

## Storage Tuning

### Erasure Coding

For data durability without full replication:

```yaml
storage:
  erasure:
    enabled: true
    data_shards: 8
    parity_shards: 4
    block_size: 1048576    # 1MB blocks
```

**Trade-offs**:

| Data:Parity | Storage Overhead | Tolerable Failures | Write Performance |
|-------------|-----------------|--------------------|--------------------|
| 4:2         | 50%             | 2                  | High               |
| 8:4         | 50%             | 4                  | Medium             |
| 12:4        | 33%             | 4                  | Medium-High        |
| 16:4        | 25%             | 4                  | High               |

### Local Storage

For local storage backend, use SSDs and tune the filesystem:

```bash
# Mount options for ext4 on SSD
mount -t ext4 -o noatime,discard /dev/sdb1 /var/lib/nexus/data
```

### S3 Backend

When using S3 as a backend, tune the connection pool:

```yaml
storage:
  s3:
    max_retries: 3
    max_connections: 100
    timeout: "30s"
```

## Metadata Tuning

### BoltDB

For single-node deployments:

```yaml
metadata:
  backend: "boltdb"
  path: "/var/lib/nexus/metadata.db"
  boltdb:
    no_freelist_sync: true
    freelist_type: "map"
    page_size: 4096
```

### Raft Cluster

For multi-node deployments, tune Raft parameters:

```yaml
metadata:
  raft:
    heartbeat_timeout: "1s"
    election_timeout: "1s"
    leader_lease_timeout: "500ms"
    commit_timeout: "50ms"
    max_append_entries: 64
    snapshot_threshold: 8192
    snapshot_interval: "30s"
```

**Tuning guidelines**:

- Increase `heartbeat_timeout` for geographically distributed clusters
- Reduce `commit_timeout` for lower write latency (at the cost of throughput)
- Increase `snapshot_threshold` to reduce snapshot frequency

## Encryption Performance

### Scaling Microservices

The encryption pipeline can become a bottleneck. Scale based on workload:

| Workload | Encrypt Replicas | Decrypt Replicas |
|----------|-----------------|------------------|
| Light (<100 RPS) | 1 | 1 |
| Medium (100-1000 RPS) | 2-4 | 2-4 |
| Heavy (>1000 RPS) | 4-8 | 4-8 |

### Key Caching

Enable key caching in the keystore service to reduce gRPC calls:

```yaml
services:
  keystore:
    cache:
      enabled: true
      ttl: "5m"
      max_size: 1000
```

## Search Performance

### Vector Search

```yaml
vector:
  index:
    type: "mmap"           # mmap for large datasets, inmemory for small
    quantization: "int8"   # int8 for 4x memory reduction, float32 for accuracy
    dimensions: 1536
```

**Quantization trade-offs**:

| Type | Memory | Accuracy | Build Time |
|------|--------|----------|------------|
| float32 | 1x | 100% | Baseline |
| float16 | 0.5x | ~99.9% | 0.8x |
| int8 | 0.25x | ~98% | 0.6x |

### Full-Text Search

```yaml
fts:
  backend: "bm25"
  tokenizer: "unicode"
  max_results: 100
  compaction:
    enabled: true
    interval: "1h"
    segment_threshold: 1000
```

## Memory Tuning

### System-Level

```bash
# Increase file descriptor limits
ulimit -n 65536

# Adjust kernel parameters
sysctl -w vm.swappiness=1
sysctl -w vm.dirty_ratio=15
sysctl -w vm.dirty_background_ratio=5
```

### Application-Level

```yaml
cache:
  max_size: "2GB"
  eviction: "lru"

observability:
  metrics:
    enabled: true
    go_runtime_metrics: true
```

## Benchmarking

Use `nexusctl` to benchmark your deployment:

```bash
# Write benchmark
nexusctl benchmark write --objects 10000 --size 1MB --concurrency 50

# Read benchmark
nexusctl benchmark read --objects 10000 --concurrency 100

# Mixed workload
nexusctl benchmark mixed --read-ratio 0.8 --concurrency 50
```
