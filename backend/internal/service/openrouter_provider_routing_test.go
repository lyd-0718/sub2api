package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func openRouterRoutingExtra(enabled bool, models map[string]any) map[string]any {
	// 与数据库读出的形态一致：嵌套 JSON 反序列化成 map[string]any / []any。
	return map[string]any{
		OpenRouterProviderRoutingExtraKey: map[string]any{
			"enabled": enabled,
			"models":  models,
		},
	}
}

func TestNormalizeOpenRouterProviderRouting(t *testing.T) {
	got, err := NormalizeOpenRouterProviderRouting(OpenRouterProviderRouting{
		Enabled: true,
		Models: map[string][]string{
			" z-ai/glm-5.3-flash ":         {" Wafer ", "relace", "wafer"},
			"z-ai/glm-5.3":                 {"sail-research/us"},
			"deepseek/deepseek-v4.1-flash": {"", " "},
			"  ":                           {"wafer"},
		},
	})
	require.NoError(t, err)
	require.True(t, got.Enabled)
	require.Equal(t, map[string][]string{
		"z-ai/glm-5.3-flash": {"wafer", "relace"},
		"z-ai/glm-5.3":       {"sail-research/us"},
	}, got.Models)

	_, err = NormalizeOpenRouterProviderRouting(OpenRouterProviderRouting{Models: map[string][]string{
		"z-ai/glm-5.3": {"wafer", "relace", "friendli"},
	}})
	require.ErrorContains(t, err, "at most 2 providers")

	_, err = NormalizeOpenRouterProviderRouting(OpenRouterProviderRouting{Models: map[string][]string{
		"z-ai/glm-5.3": {"wafer?x=1"},
	}})
	require.ErrorContains(t, err, "invalid OpenRouter provider slug")
}

func TestApplyOpenRouterProviderRouting(t *testing.T) {
	const orURL = "https://openrouter.ai/api/v1/chat/completions"
	extra := openRouterRoutingExtra(true, map[string]any{"z-ai/glm-5.3-flash": []any{"wafer", "relace"}})
	body := []byte(`{"model":"z-ai/glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`)

	t.Run("injects order with fallbacks to OpenRouter auto routing", func(t *testing.T) {
		got := applyOpenRouterProviderRouting(&Account{Extra: extra}, orURL, body)
		require.JSONEq(t, `["wafer","relace"]`, gjson.GetBytes(got, "provider.order").Raw)
		require.True(t, gjson.GetBytes(got, "provider.allow_fallbacks").Bool())
		require.Equal(t, "z-ai/glm-5.3-flash", gjson.GetBytes(got, "model").String())
	})

	t.Run("anthropic and responses endpoints on openrouter are covered", func(t *testing.T) {
		for _, u := range []string{"https://openrouter.ai/api/v1/messages", "https://openrouter.ai/api/v1/responses"} {
			got := applyOpenRouterProviderRouting(&Account{Extra: extra}, u, body)
			require.True(t, gjson.GetBytes(got, "provider").Exists(), u)
		}
	})

	untouched := []struct {
		name    string
		account *Account
		url     string
		body    []byte
	}{
		{"non-openrouter host", &Account{Extra: extra}, "https://api.deepseek.com/chat/completions", body},
		{"plain http openrouter", &Account{Extra: extra}, "http://openrouter.ai/api/v1/chat/completions", body},
		{"disabled", &Account{Extra: openRouterRoutingExtra(false, map[string]any{"z-ai/glm-5.3-flash": []any{"wafer"}})}, orURL, body},
		{"model not configured", &Account{Extra: extra}, orURL, []byte(`{"model":"z-ai/glm-5.3","messages":[]}`)},
		{"client provider wins", &Account{Extra: extra}, orURL, []byte(`{"model":"z-ai/glm-5.3-flash","provider":{"order":["together"]}}`)},
		{"no config", &Account{}, orURL, body},
		{"nil account", nil, orURL, body},
	}
	for _, tt := range untouched {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, string(tt.body), string(applyOpenRouterProviderRouting(tt.account, tt.url, tt.body)))
		})
	}
}

func TestParseOpenRouterEndpoints(t *testing.T) {
	body := []byte(`{"data":{"id":"z-ai/glm-5.3-flash","endpoints":[
		{"provider_name":"Morph","tag":"morph","quantization":"unknown","status":-2,"uptime_last_1d":99.99,
		 "pricing":{"prompt":"0.000000088","completion":"0.000000308"},"supported_parameters":["tools"]},
		{"provider_name":"Relace","tag":"relace","quantization":"unknown","status":0,"uptime_last_1d":99.90,
		 "context_length":1048576,"pricing":{"prompt":"0.0000001","completion":"0.00000036","input_cache_read":"0.00000002"},
		 "supported_parameters":["reasoning","tools","tool_choice"]},
		{"provider_name":"Wafer","tag":"wafer","quantization":"unknown","status":0,"uptime_last_1d":99.95,
		 "pricing":{"prompt":"0.000000089","completion":"0.00000035","discount":0.1},"supported_parameters":["reasoning"]},
		{"provider_name":"NoTag","tag":"","status":0}
	]}}`)

	got := parseOpenRouterEndpoints(body)
	require.Len(t, got, 3)
	require.Equal(t, []string{"wafer", "relace", "morph"}, []string{got[0].Slug, got[1].Slug, got[2].Slug})

	relace := got[1]
	require.Equal(t, "Relace", relace.Name)
	require.True(t, relace.SupportsTools)
	require.Equal(t, int64(1048576), relace.ContextLength)
	require.InDelta(t, 0.1, *relace.PromptPrice, 1e-9)
	require.InDelta(t, 0.36, *relace.CompletionPrice, 1e-9)
	require.InDelta(t, 0.02, *relace.CacheReadPrice, 1e-9)

	require.False(t, got[0].SupportsTools)
	require.Nil(t, got[0].CacheReadPrice)
	require.InDelta(t, 0.1, got[0].Discount, 1e-9)
	require.Equal(t, -2, got[2].Status)
}

func TestIsOpenRouterAccount(t *testing.T) {
	require.True(t, isOpenRouterAccount(&Account{Platform: PlatformDeepseek, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "https://openrouter.ai/api/v1"}}))
	require.False(t, isOpenRouterAccount(&Account{Platform: PlatformDeepseek, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "https://api.deepseek.com"}}))
	require.False(t, isOpenRouterAccount(nil))
}
