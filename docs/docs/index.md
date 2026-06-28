# Nexus Object Storage

Nexus is an enterprise-grade, S3-compatible object storage system built with a
microservices architecture. It provides advanced features including vector
search, full-text search, server-side encryption, and multi-tier storage.

## Key Features

- **S3-Compatible API**: Drop-in replacement for applications using AWS S3
- **Microservices Architecture**: Independently scalable encryption, key management, and token services
- **Vector Search**: Built-in vector similarity search for AI/ML workloads
- **Full-Text Search**: BM25 and hybrid search across stored objects
- **Server-Side Encryption**: SSE-C, SSE-S3, and SSE-KMS support
- **Erasure Coding**: Configurable data durability with Reed-Solomon erasure coding
- **Multi-Tier Storage**: Automatic tiering between hot, warm, and cold storage
- **IAM & ABAC**: Fine-grained access control with attribute-based policies
- **Raft Consensus**: Strong consistency for metadata with HashiCorp Raft
- **Background Task Queue**: Persistent task scheduling with retries and dead-letter queue for vector/FTS indexing and pipelines
- **Observability**: Built-in Prometheus metrics, OpenTelemetry tracing, and structured logging

## Quick Links

- [Quick Start Guide](getting-started/quickstart.md) - Get Nexus running in 5 minutes
- [Configuration Reference](getting-started/config.md) - All configuration options
- [Deployment Guide](deployment/single-node.md) - Deploy to production
- [API Reference](api/s3.md) - S3-compatible API documentation

## Architecture Overview

```
                    ┌──────────────┐
                    │   Gateway    │
                    │  (S3 API)    │
                    └──────┬───────┘
                           │
          ┌────────────────┼────────────────┐
          │                │                │
   ┌──────┴──────┐  ┌─────┴──────┐  ┌──────┴──────┐
   │   Encrypt   │  │  Metadata   │  │   Storage    │
   │  Service    │  │   Store     │  │   Engine     │
   └──────┬──────┘  └────────────┘  └─────────────┘
          │
   ┌──────┴──────┐
   │   Decrypt   │
   │  Service    │
   └─────────────┘
```

## License

Nexus is licensed under the Apache License 2.0.
