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
	// Retried current-turn payloads have already been expanded. Do not add a
	// second copy when the transport retries that same normalized payload.
	// The already-applied check is per injection mode: prepend/append look at
	// the respective end of the string; replace is detected by equality.
	if prompt != "" && instructions.Type == gjson.String && customPromptAlreadyApplied(instructions.String(), prompt, resolution.InjectionMode) {
		copy := *resolution
		copy.SystemPrompt = ""
		return ApplyCustomModelRequest(body, &copy, "responses")
	}
	return ApplyCustomModelRequest(body, resolution, "responses")
}

// customPromptAlreadyApplied 判断客户端 instructions 是否已包含本模型的注入结果。
func customPromptAlreadyApplied(current, prompt, mode string) bool {
	switch mode {
	case "append":
		return current == prompt || strings.HasSuffix(current, "\n\n"+prompt)
	case "replace":
		return current == prompt
	default: // "prepend"
		return current == prompt || strings.HasPrefix(current, prompt+"\n\n")
	}
}
