package config

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"syscall"

	"github.com/fsnotify/fsnotify"
)

// hotReloadableFields defines which config fields can be changed at runtime
// without requiring a restart. The keys are dot-separated field paths.
var hotReloadableFields = map[string]bool{
	"logging.level":        true,
	"logging.format":       true,
	"logging.output_path":  true,
	"logging.access_log_dir": true,
	"ratelimit.enabled":            true,
	"ratelimit.global_rps":         true,
	"ratelimit.global_burst":       true,
	"ratelimit.ip_rps":             true,
	"ratelimit.ip_burst":           true,
	"ratelimit.user_rps":           true,
	"ratelimit.user_burst":         true,
	"ratelimit.bucket_rps":         true,
	"ratelimit.bucket_burst":       true,
	"ratelimit.upload_bytes_per_sec": true,
	"ratelimit.upload_burst_bytes": true,
	"ratelimit.api_limits":         true,
	"ratelimit.whitelist":          true,
	"cache.ttl":                    true,
	"cache.metadata_max_size":      true,
	"cache.object_max_size":        true,
	"cache.policy":                 true,
	"events.enabled":        true,
	"events.workers":        true,
	"events.max_retries":    true,
	"events.retry_base_ms":  true,
	"events.webhook_timeout": true,
	"events.dead_letter_dir": true,
}

// requiresRestartFields defines field prefixes that require a restart.
var requiresRestartPrefixes = []string{
	"node.",
	"encryption.",
	"crypto_services.",
	"vector.",
	"tls.",
}

// FieldChange represents a single field change between two configs.
type FieldChange struct {
	Field     string
	OldValue  interface{}
	NewValue  interface{}
	Reloadable bool
}

// IsHotReloadable returns whether a given dot-separated field path can be
// hot-reloaded without restarting the service.
func IsHotReloadable(field string) bool {
	if hotReloadableFields[field] {
		return true
	}
	return false
}

// RequiresRestart returns whether a given dot-separated field path requires
// a service restart to take effect.
func RequiresRestart(field string) bool {
	for _, prefix := range requiresRestartPrefixes {
		if strings.HasPrefix(field, prefix) {
			return true
		}
	}
	return false
}

// DiffConfigs compares two Config structs and returns the list of field changes.
func DiffConfigs(oldCfg, newCfg *Config) []FieldChange {
	return diffStructs(reflect.ValueOf(*oldCfg), reflect.ValueOf(*newCfg), "")
}

// diffStructs recursively compares two struct values and returns field changes.
func diffStructs(oldVal, newVal reflect.Value, prefix string) []FieldChange {
	var changes []FieldChange

	// Dereference pointers
	for oldVal.Kind() == reflect.Ptr || oldVal.Kind() == reflect.Interface {
		oldVal = oldVal.Elem()
	}
	for newVal.Kind() == reflect.Ptr || newVal.Kind() == reflect.Interface {
		newVal = newVal.Elem()
	}

	if oldVal.Kind() != reflect.Struct || newVal.Kind() != reflect.Struct {
		// Compare non-struct values directly
		if !reflect.DeepEqual(oldVal.Interface(), newVal.Interface()) {
			field := prefix
			changes = append(changes, FieldChange{
				Field:      field,
				OldValue:   oldVal.Interface(),
				NewValue:   newVal.Interface(),
				Reloadable: IsHotReloadable(field),
			})
		}
		return changes
	}

	typ := oldVal.Type()
	for i := 0; i < oldVal.NumField(); i++ {
		field := typ.Field(i)
		// Skip unexported fields
		if !field.IsExported() {
			continue
		}

		// Use mapstructure tag if available, otherwise use field name
		tag := field.Tag.Get("mapstructure")
		fieldName := field.Name
		if tag != "" {
			// Split by comma and take first part (ignore ",omitempty" etc.)
			tagParts := strings.Split(tag, ",")
			if tagParts[0] != "" && tagParts[0] != "-" {
				fieldName = tagParts[0]
			}
		}

		// Skip internal fields (mapstructure:"-")
		if tag == "-" {
			continue
		}

		fieldPath := fieldName
		if prefix != "" {
			fieldPath = prefix + "." + fieldName
		}

		oldField := oldVal.Field(i)
		newField := newVal.Field(i)

		// Handle slices, maps, and basic types
		if oldField.Kind() == reflect.Slice || oldField.Kind() == reflect.Map {
			if !reflect.DeepEqual(oldField.Interface(), newField.Interface()) {
				changes = append(changes, FieldChange{
					Field:      fieldPath,
					OldValue:   oldField.Interface(),
					NewValue:   newField.Interface(),
					Reloadable: IsHotReloadable(fieldPath),
				})
			}
			continue
		}

		// Recurse into nested structs
		if oldField.Kind() == reflect.Struct {
			changes = append(changes, diffStructs(oldField, newField, fieldPath)...)
			continue
		}

		// Compare basic types
		if !reflect.DeepEqual(oldField.Interface(), newField.Interface()) {
			changes = append(changes, FieldChange{
				Field:      fieldPath,
				OldValue:   oldField.Interface(),
				NewValue:   newField.Interface(),
				Reloadable: IsHotReloadable(fieldPath),
			})
		}
	}

	return changes
}

// ApplyHotReload merges hot-reloadable fields from newCfg into oldCfg and
// returns the resulting Config. Fields that require a restart are left
// unchanged from oldCfg. The function also returns lists of reloaded and
// skipped (restart-required) field changes.
func ApplyHotReload(oldCfg, newCfg *Config) (*Config, []string, []string) {
	changes := DiffConfigs(oldCfg, newCfg)

	// Start with a copy of the old config
	result := *oldCfg

	var reloaded, skipped []string

	for _, change := range changes {
		if change.Reloadable {
			reloaded = append(reloaded, change.Field)
		} else {
			skipped = append(skipped, change.Field)
		}
	}

	// Apply hot-reloadable fields using reflection
	resultVal := reflect.ValueOf(&result).Elem()
	newVal := reflect.ValueOf(*newCfg)

	for _, change := range changes {
		if !change.Reloadable {
			continue
		}
		applyField(resultVal, newVal, change.Field)
	}

	// Re-normalize to update computed fields (e.g., HotMaxBytes)
	_ = result.normalize()

	return &result, reloaded, skipped
}

// applyField sets a dot-separated field path on the target struct from the source.
func applyField(target, source reflect.Value, path string) {
	parts := strings.Split(path, ".")
	setNestedField(target, source, parts)
}

// setNestedField recursively traverses struct fields to copy a value.
func setNestedField(target, source reflect.Value, parts []string) bool {
	if len(parts) == 0 {
		return false
	}

	// Dereference pointers
	for target.Kind() == reflect.Ptr {
		target = target.Elem()
	}
	for source.Kind() == reflect.Ptr {
		source = source.Elem()
	}

	if target.Kind() != reflect.Struct || source.Kind() != reflect.Struct {
		return false
	}

	fieldName := parts[0]
	// Find the struct field by mapstructure tag or name
	typ := source.Type()
	for i := 0; i < source.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}

		tag := field.Tag.Get("mapstructure")
		name := field.Name
		if tag != "" {
			tagParts := strings.Split(tag, ",")
			if tagParts[0] != "" && tagParts[0] != "-" {
				name = tagParts[0]
			}
		}

		if name == fieldName {
			if len(parts) == 1 {
				// Leaf field: copy value
				targetField := target.Field(i)
				sourceField := source.Field(i)
				if targetField.CanSet() {
					targetField.Set(sourceField)
				}
				return true
			}
			// Recurse into nested struct
			return setNestedField(target.Field(i), source.Field(i), parts[1:])
		}
	}

	return false
}

// WatchConfig watches a configuration file for changes and supports SIGHUP
// signal-triggered reloads. On file change or SIGHUP, it loads the new config,
// validates it, and calls onReload with the merged config if validation passes.
// This function blocks until the watcher is stopped or an unrecoverable error occurs.
func WatchConfig(path string, onReload func(*Config)) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create file watcher: %w", err)
	}
	defer watcher.Close()

	if err := watcher.Add(path); err != nil {
		return fmt.Errorf("failed to watch config file %s: %w", path, err)
	}

	// Load initial config
	currentCfg, err := Load(path)
	if err != nil {
		return fmt.Errorf("failed to load initial config: %w", err)
	}

	// Set up SIGHUP signal handler
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGHUP)
	defer signal.Stop(sigChan)

	log.Printf("[hotreload] watching config file: %s", path)

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) {
				log.Printf("[hotreload] config file changed: %s", event.Name)
				currentCfg = reloadConfig(path, currentCfg, onReload)
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			log.Printf("[hotreload] watcher error: %v", err)

		case <-sigChan:
			log.Printf("[hotreload] received SIGHUP, reloading config")
			currentCfg = reloadConfig(path, currentCfg, onReload)
		}
	}
}

// reloadConfig loads, validates, and applies a hot-reload to the current config.
func reloadConfig(path string, currentCfg *Config, onReload func(*Config)) *Config {
	newCfg, err := Load(path)
	if err != nil {
		log.Printf("[hotreload] failed to load config: %v", err)
		return currentCfg
	}

	// Validate the new config
	errs := Validate(newCfg)
	if HasErrors(errs) {
		log.Printf("[hotreload] validation failed, keeping current config:")
		for _, e := range errs {
			if e.Severity == "error" {
				log.Printf("[hotreload]   ERROR: %s", e)
			}
		}
		return currentCfg
	}

	// Log warnings
	for _, e := range errs {
		if e.Severity == "warning" {
			log.Printf("[hotreload]   WARNING: %s", e)
		}
	}

	// Apply hot-reloadable fields
	merged, reloaded, skipped := ApplyHotReload(currentCfg, newCfg)

	if len(reloaded) > 0 {
		log.Printf("[hotreload] reloaded fields: %v", reloaded)
	}
	if len(skipped) > 0 {
		log.Printf("[hotreload] fields requiring restart: %v", skipped)
	}

	if onReload != nil {
		onReload(merged)
	}

	return merged
}
