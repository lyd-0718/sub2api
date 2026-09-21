package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/tidwall/gjson"
)

// OpenRouter 兼容：DeepSeek 平台账号的 base_url 指向 OpenRouter 时，余额端点
// 与官方 DeepSeek 不同（后者为 {base}/user/balance）：
//
//   - 账户余额：GET {base}/credits → data.total_credits / data.total_usage
//   - 单 key 限额：GET {base}/key   → data.limit / data.limit_remaining（null = 未设限额）
//
// 有效余额取两者较小值：key 限额打满时账户里仍有余额，请求同样会被上游拒绝。
//
// 本文件为二开模块，避免在上游 cn_provider_balance_service.go 内做大段改动
// （只保留：明细 Label 字段、cnBalanceURL 的 openrouter 分支、解析分流三处小改动）。
const (
	cnOpenRouterBalanceLabelAccount = "account"
	cnOpenRouterBalanceLabelKey     = "key"
)

// isOpenRouterBalanceAccount 判定 deepseek 平台账号是否接的是 OpenRouter。
func isOpenRouterBalanceAccount(account *Account) bool {
	if account == nil || account.Platform != PlatformDeepseek {
		return false
	}
	return isOpenRouterBalanceBase(strings.TrimRight(strings.TrimSpace(account.GetOpenAIFormatBaseURL()), "/"))
}

// isOpenRouterBalanceBase 严格 host 判定（对齐 isOllamaCloudBaseURL 先例）：
// https + host 精确等于 openrouter.ai，避免子串匹配把第三方中转误判为 OpenRouter
// 而把 API key 发往错误的主机路径。
func isOpenRouterBalanceBase(base string) bool {
	if base == "" {
		return false
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return false
	}
	return strings.EqualFold(parsed.Host, "openrouter.ai")
}

// buildOpenRouterBalanceEntries 解析 /credits 响应并追加 /key 限额明细。
// 条目按“有效余额（较小值）在前”排序：共享持久化代码以 entries[0] 为主余额，
// 该值驱动阈值停调。available 表示有效余额是否为正。
//
// 契约（与官方 DeepSeek 解析一致）：解析失败返回 error → 调用方置
// Success=false 且不落库，避免用合成零值覆盖真实余额或误停账号。
func (s *CNProviderBalanceService) buildOpenRouterBalanceEntries(
	ctx context.Context,
	account *Account,
	creditsBody []byte,
) ([]CNProviderBalanceEntry, bool, error) {
	totalCreditsRaw := gjson.GetBytes(creditsBody, "data.total_credits")
	totalUsageRaw := gjson.GetBytes(creditsBody, "data.total_usage")
	totalCredits, creditsOK := cnParseF64(totalCreditsRaw.Value())
	if !totalCreditsRaw.Exists() || !creditsOK {
		return nil, true, errors.New("Invalid balance response: missing total_credits")
	}
	totalUsage, usageOK := cnParseF64(totalUsageRaw.Value())
	if !totalUsageRaw.Exists() || !usageOK {
		totalUsage = 0
	}
	// BYOK / 无充值记录的账号：credits 与 usage 均无数据时视为“无余额数据”，
	// 不落库不改状态（BYOK 账号余额为 0 属正常，误停会打断它的全部流量）。
	if totalCredits <= 0 && totalUsage <= 0 {
		return nil, true, errors.New("Invalid balance response: no credit data")
	}

	entries := []CNProviderBalanceEntry{{
		Currency: "USD",
		Balance:  totalCredits - totalUsage,
		Label:    cnOpenRouterBalanceLabelAccount,
	}}
	if keyLimit, ok := s.fetchOpenRouterKeyLimit(ctx, account); ok {
		entries = append(entries, CNProviderBalanceEntry{
			Currency: "USD",
			Balance:  keyLimit,
			Label:    cnOpenRouterBalanceLabelKey,
		})
	}
	if len(entries) > 1 && entries[1].Balance < entries[0].Balance {
		entries[0], entries[1] = entries[1], entries[0]
	}
	return entries, entries[0].Balance > 0, nil
}

// fetchOpenRouterKeyLimit 读取单 key 剩余限额（GET {base}/key）。
// 失败或未设限额时返回 ok=false：key 限额是补充信息，其探测失败不应让整次
// 余额探测失败（账户余额仍然可用）。
func (s *CNProviderBalanceService) fetchOpenRouterKeyLimit(ctx context.Context, account *Account) (float64, bool) {
	body, err := s.fetchOpenRouterJSON(ctx, account, "/key")
	if err != nil {
		slog.Warn("cn_openrouter_key_limit_probe_failed",
			"account_id", account.ID,
			"error", err,
		)
		return 0, false
	}
	limit := gjson.GetBytes(body, "data.limit")
	if !limit.Exists() || limit.Type == gjson.Null {
		return 0, false // 未设置单 key 限额
	}
	remaining, ok := cnParseF64(gjson.GetBytes(body, "data.limit_remaining").Value())
	if !ok {
		return 0, false
	}
	return remaining, true
}

// fetchOpenRouterJSON 复刻 queryBalanceForAccount 的出站路径（同样的出站 URL
// 安全校验 / 鉴权头 / 账号级 header 覆写 / 代理 / 出站传输），供次要端点复用。
func (s *CNProviderBalanceService) fetchOpenRouterJSON(ctx context.Context, account *Account, path string) ([]byte, error) {
	apiKey := strings.TrimSpace(account.GetCNAPIKey())
	if apiKey == "" {
		return nil, errors.New("account api_key is empty")
	}
	base := strings.TrimRight(strings.TrimSpace(account.GetOpenAIFormatBaseURL()), "/")
	targetURL, err := cnValidateProbeURL(s.cfg, base+path)
	if err != nil {
		return nil, fmt.Errorf("probe target rejected by URL security policy: %w", err)
	}

	callCtx, cancel := context.WithTimeout(ctx, cnBalanceUpstreamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	account.ApplyHeaderOverrides(req.Header)

	resp, err := doAccountHTTPUpstream(s.httpUpstream, s.tlsFPProfileService, req, s.resolveProxyURL(ctx, account), account, maxInt(account.Concurrency, 1))
	if err != nil {
		return nil, fmt.Errorf("upstream request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, cnBalanceMaxBodyBytes))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
	}
	return body, nil
}
