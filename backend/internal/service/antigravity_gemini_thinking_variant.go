package service

import (
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
)

// Antigravity 上游的 Gemini 模型目录只有 gemini-3.8-flash-low / -medium / -high / -tiered
// 这类带后缀的变体，裸名（gemini-3.8-flash）直接转发会被上游以 404
// "Requested entity was not found." 拒绝；思考深度也只能靠变体表达——
// Chat Completions / Responses / Messages 转 Gemini 时 effort 不会进入 generationConfig。
//
// 这里让客户端用"裸名 + 协议自带的思考深度参数"调用，由网关按参数挑变体：
//   - Gemini 原生：generationConfig.thinkingConfig 的 thinkingLevel / thinkingBudget
//   - Chat Completions / Responses：reasoning_effort / reasoning.effort
//   - Anthropic Messages：output_config.effort，其次 thinking（budget_tokens / adaptive / disabled）
//
// 未携带任何思考参数 → high（与 Gemini 3 系列默认开启动态思考一致）。
// 选中的后缀在映射表里不存在时按 high → medium → low → tiered 的顺序降级到存在的变体。
// 客户端直接写带后缀的变体名时原样处理，不做推导。

var geminiThinkingVariantSuffixes = []string{"-low", "-medium", "-high", "-tiered"}

const (
	geminiThinkingBudgetLowMax    = 1024
	geminiThinkingBudgetMediumMax = 8192
)

type geminiThinkingConfigProbe struct {
	GenerationConfig struct {
		ThinkingConfig *struct {
			ThinkingBudget *json.Number `json:"thinkingBudget"`
			ThinkingLevel  string       `json:"thinkingLevel"`
		} `json:"thinkingConfig"`
	} `json:"generationConfig"`
}

// hasGeminiThinkingVariantSuffix 判断模型名是否已经带了思考深度后缀。
func hasGeminiThinkingVariantSuffix(model string) bool {
	for _, suffix := range geminiThinkingVariantSuffixes {
		if strings.HasSuffix(model, suffix) {
			return true
		}
	}
	return false
}

// trimGeminiThinkingVariantSuffix 返回 Gemini 思考深度变体对应的裸名
// （gemini-3.8-flash-high → gemini-3.8-flash），其他模型名原样返回。
func trimGeminiThinkingVariantSuffix(model string) string {
	if !strings.HasPrefix(strings.ToLower(model), "gemini-") {
		return model
	}
	for _, suffix := range geminiThinkingVariantSuffixes {
		if len(model) > len(suffix) && strings.EqualFold(model[len(model)-len(suffix):], suffix) {
			return model[:len(model)-len(suffix)]
		}
	}
	return model
}

// geminiThinkingLevelFromEffort 把 OpenAI reasoning effort / Anthropic output_config.effort
// 归到 low/medium/high；无法识别时返回 ""。上游没有"无思考"变体，none/minimal 取最浅档。
func geminiThinkingLevelFromEffort(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "none", "minimal", "low":
		return "low"
	case "medium":
		return "medium"
	case "high", "xhigh", "max":
		return "high"
	}
	return ""
}

// geminiThinkingLevelFromBudget 把思考 token 预算归到 low/medium/high。
// 负数（Gemini 的 -1 动态思考）视为 high；0（关闭思考）取最浅档。
func geminiThinkingLevelFromBudget(budget float64) string {
	switch {
	case budget < 0:
		return "high"
	case budget <= geminiThinkingBudgetLowMax:
		return "low"
	case budget <= geminiThinkingBudgetMediumMax:
		return "medium"
	default:
		return "high"
	}
}

// geminiThinkingLevelFromBody 从 Gemini 原生请求体推导期望的思考档位（low/medium/high）。
// 解析失败或未携带 thinkingConfig 时返回 "high"。
func geminiThinkingLevelFromBody(body []byte) string {
	if len(body) == 0 {
		return "high"
	}
	var probe geminiThinkingConfigProbe
	if err := json.Unmarshal(body, &probe); err != nil || probe.GenerationConfig.ThinkingConfig == nil {
		return "high"
	}
	tc := probe.GenerationConfig.ThinkingConfig
	switch strings.ToLower(strings.TrimSpace(tc.ThinkingLevel)) {
	case "low":
		return "low"
	case "medium":
		return "medium"
	case "high":
		return "high"
	}
	if tc.ThinkingBudget == nil {
		return "high"
	}
	budget, err := tc.ThinkingBudget.Float64()
	if err != nil {
		return "high"
	}
	return geminiThinkingLevelFromBudget(budget)
}

// geminiThinkingLevelFromOpenAIBody 从 Chat Completions / Responses 请求体推导思考档位，
// 依次读 reasoning.effort、reasoning_effort；未携带或无法识别时返回 ""。
func geminiThinkingLevelFromOpenAIBody(body []byte) string {
	raw := gjson.GetBytes(body, "reasoning.effort").String()
	if strings.TrimSpace(raw) == "" {
		raw = gjson.GetBytes(body, "reasoning_effort").String()
	}
	return geminiThinkingLevelFromEffort(raw)
}

// geminiThinkingLevelFromClaudeBody 从 Anthropic Messages 请求体推导思考档位：
// output_config.effort 优先，其次 thinking；都未携带时返回 ""。
func geminiThinkingLevelFromClaudeBody(body []byte) string {
	if level := geminiThinkingLevelFromEffort(gjson.GetBytes(body, "output_config.effort").String()); level != "" {
		return level
	}
	thinking := gjson.GetBytes(body, "thinking")
	switch strings.ToLower(strings.TrimSpace(thinking.Get("type").String())) {
	case "disabled":
		return "low"
	case "enabled":
		if budget := thinking.Get("budget_tokens"); budget.Exists() && budget.Float() > 0 {
			return geminiThinkingLevelFromBudget(budget.Float())
		}
		return "high"
	case "adaptive":
		return "high"
	}
	return ""
}

// resolveGeminiThinkingVariant 为裸 Gemini 模型名挑选账号映射表里存在的思考深度变体。
// level 为期望档位（low/medium/high），空串按 high 处理。
// 返回 (映射后的上游模型名, 是否命中)。未命中时调用方应回退到常规 getMappedModel 流程。
func resolveGeminiThinkingVariant(account *Account, requestedModel string, level string) (string, bool) {
	if account == nil {
		return "", false
	}
	model := strings.TrimSpace(strings.TrimPrefix(requestedModel, "models/"))
	if !strings.HasPrefix(model, "gemini-") || hasGeminiThinkingVariantSuffix(model) {
		return "", false
	}
	mapping := account.GetModelMapping()
	if len(mapping) == 0 {
		return "", false
	}
	// 裸名映射到别的模型（如 gemini-3.8-flash → gemini-3.8-flash-tiered）是用户明确指定的目标，
	// 尊重现有配置不做推导。映射到自己（原样透传）不算：上游目录里没有裸名，这条透传
	// 只会 404；而且 DefaultAntigravityModelMapping 和 ensureAntigravityDefaultPassthroughs
	// 都会写入这条自映射，后台按默认表创建的账号 credentials 里也带着它，并非用户意图。
	if mapped, matched := resolveRequestedModelInMapping(mapping, model); matched && strings.TrimSpace(mapped) != model {
		return "", false
	}

	preferred := strings.TrimSpace(level)
	if preferred == "" {
		preferred = "high"
	}
	order := []string{preferred}
	for _, fallback := range []string{"high", "medium", "low", "tiered"} {
		if fallback != preferred {
			order = append(order, fallback)
		}
	}
	for _, candidateLevel := range order {
		candidate := model + "-" + candidateLevel
		if mapped, matched := resolveRequestedModelInMapping(mapping, candidate); matched && strings.TrimSpace(mapped) != "" {
			return mapped, true
		}
	}
	return "", false
}

// mapAntigravityModelWithThinkingLevel 先按思考档位为裸 Gemini 名挑变体，未命中时回退常规映射。
// 第二个返回值表示是否走了变体推导。
func (s *AntigravityGatewayService) mapAntigravityModelWithThinkingLevel(account *Account, requestedModel string, level string) (string, bool) {
	if mapped, ok := resolveGeminiThinkingVariant(account, requestedModel, level); ok {
		return mapped, true
	}
	return s.getMappedModel(account, requestedModel), false
}
