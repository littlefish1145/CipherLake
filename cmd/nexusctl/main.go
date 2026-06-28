package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"
)

var (
	address          string
	verbose          bool
	sessionAccessKey string // in-memory only, never exported to env
	sessionSecretKey string // in-memory only, never exported to env
)

var rootCmd = &cobra.Command{
	Use:   "nexusctl",
	Short: "Nexus CLI - Management tool for Nexus storage system",
	Long: `Nexus Control (nexusctl) is a command-line tool for managing
Nexus S3-compatible storage system.`,
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		// Load config and apply defaults
		cfg, err := loadConfig()
		if err != nil {
			// Non-fatal: just warn
			fmt.Fprintf(os.Stderr, "Warning: failed to load config: %v\n", err)
			return
		}

		// Apply config defaults if flags not explicitly set
		if !cmd.Flags().Changed("address") && cfg.Address != "" {
			address = cfg.Address
		}
		if !cmd.Flags().Changed("output") && cfg.Output != "" {
			outputFmt = cfg.Output
		}

		// Store credentials in memory for this session only (do NOT set as env vars
		// to avoid leaking credentials to child processes and /proc/pid/environ)
		if cfg.AccessKey != "" {
			sessionAccessKey = cfg.AccessKey
		}
		if cfg.SecretKey != "" {
			sessionSecretKey = cfg.SecretKey
		}
	},
}

func init() {
	rootCmd.PersistentFlags().StringVar(&address, "address", "http://localhost:8080", "Nexus server address")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable verbose output")
	rootCmd.PersistentFlags().StringVarP(&outputFmt, "output", "o", "text", "Output format (json, yaml, text)")
	rootCmd.PersistentFlags().StringVar(&queryStr, "query", "", "JMESPath query to filter output")
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		if outputFmt == "json" || outputFmt == "yaml" {
			formatError(err, 500)
		} else {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		os.Exit(1)
	}
}

func serverRequest(method, path string) (*http.Response, error) {
	req, err := http.NewRequest(method, address+path, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Use session credentials (from config file) first, fall back to env vars
	ak := sessionAccessKey
	sk := sessionSecretKey
	if ak == "" {
		ak = os.Getenv("NEXUS_ACCESS_KEY")
	}
	if sk == "" {
		sk = os.Getenv("NEXUS_SECRET_KEY")
	}

	user := os.Getenv("NEXUS_ADMIN_USER")
	pass := os.Getenv("NEXUS_ADMIN_PASSWORD")
	if user == "" && ak != "" {
		user = ak
		pass = sk
	}

	if user != "" && pass != "" {
		req.SetBasicAuth(user, pass)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	return client.Do(req)
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show cluster status",
	Run: func(cmd *cobra.Command, args []string) {
		resp, err := serverRequest("GET", "/health")
		if err != nil {
			formatError(fmt.Errorf("connecting to server: %w", err), 503)
			os.Exit(1)
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			formatError(fmt.Errorf("reading response: %w", err), 500)
			os.Exit(1)
		}

		var health map[string]interface{}
		if err := json.Unmarshal(body, &health); err != nil {
			formatError(fmt.Errorf("parsing response: %w", err), 500)
			os.Exit(1)
		}

		out, err := formatOutput(health, outputFmt, queryStr)
		if err != nil {
			formatError(err, 500)
			os.Exit(1)
		}
		fmt.Println(out)
	},
}

var tieringCmd = &cobra.Command{
	Use:   "tiering",
	Short: "Tiering management commands",
}

var tieringRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Run tiering decision and execute migrations",
	Run: func(cmd *cobra.Command, args []string) {
		bucket, _ := cmd.Flags().GetString("bucket")

		path := "/admin/tiering/run"
		if bucket != "" {
			path = path + "?bucket=" + bucket
		}

		resp, err := serverRequest("POST", path)
		if err != nil {
			formatError(err, 503)
			os.Exit(1)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
			body, _ := io.ReadAll(resp.Body)
			formatError(fmt.Errorf("%s (status %d)", string(body), resp.StatusCode), resp.StatusCode)
			os.Exit(1)
		}

		result := map[string]string{"message": "Tiering execution triggered successfully"}
		out, err := formatOutput(result, outputFmt, queryStr)
		if err != nil {
			formatError(err, 500)
			os.Exit(1)
		}
		fmt.Println(out)
	},
}

var tieringStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show tiering status",
	Run: func(cmd *cobra.Command, args []string) {
		resp, err := serverRequest("GET", "/admin/tiering/status")
		if err != nil {
			formatError(err, 503)
			os.Exit(1)
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			formatError(fmt.Errorf("reading response: %w", err), 500)
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
	},
}

var vectorCmd = &cobra.Command{
	Use:   "vector",
	Short: "Vector index management commands",
}

var vectorRebuildCmd = &cobra.Command{
	Use:   "rebuild",
	Short: "Rebuild vector index",
	Run: func(cmd *cobra.Command, args []string) {
		bucket, _ := cmd.Flags().GetString("bucket")

		path := "/admin/vector/rebuild"
		if bucket != "" {
			path = path + "?bucket=" + bucket
		}

		resp, err := serverRequest("POST", path)
		if err != nil {
			formatError(err, 503)
			os.Exit(1)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
			body, _ := io.ReadAll(resp.Body)
			formatError(fmt.Errorf("%s (status %d)", string(body), resp.StatusCode), resp.StatusCode)
			os.Exit(1)
		}

		result := map[string]string{"message": "Vector index rebuild triggered successfully"}
		out, err := formatOutput(result, outputFmt, queryStr)
		if err != nil {
			formatError(err, 500)
			os.Exit(1)
		}
		fmt.Println(out)
	},
}

var vectorStatsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Show vector index statistics",
	Run: func(cmd *cobra.Command, args []string) {
		resp, err := serverRequest("GET", "/admin/vector/stats")
		if err != nil {
			formatError(err, 503)
			os.Exit(1)
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			formatError(fmt.Errorf("reading response: %w", err), 500)
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
	},
}

var vectorVerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Verify vector index integrity",
	Run: func(cmd *cobra.Command, args []string) {
		bucket, _ := cmd.Flags().GetString("bucket")

		path := "/admin/vector/verify"
		if bucket != "" {
			path = path + "?bucket=" + bucket
		}

		resp, err := serverRequest("POST", path)
		if err != nil {
			formatError(err, 503)
			os.Exit(1)
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			formatError(fmt.Errorf("reading response: %w", err), 500)
			os.Exit(1)
		}

		if resp.StatusCode != http.StatusOK {
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
	},
}

var cryptoCmd = &cobra.Command{
	Use:   "crypto",
	Short: "Cryptographic operations",
}

var cryptoRotateCmd = &cobra.Command{
	Use:   "rotate",
	Short: "Rotate encryption keys",
	Run: func(cmd *cobra.Command, args []string) {
		bucket, _ := cmd.Flags().GetString("bucket")
		keyID, _ := cmd.Flags().GetString("key-id")
		rewrap, _ := cmd.Flags().GetBool("rewrap")

		path := "/admin/crypto/rotate"
		params := []string{}
		if bucket != "" {
			params = append(params, "bucket="+bucket)
		}
		if keyID != "" {
			params = append(params, "key_id="+keyID)
		}
		if rewrap {
			params = append(params, "rewrap=true")
		}
		if len(params) > 0 {
			path = path + "?" + params[0]
			for _, p := range params[1:] {
				path = path + "&" + p
			}
		}

		resp, err := serverRequest("POST", path)
		if err != nil {
			formatError(err, 503)
			os.Exit(1)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
			body, _ := io.ReadAll(resp.Body)
			formatError(fmt.Errorf("%s (status %d)", string(body), resp.StatusCode), resp.StatusCode)
			os.Exit(1)
		}

		msg := "Key rotation triggered successfully"
		if rewrap {
			msg = "Key rotation and DEK rewrap triggered successfully"
		}
		result := map[string]string{"message": msg}
		out, err := formatOutput(result, outputFmt, queryStr)
		if err != nil {
			formatError(err, 500)
			os.Exit(1)
		}
		fmt.Println(out)
	},
}

var cryptoStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show encryption status",
	Run: func(cmd *cobra.Command, args []string) {
		resp, err := serverRequest("GET", "/admin/crypto/status")
		if err != nil {
			formatError(err, 503)
			os.Exit(1)
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			formatError(fmt.Errorf("reading response: %w", err), 500)
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
	},
}

var profileCmd = &cobra.Command{
	Use:   "profile",
	Short: "Performance profiling",
	Run: func(cmd *cobra.Command, args []string) {
		cpuduration, _ := cmd.Flags().GetString("cpu")

		path := "/debug/pprof/profile"
		if cpuduration != "" {
			path = path + "?seconds=" + cpuduration
		}

		resp, err := serverRequest("GET", path)
		if err != nil {
			formatError(err, 503)
			os.Exit(1)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			formatError(fmt.Errorf("%s (status %d)", string(body), resp.StatusCode), resp.StatusCode)
			os.Exit(1)
		}

		outFile, _ := cmd.Flags().GetString("output")
		if outFile == "" {
			outFile = "profile.out"
		}

		f, err := os.Create(outFile)
		if err != nil {
			formatError(fmt.Errorf("creating file: %w", err), 500)
			os.Exit(1)
		}
		defer f.Close()

		if _, err := io.Copy(f, resp.Body); err != nil {
			formatError(fmt.Errorf("writing profile: %w", err), 500)
			os.Exit(1)
		}

		result := map[string]string{"message": fmt.Sprintf("Profile saved to %s", outFile)}
		out, err := formatOutput(result, outputFmt, queryStr)
		if err != nil {
			formatError(err, 500)
			os.Exit(1)
		}
		fmt.Println(out)
	},
}

func init() {
	rootCmd.AddCommand(statusCmd)

	rootCmd.AddCommand(tieringCmd)
	tieringCmd.AddCommand(tieringRunCmd)
	tieringRunCmd.Flags().String("bucket", "", "Specific bucket to run tiering on")
	tieringCmd.AddCommand(tieringStatusCmd)

	rootCmd.AddCommand(vectorCmd)
	vectorCmd.AddCommand(vectorRebuildCmd)
	vectorRebuildCmd.Flags().String("bucket", "", "Specific bucket to rebuild index for")
	vectorCmd.AddCommand(vectorStatsCmd)
	vectorCmd.AddCommand(vectorVerifyCmd)
	vectorVerifyCmd.Flags().String("bucket", "", "Specific bucket to verify index for")

	rootCmd.AddCommand(cryptoCmd)
	cryptoCmd.AddCommand(cryptoRotateCmd)
	cryptoRotateCmd.Flags().String("bucket", "", "Specific bucket to rotate keys for")
	cryptoRotateCmd.Flags().String("key-id", "", "Specific key ID to rotate")
	cryptoRotateCmd.Flags().Bool("rewrap", false, "Trigger background rewrap of existing DEKs after rotation")
	cryptoCmd.AddCommand(cryptoStatusCmd)

	rootCmd.AddCommand(profileCmd)
	profileCmd.Flags().String("cpu", "30", "CPU profile duration in seconds")
	profileCmd.Flags().String("output", "", "Output file path (default: profile.out)")

	rootCmd.AddCommand(userCmd)
}
