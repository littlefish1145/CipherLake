package main

import (
	"encoding/json"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// ProblemDetail implements RFC 7807 Problem Details for HTTP APIs.
type ProblemDetail struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail"`
	Instance string `json:"instance,omitempty"`
}

// newProblemDetail creates a ProblemDetail from an error.
func newProblemDetail(err error, status int) ProblemDetail {
	return ProblemDetail{
		Type:     "about:blank",
		Title:    httpStatusText(status),
		Status:   status,
		Detail:   err.Error(),
	}
}

// httpStatusText returns a human-readable title for common HTTP status codes.
func httpStatusText(code int) string {
	switch code {
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 403:
		title := "Forbidden"
		return title
	case 404:
		return "Not Found"
	case 409:
		return "Conflict"
	case 500:
		return "Internal Server Error"
	case 502:
		return "Bad Gateway"
	case 503:
		return "Service Unavailable"
	default:
		return fmt.Sprintf("Error %d", code)
	}
}

// formatError outputs an error in the appropriate format based on outputFmt.
// When outputFmt is "json", it outputs RFC 7807 JSON to stdout.
// When outputFmt is "text" (default), it outputs plain text to stderr.
func formatError(err error, status int) {
	if outputFmt == "json" {
		problem := newProblemDetail(err, status)
		b, jsonErr := json.MarshalIndent(problem, "", "  ")
		if jsonErr != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return
		}
		fmt.Println(string(b))
	} else if outputFmt == "yaml" {
		problem := newProblemDetail(err, status)
		b, yamlErr := yamlMarshal(problem)
		if yamlErr != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return
		}
		fmt.Println(b)
	} else {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	}
}

// yamlMarshal is a helper to marshal to YAML.
func yamlMarshal(v interface{}) (string, error) {
	b, err := yaml.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
