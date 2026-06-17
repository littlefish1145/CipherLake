# Configuration Reference

Nexus is configured via a YAML configuration file. The default path is
`config.yaml` in the working directory, or set via the `NEXUS_CONFIG`
environment variable.

## Configuration File Structure

```yaml
# Node configuration
node:
  role: "all"                    # all | gateway | metadata | storage
  listen_addr: ":8080"           # S3 gateway listen address
  data_dir: "./data"             # Data directory
  cluster_peers: []              # Cluster peer addresses
  domain: "s3.example.com"       # Domain(s) for virtual-hosted-style requests (comma-separated)

# Gateway (S3 API) configuration
gateway:
  address: ":9000"              # Listen address
  admin_address: ":9001"        # Admin API address
  access_key: "nexus"           # Default access key
  secret_key: "nexus-secret"    # Default secret key
  tls:
    enabled: false
    cert_file: ""
    key_file: ""
  rate_limit:
    enabled: true
    requests_per_second: 100
    burst: 200
  access_log:
    enabled: true
    format: "json"              # json | text

# Metadata store configuration
metadata:
  backend: "boltdb"             # boltdb | etcd
  path: "/var/lib/nexus/metadata.db"
  raft:
    enabled: false
    data_dir: "/var/lib/nexus/raft"
    bind_address: ":9090"
    peers: []

# Storage engine configuration
storage:
  backend: "local"              # local | s3 | azure
  local:
    data_dir: "/var/lib/nexus/data"
  s3:
    endpoint: ""
    region: ""
    access_key: ""
    secret_key: ""
    bucket: ""
  azure:
    connection_string: ""
    container: ""
  erasure:
    enabled: false
    data_shards: 4
    parity_shards: 2
    block_size: 1048576

# Encryption services
encryption:
  sse_s3:
    enabled: true
  sse_kms:
    enabled: false
    kms:
      backend: "local"          # local | aws | vault
      local:
        key_path: "/var/lib/nexus/kms-key"
      aws:
        key_id: ""
        region: ""
      vault:
        address: ""
        token: ""
        key_name: ""

# Microservice endpoints
services:
  encrypt:
    address: "localhost:50051"
  decrypt:
    address: "localhost:50052"
  keygen:
    address: "localhost:50053"
  keystore:
    address: "localhost:50054"
  keyunwrap:
    address: "localhost:50055"
  token:
    address: "localhost:50056"
  sts:
    address: "localhost:50057"

# IAM configuration
iam:
  enabled: true
  store:
    backend: "boltdb"
    path: "/var/lib/nexus/iam.db"
  abac:
    enabled: true
  scp:
    enabled: false

# Observability
observability:
  metrics:
    enabled: true
    address: ":9091"
  tracing:
    enabled: false
    endpoint: ""
    sampler: 0.1
  logging:
    level: "info"               # debug | info | warn | error
    format: "json"              # json | text

# Full-text search
fts:
  enabled: true
  backend: "bm25"
  tokenizer: "unicode"
  max_results: 100

# Vector search
vector:
  enabled: true
  embedding:
    model: "text-embedding-3-small"
    dimensions: 1536
  index:
    type: "mmap"
    path: "/var/lib/nexus/vector-index"

# Storage tiering
tiering:
  enabled: false
  policies:
    - name: "default"
      hot:
        backend: "local"
        age_days: 30
      warm:
        backend: "s3"
        age_days: 90
      cold:
        backend: "azure"

# Event system
events:
  enabled: true
  bus:
    backend: "memory"           # memory | nats
  webhooks:
    enabled: false
    max_retries: 3
  dead_letter:
    enabled: true
    max_age: "168h"

# Backup
backup:
  enabled: true
  incremental:
    enabled: true
    interval: "6h"
    retention: 7
  remote:
    enabled: false
    endpoint: ""
    bucket: ""

# Scheduler
scheduler:
  enabled: true
  jobs:
    - name: "gc"
      cron: "0 3 * * *"
    - name: "index-rebuild"
      cron: "0 4 * * 0"

# Background task queue
task_queue:
  enabled: true
  store_path: "data/tasks.db"    # BoltDB path for task persistence
  workers: 4                     # Concurrent worker goroutines
  poll_interval: "500ms"         # Polling interval for new tasks
  retry_base: "1s"               # Base delay for exponential backoff
  max_retry_delay: "5m"          # Cap for retry backoff
  default_retry: 3               # Default max attempts per task
```

## Background Task Queue

The `task_queue` section controls how asynchronous background work—such as
vector indexing, full-text indexing, and pipeline execution—is scheduled and
retried.

| Field | Default | Description |
|-------|---------|-------------|
| `enabled` | `true` | Enable the task queue. When disabled, background tasks are skipped. |
| `store_path` | `data/tasks.db` | BoltDB file used to persist tasks. |
| `workers` | `4` | Number of concurrent workers that consume tasks. |
| `poll_interval` | `500ms` | How often workers poll for new tasks. |
| `retry_base` | `1s` | Initial delay before the first retry. |
| `max_retry_delay` | `5m` | Maximum delay between retries. |
| `default_retry` | `3` | Default maximum attempts for tasks submitted by the gateway. |

Failed tasks are retried with exponential backoff. After exhausting all
attempts, they are moved to the dead-letter queue and can be inspected in the
configured BoltDB store.

## Environment Variables

All configuration values can be overridden with environment variables using
the prefix `NEXUS_` and replacing dots with underscores:

| Config Path              | Environment Variable                  |
|--------------------------|---------------------------------------|
| `gateway.address`        | `NEXUS_GATEWAY_ADDRESS`              |
| `gateway.access_key`     | `NEXUS_GATEWAY_ACCESS_KEY`           |
| `gateway.secret_key`     | `NEXUS_GATEWAY_SECRET_KEY`           |
| `storage.backend`        | `NEXUS_STORAGE_BACKEND`              |
| `observability.logging.level` | `NEXUS_OBSERVABILITY_LOGGING_LEVEL` |

## Hot Reload

Nexus supports hot reload for certain configuration values. Changes to the
following take effect without restart:

- `observability.logging.level`
- `rate_limit.requests_per_second`
- `rate_limit.burst`

Other changes require a service restart.
