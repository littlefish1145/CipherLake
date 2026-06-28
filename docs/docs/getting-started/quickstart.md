# Quick Start

Get Nexus up and running in under 5 minutes using Docker Compose.

## Prerequisites

- Docker 20.10+ and Docker Compose v2
- 4 GB RAM minimum
- 10 GB free disk space

## Step 1: Clone and Configure

```bash
git clone https://github.com/nexus/nexus.git
cd nexus
cp config.yaml config.local.yaml
```

Edit `config.local.yaml` and set your access credentials:

```yaml
gateway:
  address: ":9000"
  access_key: "my-access-key"
  secret_key: "my-secret-key"
```

## Step 2: Start Services

```bash
docker compose -f deploy/docker-compose.yml up -d
```

Wait for all services to become healthy:

```bash
docker compose -f deploy/docker-compose.yml ps
```

All services should show status `healthy`.

## Step 3: Verify

```bash
# Check health endpoint
curl http://localhost:9000/health

# Create a bucket using the CLI
./nexusctl bucket create my-first-bucket

# Upload a file
./nexusctl object put my-first-bucket test.txt ./test.txt

# List objects
./nexusctl object list my-first-bucket
```

## Step 4: Use the S3 API

Using the AWS CLI:

```bash
aws --endpoint-url http://localhost:9000 s3 mb s3://test-bucket
aws --endpoint-url http://localhost:9000 s3 cp file.txt s3://test-bucket/
aws --endpoint-url http://localhost:9000 s3 ls s3://test-bucket/
```

Using the Go SDK:

```go
package main

import (
    "context"
    "log"
    "strings"

    s3sdk "nexus/sdk/go/s3"
)

func main() {
    client, err := s3sdk.NewClient(s3sdk.Config{
        Endpoint:  "http://localhost:9000",
        Region:    "us-east-1",
        AccessKey: "my-access-key",
        SecretKey: "my-secret-key",
    })
    if err != nil {
        log.Fatal(err)
    }

    ctx := context.Background()
    err = client.PutObject(ctx, "my-bucket", "hello.txt", strings.NewReader("Hello, Nexus!"))
    if err != nil {
        log.Fatal(err)
    }
    log.Println("Object uploaded successfully!")
}
```

Using the Python SDK:

```python
from nexus import NexusClient

client = NexusClient(
    endpoint_url="http://localhost:9000",
    access_key="my-access-key",
    secret_key="my-secret-key",
)

client.create_bucket("my-bucket")
client.put_object("my-bucket", "hello.txt", open("hello.txt", "rb"))
```

## Next Steps

- [Configuration Reference](config.md) - Customize your deployment
- [Single Node Deployment](../deployment/single-node.md) - Production single-node setup
- [S3 API Reference](../api/s3.md) - Supported S3 operations
