package raft

import (
	"context"
	"fmt"

	"github.com/hashicorp/raft"
)

// IsLeader checks if this node is the Raft leader.
func (n *RaftNode) IsLeader() bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.isLeader
}

// GetLeaderAddr returns the address of the current Raft leader.
func (n *RaftNode) GetLeaderAddr() string {
	_, addr := n.raft.LeaderWithID()
	return string(addr)
}

// State returns the current Raft state of this node.
func (n *RaftNode) State() raft.RaftState {
	return n.raft.State()
}

// LinearizableRead performs a linearizable read by verifying leadership
// before allowing the read to proceed.
func (n *RaftNode) LinearizableRead(ctx context.Context) error {
	f := n.raft.VerifyLeader()
	errCh := make(chan error, 1)
	go func() {
		errCh <- f.Error()
	}()
	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("linearizable read failed - not leader: %w", err)
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
