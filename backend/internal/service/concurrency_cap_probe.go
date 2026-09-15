package service

// 并发上限回升探测器（PLAN-cn-cap-v5 §4.1 / §4.2）。
//
// 探测必须在"独占上游并发"的前提下进行：受限账号此刻仍在服务用户流量，若直接叠加
// 探测流，上游看到的并发数会超过 cap → 必然返回"并发受限" → 探测恒失败 → flap 三连
// → 熔断，cap 永远停在 1。因此流程为：
//
//	占满当前 cap 条真实槽位（占满后调度器不会再选它，上游只见探测流）
//	  → 并发发"目标车道数"条流式探测请求 → 判定三档 → 立即释放占位
//
// 占不到槽位说明此刻有用户流量在跑：按 probe_drain_timeout 间隔重试，本轮总预算
// 60s，超时放弃本轮（返回 capStepOutcomeDeferred，不计 flap、不判失败，只重排）。
//
// 零副作用（PLAN §4.2）：出站使用 HTTPUpstream.Do 原语（无计费、用量、健康上报钩子），
// 出站 URL 过 cnValidateProbeURL，探测 403 不经 handle403，不写 usage_logs，
// 不触发限流/停车副作用。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

const (
	// concurrencyCapProbeDrainBudget 是单轮"占满槽位"的总预算（PLAN §4.1）。
	// 单轮预算 = leader_lock_ttl(90s) - 15s 安全边 = 75s；单账号最坏 = 占槽预算 + 探测
	// 超时(30s)。占槽预算取 40s（4 次 10s 重试），最坏 70s 落在单轮预算内——占不满
	// 就推迟到下一轮，绝不让探测流在 ctx 到期的边缘起跑。
	concurrencyCapProbeDrainBudget = 40 * time.Second
	// concurrencyCapProbeMaxBodyBytes 限制单条探测响应体读取量。探测流必须读全才能看到
	// 流内 error 事件（与生产 403 同源的判定依据），但读取量要有上限。
	concurrencyCapProbeMaxBodyBytes = 64 << 10
	// concurrencyCapProbeFallbackMaxTokens 是上游拒绝极小 max_tokens 时的兼容值（PLAN §八：1，兼容失败再 4）。
	concurrencyCapProbeFallbackMaxTokens = 4
	// concurrencyCapProbeDefaultEndpoint 是默认探测路径，与生产原生 Anthropic 转发
	// （nativeAnthropicTargetURL）同源。
	concurrencyCapProbeDefaultEndpoint = "/v1/messages"
	// concurrencyCapProbePrompt 是探测请求体内容，无业务语义，仅用于触发一次最小流式生成。
	concurrencyCapProbePrompt = "ping"
)

// capLaneResult 是单条探测流的判定结果。
type capLaneResult int

const (
	// capLaneResultInconclusive：超时 / 5xx / 网络错误 / 非并发类上游错误。不推进、不计 flap。
	capLaneResultInconclusive capLaneResult = iota
	// capLaneResultSuccess：上游正常受理并完成本次流式响应。
	capLaneResultSuccess
	// capLaneResultLimited：显式"并发受限"（契约 2 分类器判定），唯一计入失败的结果。
	capLaneResultLimited
)

// ConcurrencyCapProbeMetrics 是进程内原子计数器集合（模式参考 securityaudit.NewAtomicMetrics）。
// 指标口径见 PLAN §八：cap_probe_total{target,result} / cap_raise_total / cap_flap_total / cap_fuse_total。
type ConcurrencyCapProbeMetrics struct {
	mu          sync.Mutex
	probeTotals map[concurrencyCapProbeMetricKey]*atomic.Int64
	raiseTotal  atomic.Int64
	flapTotal   atomic.Int64
	fuseTotal   atomic.Int64
}

type concurrencyCapProbeMetricKey struct {
	target int
	result string
}

const (
	concurrencyCapProbeResultPass         = "pass"
	concurrencyCapProbeResultLimited      = "limited"
	concurrencyCapProbeResultInconclusive = "inconclusive"
)

// NewConcurrencyCapProbeMetrics 创建进程内指标集合。
func NewConcurrencyCapProbeMetrics() *ConcurrencyCapProbeMetrics {
	return &ConcurrencyCapProbeMetrics{probeTotals: make(map[concurrencyCapProbeMetricKey]*atomic.Int64)}
}

// IncProbe 记录一次探测结果（result ∈ pass / limited / inconclusive）。
func (m *ConcurrencyCapProbeMetrics) IncProbe(target int, result string) {
	if m == nil {
		return
	}
	key := concurrencyCapProbeMetricKey{target: target, result: result}
	m.mu.Lock()
	counter, ok := m.probeTotals[key]
	if !ok {
		counter = &atomic.Int64{}
		m.probeTotals[key] = counter
	}
	m.mu.Unlock()
	counter.Add(1)
}

// IncRaise 记录一次成功的并发上限回升。
func (m *ConcurrencyCapProbeMetrics) IncRaise() {
	if m == nil {
		return
	}
	m.raiseTotal.Add(1)
}

// IncFlap 记录一次 flap 事件（探测失败 / 回升后 72h 内再次撞并发 403）。
func (m *ConcurrencyCapProbeMetrics) IncFlap() {
	if m == nil {
		return
	}
	m.flapTotal.Add(1)
}

// IncFuse 记录一次熔断（滚动 7 天 flap 数首次达到阈值）。
func (m *ConcurrencyCapProbeMetrics) IncFuse() {
	if m == nil {
		return
	}
	m.fuseTotal.Add(1)
}

// ConcurrencyCapProbeMetricsSnapshot 是指标快照，供告警/运维导出使用。
type ConcurrencyCapProbeMetricsSnapshot struct {
	ProbeTotal []ConcurrencyCapProbeCount
	RaiseTotal int64
	FlapTotal  int64
	FuseTotal  int64
}

// ConcurrencyCapProbeCount 是 cap_probe_total 的一个标签组合计数。
type ConcurrencyCapProbeCount struct {
	Target int
	Result string
	Count  int64
}

// Snapshot 返回当前指标（按 target、result 排序，便于稳定输出与断言）。
func (m *ConcurrencyCapProbeMetrics) Snapshot() ConcurrencyCapProbeMetricsSnapshot {
	if m == nil {
		return ConcurrencyCapProbeMetricsSnapshot{}
	}
	snapshot := ConcurrencyCapProbeMetricsSnapshot{
		RaiseTotal: m.raiseTotal.Load(),
		FlapTotal:  m.flapTotal.Load(),
		FuseTotal:  m.fuseTotal.Load(),
	}
	m.mu.Lock()
	keys := make([]concurrencyCapProbeMetricKey, 0, len(m.probeTotals))
	for key := range m.probeTotals {
		keys = append(keys, key)
	}
	counters := make(map[concurrencyCapProbeMetricKey]*atomic.Int64, len(m.probeTotals))
	for key, counter := range m.probeTotals {
		counters[key] = counter
	}
	m.mu.Unlock()
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].target != keys[j].target {
			return keys[i].target < keys[j].target
		}
		return keys[i].result < keys[j].result
	})
	for _, key := range keys {
		snapshot.ProbeTotal = append(snapshot.ProbeTotal, ConcurrencyCapProbeCount{
			Target: key.target,
			Result: key.result,
			Count:  counters[key].Load(),
		})
	}
	return snapshot
}

// ConcurrencyCapProbe 执行"占槽 → 并发流式探测 → 判定三档 → 释放"。
type ConcurrencyCapProbe struct {
	concurrency *ConcurrencyService
	proxyRepo   ProxyRepository
	upstream    HTTPUpstream
	cfg         *config.Config
	metrics     *ConcurrencyCapProbeMetrics

	now   func() time.Time
	sleep func(context.Context, time.Duration) bool
}

// NewConcurrencyCapProbe 构造探测器。proxyRepo 可为 nil：账号未预加载代理对象时按直连处理。
func NewConcurrencyCapProbe(
	concurrency *ConcurrencyService,
	proxyRepo ProxyRepository,
	upstream HTTPUpstream,
	cfg *config.Config,
	metrics *ConcurrencyCapProbeMetrics,
) *ConcurrencyCapProbe {
	if metrics == nil {
		metrics = NewConcurrencyCapProbeMetrics()
	}
	return &ConcurrencyCapProbe{
		concurrency: concurrency,
		proxyRepo:   proxyRepo,
		upstream:    upstream,
		cfg:         cfg,
		metrics:     metrics,
		now:         time.Now,
		sleep:       probeSleepWithContext,
	}
}

// Run 执行一次探测。
//
// occupyLanes 为需要占满的当前 cap 槽位数；targetLanes 为探测并发流条数（= 当前 cap + 1）。
// 返回 capStepOutcomeDeferred 表示本轮未取得独占（有用户流量在跑）：调用方只重排，
// 不计 flap、不判失败。
func (p *ConcurrencyCapProbe) Run(ctx context.Context, account *Account, occupyLanes, targetLanes int, settings concurrencyCapSettings) capStepOutcome {
	if p == nil || account == nil || p.upstream == nil || targetLanes <= 0 {
		return capStepOutcomeInconclusive
	}
	release, ok := p.occupySlots(ctx, account.ID, occupyLanes, settings.ProbeDrainTimeout)
	if !ok {
		logger.L().Info("concurrency_cap_probe_deferred",
			zap.Int64("account_id", account.ID),
			zap.Int("occupy_lanes", occupyLanes),
		)
		return capStepOutcomeDeferred
	}
	defer release()

	outcome := p.runLanes(ctx, account, targetLanes, settings)
	switch outcome {
	case capStepOutcomePass:
		p.metrics.IncProbe(targetLanes, concurrencyCapProbeResultPass)
	case capStepOutcomeLimited:
		p.metrics.IncProbe(targetLanes, concurrencyCapProbeResultLimited)
	default:
		p.metrics.IncProbe(targetLanes, concurrencyCapProbeResultInconclusive)
	}
	return outcome
}

// occupySlots 占满 lanes 条真实槽位，占不到则按 probe_drain_timeout 间隔重试，
// 直到本轮 60s 预算耗尽。返回的 release 释放全部已占槽位（必须调用）。
func (p *ConcurrencyCapProbe) occupySlots(ctx context.Context, accountID int64, lanes int, retryInterval time.Duration) (func(), bool) {
	if lanes <= 0 {
		return func() {}, true
	}
	if p.concurrency == nil {
		// 没有并发原语就无法保证独占，宁可不探测（推迟）也不叠加用户流量。
		return nil, false
	}
	if retryInterval <= 0 {
		retryInterval = concurrencyCapDefaultProbeDrainTimeout
	}
	deadline := p.currentTime().Add(concurrencyCapProbeDrainBudget)
	for {
		releases := make([]func(), 0, lanes)
		acquiredAll := true
		for range lanes {
			result, err := p.concurrency.AcquireAccountSlot(ctx, accountID, lanes)
			if err != nil || result == nil || !result.Acquired || result.ReleaseFunc == nil {
				acquiredAll = false
				break
			}
			releases = append(releases, result.ReleaseFunc)
		}
		if acquiredAll {
			return releaseAllSlots(releases), true
		}
		releaseAllSlots(releases)()
		if !p.currentTime().Before(deadline) {
			return nil, false
		}
		if !p.wait(ctx, retryInterval) {
			return nil, false
		}
	}
}

// releaseAllSlots 返回一次性释放全部槽位的函数（逆序释放）。
func releaseAllSlots(releases []func()) func() {
	return func() {
		for i := len(releases) - 1; i >= 0; i-- {
			if releases[i] != nil {
				releases[i]()
			}
		}
	}
}

// currentTime 返回当前时间（时钟可注入，便于测试）。
func (p *ConcurrencyCapProbe) currentTime() time.Time {
	if p == nil || p.now == nil {
		return time.Now()
	}
	return p.now()
}

// probeTimeout 返回单条探测流的超时（probe_timeout，默认 30s）。
func probeTimeout(settings concurrencyCapSettings) time.Duration {
	if settings.ProbeTimeout <= 0 {
		return concurrencyCapDefaultProbeTimeout
	}
	return settings.ProbeTimeout
}

// wait 是可中断等待（休眠函数可注入，便于测试占槽重试）。
func (p *ConcurrencyCapProbe) wait(ctx context.Context, d time.Duration) bool {
	if p == nil || p.sleep == nil {
		return probeSleepWithContext(ctx, d)
	}
	return p.sleep(ctx, d)
}

// runLanes 并发发出 targetLanes 条探测流并汇总判定。
// 判定三档：全部成功 = 通过；任一条命中"并发受限" = 失败；其余 = 不确定。
func (p *ConcurrencyCapProbe) runLanes(ctx context.Context, account *Account, targetLanes int, settings concurrencyCapSettings) capStepOutcome {
	results := make([]capLaneResult, targetLanes)
	var wg sync.WaitGroup
	for i := range targetLanes {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results[index] = p.runLane(ctx, account, targetLanes, settings)
		}(i)
	}
	wg.Wait()

	limited := false
	inconclusive := false
	successes := 0
	for _, result := range results {
		switch result {
		case capLaneResultLimited:
			limited = true
		case capLaneResultSuccess:
			successes++
		default:
			inconclusive = true
		}
	}
	if limited {
		return capStepOutcomeLimited
	}
	if inconclusive || successes != targetLanes {
		return capStepOutcomeInconclusive
	}
	return capStepOutcomePass
}

// runLane 执行单条探测流；上游以极小 max_tokens 拒绝时用兼容值重试一次。
func (p *ConcurrencyCapProbe) runLane(ctx context.Context, account *Account, lanes int, settings concurrencyCapSettings) capLaneResult {
	maxTokens := settings.ProbeMaxTokens
	if maxTokens <= 0 {
		maxTokens = 1
	}
	result, status, body, err := p.sendLane(ctx, account, lanes, maxTokens, settings)
	if err == nil && status == http.StatusBadRequest && maxTokens != concurrencyCapProbeFallbackMaxTokens && capProbeMaxTokensRejected(body) {
		retryResult, _, _, _ := p.sendLane(ctx, account, lanes, concurrencyCapProbeFallbackMaxTokens, settings)
		return retryResult
	}
	return result
}

// sendLane 发送一条探测流并判定其结果（不产生任何网关副作用）。
func (p *ConcurrencyCapProbe) sendLane(ctx context.Context, account *Account, lanes, maxTokens int, settings concurrencyCapSettings) (capLaneResult, int, []byte, error) {
	targetURL, err := p.probeTargetURL(account, settings)
	if err != nil {
		logger.L().Warn("concurrency_cap_probe_target_rejected",
			zap.Int64("account_id", account.ID),
			zap.Error(err),
		)
		return capLaneResultInconclusive, 0, nil, err
	}
	apiKey := strings.TrimSpace(account.GetOpenAIProtocolAPIKey())
	if apiKey == "" {
		return capLaneResultInconclusive, 0, nil, errors.New("account has no api key")
	}
	body, err := capProbePayload(settings.ProbeProtocol, selectCapProbeModel(account), maxTokens)
	if err != nil {
		return capLaneResultInconclusive, 0, nil, err
	}

	laneCtx, cancel := context.WithTimeout(ctx, probeTimeout(settings))
	defer cancel()
	req, err := http.NewRequestWithContext(laneCtx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return capLaneResultInconclusive, 0, nil, err
	}
	p.applyProbeHeaders(req, account, apiKey, settings.ProbeProtocol)

	resp, err := p.upstream.Do(req, p.probeProxyURL(ctx, account), account.ID, probeAccountConcurrency(account, lanes))
	if err != nil {
		return capLaneResultInconclusive, 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, concurrencyCapProbeMaxBodyBytes))
	status := resp.StatusCode
	if ClassifyCNUpstreamError(account.Platform, status, respBody) == UpstreamErrorConcurrentLimit {
		return capLaneResultLimited, status, respBody, nil
	}
	if status != http.StatusOK {
		return capLaneResultInconclusive, status, respBody, readErr
	}
	// 200 也可能是流内 error（生产 403 同源的 Anthropic 流内错误），显式抽取后再分类。
	// 注意必须传语义状态码 403 而不是实测的 200：分类器只认 401/403/429，
	// 传 200 会让「并发受限」的流内文案永远判不出 limited（不计 flap、状态机退化）。
	if message, ok := capProbeStreamErrorMessage(respBody); ok {
		if ClassifyCNUpstreamError(account.Platform, http.StatusForbidden, []byte(message)) == UpstreamErrorConcurrentLimit {
			return capLaneResultLimited, status, respBody, nil
		}
		return capLaneResultInconclusive, status, respBody, nil
	}
	if readErr != nil {
		// 流被截断（超时/连接中断）：无法证明上游受理，判不确定。
		return capLaneResultInconclusive, status, respBody, readErr
	}
	return capLaneResultSuccess, status, respBody, nil
}

// probeTargetURL 组装与生产同一来源的探测端点，并过出站 URL 安全策略：
//   - anthropic（默认）：GetAnthropicProtocolBaseURL + /v1/messages，与原生 Anthropic
//     转发 nativeAnthropicTargetURL 完全同源（生产并发 403 绝大多数出自该路径）；
//   - chat_completions / responses：走生产同一 URL 组装函数（buildOpenAIEndpointURL /
//     buildOpenAIResponsesURLForPlatform）。
func (p *ConcurrencyCapProbe) probeTargetURL(account *Account, settings concurrencyCapSettings) (string, error) {
	if account == nil {
		return "", errors.New("account is nil")
	}
	var raw string
	switch settings.ProbeProtocol {
	case APIProtocolChatCompletions:
		base := strings.TrimSpace(account.GetCNProtocolBaseURL(APIProtocolChatCompletions))
		if base == "" {
			return "", errors.New("account has no chat completions base url")
		}
		raw = buildOpenAIEndpointURL(base, "/v1/chat/completions")
	case APIProtocolResponses:
		base := strings.TrimSpace(account.GetCNProtocolBaseURL(APIProtocolResponses))
		if base == "" {
			return "", errors.New("account has no responses base url")
		}
		raw = buildOpenAIResponsesURLForPlatform(account.Platform, base)
	default:
		base := strings.TrimSpace(account.GetAnthropicProtocolBaseURL())
		if base == "" {
			return "", errors.New("account has no anthropic base url")
		}
		raw = strings.TrimRight(base, "/") + capProbeEndpointPath(settings.ProbeEndpoint)
	}
	return cnValidateProbeURL(p.cfg, raw)
}

// capProbeEndpointPath 规范化探测路径（默认 /v1/messages）。
func capProbeEndpointPath(endpoint string) string {
	path := strings.TrimSpace(endpoint)
	if path == "" {
		path = concurrencyCapProbeDefaultEndpoint
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return path
}

// applyProbeHeaders 注入与生产同源的鉴权与协议头；账号级请求头覆写最后生效。
func (p *ConcurrencyCapProbe) applyProbeHeaders(req *http.Request, account *Account, apiKey string, protocol string) {
	if req == nil {
		return
	}
	switch protocol {
	case APIProtocolChatCompletions, APIProtocolResponses:
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
	default:
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("anthropic-version", "2023-06-01")
		for key, value := range claude.DefaultHeaders {
			req.Header.Set(key, value)
		}
		req.Header.Set("anthropic-beta", claude.APIKeyBetaHeader)
		setAnthropicAPIKeyAuthHeader(req.Header, account, apiKey, account.GetAnthropicProtocolBaseURL())
	}
	account.ApplyHeaderOverrides(req.Header)
}

// probeProxyURL 解析账号代理：优先已加载对象，其次按 ProxyID 回源（与额度探测一致）。
func (p *ConcurrencyCapProbe) probeProxyURL(ctx context.Context, account *Account) string {
	if account == nil || account.ProxyID == nil {
		return ""
	}
	if account.Proxy != nil {
		return account.Proxy.URL()
	}
	if p.proxyRepo == nil {
		return ""
	}
	proxy, err := p.proxyRepo.GetByID(ctx, *account.ProxyID)
	if err != nil || proxy == nil {
		return ""
	}
	account.Proxy = proxy
	return proxy.URL()
}

// probeAccountConcurrency 返回传给出站连接的账号并发提示（仅用于连接池 sizing，
// 与生产转发保持一致：优先账号配置并发，缺失时退回本轮探测车道数）。
func probeAccountConcurrency(account *Account, lanes int) int {
	if account != nil && account.Concurrency > 0 {
		return account.Concurrency
	}
	return lanes
}

// capProbePayload 构造探测请求体：stream=true + 极小 max_tokens（PLAN §4.2）。
func capProbePayload(protocol, model string, maxTokens int) ([]byte, error) {
	if maxTokens <= 0 {
		maxTokens = 1
	}
	messages := []map[string]any{{"role": "user", "content": concurrencyCapProbePrompt}}
	var payload map[string]any
	switch protocol {
	case APIProtocolChatCompletions:
		payload = map[string]any{
			"model":      model,
			"max_tokens": maxTokens,
			"stream":     true,
			"messages":   messages,
		}
	case APIProtocolResponses:
		payload = map[string]any{
			"model":             model,
			"max_output_tokens": maxTokens,
			"stream":            true,
			"input":             messages,
		}
	default:
		// anthropic（含 adaptive 的 Anthropic 端点）：生产 403 同源路径。
		payload = map[string]any{
			"model":      model,
			"max_tokens": maxTokens,
			"stream":     true,
			"messages":   messages,
		}
	}
	return json.Marshal(payload)
}

// selectCapProbeModel 选出探测用的上游模型。
//
// 探测必须用上游真实存在的模型，否则只会拿到 400 model-not-found 并判为"不确定"，
// 回升永不发生。优先取账号 model_mapping 的上游模型（值），按字典序取首个具体
// （非通配符）模型以保证可复现；无映射时回退生产默认测试模型的映射结果。
func selectCapProbeModel(account *Account) string {
	if account == nil {
		return ""
	}
	mapping := account.GetModelMapping()
	candidates := make([]string, 0, len(mapping))
	for _, upstream := range mapping {
		upstream = strings.TrimSpace(upstream)
		if upstream == "" || strings.Contains(upstream, "*") {
			continue
		}
		candidates = append(candidates, upstream)
	}
	if len(candidates) > 0 {
		sort.Strings(candidates)
		return candidates[0]
	}
	if mapped := strings.TrimSpace(account.GetMappedModel(claude.DefaultTestModel)); mapped != "" {
		return mapped
	}
	return claude.DefaultTestModel
}

// capProbeMaxTokensRejected 判断 400 响应体是否为 max_tokens 取值被拒（用于兼容重试）。
func capProbeMaxTokensRejected(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	lowered := strings.ToLower(string(body))
	return strings.Contains(lowered, "max_tokens") ||
		strings.Contains(lowered, "max_output_tokens") ||
		strings.Contains(lowered, "max output tokens")
}

// capProbeStreamErrorMessage 从流式响应体中抽取流内错误文案。
// 覆盖 Anthropic 的 event: error、Responses 的 error / response.failed，以及
// Chat Completions 兼容端点直接给 {"error": {...}} 的形态。
func capProbeStreamErrorMessage(body []byte) (string, bool) {
	if len(body) == 0 {
		return "", false
	}
	for _, rawLine := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(rawLine)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		if message, ok := capProbeEventErrorMessage(event); ok {
			return message, true
		}
	}
	return "", false
}

func capProbeEventErrorMessage(event map[string]any) (string, bool) {
	switch eventType, _ := event["type"].(string); eventType {
	case "error":
		return capProbeNestedErrorMessage(event["error"])
	case "response.failed":
		if response, ok := event["response"].(map[string]any); ok {
			return capProbeNestedErrorMessage(response["error"])
		}
		return "", false
	}
	if _, present := event["error"]; present {
		return capProbeNestedErrorMessage(event["error"])
	}
	return "", false
}

func capProbeNestedErrorMessage(raw any) (string, bool) {
	object, ok := raw.(map[string]any)
	if !ok {
		return "", false
	}
	message, _ := object["message"].(string)
	message = strings.TrimSpace(message)
	if message == "" {
		return "", false
	}
	return message, true
}

// probeSleepWithContext 可中断等待：返回 false 表示 ctx 已结束（或 d<=0 且 ctx 已取消）。
func probeSleepWithContext(ctx context.Context, d time.Duration) bool {
	if ctx == nil {
		return true
	}
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
