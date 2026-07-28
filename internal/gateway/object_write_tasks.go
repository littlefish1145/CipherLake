package gateway

import (
	"context"

	"cipherlake/internal/taskqueue"
)

type objectWriteTaskScheduler interface {
	ScheduleObjectWriteTasks(ctx context.Context, req objectWriteTaskRequest)
}

type objectWriteTaskRequest struct {
	Bucket        string
	Key           string
	ContentType   string
	Metadata      map[string]string
	UserID        string
	VersionID     string
	SkipVectorize bool
	SkipFTS       bool
}

type gatewayObjectWriteTaskScheduler struct {
	gateway *S3Gateway
}

func newGatewayObjectWriteTaskScheduler(gateway *S3Gateway) *gatewayObjectWriteTaskScheduler {
	return &gatewayObjectWriteTaskScheduler{gateway: gateway}
}

func (s *gatewayObjectWriteTaskScheduler) ScheduleObjectWriteTasks(ctx context.Context, req objectWriteTaskRequest) {
	g := s.gateway
	if g == nil {
		return
	}

	if g.vector != nil && g.config.Vector.Enabled && !req.SkipVectorize {
		g.submitBackgroundTask(ctx, taskqueue.KindVectorize, taskqueue.VectorizePayload{
			Bucket:      req.Bucket,
			Key:         req.Key,
			ContentType: req.ContentType,
			Metadata:    req.Metadata,
			UserID:      req.UserID,
		})
	}

	if g.ftsIndex != nil && g.config.FTS.Enabled && !req.SkipFTS {
		g.submitBackgroundTask(ctx, taskqueue.KindFTS, taskqueue.FTSPayload{
			Bucket:      req.Bucket,
			Key:         req.Key,
			ContentType: req.ContentType,
			VersionID:   req.VersionID,
		})
	}

	if g.pipeline != nil {
		g.submitBackgroundTask(ctx, taskqueue.KindPipeline, taskqueue.PipelinePayload{
			Bucket:      req.Bucket,
			Key:         req.Key,
			ContentType: req.ContentType,
			Metadata:    req.Metadata,
		})
	}
}
