# gRPC Service Reference

Nexus uses gRPC for internal communication between microservices. These
services are not intended for external use but are documented for operators
and developers.

## Common Messages

### EncryptionKey

```protobuf
message EncryptionKey {
  string key_id = 1;
  bytes encrypted_key = 2;
  bytes iv = 3;
  string algorithm = 4;
}
```

### ServiceHealth

```protobuf
message ServiceHealth {
  string service_name = 1;
  bool healthy = 2;
  string message = 3;
  int64 uptime_seconds = 4;
}
```

## Encrypt Service

**Package**: `nexus.encrypt`
**Port**: 50051

### Encrypt

Encrypts data using the specified algorithm.

```protobuf
rpc Encrypt(EncryptRequest) returns (EncryptResponse);

message EncryptRequest {
  bytes plaintext = 1;
  string algorithm = 2;  // AES-256-GCM, AES-256-CBC
  bytes key_id = 3;      // Optional: use specific key
}

message EncryptResponse {
  bytes ciphertext = 1;
  bytes iv = 2;
  string key_id = 3;
  bytes tag = 4;          // GCM auth tag
}
```

### EncryptStream

Streaming encryption for large objects.

```protobuf
rpc EncryptStream(stream EncryptStreamRequest) returns (stream EncryptStreamResponse);
```

## Decrypt Service

**Package**: `nexus.decrypt`
**Port**: 50052

### Decrypt

Decrypts previously encrypted data.

```protobuf
rpc Decrypt(DecryptRequest) returns (DecryptResponse);

message DecryptRequest {
  bytes ciphertext = 1;
  bytes iv = 2;
  string key_id = 3;
  bytes tag = 4;
  string algorithm = 5;
}

message DecryptResponse {
  bytes plaintext = 1;
}
```

### DecryptStream

Streaming decryption for large objects.

```protobuf
rpc DecryptStream(stream DecryptStreamRequest) returns (stream DecryptStreamResponse);
```

## KeyGen Service

**Package**: `nexus.keygen`
**Port**: 50053

### GenerateKey

Generates a new encryption key.

```protobuf
rpc GenerateKey(GenerateKeyRequest) returns (GenerateKeyResponse);

message GenerateKeyRequest {
  string algorithm = 1;   // AES-256
  string purpose = 2;     // SSE-S3, SSE-KMS, SSE-C
}

message GenerateKeyResponse {
  string key_id = 1;
  bytes key = 2;          // Plaintext key (encrypted in transit via TLS)
  bytes encrypted_key = 3; // Key encrypted with master key
}
```

### RotateKey

Rotates an existing key.

```protobuf
rpc RotateKey(RotateKeyRequest) returns (RotateKeyResponse);

message RotateKeyRequest {
  string key_id = 1;
}

message RotateKeyResponse {
  string new_key_id = 1;
  bytes new_encrypted_key = 2;
}
```

## KeyStore Service

**Package**: `nexus.keystore`
**Port**: 50054

### StoreKey

Stores an encryption key.

```protobuf
rpc StoreKey(StoreKeyRequest) returns (StoreKeyResponse);

message StoreKeyRequest {
  string key_id = 1;
  bytes encrypted_key = 2;
  map<string, string> metadata = 3;
}

message StoreKeyResponse {
  bool success = 1;
}
```

### RetrieveKey

Retrieves a stored encryption key.

```protobuf
rpc RetrieveKey(RetrieveKeyRequest) returns (RetrieveKeyResponse);

message RetrieveKeyRequest {
  string key_id = 1;
}

message RetrieveKeyResponse {
  bytes encrypted_key = 1;
  map<string, string> metadata = 2;
}
```

### DeleteKey

Deletes a stored key.

```protobuf
rpc DeleteKey(DeleteKeyRequest) returns (DeleteKeyResponse);

message DeleteKeyRequest {
  string key_id = 1;
}

message DeleteKeyResponse {
  bool success = 1;
}
```

## KeyUnwrap Service

**Package**: `nexus.keyunwrap`
**Port**: 50055

### UnwrapKey

Unwraps an encrypted key using the master key.

```protobuf
rpc UnwrapKey(UnwrapKeyRequest) returns (UnwrapKeyResponse);

message UnwrapKeyRequest {
  bytes encrypted_key = 1;
  string master_key_id = 2;
  string algorithm = 3;
}

message UnwrapKeyResponse {
  bytes plaintext_key = 1;
}
```

## Token Service

**Package**: `nexus.token`
**Port**: 50056

### IssueToken

Issues a new authentication token.

```protobuf
rpc IssueToken(IssueTokenRequest) returns (IssueTokenResponse);

message IssueTokenRequest {
  string access_key = 1;
  string secret_key = 2;
  int64 ttl_seconds = 3;
  repeated string scopes = 4;
}

message IssueTokenResponse {
  string token = 1;
  int64 expires_at = 2;
}
```

### ValidateToken

Validates an authentication token.

```protobuf
rpc ValidateToken(ValidateTokenRequest) returns (ValidateTokenResponse);

message ValidateTokenRequest {
  string token = 1;
}

message ValidateTokenResponse {
  bool valid = 1;
  string access_key = 2;
  repeated string scopes = 3;
  int64 expires_at = 4;
}
```

### RevokeToken

Revokes a previously issued token.

```protobuf
rpc RevokeToken(RevokeTokenRequest) returns (RevokeTokenResponse);

message RevokeTokenRequest {
  string token = 1;
}

message RevokeTokenResponse {
  bool success = 1;
}
```

## STS Service

**Package**: `nexus.sts`
**Port**: 50057

### AssumeRole

Assumes an IAM role and returns temporary credentials.

```protobuf
rpc AssumeRole(AssumeRoleRequest) returns (AssumeRoleResponse);

message AssumeRoleRequest {
  string role_arn = 1;
  string session_name = 2;
  int64 duration_seconds = 3;
  string policy = 4;
}

message AssumeRoleResponse {
  string access_key = 1;
  string secret_key = 2;
  string session_token = 3;
  int64 expiration = 4;
}
```

### GetSessionToken

Returns temporary credentials for the requesting user.

```protobuf
rpc GetSessionToken(GetSessionTokenRequest) returns (GetSessionTokenResponse);

message GetSessionTokenRequest {
  int64 duration_seconds = 1;
}

message GetSessionTokenResponse {
  string access_key = 1;
  string secret_key = 2;
  string session_token = 3;
  int64 expiration = 4;
}
```
