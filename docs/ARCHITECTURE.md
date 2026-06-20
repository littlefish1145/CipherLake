# Nexus Codebase Map

## Project Overview

Nexus is an S3-compatible intelligent object storage system written in Go. It features intelligent tiering, zero-trust encryption with external KMS, native vector search, content processing pipelines, and a full IAM system compatible with AWS S3 semantics.

---

## `/cmd/` -- Entrypoints (Binary Targets)

### `cmd/nexus/`

| File | Description |
|------|-------------|
| `cmd/nexus/main.go` | **Main server entrypoint.** Cobra-based CLI that bootstraps the S3Gateway, config, IAM, tiering, and HTTP server; handles graceful shutdown on signals. |

### `cmd/nexusctl/`

| File | Description |
|------|-------------|
| `cmd/nexusctl/main.go` | **Nexus CLI tool entrypoint.** Cobra-rooted management CLI (`nexusctl`) with persistent config loading and REST API client helpers. |
| `cmd/nexusctl/config.go` | CLI configuration loading (`~/.nexusctl.yaml`), encrypted secret storage with AES-256-GCM + HKDF, and CRUD for server config. |
| `cmd/nexusctl/bucket.go` | Bucket management commands: `create`, `list`, `delete`, `info`, `policy get/set`, `versioning` via REST API. |
| `cmd/nexusctl/user.go` | User management commands: `create`, `list`, `info`, `update`, `delete` with API key and permission management. |
| `cmd/nexusctl/iam.go` | IAM management commands: users, groups, policies, roles, access keys. |
| `cmd/nexusctl/cluster.go` | Cluster management commands: `status`, `add-peer`, `remove-peer`, `migrate` for Raft consensus. |
| `cmd/nexusctl/backup.go` | Backup management commands: `create`, `list`, `restore`, `verify`, `drill` for full and incremental backups. |
| `cmd/nexusctl/completion.go` | Shell completion generator for `bash`, `zsh`, `fish`, `powershell`. |
| `cmd/nexusctl/errors.go` | RFC 7807 Problem Details error formatting and HTTP status helper for CLI responses. |
| `cmd/nexusctl/output.go` | Output formatting engine supporting `json`, `yaml`, `table` formats with JMESPath query filtering. |

### Microservice Binaries (`cmd/token-service/`, `cmd/keygen-service/`, `cmd/keyunwrap-service/`, `cmd/encrypt-service/`, `cmd/decrypt-service/`, `cmd/keystore-service/`, `cmd/sts-service/`)

Each is a standalone gRPC microservice for the distributed encryption ecosystem (ports 50051-50057).

---

## `/internal/` -- Core Implementation

### `internal/gateway/` -- S3 API Gateway

| File | Description |
|------|-------------|
| `internal/gateway/s3.go` | **Main S3Gateway.** HTTP request router/mux for all S3-compatible operations (~1400 lines). Routes bucket/object operations, wraps handlers with auth, rate-limiting, observability, and CORS. |
| `internal/gateway/auth.go` | **Legacy auth handler.** JWT-based authentication (HS256/HS512), basic auth, API key auth, user credential management with bcrypt password hashing, refresh token rotation, session management. |
| `internal/gateway/admin_auth.go` | **Admin authentication subsystem.** Separate admin HTTP server with mTLS, JWT, IP whitelisting, token-file auth, admin API routes. |
| `internal/gateway/sigv4.go` | **AWS SigV4 signature verification.** Parses and validates `AWS4-HMAC-SHA256` authorization headers. |
| `internal/gateway/object_service.go` | **Object CRUD service layer.** Handles PutObject, GetObject, HeadObject, DeleteObject, CopyObject with conditional headers, checksums, SSE-C, and encryption coordinator integration. |
| `internal/gateway/bucket_service.go` | **Bucket CRUD service layer.** Handles ListBuckets, CreateBucket, DeleteBucket, HeadBucket, ListObjects, ListObjectsV2 with pagination and prefix/delimiter filtering. |
| `internal/gateway/multipart.go` | **Multipart upload handler.** Core engine for CreateMultipartUpload, UploadPart, CompleteMultipartUpload, AbortMultipartUpload, ListParts with disk-based staging and ETag computation. |
| `internal/gateway/multipart_service.go` | **Multipart service facade.** Thin wrapper mapping HTTP routes to multipart operations. |
| `internal/gateway/resumable.go` | **Resumable upload handler.** Implements resumable upload sessions with chunked upload via `PUT` with `Content-Range`, SHA-256 integrity verification, and session persistence. |
| `internal/gateway/resumable_cleanup.go` | **Resumable upload cleanup daemon.** Background goroutine that periodically purges expired resumable upload sessions. |
| `internal/gateway/search_service.go` | **Search service (S3 Select-like).** Handles FTS search, vector search, and hybrid search queries over stored objects. |
| `internal/gateway/tls.go` | **TLS manager.** Auto-generates self-signed certificates, hot-reloads certs on SIGHUP, manages mTLS configuration. |
| `internal/gateway/access_log.go` | **Access logging.** File-based access log writer with daily rotation, max-size rotation, configurable format, and concurrent-safe writes. |
| `internal/gateway/iam_bridge.go` | **IAM-to-legacy-bridge.** Adapts the new IAM system to the legacy `AuthHandler`. |
| `internal/gateway/auth_provider.go` | **Unified auth provider.** Migration shim adapting both `AuthHandler` and `IAMAuthBridge` to the `auth.Provider` interface. |

### `internal/storage/` -- Storage Backends

| File | Description |
|------|-------------|
| `internal/storage/store.go` | **ObjectStore interface and composite store.** Defines `ObjectStore` interface and `CompositeTieredStore` routing objects across Hot/Warm/Cold/Archive tiers. Contains tier-aware file and directory backends. |
| `internal/storage/backend_factory.go` | **Backend factory.** Creates `BackendStorage` instances from `StorageClassConfig` -- supports `file`, `s3`, and `azure` backend types. |
| `internal/storage/s3_backend.go` | **S3 remote backend.** Implements `BackendStorage` using AWS SDK v2 to proxy requests to an external S3-compatible service. |
| `internal/storage/azure_backend.go` | **Azure Blob remote backend.** Implements `BackendStorage` using Azure SDK for Blob storage. |
| `internal/storage/erasure.go` | **Erasure-coded backend.** Reed-Solomon erasure coding implementation splitting data into shards distributed across multiple backends. |
| `internal/storage/erasure_config.go` | **Erasure coding configuration.** `ErasureConfig` struct (DataShards, ParityShards, Backends) and default shard path mapping function. |

### `internal/tiering/` -- Intelligent Tiering

| File | Description |
|------|-------------|
| `internal/tiering/manager.go` | **Tiering manager.** Monitors access patterns, computes hotness scores, makes tier migration decisions (Hot/Warm/Cold/Archive), and executes scheduled data movement. |

### `internal/config/` -- Configuration

| File | Description |
|------|-------------|
| `internal/config/config.go` | **Configuration structs and Viper loading.** All config types loaded from YAML via Viper with env var overrides, defaults, and mapstructure tags. |
| `internal/config/validate.go` | **Configuration validation.** Schema checking for all config fields with structured `ValidationError` list. |
| `internal/config/hotreload.go` | **Hot-reload subsystem.** Watches config file via fsnotify for changes and applies runtime-safe updates. |

### `internal/metadata/` -- Metadata Store

| File | Description |
|------|-------------|
| `internal/metadata/store.go` | **BoltDB metadata store.** Full implementation using BoltDB: object metadata CRUD, bucket CRUD, versioning, listing with filters, quotas, atomic transactions. |
| `internal/metadata/integrity.go` | **Data integrity verification.** Checksum computation, background scrubber that periodically reads and verifies stored objects, and integrity report generation. |

### `internal/pipeline/` -- Content Processing Pipeline

| File | Description |
|------|-------------|
| `internal/pipeline/executor.go` | **Pipeline executor.** Loads pipeline definitions from YAML, matches triggers against incoming object metadata, executes step sequences via plugin system. |
| `internal/pipeline/image_plugins.go` | **Image processing plugins.** `RealImageCompressPlugin` and `RealImageResizePlugin` for compressing/resizing images (JPEG, PNG, GIF, WebP). |

### `internal/vector/` -- Vector Search

| File | Description |
|------|-------------|
| `internal/vector/engine.go` | **Vector search engine.** In-memory HNSW-like index for approximate nearest neighbor search with cosine/euclidean/dot-product metrics. |
| `internal/vector/embedding.go` | **Embedding provider abstraction.** Interface for text-to-vector embedding generation supporting local (Ollama, sentence-transformers) and remote (OpenAI, custom API) providers. |
| `internal/vector/milvus_backend.go` | **Milvus vector database backend.** Integration with Milvus 2.x for distributed vector storage and search with collections, indexes, and resource groups. |

### `internal/events/` -- Event Bus

| File | Description |
|------|-------------|
| `internal/events/bus.go` | **Event bus.** In-memory pub/sub event system for S3 events with bucket-level subscriptions and concurrent delivery. |
| `internal/events/rule.go` | **Notification rules.** `NotificationRule` struct with prefix/suffix filtering, event type matching, and destination configuration (webhook, Kafka, NATS, AMQP). |
| `internal/events/webhook.go` | **Webhook delivery.** HTTP/S webhook sender with HMAC-SHA256 signing, configurable retry with backoff, and timeout. |
| `internal/events/deadletter.go` | **Dead letter queue.** File-based persistent storage for failed event deliveries with configurable max retries. |
| `internal/events/metrics.go` | **Event delivery metrics.** Atomic counters for dead-letter, delivery success/failure/retry, and dropped events. |

### `internal/auth/` -- Authentication Interfaces

| File | Description |
|------|-------------|
| `internal/auth/auth.go` | **Unified auth interfaces.** `Identity` struct, `Provider` and `PermissionChecker` interfaces for pluggable authentication and authorization. |

### `internal/iam/` -- Identity and Access Management

| File | Description |
|------|-------------|
| `internal/iam/types.go` | **IAM data types.** All core IAM types with ARN constants and AWS-compatible structures. |
| `internal/iam/store.go` | **IAM BoltDB store.** Persistence layer for IAM data using BoltDB with atomic transactions. |
| `internal/iam/service.go` | **IAM service.** Core IAM operations: user/group/policy/role CRUD, access key management, temporary credential generation, policy evaluation dispatch. |
| `internal/iam/policy.go` | **Policy evaluator.** Full AWS IAM policy evaluation engine: parses JSON policies, evaluates conditions, implements Deny/Allow priority with SCP and permission boundary checks. |
| `internal/iam/admin_api.go` | **IAM admin HTTP API.** REST API handlers for all IAM CRUD operations. |
| `internal/iam/provider.go` | **IAM service provider interface.** Interface defining the contract for both local and remote IAM lookups. |
| `internal/iam/json.go` | **Custom JSON marshaling.** `StringOrSlice` type for AWS IAM policy fields that accept either a string or array. |
| `internal/iam/crypto.go` | **Master key management.** AES-256-GCM key generation, loading, and encryption/decryption for IAM secret keys. |
| `internal/iam/abac.go` | **Attribute-Based Access Control.** Tag resolution functions for ABAC policies. |
| `internal/iam/boundary.go` | **Permission boundaries.** Limits the maximum permissions an identity-based policy can grant. |
| `internal/iam/scp.go` | **Service Control Policies.** Organization-level SCP management constraining all IAM decisions. |
| `internal/iam/remote_service.go` | **Remote IAM service client.** `RemoteIAMService` implementing `IAMServiceProvider` via gRPC calls to `sts-service`. |
| `internal/iam/remote_admin_api.go` | **Remote IAM admin API.** `RemoteAdminAPI` proxying all admin HTTP operations to `sts-service` via gRPC for distributed mode. |

### `internal/ratelimit/` -- Rate Limiting

| File | Description |
|------|-------------|
| `internal/ratelimit/ratelimit.go` | **Rate limiter with circuit breaker.** Sliding window counter per-IP/per-user/per-bucket, token bucket for bandwidth limiting, circuit breaker pattern. |

### `internal/cache/` -- Caching

| File | Description |
|------|-------------|
| `internal/cache/cache.go` | **LRU object cache.** In-memory TTL-based LRU cache for object data with byte-size tracking and concurrent-safe operations. |

### `internal/common/` -- Shared Types

| File | Description |
|------|-------------|
| `internal/common/types.go` | **Shared types and utilities.** Context key constants, request ID helpers, `StorageTier` enum, `ObjectMetadata` struct, bucket/object name validation. |

### `internal/kms/` -- Key Management Service

| File | Description |
|------|-------------|
| `internal/kms/kms.go` | **KMS client interface.** `KMSClient` interface defining `GenerateDataKey`, `DecryptDataKey`, `GetPublicKey`, `Close`. |
| `internal/kms/local.go` | **Local KMS implementation.** ECDSA P-256 key pair with ECIES for encrypting/decrypting data encryption keys. |
| `internal/kms/aws.go` | **AWS KMS implementation.** Client wrapping AWS KMS APIs. |
| `internal/kms/vault.go` | **HashiCorp Vault Transit KMS implementation.** Client wrapping Vault Transit API. |
| `internal/kms/fallback.go` | **KMS fallback wrapper.** Degradation modes and DEK caching for availability during KMS outages. |

### `internal/services/` -- Encryption Microservices

| File | Description |
|------|-------------|
| `internal/services/types.go` | **Shared crypto types.** Interfaces (`KeyGenerator`, `KeyUnwrapper`, `DataEncryptor`, `DataDecryptor`, `KeyStorer`) for the crypto microservice ecosystem. |
| `internal/services/coordinator.go` | **Encryption coordinator.** Orchestrates the full encryption/decryption workflow across all crypto microservices. |
| `internal/services/registry.go` | **Service registry.** Consul-based service registration and discovery. |
| `internal/services/grpc_clients.go` | **gRPC client adapters.** Client wrappers for all crypto services converting between internal types and protobuf types. |
| `internal/services/opa.go` | **Open Policy Agent client.** HTTP client for evaluating OPA Rego policies for encryption key access control. |

### `internal/services/server/` -- gRPC Server Framework

| File | Description |
|------|-------------|
| `internal/services/server/server.go` | **Common gRPC server startup.** `StartService` function with TLS, Consul registration, health check, and graceful shutdown. |

### Microservice Implementations (`internal/services/*_service/`)

Each service subdirectory contains `service.go` (core logic) and `grpc.go` (gRPC adapter):

| Directory | Service |
|-----------|---------|
| `token_service/` | Ed25519 delegation token issuance/validation/revocation |
| `sts_service/` | STS AssumeRole + IAM admin gRPC proxy |
| `keygen_service/` | AES-256 DEK generation via ECIES |
| `keyunwrap_service/` | DEK decryption (unwrapping) via ECDH |
| `encrypt_service/` | AES-256-GCM data encryption |
| `decrypt_service/` | AES-256-GCM data decryption |
| `keystore_service/` | Encrypted DEK persistent storage |

### `internal/observability/` -- Observability

| File | Description |
|------|-------------|
| `internal/observability/metrics.go` | **Prometheus metrics registry.** Comprehensive metrics for all subsystems. |
| `internal/observability/tracing.go` | **OpenTelemetry tracing.** OTLP gRPC exporter setup. |
| `internal/observability/health.go` | **Health check endpoints.** `/healthz` and `/readyz` HTTP handlers with pluggable dependency checks. |
| `internal/observability/logger.go` | **Structured logging initialization.** `slog`-based logger with JSON format and trace/span ID injection. |

### `internal/logger/` -- Legacy Logger

| File | Description |
|------|-------------|
| `internal/logger/logger.go` | **Zap logger wrapper.** Global zap logger initialization with configurable level, format, and output path. |

### `internal/bootstrap/` -- System Bootstrap

| File | Description |
|------|-------------|
| `internal/bootstrap/crypto.go` | **Crypto bootstrap.** Creates `EncryptionCoordinator` (local or distributed gRPC mode) and appropriate `KMSClient`. |

### `internal/s3/` -- S3 Protocol Helpers

| File | Description |
|------|-------------|
| `internal/s3/ssec.go` | **SSE-C header parser.** Parses and validates S3 SSE-C headers, returns decoded AES-256 key. |

### `internal/fts/` -- Full-Text Search

| File | Description |
|------|-------------|
| `internal/fts/index.go` | **Inverted index.** Core FTS inverted index built on BoltDB with BM25 scoring. |
| `internal/fts/segment.go` | **Index segment.** In-memory/flushed index segment with posting lists and merge operations. |
| `internal/fts/tokenizer.go` | **Text tokenizer.** Multi-language tokenizer with Unicode normalization, stop word removal, and stemming. |
| `internal/fts/bm25.go` | **BM25 scorer.** BM25 ranking algorithm with configurable K1 and B parameters. |
| `internal/fts/hybrid.go` | **Hybrid search fusion.** Combines BM25 and vector search results using weighted averaging or RRF. |
| `internal/fts/compaction.go` | **Segment compaction manager.** Background goroutine merging small segments into larger ones. |

### `internal/raft/` -- Raft Consensus

| File | Description |
|------|-------------|
| `internal/raft/raft.go` | **Raft node wrapper.** Wraps HashiCorp Raft with BoltDB transport and FSM. |
| `internal/raft/fsm.go` | **BoltDB-based FSM.** Implements `raft.FSM` interface for replicated metadata operations. |
| `internal/raft/proxy.go` | **Raft metadata proxy.** Routes writes through Raft consensus, reads locally. |
| `internal/raft/bootstrap.go` | **Raft cluster bootstrapping.** Single-node and multi-node cluster initialization. |
| `internal/raft/roles.go` | **Raft role helpers.** `IsLeader`, `GetLeaderAddr`, `State`, `LinearizableRead` utility methods. |

### `internal/errors/` -- Domain Errors

| File | Description |
|------|-------------|
| `internal/errors/errors.go` | **S3-compatible error types.** Domain error struct with S3 XML error codes and HTTP status mapping. |

### `internal/units/` -- Unit Conversions

| File | Description |
|------|-------------|
| `internal/units/bytes.go` | **Size parsing.** `ParseSize` converts "10GB" to bytes; `FormatSize` converts bytes to human-readable strings. |

### `internal/taskqueue/` -- Background Task Queue

| File | Description |
|------|-------------|
| `internal/taskqueue/task.go` | **Task model.** Task struct with ID, kind, payload, priority, status lifecycle, retry tracking. |
| `internal/taskqueue/queue.go` | **Task queue.** Worker pool queue with configurable concurrency, retry with exponential backoff, dead-letter escalation, and Prometheus metrics. |
| `internal/taskqueue/store.go` | **Task store interface.** `Store` interface for task persistence; `MemoryStore` implementation. |
| `internal/taskqueue/bolt_store.go` | **BoltDB task store.** `BoltStore` with priority-ordered retrieval and crash recovery. |
| `internal/taskqueue/payloads.go` | **Well-known task payloads.** `VectorizePayload`, `FTSPayload`, `PipelinePayload` structs. |

### `internal/replication/` -- Replication

| File | Description |
|------|-------------|
| `internal/replication/replication.go` | **Cross-region replication engine.** Replication rules, bidirectional sync, bandwidth throttling, conflict resolution, and scheduled sync. |

### `internal/backup/` -- Backup & Restore

| File | Description |
|------|-------------|
| `internal/backup/backup.go` | **Full backup manager.** Creates tar.gz backups with configurable retention, listing, restore, and verification. |
| `internal/backup/incremental.go` | **Incremental backup.** Change-log-based incremental backups with point-in-time restore orchestration. |
| `internal/backup/remote.go` | **Remote backup transport.** Push/pull backups to/from S3-compatible storage or local paths with encryption. |

### `internal/scheduler/` -- Cron Scheduler

| File | Description |
|------|-------------|
| `internal/scheduler/scheduler.go` | **Cron-based task scheduler.** Manages scheduled tasks (tiering, scrub, key rotation, cleanup, compaction, replication, lifecycle) using `robfig/cron`. |

---

## `/proto/` -- Protobuf Definitions

### Source Files (.proto)

| File | Description |
|------|-------------|
| `proto/common.proto` | Shared protobuf types for all crypto services. |
| `proto/token.proto` | Token service gRPC contract. |
| `proto/keygen.proto` | KeyGen service gRPC contract. |
| `proto/keyunwrap.proto` | KeyUnwrap service gRPC contract. |
| `proto/encrypt.proto` | Encrypt service gRPC contract (streaming). |
| `proto/decrypt.proto` | Decrypt service gRPC contract (streaming). |
| `proto/keystore.proto` | KeyStore service gRPC contract. |
| `proto/sts.proto` | STS service gRPC contract. |

### Generated Go Files

| Directory | Contents |
|-----------|----------|
| `proto/common/` | `common.pb.go` |
| `proto/token/` | `token.pb.go`, `token_grpc.pb.go` |
| `proto/keygen/` | `keygen.pb.go`, `keygen_grpc.pb.go` |
| `proto/keyunwrap/` | `keyunwrap.pb.go`, `keyunwrap_grpc.pb.go` |
| `proto/encrypt/` | `encrypt.pb.go`, `encrypt_grpc.pb.go` |
| `proto/decrypt/` | `decrypt.pb.go`, `decrypt_grpc.pb.go` |
| `proto/keystore/` | `keystore.pb.go`, `keystore_grpc.pb.go` |
| `proto/sts/` | `sts.pb.go`, `sts_grpc.pb.go` |

---

## Key Configuration Files

| File | Description |
|------|-------------|
| `config.yaml` | Default development configuration. |
| `config.prod.yaml` | Production configuration template. |
| `pipelines.yaml` | Content processing pipeline definitions (image optimizer). |
| `.golangci.yml` | Golangci-lint configuration with 40+ enabled linters. |

---

## Summary Statistics

| Category | Count |
|----------|-------|
| **Go source files** | ~145 |
| **Binary targets** | 8 (nexus + 7 crypto microservices + nexusctl CLI) |
| **Protobuf services** | 7 |
| **Internal packages** | ~30 distinct packages |
