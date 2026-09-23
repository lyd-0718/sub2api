package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func newAntigravityAccountWithMapping(mapping map[string]string) *Account {
	mappingAny := make(map[string]any, len(mapping))
	for k, v := range mapping {
		mappingAny[k] = v
	}
	return &Account{
		ID:       1,
		Name:     "ag",
		Platform: PlatformAntigravity,
		Credentials: map[string]any{
			"model_mapping": mappingAny,
		},
	}
}

func TestGeminiThinkingLevelFromBody(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"empty body", ``, "high"},
		{"no thinkingConfig", `{"contents":[]}`, "high"},
		{"budget -1 dynamic", `{"generationConfig":{"thinkingConfig":{"thinkingBudget":-1}}}`, "high"},
		{"budget 0 off", `{"generationConfig":{"thinkingConfig":{"thinkingBudget":0}}}`, "low"},
		{"budget 1000 (agy low)", `{"generationConfig":{"thinkingConfig":{"thinkingBudget":1000}}}`, "low"},
		{"budget 4000 (agy medium)", `{"generationConfig":{"thinkingConfig":{"thinkingBudget":4000}}}`, "medium"},
		{"budget 24576", `{"generationConfig":{"thinkingConfig":{"thinkingBudget":24576}}}`, "high"},
		{"thinkingLevel wins over budget", `{"generationConfig":{"thinkingConfig":{"thinkingBudget":24576,"thinkingLevel":"low"}}}`, "low"},
		{"thinkingLevel medium", `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"MEDIUM"}}}`, "medium"},
		{"invalid json", `{"generationConfig":`, "high"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, geminiThinkingLevelFromBody([]byte(tt.body)))
		})
	}
}

func TestResolveGeminiThinkingVariant(t *testing.T) {
	// 与生产 Antigravity 账号从上游同步来的目录一致：只有带后缀的变体，没有裸名。
	catalog := map[string]string{
		"gemini-3.8-flash-low":    "gemini-3.8-flash-low",
		"gemini-3.8-flash-medium": "gemini-3.8-flash-medium",
		"gemini-3.8-flash-high":   "gemini-3.8-flash-high",
		"gemini-3.8-flash-tiered": "gemini-3.8-flash-tiered",
		"gemini-3.6-flash-high":   "gemini-3.6-flash-high",
		"gemini-3.1-pro-high":     "gemini-3.1-pro-high",
		"gemini-2.5-flash":        "gemini-2.5-flash",
	}
	budget := func(v string) []byte {
		return []byte(`{"contents":[],"generationConfig":{"thinkingConfig":{"thinkingBudget":` + v + `}}}`)
	}

	tests := []struct {
		name      string
		mapping   map[string]string
		model     string
		body      []byte
		want      string
		wantMatch bool
	}{
		{"agy high (-1) -> -high", catalog, "gemini-3.8-flash", budget("-1"), "gemini-3.8-flash-high", true},
		{"agy medium (4000) -> -medium", catalog, "gemini-3.8-flash", budget("4000"), "gemini-3.8-flash-medium", true},
		{"agy low (1000) -> -low", catalog, "gemini-3.8-flash", budget("1000"), "gemini-3.8-flash-low", true},
		{"models/ prefix stripped", catalog, "models/gemini-3.8-flash", budget("1000"), "gemini-3.8-flash-low", true},
		{"no thinkingConfig -> -high", catalog, "gemini-3.8-flash", []byte(`{"contents":[]}`), "gemini-3.8-flash-high", true},
		// gemini-3.6/3.7/3.8-flash 的四个变体会被 resolveModelMapping 自动补齐，所以降级用一个不在默认表里的型号
		{"only -high exists: low request degrades to high", map[string]string{
			"gemini-3.5-flash-high": "gemini-3.5-flash-high",
		}, "gemini-3.5-flash", budget("1000"), "gemini-3.5-flash-high", true},
		{"injected default variants are usable", catalog, "gemini-3.6-flash", budget("1000"), "gemini-3.6-flash-low", true},
		{"already suffixed: untouched", catalog, "gemini-3.8-flash-low", budget("-1"), "", false},
		{"bare model without variants: untouched", catalog, "gemini-2.5-flash", budget("-1"), "", false},
		{"no variants in mapping: untouched", catalog, "gemini-9.9-flash", budget("-1"), "", false},
		{"non-gemini model: untouched", catalog, "claude-sonnet-4-6", budget("-1"), "", false},
		{"explicit bare alias wins over variants", map[string]string{
			"gemini-3.8-flash":      "gemini-3.8-flash-tiered",
			"gemini-3.8-flash-high": "gemini-3.8-flash-high",
		}, "gemini-3.8-flash", budget("-1"), "", false},
		{"variant value is itself a mapping target", map[string]string{
			"gemini-3.8-flash-high": "gemini-3.8-flash-tiered",
		}, "gemini-3.8-flash", budget("-1"), "gemini-3.8-flash-tiered", true},
		{"bare identity passthrough in credentials: still resolved", map[string]string{
			// 后台按 DefaultAntigravityModelMapping 建号会把裸名自映射写进 credentials，
			// 上游没有裸名，这条透传不代表用户意图。
			"gemini-3.8-flash":      "gemini-3.8-flash",
			"gemini-3.8-flash-high": "gemini-3.8-flash-high",
		}, "gemini-3.8-flash", budget("1000"), "gemini-3.8-flash-low", true},
		{"runtime-injected bare passthrough (not in credentials): still resolved", map[string]string{
			// resolveModelMapping 会补 gemini-3.7-flash → gemini-3.7-flash 的默认透传，
			// 但 credentials 里没有这条，应当继续推导变体。
			"gemini-3.7-flash-medium": "gemini-3.7-flash-medium",
		}, "gemini-3.7-flash", budget("4000"), "gemini-3.7-flash-medium", true},
		// 空 credentials 映射 → 走 DefaultAntigravityModelMapping，其中同样只有带后缀的 3.8 flash
		{"empty mapping falls back to default catalog", map[string]string{}, "gemini-3.8-flash", budget("-1"), "gemini-3.8-flash-high", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := newAntigravityAccountWithMapping(tt.mapping)
			got, matched := resolveGeminiThinkingVariant(account, tt.model, geminiThinkingLevelFromBody(tt.body))
			require.Equal(t, tt.wantMatch, matched)
			require.Equal(t, tt.want, got)
		})
	}

	t.Run("nil account", func(t *testing.T) {
		got, matched := resolveGeminiThinkingVariant(nil, "gemini-3.8-flash", "high")
		require.False(t, matched)
		require.Empty(t, got)
	})
}

func TestTrimGeminiThinkingVariantSuffix(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		{"gemini-3.8-flash-high", "gemini-3.8-flash"},
		{"gemini-3.8-flash-TIERED", "gemini-3.8-flash"},
		{"gemini-3.1-pro-low", "gemini-3.1-pro"},
		{"gemini-3.8-flash", "gemini-3.8-flash"},
		{"gemini-2.5-flash-thinking", "gemini-2.5-flash-thinking"},
		{"kimi-k3-high", "kimi-k3-high"},
		{"-high", "-high"},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			require.Equal(t, tt.want, trimGeminiThinkingVariantSuffix(tt.model))
		})
	}
}

func TestGeminiThinkingLevelFromOpenAIBody(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"no effort", `{"model":"gemini-3.8-flash"}`, ""},
		{"chat reasoning_effort low", `{"reasoning_effort":"low"}`, "low"},
		{"chat reasoning_effort minimal", `{"reasoning_effort":"minimal"}`, "low"},
		{"chat reasoning_effort none", `{"reasoning_effort":"none"}`, "low"},
		{"chat reasoning_effort medium", `{"reasoning_effort":"Medium"}`, "medium"},
		{"chat reasoning_effort xhigh", `{"reasoning_effort":"xhigh"}`, "high"},
		{"chat reasoning_effort max", `{"reasoning_effort":"max"}`, "high"},
		{"responses reasoning.effort wins", `{"reasoning":{"effort":"low"},"reasoning_effort":"high"}`, "low"},
		{"unknown effort", `{"reasoning_effort":"turbo"}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, geminiThinkingLevelFromOpenAIBody([]byte(tt.body)))
		})
	}
}

func TestGeminiThinkingLevelFromClaudeBody(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"nothing", `{"model":"gemini-3.8-flash"}`, ""},
		{"output_config effort low", `{"output_config":{"effort":"low"}}`, "low"},
		{"output_config effort max", `{"output_config":{"effort":"max"}}`, "high"},
		{"output_config wins over thinking", `{"output_config":{"effort":"medium"},"thinking":{"type":"enabled","budget_tokens":32000}}`, "medium"},
		{"thinking disabled", `{"thinking":{"type":"disabled"}}`, "low"},
		{"thinking adaptive", `{"thinking":{"type":"adaptive"}}`, "high"},
		{"thinking budget 1024", `{"thinking":{"type":"enabled","budget_tokens":1024}}`, "low"},
		{"thinking budget 4096", `{"thinking":{"type":"enabled","budget_tokens":4096}}`, "medium"},
		{"thinking budget 10240", `{"thinking":{"type":"enabled","budget_tokens":10240}}`, "high"},
		{"thinking enabled without budget", `{"thinking":{"type":"enabled"}}`, "high"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, geminiThinkingLevelFromClaudeBody([]byte(tt.body)))
		})
	}
}

func TestResolveGeminiThinkingVariantLevelArgument(t *testing.T) {
	account := newAntigravityAccountWithMapping(map[string]string{
		"gemini-3.8-flash":        "gemini-3.8-flash",
		"gemini-3.8-flash-low":    "gemini-3.8-flash-low",
		"gemini-3.8-flash-medium": "gemini-3.8-flash-medium",
		"gemini-3.8-flash-high":   "gemini-3.8-flash-high",
		"gemini-3.8-flash-tiered": "gemini-3.8-flash-tiered",
	})
	for level, want := range map[string]string{
		"":       "gemini-3.8-flash-high",
		"low":    "gemini-3.8-flash-low",
		"medium": "gemini-3.8-flash-medium",
		"high":   "gemini-3.8-flash-high",
	} {
		got, matched := resolveGeminiThinkingVariant(account, "gemini-3.8-flash", level)
		require.True(t, matched, "level %q", level)
		require.Equal(t, want, got, "level %q", level)
	}
}
