package gateway

import (
	"cipherlake/internal/config"
	"cipherlake/internal/ratelimit"
)

func newGatewayRateLimiter(cfg *config.Config) *ratelimit.MultiLevelLimiter {
	if !cfg.RateLimit.Enabled {
		return nil
	}

	return ratelimit.NewMultiLevelLimiter(gatewayRateLimitConfig(cfg))
}

func gatewayRateLimitConfig(cfg *config.Config) *ratelimit.MultiLevelConfig {
	apiLimits := make(map[string]ratelimit.APILimit)
	for name, limit := range cfg.RateLimit.APILimits {
		apiLimits[name] = ratelimit.APILimit{
			RPS:   limit.RPS,
			Burst: limit.Burst,
		}
	}

	return &ratelimit.MultiLevelConfig{
		GlobalRPS:         cfg.RateLimit.GlobalRPS,
		GlobalBurst:       cfg.RateLimit.GlobalBurst,
		IPRPS:             cfg.RateLimit.IPRPS,
		IPBurst:           cfg.RateLimit.IPBurst,
		UserRPS:           cfg.RateLimit.UserRPS,
		UserBurst:         cfg.RateLimit.UserBurst,
		BucketRPS:         cfg.RateLimit.BucketRPS,
		BucketBurst:       cfg.RateLimit.BucketBurst,
		UploadBytesPerSec: cfg.RateLimit.UploadBytesPerSec,
		UploadBurstBytes:  cfg.RateLimit.UploadBurstBytes,
		APILimits:         apiLimits,
		Whitelist:         cfg.RateLimit.Whitelist,
	}
}
