package raft

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	bolt "go.etcd.io/bbolt"
	"nexus/internal/config"
)

// RaftNode wraps a hashicorp/raft instance with Nexus-specific configuration.
type RaftNode struct {
	raft      *raft.Raft
	fsm       *BoltFSM
	transport raft.Transport
	isLeader  bool
	mu        sync.RWMutex
}

// NewRaftNode creates and configures a new RaftNode from the given RaftConfig.
func NewRaftNode(cfg *config.RaftConfig) (*RaftNode, error) {
	if cfg == nil {
		return nil, fmt.Errorf("raft config is nil")
	}
	if cfg.NodeID == "" {
		return nil, fmt.Errorf("raft node_id is required")
	}
	if cfg.ListenAddr == "" {
		return nil, fmt.Errorf("raft listen_addr is required")
	}
	if cfg.DataDir == "" {
		return nil, fmt.Errorf("raft data_dir is required")
	}

	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create raft data directory: %w", err)
	}

	// Parse timeout durations
	heartbeatTimeout, err := parseDuration(cfg.Heartbeat, time.Second)
	if err != nil {
		return nil, fmt.Errorf("invalid heartbeat timeout: %w", err)
	}
	electionTimeout, err := parseDuration(cfg.ElectionTimeout, time.Second)
	if err != nil {
		return nil, fmt.Errorf("invalid election timeout: %w", err)
	}

	snapshotCount := uint64(cfg.SnapshotCount)
	if snapshotCount == 0 {
		snapshotCount = 8192
	}

	// Create raft config with reasonable defaults
	raftCfg := raft.DefaultConfig()
	raftCfg.LocalID = raft.ServerID(cfg.NodeID)
	raftCfg.HeartbeatTimeout = heartbeatTimeout
	raftCfg.ElectionTimeout = electionTimeout
	raftCfg.CommitTimeout = 50 * time.Millisecond
	raftCfg.SnapshotThreshold = snapshotCount
	// Pre-vote is enabled by default (PreVoteDisabled defaults to false).
	// Explicitly ensure it's not disabled for split-brain prevention.
	raftCfg.PreVoteDisabled = false

	// Create TCP transport
	addr := cfg.ListenAddr
	tcpTransport, err := raft.NewTCPTransport(addr, nil, 3, 10*time.Second, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("failed to create tcp transport: %w", err)
	}

	// Create BoltDB-based FSM
	fsm, err := NewBoltFSM(filepath.Join(cfg.DataDir, "fsm.db"))
	if err != nil {
		return nil, fmt.Errorf("failed to create bolt FSM: %w", err)
	}

	// Create log store using BoltDB
	logStore, err := raftboltdb.NewBoltStore(filepath.Join(cfg.DataDir, "raft-log.db"))
	if err != nil {
		return nil, fmt.Errorf("failed to create log store: %w", err)
	}

	// Create stable store using BoltDB
	stableStore, err := raftboltdb.NewBoltStore(filepath.Join(cfg.DataDir, "raft-stable.db"))
	if err != nil {
		return nil, fmt.Errorf("failed to create stable store: %w", err)
	}

	// Create snapshot store
	snapshotStore, err := raft.NewFileSnapshotStore(cfg.DataDir, 2, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("failed to create snapshot store: %w", err)
	}

	// Create the raft instance
	raftInst, err := raft.NewRaft(raftCfg, fsm, logStore, stableStore, snapshotStore, tcpTransport)
	if err != nil {
		return nil, fmt.Errorf("failed to create raft instance: %w", err)
	}

	node := &RaftNode{
		raft:      raftInst,
		fsm:       fsm,
		transport: tcpTransport,
	}

	// Watch for leadership changes
	go node.leadershipWatcher()

	return node, nil
}

// leadershipWatcher monitors leadership transitions.
func (n *RaftNode) leadershipWatcher() {
	for isLeader := range n.raft.LeaderCh() {
		n.mu.Lock()
		n.isLeader = isLeader
		n.mu.Unlock()
	}
}

// RaftApply submits an operation to the Raft log and waits for it to be committed.
func (n *RaftNode) RaftApply(ctx context.Context, op *FSMOperation) error {
	data, err := json.Marshal(op)
	if err != nil {
		return fmt.Errorf("failed to marshal operation: %w", err)
	}

	f := n.raft.Apply(data, 30*time.Second)
	if err := f.Error(); err != nil {
		return fmt.Errorf("raft apply failed: %w", err)
	}
	return nil
}

// Shutdown gracefully shuts down the raft node.
func (n *RaftNode) Shutdown() error {
	if n.fsm != nil {
		n.fsm.Close()
	}
	f := n.raft.Shutdown()
	if err := f.Error(); err != nil {
		return fmt.Errorf("raft shutdown failed: %w", err)
	}
	return nil
}

// BoltSnapshot implements raft.FSMSnapshot for BoltDB.
type BoltSnapshot struct {
	fsm *BoltFSM
}

// Persist writes the FSM snapshot to the given sink.
func (s *BoltSnapshot) Persist(sink raft.SnapshotSink) error {
	err := func() error {
		s.fsm.mu.RLock()
		defer s.fsm.mu.RUnlock()

		// Use a read-only transaction to write the database to the sink
		return s.fsm.db.View(func(tx *bolt.Tx) error {
			_, err := tx.WriteTo(sink)
			return err
		})
	}()

	if err != nil {
		sink.Cancel()
		return err
	}

	return sink.Close()
}

// Release is a no-op for BoltSnapshot.
func (s *BoltSnapshot) Release() {}

// parseDuration parses a duration string, falling back to default if empty.
func parseDuration(s string, defaultVal time.Duration) (time.Duration, error) {
	if s == "" {
		return defaultVal, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	return d, nil
}

// FSMOperation represents an operation to be applied to the FSM.
type FSMOperation struct {
	Type   string          `json:"type"`   // "put_object", "delete_object", "create_bucket", etc.
	Bucket string          `json:"bucket"`
	Key    string          `json:"key,omitempty"`
	Data   json.RawMessage `json:"data"`
}

// FSMApplyResult holds the result of an FSM apply operation.
type FSMApplyResult struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// ReadCloserWrapper wraps an io.Reader to implement io.ReadCloser.
type ReadCloserWrapper struct {
	io.Reader
}

func (r *ReadCloserWrapper) Close() error { return nil }
