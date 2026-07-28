package raft

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/raft"
)

// IsLeader checks if this node is the Raft leader.
func (n *RaftNode) IsLeader() bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.isLeader
}

// GetLeaderAddr returns the address of the current Raft leader.
//
// hashicorp/raft v1.7.x returns (ServerAddress, ServerID) from LeaderWithID
// despite the older (ID, Address) ordering, so we use the first return value.
func (n *RaftNode) GetLeaderAddr() string {
	addr, _ := n.raft.LeaderWithID()
	return string(addr)
}

// State returns the current Raft state of this node.
func (n *RaftNode) State() raft.RaftState {
	return n.raft.State()
}

// LinearizableRead performs a linearizable read by issuing a Raft Barrier.
// The barrier is committed and applied locally before returning, so any
// subsequent local read is guaranteed to observe all entries committed at
// the time the barrier started. This approximates the ReadIndex semantics
// required by spec §12.3 P5-2 for hashicorp/raft versions that do not
// expose a native ReadIndex API.
func (n *RaftNode) LinearizableRead(ctx context.Context) error {
	// Use a timeout that respects the caller context. A short default keeps
	// tests responsive when the cluster is healthy.
	timeout := 5 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		if d := time.Until(deadline); d > 0 && d < timeout {
			timeout = d
		}
	}
	f := n.raft.Barrier(timeout)
	errCh := make(chan error, 1)
	go func() {
		errCh <- f.Error()
	}()
	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("read barrier failed: %w", err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// GetConfiguration returns the current Raft cluster configuration.
func (n *RaftNode) GetConfiguration() ([]raft.Server, error) {
	future := n.raft.GetConfiguration()
	if err := future.Error(); err != nil {
		return nil, fmt.Errorf("failed to get configuration: %w", err)
	}
	return future.Configuration().Servers, nil
}
