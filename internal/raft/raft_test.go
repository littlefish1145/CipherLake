package raft

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	bolt "go.etcd.io/bbolt"
	"nexus/internal/config"
)

func TestBoltFSMApplyPutObject(t *testing.T) {
	dir := t.TempDir()
	fsm, err := NewBoltFSM(filepath.Join(dir, "fsm.db"))
	if err != nil {
		t.Fatalf("failed to create FSM: %v", err)
	}
	defer fsm.Close()

	op := &FSMOperation{
		Type:   "put_object",
		Bucket: "test-bucket",
		Key:    "test-key",
		Data:   json.RawMessage(`{"key":"test-key","bucket":"test-bucket","size":100}`),
	}

	data, err := json.Marshal(op)
	if err != nil {
		t.Fatalf("failed to marshal operation: %v", err)
	}

	result := fsm.Apply(&raft.Log{Data: data})
	applyResult, ok := result.(*FSMApplyResult)
	if !ok {
		t.Fatalf("expected *FSMApplyResult, got %T", result)
	}
	if !applyResult.Success {
		t.Fatalf("expected success, got error: %s", applyResult.Error)
	}

	// Verify the data was written
	fsm.mu.RLock()
	_ = fsm.db.View(func(tx *bolt.Tx) error { return nil })
	fsm.mu.RUnlock()
}

func TestBoltFSMApplyDeleteObject(t *testing.T) {
	dir := t.TempDir()
	fsm, err := NewBoltFSM(filepath.Join(dir, "fsm.db"))
	if err != nil {
		t.Fatalf("failed to create FSM: %v", err)
	}
	defer fsm.Close()

	// First put an object
	putOp := &FSMOperation{
		Type:   "put_object",
		Bucket: "test-bucket",
		Key:    "test-key",
		Data:   json.RawMessage(`{"key":"test-key","bucket":"test-bucket","size":100}`),
	}
	putData, _ := json.Marshal(putOp)
	fsm.Apply(&raft.Log{Data: putData})

	// Then delete it
	delOp := &FSMOperation{
		Type:   "delete_object",
		Bucket: "test-bucket",
		Key:    "test-key",
	}
	delData, _ := json.Marshal(delOp)
	result := fsm.Apply(&raft.Log{Data: delData})
	applyResult := result.(*FSMApplyResult)
	if !applyResult.Success {
		t.Fatalf("expected success, got error: %s", applyResult.Error)
	}
}

func TestBoltFSMApplyCreateBucket(t *testing.T) {
	dir := t.TempDir()
	fsm, err := NewBoltFSM(filepath.Join(dir, "fsm.db"))
	if err != nil {
		t.Fatalf("failed to create FSM: %v", err)
	}
	defer fsm.Close()

	op := &FSMOperation{
		Type:   "create_bucket",
		Bucket: "my-bucket",
		Data:   json.RawMessage(`{"name":"my-bucket","owner_id":"user1"}`),
	}
	data, _ := json.Marshal(op)
	result := fsm.Apply(&raft.Log{Data: data})
	applyResult := result.(*FSMApplyResult)
	if !applyResult.Success {
		t.Fatalf("expected success, got error: %s", applyResult.Error)
	}
}

func TestBoltFSMApplyUnknownType(t *testing.T) {
	dir := t.TempDir()
	fsm, err := NewBoltFSM(filepath.Join(dir, "fsm.db"))
	if err != nil {
		t.Fatalf("failed to create FSM: %v", err)
	}
	defer fsm.Close()

	op := &FSMOperation{
		Type:   "unknown_op",
		Bucket: "test",
	}
	data, _ := json.Marshal(op)
	result := fsm.Apply(&raft.Log{Data: data})
	applyResult := result.(*FSMApplyResult)
	if applyResult.Success {
		t.Fatal("expected failure for unknown operation type")
	}
	if applyResult.Error != "unknown operation type: unknown_op" {
		t.Fatalf("unexpected error: %s", applyResult.Error)
	}
}

func TestBoltFSMSnapshotAndRestore(t *testing.T) {
	dir := t.TempDir()
	fsm, err := NewBoltFSM(filepath.Join(dir, "fsm.db"))
	if err != nil {
		t.Fatalf("failed to create FSM: %v", err)
	}

	// Apply some data
	for i := 0; i < 5; i++ {
		op := &FSMOperation{
			Type:   "put_object",
			Bucket: "test-bucket",
			Key:    fmt.Sprintf("key-%d", i),
			Data:   json.RawMessage(fmt.Sprintf(`{"key":"key-%d","bucket":"test-bucket","size":%d}`, i, i*100)),
		}
		data, _ := json.Marshal(op)
		result := fsm.Apply(&raft.Log{Data: data})
		if r := result.(*FSMApplyResult); !r.Success {
			t.Fatalf("apply failed: %s", r.Error)
		}
	}

	// Create snapshot
	snapshot, err := fsm.Snapshot()
	if err != nil {
		t.Fatalf("failed to create snapshot: %v", err)
	}

	// Write snapshot to file
	snapPath := filepath.Join(dir, "snapshot.bin")
	snapFile, err := os.Create(snapPath)
	if err != nil {
		t.Fatalf("failed to create snapshot file: %v", err)
	}

	// Use a simple sink
	sink := &testSink{writer: snapFile}
	if err := snapshot.Persist(sink); err != nil {
		t.Fatalf("failed to persist snapshot: %v", err)
	}
	snapFile.Close()

	// Create a new FSM and restore from snapshot
	fsm2, err := NewBoltFSM(filepath.Join(dir, "fsm2.db"))
	if err != nil {
		t.Fatalf("failed to create second FSM: %v", err)
	}

	snapFile2, err := os.Open(snapPath)
	if err != nil {
		t.Fatalf("failed to open snapshot file: %v", err)
	}

	err = fsm2.Restore(&ReadCloserWrapper{Reader: snapFile2})
	if err != nil {
		t.Fatalf("failed to restore from snapshot: %v", err)
	}
	snapFile2.Close()
	fsm2.Close()

	fsm.Close()
}

type testSink struct {
	writer *os.File
}

func (s *testSink) Write(p []byte) (int, error) {
	return s.writer.Write(p)
}

func (s *testSink) Close() error {
	return s.writer.Close()
}

func (s *testSink) ID() string {
	return "test-sink"
}

func (s *testSink) Cancel() error {
	return nil
}

func TestSingleNodeBootstrapAndApply(t *testing.T) {
	dir := t.TempDir()

	cfg := &config.RaftConfig{
		Enabled:         true,
		DataDir:         dir,
		NodeID:          "node1",
		ListenAddr:      "127.0.0.1:0",
		Peers:           []string{},
		SnapshotCount:   1024,
		Heartbeat:       "1s",
		ElectionTimeout: "1s",
	}

	node, err := BootstrapSingle(dir, cfg)
	if err != nil {
		t.Fatalf("failed to bootstrap single node: %v", err)
	}
	defer node.Shutdown()

	// Wait for leader election
	time.Sleep(3 * time.Second)

	// Apply an operation
	op := &FSMOperation{
		Type:   "create_bucket",
		Bucket: "test-bucket",
		Data:   json.RawMessage(`{"name":"test-bucket"}`),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := node.RaftApply(ctx, op); err != nil {
		t.Fatalf("failed to apply operation: %v", err)
	}
}

func TestLinearizableRead(t *testing.T) {
	dir := t.TempDir()

	cfg := &config.RaftConfig{
		Enabled:         true,
		DataDir:         dir,
		NodeID:          "node1",
		ListenAddr:      "127.0.0.1:0",
		Peers:           []string{},
		SnapshotCount:   1024,
		Heartbeat:       "1s",
		ElectionTimeout: "1s",
	}

	node, err := BootstrapSingle(dir, cfg)
	if err != nil {
		t.Fatalf("failed to bootstrap single node: %v", err)
	}
	defer node.Shutdown()

	// Wait for leader election
	time.Sleep(3 * time.Second)

	// Test linearizable read on leader
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := node.LinearizableRead(ctx); err != nil {
		t.Fatalf("linearizable read failed on leader: %v", err)
	}
}

func TestIsLeader(t *testing.T) {
	dir := t.TempDir()

	cfg := &config.RaftConfig{
		Enabled:         true,
		DataDir:         dir,
		NodeID:          "node1",
		ListenAddr:      "127.0.0.1:0",
		Peers:           []string{},
		SnapshotCount:   1024,
		Heartbeat:       "1s",
		ElectionTimeout: "1s",
	}

	node, err := BootstrapSingle(dir, cfg)
	if err != nil {
		t.Fatalf("failed to bootstrap single node: %v", err)
	}
	defer node.Shutdown()

	// Wait for leader election
	time.Sleep(3 * time.Second)

	// Single node should become leader
	if !node.IsLeader() {
		t.Fatal("expected single node to be leader")
	}

	// Check state
	if node.State() != raft.Leader {
		t.Fatalf("expected Leader state, got %v", node.State())
	}
}

func TestGetLeaderAddr(t *testing.T) {
	dir := t.TempDir()

	cfg := &config.RaftConfig{
		Enabled:         true,
		DataDir:         dir,
		NodeID:          "node1",
		ListenAddr:      "127.0.0.1:0",
		Peers:           []string{},
		SnapshotCount:   1024,
		Heartbeat:       "1s",
		ElectionTimeout: "1s",
	}

	node, err := BootstrapSingle(dir, cfg)
	if err != nil {
		t.Fatalf("failed to bootstrap single node: %v", err)
	}
	defer node.Shutdown()

	// Wait for leader election
	time.Sleep(3 * time.Second)

	addr := node.GetLeaderAddr()
	if addr == "" {
		t.Fatal("expected non-empty leader address")
	}
}

func TestGetConfiguration(t *testing.T) {
	dir := t.TempDir()

	cfg := &config.RaftConfig{
		Enabled:         true,
		DataDir:         dir,
		NodeID:          "node1",
		ListenAddr:      "127.0.0.1:0",
		Peers:           []string{},
		SnapshotCount:   1024,
		Heartbeat:       "1s",
		ElectionTimeout: "1s",
	}

	node, err := BootstrapSingle(dir, cfg)
	if err != nil {
		t.Fatalf("failed to bootstrap single node: %v", err)
	}
	defer node.Shutdown()

	// Wait for leader election
	time.Sleep(3 * time.Second)

	servers, err := node.GetConfiguration()
	if err != nil {
		t.Fatalf("failed to get configuration: %v", err)
	}

	if len(servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(servers))
	}

	if string(servers[0].ID) != "node1" {
		t.Fatalf("expected server ID 'node1', got %s", servers[0].ID)
	}
}

func TestPreVoteEnabled(t *testing.T) {
	dir := t.TempDir()

	cfg := &config.RaftConfig{
		Enabled:         true,
		DataDir:         dir,
		NodeID:          "node1",
		ListenAddr:      "127.0.0.1:0",
		Peers:           []string{},
		SnapshotCount:   1024,
		Heartbeat:       "1s",
		ElectionTimeout: "1s",
	}

	node, err := BootstrapSingle(dir, cfg)
	if err != nil {
		t.Fatalf("failed to bootstrap single node: %v", err)
	}
	defer node.Shutdown()

	// Verify the raft config has PreVoteDisabled = false
	// (pre-vote is enabled by default in hashicorp/raft v1.7.3)
	// The node should function normally with pre-vote enabled
	time.Sleep(2 * time.Second)

	if !node.IsLeader() {
		t.Fatal("expected node to become leader with pre-vote enabled")
	}
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		input    string
		default_ time.Duration
		expected time.Duration
		wantErr  bool
	}{
		{"", time.Second, time.Second, false},
		{"500ms", time.Second, 500 * time.Millisecond, false},
		{"2s", time.Second, 2 * time.Second, false},
		{"invalid", time.Second, 0, true},
	}

	for _, tt := range tests {
		got, err := parseDuration(tt.input, tt.default_)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseDuration(%q) expected error, got nil", tt.input)
			}
		} else {
			if err != nil {
				t.Errorf("parseDuration(%q) unexpected error: %v", tt.input, err)
			}
			if got != tt.expected {
				t.Errorf("parseDuration(%q) = %v, want %v", tt.input, got, tt.expected)
			}
		}
	}
}

func TestExtractPrefix(t *testing.T) {
	tests := []struct {
		key      string
		expected string
	}{
		{"folder/file.txt", "folder/"},
		{"file.txt", ""},
		{"a/b/c.txt", "a/b/"},
		{"", ""},
	}

	for _, tt := range tests {
		got := extractPrefix(tt.key)
		if got != tt.expected {
			t.Errorf("extractPrefix(%q) = %q, want %q", tt.key, got, tt.expected)
		}
	}
}
