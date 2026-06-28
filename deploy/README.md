# Nexus + Milvus 全栈编排

一键启动 Nexus 对象存储(分布式加密微服务)和 Milvus 向量数据库。

## 架构

```
                    ┌──────────────────────────────────────────┐
                    │              Docker Network               │
                    │              nexus-net (bridge)            │
                    │                                          │
  ┌─────────┐       │  ┌──────────────────────────────────┐    │
  │ Client  │───────┼──┤  Nexus (:8080)                    │    │
  │ S3 SDK  │       │  │  S3 API + Admin API + Vector API  │    │
  └─────────┘       │  └──────┬───────────────────────────┘    │
                    │         │                                 │
                    │    ┌────┴────┐  ┌──────────┐             │
                    │    │ Milvus  │  │ 6 Crypto  │             │
                    │    │:19530   │  │ Services │             │
                    │    └──┬───┬──┘  └────┬─────┘             │
                    │       │   │          │                   │
                    │  ┌────┘   └────┐  ┌──┴───┐               │
                    │  │etcd   minio │  │ keys │               │
                    │  │:2379  :9000 │  │(vol) │               │
                    │  └──────┴──────┘  └──────┘               │
                    └──────────────────────────────────────────┘
```

## 密钥自动生成机制

无需手动初始化密钥。各服务首次启动时自动生成并持久化:

| 服务 | 密钥类型 | 路径(容器内) | 说明 |
|------|---------|--------------|------|
| keygen-service | ECDSA P-256 | `/var/lib/nexus/keys/keygen.priv` `.pub` | 对象加密主密钥,首次启动自动生成 |
| keyunwrap-service | (共享) | 同上(只读) | 读取 keygen 生成的私钥进行解包 |
| token-service | Ed25519 | `/var/lib/nexus/keys/token.priv` `.pub` | 令牌签名密钥,首次启动自动生成 |
| keystore-service | BoltDB | `/var/lib/nexus/keystore/` | DEK 持久化存储,自动创建 |

密钥持久化在 `nexus-keys` Docker 卷中,重启不丢失。清除数据需 `docker compose down -v`。

## 快速开始

### 1. 构建 Nexus 镜像

```bash
cd nexus
docker build -t nexus:latest .
```

### 2. 启动全栈

```bash
cd deploy
docker compose -f docker-compose.full.yml up -d
```

首次启动会自动构建镜像(如果不存在)。Milvus 启动较慢(约 30-60 秒),Nexus 会等待 Milvus 健康后启动。

### 3. 验证

```bash
# Nexus 健康检查
curl http://localhost:8080/healthz

# Milvus 健康检查
curl http://localhost:9091/healthz

# 查看密钥生成日志
docker compose -f docker-compose.full.yml logs keygen-service | grep "generated"
docker compose -f docker-compose.full.yml logs token-service | grep "generated"
```

### 4. 使用 S3 API

```bash
# 创建桶
curl -X PUT http://localhost:8080/my-bucket

# 上传对象
echo "Hello Nexus" | curl -X PUT -d @- http://localhost:8080/my-bucket/hello.txt

# 下载对象
curl http://localhost:8080/my-bucket/hello.txt

# 向量搜索
curl -X POST http://localhost:8080/_vector/search \
  -H "Content-Type: application/json" \
  -d '{"query": "storage systems", "top_k": 5}'
```

## 服务端口

| 服务 | 端口 | 说明 |
|------|------|------|
| Nexus S3 API | 8080 | S3 兼容接口 |
| Nexus Admin API | 8081 | 管理接口 |
| Milvus gRPC | 19530 | 向量数据库 gRPC |
| Milvus Health | 9091 | 健康检查 / metrics |

## 配置

编辑 [config.docker.yaml](config.docker.yaml),修改后重启:

```bash
docker compose -f docker-compose.full.yml restart nexus
```

常用配置项:
- `vector.milvus.address` — Milvus 地址(默认 `milvus:19530`)
- `vector.milvus.index_type` — 索引类型(`HNSW` / `IVF_FLAT` / `IVF_SQ8` / `DISKANN`)
- `crypto_services.distributed_mode` — `true` 分布式,`false` 嵌入式
- `embedding_provider` — `mock` / `api` / `onnx`

## 自定义 Embedding API

如需使用 OpenAI 兼容 API 生成 embedding:

```yaml
vector:
  embedding_provider: "api"
  embedding_api_endpoint: "http://your-llm:1234/v1/embeddings"
  embedding_api_key: "any"
  embedding_model_name: "text-embedding-model"
```

## 管理命令

```bash
# 查看所有服务状态
docker compose -f docker-compose.full.yml ps

# 查看日志
docker compose -f docker-compose.full.yml logs -f nexus
docker compose -f docker-compose.full.yml logs -f milvus

# 停止
docker compose -f docker-compose.full.yml down

# 停止并清除所有数据(含密钥)
docker compose -f docker-compose.full.yml down -v
```

## 故障排查

### Nexus 启动失败:Milvus 连接超时

Milvus 首次启动需要加载模型,可能超过 60 秒。增大 start_period:

```yaml
milvus:
  healthcheck:
    start_period: 120s
```

### protobuf 冲突警告

日志中出现 `proto: file "common.proto" is already registered` 是已知问题(Milvus SDK 与项目 proto 冲突),不影响功能。compose 中已设置 `GOLANG_PROTOBUF_REGISTRATION_CONFLICT=warn`。

### 密钥丢失

如果 `nexus-keys` 卷被删除,需重新生成密钥:

```bash
docker compose -f docker-compose.full.yml down -v
docker compose -f docker-compose.full.yml up -d
```

密钥重新生成后,之前加密的数据将无法解密。
