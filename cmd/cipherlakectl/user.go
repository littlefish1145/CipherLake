package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var (
	adminUser     string
	adminPassword string
)

type userResponse struct {
	ID                string              `json:"id"`
	Name              string              `json:"name"`
	Role              string              `json:"role"`
	Permissions       []string            `json:"permissions"`
	BucketPermissions map[string][]string `json:"bucket_permissions,omitempty"`
}

type userListResponse struct {
	Users []userResponse `json:"users"`
	Count int            `json:"count"`
}

var userCmd = &cobra.Command{
	Use:   "user",
	Short: "User management commands",
	Long:  "Manage S3 users: create, list, get, update, delete, change-password",
}

var userListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all users",
	Run: func(cmd *cobra.Command, args []string) {
		resp, err := adminRequest("GET", "/admin/users", nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			fmt.Fprintf(os.Stderr, "Error: %s\n", string(body))
			os.Exit(1)
		}

		var result userListResponse
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			fmt.Fprintf(os.Stderr, "Error decoding response: %v\n", err)
			os.Exit(1)
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tID\tROLE\tPERMISSIONS")
		for _, u := range result.Users {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", u.Name, u.ID, u.Role, strings.Join(u.Permissions, ","))
		}
		w.Flush()
	},
}

var userGetCmd = &cobra.Command{
	Use:   "get [name]",
	Short: "Get user details",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		resp, err := adminRequest("GET", "/admin/users/"+args[0], nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			fmt.Fprintf(os.Stderr, "Error: %s\n", string(body))
			os.Exit(1)
		}

		var user userResponse
		if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
			fmt.Fprintf(os.Stderr, "Error decoding response: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("Name:        %s\n", user.Name)
		fmt.Printf("ID:          %s\n", user.ID)
		fmt.Printf("Role:        %s\n", user.Role)
		fmt.Printf("Permissions: %s\n", strings.Join(user.Permissions, ", "))
		if len(user.BucketPermissions) > 0 {
			fmt.Println("Bucket Permissions:")
			for bucket, perms := range user.BucketPermissions {
				fmt.Printf("  %s: %s\n", bucket, strings.Join(perms, ", "))
			}
		}
	},
}

var userCreateCmd = &cobra.Command{
	Use:   "create [name]",
	Short: "Create a new user",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		password, _ := cmd.Flags().GetString("password")
		secretKey, _ := cmd.Flags().GetString("secret-key")
		role, _ := cmd.Flags().GetString("role")
		perms, _ := cmd.Flags().GetStringSlice("permissions")
		bucketPerms, _ := cmd.Flags().GetStringToString("bucket-permissions")

		if password == "" {
			fmt.Fprintf(os.Stderr, "Error: --password is required\n")
			os.Exit(1)
		}

		body := map[string]interface{}{
			"name":        args[0],
			"password":    password,
			"role":        role,
			"permissions": perms,
		}

		if secretKey != "" {
			body["secret_key"] = secretKey
		}

		if len(bucketPerms) > 0 {
			bp := make(map[string][]string)
			for bucket, permStr := range bucketPerms {
				bp[bucket] = strings.Split(permStr, ",")
			}
			body["bucket_permissions"] = bp
		}

		resp, err := adminRequest("POST", "/admin/users", body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusCreated {
			respBody, _ := io.ReadAll(resp.Body)
			fmt.Fprintf(os.Stderr, "Error: %s\n", string(respBody))
			os.Exit(1)
		}

		var user userResponse
		if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
			fmt.Fprintf(os.Stderr, "Error decoding response: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("User created successfully:\n")
		fmt.Printf("  Name:        %s\n", user.Name)
		fmt.Printf("  ID:          %s\n", user.ID)
		fmt.Printf("  Role:        %s\n", user.Role)
		fmt.Printf("  Permissions: %s\n", strings.Join(user.Permissions, ", "))
		if len(user.BucketPermissions) > 0 {
			fmt.Println("  Bucket Permissions:")
			for bucket, perms := range user.BucketPermissions {
				fmt.Printf("    %s: %s\n", bucket, strings.Join(perms, ", "))
			}
		}
	},
}

var userDeleteCmd = &cobra.Command{
	Use:   "delete [name]",
	Short: "Delete a user",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		resp, err := adminRequest("DELETE", "/admin/users/"+args[0], nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNoContent {
			respBody, _ := io.ReadAll(resp.Body)
			fmt.Fprintf(os.Stderr, "Error: %s\n", string(respBody))
			os.Exit(1)
		}

		fmt.Printf("User '%s' deleted successfully\n", args[0])
	},
}

var userUpdateCmd = &cobra.Command{
	Use:   "update [name]",
	Short: "Update user role and permissions",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		role, _ := cmd.Flags().GetString("role")
		perms, _ := cmd.Flags().GetStringSlice("permissions")
		bucketPerms, _ := cmd.Flags().GetStringToString("bucket-permissions")

		body := map[string]interface{}{}
		if cmd.Flags().Changed("role") {
			body["role"] = role
		}
		if cmd.Flags().Changed("permissions") {
			body["permissions"] = perms
		}
		if cmd.Flags().Changed("bucket-permissions") {
			bp := make(map[string][]string)
			for bucket, permStr := range bucketPerms {
				bp[bucket] = strings.Split(permStr, ",")
			}
			body["bucket_permissions"] = bp
		}

		if len(body) == 0 {
			fmt.Fprintf(os.Stderr, "Error: specify --role, --permissions, and/or --bucket-permissions to update\n")
			os.Exit(1)
		}

		resp, err := adminRequest("PUT", "/admin/users/"+args[0], body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			fmt.Fprintf(os.Stderr, "Error: %s\n", string(respBody))
			os.Exit(1)
		}

		var user userResponse
		if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
			fmt.Fprintf(os.Stderr, "Error decoding response: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("User updated successfully:\n")
		fmt.Printf("  Name:        %s\n", user.Name)
		fmt.Printf("  ID:          %s\n", user.ID)
		fmt.Printf("  Role:        %s\n", user.Role)
		fmt.Printf("  Permissions: %s\n", strings.Join(user.Permissions, ", "))
		if len(user.BucketPermissions) > 0 {
			fmt.Println("  Bucket Permissions:")
			for bucket, perms := range user.BucketPermissions {
				fmt.Printf("    %s: %s\n", bucket, strings.Join(perms, ", "))
			}
		}
	},
}

var userPasswdCmd = &cobra.Command{
	Use:   "passwd [name]",
	Short: "Change user password",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		password, _ := cmd.Flags().GetString("password")
		if password == "" {
			fmt.Fprintf(os.Stderr, "Error: --password is required\n")
			os.Exit(1)
		}

		body := map[string]string{"password": password}
		url := "/admin/users/" + args[0] + "?action=change-password"

		resp, err := adminRequest("PATCH", url, body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNoContent {
			respBody, _ := io.ReadAll(resp.Body)
			fmt.Fprintf(os.Stderr, "Error: %s\n", string(respBody))
			os.Exit(1)
		}

		fmt.Printf("Password changed for user '%s'\n", args[0])
	},
}

func adminRequest(method, path string, body interface{}) (*http.Response, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, address+path, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	user := adminUser
	pass := adminPassword

	if user == "" {
		user = os.Getenv("CIPHERLAKE_ADMIN_USER")
	}
	if pass == "" {
		pass = os.Getenv("CIPHERLAKE_ADMIN_PASSWORD")
	}

	if user != "" && pass != "" {
		req.SetBasicAuth(user, pass)
	}

	client := &http.Client{}
	return client.Do(req)
}

func init() {
	userCmd.PersistentFlags().StringVar(&adminUser, "admin-user", "", "Admin username for authentication (env: CIPHERLAKE_ADMIN_USER)")
	userCmd.PersistentFlags().StringVar(&adminPassword, "admin-pass", "", "Admin password for authentication (env: CIPHERLAKE_ADMIN_PASSWORD)")

	userCreateCmd.Flags().String("password", "", "User password (required)")
	userCreateCmd.Flags().String("secret-key", "", "AWS SigV4 secret key (auto-generated if not provided)")
	userCreateCmd.Flags().String("role", "user", "User role (admin, user, readonly)")
	userCreateCmd.Flags().StringSlice("permissions", []string{"read", "write"}, "User permissions")
	userCreateCmd.Flags().StringToString("bucket-permissions", nil, "Bucket permissions (format: bucket=perm1,perm2)")

	userUpdateCmd.Flags().String("role", "", "New role")
	userUpdateCmd.Flags().StringSlice("permissions", nil, "New permissions")
	userUpdateCmd.Flags().StringToString("bucket-permissions", nil, "New bucket permissions (format: bucket=perm1,perm2)")

	userPasswdCmd.Flags().String("password", "", "New password (required)")

	userCmd.AddCommand(userListCmd)
	userCmd.AddCommand(userGetCmd)
	userCmd.AddCommand(userCreateCmd)
	userCmd.AddCommand(userDeleteCmd)
	userCmd.AddCommand(userUpdateCmd)
	userCmd.AddCommand(userPasswdCmd)
}
