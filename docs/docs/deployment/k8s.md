# Kubernetes Deployment

Deploy Nexus on Kubernetes using Helm.

## Prerequisites

- Kubernetes 1.24+
- Helm 3.8+
- PersistentVolume provisioner
- At least 3 worker nodes recommended

## Quick Install

```bash
helm repo add nexus https://nexus.github.io/charts
helm repo update
helm install nexus nexus/nexus \
  --namespace nexus --create-namespace \
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
      - nexus.example.com
    tls:
      - secretName: nexus-tls
        hosts:
          - nexus.example.com

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
helm install nexus nexus/nexus \
  --namespace nexus --create-namespace \
  -f values-override.yaml
```

## Using the Local Helm Chart

If deploying from source:

```bash
helm install nexus ./examples/helm \
  --namespace nexus --create-namespace \
  -f values-override.yaml
```

## Persistent Storage

Nexus requires persistent volumes for:

| Component | Mount Path | Recommended Size |
|-----------|-----------|------------------|
| Data      | /var/lib/nexus/data | 100Gi+ |
| Metadata  | /var/lib/nexus/metadata | 50Gi |
| Raft Log  | /var/lib/nexus/raft | 20Gi |
| Vector Index | /var/lib/nexus/vector-index | 20Gi |

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

Restrict access to Nexus services:

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
helm upgrade nexus nexus/nexus \
  --namespace nexus \
  -f values-override.yaml
```

For major version upgrades, review the migration guide first.

## Uninstalling

```bash
helm uninstall nexus --namespace nexus
```

!!! warning
    Uninstalling deletes all Nexus pods. Persistent volumes may be retained
    depending on your StorageClass reclaim policy.
