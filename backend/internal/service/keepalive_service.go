package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// keepalive_service.go — kimi Coding Plan 前缀缓存保活。
//
// 背景：kimi 自动前缀缓存空闲约 5-10 分钟过期，过期后整段上下文按 input 原价
// 重算（本部署 input 价是 cache_read 的 10 倍）。本模块在网关转发链路的
// 「最后一公里」（上游请求构造完毕处）捕获报文，会话空闲时向绑定账号原样
// 重放（仅收紧输出参数），使命中同一缓存条目并重置 TTL。
//
// 设计约束（与调度/计费链路的隔离）：
//   - 探测不经过调度器：钉死捕获时的账号直发，无 failover，不进 TTFT/错误率统计；
//   - 探测只在账号在途请求为 0 时发出，不与真实请求抢并发槽；
//   - 探测正常计量（usage_log 正常计费），UserAgent 标记 sub2api-keepalive 供对账；
//   - 进程内注册表（单实例部署），重启丢失无副作用：下一个真实请求会重新捕获。

const (
	keepaliveUserAgent = "sub2api-keepalive"
	// keepaliveTokenByteRatio 用报文字节数估算 prompt token 数的除数
	// （实测混合内容约 3.2 字符/token，取 3 保守估计，仅用于捕获门槛）。
	keepaliveTokenByteRatio = 3
	// keepaliveHitRatio 判定探测命中前缀缓存的 cached/prompt 比例。
	keepaliveHitRatio = 0.9
	// keepaliveMaxMisses 连续 miss 达到该次数删除条目（缓存真没了）。
	keepaliveMaxMisses = 2
	// keepaliveMaxErrors 连续传输/限流错误达到该次数删除条目。
	keepaliveMaxErrors = 3
)

// SetKeepalive 装配保活服务（wire_gen 手工注入；nil 时全链路零开销）。
func (s *OpenAIGatewayService) SetKeepalive(k *KeepaliveService) {
	if s != nil {
		s.keepalive = k
	}
}

// captureKeepalive 在各上游请求构造完毕处调用；未装配或功能关闭时直接返回。
func (s *OpenAIGatewayService) captureKeepalive(ctx context.Context, account *Account, targetURL string, header http.Header, body []byte) {
	if s == nil || s.keepalive == nil {
		return
	}
	s.keepalive.Capture(ctx, account, targetURL, header, body)
}

// KeepaliveCaptureInfo 由入口 handler 注入请求 ctx，携带计费与粘性身份。
type KeepaliveCaptureInfo struct {
	SessionHash string
	GroupID     *int64
	APIKeyID    int64
	UserID      int64
}

type keepaliveCaptureCtxKey struct{}

// WithKeepaliveCaptureInfo 把捕获描述注入请求 ctx（由 OpenAI 网关 handler 调用）。
func WithKeepaliveCaptureInfo(ctx context.Context, info *KeepaliveCaptureInfo) context.Context {
	if info == nil {
		return ctx
	}
	return context.WithValue(ctx, keepaliveCaptureCtxKey{}, info)
}

func keepaliveCaptureInfoFromContext(ctx context.Context) *KeepaliveCaptureInfo {
	if ctx == nil {
		return nil
	}
	info, _ := ctx.Value(keepaliveCaptureCtxKey{}).(*KeepaliveCaptureInfo)
	return info
}

// keepaliveEntry 一个保活会话的注册条目。
type keepaliveEntry struct {
	sessionHash string
	groupID     *int64
	accountID   int64
	apiKeyID    int64
	userID      int64

	targetURL string
	header    http.Header
	body      []byte
	model     string

	lastActivity time.Time
	backoffUntil time.Time
	misses       int
	errors       int
}

// KeepaliveService 注册表 + 后台探测协程。
type KeepaliveService struct {
	cfg           *config.Config
	accountRepo   AccountRepository
	apiKeyService *APIKeyService
	concurrency   *ConcurrencyService
	openAI        *OpenAIGatewayService
	httpClient    *http.Client

	mu      sync.Mutex
	entries map[string]*keepaliveEntry
	stopCh  chan struct{}
	stopped sync.Once
}

// NewKeepaliveService 构造保活服务（未启动）。
func NewKeepaliveService(
	cfg *config.Config,
	accountRepo AccountRepository,
	apiKeyService *APIKeyService,
	concurrency *ConcurrencyService,
	openAI *OpenAIGatewayService,
) *KeepaliveService {
	return &KeepaliveService{
		cfg:           cfg,
		accountRepo:   accountRepo,
		apiKeyService: apiKeyService,
		concurrency:   concurrency,
		openAI:        openAI,
		httpClient:    &http.Client{Timeout: 180 * time.Second},
		entries:       make(map[string]*keepaliveEntry),
		stopCh:        make(chan struct{}),
	}
}

// ProvideKeepaliveService 创建并启动保活服务（遵循服务自启动惯例）。
func ProvideKeepaliveService(
	cfg *config.Config,
	accountRepo AccountRepository,
	apiKeyService *APIKeyService,
	concurrency *ConcurrencyService,
	openAI *OpenAIGatewayService,
) *KeepaliveService {
	svc := NewKeepaliveService(cfg, accountRepo, apiKeyService, concurrency, openAI)
	svc.Start()
	return svc
}

func (s *KeepaliveService) keepaliveCfg() config.GatewayKeepaliveConfig {
	if s != nil && s.cfg != nil {
		return s.cfg.Gateway.Keepalive
	}
	return config.GatewayKeepaliveConfig{}
}

func (s *KeepaliveService) enabled() bool { return s.keepaliveCfg().Enabled }

func (s *KeepaliveService) interval() time.Duration {
	if v := s.keepaliveCfg().IntervalSeconds; v > 0 {
		return time.Duration(v) * time.Second
	}
	return 300 * time.Second
}

func (s *KeepaliveService) maxIdle() time.Duration {
	if v := s.keepaliveCfg().MaxIdleSeconds; v > 0 {
		return time.Duration(v) * time.Second
	}
	return time.Hour
}

func (s *KeepaliveService) minPromptTokens() int {
	if v := s.keepaliveCfg().MinPromptTokens; v > 0 {
		return v
	}
	return 300000
}

func (s *KeepaliveService) maxBodyBytes() int64 {
	if v := s.keepaliveCfg().MaxBodyBytes; v > 0 {
		return v
	}
	return 8 << 20
}

func (s *KeepaliveService) probeMaxTokens() int {
	if v := s.keepaliveCfg().ProbeMaxTokens; v > 0 {
		return v
	}
	return 16
}

func (s *KeepaliveService) maxEntriesPerAccount() int {
	if v := s.keepaliveCfg().MaxEntriesPerAccount; v > 0 {
		return v
	}
	return 8
}

// Capture 在上游请求构造完毕处调用（所有 CN 转发路径共用一个入口）。
// 仅在功能开启 + kimi Coding Plan 账号 + ctx 有捕获描述 + 报文达门槛时登记。
func (s *KeepaliveService) Capture(ctx context.Context, account *Account, targetURL string, header http.Header, body []byte) {
	if s == nil || !s.enabled() {
		return
	}
	if account == nil || account.Platform != PlatformKimi || !account.IsCodingPlan() {
		return
	}
	info := keepaliveCaptureInfoFromContext(ctx)
	if info == nil || info.SessionHash == "" || info.APIKeyID <= 0 {
		return
	}
	if targetURL == "" || len(body) == 0 || int64(len(body)) > s.maxBodyBytes() {
		return
	}
	if len(body)/keepaliveTokenByteRatio < s.minPromptTokens() {
		return
	}

	entry := &keepaliveEntry{
		sessionHash:  info.SessionHash,
		groupID:      info.GroupID,
		accountID:    account.ID,
		apiKeyID:     info.APIKeyID,
		userID:       info.UserID,
		targetURL:    targetURL,
		header:       cloneKeepaliveHeader(header),
		body:         append([]byte(nil), body...),
		model:        strings.TrimSpace(gjson.GetBytes(body, "model").String()),
		lastActivity: time.Now(),
	}

	key := keepaliveEntryKey(info.GroupID, info.SessionHash)
	s.mu.Lock()
	s.entries[key] = entry
	s.evictLocked(entry.accountID)
	s.mu.Unlock()
}

func keepaliveEntryKey(groupID *int64, sessionHash string) string {
	gid := int64(0)
	if groupID != nil {
		gid = *groupID
	}
	return fmt.Sprintf("%d:%s", gid, sessionHash)
}

// evictLocked 超单账号上限时淘汰最久未活动的条目（调用方须持锁）。
func (s *KeepaliveService) evictLocked(accountID int64) {
	cap := s.maxEntriesPerAccount()
	for {
		count := 0
		var oldestKey string
		var oldest time.Time
		for k, e := range s.entries {
			if e.accountID != accountID {
				continue
			}
			count++
			if oldestKey == "" || e.lastActivity.Before(oldest) {
				oldestKey, oldest = k, e.lastActivity
			}
		}
		if count <= cap || oldestKey == "" {
			return
		}
		delete(s.entries, oldestKey)
	}
}

func cloneKeepaliveHeader(h http.Header) http.Header {
	if h == nil {
		return nil
	}
	out := make(http.Header, len(h))
	for k, vs := range h {
		lk := strings.ToLower(k)
		switch lk {
		case "connection", "content-length", "transfer-encoding", "keep-alive", "upgrade":
			continue
		}
		out[k] = append([]string(nil), vs...)
	}
	return out
}

// Start 启动后台探测协程（幂等；未启用时协程空转，每次 tick 重新读开关）。
func (s *KeepaliveService) Start() {
	if s == nil {
		return
	}
	go s.loop()
}

// Stop 停止后台协程。
func (s *KeepaliveService) Stop() {
	if s == nil {
		return
	}
	s.stopped.Do(func() { close(s.stopCh) })
}

func (s *KeepaliveService) loop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.tick()
		}
	}
}

// tick 扫描一轮注册表：清理过期条目，对到期会话发探测。
func (s *KeepaliveService) tick() {
	if !s.enabled() {
		return
	}
	now := time.Now()
	interval := s.interval()
	maxIdle := s.maxIdle()

	var due []*keepaliveEntry
	s.mu.Lock()
	for key, e := range s.entries {
		idle := now.Sub(e.lastActivity)
		if idle > maxIdle {
			delete(s.entries, key)
			continue
		}
		if idle >= interval && now.After(e.backoffUntil) {
			due = append(due, e)
		}
	}
	s.mu.Unlock()

	for _, e := range due {
		s.probe(context.Background(), e)
	}
}

// probe 对单个会话执行一次保活探测。全流程 fail-open：任何异常只影响该条目。
func (s *KeepaliveService) probe(ctx context.Context, e *keepaliveEntry) {
	now := time.Now()

	// 账号现状校验：停用/换平台/粘性绑定已迁移 → 条目作废。
	account, err := s.accountRepo.GetByID(ctx, e.accountID)
	if err != nil || account == nil || !account.IsSchedulable() || account.Platform != PlatformKimi {
		s.removeEntry(e)
		return
	}
	if bound, _ := s.openAI.getStickySessionAccountID(ctx, e.groupID, e.sessionHash); bound > 0 && bound != e.accountID {
		// 绑定已迁移：目标 URL/认证是旧账号的，不可复用；等下一个真实请求重新捕获。
		s.removeEntry(e)
		return
	}
	// 不抢并发槽：账号有在途请求时跳过本轮。
	if s.accountBusy(ctx, account) {
		return
	}

	probeBody := buildKeepaliveProbeBody(e.body, s.probeMaxTokens())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.targetURL, bytes.NewReader(probeBody))
	if err != nil {
		s.registerError(e)
		return
	}
	for k, vs := range e.header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := s.httpClient.Do(req)
	if err != nil {
		s.registerError(e)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	elapsed := time.Since(start)

	switch {
	case resp.StatusCode == http.StatusOK:
		usage := parseKeepaliveUsage(respBody)
		s.billProbe(ctx, e, account, usage, elapsed)
		if usage.cacheRead >= int(float64(usage.promptTotal)*keepaliveHitRatio) && usage.promptTotal > 0 {
			s.registerHit(ctx, e, now)
		} else {
			s.registerMiss(e)
		}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// 凭证失效：保活无意义，等真实请求重新捕获（或账号被处理）。
		s.removeEntry(e)
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		// 限流/上游故障：静默退避两轮间隔，不触发任何停车逻辑。
		s.mu.Lock()
		if cur, ok := s.entries[keepaliveEntryKey(e.groupID, e.sessionHash)]; ok && cur == e {
			e.backoffUntil = now.Add(2 * s.interval())
			e.errors++
			if e.errors >= keepaliveMaxErrors {
				delete(s.entries, keepaliveEntryKey(e.groupID, e.sessionHash))
			}
		}
		s.mu.Unlock()
	default:
		s.registerError(e)
	}
}

// accountBusy 查询账号当前在途请求数（复用调度器的并发计数）。
func (s *KeepaliveService) accountBusy(ctx context.Context, account *Account) bool {
	if s.concurrency == nil {
		return false
	}
	loadMap, err := s.concurrency.GetAccountsLoadBatch(ctx, []AccountWithConcurrency{{
		ID:             account.ID,
		MaxConcurrency: account.EffectiveLoadFactor(),
	}})
	if err != nil || loadMap == nil {
		return false // 负载未知时放行：探测本身极轻
	}
	if info := loadMap[account.ID]; info != nil && info.CurrentConcurrency > 0 {
		return true
	}
	return false
}

// billProbe 探测用量正常计量（UserAgent 打标，供对账筛选）。
func (s *KeepaliveService) billProbe(ctx context.Context, e *keepaliveEntry, account *Account, usage keepaliveUsage, elapsed time.Duration) {
	if s.apiKeyService == nil || s.openAI == nil {
		return
	}
	apiKey, err := s.apiKeyService.GetByID(ctx, e.apiKeyID)
	if err != nil || apiKey == nil {
		return
	}
	user := apiKey.User
	if user == nil && e.userID > 0 {
		user = &User{ID: e.userID}
	}
	if user == nil {
		return
	}
	result := &OpenAIForwardResult{
		RequestID:        "keepalive-" + keepaliveRandID(),
		Usage:            usage.toOpenAIUsage(),
		Model:            e.model,
		Duration:         elapsed,
		UpstreamEndpoint: e.targetURL,
	}
	input := &OpenAIRecordUsageInput{
		Result:        result,
		APIKey:        apiKey,
		User:          user,
		Account:       account,
		UserAgent:     keepaliveUserAgent,
		SessionID:     e.sessionHash,
		APIKeyService: s.apiKeyService,
		PricingAt:     time.Now(),
	}
	if err := s.openAI.RecordUsage(ctx, input); err != nil {
		slog.Warn("keepalive_probe_billing_failed", "account_id", e.accountID, "api_key_id", e.apiKeyID, "error", err)
	}
}

func keepaliveRandID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (s *KeepaliveService) registerHit(ctx context.Context, e *keepaliveEntry, now time.Time) {
	s.mu.Lock()
	if _, ok := s.entries[keepaliveEntryKey(e.groupID, e.sessionHash)]; ok {
		e.lastActivity = now
		e.misses = 0
		e.errors = 0
	}
	s.mu.Unlock()
	// 顺手给粘性绑定续命：kimi 缓存与账号绑定一起保活。
	if s.openAI == nil {
		return
	}
	if err := s.openAI.BindStickySession(ctx, e.groupID, e.sessionHash, e.accountID); err != nil {
		slog.Warn("keepalive_sticky_refresh_failed", "account_id", e.accountID, "error", err)
	}
	slog.Info("keepalive_probe_hit",
		"account_id", e.accountID,
		"api_key_id", e.apiKeyID,
		"model", e.model,
	)
}

func (s *KeepaliveService) registerMiss(e *keepaliveEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := keepaliveEntryKey(e.groupID, e.sessionHash)
	if _, ok := s.entries[key]; !ok {
		return
	}
	e.misses++
	slog.Info("keepalive_probe_miss", "account_id", e.accountID, "misses", e.misses)
	if e.misses >= keepaliveMaxMisses {
		delete(s.entries, key)
	}
}

func (s *KeepaliveService) registerError(e *keepaliveEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := keepaliveEntryKey(e.groupID, e.sessionHash)
	if _, ok := s.entries[key]; !ok {
		return
	}
	e.errors++
	if e.errors >= keepaliveMaxErrors {
		delete(s.entries, key)
	}
}

func (s *KeepaliveService) removeEntry(e *keepaliveEntry) {
	s.mu.Lock()
	delete(s.entries, keepaliveEntryKey(e.groupID, e.sessionHash))
	s.mu.Unlock()
}

// entryCount 返回当前注册条目数（测试与观测用）。
func (s *KeepaliveService) entryCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// keepaliveUsage 归一化三种上游响应形态（chat completions / anthropic / responses）的 usage。
type keepaliveUsage struct {
	promptTotal   int // 含缓存的总 prompt（OpenAI 语义）
	completion    int
	cacheRead     int
	cacheCreation int
}

func (u keepaliveUsage) toOpenAIUsage() OpenAIUsage {
	return OpenAIUsage{
		InputTokens:              u.promptTotal,
		OutputTokens:             u.completion,
		CacheReadInputTokens:     u.cacheRead,
		CacheCreationInputTokens: u.cacheCreation,
	}
}

// parseKeepaliveUsage 解析探测响应的 usage。anthropic 原生端点的 input_tokens
// 不含缓存，这里统一折算为「含缓存」的 OpenAI 语义，与 RecordUsage 的减法一致。
func parseKeepaliveUsage(body []byte) keepaliveUsage {
	usage := gjson.GetBytes(body, "usage")
	if !usage.Exists() {
		return keepaliveUsage{}
	}
	if cached := usage.Get("cache_read_input_tokens"); cached.Exists() {
		// anthropic 形态：input/output + cache_* 分列
		input := int(usage.Get("input_tokens").Int())
		cacheRead := int(cached.Int())
		cacheCreation := int(usage.Get("cache_creation_input_tokens").Int())
		return keepaliveUsage{
			promptTotal:   input + cacheRead + cacheCreation,
			completion:    int(usage.Get("output_tokens").Int()),
			cacheRead:     cacheRead,
			cacheCreation: cacheCreation,
		}
	}
	if details := usage.Get("prompt_tokens_details.cached_tokens"); details.Exists() {
		// chat completions 形态：prompt_tokens 含缓存
		return keepaliveUsage{
			promptTotal: int(usage.Get("prompt_tokens").Int()),
			completion:  int(usage.Get("completion_tokens").Int()),
			cacheRead:   int(details.Int()),
		}
	}
	// responses 形态：input_tokens 含缓存
	return keepaliveUsage{
		promptTotal: int(usage.Get("input_tokens").Int()),
		completion:  int(usage.Get("output_tokens").Int()),
		cacheRead:   int(usage.Get("input_tokens_details.cached_tokens").Int()),
	}
}

// buildKeepaliveProbeBody 收紧输出参数后的探测报文：
// messages/tools/prompt_cache_key 等缓存键输入保持逐字节不变。
func buildKeepaliveProbeBody(body []byte, maxTokens int) []byte {
	if maxTokens <= 0 {
		maxTokens = 16
	}
	out := body
	// 非流式，usage 可直接解析
	for _, key := range []string{"stream", "stream_options", "thinking", "store"} {
		out, _ = sjson.DeleteBytes(out, key)
	}
	// 按请求形态设置输出上限：responses 用 max_output_tokens，其余 max_tokens。
	if gjson.GetBytes(out, "input").Exists() && !gjson.GetBytes(out, "messages").Exists() {
		out, _ = sjson.SetBytes(out, "max_output_tokens", maxTokens)
		out, _ = sjson.DeleteBytes(out, "max_tokens")
	} else {
		out, _ = sjson.SetBytes(out, "max_tokens", maxTokens)
		out, _ = sjson.DeleteBytes(out, "max_output_tokens")
	}
	return out
}
