# Nexus Alerting Runbook

## Alert: Encryption RPC Error Rate > 1% for 5 minutes

**Metric:** `rate(nexus_encrypt_duration_seconds_count{status="error"}[5m]) / rate(nexus_encrypt_duration_seconds_count[5m]) > 0.01`

**Severity:** Critical

**Impact:** Objects cannot be encrypted at rest, potentially exposing plaintext data.

**Diagnosis:**
1. Check encryption service health: `grpc_health_probe <encrypt-service-addr>`
2. Review service logs for connection refused or timeout errors
3. Verify KMS/key management service availability
4. Check mTLS certificate expiry if distributed mode is used

**Resolution:**
- Restart the encryption service pod
- Rotate expired mTLS certificates
- Verify network connectivity between gateway and crypto services
- If KMS is down, fall back to local key mode temporarily

---

## Alert: Replication Lag > 60 seconds

**Metric:** `nexus_replication_lag_seconds > 60`

**Severity:** Warning

**Impact:** Replicated objects may be stale or missing on the destination, risking data loss on primary failure.

**Diagnosis:**
1. Check replication worker health and queue depth
2. Verify network connectivity to destination endpoint
3. Check destination storage capacity and write latency
4. Review replication rule configuration for misconfigured endpoints

**Resolution:**
- Scale up replication workers if queue is backed up
- Fix network issues to destination endpoint
- Clear disk space on destination if full
- Temporarily pause and re-enable the replication rule to reset stuck workers

---

## Alert: Disk Usage > 85%

**Metric:** Node filesystem usage exceeds 85%

**Severity:** Warning

**Impact:** Approaching disk full condition can cause write failures, metadata corruption, and service degradation.

**Diagnosis:**
1. Check per-tier storage usage: `df -h /var/lib/nexus/{hot,warm,cold,archive}`
2. Identify large or orphaned objects
3. Check if tiering is moving data correctly from hot to warm/cold
4. Review multipart upload incomplete parts

**Resolution:**
- Trigger manual tiering migration to move cold data
- Clean up aborted multipart uploads
- Expand storage volume
- Adjust tiering thresholds to migrate data sooner

---

## Alert: Vector Search P99 Latency > 1 second

**Metric:** `histogram_quantile(0.99, rate(nexus_vector_search_duration_seconds_bucket[5m])) > 1`

**Severity:** Warning

**Impact:** Semantic search queries are slow, degrading user experience.

**Diagnosis:**
1. Check `nexus_vector_index_size_vectors` gauge — large indexes may need optimization
2. Review embedding API latency if using external provider
3. Check CPU and memory usage on the gateway node
4. Verify index type is appropriate for dataset size (HNSW vs IVF)

**Resolution:**
- Switch to a more efficient index type (e.g., HNSW for large datasets)
- Increase query cache TTL
- Scale up gateway resources (CPU/memory)
- Reduce `max_search_top_k` to limit result set size
- Pre-compute embeddings if using external API with high latency

---

## Alert: Dead Letter Queue Growing

**Metric:** `rate(nexus_event_delivery_total{status="dead_letter"}[5m]) > 0`

**Severity:** Warning

**Impact:** Events are failing delivery after max retries, potentially missing notifications for object changes.

**Diagnosis:**
1. Inspect dead letter directory for failed event payloads
2. Check webhook endpoint availability and response codes
3. Review webhook timeout configuration
4. Verify network connectivity to webhook targets

**Resolution:**
- Fix the downstream webhook endpoint
- Increase `webhook_timeout` if targets are slow
- Increase `max_retries` for transient failures
- Replay dead letter events after fixing the endpoint
- Add webhook endpoint health monitoring
