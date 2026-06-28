# Security Whitepaper

This document describes the security architecture, threat model, and
hardening recommendations for Nexus Object Storage.

## Threat Model

### Assets

| Asset | Classification | Description |
|-------|---------------|-------------|
| Object data | High | User-stored objects (may contain sensitive data) |
| Encryption keys | Critical | Keys used for SSE-S3 and SSE-KMS |
| IAM credentials | Critical | Access keys, secret keys, tokens |
| Metadata | Medium | Bucket/object metadata, IAM policies |
| Configuration | Medium | Service configuration including secrets |
| Audit logs | Medium | Access logs and admin API logs |

### Threat Actors

| Actor | Capability | Motivation |
|-------|-----------|------------|
| External attacker | Network access to API | Data theft, service disruption |
| Malicious insider | Valid credentials, possibly admin | Data theft, privilege escalation |
| Compromised service | Access to internal network | Lateral movement, key extraction |
| Supply chain | Modified dependencies | Backdoor insertion |

### Threat Scenarios

| Threat | Likelihood | Impact | Mitigation |
|--------|-----------|--------|------------|
| Credential theft | Medium | High | SigV4, MFA, short-lived tokens |
| Data exfiltration | Medium | Critical | Encryption at rest, TLS in transit |
| Privilege escalation | Low | Critical | ABAC, SCP, least-privilege policies |
| Denial of service | Medium | Medium | Rate limiting, circuit breakers |
| Key compromise | Low | Critical | HSM, key rotation, key wrapping |
| Metadata tampering | Low | High | Integrity checks, Raft consensus |

## Encryption

### At Rest

Nexus supports three server-side encryption modes:

#### SSE-S3 (Server-Side Encryption with Nexus-Managed Keys)

- Nexus generates and manages encryption keys
- Each object is encrypted with a unique data key
- Data keys are encrypted with a master key
- Master keys are stored in the KeyStore service

```
Object → Data Key (unique) → Master Key (rotated) → KeyStore
```

#### SSE-KMS (Server-Side Encryption with KMS)

- Uses an external KMS (AWS KMS, HashiCorp Vault, or local KMS)
- Per-bucket or per-object KMS key support
- Audit trail for key usage

```yaml
encryption:
  sse_kms:
    enabled: true
    kms:
      backend: "aws"
      key_id: "arn:aws:kms:us-east-1:123456789:key/..."
```

#### SSE-C (Server-Side Encryption with Customer-Provided Keys)

- Customer provides the encryption key with each request
- Nexus never stores the plaintext key
- Key is used for the operation and then discarded

### In Transit

- TLS 1.2+ required for all external communication
- mTLS for internal gRPC communication between microservices
- Certificate rotation supported

```yaml
gateway:
  tls:
    enabled: true
    cert_file: "/etc/nexus/tls/server.crt"
    key_file: "/etc/nexus/tls/server.key"
    min_version: "1.2"

services:
  tls:
    enabled: true
    ca_file: "/etc/nexus/tls/ca.crt"
    cert_file: "/etc/nexus/tls/service.crt"
    key_file: "/etc/nexus/tls/service.key"
```

## Authentication and Authorization

### Authentication

- **SigV4**: AWS Signature Version 4 for S3 API requests
- **Bearer Token**: JWT tokens for Admin API requests
- **STSSessionToken**: Temporary credentials via STS service

### Authorization

- **ABAC (Attribute-Based Access Control)**: Policies with conditions based on request attributes
- **SCP (Service Control Policies)**: Organization-level guardrails
- **Boundary Policies**: Maximum permission boundaries for users

### Policy Example

```json
{
  "Version": "2024-01-01",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:PutObject"],
      "Resource": ["arn:nexus:s3:::data-bucket/*"],
      "Condition": {
        "StringEquals": {
          "nexus:SourceIP": "10.0.0.0/8"
        },
        "Bool": {
          "nexus:SecureTransport": "true"
        }
      }
    }
  ]
}
```

## Hardening Recommendations

### Network Security

1. **Firewall rules**: Only expose necessary ports (9000 for S3 API)
2. **Internal network**: Microservice ports should not be externally accessible
3. **TLS everywhere**: Enable TLS on all endpoints
4. **Network policies**: In Kubernetes, use NetworkPolicy resources

### Access Control

1. **Principle of least privilege**: Grant minimal permissions
2. **Use STS for temporary credentials**: Avoid long-lived access keys
3. **Enable SCP**: Set maximum permission boundaries
4. **Regular credential rotation**: Rotate access keys periodically

### Key Management

1. **Use external KMS**: AWS KMS or HashiCorp Vault for production
2. **Key rotation**: Enable automatic key rotation
3. **Key wrapping**: All keys are wrapped with master keys
4. **HSM**: Use HSM-backed KMS for highest security

### Operational Security

1. **Audit logging**: Enable access logs for all API operations
2. **Monitoring**: Set up alerts for suspicious activity
3. **Regular updates**: Keep Nexus and dependencies updated
4. **Backup encryption**: Encrypt backups with separate keys

### Container Security

1. **Distroless base image**: Minimize attack surface
2. **Non-root user**: Run Nexus as non-root user
3. **Read-only filesystem**: Mount config as read-only where possible
4. **Security contexts**: In Kubernetes, use `runAsNonRoot: true`

```yaml
securityContext:
  runAsNonRoot: true
  runAsUser: 1000
  readOnlyRootFilesystem: true
  allowPrivilegeEscalation: false
  capabilities:
    drop: ["ALL"]
```

## Compliance

Nexus supports the following compliance requirements:

| Requirement | Implementation |
|-------------|---------------|
| Data encryption at rest | SSE-S3, SSE-KMS, SSE-C |
| Data encryption in transit | TLS 1.2+, mTLS |
| Access logging | Structured access logs |
| Key management | KMS integration, key rotation |
| Identity and access management | ABAC, SCP, STS |
| Audit trail | Admin API audit logs |

## Vulnerability Reporting

Report security vulnerabilities to security@nexus.dev. Please do not file
public issues for security vulnerabilities.
