package gateway

import (
	"time"

	"cipherlake/internal/auth"
	"cipherlake/internal/config"
)

type gatewayAuthRuntime struct {
	handler     *AuthHandler
	provider    auth.Provider
	permissions auth.PermissionChecker
}

func newGatewayAuthRuntime(cfg *config.Config, iamBridge *IAMAuthBridge) *gatewayAuthRuntime {
	handler := NewAuthHandlerWithConfig(gatewayAuthConfig(cfg))
	provider := newGatewayAuthProvider(handler, iamBridge)
	return &gatewayAuthRuntime{
		handler:     handler,
		provider:    provider,
		permissions: provider,
	}
}

func gatewayAuthConfig(cfg *config.Config) *AuthConfig {
	authConfig := &AuthConfig{
		RequireAuth:   cfg.Auth.RequireAuth,
		AnonymousRead: cfg.Auth.AnonymousRead,
		JWTSecret:     cfg.Auth.JWTSecret,
	}
	if cfg.Auth.TokenExpiry != "" {
		if d, err := time.ParseDuration(cfg.Auth.TokenExpiry); err == nil {
			authConfig.TokenExpiry = d
		}
	}
	if cfg.Auth.RefreshExpiry != "" {
		if d, err := time.ParseDuration(cfg.Auth.RefreshExpiry); err == nil {
			authConfig.RefreshExpiry = d
		}
	}
	return authConfig
}
