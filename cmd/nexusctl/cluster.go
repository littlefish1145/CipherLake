package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"
)

var clusterCmd = &cobra.Command{
	Use:   "cluster",
	Short: "Cluster management commands",
	Long:  `Manage the Nexus Raft consensus cluster: check status, add/remove peers, and migrate from single-node to multi-node.`,
}

var clusterStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show cluster state, leader, and peers",
	RunE:  runClusterStatus,
}

var clusterAddPeerCmd = &cobra.Command{
	Use:   "add-peer <address>",
	Short: "Add a new peer to the cluster",
	Args:  cobra.ExactArgs(1),
	RunE:  runClusterAddPeer,
}

var clusterRemovePeerCmd = &cobra.Command{
	Use:   "remove-peer <node-id>",
	Short: "Remove a peer from the cluster",
	Args:  cobra.ExactArgs(1),
	RunE:  runClusterRemovePeer,
}

var clusterMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Migrate from single-node to 3-node cluster",
	RunE:  runClusterMigrate,
}

func init() {
	rootCmd.AddCommand(clusterCmd)
	clusterCmd.AddCommand(clusterStatusCmd)
	clusterCmd.AddCommand(clusterAddPeerCmd)
	clusterCmd.AddCommand(clusterRemovePeerCmd)
	clusterCmd.AddCommand(clusterMigrateCmd)

	clusterAddPeerCmd.Flags().String("node-id", "", "Node ID for the new peer (auto-generated if omitted)")
	clusterMigrateCmd.Flags().StringSlice("peers", []string{}, "Addresses of the two additional peers (e.g. \"10.0.0.2:9090,10.0.0.3:9090\")")
}

func runClusterStatus(cmd *cobra.Command, args []string) error {
	resp, err := serverRequest("GET", "/admin/cluster/status")
	if err != nil {
		return fmt.Errorf("connecting to server: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server error (status %d): %s", resp.StatusCode, string(body))
	}

	var data interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		return fmt.Errorf("parsing response: %w", err)
	}

	if outputFmt == "text" && queryStr == "" {
		printClusterStatus(data)
		return nil
	}

	out, err := formatOutput(data, outputFmt, queryStr)
	if err != nil {
		return err
	}
	fmt.Println(out)
	return nil
}

func printClusterStatus(data interface{}) {
	m, ok := data.(map[string]interface{})
	if !ok {
		fmt.Println(string(mustMarshal(data)))
		return
	}

	fmt.Printf("%-20s %v\n", "State:", m["state"])
	fmt.Printf("%-20s %v\n", "Leader ID:", m["leader_id"])
	fmt.Printf("%-20s %v\n", "Leader Address:", m["leader_addr"])

	if peers, ok := m["peers"].([]interface{}); ok {
		fmt.Printf("%-20s %d\n", "Peer Count:", len(peers))
		fmt.Println("\nPeers:")
		fmt.Printf("%-15s %-25s %s\n", "NODE ID", "ADDRESS", "SUFFRAGE")
		for _, p := range peers {
			if pm, ok := p.(map[string]interface{}); ok {
				fmt.Printf("%-15s %-25s %v\n", pm["id"], pm["address"], pm["suffrage"])
			}
		}
	}
}

func runClusterAddPeer(cmd *cobra.Command, args []string) error {
	addr := args[0]
	nodeID, _ := cmd.Flags().GetString("node-id")

	payload := map[string]string{
		"address": addr,
	}
	if nodeID != "" {
		payload["node_id"] = nodeID
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", address+"/admin/cluster/add-peer", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	user := os.Getenv("NEXUS_ADMIN_USER")
	pass := os.Getenv("NEXUS_ADMIN_PASSWORD")
	if user != "" && pass != "" {
		req.SetBasicAuth(user, pass)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server error (status %d): %s", resp.StatusCode, string(respBody))
	}

	result := map[string]string{
		"message": fmt.Sprintf("Peer %s added successfully", addr),
		"address": addr,
	}
	out, err := formatOutput(result, outputFmt, queryStr)
	if err != nil {
		return err
	}
	fmt.Println(out)
	return nil
}

func runClusterRemovePeer(cmd *cobra.Command, args []string) error {
	nodeID := args[0]

	payload := map[string]string{
		"node_id": nodeID,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", address+"/admin/cluster/remove-peer", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	user := os.Getenv("NEXUS_ADMIN_USER")
	pass := os.Getenv("NEXUS_ADMIN_PASSWORD")
	if user != "" && pass != "" {
		req.SetBasicAuth(user, pass)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server error (status %d): %s", resp.StatusCode, string(respBody))
	}

	result := map[string]string{
		"message": fmt.Sprintf("Peer %s removed successfully", nodeID),
		"node_id": nodeID,
	}
	out, err := formatOutput(result, outputFmt, queryStr)
	if err != nil {
		return err
	}
	fmt.Println(out)
	return nil
}

func runClusterMigrate(cmd *cobra.Command, args []string) error {
	peers, _ := cmd.Flags().GetStringSlice("peers")

	if len(peers) < 2 {
		return fmt.Errorf("at least 2 peer addresses are required for migration to 3-node cluster; use --peers flag")
	}

	payload := map[string]interface{}{
		"peers": peers,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", address+"/admin/cluster/migrate", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	user := os.Getenv("NEXUS_ADMIN_USER")
	pass := os.Getenv("NEXUS_ADMIN_PASSWORD")
	if user != "" && pass != "" {
		req.SetBasicAuth(user, pass)
	}

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server error (status %d): %s", resp.StatusCode, string(respBody))
	}

	result := map[string]interface{}{
		"message": "Cluster migration from single-node to 3-node initiated successfully",
		"peers":   peers,
	}
	out, err := formatOutput(result, outputFmt, queryStr)
	if err != nil {
		return err
	}
	fmt.Println(out)
	return nil
}

func mustMarshal(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}
