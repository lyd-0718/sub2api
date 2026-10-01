package routes

import (
	"errors"
	"net/http"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// Run after downstream admission, before composite routing and protocol
// dispatch. Keep the authenticated API key intact: only scheduling uses the
// upstream group recorded in the request context.
func customModelMiddleware(models *service.CustomModelService, composite *service.CompositeRouteResolver) gin.HandlerFunc {
	return func(c *gin.Context) {
		if models == nil || c.Request.Method != http.MethodPost {
			c.Next()
			return
		}
		key, ok := middleware.GetAPIKeyFromContext(c)
		if !ok || key == nil || key.GroupID == nil {
			c.Next()
			return
		}
		body, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
		if err != nil {
			status := http.StatusBadRequest
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				status = http.StatusRequestEntityTooLarge
			}
			customModelError(c, status, "Failed to read request body")
			return
		}
		requestmodel.ResetRequestBody(c.Request, body)
		protocol := customModelProtocol(c.FullPath())
		candidates := requestmodel.FromBodyCandidates(c.FullPath(), c.GetHeader("Content-Type"), body)
		if protocol == "gemini" {
			candidates = []string{compositeGeminiModelFromParams(c)}
		}
		var resolved *service.CustomModelResolution
		for i, model := range candidates {
			if model == "" || (i > 0 && model == candidates[0]) {
				continue
			}
			resolution, resolveErr := models.ResolveCustomModel(c.Request.Context(), model, *key.GroupID)
			if errors.Is(resolveErr, service.ErrCustomModelNotFound) {
				continue
			}
			if resolveErr != nil {
				status := infraerrors.Code(resolveErr)
				message := infraerrors.Message(resolveErr)
				if status >= 500 {
					message = "Failed to resolve custom model"
				}
				customModelError(c, status, message)
				return
			}
			resolved = resolution
		}
		if resolved == nil {
			c.Next()
			return
		}
		for _, model := range candidates {
			if model != resolved.ModelID {
				customModelError(c, http.StatusBadRequest, "Conflicting model fields in custom model request")
				return
			}
		}
		ctx := c.Request.Context()
		if resolved.UpstreamGroup.Platform == service.PlatformComposite {
			if composite == nil {
				customModelError(c, http.StatusInternalServerError, "Composite routing is unavailable")
				return
			}
			decision, resolveErr := composite.Resolve(ctx, resolved.UpstreamGroupID, resolved.UpstreamModel, compositeRouteEndpointForPath(c.Request.URL.Path))
			if resolveErr != nil || !decision.Matched {
				customModelError(c, http.StatusBadRequest, "Custom model upstream platform could not be resolved")
				return
			}
			ctx = service.WithCompositeRouteDecision(ctx, decision)
		}
		ctx = service.WithCustomModelResolution(ctx, resolved, *key.GroupID)
		forward := *resolved
		forward.UpstreamModel, _ = service.ResolvedUpstreamModelFromContext(ctx)
		if protocol == "gemini" {
			platform, _ := service.ResolvedTargetPlatformFromContext(ctx)
			if platform != service.PlatformGemini && platform != service.PlatformAntigravity {
				customModelError(c, http.StatusBadRequest, "Custom model upstream does not support the Gemini native protocol")
				return
			}
			if !service.IsSafeGeminiModelPathSegment(forward.UpstreamModel) {
				customModelError(c, http.StatusBadRequest, "Invalid custom model upstream Gemini model")
				return
			}
		}
		body, err = service.ApplyCustomModelRequest(body, &forward, protocol)
		if err != nil {
			customModelError(c, http.StatusBadRequest, err.Error())
			return
		}
		c.Request = c.Request.WithContext(ctx)
		requestmodel.ResetRequestBody(c.Request, body)
		c.Next()
	}
}

func customModelProtocol(path string) string {
	switch {
	case strings.Contains(path, "/v1beta/models/"):
		return "gemini"
	case strings.HasSuffix(path, "/messages"), strings.HasSuffix(path, "/messages/count_tokens"):
		return "messages"
	case strings.HasSuffix(path, "/chat/completions"):
		return "chat"
	case strings.HasSuffix(path, "/responses"), strings.Contains(path, "/responses/"):
		return "responses"
	default:
		return ""
	}
}

func customModelError(c *gin.Context, status int, message string) {
	writer := middleware.OpenAIErrorWriter
	switch customModelProtocol(c.FullPath()) {
	case "messages":
		writer = middleware.AnthropicErrorWriter
	case "gemini":
		writer = middleware.GoogleErrorWriter
	}
	writer(c, status, message)
	c.Abort()
}
