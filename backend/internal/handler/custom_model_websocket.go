package handler

import (
	"context"
	"errors"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *OpenAIGatewayHandler) resolveCustomWebSocketModel(c *gin.Context, key *service.APIKey, model string) (string, error) {
	if h.customModelService == nil || key.GroupID == nil {
		return model, nil
	}
	resolution, err := h.customModelService.ResolveCustomModel(c.Request.Context(), model, *key.GroupID)
	if errors.Is(err, service.ErrCustomModelNotFound) {
		return model, nil
	}
	if err != nil {
		return "", err
	}
	platform := resolution.UpstreamGroup.Platform
	if platform == service.PlatformComposite {
		platform, _ = service.DetectModelPlatform(resolution.UpstreamModel)
	}
	if !isResponsesWebSocketCompositePlatform(platform) {
		return "", fmt.Errorf("custom model upstream does not support Responses WebSocket")
	}
	ctx := service.WithCustomModelResolution(c.Request.Context(), resolution, *key.GroupID)
	c.Request = c.Request.WithContext(service.WithResolvedTargetPlatform(ctx, platform))
	return resolution.UpstreamModel, nil
}

func (h *OpenAIGatewayHandler) customWebSocketModelForTurn(ctx context.Context, key *service.APIKey, model string) (string, error) {
	if h.customModelService == nil || key.GroupID == nil {
		return model, nil
	}
	bound, custom := service.CustomModelResolutionFromContext(ctx)
	if custom {
		if model != bound.ModelID && model != bound.UpstreamModel {
			return "", fmt.Errorf("reconnect to switch a custom model")
		}
		current, err := h.customModelService.ResolveCustomModel(ctx, bound.ModelID, *key.GroupID)
		if err != nil {
			return "", err
		}
		if current.UpstreamGroupID != bound.UpstreamGroupID || current.UpstreamModel != bound.UpstreamModel || current.SystemPrompt != bound.SystemPrompt {
			return "", fmt.Errorf("custom model configuration changed; reconnect to continue")
		}
		return current.UpstreamModel, nil
	}
	_, err := h.customModelService.Get(ctx, model)
	if errors.Is(err, service.ErrCustomModelNotFound) {
		return model, nil
	}
	if err != nil {
		return "", err
	}
	return "", fmt.Errorf("reconnect to switch to a custom model")
}
