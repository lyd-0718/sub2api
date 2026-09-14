package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// cnBalanceURLUpstream 按请求路径返回不同响应（OpenRouter /credits 与 /key 形状不同）。
type cnBalanceURLUpstream struct {
	responses map[string]cnBalancePathResponse
	requested []string
}

type cnBalancePathResponse struct {
	statusCode int
	body       string
}

func (u *cnBalanceURLUpstream) Do(
	req *http.Request,
	_ string,
	_ int64,
	_ int,
) (*http.Response, error) {
	u.requested = append(u.requested, req.URL.Path)
	resp, ok := u.responses[req.URL.Path]
	if !ok {
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
	}
	return &http.Response{
		StatusCode: resp.statusCode,
		Body:       io.NopCloser(strings.NewReader(resp.body)),
		Header:     make(http.Header),
	}, nil
}

func (u *cnBalanceURLUpstream) DoWithTLS(
	req *http.Request,
	proxyURL string,
	accountID int64,
	accountConcurrency int,
	_ *tlsfingerprint.Profile,
) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func newOpenRouterBalanceProbeAccount() *Account {
	return &Account{
		ID:       77,
		Platform: PlatformDeepseek,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Credentials: map[string]any{
			"account_mode": AccountModePayG,
			"api_key":      "sk-or-v1-test",
			"base_url":     "https://openrouter.ai/api/v1",
			"api_protocol": APIProtocolAdaptive,
			"api_base_urls": map[string]any{
				APIProtocolChatCompletions: "https://openrouter.ai/api/v1",
				APIProtocolAnthropic:       "https://openrouter.ai/api",
				APIProtocolResponses:       "https://openrouter.ai/api/v1",
			},
		},
	}
}

func TestCNProviderBalanceService_OpenRouterBalanceURL(t *testing.T) {
	account := newOpenRouterBalanceProbeAccount()
	require.Equal(t, "https://openrouter.ai/api/v1/credits", cnBalanceURL(account))

	// 非 OpenRouter 的 deepseek relay 保持官方路径（回归）。
	relay := newDeepSeekBalanceProbeAccount()
	require.Equal(t, "https://relay.example.com/user/balance", cnBalanceURL(relay))

	// host 子串/相似域名不得被误判为 OpenRouter（adaptive 账号取 api_base_urls 的
	// chat_completions 地址，两个字段都要改）。
	lookalike := newOpenRouterBalanceProbeAccount()
	lookalike.Credentials["base_url"] = "https://openrouter.ai.evil.example/api/v1"
	lookalike.Credentials["api_base_urls"] = map[string]any{
		APIProtocolChatCompletions: "https://openrouter.ai.evil.example/api/v1",
	}
	require.Equal(t, "https://openrouter.ai.evil.example/api/v1/user/balance", cnBalanceURL(lookalike))
}

func TestCNProviderBalanceService_OpenRouterCreditsAndKeyLimit(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newOpenRouterBalanceProbeAccount()}
	upstream := &cnBalanceURLUpstream{responses: map[string]cnBalancePathResponse{
		"/api/v1/credits": {statusCode: http.StatusOK, body: `{"data":{"total_credits":794,"total_usage":287.138756042}}`},
		"/api/v1/key": {statusCode: http.StatusOK, body: `{"data":{"limit":500,"limit_remaining":499.99957464}}`},
	}}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.True(t, result.Success)
	require.True(t, result.Available)
	require.Equal(t, "USD", result.Currency)
	// 有效余额取两者较小值（key 限额在前），供阈值停调使用。
	require.InDelta(t, 499.99957464, result.Balance, 1e-9)
	require.Len(t, result.Balances, 2)
	require.Equal(t, cnOpenRouterBalanceLabelKey, result.Balances[0].Label)
	require.InDelta(t, 499.99957464, result.Balances[0].Balance, 1e-9)
	require.Equal(t, cnOpenRouterBalanceLabelAccount, result.Balances[1].Label)
	require.InDelta(t, 794-287.138756042, result.Balances[1].Balance, 1e-6)
	require.Equal(t, []string{"/api/v1/credits", "/api/v1/key"}, upstream.requested)
	require.Len(t, repo.extraWrites, 1)
	entries, ok := repo.extraWrites[0]["deepseek_balances"].([]any)
	require.True(t, ok)
	require.Len(t, entries, 2)
	require.Equal(t, cnOpenRouterBalanceLabelKey, entries[0].(map[string]any)["label"])
}

func TestCNProviderBalanceService_OpenRouterAccountBalanceWithoutKeyLimit(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newOpenRouterBalanceProbeAccount()}
	upstream := &cnBalanceURLUpstream{responses: map[string]cnBalancePathResponse{
		"/api/v1/credits": {statusCode: http.StatusOK, body: `{"data":{"total_credits":794,"total_usage":287.138756042}}`},
		// limit=null 表示该 key 未设限额：不追加 key 明细。
		"/api/v1/key": {statusCode: http.StatusOK, body: `{"data":{"limit":null,"limit_remaining":null}}`},
	}}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.True(t, result.Success)
	require.True(t, result.Available)
	require.InDelta(t, 794-287.138756042, result.Balance, 1e-6)
	require.Len(t, result.Balances, 1)
	require.Equal(t, cnOpenRouterBalanceLabelAccount, result.Balances[0].Label)
}

func TestCNProviderBalanceService_OpenRouterKeyProbeFailureKeepsAccountBalance(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newOpenRouterBalanceProbeAccount()}
	upstream := &cnBalanceURLUpstream{responses: map[string]cnBalancePathResponse{
		"/api/v1/credits": {statusCode: http.StatusOK, body: `{"data":{"total_credits":10,"total_usage":4}}`},
		"/api/v1/key":     {statusCode: http.StatusInternalServerError, body: `{"error":"boom"}`},
	}}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.True(t, result.Success, "key 限额探测失败不应让整次余额探测失败")
	require.InDelta(t, 6, result.Balance, 1e-9)
	require.Len(t, result.Balances, 1)
}

func TestCNProviderBalanceService_OpenRouterCreditsExhaustedMarksUnavailable(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newOpenRouterBalanceProbeAccount()}
	upstream := &cnBalanceURLUpstream{responses: map[string]cnBalancePathResponse{
		"/api/v1/credits": {statusCode: http.StatusOK, body: `{"data":{"total_credits":100,"total_usage":150}}`},
		"/api/v1/key":     {statusCode: http.StatusOK, body: `{"data":{"limit":500,"limit_remaining":400}}`},
	}}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.True(t, result.Success)
	require.False(t, result.Available, "账户余额为负必须标记不健康以触发停调")
	require.Less(t, result.Balance, 0.0)
}

func TestCNProviderBalanceService_OpenRouterInvalidPayloadDoesNotPersist(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantError string
	}{
		{
			name:      "missing total credits",
			body:      `{"data":{"total_usage":12}}`,
			wantError: "missing total_credits",
		},
		{
			name:      "no credit data (byok)",
			body:      `{"data":{"total_credits":0,"total_usage":0}}`,
			wantError: "no credit data",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &cnBalanceProbeRepo{account: newOpenRouterBalanceProbeAccount()}
			upstream := &cnBalanceURLUpstream{responses: map[string]cnBalancePathResponse{
				"/api/v1/credits": {statusCode: http.StatusOK, body: tt.body},
			}}
			svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

			result, err := svc.QueryBalance(context.Background(), repo.account.ID)

			require.NoError(t, err)
			require.False(t, result.Success)
			require.Contains(t, result.Error, tt.wantError)
			require.Empty(t, repo.extraWrites, "无效载荷不得用合成零值落库")
		})
	}
}
