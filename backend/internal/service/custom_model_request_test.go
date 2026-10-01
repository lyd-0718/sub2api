package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCustomModelRequestPreservesClientContent(t *testing.T) {
	resolution := &CustomModelResolution{ModelID: "kimi-my", UpstreamModel: "k3", SystemPrompt: "Admin instruction"}
	for _, tc := range []struct{ name, protocol, body, want string }{
		{"anthropic string", "messages", `{"model":"kimi-my","system":"Client instruction","messages":[{"role":"user","content":"hello"}],"max_tokens":16}`, `{"model":"k3","system":"Admin instruction\n\nClient instruction","messages":[{"role":"user","content":"hello"}],"max_tokens":16}`},
		{"anthropic blocks", "messages", `{"model":"kimi-my","system":[{"type":"text","text":"Client instruction","cache_control":{"type":"ephemeral"}}]}`, `{"model":"k3","system":[{"type":"text","text":"Admin instruction"},{"type":"text","text":"Client instruction","cache_control":{"type":"ephemeral"}}]}`},
		{"chat structured content", "chat", `{"model":"kimi-my","messages":[{"role":"system","content":[{"type":"text","text":"Client instruction"}]},{"role":"user","content":"hello"}],"seed":9007199254740993}`, `{"model":"k3","messages":[{"role":"system","content":"Admin instruction"},{"role":"system","content":[{"type":"text","text":"Client instruction"}]},{"role":"user","content":"hello"}],"seed":9007199254740993}`},
		{"responses", "responses", `{"model":"kimi-my","instructions":"Client instruction","input":"hello","stream":true}`, `{"model":"k3","instructions":"Admin instruction\n\nClient instruction","input":"hello","stream":true}`},
		{"gemini parts", "gemini", `{"system_instruction":{"parts":[{"text":"Client instruction"}],"role":"system"},"contents":[]}`, `{"systemInstruction":{"parts":[{"text":"Admin instruction"},{"text":"Client instruction"}],"role":"system"},"contents":[]}`},
		{"gemini count", "gemini", `{"generateContentRequest":{"model":"models/kimi-my","contents":[],"systemInstruction":{"parts":[{"text":"Client instruction"}]}}}`, `{"generateContentRequest":{"model":"models/k3","contents":[],"systemInstruction":{"parts":[{"text":"Admin instruction"},{"text":"Client instruction"}]}}}`},
		{"canonical model", "responses", `{"Model":"kimi-my","model":"kimi-my","model":"kimi-my","input":[]}`, `{"model":"k3","input":[],"instructions":"Admin instruction"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ApplyCustomModelRequest([]byte(tc.body), resolution, tc.protocol)
			require.NoError(t, err)
			require.JSONEq(t, tc.want, string(got))
			if tc.name == "chat structured content" {
				require.Contains(t, string(got), "9007199254740993")
			}
		})
	}
}

func TestCustomModelGeminiCountWithoutPromptRewritesNestedModel(t *testing.T) {
	body, err := ApplyCustomModelRequest([]byte(`{"generate_content_request":{"model":"models/public","contents":[{"parts":[{"text":"hello"}]}]}}`), &CustomModelResolution{ModelID: "public", UpstreamModel: "gemini-2.5-pro"}, "gemini")
	require.NoError(t, err)
	require.JSONEq(t, `{"generateContentRequest":{"model":"models/gemini-2.5-pro","contents":[{"parts":[{"text":"hello"}]}]}}`, string(body))
}

func TestCustomModelRequestRejectsMalformedPromptContainers(t *testing.T) {
	resolution := &CustomModelResolution{UpstreamModel: "k3", SystemPrompt: "required"}
	for _, tc := range []struct{ protocol, body string }{
		{"messages", `{"system":123}`}, {"chat", `{"messages":"hello"}`},
		{"responses", `{"instructions":[]}`}, {"gemini", `{"systemInstruction":{"parts":true}}`},
		{"gemini", `{"generateContentRequest":null}`}, {"responses", `null`},
	} {
		_, err := ApplyCustomModelRequest([]byte(tc.body), resolution, tc.protocol)
		require.Error(t, err, tc.body)
	}
}

func TestCustomModelWebSocketRetryDoesNotDuplicatePrompt(t *testing.T) {
	resolution := &CustomModelResolution{ModelID: "public", UpstreamModel: "gpt-5.4", UpstreamGroupID: 2, UpstreamGroup: &Group{ID: 2, Platform: PlatformOpenAI}, SystemPrompt: "instruction"}
	ctx := WithCustomModelResolution(context.Background(), resolution, 1)
	body, err := applyCustomModelWebSocketRequest(ctx, []byte(`{"model":"public","instructions":"client","input":"hello"}`), "public")
	require.NoError(t, err)
	retry, err := applyCustomModelWebSocketRequest(ctx, body, "public")
	require.NoError(t, err)
	require.JSONEq(t, `{"model":"gpt-5.4","instructions":"instruction\n\nclient","input":"hello"}`, string(retry))
	_, err = applyCustomModelWebSocketRequest(ctx, body, "different-model")
	require.ErrorContains(t, err, "reconnect")
}
