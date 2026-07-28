package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jmespath/go-jmespath"
	"gopkg.in/yaml.v3"
)

// Global flags for output formatting
var (
	outputFmt string
	queryStr  string
)

// formatOutput formats data according to the specified output format and optional JMESPath query.
func formatOutput(data interface{}, outputFmt string, query string) (string, error) {
	var err error
	filtered := data

	// Apply JMESPath query if specified
	if query != "" {
		filtered, err = jmespath.Search(query, data)
		if err != nil {
			return "", fmt.Errorf("jmespath query error: %w", err)
		}
	}

	switch outputFmt {
	case "json":
		return formatJSON(filtered)
	case "yaml":
		return formatYAML(filtered)
	case "text":
		return formatText(filtered)
	default:
		return "", fmt.Errorf("unsupported output format: %s (valid: json, yaml, text)", outputFmt)
	}
}

func formatJSON(data interface{}) (string, error) {
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal JSON: %w", err)
	}
	return string(b), nil
}

func formatYAML(data interface{}) (string, error) {
	b, err := yaml.Marshal(data)
	if err != nil {
		return "", fmt.Errorf("failed to marshal YAML: %w", err)
	}
	return strings.TrimRight(string(b), "\n"), nil
}

func formatText(data interface{}) (string, error) {
	switch v := data.(type) {
	case string:
		return v, nil
	case fmt.Stringer:
		return v.String(), nil
	default:
		// Fallback to JSON for unknown types in text mode
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return "", fmt.Errorf("failed to format text output: %w", err)
		}
		return string(b), nil
	}
}
