//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 生产 OpenRouter 账号为 adaptive 协议：三种入站各走 OpenRouter 的原生端点，
// 三个出站构造点都必须带上 provider 偏好。
func openRouterAdaptiveTestAccount() *Account {
	account := adaptiveProtocolTestAccount(PlatformDeepseek, map[string]any{
		APIProtocolChatCompletions: "https://openrouter.ai/api/v1",
		APIProtocolAnthropic:       "https://openrouter.ai/api",
		APIProtocolResponses:       "https://openrouter.ai/api/v1",
	})
	account.Extra = openRouterRoutingExtra(true, map[string]any{"z-ai/glm-5.3-flash": []any{"wafer", "relace"}})
	return account
}

func requireOpenRouterProviderOrder(t *testing.T, body []byte) {
	t.Helper()
	require.JSONEq(t, `["wafer","relace"]`, gjson.GetBytes(body, "provider.order").Raw)
	require.True(t, gjson.GetBytes(body, "provider.allow_fallbacks").Bool())
}

func TestOpenRouterProviderRoutingInjectedOnChatCompletions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"z-ai/glm-5.3-flash","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	upstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}

	_, err := svc.ForwardAsChatCompletions(context.Background(), adaptiveProtocolTestContext("/v1/chat/completions", body), openRouterAdaptiveTestAccount(), body, "", "")
	require.Error(t, err)
	require.Equal(t, "https://openrouter.ai/api/v1/chat/completions", upstream.lastReq.URL.String())
	requireOpenRouterProviderOrder(t, upstream.lastBody)
}

func TestOpenRouterProviderRoutingInjectedOnResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"z-ai/glm-5.3-flash","input":"hello","max_output_tokens":32,"stream":false}`)
	upstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}

	_, err := svc.Forward(context.Background(), adaptiveProtocolTestContext("/v1/responses", body), openRouterAdaptiveTestAccount(), body)
	require.Error(t, err)
	require.Equal(t, "https://openrouter.ai/api/v1/responses", upstream.lastReq.URL.String())
	requireOpenRouterProviderOrder(t, upstream.lastBody)
}

func TestOpenRouterProviderRoutingInjectedOnMessages(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"z-ai/glm-5.3-flash","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	upstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}

	_, err := svc.ForwardAsAnthropic(context.Background(), adaptiveProtocolTestContext("/v1/messages", body), openRouterAdaptiveTestAccount(), body, "", "")
	require.Error(t, err)
	require.Equal(t, "https://openrouter.ai/api/v1/messages", upstream.lastReq.URL.String())
	requireOpenRouterProviderOrder(t, upstream.lastBody)
}

func TestOpenRouterProviderRoutingSkipsUnconfiguredModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	upstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}

	_, err := svc.ForwardAsChatCompletions(context.Background(), adaptiveProtocolTestContext("/v1/chat/completions", body), openRouterAdaptiveTestAccount(), body, "", "")
	require.Error(t, err)
	require.False(t, gjson.GetBytes(upstream.lastBody, "provider").Exists())
}

// OpenRouter 平台账号不填 api_base_urls 时走平台默认地址：三种入站各落到 OpenRouter 原生端点，
// 且都带上供应商路由（误填 /api/v1 的 Anthropic 地址也不会拼出 /v1/v1/messages）。
func TestOpenRouterPlatformDefaultEndpointsCarryProviderRouting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newAccount := func(extraCreds map[string]any) *Account {
		account := openRouterAdaptiveTestAccount()
		account.Platform = PlatformOpenRouter
		delete(account.Credentials, "api_base_urls")
		for k, v := range extraCreds {
			account.Credentials[k] = v
		}
		return account
	}

	cases := []struct {
		name    string
		account *Account
		path    string
		body    string
		wantURL string
		call    func(svc *OpenAIGatewayService, c *gin.Context, account *Account, body []byte) error
	}{
		{"chat completions", newAccount(nil), "/v1/chat/completions",
			`{"model":"z-ai/glm-5.3-flash","messages":[{"role":"user","content":"hello"}],"stream":false}`,
			"https://openrouter.ai/api/v1/chat/completions",
			func(svc *OpenAIGatewayService, c *gin.Context, account *Account, body []byte) error {
				_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
				return err
			}},
		{"responses", newAccount(nil), "/v1/responses",
			`{"model":"z-ai/glm-5.3-flash","input":"hello","max_output_tokens":32,"stream":false}`,
			"https://openrouter.ai/api/v1/responses",
			func(svc *OpenAIGatewayService, c *gin.Context, account *Account, body []byte) error {
				_, err := svc.Forward(context.Background(), c, account, body)
				return err
			}},
		{"messages", newAccount(nil), "/v1/messages",
			`{"model":"z-ai/glm-5.3-flash","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":false}`,
			"https://openrouter.ai/api/v1/messages",
			func(svc *OpenAIGatewayService, c *gin.Context, account *Account, body []byte) error {
				_, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
				return err
			}},
		{"messages with /api/v1 anthropic base", newAccount(map[string]any{
			"api_base_urls": map[string]any{APIProtocolAnthropic: "https://openrouter.ai/api/v1"},
		}), "/v1/messages",
			`{"model":"z-ai/glm-5.3-flash","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":false}`,
			"https://openrouter.ai/api/v1/messages",
			func(svc *OpenAIGatewayService, c *gin.Context, account *Account, body []byte) error {
				_, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
				return err
			}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			body := []byte(tt.body)
			require.Error(t, tt.call(svc, adaptiveProtocolTestContext(tt.path, body), tt.account, body))
			require.Equal(t, tt.wantURL, upstream.lastReq.URL.String())
			requireOpenRouterProviderOrder(t, upstream.lastBody)
		})
	}
}
