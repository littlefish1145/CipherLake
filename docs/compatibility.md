# Nexus Version Compatibility Strategy

## Version Numbering

Nexus follows [Semantic Versioning 2.0.0](https://semver.org/):

- **MAJOR** version: Incompatible API changes
- **MINOR** version: Backward-compatible functionality additions
- **PATCH** version: Backward-compatible bug fixes

Example: `1.2.3` → Major 1, Minor 2, Patch 3

### Pre-release Versions

Pre-release versions are denoted by appending a hyphen and identifiers:
- Alpha: `1.0.0-alpha.1`
- Beta: `1.0.0-beta.2`
- Release Candidate: `1.0.0-rc.1`

Pre-release versions have no stability guarantees and are intended for testing only.

## API Compatibility Guarantees

### S3-Compatible API

The S3-compatible API is the primary public interface. We guarantee:

1. **Backward compatibility**: All documented S3 API behaviors will remain consistent across minor and patch releases.
2. **New fields**: API responses may include additional fields in future versions. Clients must ignore unknown fields.
3. **Error codes**: Existing error codes will not be removed or change meaning in minor/patch releases.
4. **Authentication**: SigV4 authentication is the standard and will remain supported.

### gRPC Services

Internal gRPC services between microservices follow these rules:

1. **Field additions**: New fields may be added to existing messages (proto3 compatibility).
2. **Field removals**: Fields are deprecated for at least one major release before removal.
3. **Service methods**: New RPC methods may be added; existing methods maintain their signatures.

### Admin API

The Admin REST API follows the same compatibility rules as the S3 API. All breaking changes require a major version bump.

## Deprecation Policy

1. **Announcement**: Features are marked as deprecated in release notes and documentation.
2. **Grace period**: Deprecated features remain functional for at least two minor releases (typically 6 months).
3. **Warnings**: Deprecated API endpoints return a `X-Nexus-Deprecated` header with migration guidance.
4. **Removal**: Features are removed only in major version releases.

### Currently Deprecated Features

| Feature | Deprecated Since | Removal Version | Migration Path |
|---------|-----------------|-----------------|----------------|
| (none)  | -               | -               | -              |

## Upgrade Path

### Patch Releases (e.g., 1.2.3 → 1.2.4)

- Direct upgrade supported
- No configuration changes required
- Rolling update possible (mixed versions briefly coexist)

### Minor Releases (e.g., 1.2.x → 1.3.0)

- Direct upgrade supported
- Review release notes for new configuration options
- Rolling update possible with brief mixed-version state
- New features are opt-in by default

### Major Releases (e.g., 1.x → 2.0.0)

- May require migration steps
- Read the migration guide before upgrading
- Test in a staging environment first
- Backup data before proceeding
- Mixed-version state is NOT supported during upgrade

#### Recommended Major Upgrade Process

1. Deploy the new version alongside the old (blue-green)
2. Migrate data using `nexusctl backup` and `nexusctl restore`
3. Validate data integrity
4. Switch traffic to the new version
5. Decommission the old version

## SDK Compatibility

### Go SDK

- Minimum Go version: follows the two most recent Go releases
- Compatible with the latest two Nexus major versions
- Follows the same semver scheme as Nexus

### Python SDK

- Minimum Python version: 3.9+
- Compatible with the latest two Nexus major versions
- boto3 dependency: latest stable release

## Configuration Compatibility

- Configuration files from the previous minor release are always supported
- Deprecated configuration keys emit warnings but continue to function
- New defaults are chosen to maintain existing behavior when possible
