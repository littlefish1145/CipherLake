package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

var pluginCmd = &cobra.Command{
	Use:   "plugin",
	Short: "Plugin loader management commands",
}

var pluginListCmd = &cobra.Command{
	Use:   "list",
	Short: "List installed plugins",
	Run: func(cmd *cobra.Command, args []string) {
		resp, err := pluginAdminRequest("GET", "/admin/plugin/list", nil)
		if err != nil {
			formatError(err, 503)
			os.Exit(1)
		}
		defer resp.Body.Close()
		printAdminResponse(resp)
	},
}

var pluginInstallCmd = &cobra.Command{
	Use:   "install <manifest.json> <plugin.wasm> [signature.sig] [key_id]",
	Short: "Install a plugin from local files",
	Args:  cobra.RangeArgs(2, 4),
	Run: func(cmd *cobra.Command, args []string) {
		manifest, err := os.ReadFile(args[0])
		if err != nil {
			formatError(fmt.Errorf("read manifest: %w", err), 500)
			os.Exit(1)
		}
		wasm, err := os.ReadFile(args[1])
		if err != nil {
			formatError(fmt.Errorf("read wasm: %w", err), 500)
			os.Exit(1)
		}
		var sig []byte
		var keyID string
		if len(args) >= 3 {
			sig, _ = os.ReadFile(args[2])
		}
		if len(args) >= 4 {
			keyID = args[3]
		}
		body := map[string]interface{}{
			"manifest_bytes": manifest,
			"wasm_bytes":     wasm,
			"signature":      sig,
			"key_id":         keyID,
		}
		resp, err := pluginAdminRequest("POST", "/admin/plugin/install", body)
		if err != nil {
			formatError(err, 503)
			os.Exit(1)
		}
		defer resp.Body.Close()
		printAdminResponse(resp)
	},
}

var pluginUninstallCmd = &cobra.Command{
	Use:   "uninstall <plugin_name>",
	Short: "Uninstall a plugin",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		force, _ := cmd.Flags().GetBool("force")
		body := map[string]interface{}{
			"plugin_name": args[0],
			"force":       force,
		}
		resp, err := pluginAdminRequest("POST", "/admin/plugin/uninstall", body)
		if err != nil {
			formatError(err, 503)
			os.Exit(1)
		}
		defer resp.Body.Close()
		printAdminResponse(resp)
	},
}

var pluginReloadCmd = &cobra.Command{
	Use:   "reload <plugin_name>",
	Short: "Reload a plugin from local cache",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		body := map[string]interface{}{"plugin_name": args[0]}
		resp, err := pluginAdminRequest("POST", "/admin/plugin/reload", body)
		if err != nil {
			formatError(err, 503)
			os.Exit(1)
		}
		defer resp.Body.Close()
		printAdminResponse(resp)
	},
}

var pluginStateCmd = &cobra.Command{
	Use:   "state <plugin_name> [key]",
	Short: "Get plugin state (single key or dump all)",
	Args:  cobra.RangeArgs(1, 2),
	Run: func(cmd *cobra.Command, args []string) {
		pluginName := args[0]
		var path string
		if len(args) == 2 {
			path = fmt.Sprintf("/admin/plugin/state?plugin=%s&key=%s", pluginName, args[1])
		} else {
			path = fmt.Sprintf("/admin/plugin/state/dump?plugin=%s", pluginName)
		}
		resp, err := pluginAdminRequest("GET", path, nil)
		if err != nil {
			formatError(err, 503)
			os.Exit(1)
		}
		defer resp.Body.Close()
		printAdminResponse(resp)
	},
}

var pluginLogsCmd = &cobra.Command{
	Use:   "logs <plugin_name>",
	Short: "Show recent plugin log entries",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		limit, _ := cmd.Flags().GetInt32("limit")
		path := fmt.Sprintf("/admin/plugin/logs?plugin=%s&limit=%d", args[0], limit)
		resp, err := pluginAdminRequest("GET", path, nil)
		if err != nil {
			formatError(err, 503)
			os.Exit(1)
		}
		defer resp.Body.Close()
		printAdminResponse(resp)
	},
}

var pluginMetricsCmd = &cobra.Command{
	Use:   "metrics <plugin_name>",
	Short: "Show recent plugin metric samples",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		limit, _ := cmd.Flags().GetInt32("limit")
		path := fmt.Sprintf("/admin/plugin/metrics?plugin=%s&limit=%d", args[0], limit)
		resp, err := pluginAdminRequest("GET", path, nil)
		if err != nil {
			formatError(err, 503)
			os.Exit(1)
		}
		defer resp.Body.Close()
		printAdminResponse(resp)
	},
}

var pluginLogLevelCmd = &cobra.Command{
	Use:   "log-level <plugin_name> <level>",
	Short: "Set per-plugin log level dynamically",
	Args:  cobra.ExactArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		path := fmt.Sprintf("/admin/plugin/log-level?plugin=%s&level=%s", args[0], args[1])
		resp, err := pluginAdminRequest("POST", path, nil)
		if err != nil {
			formatError(err, 503)
			os.Exit(1)
		}
		defer resp.Body.Close()
		printAdminResponse(resp)
	},
}

var pluginTrustCmd = &cobra.Command{
	Use:   "trust",
	Short: "Manage trusted plugin signing keys",
}

var pluginTrustAddCmd = &cobra.Command{
	Use:   "add <key_id> <public_key_base64>",
	Short: "Add a trusted Ed25519 public key",
	Args:  cobra.ExactArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		isCore, _ := cmd.Flags().GetBool("core")
		body := map[string]interface{}{
			"key_id":     args[0],
			"public_key": args[1],
			"is_core":    isCore,
		}
		resp, err := pluginAdminRequest("POST", "/admin/plugin/trust/add", body)
		if err != nil {
			formatError(err, 503)
			os.Exit(1)
		}
		defer resp.Body.Close()
		printAdminResponse(resp)
	},
}

var pluginTrustRemoveCmd = &cobra.Command{
	Use:   "remove <key_id>",
	Short: "Remove a trusted Ed25519 public key",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		body := map[string]interface{}{"key_id": args[0]}
		resp, err := pluginAdminRequest("POST", "/admin/plugin/trust/remove", body)
		if err != nil {
			formatError(err, 503)
			os.Exit(1)
		}
		defer resp.Body.Close()
		printAdminResponse(resp)
	},
}

var pluginTrustListCmd = &cobra.Command{
	Use:   "list",
	Short: "List trusted plugin signing keys",
	Run: func(cmd *cobra.Command, args []string) {
		resp, err := pluginAdminRequest("GET", "/admin/plugin/trust/list", nil)
		if err != nil {
			formatError(err, 503)
			os.Exit(1)
		}
		defer resp.Body.Close()
		printAdminResponse(resp)
	},
}

func init() {
	rootCmd.AddCommand(pluginCmd)
	pluginCmd.AddCommand(pluginListCmd)
	pluginCmd.AddCommand(pluginInstallCmd)
	pluginCmd.AddCommand(pluginUninstallCmd)
	pluginUninstallCmd.Flags().Bool("force", false, "Force uninstall even if other plugins depend on it")
	pluginCmd.AddCommand(pluginReloadCmd)
	pluginCmd.AddCommand(pluginStateCmd)
	pluginCmd.AddCommand(pluginLogsCmd)
	pluginLogsCmd.Flags().Int32("limit", 100, "Maximum number of log entries to return")
	pluginCmd.AddCommand(pluginMetricsCmd)
	pluginMetricsCmd.Flags().Int32("limit", 100, "Maximum number of metric samples to return")
	pluginCmd.AddCommand(pluginLogLevelCmd)
	pluginCmd.AddCommand(pluginTrustCmd)
	pluginTrustCmd.AddCommand(pluginTrustAddCmd)
	pluginTrustAddCmd.Flags().Bool("core", false, "Mark as core key (Tier 2)")
	pluginTrustCmd.AddCommand(pluginTrustRemoveCmd)
	pluginTrustCmd.AddCommand(pluginTrustListCmd)
}

// pluginAdminRequest sends a JSON request to the Gateway admin API. The
// address is taken from the global --address flag.
func pluginAdminRequest(method, path string, body interface{}) (*http.Response, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, strings.TrimSuffix(address, "/")+path, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	ak := sessionAccessKey
	sk := sessionSecretKey
	if ak == "" {
		ak = os.Getenv("CIPHERLAKE_ACCESS_KEY")
	}
	if sk == "" {
		sk = os.Getenv("CIPHERLAKE_SECRET_KEY")
	}
	if ak != "" && sk != "" {
		req.SetBasicAuth(ak, sk)
	}

	client := &http.Client{Timeout: defaultAdminTimeout}
	return client.Do(req)
}

const defaultAdminTimeout = 60

func printAdminResponse(resp *http.Response) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		formatError(fmt.Errorf("reading response: %w", err), 500)
		os.Exit(1)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		formatError(fmt.Errorf("%s (status %d)", string(body), resp.StatusCode), resp.StatusCode)
		os.Exit(1)
	}

	var data interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		fmt.Println(string(body))
		return
	}
	out, err := formatOutput(data, outputFmt, queryStr)
	if err != nil {
		formatError(err, 500)
		os.Exit(1)
	}
	fmt.Println(out)
}
