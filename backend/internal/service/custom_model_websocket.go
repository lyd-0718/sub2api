package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

// A WebSocket is bound to one upstream account pool. New turns keep that
// binding, including when the client omits model; switching requires reconnect.
func applyCustomModelWebSocketRequest(ctx context.Context, body []byte, model string) ([]byte, error) {
	resolution, ok := CustomModelResolutionFromContext(ctx)
	if !ok {
		return body, nil
	}
	if model != resolution.ModelID && model != resolution.UpstreamModel {
		return nil, fmt.Errorf("reconnect to switch a custom model")
	}
	prompt := resolution.SystemPrompt
	instructions := gjson.GetBytes(body, "instructions")
	// Retried current-turn payloads have already been expanded. Do not prepend
	// a second copy when the transport retries that same normalized payload.
	if prompt != "" && instructions.Type == gjson.String &&
		(instructions.String() == prompt || strings.HasPrefix(instructions.String(), prompt+"\n\n")) {
		copy := *resolution
		copy.SystemPrompt = ""
		return ApplyCustomModelRequest(body, &copy, "responses")
	}
	return ApplyCustomModelRequest(body, resolution, "responses")
}
