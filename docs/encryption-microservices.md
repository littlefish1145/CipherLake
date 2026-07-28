# 加密微服务架构

本文档详细说明 CipherLake 加密系统从单体模块重构为微服务架构的设计与实现。

---

## 1. 设计动机

### 1.1 单体架构的问题

此前，CipherLake 的加密功能以单体模块形式内嵌在主进程中：

- **密钥集中风险** — 加密密钥、解密密钥、DEK 全部在同一进程内存中，任一漏洞即可泄露全部密钥材料
- **无法独立扩展** — 加密和解密的负载特征不同（写少读多），无法分别伸缩
- **审计边界模糊** — 所有操作在同一进程内完成，难以实现操作级审计隔离
- **密钥轮换困难** — 轮换需要重启整个服务

### 1.2 微服务架构的目标

- **最小权限原则** — 每个服务只持有完成其职责所需的最少密钥材料
- **独立部署与扩展** — 各服务可独立水平扩展、独立升级
- **操作级审计** — 每个服务维护独立的审计日志
- **传输安全** — 服务间通过 mTLS + ECDH 会话密钥双重保护
- **策略驱动** — OPA 策略引擎控制加密操作的访问决策

---

## 2. 架构总览

```
                              ┌──────────────────────┐
                              │    CipherLake Gateway      │
                              │  (S3 API / Admin API) │
                              └──────────┬───────────┘
                                         │
                              ┌──────────▼───────────┐
                              │ EncryptionCoordinator │
                              │      (编排层)         │
                              └──────────┬───────────┘
                                         │ gRPC
              ┌──────────────┬───────────┼───────────┬──────────────┐
              │              │           │           │              │
     ┌────────▼───────┐ ┌───▼──────┐ ┌──▼───────┐ ┌▼───────────┐ ┌▼──────────────┐
     │  TokenService  │ │KeyGenSvc │ │KeyUnwrap │ │EncryptSvc  │ │ DecryptSvc    │
     │  :50051        │ │:50052    │ │:50053    │ │:50054      │ │ :50055        │
     │  令牌签发/验证  │ │DEK 生成  │ │DEK 解密  │ │数据加密     │ │ 数据解密      │
     │  Ed25519 签名  │ │ECDSA 公钥│ │ECDSA 私钥│ │无持久密钥   │ │ 无持久密钥    │
     └────────────────┘ └────┬─────┘ └────┬─────┘ └────────────┘ └───────────────┘
                             │            │
                     ┌───────▼────────────▼───────┐
                     │     KeyStoreService        │
                     │        :50056              │
                     │  存储/检索加密后的 DEK       │
                     │  (独立文件存储)              │
                     └────────────────────────────┘

              ┌──────────────────────────────────────┐
              │           STSService :50057          │
              │   临时安全令牌服务 (AssumeRole)       │
              └──────────────────────────────────────┘

              ┌──────────────────────────────────────┐
              │       OPA Policy Engine              │
              │   encryption.rego 策略评估           │
              └──────────────────────────────────────┘
```

---

## 3. 微服务详解

### 3.1 TokenService（令牌服务）

| 属性 | 值 |
|------|-----|
| 端口 | `:50051` |
| 密钥 | Ed25519 签名密钥 |
| 职责 | 签发读/写/删除令牌，验证令牌有效性 |
| gRPC 方法 | `IssueWriteToken`, `IssueReadToken`, `IssueDeleteToken`, `ValidateToken` |

令牌包含 `tokenID`、`userID`、`bucket`、`objectKey`、操作类型和过期时间（默认 30 秒）。令牌签发与验证解耦：验证方只需公钥，无需访问签发状态。

### 3.2 KeyGenService（密钥生成服务）

| 属性 | 值 |
|------|-----|
| 端口 | `:50052` |
| 密钥 | 仅持有 ECDSA P-256 长期**公钥** |
| 职责 | 生成随机 DEK，用公钥加密 DEK，用 ECDH 会话密钥二次加密 DEK |
| gRPC 方法 | `GenerateDataKey`, `GetPublicKey` |

**关键设计：KeyGenService 只有公钥，无法解密任何数据。** 即使该服务被攻破，攻击者也无法解密已存储的 DEK。

DEK 生成流程：
1. 生成 32 字节随机 DEK（AES-256）
2. 用长期 ECDSA 公钥通过 ECIES 加密 DEK → `EncryptedDEK`（持久化到 KeyStore）
3. 生成临时 ECDH 密钥对，与客户端公钥协商共享密钥
4. 用 HKDF-SHA256 从共享密钥派生 32 字节会话密钥
5. 用会话密钥通过 AES-256-GCM 加密 DEK → `ECDHEncryptedDEK`（传给 EncryptService）

### 3.3 KeyUnwrapService（密钥解包服务）

| 属性 | 值 |
|------|-----|
| 端口 | `:50053` |
| 密钥 | 持有 ECDSA P-256 长期**私钥**（SK_Decrypt） |
| 职责 | 用私钥解密 `EncryptedDEK` 得到明文 DEK，再用 ECDH 会话密钥重新加密 |
| gRPC 方法 | `UnwrapKey` |

**关键设计：KeyUnwrapService 只有私钥，无法加密新数据。** 加密和解密操作在物理上分离到不同服务。

密钥解包流程：
1. 用 ECDSA 私钥解密 `EncryptedDEK`（ECIES 逆操作）得到明文 DEK
2. 生成临时 ECDH 密钥对，与客户端公钥协商共享密钥
3. 用会话密钥加密 DEK → `ECDHEncryptedDEK`（传给 DecryptService）

### 3.4 EncryptService（加密服务）

| 属性 | 值 |
|------|-----|
| 端口 | `:50054` |
| 密钥 | 无持久密钥 |
| 职责 | 用 ECDH 派生的会话密钥解密传入的 DEK，再用 DEK 加密数据 |
| gRPC 方法 | `Encrypt`（流式）, `EncryptChunk`（单次）, `Health` |

加密流程：
1. 用客户端 ECDH 私钥 + 服务端 ECDH 公钥派生会话密钥
2. 用会话密钥解密 `ECDHEncryptedDEK` 得到明文 DEK
3. 用 DEK 通过 AES-256-GCM 加密明文数据
4. 返回密文 + nonce（12 字节）+ authTag（16 字节）
5. 立即清零内存中的 DEK 和会话密钥

### 3.5 DecryptService（解密服务）

| 属性 | 值 |
|------|-----|
| 端口 | `:50055` |
| 密钥 | 无持久密钥 |
| 职责 | 用 ECDH 派生的会话密钥解密传入的 DEK，再用 DEK 解密数据 |
| gRPC 方法 | `Decrypt`（流式）, `DecryptChunk`（单次）, `Health` |

解密流程与加密对称，使用相同的 ECDH 密钥派生和 AES-256-GCM 解密。

### 3.6 KeyStoreService（密钥存储服务）

| 属性 | 值 |
|------|-----|
| 端口 | `:50056` |
| 密钥 | 无密钥 |
| 职责 | 存储/检索/删除加密后的 DEK |
| gRPC 方法 | `StoreKey`, `GetKey`, `DeleteKey`, `ListKeys` |

使用独立文件存储（`./data/keystore`），以 `bucket/objectKey` 为索引存储 `EncryptedDEK`。由于 DEK 已被加密，KeyStoreService 不需要任何密钥材料。

### 3.7 STSService（临时安全令牌服务）

| 属性 | 值 |
|------|-----|
| 端口 | `:50057` |
| 职责 | 实现 AssumeRole，签发临时访问凭证 |
| gRPC 方法 | `AssumeRole` |

---

## 4. 完整加密流程

### 4.1 加密操作（EncryptOperation）

由 `EncryptionCoordinator.EncryptOperation` 编排：

```
客户端请求加密 (userID, bucket, objectKey, plaintext)
  │
  ├─ 1. OPA 策略评估 (write + encrypt_object)
  │     └─ 拒绝 → 返回 "access denied by policy"
  │
  ├─ 2. 生成客户端临时 ECDH 密钥对 (P-256)
  │
  ├─ 3. TokenService.IssueWriteToken(userID, bucket, objectKey, 30s)
  │     └─ 返回 writeToken (Ed25519 签名)
  │
  ├─ 4. KeyGenService.GenerateDataKey(tokenID, userID, bucket, objectKey, clientECDHPub)
  │     ├─ 生成 32B 随机 DEK
  │     ├─ ECIES 加密 DEK → EncryptedDEK
  │     ├─ ECDH 协商 + HKDF → 会话密钥
  │     └─ 会话密钥加密 DEK → ECDHEncryptedDEK
  │     返回: (EncryptedDEK, ECDHEncryptedDEK, serviceECDHPub)
  │
  ├─ 5. KeyStoreService.StoreKey(bucket, objectKey, EncryptedDEK)
  │     └─ 返回 keyID
  │
  ├─ 6. EncryptionCoordinator 本地创建流式加密 reader
  │     ├─ ECDH 派生会话密钥 → 解密 ECDHEncryptedDEK → 明文 DEK
  │     ├─ AES-256-GCM 分块加密明文,不整对象入内存
  │     └─ 清零 DEK 和会话密钥
  │     返回: (ciphertext reader, nonce metadata)
  │
  └─ 返回: (ciphertext, keyID, nonce+authTag)
```

### 4.2 解密操作（DecryptOperation）

由 `EncryptionCoordinator.DecryptOperation` 编排：

```
客户端请求解密 (userID, bucket, objectKey, ciphertext, keyID, metadata)
  │
  ├─ 1. OPA 策略评估 (read + decrypt_object)
  │
  ├─ 2. 生成客户端临时 ECDH 密钥对 (P-256)
  │
  ├─ 3. TokenService.IssueReadToken(userID, bucket, objectKey, "", 30s)
  │
  ├─ 4. KeyStoreService.GetKey(bucket, objectKey)
  │     └─ 返回 EncryptedDEK
  │
  ├─ 5. KeyUnwrapService.UnwrapKey(tokenID, userID, bucket, objectKey, EncryptedDEK, clientECDHPub)
  │     ├─ ECDSA 私钥解密 EncryptedDEK → 明文 DEK
  │     ├─ ECDH 协商 + HKDF → 会话密钥
  │     ├─ 会话密钥加密 DEK → ECDHEncryptedDEK
  │     └─ 清零 DEK
  │     返回: (ECDHEncryptedDEK, serviceECDHPub)
  │
  ├─ 6. EncryptionCoordinator 本地创建流式解密 reader
  │     ├─ ECDH 派生会话密钥 → 解密 ECDHEncryptedDEK → 明文 DEK
  │     ├─ AES-256-GCM 分块解密密文,不整对象入内存
  │     └─ 清零 DEK 和会话密钥
  │     返回: plaintext reader
  │
  └─ 返回: plaintext
```

---

## 5. 密码学方案

### 5.1 密钥层次

```
长期 ECDSA P-256 密钥对
  ├── 公钥 → KeyGenService (加密 DEK)
  └── 私钥 → KeyUnwrapService (解密 DEK)

临时 ECDH P-256 密钥对 (每次操作生成)
  └── HKDF-SHA256 → 32B 会话密钥 (服务间 DEK 传输加密)

随机 DEK (32B, 每对象独立)
  └── AES-256-GCM (数据加密)
```

### 5.2 加密算法

| 用途 | 算法 | 说明 |
|------|------|------|
| DEK 长期加密 | ECIES-P256-AES-256-GCM | 用长期 ECDSA 公钥加密 DEK，持久化存储 |
| DEK 传输加密 | ECDH-P256 + HKDF-SHA256 + AES-256-GCM | 临时 ECDH 协商会话密钥，加密 DEK 在服务间传输 |
| 数据加密 | AES-256-GCM | 认证加密，提供机密性 + 完整性 |
| 令牌签名 | Ed25519 | 非对称签名，验证方只需公钥 |
| 密钥派生 | HKDF-SHA256 | 从 ECDH 共享密钥派生对称密钥 |

### 5.3 安全措施

- **密钥隔离** — 加密服务只有公钥，解密服务只有私钥，加/解密服务无持久密钥
- **临时密钥** — 每次操作生成新的 ECDH 密钥对，前向安全
- **内存清零** — DEK 和会话密钥使用后立即 `clearBytes()` 清零
- **mTLS** — 分布式模式下服务间通信强制双向 TLS
- **OPA 策略** — 所有加密/解密操作需通过策略评估
- **审计日志** — 每个服务独立记录操作审计

---

## 6. 部署模式

### 6.1 嵌入式模式（默认）

`config.yaml` 中 `crypto_services.distributed_mode: false` 时，所有微服务以库形式嵌入 CipherLake 主进程内运行，通过直接函数调用通信，无需启动独立进程。

适用于：单机部署、开发测试、小型 VPS。

### 6.2 分布式模式

`crypto_services.distributed_mode: true` 时，各服务作为独立进程运行，通过 gRPC 通信。支持 Consul 服务发现和 mTLS。

适用于：生产环境、需要独立扩展、多节点部署。

启动方式：

```bash
# 使用启动脚本（推荐）
python start_services.py

# 或手动逐个启动
./token-service     -port 50051 -key-path ./data/keys/token
./keygen-service    -port 50052 -key-path ./data/keys/keygen
./keyunwrap-service -port 50053 -key-path ./data/keys/keygen
./encrypt-service   -port 50054
./decrypt-service   -port 50055
./keystore-service  -port 50056 -data-path ./data/keystore
./sts-service       -port 50057
./cipherlake
```

### 6.3 配置参考

```yaml
crypto_services:
  enabled: true
  distributed_mode: false          # true = 独立进程, false = 嵌入式
  key_path: "./data/keys"          # ECDSA 密钥对存储路径
  keystore_path: "./data/keystore" # 加密 DEK 存储路径
  opa_address: ""                  # OPA 策略引擎地址
  consul_address: ""               # Consul 服务发现地址
  audit_size: 10000                # 审计日志最大条数
  # gRPC 地址（仅 distributed_mode=true 时使用）
  token_service_addr: "localhost:50051"
  keygen_service_addr: "localhost:50052"
  keyunwrap_service_addr: "localhost:50053"
  encrypt_service_addr: "localhost:50054"
  decrypt_service_addr: "localhost:50055"
  keystore_service_addr: "localhost:50056"
  # mTLS（仅 distributed_mode=true 时使用）
  mtls_cert_file: ""
  mtls_key_file: ""
  mtls_ca_file: ""
```

---

## 7. OPA 策略

加密操作受 OPA 策略控制，策略定义在 `policy/encryption.rego`：

- **默认要求加密** — 所有对象默认需要加密
- **敏感桶强制加密** — `secrets`、`credentials`、`private`、`confidential` 桶必须加密
- **敏感元数据触发加密** — 对象元数据 `sensitive=true` 时强制加密
- **大文件强制加密** — 超过 1MB 的对象必须加密
- **公共桶豁免** — `public`、`assets`、`thumbnails` 桶默认不加密
- **小文件豁免** — 非敏感桶中 1KB 以下对象可不加密

---

## 8. 服务发现

分布式模式下支持 Consul 服务注册与发现：

- 每个服务启动时向 Consul 注册，配置 TTL 健康检查
- `EncryptionCoordinator` 通过 Consul 发现服务实例
- 支持多实例负载均衡（返回第一个健康实例）
- 不健康实例 5 分钟后自动注销

---

## 9. 审计

每个微服务维护独立的内存审计日志，记录：

| 字段 | 说明 |
|------|------|
| Timestamp | 操作时间 |
| Operation | 操作类型（generate / unwrap / encrypt / decrypt） |
| KeyID | 密钥标识 |
| TokenID | 令牌标识 |
| UserID | 用户标识 |
| Bucket | 桶名 |
| ObjectKey | 对象键 |
| Result | 操作结果（success / failure） |

审计日志最大条数通过 `audit_size` 配置，默认 10000。
