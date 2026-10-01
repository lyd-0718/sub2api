package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// Discovery always uses the authenticated downstream group, never a routing
// override or the administrative group-zero listing.
func customDiscoveryModelIDs(ctx context.Context, customs *service.CustomModelService, apiKey *service.APIKey) ([]string, error) {
	if customs == nil || apiKey == nil || apiKey.Group == nil || apiKey.Group.ID <= 0 {
		return nil, nil
	}
	models, err := customs.List(ctx, apiKey.Group.ID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(models))
	// Keep the exact public IDs: allowlist spelling must not rename a custom
	// model, and Gemini resource names use the same admission normalization.
	for _, model := range models {
		if apiKey.Group.ModelAllowlist.Allows(model.ModelID) {
			ids = append(ids, model.ModelID)
		}
	}
	return ids, nil
}

func customDiscoveryEntries(ids []string, platform string) ([]json.RawMessage, error) {
	entries := make([]json.RawMessage, 0, len(ids))
	for _, id := range ids {
		var model any = claude.Model{ID: id, Type: "model", DisplayName: id, CreatedAt: "2024-01-01T00:00:00Z"}
		if platform == service.PlatformOpenAI {
			model = openai.Model{ID: id, Object: "model", Created: 1704067200, OwnedBy: "openai", Type: "model", DisplayName: id}
		}
		entry, err := json.Marshal(model)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// Preserve the native envelope and all native metadata on exact ID collisions.
func appendCustomModelList(body []byte, ids []string, platform string) ([]byte, error) {
	if len(ids) == 0 {
		return body, nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	var models []json.RawMessage
	if envelope == nil || envelope["data"] == nil {
		return nil, fmt.Errorf("missing model catalogue data")
	}
	if err := json.Unmarshal(envelope["data"], &models); err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(models)+len(ids))
	for _, raw := range models {
		var model struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &model); err != nil {
			return nil, err
		}
		seen[model.ID] = true
	}
	extra, err := customDiscoveryEntries(ids, platform)
	if err != nil {
		return nil, err
	}
	for i, id := range ids {
		if !seen[id] {
			models = append(models, extra[i])
			seen[id] = true
		}
	}
	envelope["data"], err = json.Marshal(models)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope)
}

func writeDefaultModelsWithCustoms(c *gin.Context, native any, ids []string, platform string) {
	if len(ids) == 0 {
		writeModelsListResponse(c, native)
		return
	}
	body, err := json.Marshal(gin.H{"object": "list", "data": native})
	if err == nil {
		body, err = appendCustomModelList(body, ids, platform)
	}
	if err != nil {
		writeOpenAIModelsError(c, http.StatusInternalServerError, "api_error", "Failed to build model catalogue")
		return
	}
	writeOpenAIModelsResponse(c, &service.OpenAIModelsResponse{Body: body})
}

func applyCustomDiscoveryETag(response *service.OpenAIModelsResponse, ifNoneMatch string) {
	response.ETag = service.CodexModelsManifestETag(response.Body)
	response.NotModified = service.CodexModelsManifestETagMatches(ifNoneMatch, response.ETag)
	if response.NotModified {
		response.Body = nil
	}
}
