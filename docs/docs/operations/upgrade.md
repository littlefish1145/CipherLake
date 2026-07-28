# Upgrade Procedures

Guide for upgrading CipherLake between versions.

## Pre-Upgrade Checklist

- [ ] Read the release notes for the target version
- [ ] Verify system requirements haven't changed
- [ ] Create a full backup: `cipherlakectl backup create --output /backups/pre-upgrade.tar.gz`
- [ ] Test the upgrade in a staging environment
- [ ] Plan a maintenance window for major upgrades
- [ ] Notify users of any planned downtime

## Patch Release Upgrade (e.g., 1.2.3 → 1.2.4)

Patch releases contain only bug fixes and are safe to apply directly.

### Binary Installation

```bash
# Download new version
curl -L https://github.com/cipherlake/cipherlake/releases/download/v1.2.4/cipherlake-linux-amd64 -o /usr/local/bin/cipherlake
chmod +x /usr/local/bin/cipherlake

# Restart services
sudo systemctl restart cipherlake.target
```

### Docker

```bash
docker compose pull
docker compose up -d
```

### Helm

```bash
helm repo update
helm upgrade cipherlake cipherlake/cipherlake --namespace cipherlake
```

## Minor Release Upgrade (e.g., 1.2.x → 1.3.0)

Minor releases add features but maintain backward compatibility.

1. Review release notes for new configuration options
2. Update the configuration file if needed
3. Perform the upgrade using the same steps as a patch release
4. Verify new features are working

### Rolling Upgrade (Cluster)

For clusters, perform a rolling upgrade:

```bash
# Upgrade one gateway at a time
cipherlakectl cluster upgrade --node gateway1 --version v1.3.0
cipherlakectl cluster upgrade --node gateway2 --version v1.3.0
cipherlakectl cluster upgrade --node gateway3 --version v1.3.0

# Upgrade metadata nodes (followers first, leader last)
cipherlakectl cluster upgrade --node meta2 --version v1.3.0
cipherlakectl cluster upgrade --node meta3 --version v1.3.0
# Step down leader before upgrading
cipherlakectl cluster step-down --node meta1
cipherlakectl cluster upgrade --node meta1 --version v1.3.0
```

## Major Release Upgrade (e.g., 1.x → 2.0.0)

Major releases may include breaking changes. Follow these steps carefully:

1. **Read the migration guide** in the release notes
2. **Full backup**: `cipherlakectl backup create --output /backups/pre-major-upgrade.tar.gz`
3. **Test in staging**: Restore the backup to a test environment and verify
4. **Plan downtime**: Major upgrades typically require a maintenance window
5. **Execute the upgrade**:

```bash
# Stop all services
sudo systemctl stop cipherlake.target

# Run migration (if required)
cipherlakectl migrate --from v1.x --to v2.0.0

# Update binaries
curl -L https://github.com/cipherlake/cipherlake/releases/download/v2.0.0/cipherlake-linux-amd64 -o /usr/local/bin/cipherlake
chmod +x /usr/local/bin/cipherlake

# Update configuration
cipherlakectl config migrate --from v1 --to v2 /etc/cipherlake/config.yaml

# Start services
sudo systemctl start cipherlake.target
```

6. **Verify**: Check all services are healthy and data is accessible

## Rollback

If the upgrade fails:

### Patch/Minor Rollback

```bash
# Revert to previous version
sudo systemctl stop cipherlake.target
# Install previous binary
curl -L https://github.com/cipherlake/cipherlake/releases/download/v1.2.3/cipherlake-linux-amd64 -o /usr/local/bin/cipherlake
chmod +x /usr/local/bin/cipherlake
sudo systemctl start cipherlake.target
```

### Major Rollback

```bash
# Stop services
sudo systemctl stop cipherlake.target

# Restore from backup
cipherlakectl backup restore /backups/pre-major-upgrade.tar.gz

# Install previous version
curl -L https://github.com/cipherlake/cipherlake/releases/download/v1.2.4/cipherlake-linux-amd64 -o /usr/local/bin/cipherlake
chmod +x /usr/local/bin/cipherlake

# Start services
sudo systemctl start cipherlake.target
```

## Post-Upgrade Verification

After any upgrade, verify:

```bash
# Check service health
curl http://localhost:9000/health

# Verify data access
cipherlakectl bucket list
cipherlakectl object list my-bucket

# Check cluster status (if applicable)
cipherlakectl cluster status

# Review metrics
curl http://localhost:9091/metrics | grep cipherlake_up
```
