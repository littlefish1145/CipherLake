package services

import (
	"context"
	"crypto/ecdh"
	"crypto/tls"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	pbcommon "nexus/proto/common"
	pbdecrypt "nexus/proto/decrypt"
	pbencrypt "nexus/proto/encrypt"
	pbkeygen "nexus/proto/keygen"
	pbkeystore "nexus/proto/keystore"
	pbkeyunwrap "nexus/proto/keyunwrap"
	pbtoken "nexus/proto/token"

	"nexus/internal/services/token_service"
)

// ---------------------------------------------------------------------------
// Helper conversion functions (internal types <-> proto types)
// ---------------------------------------------------------------------------

func ecdhPubToProto(k *ECDHPublicKey) *pbcommon.ECDHPublicKey {
	if k == nil {
		return nil
	}
	return &pbcommon.ECDHPublicKey{
		PublicKey: k.PublicKey,
		Curve:     k.Curve,
	}
}

func protoToECDHPub(k *pbcommon.ECDHPublicKey) *ECDHPublicKey {
	if k == nil {
		return nil
	}
	return &ECDHPublicKey{
		PublicKey: k.GetPublicKey(),
		Curve:     k.GetCurve(),
	}
}

func encryptedDEKToProto(k *EncryptedDEK) *pbcommon.EncryptedDEK {
	if k == nil {
		return nil
	}
	return &pbcommon.EncryptedDEK{
		EncryptedKey: k.EncryptedKey,
		Algorithm:    k.Algorithm,
		KeyId:        k.KeyID,
		KeyVersion:   int32(k.KeyVersion),
	}
}

func protoToEncryptedDEK(k *pbcommon.EncryptedDEK) *EncryptedDEK {
	if k == nil {
		return nil
	}
	return &EncryptedDEK{
		EncryptedKey: k.GetEncryptedKey(),
		Algorithm:    k.GetAlgorithm(),
		KeyID:        k.GetKeyId(),
		KeyVersion:   int(k.GetKeyVersion()),
	}
}

func ecdhEncryptedDEKToProto(k *ECDHEncryptedDEK) *pbcommon.ECDHEncryptedDEK {
	if k == nil {
		return nil
	}
	return &pbcommon.ECDHEncryptedDEK{
		Ciphertext:         k.Ciphertext,
		Nonce:              k.Nonce,
		EphemeralPublicKey: k.EphemeralPublicKey,
	}
}

func protoToECDHEncryptedDEK(k *pbcommon.ECDHEncryptedDEK) *ECDHEncryptedDEK {
	if k == nil {
		return nil
	}
	return &ECDHEncryptedDEK{
		Ciphertext:         k.GetCiphertext(),
		Nonce:              k.GetNonce(),
		EphemeralPublicKey: k.GetEphemeralPublicKey(),
	}
}

// protoError checks if a proto response contains an error field and returns it.
func protoError(err *pbcommon.Error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("rpc error [%d]: %s: %s", err.GetCode(), err.GetMessage(), err.GetDetails())
}

// ---------------------------------------------------------------------------
// dialHelper creates a gRPC client connection with optional TLS.
// ---------------------------------------------------------------------------

func dialHelper(addr string, tlsCfg *tls.Config) (*grpc.ClientConn, error) {
	opts := []grpc.DialOption{grpc.WithDefaultCallOptions()}
	if tlsCfg != nil {
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})))
	}
	return grpc.NewClient(addr, opts...)
}

// ===========================================================================
// Generic gRPC client wrapper
// ===========================================================================

// GRPCClient holds a gRPC connection and the typed service client generated
// from a protobuf definition. It centralizes connection lifecycle logic so
// service-specific wrappers only need to define their business methods.
type GRPCClient[T any] struct {
	conn   *grpc.ClientConn
	Client T
}

// NewGRPCClient dials addr using the shared TLS/insecure helper, constructs
// the typed client via newClient, and returns a reusable GRPCClient wrapper.
func NewGRPCClient[T any](addr string, tlsCfg *tls.Config, serviceName string, newClient func(grpc.ClientConnInterface) T) (*GRPCClient[T], error) {
	conn, err := dialHelper(addr, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to connect %s service: %w", serviceName, err)
	}
	return &GRPCClient[T]{
		conn:   conn,
		Client: newClient(conn),
	}, nil
}

// Close closes the underlying gRPC connection. It is safe to call on a nil
// receiver or on a wrapper whose connection was never established.
func (c *GRPCClient[T]) Close() error {
	if c != nil && c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// ===========================================================================
// GRPCKeyGenerator – implements KeyGenerator
// ===========================================================================

type GRPCKeyGenerator struct {
	*GRPCClient[pbkeygen.KeyGenServiceClient]
}

func NewGRPCKeyGenerator(addr string, tlsCfg *tls.Config) (*GRPCKeyGenerator, error) {
	base, err := NewGRPCClient(addr, tlsCfg, "keygen", pbkeygen.NewKeyGenServiceClient)
	if err != nil {
		return nil, err
	}
	return &GRPCKeyGenerator{base}, nil
}

func (g *GRPCKeyGenerator) GenerateDataKey(ctx context.Context, tokenID, userID, bucket, objectKey string, clientECDHPub *ECDHPublicKey) (*EncryptedDEK, *ECDHEncryptedDEK, *ECDHPublicKey, error) {
	resp, err := g.Client.GenerateDataKey(ctx, &pbkeygen.GenerateDataKeyRequest{
		Token: &pbcommon.DelegationToken{
			TokenId:   tokenID,
			TokenType: pbcommon.TokenType_TOKEN_TYPE_WRITE,
			UserId:    userID,
			Bucket:    bucket,
			ObjectKey: objectKey,
		},
		ClientEcdhPub: ecdhPubToProto(clientECDHPub),
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("grpc GenerateDataKey: %w", err)
	}
	if rerr := protoError(resp.GetError()); rerr != nil {
		return nil, nil, nil, rerr
	}
	return protoToEncryptedDEK(resp.GetEncryptedDek()),
		protoToECDHEncryptedDEK(resp.GetEcdhEncryptedDek()),
		protoToECDHPub(resp.GetServiceEcdhPub()),
		nil
}

func (g *GRPCKeyGenerator) GetPublicKey() ([]byte, string, string) {
	resp, err := g.Client.GetPublicKey(context.Background(), &pbkeygen.GetPublicKeyRequest{})
	if err != nil {
		return nil, "", ""
	}
	return resp.GetPublicKey(), resp.GetKeyId(), resp.GetAlgorithm()
}

// ===========================================================================
// GRPCKeyUnwrapper – implements KeyUnwrapper
// ===========================================================================

type GRPCKeyUnwrapper struct {
	*GRPCClient[pbkeyunwrap.KeyUnwrapServiceClient]
}

func NewGRPCKeyUnwrapper(addr string, tlsCfg *tls.Config) (*GRPCKeyUnwrapper, error) {
	base, err := NewGRPCClient(addr, tlsCfg, "keyunwrap", pbkeyunwrap.NewKeyUnwrapServiceClient)
	if err != nil {
		return nil, err
	}
	return &GRPCKeyUnwrapper{base}, nil
}

func (u *GRPCKeyUnwrapper) UnwrapKey(ctx context.Context, tokenID, userID, bucket, objectKey string, encryptedDEK *EncryptedDEK, clientECDHPub *ECDHPublicKey) (*ECDHEncryptedDEK, *ECDHPublicKey, error) {
	resp, err := u.Client.UnwrapKey(ctx, &pbkeyunwrap.UnwrapKeyRequest{
		Token: &pbcommon.DelegationToken{
			TokenId:   tokenID,
			TokenType: pbcommon.TokenType_TOKEN_TYPE_READ,
			UserId:    userID,
			Bucket:    bucket,
			ObjectKey: objectKey,
		},
		EncryptedDek:  encryptedDEKToProto(encryptedDEK),
		ClientEcdhPub: ecdhPubToProto(clientECDHPub),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("grpc UnwrapKey: %w", err)
	}
	if rerr := protoError(resp.GetError()); rerr != nil {
		return nil, nil, rerr
	}
	return protoToECDHEncryptedDEK(resp.GetEcdhEncryptedDek()),
		protoToECDHPub(resp.GetServiceEcdhPub()),
		nil
}

// ===========================================================================
// GRPCDataEncryptor – implements DataEncryptor
// ===========================================================================

type GRPCDataEncryptor struct {
	*GRPCClient[pbencrypt.EncryptServiceClient]
}

func NewGRPCDataEncryptor(addr string, tlsCfg *tls.Config) (*GRPCDataEncryptor, error) {
	base, err := NewGRPCClient(addr, tlsCfg, "encrypt", pbencrypt.NewEncryptServiceClient)
	if err != nil {
		return nil, err
	}
	return &GRPCDataEncryptor{base}, nil
}

func (e *GRPCDataEncryptor) Encrypt(clientECDHPriv *ecdh.PrivateKey, serviceECDHPub *ECDHPublicKey, ecdhEncryptedDEK *ECDHEncryptedDEK, plaintext []byte, algorithm string) ([]byte, []byte, []byte, error) {
	resp, err := e.Client.EncryptChunk(context.Background(), &pbencrypt.EncryptChunkRequest{
		EcdhEncryptedDek:     ecdhEncryptedDEKToProto(ecdhEncryptedDEK),
		ClientEcdhPrivateKey: clientECDHPriv.Bytes(),
		ServiceEcdhPub:       ecdhPubToProto(serviceECDHPub),
		Plaintext:            plaintext,
		Algorithm:            algorithm,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("grpc EncryptChunk: %w", err)
	}
	if rerr := protoError(resp.GetError()); rerr != nil {
		return nil, nil, nil, rerr
	}
	return resp.GetCiphertext(), resp.GetNonce(), resp.GetAuthTag(), nil
}

// ===========================================================================
// GRPCDataDecryptor – implements DataDecryptor
// ===========================================================================

type GRPCDataDecryptor struct {
	*GRPCClient[pbdecrypt.DecryptServiceClient]
}

func NewGRPCDataDecryptor(addr string, tlsCfg *tls.Config) (*GRPCDataDecryptor, error) {
	base, err := NewGRPCClient(addr, tlsCfg, "decrypt", pbdecrypt.NewDecryptServiceClient)
	if err != nil {
		return nil, err
	}
	return &GRPCDataDecryptor{base}, nil
}

func (d *GRPCDataDecryptor) Decrypt(clientECDHPriv *ecdh.PrivateKey, serviceECDHPub *ECDHPublicKey, ecdhEncryptedDEK *ECDHEncryptedDEK, ciphertext []byte, nonce []byte, authTag []byte, algorithm string) ([]byte, error) {
	resp, err := d.Client.DecryptChunk(context.Background(), &pbdecrypt.DecryptChunkRequest{
		EcdhEncryptedDek:     ecdhEncryptedDEKToProto(ecdhEncryptedDEK),
		ClientEcdhPrivateKey: clientECDHPriv.Bytes(),
		ServiceEcdhPub:       ecdhPubToProto(serviceECDHPub),
		Ciphertext:           ciphertext,
		Nonce:                nonce,
		AuthTag:              authTag,
		Algorithm:            algorithm,
	})
	if err != nil {
		return nil, fmt.Errorf("grpc DecryptChunk: %w", err)
	}
	if rerr := protoError(resp.GetError()); rerr != nil {
		return nil, rerr
	}
	return resp.GetPlaintext(), nil
}

// ===========================================================================
// GRPCKeyStorer – implements KeyStorer
// ===========================================================================

type GRPCKeyStorer struct {
	*GRPCClient[pbkeystore.KeyStoreServiceClient]
}

func NewGRPCKeyStorer(addr string, tlsCfg *tls.Config) (*GRPCKeyStorer, error) {
	base, err := NewGRPCClient(addr, tlsCfg, "keystore", pbkeystore.NewKeyStoreServiceClient)
	if err != nil {
		return nil, err
	}
	return &GRPCKeyStorer{base}, nil
}

func (s *GRPCKeyStorer) StoreKey(bucket, objectKey string, encryptedDEK *EncryptedDEK, objectSize int64) (string, error) {
	resp, err := s.Client.StoreKey(context.Background(), &pbkeystore.StoreKeyRequest{
		Bucket:       bucket,
		ObjectKey:    objectKey,
		EncryptedDek: encryptedDEKToProto(encryptedDEK),
		ObjectSize:   objectSize,
	})
	if err != nil {
		return "", fmt.Errorf("grpc StoreKey: %w", err)
	}
	if rerr := protoError(resp.GetError()); rerr != nil {
		return "", rerr
	}
	return resp.GetKeyId(), nil
}

func (s *GRPCKeyStorer) GetKey(bucket, objectKey string) (*EncryptedDEK, error) {
	resp, err := s.Client.GetKey(context.Background(), &pbkeystore.GetKeyRequest{
		Bucket:    bucket,
		ObjectKey: objectKey,
	})
	if err != nil {
		return nil, fmt.Errorf("grpc GetKey: %w", err)
	}
	if rerr := protoError(resp.GetError()); rerr != nil {
		return nil, rerr
	}
	return protoToEncryptedDEK(resp.GetEncryptedDek()), nil
}

func (s *GRPCKeyStorer) DeleteKey(bucket, objectKey string) error {
	resp, err := s.Client.DeleteKey(context.Background(), &pbkeystore.DeleteKeyRequest{
		Bucket:    bucket,
		ObjectKey: objectKey,
	})
	if err != nil {
		return fmt.Errorf("grpc DeleteKey: %w", err)
	}
	if rerr := protoError(resp.GetError()); rerr != nil {
		return rerr
	}
	return nil
}

// ===========================================================================
// GRPCTokenService – implements TokenIssuer for the coordinator
// ===========================================================================

// GRPCTokenService wraps the remote TokenService via gRPC so that the
// EncryptionCoordinator can issue tokens in distributed mode.
type GRPCTokenService struct {
	*GRPCClient[pbtoken.TokenServiceClient]
}

func NewGRPCTokenService(addr string, tlsCfg *tls.Config) (*GRPCTokenService, error) {
	base, err := NewGRPCClient(addr, tlsCfg, "token", pbtoken.NewTokenServiceClient)
	if err != nil {
		return nil, err
	}
	return &GRPCTokenService{base}, nil
}

func (t *GRPCTokenService) IssueWriteToken(ctx context.Context, userID, bucket, objectKey string, ttlSeconds int64) (*token_service.DelegationToken, error) {
	resp, err := t.Client.IssueWriteToken(ctx, &pbtoken.IssueWriteTokenRequest{
		UserId:     userID,
		Bucket:     bucket,
		ObjectKey:  objectKey,
		TtlSeconds: ttlSeconds,
	})
	if err != nil {
		return nil, fmt.Errorf("grpc IssueWriteToken: %w", err)
	}
	if rerr := protoError(resp.GetError()); rerr != nil {
		return nil, rerr
	}
	return protoToDelegationToken(resp.GetToken()), nil
}

func (t *GRPCTokenService) IssueReadToken(ctx context.Context, userID, bucket, objectKey, contentHash string, ttlSeconds int64) (*token_service.DelegationToken, error) {
	resp, err := t.Client.IssueReadToken(ctx, &pbtoken.IssueReadTokenRequest{
		UserId:      userID,
		Bucket:      bucket,
		ObjectKey:   objectKey,
		ContentHash: contentHash,
		TtlSeconds:  ttlSeconds,
	})
	if err != nil {
		return nil, fmt.Errorf("grpc IssueReadToken: %w", err)
	}
	if rerr := protoError(resp.GetError()); rerr != nil {
		return nil, rerr
	}
	return protoToDelegationToken(resp.GetToken()), nil
}

func (t *GRPCTokenService) IssueDeleteToken(ctx context.Context, userID, bucket, objectKey string, ttlSeconds int64) (*token_service.DelegationToken, error) {
	resp, err := t.Client.IssueDeleteToken(ctx, &pbtoken.IssueDeleteTokenRequest{
		UserId:     userID,
		Bucket:     bucket,
		ObjectKey:  objectKey,
		TtlSeconds: ttlSeconds,
	})
	if err != nil {
		return nil, fmt.Errorf("grpc IssueDeleteToken: %w", err)
	}
	if rerr := protoError(resp.GetError()); rerr != nil {
		return nil, rerr
	}
	return protoToDelegationToken(resp.GetToken()), nil
}

func protoToDelegationToken(tok *pbcommon.DelegationToken) *token_service.DelegationToken {
	if tok == nil {
		return nil
	}
	tt := token_service.TokenTypeWrite
	switch tok.GetTokenType() {
	case pbcommon.TokenType_TOKEN_TYPE_READ:
		tt = token_service.TokenTypeRead
	case pbcommon.TokenType_TOKEN_TYPE_DELETE:
		tt = token_service.TokenTypeDelete
	}
	return &token_service.DelegationToken{
		TokenID:     tok.GetTokenId(),
		TokenType:   tt,
		UserID:      tok.GetUserId(),
		Bucket:      tok.GetBucket(),
		ObjectKey:   tok.GetObjectKey(),
		Expiry:      time.Unix(tok.GetExpiry(), 0),
		CreatedAt:   time.Unix(tok.GetCreatedAt(), 0),
		Operations:  tok.GetOperations(),
		ContentHash: tok.GetContentHash(),
		Signature:   tok.GetSignature(),
	}
}

// Compile-time interface checks.
var (
	_ KeyGenerator  = (*GRPCKeyGenerator)(nil)
	_ KeyUnwrapper  = (*GRPCKeyUnwrapper)(nil)
	_ DataEncryptor = (*GRPCDataEncryptor)(nil)
	_ DataDecryptor = (*GRPCDataDecryptor)(nil)
	_ KeyStorer     = (*GRPCKeyStorer)(nil)
	_ TokenIssuer   = (*GRPCTokenService)(nil)
)

// ===========================================================================
// NewDistributedCoordinator – creates EncryptionCoordinator with gRPC clients
// ===========================================================================

func NewDistributedCoordinator(tokenAddr, keygenAddr, keyunwrapAddr, encryptAddr, decryptAddr, keystoreAddr string, opaClient *OPAClient, tlsCfg *tls.Config) (*EncryptionCoordinator, error) {
	tokenSvc, err := NewGRPCTokenService(tokenAddr, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("token service: %w", err)
	}

	keyGenSvc, err := NewGRPCKeyGenerator(keygenAddr, tlsCfg)
	if err != nil {
		tokenSvc.Close()
		return nil, fmt.Errorf("keygen service: %w", err)
	}

	keyUnwrapSvc, err := NewGRPCKeyUnwrapper(keyunwrapAddr, tlsCfg)
	if err != nil {
		tokenSvc.Close()
		keyGenSvc.Close()
		return nil, fmt.Errorf("keyunwrap service: %w", err)
	}

	encryptSvc, err := NewGRPCDataEncryptor(encryptAddr, tlsCfg)
	if err != nil {
		tokenSvc.Close()
		keyGenSvc.Close()
		keyUnwrapSvc.Close()
		return nil, fmt.Errorf("encrypt service: %w", err)
	}

	decryptSvc, err := NewGRPCDataDecryptor(decryptAddr, tlsCfg)
	if err != nil {
		tokenSvc.Close()
		keyGenSvc.Close()
		keyUnwrapSvc.Close()
		encryptSvc.Close()
		return nil, fmt.Errorf("decrypt service: %w", err)
	}

	keyStoreSvc, err := NewGRPCKeyStorer(keystoreAddr, tlsCfg)
	if err != nil {
		tokenSvc.Close()
		keyGenSvc.Close()
		keyUnwrapSvc.Close()
		encryptSvc.Close()
		decryptSvc.Close()
		return nil, fmt.Errorf("keystore service: %w", err)
	}

	return NewEncryptionCoordinator(CoordinatorConfig{
		TokenService:     tokenSvc,
		KeyGenService:    keyGenSvc,
		KeyUnwrapService: keyUnwrapSvc,
		EncryptService:   encryptSvc,
		DecryptService:   decryptSvc,
		KeyStoreService:  keyStoreSvc,
		OPAClient:        opaClient,
	}), nil
}
