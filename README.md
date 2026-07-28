<div align="center">

<img src="Logo.png" alt="CipherLake" width="120" />

# CipherLake

**高性能 · S3 兼容 · 存算一体 · 零信任加密 · 智能对象存储**

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.21%2B-00ADD8.svg)](https://go.dev/)

</div>

---

CipherLake 是一款将存储、计算、搜索与零信任加密融合为统一平台的智能对象存储系统。传统对象存储只负责"存"和"取"，数据的价值需要额外的计算集群和搜索引擎来挖掘，数据的安全需要外部 KMS 来保障。CipherLake 将管线处理、向量索引和微服务化加密内置于存储层——**上传即处理、存储即索引、读取即解密**，无需编排外部服务。

## 核心特性

### 零信任加密（微服务架构）

CipherLake 的加密系统采用**微服务架构 + 信封加密（Envelope Encryption）**，将密钥生成、密钥解包、数据加密、数据解密、密钥存储、令牌管理拆分为 6 个独立微服务，通过 gRPC 通信，由 `EncryptionCoordinator` 编排：

- **密钥隔离** — KeyGenService 仅持有公钥（只能加密），KeyUnwrapService 仅持有私钥（只能解密），EncryptService/DecryptService 不持久化任何密钥
- **信封加密** — 每对象独立 DEK（AES-256），DEK 由 ECDSA 长期密钥加密存储，数据用 DEK 加密
- **ECDH 会话密钥** — 服务间 DEK 传输使用临时 ECDH 协商 + HKDF-SHA256 派生的会话密钥加密，前向安全
- **ECIES + AES-256-GCM** — DEK 长期加密使用 ECIES-P256，数据加密使用 AES-256-GCM（认证加密）
- **Ed25519 令牌签名** — 令牌签发与验证解耦，验证方只需公钥
- **OPA 策略引擎** — 所有加密/解密操作受 Rego 策略控制
- **全链路审计** — 每个微服务独立记录操作审计日志
- **双模式部署** — 嵌入式（单进程）或分布式（独立进程 + gRPC + mTLS + Consul）

> 详细架构文档见 [docs/encryption-microservices.md](docs/encryption-microservices.md)

### 存算一体

上传对象时自动触发管线处理——图片压缩、缩略图生成、PII 脱敏、元数据提取——处理结果立即可用，无需额外调度。管线通过 `pipelines.yaml` 声明式配置，支持自定义插件扩展。

### 原生向量搜索

内置 HNSW 和 IVF-PQ 向量索引引擎，支持 ONNX / OpenAI / 自定义 API 等多种嵌入提供者。对象上传后自动生成向量并索引，毫秒级语义检索，无需独立向量数据库。

### 智能分层存储

根据访问模式自动在 Hot / Warm / Cold / Archive 四层之间迁移数据，可配置调度策略和存储介质，冷热分离，成本最优。

### 安全与合规

- AWS Signature V4 + JWT (HS256) 双认证
- IAM 子系统：用户、组、策略、角色、Access Key、桶策略
- STS 临时安全令牌（AssumeRole）
- Virtual-hosted-style 请求支持（桶名从 Host 头解析，兼容浏览器直传场景）
- TLS/mTLS 支持，自动证书生成与热重载
- 多级速率限制（IP / 用户 / 桶 / API 方法）
- CORS 基于 Bucket 配置验证
- SSRF 防护（复制目标禁止私有/回环地址，内网部署可配置放行）
- 安全头中间件（HSTS / CSP / X-Frame-Options）

### 轻量部署

- BoltDB 嵌入式存储，零外部数据库依赖
- 单二进制部署，支持小型 VPS 和内网环境
- 加密微服务支持嵌入式模式，无需独立进程

---

## 快速开始

### 前置条件

- Go 1.21+

### 构建

```bash
# 构建主服务
go build -o cipherlake ./cmd/cipherlake

# 构建 CLI 工具
go build -o cipherlakectl ./cmd/cipherlakectl

# 构建加密微服务（分布式模式需要）
go build -o token-service     ./cmd/token-service
go build -o keygen-service    ./cmd/keygen-service
go build -o keyunwrap-service ./cmd/keyunwrap-service
go build -o encrypt-service   ./cmd/encrypt-service
go build -o decrypt-service   ./cmd/decrypt-service
go build -o keystore-service  ./cmd/keystore-service
go build -o sts-service       ./cmd/sts-service
```

### 启动

**嵌入式模式（默认，推荐开发/小型部署）：**

```bash
./cipherlake
```

所有加密微服务嵌入主进程内运行，无需额外配置。

**分布式模式（生产环境）：**

```bash
# 使用启动脚本（推荐）
python start_services.py

# 或手动逐个启动
./token-service     -port 50051 -key-path ./data/keys/token &
./keygen-service    -port 50052 -key-path ./data/keys/keygen &
./keyunwrap-service -port 50053 -key-path ./data/keys/keygen &
./encrypt-service   -port 50054 &
./decrypt-service   -port 50055 &
./keystore-service  -port 50056 -data-path ./data/keystore &
./sts-service       -port 50057 &
./cipherlake
```

在 `config.yaml` 中设置 `crypto_services.distributed_mode: true` 启用分布式模式。

### 配置

```bash
cp config.yaml config.local.yaml
```

| 配置段 | 说明 |
|--------|------|
| `node` | 节点角色、监听地址、数据目录、集群对等节点、域名（virtual-hosted-style） |
| `auth` | JWT 密钥、令牌有效期、匿名访问 |
| `encryption` | KMS 类型（local/vault）、主密钥路径、Vault Transit 密钥名 |
| `crypto_services` | 加密微服务配置：分布式模式、gRPC 地址、mTLS、密钥路径、审计大小 |
| `iam` | IAM 子系统：数据库路径、STS 服务地址 |
| `vector` | 嵌入提供者、维度、索引类型 |
| `pipelines` | 处理管线配置文件、并发数 |
| `tiering` | 热/温/冷存储层、调度策略 |
| `tls` | 证书路径、自动证书、最低版本 |
| `ratelimit` | IP/用户/桶/API 速率限制 |
| `replication` | 允许私有地址端点（内网部署） |
| `task_queue` | 后台任务队列：持久化、工作线程、重试策略、死信队列 |

### 环境变量

| 变量 | 说明 |
|------|------|
| `CIPHERLAKE_ADMIN_USER` | CLI 管理员用户名 |
| `CIPHERLAKE_ADMIN_PASSWORD` | CLI 管理员密码 |
| `CIPHERLAKE_ACCESS_KEY` | CLI S3 访问密钥 |
| `CIPHERLAKE_SECRET_KEY` | CLI S3 密钥 |
| `CIPHERLAKE_ACCESS_KEY_ID` | IAM Access Key ID |
| `CIPHERLAKE_SECRET_ACCESS_KEY` | IAM Secret Access Key |
| `CIPHERLAKE_REPLICATION_ALLOW_PRIVATE_ENDPOINT` | 允许内网复制目标 |

---

## 加密微服务架构

### 服务清单

| 服务 | 端口 | 密钥材料 | 职责 |
|------|------|----------|------|
| TokenService | 50051 | Ed25519 签名密钥 | 签发/验证读/写/删除令牌 |
| KeyGenService | 50052 | ECDSA P-256 公钥 | 生成 DEK，加密 DEK |
| KeyUnwrapService | 50053 | ECDSA P-256 私钥 | 解密 DEK，重新加密传输 |
| EncryptService | 50054 | 无 | 数据加密适配器（无持久密钥） |
| DecryptService | 50055 | 无 | 数据解密适配器（无持久密钥） |
| KeyStoreService | 50056 | 无 | 存储加密后的 DEK |
| STSService | 50057 | — | AssumeRole 临时凭证 |

### 加密流程

```
OPA 策略检查 → 生成 ECDH 临时密钥 → TokenService 签发令牌
  → KeyGenService 生成 DEK（ECIES 加密 + ECDH 会话密钥加密）
  → KeyStoreService 存储 EncryptedDEK
  → EncryptionCoordinator 本地解包会话 DEK 并流式加密数据（AES-256-GCM）
```

### 解密流程

```
OPA 策略检查 → 生成 ECDH 临时密钥 → TokenService 签发令牌
  → KeyStoreService 取出 EncryptedDEK
  → KeyUnwrapService 解密 DEK（ECDSA 私钥）+ ECDH 重新加密
  → EncryptionCoordinator 本地解包会话 DEK 并流式解密数据（AES-256-GCM）
```

> 完整架构设计、密码学方案、部署模式详见 [docs/encryption-microservices.md](docs/encryption-microservices.md)

---

## AI / 向量搜索

CipherLake 支持可插拔嵌入提供者：

| 提供者 | 配置值 | 说明 |
|--------|--------|------|
| Mock | `mock` | 确定性哈希嵌入（测试用） |
| ONNX | `onnx` | 本地 ONNX 模型推理 |
| OpenAI | `openai` | OpenAI 嵌入 API |
| 自定义 API | `api` | 任意 OpenAI 兼容端点 |

### 下载嵌入模型

使用 Modelscope SDK 下载多语言句嵌入模型：

```python
from modelscope import snapshot_download

model_dir = snapshot_download(
    'iic/gte_sentence-embedding_multilingual-base',
    cache_dir='/models'
)
```

配置 `config.yaml`：

```yaml
vector:
  enabled: true
  embedding_provider: "onnx"
  embedding_model_path: "/models/iic/gte_sentence-embedding_multilingual-base"
  dim: 768
```

---

## 存储管线

管线通过 `pipelines.yaml` 声明式配置，定义触发器、过滤器和处理步骤：

```yaml
pipelines:
  - name: "image-processing"
    trigger: "on_upload"
    filter: "content-type matches 'image/*' and size < 10MB"
    steps:
      - name: "compress"
        plugin: "image_compress"
        params:
          format: "webp"
          quality: "80"
        output: "{key}_compressed.webp"
      - name: "thumbnail"
        plugin: "thumbnail_generator"
        params:
          sizes: "128,256,512"
    priority: 10
    enabled: true
```

可用插件：`image_compress` · `image_resize` · `thumbnail_generator` · `image_metadata_extract` · `metadata_extract` · `encrypt_pii` · `video_thumbnail` · `pdf_to_text`

---

## 后台任务队列

向量索引、全文索引和管线处理等后台任务通过 `internal/taskqueue` 统一调度：

- **持久化存储**：任务默认持久化到 BoltDB（`data/tasks.db`），进程重启后可恢复执行
- **工作线程池**：可配置并发工作线程数，避免无限制 goroutine 导致资源耗尽
- **指数退避重试**：任务失败时按指数退避策略自动重试，支持最大重试次数
- **死信队列**：超过最大重试次数的任务进入死信队列，便于后续排查与人工处理
- **优先级**：高优先级任务优先被消费
- **截止期限**：任务可配置执行截止时间，超时自动转入死信队列

相关配置项：

```yaml
task_queue:
  enabled: true
  store_path: "data/tasks.db"
  workers: 4
  poll_interval: "500ms"
  retry_base: "1s"
  max_retry_delay: "5m"
  default_retry: 3
```

---

## CLI 使用（cipherlakectl）

### 集群状态

```bash
./cipherlakectl status
```

### 用户管理

```bash
# 创建用户
./cipherlakectl user create myuser --password mypass --role user --permissions read,write

# 列出用户
./cipherlakectl user list

# 查看用户详情
./cipherlakectl user get myuser

# 更新用户角色/权限
./cipherlakectl user update myuser --role admin --permissions read,write,delete

# 修改密码
./cipherlakectl user passwd myuser --password newpass

# 删除用户
./cipherlakectl user delete myuser
```

### 存储桶管理

```bash
# 创建桶
./cipherlakectl bucket create mybucket --acl private

# 列出桶
./cipherlakectl bucket list

# 查看桶信息
./cipherlakectl bucket info mybucket

# 列出桶内对象
./cipherlakectl bucket objects mybucket

# 设置桶 ACL
./cipherlakectl bucket set-acl mybucket --acl public-read

# 获取桶 ACL
./cipherlakectl bucket get-acl mybucket

# 删除桶
./cipherlakectl bucket delete mybucket
```

### IAM 管理

```bash
# --- 用户 ---
./cipherlakectl iam create-user myuser --display-name "My User"
./cipherlakectl iam list-users
./cipherlakectl iam delete-user myuser

# --- Access Key ---
./cipherlakectl iam create-access-key myuser --description "CI/CD key"
./cipherlakectl iam list-access-keys myuser
./cipherlakectl iam delete-access-key --user myuser --key-id AKIA...

# --- 组 ---
./cipherlakectl iam group create developers --description "Dev team"
./cipherlakectl iam group list
./cipherlakectl iam group add-user --user myuser --group developers
./cipherlakectl iam group remove-user --user myuser --group developers
./cipherlakectl iam group delete developers

# --- 策略 ---
./cipherlakectl iam policy create read-only --file policy.json --description "Read only access"
./cipherlakectl iam policy list
./cipherlakectl iam policy attach-user-policy --user myuser --policy read-only
./cipherlakectl iam policy detach-user-policy --user myuser --policy read-only
./cipherlakectl iam policy attach-group-policy --group developers --policy read-only
./cipherlakectl iam policy delete read-only

# --- 角色 ---
./cipherlakectl iam role create cross-account --trust-policy-file trust.json --max-session 3600
./cipherlakectl iam role list
./cipherlakectl iam role delete cross-account

# --- 桶策略 ---
./cipherlakectl iam bucket-policy set mybucket --file bucket-policy.json
./cipherlakectl iam bucket-policy get mybucket
./cipherlakectl iam bucket-policy delete mybucket
```

### 加密操作

```bash
# 查看加密状态
./cipherlakectl crypto status

# 轮换加密密钥
./cipherlakectl crypto rotate --bucket mybucket
```

### 存储分层

```bash
# 触发分层迁移
./cipherlakectl tiering run --bucket mybucket

# 查看分层状态
./cipherlakectl tiering status
```

### 向量索引

```bash
# 重建向量索引
./cipherlakectl vector rebuild --bucket mybucket

# 查看向量索引统计
./cipherlakectl vector stats
```

### 性能分析

```bash
# CPU Profile
./cipherlakectl profile --cpu 30 --output profile.out
```

---

## 架构

```
┌─────────────────────────────────────────────────────────────────┐
│                        Gateway                                   │
│   S3 API │ Admin API │ IAM API │ Auth │ CORS │ RateLimit        │
├─────────────────────────────────────────────────────────────────┤
│                   Encryption Coordinator                         │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐           │
│  │TokenSvc  │ │KeyGenSvc │ │KeyUnwrap │ │EncryptSvc│           │
│  │:50051    │ │:50052    │ │:50053    │ │:50054    │           │
│  └──────────┘ └──────────┘ └──────────┘ └──────────┘           │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐                        │
│  │DecryptSvc│ │KeyStore  │ │STS Service│  OPA Policy Engine     │
│  │:50055    │ │:50056    │ │:50057    │                        │
│  └──────────┘ └──────────┘ └──────────┘                        │
├─────────────────────────────────────────────────────────────────┤
│                     Storage Engine                               │
│   Hot Tier │ Warm Tier │ Cold Tier │ Archive                    │
├─────────────────────────────────────────────────────────────────┤
│                    Compute Pipelines                              │
│   Compress │ Resize │ Thumbnail │ PII │ Vector                  │
├─────────────────────────────────────────────────────────────────┤
│                      Core Services                               │
│   Metadata │ Cache │ Replication │ IAM │ Tiering                │
└─────────────────────────────────────────────────────────────────┘
```

---

## 文档

| 文档 | 说明 |
|------|------|
| [加密微服务架构](docs/encryption-microservices.md) | 微服务设计、密码学方案、完整加解密流程、部署模式、OPA 策略 |

---

## 测试

```bash
go test ./... -count=1
```

---

## 许可证

[MIT](LICENSE)
