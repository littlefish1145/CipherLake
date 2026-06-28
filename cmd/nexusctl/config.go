package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/hkdf"
	"nexus/internal/config"
	"gopkg.in/yaml.v3"
)

// CLIConfig represents the nexusctl configuration file.
type CLIConfig struct {
	Address   string            `yaml:"address"`
	AccessKey string            `yaml:"access_key"`
	SecretKey string            `yaml:"secret_key"`
	Region    string            `yaml:"region"`
	Output    string            `yaml:"output"`
	Aliases   map[string]string `yaml:"aliases"`
}

// configPath returns the path to the config file.
func configPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".nexusctl.yaml")
}

// loadConfig loads the CLI configuration from disk.
func loadConfig() (*CLIConfig, error) {
	path := configPath()
	if path == "" {
		return &CLIConfig{}, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &CLIConfig{}, nil
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// Check file permissions
	checkConfigPermissions(path)

	var cfg CLIConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Decrypt credentials if encrypted
	if cfg.AccessKey != "" && strings.HasPrefix(cfg.AccessKey, "enc:") {
		decrypted, err := decryptConfigValue(strings.TrimPrefix(cfg.AccessKey, "enc:"))
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt access_key: %w", err)
		}
		cfg.AccessKey = decrypted
	}
	if cfg.SecretKey != "" && strings.HasPrefix(cfg.SecretKey, "enc:") {
		decrypted, err := decryptConfigValue(strings.TrimPrefix(cfg.SecretKey, "enc:"))
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt secret_key: %w", err)
		}
		cfg.SecretKey = decrypted
	}

	return &cfg, nil
}

// saveConfig saves the CLI configuration to disk.
func saveConfig(cfg *CLIConfig) error {
	path := configPath()
	if path == "" {
		return fmt.Errorf("cannot determine config file path")
	}

	// Create a copy for serialization (encrypt sensitive fields)
	saveCfg := *cfg
	if saveCfg.AccessKey != "" {
		encrypted, err := encryptConfigValue(saveCfg.AccessKey)
		if err != nil {
			return fmt.Errorf("failed to encrypt access_key: %w", err)
		}
		saveCfg.AccessKey = "enc:" + encrypted
	}
	if saveCfg.SecretKey != "" {
		encrypted, err := encryptConfigValue(saveCfg.SecretKey)
		if err != nil {
			return fmt.Errorf("failed to encrypt secret_key: %w", err)
		}
		saveCfg.SecretKey = "enc:" + encrypted
	}

	data, err := yaml.Marshal(&saveCfg)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

// checkConfigPermissions warns if the config file has wider permissions than 0600.
func checkConfigPermissions(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	perm := info.Mode().Perm()
	if perm != 0600 {
		fmt.Fprintf(os.Stderr, "Warning: config file %s has permissions %o, should be 0600\n", path, perm)
	}
}

// deriveEncryptionKey derives a 32-byte AES key from the machine ID using HKDF.
// This is more secure than simple padding/truncation as it properly distributes
// entropy across the full key length.
func deriveEncryptionKey() ([]byte, error) {
	machineID, err := readMachineID()
	if err != nil {
		machineID = "nexusctl-default-key"
	}

	h := hkdf.New(sha256.New, []byte(machineID), []byte("nexusctl-config-encryption"), nil)
	key := make([]byte, 32)
	if _, err := io.ReadFull(h, key); err != nil {
		return nil, fmt.Errorf("failed to derive encryption key: %w", err)
	}
	return key, nil
}

// readMachineID reads the machine ID from /etc/machine-id or similar.
func readMachineID() (string, error) {
	for _, path := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		data, err := os.ReadFile(path)
		if err == nil {
			return strings.TrimSpace(string(data)), nil
		}
	}
	return "", fmt.Errorf("machine-id not found")
}

// encryptConfigValue encrypts a value using AES-GCM.
func encryptConfigValue(plaintext string) (string, error) {
	key, err := deriveEncryptionKey()
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, aesGCM.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	ciphertext := aesGCM.Seal(nonce, nonce, []byte(plaintext), nil)
	return hex.EncodeToString(ciphertext), nil
}

// decryptConfigValue decrypts a value using AES-GCM.
func decryptConfigValue(encoded string) (string, error) {
	key, err := deriveEncryptionKey()
	if err != nil {
		return "", err
	}

	ciphertext, err := hex.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("failed to decode hex: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonceSize := aesGCM.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := aesGCM.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt: %w", err)
	}

	return string(plaintext), nil
}

// --- Config commands ---

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage nexusctl configuration",
	Long:  "Manage the ~/.nexusctl.yaml configuration file",
}

var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a configuration value",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		key := args[0]
		value := args[1]

		validKeys := map[string]bool{
			"address":    true,
			"access_key": true,
			"secret_key": true,
			"region":     true,
			"output":     true,
		}
		if !validKeys[key] {
			return fmt.Errorf("invalid config key '%s'. Valid keys: address, access_key, secret_key, region, output", key)
		}

		cfg, err := loadConfig()
		if err != nil {
			return err
		}

		switch key {
		case "address":
			cfg.Address = value
		case "access_key":
			cfg.AccessKey = value
		case "secret_key":
			cfg.SecretKey = value
		case "region":
			cfg.Region = value
		case "output":
			if value != "json" && value != "yaml" && value != "text" {
				return fmt.Errorf("invalid output format '%s'. Valid values: json, yaml, text", value)
			}
			cfg.Output = value
		}

		if err := saveConfig(cfg); err != nil {
			return err
		}

		out, err := formatOutput(map[string]string{key: value}, outputFmt, queryStr)
		if err != nil {
			return err
		}
		fmt.Println(out)
		return nil
	},
}

var configGetCmd = &cobra.Command{
	Use:   "get <key>",
	Short: "Get a configuration value",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		key := args[0]

		cfg, err := loadConfig()
		if err != nil {
			return err
		}

		var value string
	switch key {
	case "address":
		value = cfg.Address
	case "access_key":
		value = maskSensitive(cfg.AccessKey)
	case "secret_key":
		value = maskSensitive(cfg.SecretKey)
	case "region":
		value = cfg.Region
	case "output":
		value = cfg.Output
	default:
		return fmt.Errorf("invalid config key '%s'. Valid keys: address, access_key, secret_key, region, output", key)
	}

		out, err := formatOutput(map[string]string{key: value}, outputFmt, queryStr)
		if err != nil {
			return err
		}
		fmt.Println(out)
		return nil
	},
}

var configListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all configuration values",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}

		// Mask sensitive values for display
		displayCfg := map[string]string{
			"address":    cfg.Address,
			"access_key": maskSensitive(cfg.AccessKey),
			"secret_key": maskSensitive(cfg.SecretKey),
			"region":     cfg.Region,
			"output":     cfg.Output,
		}

		out, err := formatOutput(displayCfg, outputFmt, queryStr)
		if err != nil {
			return err
		}
		fmt.Println(out)
		return nil
	},
}

// maskSensitive masks a sensitive string, showing only the first and last 4 chars.
func maskSensitive(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "****" + s[len(s)-4:]
}

var configValidateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate a Nexus server configuration file",
	Long:  "Validate a Nexus server configuration file against the schema and report errors and warnings",
	RunE: func(cmd *cobra.Command, args []string) error {
		file, _ := cmd.Flags().GetString("file")
		if file == "" {
			return fmt.Errorf("--file flag is required")
		}

		cfg, err := config.Load(file)
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		errs := config.Validate(cfg)

		outputJSON, _ := cmd.Flags().GetBool("output-json")

		if outputJSON {
			return printValidationJSON(errs)
		}

		return printValidationText(errs)
	},
}

func printValidationJSON(errs []config.ValidationError) error {
	type validationOutput struct {
		Valid    bool                    `json:"valid"`
		Errors   []config.ValidationError `json:"errors"`
		Warnings []config.ValidationError `json:"warnings"`
	}

	output := validationOutput{
		Valid:    !config.HasErrors(errs),
		Errors:   []config.ValidationError{},
		Warnings: []config.ValidationError{},
	}

	for _, e := range errs {
		if e.Severity == "error" {
			output.Errors = append(output.Errors, e)
		} else {
			output.Warnings = append(output.Warnings, e)
		}
	}

	b, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal output: %w", err)
	}
	fmt.Println(string(b))

	if output.Valid {
		return nil
	}
	// Return a non-nil error to trigger exit code 1
	return fmt.Errorf("validation failed with %d error(s)", len(output.Errors))
}

func printValidationText(errs []config.ValidationError) error {
	var errorCount, warningCount int
	for _, e := range errs {
		switch e.Severity {
		case "error":
			fmt.Fprintf(os.Stderr, "ERROR: %s\n", e)
			errorCount++
		case "warning":
			fmt.Fprintf(os.Stderr, "WARNING: %s\n", e)
			warningCount++
		}
	}

	if len(errs) == 0 {
		fmt.Println("Configuration is valid.")
		return nil
	}

	fmt.Fprintf(os.Stderr, "\n%d error(s), %d warning(s)\n", errorCount, warningCount)

	if errorCount > 0 {
		return fmt.Errorf("validation failed with %d error(s)", errorCount)
	}
	return nil
}

func init() {
	rootCmd.AddCommand(configCmd)
	configCmd.AddCommand(configSetCmd)
	configCmd.AddCommand(configGetCmd)
	configCmd.AddCommand(configListCmd)
	configCmd.AddCommand(configValidateCmd)
	configValidateCmd.Flags().StringP("file", "f", "", "Path to the Nexus server config file to validate")
	configValidateCmd.Flags().Bool("output-json", false, "Output validation results in JSON format")
}
