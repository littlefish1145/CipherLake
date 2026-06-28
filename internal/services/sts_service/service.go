package sts_service

import (
	"context"
	"fmt"
	"time"

	"nexus/internal/iam"

	"go.uber.org/zap"
)

// STSService implements the Security Token Service
type STSService struct {
	iamService *iam.IAMService
	stopCh     chan struct{}
}

// NewSTSService creates a new STS service
func NewSTSService(iamService *iam.IAMService) *STSService {
	return &STSService{
		iamService: iamService,
		stopCh:     make(chan struct{}),
	}
}

// AssumeRole creates temporary credentials for role assumption
func (s *STSService) AssumeRole(ctx context.Context, roleARN, sessionName string, durationSeconds int, externalID, policyJSON, callerARN string) (*iam.TemporaryCredential, error) {
	req := &iam.AssumeRoleRequest{
		RoleARN:         roleARN,
		RoleSessionName: sessionName,
		DurationSeconds: durationSeconds,
		ExternalID:      externalID,
		Policy:          policyJSON,
	}

	cred, err := s.iamService.AssumeRole(req, callerARN)
	if err != nil {
		return nil, fmt.Errorf("AssumeRole failed: %w", err)
	}

	zap.L().Info("AssumeRole succeeded",
		zap.String("role", roleARN),
		zap.String("caller", callerARN),
		zap.String("session", sessionName),
		zap.Time("expires", cred.Expiration))

	return cred, nil
}

// GetSessionToken creates temporary credentials for the current user
func (s *STSService) GetSessionToken(ctx context.Context, callerARN string, durationSeconds int) (*iam.TemporaryCredential, error) {
	cred, err := s.iamService.GetSessionToken(callerARN, durationSeconds)
	if err != nil {
		return nil, fmt.Errorf("GetSessionToken failed: %w", err)
	}

	return cred, nil
}

// GetFederationToken creates temporary credentials for a federated user
func (s *STSService) GetFederationToken(ctx context.Context, name, callerARN string, durationSeconds int, policyJSON string) (*iam.TemporaryCredential, error) {
	var policy *iam.PolicyDocument
	if policyJSON != "" {
		parsed, err := iam.ParsePolicyDocumentFromString(policyJSON)
		if err != nil {
			return nil, fmt.Errorf("invalid policy JSON: %w", err)
		}
		policy = parsed
	}

	cred, err := s.iamService.GetFederationToken(name, callerARN, durationSeconds, policy)
	if err != nil {
		return nil, fmt.Errorf("GetFederationToken failed: %w", err)
	}

	return cred, nil
}

// CleanupExpired runs periodic cleanup of expired temp credentials
func (s *STSService) CleanupExpired() {
	count, err := s.iamService.GetStore().CleanupExpiredTempCreds()
	if err != nil {
		zap.L().Error("failed to cleanup expired temp credentials", zap.Error(err))
		return
	}
	if count > 0 {
		zap.L().Info("cleaned up expired temp credentials", zap.Int("count", count))
	}
}

// StartCleanupLoop starts a background goroutine to clean up expired credentials
func (s *STSService) StartCleanupLoop(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-s.stopCh:
				return
			case <-ticker.C:
				s.CleanupExpired()
			}
		}
	}()
}

// StopCleanupLoop signals the cleanup goroutine to exit
func (s *STSService) StopCleanupLoop() {
	close(s.stopCh)
}
