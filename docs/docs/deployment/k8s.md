# Kubernetes Deployment

Deploy CipherLake on Kubernetes using Helm.

## Prerequisites

- Kubernetes 1.24+
- Helm 3.8+
- PersistentVolume provisioner
- At least 3 worker nodes recommended

## Quick Install

```bash
helm repo add cipherlake https://cipherlake.github.io/charts
helm repo update
helm install cipherlake cipherlake/cipherlake \
  --namespace cipherlake --create-namespace \
  --set gateway.accessKey=my-access-key \
  --set gateway.secretKey=my-secret-key
```

## Configuration

Create a `values-override.yaml` file:

```yaml
# Gateway configuration
gateway:
  replicas: 3
  accessKey: "my-access-key"
  secretKey: "my-secret-key"
  resources:
    requests:
      cpu: "500m"
      memory: "512Mi"
    limits:
      cpu: "2"
      memory: "2Gi"
  service:
    type: ClusterIP
    port: 9000
  ingress:
    enabled: true
    className: "nginx"
    hosts:
      - cipherlake.example.com
    tls:
      - secretName: cipherlake-tls
        hosts:
          - cipherlake.example.com

# Metadata (Raft) configuration
metadata:
  replicas: 3
  persistence:
    enabled: true
    size: 50Gi
    storageClass: "ssd"

# Microservice configurations
encryptService:
  replicas: 2
decryptService:
  replicas: 2
keygenService:
  replicas: 1
keystoreService:
  replicas: 1
keyunwrapService:
  replicas: 1
tokenService:
  replicas: 1
stsService:
  replicas: 1

# Storage configuration
storage:
  backend: "local"
  persistence:
    enabled: true
    size: 500Gi
    storageClass: "ssd"

# Observability
observability:
  metrics:
    enabled: true
    serviceMonitor:
      enabled: true
      namespace: monitoring
  tracing:
    enabled: false
```

Install with the override file:

```bash
helm install cipherlake cipherlake/cipherlake \
  --namespace cipherlake --create-namespace \
  -f values-override.yaml
```

## Using the Local Helm Chart

If deploying from source:

```bash
helm install cipherlake ./examples/helm \
  --namespace cipherlake --create-namespace \
  -f values-override.yaml
```

## Persistent Storage

CipherLake requires persistent volumes for:

| Component | Mount Path | Recommended Size |
|-----------|-----------|------------------|
| Data      | /var/lib/cipherlake/data | 100Gi+ |
| Metadata  | /var/lib/cipherlake/metadata | 50Gi |
| Raft Log  | /var/lib/cipherlake/raft | 20Gi |
| Vector Index | /var/lib/cipherlake/vector-index | 20Gi |

Use a StorageClass with SSD backing for production:

```yaml
persistence:
  storageClass: "premium-rwo"
```

## Horizontal Pod Autoscaling

```yaml
autoscaling:
  enabled: true
  minReplicas: 3
  maxReplicas: 10
  targetCPUUtilizationPercentage: 70
  targetMemoryUtilizationPercentage: 80
```

## Network Policies

Restrict access to CipherLake services:

```yaml
networkPolicy:
  enabled: true
  ingress:
    from:
      - namespaceSelector:
          matchLabels:
            name: production
      - podSelector:
          matchLabels:
            app: my-app
```

## Upgrading

```bash
helm repo update
helm upgrade cipherlake cipherlake/cipherlake \
  --namespace cipherlake \
  -f values-override.yaml
```

For major version upgrades, review the migration guide first.

## Uninstalling

```bash
helm uninstall cipherlake --namespace cipherlake
```

!!! warning
    Uninstalling deletes all CipherLake pods. Persistent volumes may be retained
    depending on your StorageClass reclaim policy.
