package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// --- JSON output formatting tests ---

func TestFormatOutputJSON(t *testing.T) {
	data := map[string]string{
		"name":   "test-bucket",
		"region": "us-east-1",
	}

	result, err := formatOutput(data, "json", "")
	if err != nil {
		t.Fatalf("formatOutput json failed: %v", err)
	}

	// Verify it's valid JSON
	var parsed map[string]string
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, result)
	}

	if parsed["name"] != "test-bucket" {
		t.Errorf("expected name=test-bucket, got %s", parsed["name"])
	}
	if parsed["region"] != "us-east-1" {
		t.Errorf("expected region=us-east-1, got %s", parsed["region"])
	}
}

func TestFormatOutputJSONArray(t *testing.T) {
	data := []map[string]string{
		{"name": "bucket1", "region": "us-east-1"},
		{"name": "bucket2", "region": "eu-west-1"},
	}

	result, err := formatOutput(data, "json", "")
	if err != nil {
		t.Fatalf("formatOutput json array failed: %v", err)
	}

	var parsed []map[string]string
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("output is not valid JSON array: %v\noutput: %s", err, result)
	}

	if len(parsed) != 2 {
		t.Errorf("expected 2 items, got %d", len(parsed))
	}
}

func TestFormatOutputJSONPrettyPrinted(t *testing.T) {
	data := map[string]string{"key": "value"}
	result, err := formatOutput(data, "json", "")
	if err != nil {
		t.Fatalf("formatOutput json failed: %v", err)
	}

	// Pretty JSON should have indentation
	if !strings.Contains(result, "  ") {
		t.Error("expected pretty-printed JSON with indentation")
	}
}

// --- YAML output formatting tests ---

func TestFormatOutputYAML(t *testing.T) {
	data := map[string]string{
		"name":   "test-bucket",
		"region": "us-east-1",
	}

	result, err := formatOutput(data, "yaml", "")
	if err != nil {
		t.Fatalf("formatOutput yaml failed: %v", err)
	}

	// Verify it's valid YAML
	var parsed map[string]string
	if err := yaml.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("output is not valid YAML: %v\noutput: %s", err, result)
	}

	if parsed["name"] != "test-bucket" {
		t.Errorf("expected name=test-bucket, got %s", parsed["name"])
	}
	if parsed["region"] != "us-east-1" {
		t.Errorf("expected region=us-east-1, got %s", parsed["region"])
	}
}

func TestFormatOutputYAMLStruct(t *testing.T) {
	type testStruct struct {
		Name   string `yaml:"name" json:"name"`
		Count  int    `yaml:"count" json:"count"`
		Active bool   `yaml:"active" json:"active"`
	}

	data := testStruct{Name: "hello", Count: 42, Active: true}
	result, err := formatOutput(data, "yaml", "")
	if err != nil {
		t.Fatalf("formatOutput yaml struct failed: %v", err)
	}

	if !strings.Contains(result, "name: hello") {
		t.Errorf("expected 'name: hello' in YAML output, got: %s", result)
	}
	if !strings.Contains(result, "count: 42") {
		t.Errorf("expected 'count: 42' in YAML output, got: %s", result)
	}
	if !strings.Contains(result, "active: true") {
		t.Errorf("expected 'active: true' in YAML output, got: %s", result)
	}
}

// --- Text output formatting tests ---

func TestFormatOutputText(t *testing.T) {
	data := "hello world"
	result, err := formatOutput(data, "text", "")
	if err != nil {
		t.Fatalf("formatOutput text failed: %v", err)
	}

	if result != "hello world" {
		t.Errorf("expected 'hello world', got '%s'", result)
	}
}

func TestFormatOutputTextMap(t *testing.T) {
	data := map[string]string{"key": "value"}
	result, err := formatOutput(data, "text", "")
	if err != nil {
		t.Fatalf("formatOutput text map failed: %v", err)
	}

	// For maps, text mode falls back to JSON
	if !strings.Contains(result, "key") || !strings.Contains(result, "value") {
		t.Errorf("expected key/value in text output, got: %s", result)
	}
}

// --- JMESPath query filtering tests ---

func TestFormatOutputWithJMESPathQuery(t *testing.T) {
	data := []map[string]interface{}{
		{"name": "bucket1", "region": "us-east-1"},
		{"name": "bucket2", "region": "eu-west-1"},
		{"name": "bucket3", "region": "us-east-1"},
	}

	result, err := formatOutput(data, "json", "[?region=='us-east-1'].name")
	if err != nil {
		t.Fatalf("formatOutput with JMESPath query failed: %v", err)
	}

	var parsed []string
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, result)
	}

	if len(parsed) != 2 {
		t.Errorf("expected 2 results, got %d", len(parsed))
	}
	if parsed[0] != "bucket1" {
		t.Errorf("expected bucket1, got %s", parsed[0])
	}
	if parsed[1] != "bucket3" {
		t.Errorf("expected bucket3, got %s", parsed[1])
	}
}

func TestFormatOutputWithJMESPathNestedQuery(t *testing.T) {
	data := map[string]interface{}{
		"buckets": []map[string]interface{}{
			{"name": "b1", "size": 100},
			{"name": "b2", "size": 200},
		},
	}

	result, err := formatOutput(data, "json", "buckets[*].name")
	if err != nil {
		t.Fatalf("formatOutput with nested JMESPath query failed: %v", err)
	}

	var parsed []string
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, result)
	}

	if len(parsed) != 2 {
		t.Errorf("expected 2 results, got %d", len(parsed))
	}
	if parsed[0] != "b1" || parsed[1] != "b2" {
		t.Errorf("expected [b1, b2], got %v", parsed)
	}
}

func TestFormatOutputWithInvalidJMESPathQuery(t *testing.T) {
	data := map[string]string{"key": "value"}

	_, err := formatOutput(data, "json", "[invalid query syntax")
	if err == nil {
		t.Error("expected error for invalid JMESPath query, got nil")
	}
}

func TestFormatOutputWithJMESPathYAML(t *testing.T) {
	data := []map[string]interface{}{
		{"name": "bucket1", "region": "us-east-1"},
		{"name": "bucket2", "region": "eu-west-1"},
	}

	result, err := formatOutput(data, "yaml", "[?region=='us-east-1'].name")
	if err != nil {
		t.Fatalf("formatOutput with JMESPath YAML query failed: %v", err)
	}

	// YAML list output
	if !strings.Contains(result, "bucket1") {
		t.Errorf("expected bucket1 in YAML output, got: %s", result)
	}
}

// --- RFC 7807 error formatting tests ---

func TestProblemDetailCreation(t *testing.T) {
	err := errors.New("bucket not found")
	problem := newProblemDetail(err, 404)

	if problem.Type != "about:blank" {
		t.Errorf("expected type 'about:blank', got '%s'", problem.Type)
	}
	if problem.Title != "Not Found" {
		t.Errorf("expected title 'Not Found', got '%s'", problem.Title)
	}
	if problem.Status != 404 {
		t.Errorf("expected status 404, got %d", problem.Status)
	}
	if problem.Detail != "bucket not found" {
		t.Errorf("expected detail 'bucket not found', got '%s'", problem.Detail)
	}
}

func TestProblemDetailJSONMarshal(t *testing.T) {
	err := errors.New("access denied")
	problem := newProblemDetail(err, 403)

	b, jsonErr := json.MarshalIndent(problem, "", "  ")
	if jsonErr != nil {
		t.Fatalf("failed to marshal ProblemDetail: %v", jsonErr)
	}

	result := string(b)
	if !strings.Contains(result, `"type": "about:blank"`) {
		t.Errorf("expected type field in JSON, got: %s", result)
	}
	if !strings.Contains(result, `"title": "Forbidden"`) {
		t.Errorf("expected title field in JSON, got: %s", result)
	}
	if !strings.Contains(result, `"status": 403`) {
		t.Errorf("expected status field in JSON, got: %s", result)
	}
	if !strings.Contains(result, `"detail": "access denied"`) {
		t.Errorf("expected detail field in JSON, got: %s", result)
	}
}

func TestHTTPStatusText(t *testing.T) {
	tests := []struct {
		code     int
		expected string
	}{
		{400, "Bad Request"},
		{401, "Unauthorized"},
		{403, "Forbidden"},
		{404, "Not Found"},
		{409, "Conflict"},
		{500, "Internal Server Error"},
		{502, "Bad Gateway"},
		{503, "Service Unavailable"},
		{418, "Error 418"},
	}

	for _, tt := range tests {
		result := httpStatusText(tt.code)
		if result != tt.expected {
			t.Errorf("httpStatusText(%d) = %q, want %q", tt.code, result, tt.expected)
		}
	}
}

func TestFormatErrorJSON(t *testing.T) {
	// Save and restore outputFmt
	origFmt := outputFmt
	defer func() { outputFmt = origFmt }()

	outputFmt = "json"
	// formatError prints to stdout for json, we can't easily capture it here
	// but we can verify it doesn't panic
	formatError(errors.New("test error"), 500)
}

func TestFormatErrorText(t *testing.T) {
	origFmt := outputFmt
	defer func() { outputFmt = origFmt }()

	outputFmt = "text"
	// formatError prints to stderr for text mode
	formatError(errors.New("test error"), 500)
}

// --- Config file loading/saving tests ---

func TestConfigSaveAndLoad(t *testing.T) {
	// Create a temp directory for the config file
	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, ".cipherlakectl.yaml")

	cfg := &CLIConfig{
		Address:   "http://localhost:9090",
		AccessKey: "AKIA1234567890",
		SecretKey: "secretkey123",
		Region:    "us-west-2",
		Output:    "json",
		Aliases:   map[string]string{"dev": "http://dev.example.com"},
	}

	// Save config
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("failed to marshal config: %v", err)
	}
	if err := os.WriteFile(configFile, data, 0600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	// Load config
	loadedData, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("failed to read config: %v", err)
	}

	var loadedCfg CLIConfig
	if err := yaml.Unmarshal(loadedData, &loadedCfg); err != nil {
		t.Fatalf("failed to unmarshal config: %v", err)
	}

	if loadedCfg.Address != cfg.Address {
		t.Errorf("expected address %s, got %s", cfg.Address, loadedCfg.Address)
	}
	if loadedCfg.Region != cfg.Region {
		t.Errorf("expected region %s, got %s", cfg.Region, loadedCfg.Region)
	}
	if loadedCfg.Output != cfg.Output {
		t.Errorf("expected output %s, got %s", cfg.Output, loadedCfg.Output)
	}
}

func TestConfigEncryption(t *testing.T) {
	plaintext := "my-secret-key"

	encrypted, err := encryptConfigValue(plaintext)
	if err != nil {
		t.Fatalf("encryptConfigValue failed: %v", err)
	}

	if encrypted == plaintext {
		t.Error("encrypted value should not equal plaintext")
	}

	decrypted, err := decryptConfigValue(encrypted)
	if err != nil {
		t.Fatalf("decryptConfigValue failed: %v", err)
	}

	if decrypted != plaintext {
		t.Errorf("decrypted value %q != original %q", decrypted, plaintext)
	}
}

func TestConfigEncryptionEmpty(t *testing.T) {
	encrypted, err := encryptConfigValue("")
	if err != nil {
		t.Fatalf("encryptConfigValue empty failed: %v", err)
	}

	decrypted, err := decryptConfigValue(encrypted)
	if err != nil {
		t.Fatalf("decryptConfigValue empty failed: %v", err)
	}

	if decrypted != "" {
		t.Errorf("expected empty string, got %q", decrypted)
	}
}

func TestConfigFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file permissions are not enforced on Windows")
	}

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, ".cipherlakectl.yaml")

	// Write with correct permissions
	if err := os.WriteFile(configFile, []byte("address: http://localhost:8080\n"), 0600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	info, err := os.Stat(configFile)
	if err != nil {
		t.Fatalf("failed to stat config: %v", err)
	}

	if info.Mode().Perm() != 0600 {
		t.Errorf("expected permissions 0600, got %o", info.Mode().Perm())
	}
}

func TestMaskSensitive(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", ""},
		{"short", "****"},
		{"AKIAIOSFODNN7EXAMPLE", "AKIA****MPLE"},
	}

	for _, tt := range tests {
		result := maskSensitive(tt.input)
		if result != tt.expected {
			t.Errorf("maskSensitive(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

// --- Unsupported format test ---

func TestFormatOutputUnsupportedFormat(t *testing.T) {
	_, err := formatOutput(map[string]string{"key": "value"}, "xml", "")
	if err == nil {
		t.Error("expected error for unsupported format, got nil")
	}
}

// --- Bucket struct JSON tags test ---

func TestBucketJSONTags(t *testing.T) {
	b := Bucket{
		Name:         "test-bucket",
		CreationDate: parseTime("2024-01-15T10:30:00Z"),
	}

	data, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("failed to marshal Bucket: %v", err)
	}

	result := string(data)
	if !strings.Contains(result, `"name"`) {
		t.Errorf("expected 'name' JSON key, got: %s", result)
	}
	if !strings.Contains(result, `"creation_date"`) {
		t.Errorf("expected 'creation_date' JSON key, got: %s", result)
	}
}

func TestObjectJSONTags(t *testing.T) {
	o := Object{
		Key:          "test-key",
		LastModified: parseTime("2024-01-15T10:30:00Z"),
		ETag:         "abc123",
		Size:         1024,
	}

	data, err := json.Marshal(o)
	if err != nil {
		t.Fatalf("failed to marshal Object: %v", err)
	}

	result := string(data)
	if !strings.Contains(result, `"key"`) {
		t.Errorf("expected 'key' JSON key, got: %s", result)
	}
	if !strings.Contains(result, `"size"`) {
		t.Errorf("expected 'size' JSON key, got: %s", result)
	}
}

// parseTime is a test helper to parse a time string.
func parseTime(s string) (t time.Time) {
	t, _ = time.Parse(time.RFC3339, s)
	return
}
