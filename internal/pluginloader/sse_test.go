package pluginloader

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSSERegistry_CreateAndOwner(t *testing.T) {
	r := newSSERegistry()
	id := r.CreateSSESession("demo")
	require.NotZero(t, id)
	require.Equal(t, "demo", r.Owner(id))
	require.Equal(t, "", r.Owner(id+1))
}

func TestSSERegistry_Heartbeat(t *testing.T) {
	r := newSSERegistry()
	id := r.CreateSSESession("demo")
	require.True(t, r.Heartbeat(id))
	require.False(t, r.Heartbeat(9999))
}

func TestSSERegistry_Stale(t *testing.T) {
	r := newSSERegistry()
	id := r.CreateSSESession("demo")
	r.sessions[id].lastHeartbeat = time.Now().Add(-2 * time.Minute)
	stale := r.Stale(30 * time.Second)
	require.Contains(t, stale, id)
}

func TestSSERegistry_CloseAll(t *testing.T) {
	r := newSSERegistry()
	id1 := r.CreateSSESession("demo")
	id2 := r.CreateSSESession("demo")
	id3 := r.CreateSSESession("other")

	closed := r.CloseAll("demo")
	require.Len(t, closed, 2)
	require.Contains(t, closed, id1)
	require.Contains(t, closed, id2)
	require.Equal(t, "", r.Owner(id1))
	require.Equal(t, "", r.Owner(id2))
	require.Equal(t, "other", r.Owner(id3))
}

func TestLoader_SSESessionLifecycle(t *testing.T) {
	l := newTestAdminLoader(t)

	id := l.CreateSSESession("demo")
	require.NotZero(t, id)
	require.Equal(t, "demo", l.GetSSESessionOwner(id))

	require.True(t, l.HeartbeatSSESession(id))
	require.False(t, l.HeartbeatSSESession(9999))

	err := l.CloseSSESession(context.Background(), id, SSECloseReasonClientDisconnect)
	require.NoError(t, err)
	require.Equal(t, "", l.GetSSESessionOwner(id))

	err = l.CloseSSESession(context.Background(), id, SSECloseReasonClientDisconnect)
	require.Error(t, err)
}
