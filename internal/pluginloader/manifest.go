package pluginloader

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/robfig/cron/v3"
)

// Supported manifest + host API versions. Bumping SupportedManifestVersion
// is a manifest schema major bump (spec §3.15); bumping HostAPIVersion is
// a host imports major bump — both require an IDL re-release (spec §8.8).
const (
	SupportedManifestVersion = "1.0"
	HostAPIVersion            = "1.0.0"
	// HostAPICompatRange is the semver range the loader accepts from
	// manifests. A minor bump adds imports backward-compatibly; a major
	// bump removes/changes imports and requires plugin recompilation.
	HostAPICompatRange = ">=1.0.0 <2.0.0"
)

// pluginNameRe restricts plugin names to URL-safe lowercase tokens. The
// name is used as a route prefix segment, a BoltDB bucket name suffix,
// and a Prometheus label value, so it must be conservative.
var pluginNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}[a-z0-9]$`)

// Manifest is the plugin declaration shipped alongside the WASM module
// (spec §6). The loader parses it on install, verifies the api_version
// range is compatible (spec §3.15 A15), and stores a copy in
// BucketPluginRegistry so followers see the same metadata.
type Manifest struct {
	// ManifestVersion is the schema version of this manifest file
	// (spec §3.15 A15). Must be in the loader's supported set.
	ManifestVersion string `json:"manifest_version" yaml:"manifest_version"`

	// APIVersion is a semver range (e.g. "^1.2.0") that must cover
	// the loader's current HostAPIVersion. Used to gate plugins that
	// require host imports newer than the loader supports.
	APIVersion string `json:"api_version" yaml:"api_version"`

	Name        string `json:"name" yaml:"name"`
	Version     string `json:"version" yaml:"version"`
	Author      string `json:"author" yaml:"author"`
	Description string `json:"description" yaml:"description"`

	// TrustTierRequested is the tier the plugin wishes to be loaded at
	// (0/1/2). The loader may downgrade based on signature verification
	// (spec §3.7): unsigned → Tier 0; trusted key → Tier 1; core key →
	// Tier 2. A request for Tier 2 with an untrusted signature is a hard
	// install error, not a silent downgrade.
	TrustTierRequested int `json:"trust_tier_requested" yaml:"trust_tier_requested"`

	Capabilities []string       `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	Hooks              []ManifestHook `json:"hooks,omitempty" yaml:"hooks,omitempty"`
	Routes             []ManifestRoute `json:"routes,omitempty" yaml:"routes,omitempty"`
	StateSchema        []ManifestStateKey `json:"state_schema,omitempty" yaml:"state_schema,omitempty"`
	Resources          *ManifestResources `json:"resources,omitempty" yaml:"resources,omitempty"`
	NetworkEgress      []string       `json:"network_egress,omitempty" yaml:"network_egress,omitempty"`
	DependsOn          []string       `json:"depends_on,omitempty" yaml:"depends_on,omitempty"`
	EventSubscriptions []ManifestEventSubscription `json:"event_subscriptions,omitempty" yaml:"event_subscriptions,omitempty"`

	// TaskSchedules declares cron schedules the loader registers on
	// install (spec §3.13, P4-3). Each trigger calls the plugin's
	// on_task entry point (or the configured handler).
	TaskSchedules []ManifestTaskSchedule `json:"task_schedules,omitempty" yaml:"task_schedules,omitempty"`

	// PipelineSteps declares the pipeline step names this plugin exposes
	// (spec §3.12, P4-1). Each step maps to the plugin's on_pipeline_step
	// entry point (or the configured handler). The plugin must also hold
	// the matching "pipeline:step:<name>" capability.
	PipelineSteps []ManifestPipelineStep `json:"pipeline_steps,omitempty" yaml:"pipeline_steps,omitempty"`

	// Grants allows other plugins to read this plugin's state (spec §3.14
	// A13, P5-3). Each grant names the reader plugin and a key prefix
	// (glob, e.g. "shared/*"). Honored only if the reading plugin holds
	// the state:cross_plugin:<this_plugin> capability.
	Grants []ManifestGrant `json:"grants,omitempty" yaml:"grants,omitempty"`

	// DeprecatedImports lists host imports that will be removed in the
	// next minor api_version. Plugins using them get a one-minor grace
	// period to migrate (spec §3.15).
	DeprecatedImports []string `json:"deprecated_imports,omitempty" yaml:"deprecated_imports,omitempty"`
}

// ManifestHook declares a single S3 operation hook (spec §3.2).
type ManifestHook struct {
	Type      string `json:"type" yaml:"type"`             // "before" | "after" | "around"
	Operation string `json:"op" yaml:"op"`                // "s3:GetObject", ...
	Condition string `json:"condition,omitempty" yaml:"condition,omitempty"` // CEL expression
	Handler   string `json:"handler" yaml:"handler"`      // WASM entry function name
	Priority  int    `json:"priority,omitempty" yaml:"priority,omitempty"`
	OnFailure string `json:"on_failure,omitempty" yaml:"on_failure,omitempty"` // "deny"|"allow"|"log_and_allow"
	Critical  bool   `json:"critical,omitempty" yaml:"critical,omitempty"`
}

// ManifestRoute declares an HTTP route prefix owned by this plugin.
type ManifestRoute struct {
	Prefix      string `json:"prefix" yaml:"prefix"`
	Handler     string `json:"handler" yaml:"handler"`
	SSESessions bool   `json:"sse_sessions,omitempty" yaml:"sse_sessions,omitempty"`
}

// ManifestStateKey declares a state:kv key pattern for documentation and
// future schema enforcement. The loader does not currently enforce this
// schema at runtime; it is informational for tooling.
type ManifestStateKey struct {
	Key  string `json:"key" yaml:"key"`
	Type string `json:"type,omitempty" yaml:"type,omitempty"` // "json" | "int" | "string" | "bytes"
}

// ManifestResources is the per-plugin resource request (spec §3.6). The
// final limit applied to an instance is min(global, tier, manifest).
type ManifestResources struct {
	MemoryMB    int    `json:"memory,omitempty" yaml:"memory,omitempty"`
	FuelPerSec  int64  `json:"fuel_per_sec,omitempty" yaml:"fuel_per_sec,omitempty"`
	CallTimeout string `json:"call_timeout,omitempty" yaml:"call_timeout,omitempty"`
	IOBytesPerSec int64 `json:"io_bytes_per_sec,omitempty" yaml:"io_bytes_per_sec,omitempty"`
	Concurrent  int    `json:"concurrent,omitempty" yaml:"concurrent,omitempty"`
}

// ManifestGrant authorizes another plugin to read this plugin's state
// (spec §3.14 A13).
type ManifestGrant struct {
	Plugin string `json:"plugin" yaml:"plugin"`
	Keys   []string `json:"keys" yaml:"keys"` // glob patterns
}

// ManifestTaskSchedule declares a cron schedule for a plugin (P4-3).
type ManifestTaskSchedule struct {
	Schedule string `json:"schedule" yaml:"schedule"` // cron expression
	Payload  string `json:"payload,omitempty" yaml:"payload,omitempty"`
	Handler  string `json:"handler,omitempty" yaml:"handler,omitempty"` // default "on_task"
}

// ManifestPipelineStep declares a pipeline step exposed by a plugin (P4-1).
type ManifestPipelineStep struct {
	Name    string `json:"name" yaml:"name"`       // step name referenced by YAML pipelines
	Handler string `json:"handler,omitempty" yaml:"handler,omitempty"` // default "on_pipeline_step"
}

// ParseManifest decodes a manifest from JSON bytes and runs validation.
// Callers that already have a parsed struct can call Validate directly.
func ParseManifest(raw []byte) (*Manifest, error) {
	if len(raw) == 0 {
		return nil, errors.New("manifest is empty")
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("failed to parse manifest JSON: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate runs all manifest-level validation checks.
func (m *Manifest) Validate() error {
	if m.ManifestVersion != SupportedManifestVersion {
		return ErrManifestInvalid{
			Field:   "manifest_version",
			Reason:  fmt.Sprintf("unsupported manifest_version %q (loader supports %q)", m.ManifestVersion, SupportedManifestVersion),
		}
	}
	if m.APIVersion == "" {
		return ErrManifestInvalid{Field: "api_version", Reason: "api_version is required"}
	}
	if err := validateAPIVersionRange(m.APIVersion); err != nil {
		return ErrManifestInvalid{Field: "api_version", Reason: err.Error()}
	}
	if !pluginNameRe.MatchString(m.Name) {
		return ErrManifestInvalid{
			Field: "name",
			Reason: fmt.Sprintf("invalid plugin name %q (must match %s; 3-64 chars, lowercase alnum and '-')", m.Name, pluginNameRe.String()),
		}
	}
	if m.Version == "" {
		return ErrManifestInvalid{Field: "version", Reason: "version is required"}
	}
	if _, err := semver.NewVersion(m.Version); err != nil {
		return ErrManifestInvalid{Field: "version", Reason: fmt.Sprintf("invalid semver %q: %v", m.Version, err)}
	}
	if m.TrustTierRequested < 0 || m.TrustTierRequested > 2 {
		return ErrManifestInvalid{Field: "trust_tier_requested", Reason: "must be 0, 1, or 2"}
	}
	for i, cap := range m.Capabilities {
		if err := validateCapability(cap); err != nil {
			return ErrManifestInvalid{Field: fmt.Sprintf("capabilities[%d]", i), Reason: err.Error()}
		}
		if IsTier2Only(cap) && m.TrustTierRequested < TierCore {
			return ErrManifestInvalid{
				Field:  fmt.Sprintf("capabilities[%d]", i),
				Reason: fmt.Sprintf("capability %q requires trust_tier_requested = 2", cap),
			}
		}
	}
	for i, h := range m.Hooks {
		if err := validateHook(&h); err != nil {
			return ErrManifestInvalid{Field: fmt.Sprintf("hooks[%d]", i), Reason: err.Error()}
		}
	}
	for i, r := range m.Routes {
		if err := validateRoute(&r); err != nil {
			return ErrManifestInvalid{Field: fmt.Sprintf("routes[%d]", i), Reason: err.Error()}
		}
	}
	for i, dep := range m.DependsOn {
		if !pluginNameRe.MatchString(dep) {
			return ErrManifestInvalid{Field: fmt.Sprintf("depends_on[%d]", i), Reason: "invalid plugin name"}
		}
	}
	for i, sub := range m.EventSubscriptions {
		if len(sub.Events) == 0 {
			return ErrManifestInvalid{Field: fmt.Sprintf("event_subscriptions[%d].events", i), Reason: "at least one event pattern is required"}
		}
		for j, pat := range sub.Events {
			if pat == "" {
				return ErrManifestInvalid{Field: fmt.Sprintf("event_subscriptions[%d].events[%d]", i, j), Reason: "empty event pattern"}
			}
		}
	}
	if len(m.TaskSchedules) > 0 && !hasCapability(m.Capabilities, "task:schedule") {
		return ErrManifestInvalid{Field: "task_schedules", Reason: "manifest declares task schedules but capability \"task:schedule\" is not granted"}
	}
	for i, ts := range m.TaskSchedules {
		if err := validateTaskSchedule(i, &ts); err != nil {
			return err
		}
	}
	seenSteps := make(map[string]bool)
	for i, ps := range m.PipelineSteps {
		if err := validatePipelineStep(i, &ps, m.Capabilities); err != nil {
			return err
		}
		if seenSteps[ps.Name] {
			return ErrManifestInvalid{Field: fmt.Sprintf("pipeline_steps[%d].name", i), Reason: fmt.Sprintf("duplicate pipeline step name %q", ps.Name)}
		}
		seenSteps[ps.Name] = true
	}
	return nil
}

// hasCapability reports whether caps contains the exact capability string.
func hasCapability(caps []string, cap string) bool {
	for _, c := range caps {
		if c == cap {
			return true
		}
	}
	return false
}

// taskScheduleCronParser accepts standard 5-field cron and 6-field cron-with-seconds,
// matching the parser used by internal/scheduler.
var taskScheduleCronParser = cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// validateTaskSchedule validates a single manifest task schedule.
func validateTaskSchedule(idx int, ts *ManifestTaskSchedule) error {
	if ts.Schedule == "" {
		return ErrManifestInvalid{Field: fmt.Sprintf("task_schedules[%d].schedule", idx), Reason: "schedule is required"}
	}
	if _, err := taskScheduleCronParser.Parse(ts.Schedule); err != nil {
		return ErrManifestInvalid{Field: fmt.Sprintf("task_schedules[%d].schedule", idx), Reason: fmt.Sprintf("invalid cron expression: %v", err)}
	}
	return nil
}

// validatePipelineStep validates a single manifest pipeline step declaration.
func validatePipelineStep(idx int, ps *ManifestPipelineStep, caps []string) error {
	if ps.Name == "" {
		return ErrManifestInvalid{Field: fmt.Sprintf("pipeline_steps[%d].name", idx), Reason: "pipeline step name is required"}
	}
	if !pluginNameRe.MatchString(ps.Name) {
		return ErrManifestInvalid{Field: fmt.Sprintf("pipeline_steps[%d].name", idx), Reason: fmt.Sprintf("invalid pipeline step name %q", ps.Name)}
	}
	requiredCap := "pipeline:step:" + ps.Name
	if !hasCapability(caps, requiredCap) {
		return ErrManifestInvalid{Field: fmt.Sprintf("pipeline_steps[%d]", idx), Reason: fmt.Sprintf("pipeline step %q requires capability %q", ps.Name, requiredCap)}
	}
	return nil
}

// validateAPIVersionRange parses the manifest's api_version as a semver
// range and checks that the loader's current HostAPIVersion satisfies it.
// This implements the spec §3.15 A15 dual-version check.
func validateAPIVersionRange(apiRange string) error {
	constraint, err := semver.NewConstraint(apiRange)
	if err != nil {
		return fmt.Errorf("invalid semver range %q: %w", apiRange, err)
	}
	hostVer, err := semver.NewVersion(HostAPIVersion)
	if err != nil {
		// Should never happen — the loader's own version is broken.
		return fmt.Errorf("loader host API version %q is not a valid semver: %w", HostAPIVersion, err)
	}
	if !constraint.Check(hostVer) {
		return fmt.Errorf("api_version range %q does not cover loader host API %s", apiRange, HostAPIVersion)
	}
	return nil
}

// validateCapability checks the capability string is non-empty and has
// a known namespace prefix. The scope suffix is plugin-defined and is
// matched at host-import time, so we only check the namespace here.
func validateCapability(cap string) error {
	if cap == "" {
		return errors.New("empty capability")
	}
	idx := strings.IndexByte(cap, ':')
	if idx <= 0 {
		return fmt.Errorf("invalid capability %q (expected namespace:name[:scope])", cap)
	}
	ns := cap[:idx]
	knownNamespaces := map[string]bool{
		"http":      true,
		"storage":   true,
		"vector":    true,
		"fts":       true,
		"state":     true,
		"event":     true,
		"pipeline":  true,
		"task":      true,
		"net":       true,
		"log":       true,
		"metric":    true,
		"trace":     true,
		"iam":       true,
		"kms":       true,
		"crypto":    true,
		"gateway":   true,
	}
	if !knownNamespaces[ns] {
		return fmt.Errorf("unknown capability namespace %q in %q", ns, cap)
	}
	return nil
}

// validateHook checks the hook declaration is well-formed (spec §3.2).
func validateHook(h *ManifestHook) error {
	switch h.Type {
	case "before", "after", "around":
	default:
		return fmt.Errorf("invalid hook type %q (must be before|after|around)", h.Type)
	}
	if h.Operation == "" {
		return errors.New("hook op is required")
	}
	if !strings.HasPrefix(h.Operation, "s3:") {
		return fmt.Errorf("hook op %q must start with s3:", h.Operation)
	}
	if h.Handler == "" {
		return errors.New("hook handler is required")
	}
	if h.Priority < 0 || h.Priority > 99 {
		return fmt.Errorf("hook priority %d out of range (0-99)", h.Priority)
	}
	switch h.OnFailure {
	case "", "deny", "allow", "log_and_allow":
	default:
		return fmt.Errorf("invalid on_failure %q", h.OnFailure)
	}
	return nil
}

// validateRoute checks the route declaration (spec §3.4).
func validateRoute(r *ManifestRoute) error {
	if !strings.HasPrefix(r.Prefix, "/") {
		return fmt.Errorf("route prefix %q must start with /", r.Prefix)
	}
	reserved := []string{"/", "/admin/", "/_plugins/", "/metrics", "/healthz", "/readyz", "/livez"}
	for _, rsv := range reserved {
		if r.Prefix == rsv || strings.HasPrefix(r.Prefix+"/", rsv) {
			// Allow plugin to claim /<name>/... but not the bare reserved roots.
			if r.Prefix == rsv {
				return fmt.Errorf("route prefix %q is reserved", r.Prefix)
			}
		}
	}
	if r.Handler == "" {
		return errors.New("route handler is required")
	}
	return nil
}

// ErrManifestInvalid is returned when a manifest fails validation.
type ErrManifestInvalid struct {
	Field  string
	Reason string
}

func (e ErrManifestInvalid) Error() string {
	return fmt.Sprintf("manifest invalid: field %s: %s", e.Field, e.Reason)
}

// IsErrManifestInvalid reports whether err is a manifest validation error.
func IsErrManifestInvalid(err error) bool {
	var m ErrManifestInvalid
	return errors.As(err, &m)
}
