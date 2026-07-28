package raft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"cipherlake/internal/config"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	bolt "go.etcd.io/bbolt"
)

// RaftNode wraps a hashicorp/raft instance with CipherLake-specific configuration.
type RaftNode struct {
	raft            *raft.Raft
	fsm             raft.FSM
	fsmCloser        io.Closer
	transport       raft.Transport
	transportCloser io.Closer
	logStore        io.Closer
	stableStore     io.Closer
	isLeader        bool
	mu              sync.RWMutex
}

// NewRaftNode creates and configures a new RaftNode from the given RaftConfig,
// using a BoltFSM as the state machine (backward-compatible).
func NewRaftNode(cfg *config.RaftConfig) (*RaftNode, error) {
	fsm, err := NewBoltFSM(filepath.Join(cfg.DataDir, "fsm.db"))
	if err != nil {
		return nil, fmt.Errorf("failed to create bolt FSM: %w", err)
	}
	return NewRaftNodeWithFSM(cfg, fsm, fsm)
}

// NewRaftNodeWithFSM creates a RaftNode with an externally-provided FSM.
// fsmCloser may be nil if the FSM does not need explicit close (it will be
// invoked by RaftNode.Shutdown). Use this constructor to plug a custom FSM
// (e.g. plugin-loader's LoaderFSM) without touching the default BoltFSM path.
func NewRaftNodeWithFSM(cfg *config.RaftConfig, fsm raft.FSM, fsmCloser io.Closer) (*RaftNode, error) {
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
	// LeaderLeaseTimeout must be <= HeartbeatTimeout; scale it down for
	// aggressive test configs without disabling leader leasing.
	if heartbeatTimeout > 0 {
		lease := heartbeatTimeout / 2
		if lease < 10*time.Millisecond {
			lease = 10 * time.Millisecond
		}
		raftCfg.LeaderLeaseTimeout = lease
	}
	// Pre-vote is enabled by default (PreVoteDisabled defaults to false).
	// Explicitly ensure it's not disabled for split-brain prevention.
	raftCfg.PreVoteDisabled = false

	// Create TCP transport
	addr := cfg.ListenAddr
	tcpTransport, err := raft.NewTCPTransport(addr, nil, 3, 10*time.Second, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("failed to create tcp transport: %w", err)
	}

	// Create log store using BoltDB
	logStore, err := raftboltdb.NewBoltStore(filepath.Join(cfg.DataDir, "raft-log.db"))
	if err != nil {
		tcpTransport.Close()
		return nil, fmt.Errorf("failed to create log store: %w", err)
	}

	// Create stable store using BoltDB
	stableStore, err := raftboltdb.NewBoltStore(filepath.Join(cfg.DataDir, "raft-stable.db"))
	if err != nil {
		logStore.Close()
		tcpTransport.Close()
		return nil, fmt.Errorf("failed to create stable store: %w", err)
	}

	// Create snapshot store
	snapshotStore, err := raft.NewFileSnapshotStore(cfg.DataDir, 2, os.Stderr)
	if err != nil {
		stableStore.Close()
		logStore.Close()
		tcpTransport.Close()
		return nil, fmt.Errorf("failed to create snapshot store: %w", err)
	}

	// Create the raft instance with the injected FSM
	raftInst, err := raft.NewRaft(raftCfg, fsm, logStore, stableStore, snapshotStore, tcpTransport)
	if err != nil {
		stableStore.Close()
		logStore.Close()
		tcpTransport.Close()
		return nil, fmt.Errorf("failed to create raft instance: %w", err)
	}

	node := &RaftNode{
		raft:            raftInst,
		fsm:             fsm,
		fsmCloser:       fsmCloser,
		transport:       tcpTransport,
		transportCloser: tcpTransport,
		logStore:        logStore,
		stableStore:     stableStore,
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

// ApplyRaw submits an arbitrary JSON-encoded FSM operation to the Raft log
// and returns the FSM's Apply response. Use this with custom FSMs plugged
// in via NewRaftNodeWithFSM (e.g. the plugin loader's LoaderFSM) whose
// operation type is not the package-level FSMOperation.
//
// Returns:
//   - (*FSMApplyResult, true)  if the FSM returned one (inspect .Success / .Error)
//   - (nil, nil)               if the FSM returned nil or an unrecognized type
//   - (nil, err)               on Raft-level errors (timeout, shutdown, etc.)
func (n *RaftNode) ApplyRaw(ctx context.Context, opJSON []byte, timeout time.Duration) (*FSMApplyResult, bool, error) {
	if n.raft == nil {
		return nil, false, fmt.Errorf("raft instance is nil")
	}
	f := n.raft.Apply(opJSON, timeout)
	if err := f.Error(); err != nil {
		return nil, false, fmt.Errorf("raft apply failed: %w", err)
	}
	if result, ok := f.Response().(*FSMApplyResult); ok {
		return result, true, nil
	}
	return nil, false, nil
}

// Underlying exposes the underlying *raft.Raft for callers that need
// APIs not yet wrapped by RaftNode (e.g. Snapshot, State, LeadershipTransfer).
// Prefer adding a wrapper method when one is missing.
func (n *RaftNode) Underlying() *raft.Raft {
	return n.raft
}

// BootstrapSingle bootstraps a single-node Raft cluster using the given
// configuration. Safe to call on an already-bootstrapped cluster
// (returns nil on raft.ErrCantBootstrap). Use this when NewRaftNode or
// NewRaftNodeWithFSM has already been used to create the node.
func (n *RaftNode) BootstrapSingle(cfg *config.RaftConfig) error {
	configuration := raft.Configuration{
		Servers: []raft.Server{
			{
				ID:      raft.ServerID(cfg.NodeID),
				Address: raft.ServerAddress(cfg.ListenAddr),
			},
		},
	}
	f := n.raft.BootstrapCluster(configuration)
	if err := f.Error(); err != nil {
		if err == raft.ErrCantBootstrap {
			return nil // already bootstrapped, that's fine
		}
		return fmt.Errorf("failed to bootstrap single node cluster: %w", err)
	}
	return nil
}

// Shutdown gracefully shuts down the raft node.
func (n *RaftNode) Shutdown() error {
	var errs []error
	if n.raft != nil {
		f := n.raft.Shutdown()
		if err := f.Error(); err != nil {
			errs = append(errs, fmt.Errorf("raft shutdown failed: %w", err))
		}
	}
	if n.transportCloser != nil {
		if err := n.transportCloser.Close(); err != nil {
			errs = append(errs, fmt.Errorf("raft transport close failed: %w", err))
		}
	}
	if n.fsmCloser != nil {
		if closer, ok := n.fsmCloser.(interface{ Close() error }); ok {
			if err := closer.Close(); err != nil {
				errs = append(errs, fmt.Errorf("raft FSM close failed: %w", err))
			}
		}
	}
	if n.logStore != nil {
		if err := n.logStore.Close(); err != nil {
			errs = append(errs, fmt.Errorf("raft log store close failed: %w", err))
		}
	}
	if n.stableStore != nil {
		if err := n.stableStore.Close(); err != nil {
			errs = append(errs, fmt.Errorf("raft stable store close failed: %w", err))
		}
	}
	return errors.Join(errs...)
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
	Type   string          `json:"type"` // "put_object", "delete_object", "create_bucket", etc.
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
