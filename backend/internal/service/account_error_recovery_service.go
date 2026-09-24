package service

// 历史被禁用账号自动归队（PLAN-cn-cap-v5 §2.4）。
//
// 背景：国产 Coding Plan（kimi / zhipu / minimax）账号在额度耗尽时撞 403，老路径按
// 通用 403 计数 3 次 → SetError（status='error' + schedulable=false）；ClearError
// 不复位 schedulable，账号因此永久卡死，只能人工救。
//
// 本服务周期扫描这些账号，在额度恢复后自动拉回调度池：
//  1. ListCNQuotaDisabled 取候选（status='error' 的 CN 平台账号，不带 active 过滤）；
//  2. 用本地额度快照的 reset_at 判定「周未满 且 5h 未满」——reset_at 已过 = 窗口
//     已重置，是确定事实（管理前端的额度倒计时读的就是它），无需探测上游；
//     快照里没有窗口重置键的，交给第 3 步的验证请求仲裁；
//  3. 发一条最小请求点火验证（独立出站：HTTPUpstream.Do 原语无计费/用量/健康上报
//     钩子，出站 URL 过 cnValidateProbeURL）——防「鉴权已死」账号误恢复形成死循环；
//  4. CAS 恢复：验证后重读 accounts.updated_at 再比对（任何并发写入都会推进它，
//     用旧值比对影响行数恒为 0）。
//     必须在「探测 + 验证」完成后【重新读取】updated_at 再做 CAS；
//     影响行数 0 = 并发改写 → 重排下一轮且不消耗退避预算；
//  5. 退避：10m → 20m → 40m → 封顶 6h，每账号每轮最多 3 次探测。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const (
	// cnRecoveryRoundBudget 是单轮扫描的总预算；耗尽后剩余账号顺延到下一轮
	// （已处理的账号已进入退避窗口，不会造成尾部饥饿）。
	cnRecoveryRoundBudget = 5 * time.Minute
	// cnRecoveryVerifyTimeout 是单条最小验证请求的超时。
	cnRecoveryVerifyTimeout = 30 * time.Second
	// cnRecoveryVerifyMaxTokens 是最小验证请求的 max_tokens（尽量不消耗额度）。
	cnRecoveryVerifyMaxTokens = 8
	// cnRecoveryVerifyMaxBodyBytes 限制验证响应的读取量（仅用于判定与日志）。
	cnRecoveryVerifyMaxBodyBytes = 64 * 1024
	// cnRecoveryVerifyPrompt 是最小验证请求的提示词。
	cnRecoveryVerifyPrompt = "hi"
	// cnRecoveryLeaderLockKey 保证多实例下只有一个实例执行扫描。
	cnRecoveryLeaderLockKey = "cn:quota:recovery:leader"
	// cnRecoveryLeaderLockTTL 是 leader 锁 TTL 的兜底值（配置缺失时使用）。
	cnRecoveryDefaultLeaderLockTTL = 90 * time.Second
	// cnRecoveryLeaderLockMargin 是单轮预算与 leader 锁 TTL 之间保留的安全余量：
	// tryAcquireSingletonLeaderLock 不续租，单轮必须锁 TTL 内跑完，否则锁会在扫描
	// 中途过期、另一个实例并发进入（重复探测虽被 CAS/退避兜住，但属无用功）。
	cnRecoveryLeaderLockMargin = 15 * time.Second
)

// defaultCNRecoveryBackoff 是退避表的兜底值（配置缺失/非法时使用）。
var defaultCNRecoveryBackoff = []time.Duration{
	10 * time.Minute,
	20 * time.Minute,
	40 * time.Minute,
	6 * time.Hour,
}

// cnRecoveryState 是单账号的探测退避状态。
// 进程内保存：只有 leader 实例执行扫描，重启后归零 = 允许立即重试（无副作用）。
type cnRecoveryState struct {
	failures      int
	nextAttemptAt time.Time
}

// AccountErrorRecoveryService 周期恢复因额度耗尽被禁用的国产 Coding Plan 账号。
type AccountErrorRecoveryService struct {
	accountRepo         AccountRepository
	httpUpstream        HTTPUpstream
	tlsFPProfileService *TLSFingerprintProfileService
	cfg                 *config.Config
	interval            time.Duration

	lockCache  LeaderLockCache
	db         *sql.DB
	instanceID string

	// now 可在测试中注入（nil → time.Now）。
	now func() time.Time

	mu     sync.Mutex
	states map[int64]*cnRecoveryState

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewAccountErrorRecoveryService 构造历史账号归队服务。
// interval <= 0 时 Start() 直接返回（不启动），便于通过配置关闭。
func NewAccountErrorRecoveryService(
	accountRepo AccountRepository,
	httpUpstream HTTPUpstream,
	cfg *config.Config,
	interval time.Duration,
) *AccountErrorRecoveryService {
	return &AccountErrorRecoveryService{
		accountRepo:  accountRepo,
		httpUpstream: httpUpstream,
		cfg:          cfg,
		interval:     interval,
		instanceID:   uuid.NewString(),
		states:       make(map[int64]*cnRecoveryState),
		stopCh:       make(chan struct{}),
	}
}

// SetTLSFingerprintProfileService keeps recovery probes on the same account
// transport policy as production traffic.
func (s *AccountErrorRecoveryService) SetTLSFingerprintProfileService(profiles *TLSFingerprintProfileService) {
	s.tlsFPProfileService = profiles
}

// SetLeaderLock 注入 leader 锁缓存与 DB（与其它周期任务一致）。
// 两者均为 nil 时不做互斥（单实例/测试行为）。
func (s *AccountErrorRecoveryService) SetLeaderLock(lockCache LeaderLockCache, db *sql.DB) {
	if s == nil {
		return
	}
	s.lockCache = lockCache
	s.db = db
}

func (s *AccountErrorRecoveryService) Start() {
	if s == nil || s.accountRepo == nil || s.httpUpstream == nil || s.cfg == nil {
		return
	}
	if !s.cfg.Gateway.CNProviders.ErrorRecoveryEnabled {
		return
	}
	if s.interval <= 0 {
		return
	}
	log.Printf("[CNRecovery] started (interval=%s)", s.interval)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		// 启动后先等待一个周期再首次执行，避免与进程启动峰重叠。
		for {
			select {
			case <-ticker.C:
				s.runOnce()
			case <-s.stopCh:
				return
			}
		}
	}()
}

func (s *AccountErrorRecoveryService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	s.wg.Wait()
}

func (s *AccountErrorRecoveryService) runOnce() {
	if s == nil || s.accountRepo == nil || s.cfg == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.roundBudget())
	defer cancel()
	release, acquired := tryAcquireSingletonLeaderLock(
		ctx, s.lockCache, s.db, cnRecoveryLeaderLockKey, s.instanceID, s.leaderLockTTL(),
	)
	if !acquired {
		return
	}
	defer release()

	now := s.currentTime()
	var stats cnRecoveryRoundStats
	for _, platform := range cnRecoveryPlatforms() {
		accounts, err := s.accountRepo.ListCNQuotaDisabled(ctx, platform)
		if err != nil {
			log.Printf("[CNRecovery] list %s disabled accounts failed: %v", platform, err)
			continue
		}
		for _, account := range accounts {
			if account == nil {
				continue
			}
			if ctx.Err() != nil {
				// 本轮预算耗尽：剩余账号顺延到下一轮，不消耗它们的退避预算。
				log.Printf("[CNRecovery] round budget exhausted (%s)", stats)
				return
			}
			s.recoverOne(ctx, account, now, &stats)
		}
	}
	if stats.restored > 0 || stats.deferred > 0 {
		log.Printf("[CNRecovery] round finished (%s)", stats)
	}
}

// cnRecoveryRoundStats 汇总单轮结果，仅用于日志。
type cnRecoveryRoundStats struct {
	restored    int
	deferred    int
	unsupported int
}

func (s cnRecoveryRoundStats) String() string {
	return fmt.Sprintf("restored=%d deferred=%d unsupported=%d", s.restored, s.deferred, s.unsupported)
}

// recoverOne 处理单个被禁用账号：额度判定 → 最小请求验证 → CAS 恢复。
// CAS 失败（影响行数 0）视为并发改写：重排下一轮且不消耗退避预算。
func (s *AccountErrorRecoveryService) recoverOne(ctx context.Context, account *Account, now time.Time, stats *cnRecoveryRoundStats) {
	if !s.due(account.ID, now) {
		return
	}
	// 只有 coding plan 账号有可信的额度恢复信号。余额型（payg）与 base_url 指向
	// 自定义中转、无法识别官方供应商的账号不参与自动归队，交给人工处置
	//（错误重放会反复失败，且没有可判定的恢复点）。
	if !account.IsCodingPlan() || account.GetCodingPlanProvider() == "" {
		stats.unsupported++
		return
	}

	recovered, known := cnQuotaRecoveredFromSnapshot(account, now, s.quotaExhaustedPercent())
	if known && !recovered {
		// 额度耗尽的恢复时间由上游窗口重置点决定（快照里写死的确定事实），
		// 不能把通用翻倍退避（10m→6h 封顶）套上来——那会把归队无谓推迟数小时。
		s.scheduleAtQuotaReset(account.ID, account, now, s.quotaExhaustedPercent())
		stats.deferred++
		return
	}

	// 快照判定已恢复，或快照里没有窗口数据（known=false）：用最小请求点火验证
	// 仲裁——它不是探额度，是防「鉴权已死」账号被误恢复形成恢复-再死循环。
	if err := s.verifyMinimalRequest(ctx, account); err != nil {
		log.Printf("[CNRecovery] verify account %d (%s) failed: %v", account.ID, account.Platform, err)
		s.scheduleNextAttempt(account.ID, now, cnRecoveryBackoffReasonVerifyFailed)
		stats.deferred++
		return
	}

	// CAS 必须用「探测 + 验证」之后重读的 updated_at：额度探测落快照走 UpdateExtra，
	// 会把 accounts.updated_at 推进到探测时刻，用旧值比对影响行数恒为 0。
	fresh, err := s.accountRepo.GetByID(ctx, account.ID)
	if err != nil || fresh == nil {
		log.Printf("[CNRecovery] reload account %d failed: %v", account.ID, err)
		s.scheduleNextAttempt(account.ID, now, cnRecoveryBackoffReasonReloadFailed)
		stats.deferred++
		return
	}
	restored, err := s.accountRepo.RestoreRecoveredAccount(ctx, account.ID, fresh.UpdatedAt)
	if err != nil {
		log.Printf("[CNRecovery] restore account %d failed: %v", account.ID, err)
		s.scheduleNextAttempt(account.ID, now, cnRecoveryBackoffReasonRestoreFailed)
		stats.deferred++
		return
	}
	if !restored {
		// 并发改写（其它实例或请求路径同时更新了账号行）：下一轮重试，不消耗退避预算。
		log.Printf("[CNRecovery] account %d changed concurrently, retry next round", account.ID)
		s.requeue(account.ID, now)
		stats.deferred++
		return
	}
	s.clearState(account.ID)
	log.Printf("[CNRecovery] restored account %d (%s)", account.ID, account.Platform)
	stats.restored++
}

// cnQuotaRecoveredFromSnapshot 用本地额度快照判定账号是否已恢复（周未满 且 5h 未满）。
// reset_at 已过 = 窗口已重置，是确定事实（管理前端显示的「额度刷新倒计时」读的就是
// 它），不需要再探测上游。快照新旧不影响判定：reset_at 是时间点事实，不是采样值。
// 返回 known=false 表示快照里连窗口重置键都没有（从未探测过/数据缺失）——
// 由调用方交给最小请求验证仲裁。
func cnQuotaRecoveredFromSnapshot(account *Account, now time.Time, threshold float64) (recovered, known bool) {
	if account == nil || len(account.Extra) == 0 {
		return false, false
	}
	hasResetKey := false
	for _, suffix := range []string{cnExtraSuffixWeeklyReset, cnExtraSuffix5hReset} {
		if _, ok := account.Extra[cnExtraKey(account.Platform, suffix)]; ok {
			hasResetKey = true
			break
		}
	}
	if !hasResetKey {
		return false, false
	}
	// maxAge=0 跳过新鲜度检查：恢复判定只看「是否有窗口仍处耗尽且重置点在未来」。
	return !cnProviderQuotaSnapshotExhausted(account, now, threshold, 0), true
}

// verifyMinimalRequest 发一条最小请求验证账号确实能再次服务上游。
// 独立出站：HTTPUpstream.Do 原语没有计费/用量/健康上报钩子；出站 URL 先过
// cnValidateProbeURL（与余额/额度探测同一套运营者策略，API key 不得发往策略外主机）。
func (s *AccountErrorRecoveryService) verifyMinimalRequest(ctx context.Context, account *Account) error {
	if s.httpUpstream == nil {
		return errors.New("http upstream is not configured")
	}
	callCtx, cancel := context.WithTimeout(ctx, cnRecoveryVerifyTimeout)
	defer cancel()

	req, err := s.buildVerifyRequest(callCtx, account)
	if err != nil {
		return err
	}
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := doAccountHTTPUpstream(s.httpUpstream, s.tlsFPProfileService, req, proxyURL, account, maxInt(account.Concurrency, 1))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, cnRecoveryVerifyMaxBodyBytes))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("upstream returned %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(body)), 240))
	}
	if cnRecoveryBodyIndicatesError(body) {
		return fmt.Errorf("upstream returned an error payload: %s", truncate(strings.TrimSpace(string(body)), 240))
	}
	return nil
}

// buildVerifyRequest 构造最小验证请求，协议与账号的生产路径同源：
// anthropic/adaptive → {base}/v1/messages；responses → /responses；其余 → /chat/completions。
// 使用非流式请求：恢复放行只需要一个确定的成功/失败信号，流式探测由 cap 探测负责。
func (s *AccountErrorRecoveryService) buildVerifyRequest(ctx context.Context, account *Account) (*http.Request, error) {
	apiKey := strings.TrimSpace(account.GetOpenAIProtocolAPIKey())
	if apiKey == "" {
		return nil, errors.New("account api key is empty")
	}

	var (
		targetURL   string
		authBaseURL string
		payload     map[string]any
	)
	protocol := account.GetAPIProtocol()
	switch protocol {
	case APIProtocolAnthropic, APIProtocolAdaptive:
		authBaseURL = strings.TrimSpace(account.GetAnthropicProtocolBaseURL())
		if authBaseURL == "" {
			return nil, errors.New("anthropic base url is empty")
		}
		targetURL = strings.TrimRight(authBaseURL, "/") + "/v1/messages"
		payload = map[string]any{
			"model":      account.GetMappedModel(claude.DefaultTestModel),
			"max_tokens": cnRecoveryVerifyMaxTokens,
			"messages": []map[string]any{
				{"role": "user", "content": cnRecoveryVerifyPrompt},
			},
		}
	case APIProtocolResponses:
		authBaseURL = strings.TrimSpace(account.GetCNProtocolBaseURL(APIProtocolResponses))
		if authBaseURL == "" {
			return nil, errors.New("responses base url is empty")
		}
		targetURL = buildOpenAIResponsesURLForPlatform(account.Platform, authBaseURL)
		payload = map[string]any{
			"model":             account.GetMappedModel(openai.DefaultTestModel),
			"input":             cnRecoveryVerifyPrompt,
			"max_output_tokens": cnRecoveryVerifyMaxTokens,
			"stream":            false,
			"store":             false,
		}
	default:
		authBaseURL = strings.TrimSpace(account.GetOpenAIBaseURL())
		if authBaseURL == "" {
			return nil, errors.New("chat completions base url is empty")
		}
		targetURL = strings.TrimRight(authBaseURL, "/") + "/chat/completions"
		payload = map[string]any{
			"model":      account.GetMappedModel(openai.DefaultTestModel),
			"max_tokens": cnRecoveryVerifyMaxTokens,
			"stream":     false,
			"messages": []map[string]any{
				{"role": "user", "content": cnRecoveryVerifyPrompt},
			},
		}
	}

	// 探测发起前过出站 URL 安全策略（与网关转发/额度探测同一套校验）。
	validatedURL, err := cnValidateProbeURL(s.cfg, targetURL)
	if err != nil {
		return nil, err
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, validatedURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if protocol == APIProtocolAnthropic || protocol == APIProtocolAdaptive {
		req.Header.Set("anthropic-version", "2023-06-01")
		for key, value := range claude.DefaultHeaders() {
			req.Header.Set(key, value)
		}
		setAnthropicAPIKeyAuthHeader(req.Header, account, apiKey, authBaseURL)
	} else {
		req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	account.ApplyHeaderOverrides(req.Header)
	return req, nil
}

// cnRecoveryBodyIndicatesError 识别 HTTP 2xx 却携带业务错误的响应体
// （anthropic 流内错误、OpenAI 兼容 error 字段、MiniMax base_resp）。
func cnRecoveryBodyIndicatesError(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	if gjson.GetBytes(body, "error").Exists() {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "type").String()), "error") {
		return true
	}
	if status := gjson.GetBytes(body, "base_resp.status_code"); status.Exists() && status.Int() != 0 {
		return true
	}
	return false
}

// cnRecoveryPlatforms 是参与自动归队的 CN 平台（快照额度键同源的三家；
// deepseek 为余额型，无 coding 套餐窗口）。
func cnRecoveryPlatforms() []string {
	return []string{PlatformKimi, PlatformZhipu, PlatformMiniMax}
}

// cnRecoveryBackoffReason* 仅用于日志与后续排查（退避表本身不区分原因）。
const (
	cnRecoveryBackoffReasonQuotaFull     = "quota_still_full"
	cnRecoveryBackoffReasonVerifyFailed  = "verify_failed"
	cnRecoveryBackoffReasonReloadFailed  = "reload_failed"
	cnRecoveryBackoffReasonRestoreFailed = "restore_failed"
)

func (s *AccountErrorRecoveryService) scheduleNextAttempt(accountID int64, now time.Time, reason string) {
	delay := s.advanceBackoff(accountID, now)
	log.Printf("[CNRecovery] account %d deferred (%s), next attempt in %s", accountID, reason, delay)
}

// scheduleAtQuotaReset 把下次检查排在快照给出的恢复时刻（所有已耗尽窗口中最晚的
// 重置点），而不是推进通用退避阶梯：「额度仍满」不是失败，翻倍退避会把归队推迟到
// 6h 封顶。快照 refresh 后若窗口实际未滚（上游延迟），下一轮会按新快照重新排点，
// 自我纠正。
func (s *AccountErrorRecoveryService) scheduleAtQuotaReset(accountID int64, account *Account, now time.Time, threshold float64) {
	at := cnQuotaNextResetAt(account, now, threshold)
	if at.IsZero() {
		// 判满即意味着存在未来重置点；兜底 10 分钟（防御，正常不会走到）。
		at = now.Add(10 * time.Minute)
	}
	s.mu.Lock()
	state := s.states[accountID]
	if state == nil {
		state = &cnRecoveryState{}
		s.states[accountID] = state
	}
	state.nextAttemptAt = at // 不动 state.failures：不消耗退避阶梯
	s.mu.Unlock()
	log.Printf("[CNRecovery] account %d deferred (quota_still_full), next attempt at %s", accountID, at.Format(time.RFC3339))
}

// cnQuotaNextResetAt 返回账号解除「窗口耗尽」状态的时刻：所有已耗尽窗口中**最晚**
// 的一个重置点（任一窗口仍满即未恢复）。没有已耗尽窗口时返回零值。
func cnQuotaNextResetAt(account *Account, now time.Time, threshold float64) time.Time {
	if account == nil || len(account.Extra) == 0 || threshold <= 0 {
		return time.Time{}
	}
	var latest time.Time
	provider := account.Platform
	for _, w := range []struct{ usedSuffix, resetSuffix string }{
		{cnExtraSuffix5hUsed, cnExtraSuffix5hReset},
		{cnExtraSuffixWeeklyUsed, cnExtraSuffixWeeklyReset},
	} {
		used := schedulingPercentValue(account.Extra[cnExtraKey(provider, w.usedSuffix)])
		if used < threshold {
			continue
		}
		reset := parseSchedulingResetAt(account.Extra[cnExtraKey(provider, w.resetSuffix)])
		if reset == nil || !reset.After(now) {
			continue
		}
		if reset.After(latest) {
			latest = *reset
		}
	}
	return latest
}

// due 报告账号是否已越过退避窗口。
func (s *AccountErrorRecoveryService) due(accountID int64, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.states[accountID]
	return state == nil || !now.Before(state.nextAttemptAt)
}

// advanceBackoff 推进退避档位并返回本次延迟。档位封顶在退避表最后一项（6h）。
func (s *AccountErrorRecoveryService) advanceBackoff(accountID int64, now time.Time) time.Duration {
	schedule := s.backoffSchedule()
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.states[accountID]
	if state == nil {
		state = &cnRecoveryState{}
		s.states[accountID] = state
	}
	index := state.failures
	if index > len(schedule)-1 {
		index = len(schedule) - 1
	}
	delay := schedule[index]
	state.nextAttemptAt = now.Add(delay)
	if state.failures < len(schedule)-1 {
		state.failures++
	}
	return delay
}

// requeue 让账号在下一轮重新参与，且不消耗退避预算（CAS 冲突语义）。
func (s *AccountErrorRecoveryService) requeue(accountID int64, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.states[accountID]
	if state == nil {
		state = &cnRecoveryState{}
		s.states[accountID] = state
	}
	state.nextAttemptAt = now
}

func (s *AccountErrorRecoveryService) clearState(accountID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.states, accountID)
}

// backoffSchedule 读取 gateway.concurrency_cap.recovery_probe_backoff
// （形如 "10m,20m,40m,6h"）；缺失/非法时回落内置默认表。
func (s *AccountErrorRecoveryService) backoffSchedule() []time.Duration {
	raw := ""
	if s != nil && s.cfg != nil {
		raw = s.cfg.Gateway.CNProviders.ErrorRecoveryBackoff
	}
	if parsed := parseCNRecoveryBackoff(raw); len(parsed) > 0 {
		return parsed
	}
	return defaultCNRecoveryBackoff
}

// parseCNRecoveryBackoff 解析逗号分隔的退避表。任一项非法即判定整表非法（返回 nil），
// 避免用户写错一项后静默退化成非预期档位。
func parseCNRecoveryBackoff(raw string) []time.Duration {
	items := strings.Split(raw, ",")
	out := make([]time.Duration, 0, len(items))
	for _, item := range items {
		trimmed := strings.TrimSpace(item)
		if trimmed == "" {
			continue
		}
		delay, err := time.ParseDuration(trimmed)
		if err != nil || delay <= 0 {
			return nil
		}
		out = append(out, delay)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *AccountErrorRecoveryService) leaderLockTTL() time.Duration {
	if s != nil && s.cfg != nil && s.cfg.Gateway.CNProviders.ErrorRecoveryLeaderLockTTL > 0 {
		return s.cfg.Gateway.CNProviders.ErrorRecoveryLeaderLockTTL
	}
	return cnRecoveryDefaultLeaderLockTTL
}

// roundBudget 返回单轮扫描预算：有协调后端时必须落在 leader 锁 TTL 之内
// （锁不续租）；单实例（无锁后端，runOnce 直接跑）用满上限。
func (s *AccountErrorRecoveryService) roundBudget() time.Duration {
	budget := cnRecoveryRoundBudget
	if s == nil || (s.lockCache == nil && s.db == nil) {
		return budget
	}
	ttl := s.leaderLockTTL()
	switch {
	case ttl > cnRecoveryLeaderLockMargin:
		if budget > ttl-cnRecoveryLeaderLockMargin {
			budget = ttl - cnRecoveryLeaderLockMargin
		}
	case ttl > 0:
		budget = ttl
	}
	return budget
}

func (s *AccountErrorRecoveryService) quotaExhaustedPercent() float64 {
	if s != nil && s.cfg != nil && s.cfg.Gateway.CNProviders.QuotaExhaustedPercent > 0 {
		return s.cfg.Gateway.CNProviders.QuotaExhaustedPercent
	}
	return 85
}

func (s *AccountErrorRecoveryService) currentTime() time.Time {
	if s != nil && s.now != nil {
		return s.now()
	}
	return time.Now()
}
