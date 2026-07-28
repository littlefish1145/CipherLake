package raft

import (
	"fmt"
	"time"

	"cipherlake/internal/config"
	"github.com/hashicorp/raft"
)

// BootstrapSingle bootstraps a single-node Raft cluster.
func BootstrapSingle(dataDir string, cfg *config.RaftConfig) (*RaftNode, error) {
	node, err := NewRaftNode(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create raft node: %w", err)
	}

	// Bootstrap with a single server
	configuration := raft.Configuration{
		Servers: []raft.Server{
			{
				ID:      raft.ServerID(cfg.NodeID),
				Address: raft.ServerAddress(cfg.ListenAddr),
			},
		},
	}

	f := node.raft.BootstrapCluster(configuration)
	if err := f.Error(); err != nil {
		if err != raft.ErrCantBootstrap {
			node.Shutdown()
			return nil, fmt.Errorf("failed to bootstrap single node cluster: %w", err)
		}
		// Cluster already bootstrapped, that's okay
	}

	return node, nil
}

// BootstrapCluster bootstraps a multi-node Raft cluster.
func BootstrapCluster(dataDir string, cfg *config.RaftConfig) (*RaftNode, error) {
	node, err := NewRaftNode(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create raft node: %w", err)
	}

	if len(cfg.Peers) == 0 {
		node.Shutdown()
		return nil, fmt.Errorf("peers list is required for cluster bootstrap")
	}

	// Build server configuration from peers
	servers := []raft.Server{
		{
			ID:      raft.ServerID(cfg.NodeID),
			Address: raft.ServerAddress(cfg.ListenAddr),
		},
	}

	for i, peer := range cfg.Peers {
		peerID := raft.ServerID(fmt.Sprintf("node%d", i+2))
		servers = append(servers, raft.Server{
			ID:      peerID,
			Address: raft.ServerAddress(peer),
		})
	}

	configuration := raft.Configuration{
		Servers: servers,
	}

	f := node.raft.BootstrapCluster(configuration)
	if err := f.Error(); err != nil {
		if err != raft.ErrCantBootstrap {
			node.Shutdown()
			return nil, fmt.Errorf("failed to bootstrap cluster: %w", err)
		}
	}

	return node, nil
}

// JoinCluster joins an existing Raft cluster by contacting the node at existingNodeAddr.
func (n *RaftNode) JoinCluster(existingNodeAddr string) error {
	// Add this node as a voter to the existing cluster
	// This is typically called via an RPC to the existing leader,
	// but we use the raft AddVoter API directly for simplicity.
	f := n.raft.AddVoter(
		raft.ServerID(n.raft.String()),
		raft.ServerAddress(existingNodeAddr),
		0,
		30*time.Second,
	)
	if err := f.Error(); err != nil {
		return fmt.Errorf("failed to join cluster: %w", err)
	}
	return nil
}

// AddPeer adds a new voter to the Raft cluster.
func (n *RaftNode) AddPeer(nodeID, addr string) error {
	f := n.raft.AddVoter(
		raft.ServerID(nodeID),
		raft.ServerAddress(addr),
		0,
		30*time.Second,
	)
	if err := f.Error(); err != nil {
		return fmt.Errorf("failed to add peer %s at %s: %w", nodeID, addr, err)
	}
	return nil
}

// RemoveNode removes a node from the Raft cluster.
func (n *RaftNode) RemoveNode(nodeID string) error {
	f := n.raft.RemoveServer(
		raft.ServerID(nodeID),
		0,
		30*time.Second,
	)
	if err := f.Error(); err != nil {
		return fmt.Errorf("failed to remove node %s: %w", nodeID, err)
	}
	return nil
}

// DemoteVoter demotes a voter to a nonvoter in the Raft cluster.
func (n *RaftNode) DemoteVoter(nodeID string) error {
	f := n.raft.DemoteVoter(
		raft.ServerID(nodeID),
		0,
		30*time.Second,
	)
	if err := f.Error(); err != nil {
		return fmt.Errorf("failed to demote voter %s: %w", nodeID, err)
	}
	return nil
}

// BootstrapStatic bootstraps a Raft cluster with a fixed peer set.
// It adds the local node plus every address in cfg.EffectiveClusterPeers()
// as a voter. Peer ServerIDs are taken from cfg.ClusterPeerIDs when
// provided (same order), otherwise the peer address is used as the ID.
// The call is idempotent: if the cluster is already bootstrapped it
// returns nil.
func (n *RaftNode) BootstrapStatic(cfg *config.RaftConfig) error {
	peers := cfg.EffectiveClusterPeers()
	peerIDs := cfg.ClusterPeerIDs
	localAddr := raft.ServerAddress(cfg.ListenAddr)
	servers := []raft.Server{
		{
			ID:      raft.ServerID(cfg.NodeID),
			Address: localAddr,
		},
	}
	for i, peer := range peers {
		peer := peer
		if peer == "" {
			continue
		}
		// Skip the local node; it was added above with its configured
		// NodeID. The remaining peers are remote voters.
		if raft.ServerAddress(peer) == localAddr {
			continue
		}
		id := raft.ServerID(peer)
		if i < len(peerIDs) && peerIDs[i] != "" {
			id = raft.ServerID(peerIDs[i])
		}
		servers = append(servers, raft.Server{
			ID:      id,
			Address: raft.ServerAddress(peer),
		})
	}
	configuration := raft.Configuration{Servers: servers}
	f := n.raft.BootstrapCluster(configuration)
	if err := f.Error(); err != nil {
		if err == raft.ErrCantBootstrap {
			return nil
		}
		return fmt.Errorf("failed to bootstrap static cluster: %w", err)
	}
	return nil
}
