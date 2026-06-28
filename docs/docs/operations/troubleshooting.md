# Troubleshooting

Common issues and their solutions.

## Service Won't Start

### Symptom: "address already in use"

```
Error: listen tcp :9000: bind: address already in use
```

**Solution**: Find and stop the conflicting process:

```bash
sudo lsof -i :9000
sudo kill <PID>
```

Or change the port in the configuration:

```yaml
gateway:
  address: ":9001"
```

### Symptom: "permission denied" on data directory

```
Error: open /var/lib/nexus/data: permission denied
```

**Solution**: Fix directory ownership:

```bash
sudo chown -R nexus:nexus /var/lib/nexus
```

### Symptom: Microservice connection refused

```
Error: rpc error: code = Unavailable desc = connection refused
```

**Solution**: Verify the microservice is running and the address is correct:

```bash
# Check service status
sudo systemctl status nexus-encrypt

# Test connectivity
grpcurl -plaintext localhost:50051 list

# Verify configuration
grep -A2 encrypt /etc/nexus/config.yaml
```

## Performance Issues

### Slow Object Uploads

**Possible causes and solutions**:

1. **Disk I/O bottleneck**: Check disk performance with `iostat -x 1`
2. **Network bandwidth**: Check with `iftop` or `nload`
3. **Encryption overhead**: Scale encrypt/decrypt services
4. **Rate limiting**: Increase rate limit values:

```yaml
gateway:
  rate_limit:
    requests_per_second: 500
    burst: 1000
```

### Slow List Operations

**Possible causes**:

1. **Large number of objects**: Use prefix-based listing
2. **Metadata store fragmentation**: Run compaction:

```bash
nexusctl metadata compact
```

### High Memory Usage

**Possible causes and solutions**:

1. **Large vector index**: Reduce index size or use quantization
2. **Cache growth**: Set cache limits:

```yaml
cache:
  max_size: "1GB"
  eviction: "lru"
```

3. **Connection leaks**: Check for unclosed response bodies in client code

## Data Issues

### Object Not Found (but should exist)

1. Verify the correct bucket and key:

```bash
nexusctl object head my-bucket path/to/object
```

2. Check if versioning is enabled and you're accessing the right version
3. Verify storage backend connectivity

### Corrupted Object

1. Verify object integrity:

```bash
nexusctl object verify my-bucket path/to/object
```

2. Restore from backup:

```bash
nexusctl backup restore /backups/latest.tar.gz --bucket my-bucket --key path/to/object
```

## Cluster Issues

### Raft Leader Election Failure

**Symptom**: Cluster is stuck with no leader

**Solution**:

1. Check connectivity between Raft nodes:

```bash
nexusctl cluster status
```

2. If a majority of nodes are down, force a new cluster:

```bash
# WARNING: This is a last resort
nexusctl cluster force-new --node-id node1
```

### Split Brain

**Symptom**: Two nodes both believe they are the leader

**Solution**:

1. Isolate the incorrect leader
2. Restart the Raft service on that node
3. Verify cluster state:

```bash
nexusctl cluster status
```

## Encryption Issues

### SSE-C Key Mismatch

**Symptom**: "InvalidSSECustomerKey" error

**Solution**: Ensure the correct customer-provided key is being used. Keys must
match exactly (including encoding).

### KMS Unreachable

**Symptom**: "KMS connection refused"

**Solution**:

1. Verify KMS endpoint is reachable:

```bash
curl -v https://kms.us-east-1.amazonaws.com/
```

2. Check IAM permissions for KMS access
3. Fall back to local KMS if needed:

```yaml
encryption:
  sse_kms:
    kms:
      backend: "local"
```

## Logging and Debugging

### Enable Debug Logging

```yaml
observability:
  logging:
    level: "debug"
```

Or via environment variable:

```bash
export NEXUS_OBSERVABILITY_LOGGING_LEVEL=debug
sudo systemctl restart nexus
```

### Collect Diagnostic Information

```bash
nexusctl debug --output /tmp/nexus-debug.tar.gz
```

This collects:

- Configuration (redacted)
- Log files
- Metrics snapshot
- Cluster status
- System information

### Check Metrics

```bash
# Overall health
curl http://localhost:9091/metrics | grep nexus_

# Request latency
curl http://localhost:9091/metrics | grep nexus_request_duration

# Error rates
curl http://localhost:9091/metrics | grep nexus_errors
```
