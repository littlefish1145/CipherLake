# Admin API Reference

The Admin API provides management operations for Nexus. It is served on a
separate port (default 9001) and requires admin authentication.

## Base URL

```
http://<nexus-host>:9001/admin/v1
```

## Authentication

Admin API requests require an admin token in the `Authorization` header:

```
Authorization: Bearer <admin-token>
```

Admin tokens are created via `nexusctl user create --admin`.

## Cluster Management

### Get Cluster Status

```
GET /cluster/status
```

Returns the current cluster state including Raft leader and member health.

**Response**:

```json
{
  "leader": "node1",
  "members": [
    {"id": "node1", "address": "node1:9090", "state": "leader", "healthy": true},
    {"id": "node2", "address": "node2:9090", "state": "follower", "healthy": true},
    {"id": "node3", "address": "node3:9090", "state": "follower", "healthy": true}
  ],
  "commit_index": 12345
}
```

### Join Cluster

```
POST /cluster/join
```

Adds a new node to the cluster.

**Body**:

```json
{
  "node_id": "node4",
  "address": "node4:9090"
}
```

### Remove Node

```
POST /cluster/remove
```

Removes a node from the cluster.

**Body**:

```json
{
  "node_id": "node4"
}
```

## IAM Management

### Create User

```
POST /iam/users
```

**Body**:

```json
{
  "username": "app-user",
  "access_key": "AKIA...",
  "secret_key": "secret...",
  "policy_arn": "arn:nexus:policy:::read-only"
}
```

### List Users

```
GET /iam/users
```

### Delete User

```
DELETE /iam/users/{username}
```

### Create Policy

```
POST /iam/policies
```

**Body**:

```json
{
  "name": "read-only",
  "document": {
    "Version": "2024-01-01",
    "Statement": [
      {
        "Effect": "Allow",
        "Action": ["s3:GetObject", "s3:ListBucket"],
        "Resource": ["arn:nexus:s3:::my-bucket/*"]
      }
    ]
  }
}
```

### Attach Policy

```
POST /iam/users/{username}/policies
```

**Body**:

```json
{
  "policy_arn": "arn:nexus:policy:::read-only"
}
```

## Bucket Management

### Get Bucket Info

```
GET /buckets/{bucket}
```

Returns detailed bucket information including size, object count, and settings.

### Set Bucket Versioning

```
PUT /buckets/{bucket}/versioning
```

**Body**:

```json
{
  "status": "Enabled"
}
```

### Get Bucket Encryption

```
GET /buckets/{bucket}/encryption
```

### Set Bucket Encryption

```
PUT /buckets/{bucket}/encryption
```

**Body**:

```json
{
  "algorithm": "AES256",
  "kms_key_id": ""
}
```

## Backup Management

### Create Backup

```
POST /backups
```

**Body**:

```json
{
  "type": "full",
  "destination": "local"
}
```

### List Backups

```
GET /backups
```

### Restore Backup

```
POST /backups/{backup_id}/restore
```

## Health and Metrics

### Health Check

```
GET /health
```

Returns the health status of all components.

**Response**:

```json
{
  "status": "healthy",
  "components": {
    "gateway": "healthy",
    "metadata": "healthy",
    "storage": "healthy",
    "encrypt_service": "healthy",
    "decrypt_service": "healthy",
    "keygen_service": "healthy",
    "keystore_service": "healthy",
    "keyunwrap_service": "healthy",
    "token_service": "healthy",
    "sts_service": "healthy"
  }
}
```

### Readiness Check

```
GET /ready
```

Returns `200 OK` when all components are ready to serve traffic.
