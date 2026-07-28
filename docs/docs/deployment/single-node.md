# Single Node Deployment

This guide covers deploying CipherLake on a single server for production use.

## System Requirements

| Component | Minimum | Recommended |
|-----------|---------|-------------|
| CPU       | 2 cores | 4+ cores    |
| RAM       | 4 GB    | 8+ GB       |
| Disk      | 50 GB   | 500+ GB SSD |
| OS        | Ubuntu 22.04 / RHEL 9 | Ubuntu 22.04 / RHEL 9 |

## Installation

### Option 1: Binary Installation

```bash
# Download the latest release
curl -L https://github.com/cipherlake/cipherlake/releases/latest/download/cipherlake-linux-amd64 -o /usr/local/bin/cipherlake
curl -L https://github.com/cipherlake/cipherlake/releases/latest/download/cipherlakectl-linux-amd64 -o /usr/local/bin/cipherlakectl
chmod +x /usr/local/bin/cipherlake /usr/local/bin/cipherlakectl
```

### Option 2: Docker

```bash
docker pull ghcr.io/cipherlake/cipherlake:latest
```

## Configuration

1. Create the data directory:

```bash
sudo mkdir -p /var/lib/cipherlake/data
sudo mkdir -p /var/lib/cipherlake/metadata
sudo useradd -r -s /bin/false cipherlake
sudo chown -R cipherlake:cipherlake /var/lib/cipherlake
```

2. Create the configuration file:

```bash
sudo cp config.yaml /etc/cipherlake/config.yaml
sudo editor /etc/cipherlake/config.yaml
```

3. Set your credentials:

```yaml
gateway:
  address: ":9000"
  access_key: "your-secure-access-key"
  secret_key: "your-secure-secret-key"
```

## Running with systemd

1. Install the systemd unit files:

```bash
sudo cp packaging/systemd/cipherlake.service /etc/systemd/system/
sudo cp packaging/systemd/cipherlake-*.service /etc/systemd/system/
sudo cp packaging/systemd/cipherlake.target /etc/systemd/system/
```

2. Enable and start all services:

```bash
sudo systemctl daemon-reload
sudo systemctl enable cipherlake.target
sudo systemctl start cipherlake.target
```

3. Verify all services are running:

```bash
sudo systemctl status cipherlake.target
```

## Running with Docker Compose

```bash
docker compose -f deploy/docker-compose.yml up -d
```

## TLS Configuration

For production, enable TLS:

```yaml
gateway:
  tls:
    enabled: true
    cert_file: "/etc/cipherlake/tls/server.crt"
    key_file: "/etc/cipherlake/tls/server.key"
```

Use certbot for Let's Encrypt certificates:

```bash
sudo certbot certonly --standalone -d cipherlake.example.com
sudo cp /etc/letsencrypt/live/cipherlake.example.com/fullchain.pem /etc/cipherlake/tls/server.crt
sudo cp /etc/letsencrypt/live/cipherlake.example.com/privkey.pem /etc/cipherlake/tls/server.key
```

## Health Checks

CipherLake exposes health endpoints:

```bash
# Gateway health
curl http://localhost:9000/health

# Readiness (all services connected)
curl http://localhost:9000/ready
```

## Firewall Configuration

Open the following ports:

| Port | Purpose       |
|------|---------------|
| 9000 | S3 API        |
| 9001 | Admin API     |
| 9091 | Prometheus metrics |

```bash
sudo ufw allow 9000/tcp
sudo ufw allow 9001/tcp
sudo ufw allow 9091/tcp
```
