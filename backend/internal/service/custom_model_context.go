package service

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

type customModelContextKey struct{}

type customModelRequest struct {
	resolution        *CustomModelResolution
	downstreamGroupID int64
}

// WithCustomModelResolution separates the upstream account pool from the
// authenticated API key's downstream billing group. It never mutates the key.
func WithCustomModelResolution(ctx context.Context, resolution *CustomModelResolution, downstreamGroupID int64) context.Context {
	ctx = context.WithValue(ctx, customModelContextKey{}, customModelRequest{resolution, downstreamGroupID})
	ctx = context.WithValue(ctx, ctxkey.RequestedPublicModel, resolution.ModelID)
	if _, resolved := ResolvedUpstreamModelFromContext(ctx); !resolved || resolution.UpstreamGroup.Platform != PlatformComposite {
		ctx = context.WithValue(ctx, ctxkey.ResolvedUpstreamModel, resolution.UpstreamModel)
	}
	if resolution.UpstreamGroup.Platform != PlatformComposite {
		ctx = WithResolvedTargetPlatform(ctx, resolution.UpstreamGroup.Platform)
	}
	return ctx
}

func CustomModelResolutionFromContext(ctx context.Context) (*CustomModelResolution, bool) {
	if ctx == nil {
		return nil, false
	}
	request, ok := ctx.Value(customModelContextKey{}).(customModelRequest)
	return request.resolution, ok && request.resolution != nil
}

// CustomModelRoutingGroupID translates the original group at scheduling entry
// points. A distinct, explicitly selected fallback group is left unchanged.
func CustomModelRoutingGroupID(ctx context.Context, groupID *int64) *int64 {
	if ctx == nil || groupID == nil {
		return groupID
	}
	request, ok := ctx.Value(customModelContextKey{}).(customModelRequest)
	if !ok || request.resolution == nil || *groupID != request.downstreamGroupID {
		return groupID
	}
	return &request.resolution.UpstreamGroupID
}

// CopyCustomModelContext carries attribution into detached usage-record tasks.
func CopyCustomModelContext(parent, target context.Context) context.Context {
	request, ok := parent.Value(customModelContextKey{}).(customModelRequest)
	if !ok {
		return target
	}
	target = WithCustomModelResolution(target, request.resolution, request.downstreamGroupID)
	if platform, ok := ResolvedTargetPlatformFromContext(parent); ok {
		target = WithResolvedTargetPlatform(target, platform)
	}
	if model, ok := ResolvedUpstreamModelFromContext(parent); ok {
		target = context.WithValue(target, ctxkey.ResolvedUpstreamModel, model)
	}
	return target
}
