package service

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// OpenRouter 供应商路由（二开）：同一模型在 OpenRouter 背后有几十家 provider，默认按价格
// 负载均衡，同一会话前后请求落到不同家会丢失 prompt 缓存。管理端按上游模型为账号配置
// 首选 / 备选 provider，转发前注入 OpenRouter 的 provider.order；allow_fallbacks 固定为
// true——两家都不可用时交还 OpenRouter 自动路由，不因缓存牺牲可用性。
//
// 配置存在 account.extra[openrouter_provider_routing]，由专用接口写入（通用账号编辑保留原值）。

const (
	OpenRouterProviderRoutingExtraKey = "openrouter_provider_routing"

	// openRouterProviderRoutingMaxOrder 每个模型最多指定的 provider 数：首选 + 备选。
	openRouterProviderRoutingMaxOrder = 2
)

// OpenRouterProviderRouting 账号级 OpenRouter 供应商路由配置。
// Models 的 key 是发往 OpenRouter 的上游模型名（账号模型映射之后），value 是 provider slug 顺序。
type OpenRouterProviderRouting struct {
	Enabled bool                `json:"enabled"`
	Models  map[string][]string `json:"models,omitempty"`
}

// OpenRouter provider slug 形如 wafer、sail-research/us、deepinfra/fp8。
var openRouterProviderSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*(/[a-z0-9._-]+)?$`)

// OpenRouter 模型 ID 形如 z-ai/glm-5.3-flash、deepseek/deepseek-v4.1-flash；拼进 endpoints
// 查询路径前必须校验，防止路径穿越。
var openRouterModelIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._:-]+$`)

// isOpenRouterAccount 判断账号的 OpenAI 兼容出站地址是否指向 openrouter.ai（不限平台）。
func isOpenRouterAccount(account *Account) bool {
	if account == nil {
		return false
	}
	return isOpenRouterBalanceBase(strings.TrimRight(strings.TrimSpace(account.GetOpenAIFormatBaseURL()), "/"))
}

// NormalizeOpenRouterProviderRouting 清洗管理端提交的配置：模型名与 slug 去空白、slug 转小写
// 去重，空列表的模型剔除；slug 非法或超过首选 + 备选两家时返回 400。
func NormalizeOpenRouterProviderRouting(in OpenRouterProviderRouting) (OpenRouterProviderRouting, error) {
	out := OpenRouterProviderRouting{Enabled: in.Enabled, Models: map[string][]string{}}
	for rawModel, rawOrder := range in.Models {
		model := strings.TrimSpace(rawModel)
		if model == "" {
			continue
		}
		seen := make(map[string]struct{}, len(rawOrder))
		order := make([]string, 0, len(rawOrder))
		for _, raw := range rawOrder {
			slug := strings.ToLower(strings.TrimSpace(raw))
			if slug == "" {
				continue
			}
			if !openRouterProviderSlugPattern.MatchString(slug) {
				return OpenRouterProviderRouting{}, infraerrors.BadRequest("INVALID_OPENROUTER_PROVIDER", "invalid OpenRouter provider slug: "+raw)
			}
			if _, dup := seen[slug]; dup {
				continue
			}
			seen[slug] = struct{}{}
			order = append(order, slug)
		}
		if len(order) > openRouterProviderRoutingMaxOrder {
			return OpenRouterProviderRouting{}, infraerrors.BadRequest("TOO_MANY_OPENROUTER_PROVIDERS",
				"at most "+strconv.Itoa(openRouterProviderRoutingMaxOrder)+" providers (primary and backup) per model: "+model)
		}
		if len(order) > 0 {
			out.Models[model] = order
		}
	}
	return out, nil
}

// openRouterProviderRoutingFromExtra 读取账号 extra 里的配置；缺失或格式损坏时返回 ok=false。
func openRouterProviderRoutingFromExtra(extra map[string]any) (OpenRouterProviderRouting, bool) {
	raw, ok := extra[OpenRouterProviderRoutingExtraKey]
	if !ok || raw == nil {
		return OpenRouterProviderRouting{}, false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return OpenRouterProviderRouting{}, false
	}
	var cfg OpenRouterProviderRouting
	if err := json.Unmarshal(encoded, &cfg); err != nil {
		return OpenRouterProviderRouting{}, false
	}
	return cfg, true
}

// applyOpenRouterProviderRouting 在发往 openrouter.ai 的请求体里注入 provider 偏好。
// 仅当目标主机是 openrouter.ai、账号启用并为该上游模型配置了顺序、且请求体没有自带
// provider（客户端显式指定优先）时生效；其余情况原样返回。
func applyOpenRouterProviderRouting(account *Account, targetURL string, body []byte) []byte {
	if account == nil || len(body) == 0 || !isOpenRouterBalanceBase(targetURL) {
		return body
	}
	cfg, ok := openRouterProviderRoutingFromExtra(account.Extra)
	if !ok || !cfg.Enabled {
		return body
	}
	order := cfg.Models[strings.TrimSpace(gjson.GetBytes(body, "model").String())]
	if len(order) == 0 || gjson.GetBytes(body, "provider").Exists() {
		return body
	}
	patched, err := sjson.SetBytes(body, "provider", map[string]any{
		"order":           order,
		"allow_fallbacks": true,
	})
	if err != nil {
		return body
	}
	return patched
}

// OpenRouterProviderOption 供管理端下拉框使用的 provider 条目，来自 OpenRouter 公开的
// /models/{id}/endpoints 接口。价格单位为美元 / 百万 token。
type OpenRouterProviderOption struct {
	Slug            string   `json:"slug"`
	Name            string   `json:"name"`
	Quantization    string   `json:"quantization,omitempty"`
	Status          int      `json:"status"`
	UptimeLast1d    *float64 `json:"uptime_last_1d,omitempty"`
	UptimeLast30m   *float64 `json:"uptime_last_30m,omitempty"`
	ContextLength   int64    `json:"context_length,omitempty"`
	PromptPrice     *float64 `json:"prompt_price,omitempty"`
	CompletionPrice *float64 `json:"completion_price,omitempty"`
	CacheReadPrice  *float64 `json:"cache_read_price,omitempty"`
	Discount        float64  `json:"discount,omitempty"`
	SupportsTools   bool     `json:"supports_tools"`
}

// OpenRouterModelProviders 单个上游模型的可选 provider；拉取失败时 Error 非空、Providers 为空。
type OpenRouterModelProviders struct {
	Model     string                     `json:"model"`
	Providers []OpenRouterProviderOption `json:"providers"`
	Error     string                     `json:"error,omitempty"`
}

// OpenRouterProviderRoutingView 管理端读取视图：当前配置 + 账号各上游模型的可选 provider。
type OpenRouterProviderRoutingView struct {
	AccountID int64                      `json:"account_id"`
	Routing   OpenRouterProviderRouting  `json:"routing"`
	Models    []OpenRouterModelProviders `json:"models"`
}

func (s *CNProviderBalanceService) loadOpenRouterAccount(ctx context.Context, accountID int64) (*Account, error) {
	if s == nil || s.accountRepo == nil || s.httpUpstream == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "CN_BALANCE_NOT_CONFIGURED", "cn provider balance service is not configured")
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusNotFound, "OPENROUTER_ACCOUNT_NOT_FOUND", "account not found: %v", err)
	}
	if !isOpenRouterAccount(account) {
		return nil, infraerrors.BadRequest("NOT_OPENROUTER_ACCOUNT", "account base URL is not openrouter.ai")
	}
	return account, nil
}

// GetOpenRouterProviderRouting 返回账号当前配置，以及账号模型映射里每个上游模型的可选 provider。
func (s *CNProviderBalanceService) GetOpenRouterProviderRouting(ctx context.Context, accountID int64) (*OpenRouterProviderRoutingView, error) {
	account, err := s.loadOpenRouterAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	routing, _ := openRouterProviderRoutingFromExtra(account.Extra)
	if routing.Models == nil {
		routing.Models = map[string][]string{}
	}

	// 可配置的模型 = 模型映射的上游目标 ∪ 已配置过的模型（映射删掉后仍能在界面上看到并清理）。
	modelSet := map[string]struct{}{}
	for _, upstream := range account.GetModelMapping() {
		upstream = strings.TrimSpace(upstream)
		if upstream != "" && !strings.Contains(upstream, "*") {
			modelSet[upstream] = struct{}{}
		}
	}
	for model := range routing.Models {
		modelSet[model] = struct{}{}
	}
	models := make([]string, 0, len(modelSet))
	for model := range modelSet {
		models = append(models, model)
	}
	sort.Strings(models)

	result := make([]OpenRouterModelProviders, len(models))
	var wg sync.WaitGroup
	for i, model := range models {
		wg.Add(1)
		go func(i int, model string) {
			defer wg.Done()
			result[i] = s.fetchOpenRouterModelProviders(ctx, account, model)
		}(i, model)
	}
	wg.Wait()

	return &OpenRouterProviderRoutingView{AccountID: account.ID, Routing: routing, Models: result}, nil
}

func (s *CNProviderBalanceService) fetchOpenRouterModelProviders(ctx context.Context, account *Account, model string) OpenRouterModelProviders {
	out := OpenRouterModelProviders{Model: model, Providers: []OpenRouterProviderOption{}}
	if !openRouterModelIDPattern.MatchString(model) {
		out.Error = "unsupported model id"
		return out
	}
	body, err := s.fetchOpenRouterJSON(ctx, account, "/models/"+model+"/endpoints")
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Providers = parseOpenRouterEndpoints(body)
	return out
}

// parseOpenRouterEndpoints 解析 /models/{id}/endpoints 响应，按"状态正常优先、1 天可用率降序"排序。
func parseOpenRouterEndpoints(body []byte) []OpenRouterProviderOption {
	perMillion := func(v gjson.Result) *float64 {
		if !v.Exists() || strings.TrimSpace(v.String()) == "" {
			return nil
		}
		f, err := strconv.ParseFloat(v.String(), 64)
		if err != nil {
			return nil
		}
		f *= 1e6
		return &f
	}
	optionalFloat := func(v gjson.Result) *float64 {
		if !v.Exists() || v.Type == gjson.Null {
			return nil
		}
		f := v.Float()
		return &f
	}

	options := []OpenRouterProviderOption{}
	gjson.GetBytes(body, "data.endpoints").ForEach(func(_, ep gjson.Result) bool {
		slug := strings.TrimSpace(ep.Get("tag").String())
		if slug == "" {
			return true
		}
		supportsTools := false
		ep.Get("supported_parameters").ForEach(func(_, p gjson.Result) bool {
			if p.String() == "tools" {
				supportsTools = true
				return false
			}
			return true
		})
		options = append(options, OpenRouterProviderOption{
			Slug:            slug,
			Name:            ep.Get("provider_name").String(),
			Quantization:    ep.Get("quantization").String(),
			Status:          int(ep.Get("status").Int()),
			UptimeLast1d:    optionalFloat(ep.Get("uptime_last_1d")),
			UptimeLast30m:   optionalFloat(ep.Get("uptime_last_30m")),
			ContextLength:   ep.Get("context_length").Int(),
			PromptPrice:     perMillion(ep.Get("pricing.prompt")),
			CompletionPrice: perMillion(ep.Get("pricing.completion")),
			CacheReadPrice:  perMillion(ep.Get("pricing.input_cache_read")),
			Discount:        ep.Get("pricing.discount").Float(),
			SupportsTools:   supportsTools,
		})
		return true
	})

	uptime := func(o OpenRouterProviderOption) float64 {
		if o.UptimeLast1d == nil {
			return -1
		}
		return *o.UptimeLast1d
	}
	sort.SliceStable(options, func(i, j int) bool {
		if (options[i].Status == 0) != (options[j].Status == 0) {
			return options[i].Status == 0
		}
		return uptime(options[i]) > uptime(options[j])
	})
	return options
}

// UpdateOpenRouterProviderRouting 校验并保存账号的供应商路由配置，返回落库后的配置。
func (s *CNProviderBalanceService) UpdateOpenRouterProviderRouting(ctx context.Context, accountID int64, in OpenRouterProviderRouting) (OpenRouterProviderRouting, error) {
	if _, err := s.loadOpenRouterAccount(ctx, accountID); err != nil {
		return OpenRouterProviderRouting{}, err
	}
	normalized, err := NormalizeOpenRouterProviderRouting(in)
	if err != nil {
		return OpenRouterProviderRouting{}, err
	}
	if err := s.accountRepo.UpdateExtra(ctx, accountID, map[string]any{OpenRouterProviderRoutingExtraKey: normalized}); err != nil {
		return OpenRouterProviderRouting{}, err
	}
	return normalized, nil
}
