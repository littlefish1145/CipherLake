package bootstrap

import (
	"fmt"
	"io"

	"go.uber.org/zap"

	"cipherlake/internal/config"
	"cipherlake/internal/kms"
	"cipherlake/internal/services"
	"cipherlake/internal/services/decrypt_service"
	"cipherlake/internal/services/encrypt_service"
	"cipherlake/internal/services/keygen_service"
	"cipherlake/internal/services/keystore_service"
	"cipherlake/internal/services/keyunwrap_service"
	"cipherlake/internal/services/token_service"
)

// InitializeCryptoServices creates an EncryptionCoordinator based on config.
// In distributed mode, it uses gRPC clients to call remote services.
// In local mode, it creates service instances in-process.
func InitializeCryptoServices(cfg *config.Config) (*services.EncryptionCoordinator, error) {
	if !cfg.CryptoServices.Enabled {
		return nil, fmt.Errorf("crypto services not enabled")
	}

	if cfg.CryptoServices.DistributedMode {
		return initializeDistributed(cfg)
	}
	return initializeLocal(cfg)
}

// InitializeKMS creates the appropriate KMSClient based on encryption config.
// It wraps the client with FallbackKMS for degradation handling.
func InitializeKMS(cfg *config.Config) (kms.KMSClient, error) {
	var primary kms.KMSClient
	var err error

	kmsType := cfg.Encryption.KMSType
	if kmsType == "" {
		kmsType = "local"
	}

	switch kmsType {
	case "local", "":
		primary, err = kms.NewLocalKMS(kms.LocalConfig{
			KeyPath: cfg.CryptoServices.KeyPath + "/keygen",
		})
		if err != nil {
			return nil, fmt.Errorf("failed to initialize local KMS: %w", err)
		}
		zap.L().Info("initialized local KMS")

	case "vault":
		primary, err = kms.NewVaultTransitKMS(kms.VaultConfig{
			Address:    cfg.Encryption.VaultAddr,
			TokenFile:  cfg.Encryption.VaultTokenFile,
			TransitKey: cfg.Encryption.VaultTransitKey,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to initialize Vault KMS: %w", err)
		}
		zap.L().Info("initialized Vault Transit KMS",
			zap.String("address", cfg.Encryption.VaultAddr),
			zap.String("transit_key", cfg.Encryption.VaultTransitKey))

	case "aws":
		primary, err = kms.NewAWSKMS(kms.AWSConfig{
			KeyID:  cfg.Encryption.AWSKMSKeyID,
			Region: cfg.Encryption.AWSRegion,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to initialize AWS KMS: %w", err)
		}
		zap.L().Info("initialized AWS KMS",
			zap.String("key_id", cfg.Encryption.AWSKMSKeyID),
			zap.String("region", cfg.Encryption.AWSRegion))

	default:
		return nil, fmt.Errorf("unsupported KMS type: %s (valid: local, vault, aws)", kmsType)
	}

	// Wrap with FallbackKMS for degradation handling
	degradationMode := kms.DegradationMode(cfg.Encryption.KMSDegradationMode)
	if degradationMode == "" {
		degradationMode = kms.RejectWrites
	}

	fallback, err := kms.NewFallbackKMS(kms.FallbackConfig{
		Primary: primary,
		Mode:    degradationMode,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize fallback KMS: %w", err)
	}

	zap.L().Info("KMS initialized with fallback",
		zap.String("kms_type", kmsType),
		zap.String("degradation_mode", string(degradationMode)))

	return fallback, nil
}

// initializeLocal creates all services in-process
func initializeLocal(cfg *config.Config) (*services.EncryptionCoordinator, error) {
	keyPath := cfg.CryptoServices.KeyPath
	if keyPath == "" {
		keyPath = "./data/keys"
	}
	keyStorePath := cfg.CryptoServices.KeyStorePath
	if keyStorePath == "" {
		keyStorePath = "./data/keystore"
	}
	auditSize := cfg.CryptoServices.AuditSize
	if auditSize <= 0 {
		auditSize = 10000
	}

	tokenSvc, err := token_service.NewTokenService(token_service.TokenServiceConfig{
		KeyPath:   keyPath + "/token",
		AuditSize: auditSize,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize token service: %w", err)
	}

	keyGenSvc, err := keygen_service.NewKeyGenService(keygen_service.KeyGenServiceConfig{
		KeyPath:   keyPath + "/keygen",
		CurveName: "P-256",
		AuditSize: auditSize,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize keygen service: %w", err)
	}

	keyUnwrapSvc, err := keyunwrap_service.NewKeyUnwrapService(keyunwrap_service.KeyUnwrapServiceConfig{
		KeyPath:   keyPath + "/keygen",
		AuditSize: auditSize,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize keyunwrap service: %w", err)
	}

	encryptSvc := encrypt_service.NewEncryptService(encrypt_service.EncryptServiceConfig{
		AuditSize: auditSize,
	})

	decryptSvc := decrypt_service.NewDecryptService(decrypt_service.DecryptServiceConfig{
		AuditSize: auditSize,
	})

	keyStoreSvc, err := keystore_service.NewKeyStoreService(keystore_service.KeyStoreServiceConfig{
		DataPath:  keyStorePath,
		AuditSize: auditSize,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize keystore service: %w", err)
	}

	var opaClient *services.OPAClient
	if cfg.CryptoServices.OPAAddress != "" {
		opaClient = services.NewOPAClient(services.OPAConfig{
			Address: cfg.CryptoServices.OPAAddress,
		})
	}

	return services.NewEncryptionCoordinator(services.CoordinatorConfig{
		TokenService:     tokenSvc,
		KeyGenService:    keyGenSvc,
		KeyUnwrapService: keyUnwrapSvc,
		KeyStoreService:  keyStoreSvc,
		OPAClient:        opaClient,
		ServiceClosers:   []io.Closer{encryptSvc, decryptSvc},
	}), nil
}

// initializeDistributed creates gRPC clients for remote services
func initializeDistributed(cfg *config.Config) (*services.EncryptionCoordinator, error) {
	var opaClient *services.OPAClient
	if cfg.CryptoServices.OPAAddress != "" {
		opaClient = services.NewOPAClient(services.OPAConfig{
			Address: cfg.CryptoServices.OPAAddress,
		})
	}

	tokenAddr := cfg.CryptoServices.TokenServiceAddr
	if tokenAddr == "" {
		tokenAddr = "localhost:50051"
	}
	keygenAddr := cfg.CryptoServices.KeyGenServiceAddr
	if keygenAddr == "" {
		keygenAddr = "localhost:50052"
	}
	keyunwrapAddr := cfg.CryptoServices.KeyUnwrapServiceAddr
	if keyunwrapAddr == "" {
		keyunwrapAddr = "localhost:50053"
	}
	encryptAddr := cfg.CryptoServices.EncryptServiceAddr
	if encryptAddr == "" {
		encryptAddr = "localhost:50054"
	}
	decryptAddr := cfg.CryptoServices.DecryptServiceAddr
	if decryptAddr == "" {
		decryptAddr = "localhost:50055"
	}
	keystoreAddr := cfg.CryptoServices.KeyStoreServiceAddr
	if keystoreAddr == "" {
		keystoreAddr = "localhost:50056"
	}

	return services.NewDistributedCoordinator(
		tokenAddr,
		keygenAddr,
		keyunwrapAddr,
		encryptAddr,
		decryptAddr,
		keystoreAddr,
		opaClient,
		nil,
	)
}
