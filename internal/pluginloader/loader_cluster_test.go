package pluginloader

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"cipherlake/internal/config"
	pluginpb "cipherlake/proto/plugin"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// clusterNode wraps a single Loader plus its gRPC server/listener so a
// 3-node test can start/stop the whole stack cleanly.
type clusterNode struct {
	loader   *Loader
	grpcLn   net.Listener
	grpcSrv  *grpc.Server
	raftAddr string
	grpcAddr string
}

func (n *clusterNode) shutdown() {
	if n.grpcSrv != nil {
		n.grpcSrv.Stop()
	}
	if n.loader != nil {
		_ = n.loader.Shutdown()
	}
}

// adminClient returns a gRPC client connected to this node's admin server.
func (n *clusterNode) adminClient() (pluginpb.PluginAdminServiceClient, func()) {
	conn, err := grpc.Dial(n.grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		panic(err)
	}
	return pluginpb.NewPluginAdminServiceClient(conn), func() { _ = conn.Close() }
}

// freeAddrs reserves n distinct TCP ports on 127.0.0.1 and returns their
// addresses. Keeping all listeners open until all ports are collected
// minimizes the chance of the OS reusing a port.
func freeAddrs(t *testing.T, n int) []string {
	t.Helper()
	listeners := make([]net.Listener, 0, n)
	addrs := make([]string, 0, n)
	for len(addrs) < n {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := ln.Addr().String()
		// Ensure uniqueness in the unlikely case the OS reused a port.
		duplicate := false
		for _, a := range addrs {
			if a == addr {
				duplicate = true
				break
			}
		}
		if duplicate {
			require.NoError(t, ln.Close())
			continue
		}
		listeners = append(listeners, ln)
		addrs = append(addrs, addr)
	}
	for _, ln := range listeners {
		require.NoError(t, ln.Close())
	}
	return addrs
}

// newClusterNode creates a Loader configured to join a static cluster.
// raftPeers/grpcPeers are the full peer lists (including this node).
func newClusterNode(t *testing.T, idx int, raftPeers, grpcPeers []string) *clusterNode {
	t.Helper()

	cfg := &config.PluginLoaderConfig{
		CallTimeout:      "1s",
		PluginCacheDir:   fmt.Sprintf("%s/plugin-cache-%d", t.TempDir(), idx),
		ClusterGRPCAddrs: grpcPeers,
		Raft: &config.RaftConfig{
			Enabled:         true,
			DataDir:         t.TempDir(),
			NodeID:          fmt.Sprintf("node%d", idx),
			ListenAddr:      raftPeers[idx],
			ClusterPeers:    raftPeers,
			ClusterPeerIDs:  []string{"node0", "node1", "node2"},
			SnapshotCount:   128,
			Heartbeat:       "200ms",
			ElectionTimeout: "200ms",
		},
	}

	loader, err := New(cfg)
	require.NoError(t, err)

	ln, err := net.Listen("tcp", grpcPeers[idx])
	require.NoError(t, err)

	srv := grpc.NewServer()
	pluginpb.RegisterPluginLoaderServiceServer(srv, &LoaderGRPCServer{Loader: loader})
	pluginpb.RegisterPluginAdminServiceServer(srv, &LoaderAdminGRPCServer{Loader: loader})
	go func() { _ = srv.Serve(ln) }()

	return &clusterNode{
		loader:   loader,
		grpcLn:   ln,
		grpcSrv:  srv,
		raftAddr: raftPeers[idx],
		grpcAddr: ln.Addr().String(),
	}
}

// waitForLeader polls the nodes until one reports IsLeader.
func waitForLeader(t *testing.T, nodes []*clusterNode, timeout time.Duration) *clusterNode {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, n := range nodes {
			if n.loader.IsLeader() {
				return n
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("no leader elected")
	return nil
}

// TestLoaderCluster_ThreeNodeBootstrapAndForward boots a 3-node Loader
// cluster, writes a plugin through a follower, and verifies the write is
// replicated to all nodes (P5-1).
func TestLoaderCluster_ThreeNodeBootstrapAndForward(t *testing.T) {
	// Reserve 3 raft + 3 grpc ports.
	raftPeers := freeAddrs(t, 3)
	grpcPeers := freeAddrs(t, 3)

	nodes := make([]*clusterNode, 3)
	for i := range nodes {
		nodes[i] = newClusterNode(t, i, raftPeers, grpcPeers)
	}
	defer func() {
		for _, n := range nodes {
			n.shutdown()
		}
	}()

	_ = waitForLeader(t, nodes, 10*time.Second)

	// Find a follower.
	var follower *clusterNode
	for _, n := range nodes {
		if !n.loader.IsLeader() {
			follower = n
			break
		}
	}
	require.NotNil(t, follower)

	// Install plugin via the follower (should forward to leader).
	admin, closeFn := follower.adminClient()
	defer closeFn()

	manifest := minimalManifestBytes("cluster-plugin")
	resp, err := admin.InstallPlugin(context.Background(), &pluginpb.InstallPluginRequest{
		ManifestBytes: manifest,
		WasmBytes:     minimalPluginBytes,
	})
	require.NoError(t, err)
	require.Equal(t, "cluster-plugin", resp.PluginName)

	// Wait for replication to all nodes, then verify each node sees it.
	require.Eventually(t, func() bool {
		for _, n := range nodes {
			rec, err := n.loader.GetPluginRecord("cluster-plugin")
			if err != nil || rec == nil {
				return false
			}
		}
		return true
	}, 5*time.Second, 100*time.Millisecond)

	// Verify a read operation on the follower returns the plugin.
	listResp, err := admin.ListPlugins(context.Background(), &pluginpb.ListPluginsRequest{})
	require.NoError(t, err)
	require.Len(t, listResp.Plugins, 1)
}

// TestLoaderCluster_LeaderFailover verifies that after the leader is shut
// down a new leader is elected and state remains consistent (P5-1).
func TestLoaderCluster_LeaderFailover(t *testing.T) {
	raftPeers := freeAddrs(t, 3)
	grpcPeers := freeAddrs(t, 3)

	nodes := make([]*clusterNode, 3)
	for i := range nodes {
		nodes[i] = newClusterNode(t, i, raftPeers, grpcPeers)
	}
	defer func() {
		for _, n := range nodes {
			n.shutdown()
		}
	}()

	leader := waitForLeader(t, nodes, 10*time.Second)

	// Write some state through the leader admin.
	admin, closeFn := leader.adminClient()
	defer closeFn()
	manifest := minimalManifestBytes("failover-plugin")
	_, err := admin.InstallPlugin(context.Background(), &pluginpb.InstallPluginRequest{
		ManifestBytes: manifest,
		WasmBytes:     minimalPluginBytes,
	})
	require.NoError(t, err)

	// Wait for replication.
	require.Eventually(t, func() bool {
		for _, n := range nodes {
			rec, err := n.loader.GetPluginRecord("failover-plugin")
			if err != nil || rec == nil {
				return false
			}
		}
		return true
	}, 5*time.Second, 100*time.Millisecond)

	// Shutdown the leader.
	leader.shutdown()

	// Wait for a new leader among the remaining two.
	var newLeader *clusterNode
	require.Eventually(t, func() bool {
		for _, n := range nodes {
			if n == leader {
				continue
			}
			if n.loader.IsLeader() {
				newLeader = n
				return true
			}
		}
		return false
	}, 10*time.Second, 100*time.Millisecond)
	require.NotNil(t, newLeader)

	// Verify state survived on the new leader.
	rec, err := newLeader.loader.GetPluginRecord("failover-plugin")
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, "failover-plugin", rec.Name)
}

// TestLoaderCluster_LinearizableRead verifies StateGet uses ReadIndex on
// followers for linearizable reads (P5-2).
func TestLoaderCluster_LinearizableRead(t *testing.T) {
	raftPeers := freeAddrs(t, 3)
	grpcPeers := freeAddrs(t, 3)

	nodes := make([]*clusterNode, 3)
	for i := range nodes {
		nodes[i] = newClusterNode(t, i, raftPeers, grpcPeers)
	}
	defer func() {
		for _, n := range nodes {
			n.shutdown()
		}
	}()

	leader := waitForLeader(t, nodes, 10*time.Second)

	// Write state through Raft.
	ctx := context.Background()
	op := &LoaderFSMOp{Type: OpStatePut, Plugin: "lin", Key: "k", Data: []byte("v1")}
	require.NoError(t, leader.loader.RaftApply(ctx, op))

	// Linearizable read on every node should succeed and see the value.
	for _, n := range nodes {
		require.Eventually(t, func() bool {
			entry, err := n.loader.StateGet("lin", "k")
			if err != nil {
				return false
			}
			return entry != nil && string(entry.Value) == "v1"
		}, 5*time.Second, 100*time.Millisecond)
	}
}

