package services

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	pbkeygen "cipherlake/proto/keygen"
)

type stubKeyGenServer struct {
	pbkeygen.UnimplementedKeyGenServiceServer
}

func (s *stubKeyGenServer) GenerateDataKey(ctx context.Context, req *pbkeygen.GenerateDataKeyRequest) (*pbkeygen.GenerateDataKeyResponse, error) {
	return &pbkeygen.GenerateDataKeyResponse{}, nil
}

func (s *stubKeyGenServer) GetPublicKey(ctx context.Context, req *pbkeygen.GetPublicKeyRequest) (*pbkeygen.GetPublicKeyResponse, error) {
	return &pbkeygen.GetPublicKeyResponse{}, nil
}

func TestGRPCClient_Close_Nil(t *testing.T) {
	client := &GRPCClient[pbkeygen.KeyGenServiceClient]{}
	require.NotPanics(t, func() {
		require.NoError(t, client.Close())
	})
}

func TestGRPCClient_Close_WithConn(t *testing.T) {
	lis := bufconn.Listen(1024)
	srv := grpc.NewServer()
	pbkeygen.RegisterKeyGenServiceServer(srv, &stubKeyGenServer{})

	go func() {
		if err := srv.Serve(lis); err != nil {
			t.Logf("bufconn server exited: %v", err)
		}
	}()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)

	client := &GRPCClient[pbkeygen.KeyGenServiceClient]{
		conn:   conn,
		Client: pbkeygen.NewKeyGenServiceClient(conn),
	}

	require.NoError(t, client.Close())
}

type recordingCloser struct {
	closed *int
	err    error
}

func (c recordingCloser) Close() error {
	*c.closed++
	return c.err
}

func TestCloseDistributedCoordinatorClientsClosesAllAndJoinsErrors(t *testing.T) {
	firstErr := errors.New("first close failed")
	secondErr := errors.New("second close failed")
	closed := 0

	err := closeDistributedCoordinatorClients(
		recordingCloser{closed: &closed, err: firstErr},
		nil,
		recordingCloser{closed: &closed, err: secondErr},
	)

	require.Equal(t, 2, closed)
	require.ErrorIs(t, err, firstErr)
	require.ErrorIs(t, err, secondErr)
}

func TestDistributedCoordinatorSetupErrorIncludesSetupAndCleanupErrors(t *testing.T) {
	setupErr := errors.New("dial failed")
	closeErr := errors.New("close failed")
	closed := 0

	err := distributedCoordinatorSetupError("keygen", setupErr, recordingCloser{closed: &closed, err: closeErr})

	require.Equal(t, 1, closed)
	require.ErrorIs(t, err, setupErr)
	require.ErrorIs(t, err, closeErr)
	require.Contains(t, err.Error(), "keygen service")
}
