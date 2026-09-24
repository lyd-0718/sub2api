package service

import (
	"context"
	"strings"
)

// OpenRouter 平台（二开）：OpenRouter 聚合网关的 API Key 账号，一个账号统一管理 OpenRouter 上的
// 所有模型。走 OpenAI 网关、支持 adaptive 多协议（Chat Completions / Responses / Anthropic），
// 只有按量付费；不属于国产供应商（IsCNProvider），因此不会套用 Coding Plan 额度、国产 403/429
// 分类、阈值停调、账号归队以及 DeepSeek 专用的请求改写。

// OpenRouter 默认 base_url：Chat Completions / Responses / models 共用 /api/v1；
// Anthropic 基址不含 /v1——nativeAnthropicTargetURL 会再拼 /v1/messages。
const (
	DefaultOpenRouterBaseURL          = "https://openrouter.ai/api/v1"
	DefaultOpenRouterAnthropicBaseURL = "https://openrouter.ai/api"
)

// IsOpenRouter 报告 platform 是否为 OpenRouter。
func IsOpenRouter(platform string) bool {
	return platform == PlatformOpenRouter
}

// IsOpenRouter 报告账号是否属于 OpenRouter 平台。
func (a *Account) IsOpenRouter() bool {
	return a != nil && a.Platform == PlatformOpenRouter
}

// defaultOpenRouterModelIDs 是 OpenRouter 账号未配置模型映射时对外列出的默认模型
// （与前端 useModelWhitelist.ts 的 openrouterModels 保持一致；完整目录可在账号弹窗里同步上游模型）。
var defaultOpenRouterModelIDs = []string{
	"z-ai/glm-5.3", "z-ai/glm-5.3-flash", "z-ai/glm-5.3-flashx", "z-ai/glm-5.3-prime", "z-ai/glm-5.2",
	"deepseek/deepseek-v4.1-flash", "deepseek/deepseek-v4-pro", "deepseek/deepseek-v4-flash", "deepseek/deepseek-v3.2",
	"moonshotai/kimi-k3", "moonshotai/kimi-k2.7-code", "moonshotai/kimi-k2.6",
	"qwen/qwen3.8-max-0902", "qwen/qwen3.8-flash",
	"minimax/minimax-m3", "minimax/minimax-m2.7",
	"openai/gpt-6-sol", "openai/gpt-6-luna",
	"anthropic/claude-opus-5.5", "anthropic/claude-sonnet-5",
	"google/gemini-3.8-flash",
	"x-ai/grok-4.7",
}

// DefaultOpenRouterModelIDs 返回 OpenRouter 默认模型列表的副本。
func DefaultOpenRouterModelIDs() []string {
	return append([]string(nil), defaultOpenRouterModelIDs...)
}

// ---- composite 分组的 OpenRouter 跨平台账号池（二开） ----
//
// composite 分组按"哪个平台的账号显式映射了该模型"决定目标平台，多个平台同时声明同一模型时
// 判为歧义并拒绝。OpenRouter 聚合了各家模型，常与原厂或其他中转同时提供同一个模型（例如
// deepseek/deepseek-v4.1-flash 同时挂在 OpenRouter 与 DeepSeek 中转账号上）。规则：
//   - 模型归属：OpenRouter 与恰好一个其他平台同时声明时，归属那个平台；只有 OpenRouter 声明时归属 OpenRouter。
//   - 调度：显式映射了本次模型的 OpenRouter 账号加入目标平台的账号池，与原平台账号一起按
//     优先级 / 负载调度、互为故障转移——与迁移前这些账号挂在同一平台下时的行为一致。

// compositePoolModelFromContext 返回 composite 分组本次请求交给账号的模型名（显式路由改写后的
// 上游模型优先，其次为客户端请求的公开模型）；非 composite 请求返回空串。
func compositePoolModelFromContext(ctx context.Context) string {
	if _, composite := ResolvedTargetPlatformFromContext(ctx); !composite {
		return ""
	}
	if model, ok := ResolvedUpstreamModelFromContext(ctx); ok {
		return model
	}
	model, _ := RequestedPublicModelFromContext(ctx)
	return model
}

// openRouterAccountJoinsPlatformPool 报告 OpenRouter 账号能否作为 platform 账号池的一员参与本次调度：
// 仅限 composite 分组请求，且账号模型映射显式声明了本次请求的模型。
func openRouterAccountJoinsPlatformPool(ctx context.Context, account *Account, platform, requestedModel string) bool {
	if account == nil || !account.IsOpenRouter() || platform == PlatformOpenRouter {
		return false
	}
	if _, composite := ResolvedTargetPlatformFromContext(ctx); !composite {
		return false
	}
	model := strings.TrimSpace(requestedModel)
	if model == "" {
		model = compositePoolModelFromContext(ctx)
	}
	return model != "" && explicitModelMappingClaims(*account, model)
}

// openAIAccountPlatformMatches 是 OpenAI 网关调度的平台匹配判定：账号平台与请求平台一致，
// 或 OpenRouter 账号按上述规则加入该平台的账号池。
func openAIAccountPlatformMatches(ctx context.Context, account *Account, platform, requestedModel string) bool {
	if account == nil {
		return false
	}
	platform = NormalizeOpenAICompatiblePlatform(platform)
	if account.Platform == platform {
		return true
	}
	return openRouterAccountJoinsPlatformPool(ctx, account, platform, requestedModel)
}

// dropOpenRouterFromSharedModelOwnership 在模型同时被 OpenRouter 与其他平台声明时移除 OpenRouter，
// 让模型归属到那个平台（OpenRouter 账号随后经 openAIAccountPlatformMatches 加入其账号池）。
func dropOpenRouterFromSharedModelOwnership(platforms map[string]struct{}) {
	if len(platforms) > 1 {
		delete(platforms, PlatformOpenRouter)
	}
}

// appendOpenRouterPoolAccounts 把 composite 分组里显式映射了本次模型的 OpenRouter 账号追加到
// platform 的候选账号列表。
func (s *OpenAIGatewayService) appendOpenRouterPoolAccounts(ctx context.Context, groupID *int64, platform string, accounts []Account) []Account {
	if s == nil || groupID == nil || platform == PlatformOpenRouter {
		return accounts
	}
	model := compositePoolModelFromContext(ctx)
	if model == "" {
		return accounts
	}
	var pool []Account
	var err error
	if s.schedulerSnapshot != nil {
		pool, _, err = s.schedulerSnapshot.ListSchedulableAccounts(ctx, groupID, PlatformOpenRouter, false)
	} else if s.accountRepo != nil {
		pool, err = s.accountRepo.ListSchedulableByGroupIDAndPlatform(ctx, *groupID, PlatformOpenRouter)
	}
	if err != nil {
		return accounts
	}
	for _, account := range pool {
		if explicitModelMappingClaims(account, model) {
			accounts = append(accounts, account)
		}
	}
	return accounts
}
