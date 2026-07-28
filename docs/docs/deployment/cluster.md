# Cluster Deployment

Deploy CipherLake in a clustered configuration for high availability and horizontal scaling.

## Architecture

A CipherLake cluster consists of:

- **Gateway nodes**: Stateless S3 API endpoints (scale horizontally)
- **Metadata cluster**: Raft-based consensus group (3 or 5 nodes recommended)
- **Storage nodes**: Data storage backends (scale based on capacity)
- **Microservice pool**: Encryption and key management services (scale based on load)

```
                    ┌─────────┐
                    │  LB /   │
                    │  VIP    │
                    └────┬────┘
                         │
          ┌──────────────┼──────────────┐
          │              │              │
    ┌─────┴─────┐ ┌─────┴─────┐ ┌─────┴─────┐
    │ Gateway 1 │ │ Gateway 2 │ │ Gateway 3 │
    └─────┬─────┘ └─────┬─────┘ └─────┬─────┘
          │              │              │
          └──────────────┼──────────────┘
                         │
          ┌──────────────┼──────────────┐
          │              │              │
    ┌─────┴─────┐ ┌─────┴─────┐ ┌─────┴─────┐
    │  Meta 1   │ │  Meta 2   │ │  Meta 3   │
    │ (Leader)  │ │ (Follower)│ │ (Follower)│
    └───────────┘ └───────────┘ └───────────┘
```

## Prerequisites

- 3+ nodes for metadata (Raft consensus)
- 2+ gateway nodes
- Shared or distributed storage backend
- Load balancer (HAProxy, Nginx, or cloud LB)

## Step 1: Configure Metadata Cluster

On each metadata node, configure the Raft cluster:

```yaml
metadata:
  backend: "boltdb"
  path: "/var/lib/cipherlake/metadata.db"
  raft:
    enabled: true
    data_dir: "/var/lib/cipherlake/raft"
    bind_address: ":9090"
    peers:
      - "node1:9090"
      - "node2:9090"
      - "node3:9090"
```

Bootstrap the first node:

```bash
cipherlakectl cluster bootstrap --node-id node1 --address node1:9090
```

Join additional nodes:

```bash
cipherlakectl cluster join --node-id node2 --address node2:9090 --leader node1:9090
cipherlakectl cluster join --node-id node3 --address node3:9090 --leader node1:9090
```

## Step 2: Configure Gateway Nodes

All gateway nodes share the same configuration:

```yaml
node:
  domain: "s3.example.com"       # Domain for virtual-hosted-style requests

gateway:
  address: ":9000"
  access_key: "${ACCESS_KEY}"
  secret_key: "${SECRET_KEY}"

metadata:
  raft:
    enabled: true
    peers:
      - "node1:9090"
      - "node2:9090"
      - "node3:9090"
```

The `node.domain` field enables virtual-hosted-style S3 requests where the
bucket name is extracted from the `Host` header (e.g.
`mybucket.s3.example.com`). Multiple domains can be specified as a
comma-separated list (e.g. `"s3.example.com,localhost"`).

## Step 3: Configure Load Balancer

Example HAProxy configuration:

```
frontend cipherlake_s3
    bind *:9000
    default_backend cipherlake_gateways

backend cipherlake_gateways
    balance roundrobin
    option httpchk GET /health
    server gw1 gateway1:9000 check
    server gw2 gateway2:9000 check
    server gw3 gateway3:9000 check
```

## Step 4: Scale Microservices

Deploy encryption microservices based on load requirements:

```yaml
services:
  encrypt:
    address: "encrypt-service:50051"
  decrypt:
    address: "decrypt-service:50052"
  keygen:
    address: "keygen-service:50053"
  keystore:
    address: "keystore-service:50054"
  keyunwrap:
    address: "keyunwrap-service:50055"
  token:
    address: "token-service:50056"
  sts:
    address: "sts-service:50057"
```

Each microservice can be scaled independently behind a service discovery layer.

## Monitoring

Monitor cluster health:

```bash
# Check Raft leader
cipherlakectl cluster status

# Check node health
cipherlakectl cluster health

# View metrics
curl http://gateway1:9091/metrics
```

## Failure Scenarios

| Scenario | Impact | Recovery |
|----------|--------|----------|
| Gateway node failure | No impact (LB routes to healthy nodes) | Auto-recovery or restart |
| Metadata follower failure | No impact (writes continue) | Restart node, re-join cluster |
| Metadata leader failure | Brief write pause during election | Raft auto-elects new leader |
| Storage backend failure | Read/write errors for affected data | Restore from backup or replica |
