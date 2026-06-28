# Backup and Restore

Procedures for backing up and restoring Nexus data.

## Backup Strategy

Nexus supports two backup modes:

1. **Full backup**: Complete snapshot of all data and metadata
2. **Incremental backup**: Only changes since the last backup

## Full Backup

### Using nexusctl

```bash
# Create a full backup
nexusctl backup create --output /backups/nexus-full-$(date +%Y%m%d).tar.gz

# Verify backup integrity
nexusctl backup verify /backups/nexus-full-20240101.tar.gz
```

### Manual Backup

1. Stop write operations (optional but recommended):

```bash
nexusctl cluster maintenance --enable
```

2. Copy data directories:

```bash
# Metadata database
sudo cp /var/lib/nexus/metadata.db /backups/metadata.db.bak

# Raft data
sudo tar czf /backups/raft-$(date +%Y%m%d).tar.gz -C /var/lib/nexus raft/

# Object data
sudo tar czf /backups/data-$(date +%Y%m%d).tar.gz -C /var/lib/nexus data/
```

3. Resume operations:

```bash
nexusctl cluster maintenance --disable
```

## Incremental Backup

Incremental backups capture only changes since the last backup, reducing
storage and time requirements.

```bash
# Create incremental backup
nexusctl backup create --incremental --output /backups/nexus-incr-$(date +%Y%m%d).tar.gz

# List backup chain
nexusctl backup list
```

Incremental backups form a chain. To restore, you need the full backup plus
all incremental backups in sequence.

## Remote Backup

Store backups in a remote S3-compatible bucket:

```yaml
backup:
  remote:
    enabled: true
    endpoint: "https://s3.amazonaws.com"
    bucket: "nexus-backups"
    access_key: "${BACKUP_ACCESS_KEY}"
    secret_key: "${BACKUP_SECRET_KEY}"
    prefix: "backups/"
```

```bash
nexusctl backup create --remote --output s3://nexus-backups/backups/full-$(date +%Y%m%d).tar.gz
```

## Automated Backups

Configure scheduled backups in the configuration file:

```yaml
backup:
  incremental:
    enabled: true
    interval: "6h"
    retention: 7
```

Or use a cron job:

```bash
# /etc/cron.d/nexus-backup
0 */6 * * * nexus nexusctl backup create --incremental --output /backups/incr-$(date +\%Y\%m\%d-\%H\%M).tar.gz
0 2 * * 0 nexus nexusctl backup create --output /backups/full-$(date +\%Y\%m\%d).tar.gz
```

## Restore

### Full Restore

```bash
# Stop all Nexus services
sudo systemctl stop nexus.target

# Restore from backup
nexusctl backup restore /backups/nexus-full-20240101.tar.gz

# Start services
sudo systemctl start nexus.target
```

### Incremental Restore

```bash
# Restore full backup first
nexusctl backup restore /backups/nexus-full-20240101.tar.gz

# Apply incremental backups in order
nexusctl backup restore /backups/nexus-incr-20240102.tar.gz
nexusctl backup restore /backups/nexus-incr-20240103.tar.gz
```

### Selective Restore

Restore specific buckets or objects:

```bash
nexusctl backup restore /backups/nexus-full-20240101.tar.gz --bucket my-important-bucket
```

## Disaster Recovery

For complete site failure:

1. Provision new infrastructure
2. Install Nexus
3. Restore from the latest remote backup
4. Verify data integrity: `nexusctl backup verify`
5. Update DNS/load balancer to point to new infrastructure

## Backup Verification

Regularly verify backup integrity:

```bash
# Verify backup file
nexusctl backup verify /backups/nexus-full-20240101.tar.gz

# Test restore to a staging environment
nexusctl backup restore /backups/nexus-full-20240101.tar.gz --dry-run
```
