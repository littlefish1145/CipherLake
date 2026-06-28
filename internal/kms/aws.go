package kms

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/smithy-go"
	"go.uber.org/zap"
)

// AWSKMS implements KMSClient using AWS Key Management Service.
type AWSKMS struct {
	client     *kms.Client
	keyID      string
	maxRetries int
}

// AWSConfig holds configuration for the AWS KMS.
type AWSConfig struct {
	// KeyID is the AWS KMS key ID or ARN.
	KeyID string
	// Region is the AWS region.
	Region string
	// MaxRetries is the maximum number of retry attempts for transient errors.
	MaxRetries int
}

// NewAWSKMS creates a new AWS KMS client.
// It uses the default AWS credential chain (env vars, shared credentials, IAM role).
func NewAWSKMS(cfg AWSConfig) (*AWSKMS, error) {
	if cfg.KeyID == "" {
		return nil, fmt.Errorf("kms/aws: key_id is required")
	}

	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
	)
	if err != nil {
		return nil, fmt.Errorf("kms/aws: failed to load AWS config: %w", err)
	}

	client := kms.NewFromConfig(awsCfg)

	maxRetries := cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}

	return &AWSKMS{
		client:     client,
		keyID:      cfg.KeyID,
		maxRetries: maxRetries,
	}, nil
}

// GenerateDataKey generates a new data encryption key using AWS KMS GenerateDataKey API.
func (a *AWSKMS) GenerateDataKey(ctx context.Context, keyID string, length int) (plaintext, encrypted []byte, err error) {
	if length != 32 && length != 16 {
		return nil, nil, fmt.Errorf("kms/aws: unsupported key length %d (must be 16 or 32)", length)
	}

	kmsKeyID := a.resolveKeyID(keyID)
	spec := types.DataKeySpecAes256
	if length == 16 {
		spec = types.DataKeySpecAes128
	}

	var result *kms.GenerateDataKeyOutput
	for attempt := 0; attempt <= a.maxRetries; attempt++ {
		result, err = a.client.GenerateDataKey(ctx, &kms.GenerateDataKeyInput{
			KeyId:   aws.String(kmsKeyID),
			KeySpec: spec,
		})
		if err == nil {
			break
		}
		if !isAWSThrottlingError(err) {
			return nil, nil, fmt.Errorf("kms/aws: GenerateDataKey failed: %w", err)
		}
		if attempt < a.maxRetries {
			backoff := time.Duration(math.Pow(2, float64(attempt))) * 200 * time.Millisecond
			zap.L().Warn("kms/aws: throttling error, retrying",
				zap.Int("attempt", attempt+1),
				zap.Duration("backoff", backoff),
				zap.Error(err))
			time.Sleep(backoff)
		} else {
			return nil, nil, fmt.Errorf("kms/aws: GenerateDataKey max retries exceeded: %w", err)
		}
	}

	if result == nil {
		return nil, nil, fmt.Errorf("kms/aws: GenerateDataKey returned nil response")
	}

	plaintext = result.Plaintext
	encrypted = result.CiphertextBlob

	return plaintext, encrypted, nil
}

// DecryptDataKey decrypts an encrypted data key using AWS KMS Decrypt API.
func (a *AWSKMS) DecryptDataKey(ctx context.Context, keyID string, encrypted []byte) (plaintext []byte, err error) {
	kmsKeyID := a.resolveKeyID(keyID)

	var result *kms.DecryptOutput
	for attempt := 0; attempt <= a.maxRetries; attempt++ {
		result, err = a.client.Decrypt(ctx, &kms.DecryptInput{
			KeyId:          aws.String(kmsKeyID),
			CiphertextBlob: encrypted,
		})
		if err == nil {
			break
		}
		if !isAWSThrottlingError(err) {
			return nil, fmt.Errorf("kms/aws: DecryptDataKey failed: %w", err)
		}
		if attempt < a.maxRetries {
			backoff := time.Duration(math.Pow(2, float64(attempt))) * 200 * time.Millisecond
			zap.L().Warn("kms/aws: throttling error, retrying",
				zap.Int("attempt", attempt+1),
				zap.Duration("backoff", backoff),
				zap.Error(err))
			time.Sleep(backoff)
		} else {
			return nil, fmt.Errorf("kms/aws: DecryptDataKey max retries exceeded: %w", err)
		}
	}

	if result == nil {
		return nil, fmt.Errorf("kms/aws: DecryptDataKey returned nil response")
	}

	plaintext = result.Plaintext
	return plaintext, nil
}

// GetPublicKey retrieves the public key using AWS KMS GetPublicKey API.
func (a *AWSKMS) GetPublicKey(ctx context.Context, keyID string) (pub []byte, err error) {
	kmsKeyID := a.resolveKeyID(keyID)

	var result *kms.GetPublicKeyOutput
	for attempt := 0; attempt <= a.maxRetries; attempt++ {
		result, err = a.client.GetPublicKey(ctx, &kms.GetPublicKeyInput{
			KeyId: aws.String(kmsKeyID),
		})
		if err == nil {
			break
		}
		if !isAWSThrottlingError(err) {
			return nil, fmt.Errorf("kms/aws: GetPublicKey failed: %w", err)
		}
		if attempt < a.maxRetries {
			backoff := time.Duration(math.Pow(2, float64(attempt))) * 200 * time.Millisecond
			zap.L().Warn("kms/aws: throttling error, retrying",
				zap.Int("attempt", attempt+1),
				zap.Duration("backoff", backoff),
				zap.Error(err))
			time.Sleep(backoff)
		} else {
			return nil, fmt.Errorf("kms/aws: GetPublicKey max retries exceeded: %w", err)
		}
	}

	if result == nil {
		return nil, fmt.Errorf("kms/aws: GetPublicKey returned nil response")
	}

	pub = result.PublicKey
	return pub, nil
}

// Close cleans up AWS KMS client resources.
func (a *AWSKMS) Close() error {
	return nil
}

// resolveKeyID returns the effective key ID to use.
func (a *AWSKMS) resolveKeyID(keyID string) string {
	if keyID != "" {
		return keyID
	}
	return a.keyID
}

// isAWSThrottlingError checks if the error is a KMS ThrottlingException.
func isAWSThrottlingError(err error) bool {
	if err == nil {
		return false
	}
	// Check using smithy.APIError interface for the error code
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		if code == "ThrottlingException" || code == "RequestLimitExceeded" {
			return true
		}
	}
	// Fallback: check error message string
	errStr := err.Error()
	return contains(errStr, "ThrottlingException") || contains(errStr, "throttling") || contains(errStr, "rate limit")
}
