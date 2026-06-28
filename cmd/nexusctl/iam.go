package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// iamCmd is the root IAM command
var iamCmd = &cobra.Command{
	Use:   "iam",
	Short: "IAM management commands",
	Long:  "Manage IAM users, groups, policies, roles, and access keys",
}

// --- Access Key commands ---

var iamCreateAccessKeyCmd = &cobra.Command{
	Use:   "create-access-key [username]",
	Short: "Create a new access key for a user",
	Long:  "Creates a new AKIA-prefixed access key. The secret key is shown ONLY once!",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		desc, _ := cmd.Flags().GetString("description")
		body := map[string]string{}
		if desc != "" {
			body["description"] = desc
		}

		resp, err := iamRequest("POST", "/iam/users/"+args[0]+"/access-keys", body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()

		respBody, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusCreated {
			fmt.Fprintf(os.Stderr, "Error: %s\n", string(respBody))
			os.Exit(1)
		}

		var result map[string]interface{}
		json.Unmarshal(respBody, &result)

		fmt.Println("==============================================")
		fmt.Println("  ACCESS KEY CREATED - SAVE NOW!")
		fmt.Printf("  Access Key ID:     %v\n", result["access_key_id"])
		fmt.Printf("  Secret Access Key: %v\n", result["secret_access_key"])
		fmt.Println("  WARNING: Secret key will NOT be shown again!")
		fmt.Println("==============================================")
	},
}

var iamListAccessKeysCmd = &cobra.Command{
	Use:   "list-access-keys [username]",
	Short: "List access keys for a user",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		resp, err := iamRequest("GET", "/iam/users/"+args[0]+"/access-keys", nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()

		respBody, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(os.Stderr, "Error: %s\n", string(respBody))
			os.Exit(1)
		}

		var result map[string]interface{}
		json.Unmarshal(respBody, &result)

		keys, ok := result["access_keys"].([]interface{})
		if !ok || len(keys) == 0 {
			fmt.Println("No access keys found")
			return
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ACCESS KEY ID\tSTATUS\tCREATED\tDESCRIPTION")
		for _, k := range keys {
			key := k.(map[string]interface{})
			fmt.Fprintf(w, "%v\t%v\t%v\t%v\n",
				key["access_key_id"], key["status"], key["created_at"], key["description"])
		}
		w.Flush()
	},
}

var iamDeleteAccessKeyCmd = &cobra.Command{
	Use:   "delete-access-key",
	Short: "Delete an access key",
	Run: func(cmd *cobra.Command, args []string) {
		userName, _ := cmd.Flags().GetString("user")
		keyID, _ := cmd.Flags().GetString("key-id")
		if userName == "" || keyID == "" {
			fmt.Fprintf(os.Stderr, "Error: --user and --key-id are required\n")
			os.Exit(1)
		}

		body := map[string]string{
			"user_name":     userName,
			"access_key_id": keyID,
		}

		resp, err := iamRequest("DELETE", "/iam/access-keys/", body)
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

		fmt.Printf("Access key '%s' deleted\n", keyID)
	},
}

// --- User commands ---

var iamCreateUserCmd = &cobra.Command{
	Use:   "create-user [username]",
	Short: "Create a new IAM user",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		displayName, _ := cmd.Flags().GetString("display-name")
		body := map[string]string{
			"name": args[0],
		}
		if displayName != "" {
			body["display_name"] = displayName
		}

		resp, err := iamRequest("POST", "/iam/users", body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()

		respBody, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusCreated {
			fmt.Fprintf(os.Stderr, "Error: %s\n", string(respBody))
			os.Exit(1)
		}

		fmt.Printf("User '%s' created successfully\n", args[0])
	},
}

var iamListUsersCmd = &cobra.Command{
	Use:   "list-users",
	Short: "List all IAM users",
	Run: func(cmd *cobra.Command, args []string) {
		resp, err := iamRequest("GET", "/iam/users", nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()

		respBody, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(os.Stderr, "Error: %s\n", string(respBody))
			os.Exit(1)
		}

		var result map[string]interface{}
		json.Unmarshal(respBody, &result)

		users, ok := result["users"].([]interface{})
		if !ok || len(users) == 0 {
			fmt.Println("No IAM users found")
			return
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tDISPLAY NAME\tKEYS\tGROUPS\tPOLICIES")
		for _, u := range users {
			user := u.(map[string]interface{})
			fmt.Fprintf(w, "%v\t%v\t%v\t%v\t%v\n",
				user["name"], user["display_name"], user["access_key_count"],
				user["groups"], user["attached_policies"])
		}
		w.Flush()
	},
}

var iamDeleteUserCmd = &cobra.Command{
	Use:   "delete-user [username]",
	Short: "Delete an IAM user",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		resp, err := iamRequest("DELETE", "/iam/users/"+args[0], nil)
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

		fmt.Printf("User '%s' deleted\n", args[0])
	},
}

// --- Group commands ---

var iamGroupCmd = &cobra.Command{
	Use:   "group",
	Short: "Group management commands",
}

var iamCreateGroupCmd = &cobra.Command{
	Use:   "create [groupname]",
	Short: "Create a new IAM group",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		desc, _ := cmd.Flags().GetString("description")
		body := map[string]string{"name": args[0]}
		if desc != "" {
			body["description"] = desc
		}

		resp, err := iamRequest("POST", "/iam/groups", body)
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

		fmt.Printf("Group '%s' created\n", args[0])
	},
}

var iamListGroupsCmd = &cobra.Command{
	Use:   "list",
	Short: "List all IAM groups",
	Run: func(cmd *cobra.Command, args []string) {
		resp, err := iamRequest("GET", "/iam/groups", nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()

		respBody, _ := io.ReadAll(resp.Body)
		fmt.Println(string(respBody))
	},
}

var iamDeleteGroupCmd = &cobra.Command{
	Use:   "delete [groupname]",
	Short: "Delete an IAM group",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		resp, err := iamRequest("DELETE", "/iam/groups/"+args[0], nil)
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

		fmt.Printf("Group '%s' deleted\n", args[0])
	},
}

var iamAddUserToGroupCmd = &cobra.Command{
	Use:   "add-user",
	Short: "Add a user to a group",
	Run: func(cmd *cobra.Command, args []string) {
		userName, _ := cmd.Flags().GetString("user")
		groupName, _ := cmd.Flags().GetString("group")
		if userName == "" || groupName == "" {
			fmt.Fprintf(os.Stderr, "Error: --user and --group are required\n")
			os.Exit(1)
		}

		body := map[string]string{"user_name": userName}
		resp, err := iamRequest("POST", "/iam/groups/"+groupName+"/users", body)
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

		fmt.Printf("User '%s' added to group '%s'\n", userName, groupName)
	},
}

var iamRemoveUserFromGroupCmd = &cobra.Command{
	Use:   "remove-user",
	Short: "Remove a user from a group",
	Run: func(cmd *cobra.Command, args []string) {
		userName, _ := cmd.Flags().GetString("user")
		groupName, _ := cmd.Flags().GetString("group")
		if userName == "" || groupName == "" {
			fmt.Fprintf(os.Stderr, "Error: --user and --group are required\n")
			os.Exit(1)
		}

		body := map[string]string{"user_name": userName}
		resp, err := iamRequest("DELETE", "/iam/groups/"+groupName+"/users", body)
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

		fmt.Printf("User '%s' removed from group '%s'\n", userName, groupName)
	},
}

// --- Policy commands ---

var iamPolicyCmd = &cobra.Command{
	Use:   "policy",
	Short: "Policy management commands",
}

var iamCreatePolicyCmd = &cobra.Command{
	Use:   "create [policyname]",
	Short: "Create a new IAM policy",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		desc, _ := cmd.Flags().GetString("description")
		policyFile, _ := cmd.Flags().GetString("file")

		if policyFile == "" {
			fmt.Fprintf(os.Stderr, "Error: --file is required (path to policy JSON file)\n")
			os.Exit(1)
		}

		policyData, err := os.ReadFile(policyFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading policy file: %v\n", err)
			os.Exit(1)
		}

		var policyDoc interface{}
		if err := json.Unmarshal(policyData, &policyDoc); err != nil {
			fmt.Fprintf(os.Stderr, "Error parsing policy JSON: %v\n", err)
			os.Exit(1)
		}

		body := map[string]interface{}{
			"name":        args[0],
			"description": desc,
			"document":    policyDoc,
		}

		resp, err := iamRequest("POST", "/iam/policies", body)
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

		fmt.Printf("Policy '%s' created\n", args[0])
	},
}

var iamListPoliciesCmd = &cobra.Command{
	Use:   "list",
	Short: "List all IAM policies",
	Run: func(cmd *cobra.Command, args []string) {
		resp, err := iamRequest("GET", "/iam/policies", nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()

		respBody, _ := io.ReadAll(resp.Body)
		fmt.Println(string(respBody))
	},
}

var iamDeletePolicyCmd = &cobra.Command{
	Use:   "delete [policyname]",
	Short: "Delete an IAM policy",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		resp, err := iamRequest("DELETE", "/iam/policies/"+args[0], nil)
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

		fmt.Printf("Policy '%s' deleted\n", args[0])
	},
}

var iamAttachUserPolicyCmd = &cobra.Command{
	Use:   "attach-user-policy",
	Short: "Attach a policy to a user",
	Run: func(cmd *cobra.Command, args []string) {
		userName, _ := cmd.Flags().GetString("user")
		policyName, _ := cmd.Flags().GetString("policy")
		if userName == "" || policyName == "" {
			fmt.Fprintf(os.Stderr, "Error: --user and --policy are required\n")
			os.Exit(1)
		}

		body := map[string]string{"policy_name": policyName}
		resp, err := iamRequest("POST", "/iam/users/"+userName+"/policies", body)
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

		fmt.Printf("Policy '%s' attached to user '%s'\n", policyName, userName)
	},
}

var iamDetachUserPolicyCmd = &cobra.Command{
	Use:   "detach-user-policy",
	Short: "Detach a policy from a user",
	Run: func(cmd *cobra.Command, args []string) {
		userName, _ := cmd.Flags().GetString("user")
		policyName, _ := cmd.Flags().GetString("policy")
		if userName == "" || policyName == "" {
			fmt.Fprintf(os.Stderr, "Error: --user and --policy are required\n")
			os.Exit(1)
		}

		body := map[string]string{"policy_name": policyName}
		resp, err := iamRequest("DELETE", "/iam/users/"+userName+"/policies", body)
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

		fmt.Printf("Policy '%s' detached from user '%s'\n", policyName, userName)
	},
}

var iamAttachGroupPolicyCmd = &cobra.Command{
	Use:   "attach-group-policy",
	Short: "Attach a policy to a group",
	Run: func(cmd *cobra.Command, args []string) {
		groupName, _ := cmd.Flags().GetString("group")
		policyName, _ := cmd.Flags().GetString("policy")
		if groupName == "" || policyName == "" {
			fmt.Fprintf(os.Stderr, "Error: --group and --policy are required\n")
			os.Exit(1)
		}

		body := map[string]string{"policy_name": policyName}
		resp, err := iamRequest("POST", "/iam/groups/"+groupName+"/policies", body)
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

		fmt.Printf("Policy '%s' attached to group '%s'\n", policyName, groupName)
	},
}

// --- Role commands ---

var iamRoleCmd = &cobra.Command{
	Use:   "role",
	Short: "Role management commands",
}

var iamCreateRoleCmd = &cobra.Command{
	Use:   "create [rolename]",
	Short: "Create a new IAM role",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		desc, _ := cmd.Flags().GetString("description")
		trustFile, _ := cmd.Flags().GetString("trust-policy-file")
		maxSession, _ := cmd.Flags().GetInt("max-session")

		if trustFile == "" {
			fmt.Fprintf(os.Stderr, "Error: --trust-policy-file is required\n")
			os.Exit(1)
		}

		trustData, err := os.ReadFile(trustFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading trust policy file: %v\n", err)
			os.Exit(1)
		}

		var trustDoc interface{}
		if err := json.Unmarshal(trustData, &trustDoc); err != nil {
			fmt.Fprintf(os.Stderr, "Error parsing trust policy JSON: %v\n", err)
			os.Exit(1)
		}

		body := map[string]interface{}{
			"name":                args[0],
			"description":         desc,
			"trust_policy":        trustDoc,
			"max_session_duration": maxSession,
		}

		resp, err := iamRequest("POST", "/iam/roles", body)
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

		fmt.Printf("Role '%s' created\n", args[0])
	},
}

var iamListRolesCmd = &cobra.Command{
	Use:   "list",
	Short: "List all IAM roles",
	Run: func(cmd *cobra.Command, args []string) {
		resp, err := iamRequest("GET", "/iam/roles", nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()

		respBody, _ := io.ReadAll(resp.Body)
		fmt.Println(string(respBody))
	},
}

var iamDeleteRoleCmd = &cobra.Command{
	Use:   "delete [rolename]",
	Short: "Delete an IAM role",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		resp, err := iamRequest("DELETE", "/iam/roles/"+args[0], nil)
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

		fmt.Printf("Role '%s' deleted\n", args[0])
	},
}

// --- Bucket Policy commands ---

var iamBucketPolicyCmd = &cobra.Command{
	Use:   "bucket-policy",
	Short: "Bucket policy management commands",
}

var iamSetBucketPolicyCmd = &cobra.Command{
	Use:   "set [bucket]",
	Short: "Set bucket policy from a JSON file",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		policyFile, _ := cmd.Flags().GetString("file")
		if policyFile == "" {
			fmt.Fprintf(os.Stderr, "Error: --file is required\n")
			os.Exit(1)
		}

		policyData, err := os.ReadFile(policyFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading policy file: %v\n", err)
			os.Exit(1)
		}

		var policyDoc interface{}
		if err := json.Unmarshal(policyData, &policyDoc); err != nil {
			fmt.Fprintf(os.Stderr, "Error parsing policy JSON: %v\n", err)
			os.Exit(1)
		}

		body := map[string]interface{}{"document": policyDoc}
		resp, err := iamRequest("PUT", "/iam/bucket-policies/"+args[0], body)
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

		fmt.Printf("Bucket policy set for '%s'\n", args[0])
	},
}

var iamGetBucketPolicyCmd = &cobra.Command{
	Use:   "get [bucket]",
	Short: "Get bucket policy",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		resp, err := iamRequest("GET", "/iam/bucket-policies/"+args[0], nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()

		respBody, _ := io.ReadAll(resp.Body)
		fmt.Println(string(respBody))
	},
}

var iamDeleteBucketPolicyCmd = &cobra.Command{
	Use:   "delete [bucket]",
	Short: "Delete bucket policy",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		resp, err := iamRequest("DELETE", "/iam/bucket-policies/"+args[0], nil)
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

		fmt.Printf("Bucket policy deleted for '%s'\n", args[0])
	},
}

// --- Helper ---

var iamSimulateCmd = &cobra.Command{
	Use:   "simulate",
	Short: "Simulate an IAM policy evaluation",
	Long:  "Simulate whether a specific action on a resource would be allowed for a principal",
	Run: func(cmd *cobra.Command, args []string) {
		action, _ := cmd.Flags().GetString("action")
		resource, _ := cmd.Flags().GetString("resource")
		principal, _ := cmd.Flags().GetString("principal")

		if action == "" || resource == "" || principal == "" {
			fmt.Fprintf(os.Stderr, "Error: --action, --resource, and --principal are required\n")
			os.Exit(1)
		}

		body := map[string]string{
			"action":    action,
			"resource":  resource,
			"principal": principal,
		}

		// Add optional fields
		sourceIP, _ := cmd.Flags().GetString("source-ip")
		if sourceIP != "" {
			body["source_ip"] = sourceIP
		}
		userAgent, _ := cmd.Flags().GetString("user-agent")
		if userAgent != "" {
			body["user_agent"] = userAgent
		}

		resp, err := iamRequest("POST", "/admin/iam/simulate", body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()

		respBody, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(os.Stderr, "Error: %s\n", string(respBody))
			os.Exit(1)
		}

		var result map[string]interface{}
		if err := json.Unmarshal(respBody, &result); err != nil {
			fmt.Println(string(respBody))
			return
		}

		fmt.Printf("Decision:    %v\n", result["decision"])
		fmt.Printf("Matched By:  %v\n", result["matched_by"])
		fmt.Printf("Policy Type: %v\n", result["policy_type"])
		if details, ok := result["details"]; ok && details != "" {
			fmt.Printf("Details:     %v\n", details)
		}
	},
}

func iamRequest(method, path string, body interface{}) (*http.Response, error) {
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

	// Use IAM access key credentials from env or flags
	accessKey := os.Getenv("NEXUS_ACCESS_KEY_ID")
	secretKey := os.Getenv("NEXUS_SECRET_ACCESS_KEY")

	if accessKey != "" && secretKey != "" {
		req.SetBasicAuth(accessKey, secretKey)
	} else {
		// Fallback to admin user/password
		user := os.Getenv("NEXUS_ADMIN_USER")
		pass := os.Getenv("NEXUS_ADMIN_PASSWORD")
		if user != "" && pass != "" {
			req.SetBasicAuth(user, pass)
		}
	}

	client := &http.Client{}
	return client.Do(req)
}

func init() {
	// IAM root command
	rootCmd.AddCommand(iamCmd)

	// Direct IAM commands
	iamCmd.AddCommand(iamCreateUserCmd)
	iamCreateUserCmd.Flags().String("display-name", "", "Display name for the user")

	iamCmd.AddCommand(iamListUsersCmd)
	iamCmd.AddCommand(iamDeleteUserCmd)

	iamCmd.AddCommand(iamCreateAccessKeyCmd)
	iamCreateAccessKeyCmd.Flags().String("description", "", "Description for the access key")

	iamCmd.AddCommand(iamListAccessKeysCmd)
	iamCmd.AddCommand(iamDeleteAccessKeyCmd)
	iamDeleteAccessKeyCmd.Flags().String("user", "", "Username (required)")
	iamDeleteAccessKeyCmd.Flags().String("key-id", "", "Access key ID to delete (required)")

	// Group sub-commands
	iamCmd.AddCommand(iamGroupCmd)
	iamGroupCmd.AddCommand(iamCreateGroupCmd)
	iamCreateGroupCmd.Flags().String("description", "", "Group description")
	iamGroupCmd.AddCommand(iamListGroupsCmd)
	iamGroupCmd.AddCommand(iamDeleteGroupCmd)
	iamGroupCmd.AddCommand(iamAddUserToGroupCmd)
	iamAddUserToGroupCmd.Flags().String("user", "", "Username to add (required)")
	iamAddUserToGroupCmd.Flags().String("group", "", "Group name (required)")
	iamGroupCmd.AddCommand(iamRemoveUserFromGroupCmd)
	iamRemoveUserFromGroupCmd.Flags().String("user", "", "Username to remove (required)")
	iamRemoveUserFromGroupCmd.Flags().String("group", "", "Group name (required)")

	// Policy sub-commands
	iamCmd.AddCommand(iamPolicyCmd)
	iamPolicyCmd.AddCommand(iamCreatePolicyCmd)
	iamCreatePolicyCmd.Flags().String("description", "", "Policy description")
	iamCreatePolicyCmd.Flags().String("file", "", "Path to policy JSON file (required)")
	iamPolicyCmd.AddCommand(iamListPoliciesCmd)
	iamPolicyCmd.AddCommand(iamDeletePolicyCmd)
	iamPolicyCmd.AddCommand(iamAttachUserPolicyCmd)
	iamAttachUserPolicyCmd.Flags().String("user", "", "Username (required)")
	iamAttachUserPolicyCmd.Flags().String("policy", "", "Policy name (required)")
	iamPolicyCmd.AddCommand(iamDetachUserPolicyCmd)
	iamDetachUserPolicyCmd.Flags().String("user", "", "Username (required)")
	iamDetachUserPolicyCmd.Flags().String("policy", "", "Policy name (required)")
	iamPolicyCmd.AddCommand(iamAttachGroupPolicyCmd)
	iamAttachGroupPolicyCmd.Flags().String("group", "", "Group name (required)")
	iamAttachGroupPolicyCmd.Flags().String("policy", "", "Policy name (required)")

	// Role sub-commands
	iamCmd.AddCommand(iamRoleCmd)
	iamRoleCmd.AddCommand(iamCreateRoleCmd)
	iamCreateRoleCmd.Flags().String("description", "", "Role description")
	iamCreateRoleCmd.Flags().String("trust-policy-file", "", "Path to trust policy JSON file (required)")
	iamCreateRoleCmd.Flags().Int("max-session", 3600, "Max session duration in seconds")
	iamRoleCmd.AddCommand(iamListRolesCmd)
	iamRoleCmd.AddCommand(iamDeleteRoleCmd)

	// Bucket Policy sub-commands
	iamCmd.AddCommand(iamBucketPolicyCmd)
	iamBucketPolicyCmd.AddCommand(iamSetBucketPolicyCmd)
	iamSetBucketPolicyCmd.Flags().String("file", "", "Path to bucket policy JSON file (required)")
	iamBucketPolicyCmd.AddCommand(iamGetBucketPolicyCmd)
	iamBucketPolicyCmd.AddCommand(iamDeleteBucketPolicyCmd)

	// Simulate command
	iamCmd.AddCommand(iamSimulateCmd)
	iamSimulateCmd.Flags().String("action", "", "Action to simulate (e.g., s3:PutObject) (required)")
	iamSimulateCmd.Flags().String("resource", "", "Resource ARN to simulate (e.g., arn:nexus:s3:::mybucket/mykey) (required)")
	iamSimulateCmd.Flags().String("principal", "", "Principal (username) to simulate (required)")
	iamSimulateCmd.Flags().String("source-ip", "", "Source IP address for condition evaluation")
	iamSimulateCmd.Flags().String("user-agent", "", "User-Agent for condition evaluation")

}
