package service

// 并发上限状态机与周期调度（PLAN-cn-cap-v5 §四：72h 冷却 → 主动探测 → 阶梯回升 → 熔断）。
//
//	cap=1 且 restricted_at 满 72h → 探测（目标 2 条车道）
//	  ├ 通过        → cap=2，next_probe_at = now+12h
//	  ├ 并发受限失败 → cap 保持 1，next_probe_at = now+72h，flap+1
//	  └ 不确定      → 不推进、不计 flap，next_probe_at = now+1h
//	cap=2 且满 12h → 探测（目标 3 条车道）
//	  ├ 通过        → cap=3（cap_max，停止回升）
//	  ├ 失败        → cap=1，next_probe_at = now+72h，flap+1
//	  └ 不确定      → cap 保持 2，next_probe_at = now+1h
//
// 熔断：flap_events 滚动 7 天 ≥ fuse_flap_threshold → 停止自动回升 + 告警。
// pinned=true 的账号跳过自动回升与熔断判定（新撞 403 的降级仍由响应式路径写入）。
// 熔断账号仍会被扫描：熔断由 flap_events 派生，事件滑出 7 天窗口后自动解除。
//
// 状态转移是纯函数（planConcurrencyCapStep），IO 只发生在 applyPlan；周期任务骨架
// 参考 CNProviderBalanceCheckService，并复用 tryAcquireSingletonLeaderLock 做多实例互斥。

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	// concurrencyCapLeaderLockKey 是周期扫描的跨实例互斥键。
	concurrencyCapLeaderLockKey = "gateway:concurrency_cap:probe:leader"
	// concurrencyCapScanInterval 是周期扫描间隔。到期判定（72h/12h/1h）粒度远大于它；
	// 扫描本身只读 cap 记录（是否真正探测由 next_probe_at 决定）。
	concurrencyCapScanInterval = 30 * time.Second
	// concurrencyCapRunBudget 是单轮扫描总预算上限（多账号串行探测，单账号最坏 60s 占槽 + 30s 探测）。
	concurrencyCapRunBudget = 15 * time.Minute
	// concurrencyCapLeaderLockSafetyMargin / concurrencyCapMinRunBudget 保证单轮预算落在 leader 锁 TTL 内（见 concurrencyCapRunBudgetFor）。
	concurrencyCapLeaderLockSafetyMargin = 15 * time.Second
	concurrencyCapMinRunBudget           = 15 * time.Second
	// concurrencyCapFlapWindow 是 flap 熔断的滚动窗口（PLAN §四：滚动 7 天）。
	concurrencyCapFlapWindow = 7 * 24 * time.Hour
	// concurrencyCapFuseLogInterval 是同一账号熔断告警的最小间隔。
	concurrencyCapFuseLogInterval = time.Hour

	// 参数默认值（契约 5 配置缺失/为 0 时的兜底）。
	concurrencyCapDefaultRestricted             = 1
	concurrencyCapDefaultCapMax                 = 3
	concurrencyCapDefaultHoldHours              = 72
	concurrencyCapDefaultObserveHours           = 12
	concurrencyCapDefaultProbeMaxTokens         = 1
	concurrencyCapDefaultProbeTimeout           = 30 * time.Second
	concurrencyCapDefaultProbeDrainTimeout      = 10 * time.Second
	concurrencyCapDefaultInconclusiveRetryHours = 1
	concurrencyCapDefaultFuseFlapThreshold      = 3
	concurrencyCapDefaultLeaderLockTTL          = 90 * time.Second

	// capReason* 是写入 cap 记录的 reason，便于运维与降级路径区分。
	capReasonNoop              = ""
	capReasonProbePass         = "cap_probe_pass"
	capReasonProbeLimited      = "cap_probe_limited"
	capReasonProbeInconclusive = "cap_probe_inconclusive"
	capReasonProbeDeferred     = "cap_probe_deferred"
	capReasonFuseHold          = "cap_fuse_hold"
)

// capSkipReason 是本轮不探测的原因（跳过同样要保留在扫描集合里，见熔断解除语义）。
type capSkipReason string

const (
	capSkipDisabled capSkipReason = "disabled"
	capSkipPinned   capSkipReason = "pinned"
	capSkipCapMax   capSkipReason = "cap_max"
	capSkipFused    capSkipReason = "fused"
	capSkipNotDue   capSkipReason = "not_due"
)

// capRescheduleMode 描述 next_probe_at 的重排强度。
type capRescheduleMode int

const (
	// capRescheduleNone：无需重排（cap 变更由 store.SetCap 自带的规则负责）。
	capRescheduleNone capRescheduleMode = iota
	// capRescheduleRequired：cap 不变但必须落盘重排，否则记录一直 due，每轮扫描都会重复
	// 探测并把同一次失败重复计 flap（并发受限 / 不确定）。
	capRescheduleRequired
	// capRescheduleBestEffort：仅在 store 支持精确重排时写入（占槽推迟 / 熔断保持）；
	// 不支持时保持 due，由下一轮扫描快速重试（占槽重试无上游流量，成本有界）。
	capRescheduleBestEffort
)

// concurrencyCapSettings 是本服务消费的 gateway.concurrency_cap 参数快照。
// 配置读取集中在此结构，便于在配置项命名/类型变动时单点修正。
type concurrencyCapSettings struct {
	Enabled                bool
	Restricted             int
	CapMax                 int
	HoldWindow             time.Duration
	ObserveWindow          time.Duration
	ProbeProtocol          string
	ProbeEndpoint          string
	ProbeMaxTokens         int
	ProbeTimeout           time.Duration
	ProbeDrainTimeout      time.Duration
	ProbeInconclusiveRetry time.Duration
	FuseFlapThreshold      int
	LeaderLockTTL          time.Duration
}

// resolveConcurrencyCapSettings 读取配置并补齐默认值（契约 5）。
func resolveConcurrencyCapSettings(cfg *config.Config) concurrencyCapSettings {
	settings := concurrencyCapSettings{
		Enabled:                true,
		Restricted:             concurrencyCapDefaultRestricted,
		CapMax:                 concurrencyCapDefaultCapMax,
		HoldWindow:             time.Duration(concurrencyCapDefaultHoldHours) * time.Hour,
		ObserveWindow:          time.Duration(concurrencyCapDefaultObserveHours) * time.Hour,
		ProbeProtocol:          APIProtocolAnthropic,
		ProbeEndpoint:          concurrencyCapProbeDefaultEndpoint,
		ProbeMaxTokens:         concurrencyCapDefaultProbeMaxTokens,
		ProbeTimeout:           concurrencyCapDefaultProbeTimeout,
		ProbeDrainTimeout:      concurrencyCapDefaultProbeDrainTimeout,
		ProbeInconclusiveRetry: time.Duration(concurrencyCapDefaultInconclusiveRetryHours) * time.Hour,
		FuseFlapThreshold:      concurrencyCapDefaultFuseFlapThreshold,
		LeaderLockTTL:          concurrencyCapDefaultLeaderLockTTL,
	}
	if cfg == nil {
		return settings
	}
	capCfg := cfg.Gateway.ConcurrencyCap
	settings.Enabled = capCfg.Enabled
	if capCfg.Restricted > 0 {
		settings.Restricted = capCfg.Restricted
	}
	if capCfg.CapMax > 0 {
		settings.CapMax = capCfg.CapMax
	}
	if capCfg.HoldHours > 0 {
		settings.HoldWindow = time.Duration(capCfg.HoldHours) * time.Hour
	}
	if capCfg.ObserveHours > 0 {
		settings.ObserveWindow = time.Duration(capCfg.ObserveHours) * time.Hour
	}
	if protocol := strings.TrimSpace(capCfg.ProbeProtocol); protocol != "" {
		settings.ProbeProtocol = protocol
	}
	if endpoint := strings.TrimSpace(capCfg.ProbeEndpoint); endpoint != "" {
		settings.ProbeEndpoint = endpoint
	}
	if capCfg.ProbeMaxTokens > 0 {
		settings.ProbeMaxTokens = capCfg.ProbeMaxTokens
	}
	if capCfg.ProbeTimeout > 0 {
		settings.ProbeTimeout = capCfg.ProbeTimeout
	}
	if capCfg.ProbeDrainTimeout > 0 {
		settings.ProbeDrainTimeout = capCfg.ProbeDrainTimeout
	}
	if capCfg.ProbeInconclusiveRetryHours > 0 {
		settings.ProbeInconclusiveRetry = time.Duration(capCfg.ProbeInconclusiveRetryHours) * time.Hour
	}
	if capCfg.FuseFlapThreshold > 0 {
		settings.FuseFlapThreshold = capCfg.FuseFlapThreshold
	}
	if capCfg.LeaderLockTTL > 0 {
		settings.LeaderLockTTL = capCfg.LeaderLockTTL
	}
	if settings.Restricted < 1 {
		settings.Restricted = concurrencyCapDefaultRestricted
	}
	if settings.CapMax < 2 {
		// 阶梯回升至少需要 1 → 2 一档，否则状态机无意义。
		settings.CapMax = concurrencyCapDefaultCapMax
	}
	return settings
}

// capStepOutcome 是一次探测的结果（判定三档 + "未测"与"推迟"两个调度语义）。
type capStepOutcome int

const (
	// capStepOutcomeUnprobed：本轮未发起探测（调用方用于求解放行/跳过判定）。
	capStepOutcomeUnprobed capStepOutcome = iota
	// capStepOutcomeDeferred：槽位被用户流量占用，本轮推迟（不计 flap、不判失败）。
	capStepOutcomeDeferred
	// capStepOutcomePass：全部探测流成功。
	capStepOutcomePass
	// capStepOutcomeLimited：任一条命中"并发受限"（唯一计入 flap 的失败）。
	capStepOutcomeLimited
	// capStepOutcomeInconclusive：超时 / 5xx / 网络错误等，不推进、不计 flap。
	capStepOutcomeInconclusive
)

// capStepPlan 是状态机对一次探测结果的纯函数输出（无 IO，便于表驱动单测）。
type capStepPlan struct {
	// Skip=true 表示本轮不探测；SkipReason 说明原因。
	Skip       bool
	SkipReason capSkipReason
	// Reason 是写入 cap 记录的 reason。
	Reason string
	// OccupyLanes / TargetLanes 是探测前置：需要占满的当前 cap 槽位数与探测并发流条数。
	OccupyLanes int
	TargetLanes int
	// NextCap / MutateCap：cap 发生变化，需要调用 store.SetCap。
	NextCap   int
	MutateCap bool
	// NextProbeAt / Reschedule：next_probe_at 的重排模式（见 capRescheduleMode）。
	NextProbeAt time.Time
	Reschedule  capRescheduleMode
	// StopProbing=true 表示已到 cap_max，此后不再探测。
	StopProbing bool
	// RecordFlap=true 表示本次要写一条 flap 事件。
	RecordFlap bool
	// Raised=true 表示本次是一次成功的阶梯回升。
	Raised bool
}

// planConcurrencyCapStep 是状态机核心：纯函数，输入当前记录 + 探测结果，输出需要执行的
// cap / next_probe_at 变更与指标动作。outcome 为 capStepOutcomeUnprobed 时只做跳过判定。
func planConcurrencyCapStep(now time.Time, current AccountConcurrencyCap, outcome capStepOutcome, flapCount7d int, settings concurrencyCapSettings) capStepPlan {
	plan := capStepPlan{Reason: capReasonNoop, NextCap: current.Cap}
	if !settings.Enabled {
		plan.Skip, plan.SkipReason = true, capSkipDisabled
		return plan
	}

	currentCap := current.Cap
	if currentCap <= 0 {
		currentCap = settings.Restricted
	}
	plan.NextCap = currentCap
	plan.OccupyLanes = currentCap
	plan.TargetLanes = currentCap + 1
	if plan.TargetLanes > settings.CapMax {
		plan.TargetLanes = settings.CapMax
	}

	// pinned：人工接管，跳过自动回升与熔断判定（不影响降级路径）。
	if current.Pinned {
		plan.Skip, plan.SkipReason = true, capSkipPinned
		return plan
	}
	if currentCap >= settings.CapMax {
		plan.Skip, plan.SkipReason = true, capSkipCapMax
		return plan
	}
	// 熔断只在"是否发起探测"这一层生效：已探测出的结果必须照常落地。
	if outcome == capStepOutcomeUnprobed && flapCount7d >= settings.FuseFlapThreshold {
		plan.Skip, plan.SkipReason = true, capSkipFused
		plan.Reason = capReasonFuseHold
		// 熔断由 flap_events 滚动窗口派生，无需显式解除状态位；这里只把下次检查推迟到
		// 重试间隔，避免每轮扫描重复告警。
		plan.Reschedule = capRescheduleBestEffort
		plan.NextProbeAt = now.Add(settings.ProbeInconclusiveRetry)
		return plan
	}
	if !current.NextProbeAt.IsZero() && current.NextProbeAt.After(now) {
		plan.Skip, plan.SkipReason = true, capSkipNotDue
		return plan
	}

	switch outcome {
	case capStepOutcomePass:
		plan.Raised = true
		plan.Reason = capReasonProbePass
		if currentCap < 2 {
			plan.NextCap, plan.MutateCap = 2, true
			plan.NextProbeAt, plan.Reschedule = now.Add(settings.ObserveWindow), capRescheduleBestEffort
			return plan
		}
		plan.NextCap, plan.MutateCap = settings.CapMax, true
		plan.StopProbing = true
		return plan
	case capStepOutcomeLimited:
		plan.Reason = capReasonProbeLimited
		plan.RecordFlap = true
		plan.NextProbeAt, plan.Reschedule = now.Add(settings.HoldWindow), capRescheduleRequired
		plan.NextCap = settings.Restricted
		plan.MutateCap = plan.NextCap != currentCap
		return plan
	case capStepOutcomeInconclusive:
		plan.Reason = capReasonProbeInconclusive
		plan.NextProbeAt, plan.Reschedule = now.Add(settings.ProbeInconclusiveRetry), capRescheduleRequired
		return plan
	case capStepOutcomeDeferred:
		plan.Reason = capReasonProbeDeferred
		plan.NextProbeAt, plan.Reschedule = now.Add(settings.ProbeDrainTimeout), capRescheduleBestEffort
		return plan
	default:
		// capStepOutcomeUnprobed 且未命中任何跳过条件：语义上等于"还不到期"。
		plan.Skip, plan.SkipReason = true, capSkipNotDue
		return plan
	}
}

// rollingFlapCount 统计滚动窗口内（含边界）的 flap 事件数。
// flap 用事件时间戳数组而非单计数器：事件滑出 7 天窗口后熔断才能自动解除。
func rollingFlapCount(events []time.Time, now time.Time, window time.Duration) int {
	if len(events) == 0 {
		return 0
	}
	cutoff := now.Add(-window)
	count := 0
	for _, event := range events {
		if event.IsZero() || event.Before(cutoff) {
			continue
		}
		count++
	}
	return count
}

// capProbeRescheduleStore 是契约 1 之外的可选扩展：SetCap 只能表达"cap 变化 + 按 store
// 规则重排"，无法表达"cap 不变但需要精确重排 next_probe_at"（不确定 1h / 占槽失败 10s /
// 熔断保持 1h）。store 实现该方法时探测编排使用精确时间；未实现时按 capRescheduleMode
// 退化（见 applyPlan 注释），不会误计 flap。
type capProbeRescheduleStore interface {
	SetNextProbeAt(ctx context.Context, accountID int64, at time.Time, reason string) error
}

// ConcurrencyCapService 周期扫描受限账号并按状态机推进 cap。
type ConcurrencyCapService struct {
	store    ConcurrencyCapStore
	accounts AccountRepository
	probe    *ConcurrencyCapProbe
	cfg      *config.Config
	metrics  *ConcurrencyCapProbeMetrics

	interval time.Duration
	now      func() time.Time

	lockCache  LeaderLockCache
	db         *sql.DB
	instanceID string

	fuseLogMu    sync.Mutex
	fuseLoggedAt map[int64]time.Time

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewConcurrencyCapService 构造周期回升服务（store 由契约 1 提供）。
func NewConcurrencyCapService(
	store ConcurrencyCapStore,
	accounts AccountRepository,
	concurrency *ConcurrencyService,
	proxyRepo ProxyRepository,
	upstream HTTPUpstream,
	cfg *config.Config,
) *ConcurrencyCapService {
	metrics := NewConcurrencyCapProbeMetrics()
	return &ConcurrencyCapService{
		store:        store,
		accounts:     accounts,
		probe:        NewConcurrencyCapProbe(concurrency, proxyRepo, upstream, cfg, metrics),
		cfg:          cfg,
		metrics:      metrics,
		interval:     concurrencyCapScanInterval,
		now:          time.Now,
		instanceID:   uuid.NewString(),
		fuseLoggedAt: make(map[int64]time.Time),
		stopCh:       make(chan struct{}),
	}
}

// SetLeaderLock 注入 leader 锁与 DB。两者都为 nil 时按单实例直接运行
// （tryAcquireSingletonLeaderLock 的无协调后端语义）。
func (s *ConcurrencyCapService) SetLeaderLock(lockCache LeaderLockCache, db *sql.DB) {
	if s == nil {
		return
	}
	s.lockCache = lockCache
	s.db = db
}

// Metrics 返回进程内指标集合（cap_probe_total / cap_raise_total / cap_flap_total / cap_fuse_total）。
func (s *ConcurrencyCapService) Metrics() *ConcurrencyCapProbeMetrics {
	if s == nil {
		return nil
	}
	return s.metrics
}

// Start 启动周期扫描。interval <= 0 时不启动（便于测试与关闭）。
func (s *ConcurrencyCapService) Start() {
	if s == nil || s.store == nil || s.accounts == nil || s.probe == nil || s.cfg == nil {
		return
	}
	settings := s.settings()
	if !settings.Enabled {
		logger.L().Info("concurrency_cap_service_disabled")
		return
	}
	if s.interval <= 0 {
		return
	}
	logger.L().Info("concurrency_cap_service_started",
		zap.Duration("interval", s.interval),
		zap.Duration("leader_lock_ttl", settings.LeaderLockTTL),
	)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		// 启动后先等一个周期再扫描，避免与进程启动峰重叠（同 CNProviderBalanceCheckService）。
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

// Stop 停止周期扫描并等待在途轮次结束。
func (s *ConcurrencyCapService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	s.wg.Wait()
}

func (s *ConcurrencyCapService) settings() concurrencyCapSettings {
	return resolveConcurrencyCapSettings(s.cfg)
}

// runOnce 扫描 ListRestricted 并对到期账号执行探测编排。
func (s *ConcurrencyCapService) runOnce() {
	if s == nil || s.store == nil || s.accounts == nil || s.cfg == nil {
		return
	}
	settings := s.settings()
	if !settings.Enabled {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), concurrencyCapRunBudgetFor(settings))
	defer cancel()

	// 多实例互斥：TTL 仅作崩溃兜底（正常路径在轮次结束即释放），需大于单轮最坏耗时。
	release, ok := tryAcquireSingletonLeaderLock(ctx, s.lockCache, s.db, concurrencyCapLeaderLockKey, s.instanceID, settings.LeaderLockTTL)
	if !ok {
		return
	}
	defer release()

	records, err := s.store.ListRestricted(ctx)
	if err != nil {
		logger.L().Warn("concurrency_cap_list_restricted_failed", zap.Error(err))
		return
	}
	if len(records) == 0 {
		return
	}

	ids := make([]int64, 0, len(records))
	for _, record := range records {
		if record != nil && record.AccountID > 0 {
			ids = append(ids, record.AccountID)
		}
	}
	accounts, err := s.accounts.GetByIDs(ctx, ids)
	if err != nil {
		logger.L().Warn("concurrency_cap_load_accounts_failed", zap.Error(err))
		return
	}
	byID := make(map[int64]*Account, len(accounts))
	for _, account := range accounts {
		if account != nil {
			byID[account.ID] = account
		}
	}

	now := s.currentTime()
	for _, record := range records {
		if record == nil || record.AccountID <= 0 {
			continue
		}
		if ctx.Err() != nil {
			// 预算耗尽：剩余账号保持 due，交给下一轮。
			logger.L().Warn("concurrency_cap_scan_budget_exhausted", zap.Int64("pending_account_id", record.AccountID))
			return
		}
		account := byID[record.AccountID]
		if account == nil || !account.IsCNProvider() {
			continue
		}
		s.processRecord(ctx, settings, record, account, now)
	}
}

// processRecord 推进单个账号：跳过判定 → 探测 → 判定三档 → 状态转移。
func (s *ConcurrencyCapService) processRecord(ctx context.Context, settings concurrencyCapSettings, record *AccountConcurrencyCap, account *Account, now time.Time) {
	flapCount7d := rollingFlapCount(record.FlapEvents, now, concurrencyCapFlapWindow)
	pre := planConcurrencyCapStep(now, *record, capStepOutcomeUnprobed, flapCount7d, settings)
	if pre.Skip {
		s.handleSkip(ctx, record, pre, flapCount7d, now)
		return
	}

	outcome := s.probe.Run(ctx, account, pre.OccupyLanes, pre.TargetLanes, settings)
	if outcome == capStepOutcomeLimited {
		count, err := s.store.RecordFlap(ctx, record.AccountID, now)
		if err != nil {
			logger.L().Warn("concurrency_cap_record_flap_failed",
				zap.Int64("account_id", record.AccountID),
				zap.Error(err),
			)
		} else {
			flapCount7d = count
			s.metrics.IncFlap()
			if flapCount7d >= settings.FuseFlapThreshold {
				s.metrics.IncFuse()
				logger.L().Warn("concurrency_cap_fused",
					zap.Int64("account_id", record.AccountID),
					zap.Int("flap_count_7d", flapCount7d),
					zap.Int("threshold", settings.FuseFlapThreshold),
				)
			}
		}
	}

	plan := planConcurrencyCapStep(now, *record, outcome, flapCount7d, settings)
	s.applyPlan(ctx, record, plan)
}

// handleSkip 记录并落地跳过分支（pinned / cap_max / 熔断 / 未到期）。
func (s *ConcurrencyCapService) handleSkip(ctx context.Context, record *AccountConcurrencyCap, plan capStepPlan, flapCount7d int, now time.Time) {
	switch plan.SkipReason {
	case capSkipFused:
		if s.shouldLogFuse(record.AccountID, now) {
			logger.L().Warn("concurrency_cap_fuse_hold",
				zap.Int64("account_id", record.AccountID),
				zap.Int("flap_count_7d", flapCount7d),
				zap.String("reason", plan.Reason),
			)
		}
		if plan.Reschedule != capRescheduleNone {
			s.setNextProbeAt(ctx, record.AccountID, plan.NextProbeAt, plan.Reason)
		}
	case capSkipPinned:
		logger.L().Debug("concurrency_cap_skip_pinned", zap.Int64("account_id", record.AccountID))
	}
}

// applyPlan 落地状态机输出。
//
// next_probe_at 的写入口径：
//   - cap 变化 → SetCap（store 按其规则重排）；
//   - cap 不变且 Reschedule=required（并发受限 / 不确定）→ 必须写：优先精确方法，
//     否则退回 SetCap 让 store 重排（不写会让记录一直 due，每轮扫描重复探测并重复计 flap）；
//   - cap 不变且 Reschedule=best-effort（占槽推迟 / 熔断保持）→ 只在 store 支持精确重排时写，
//     否则保持 due 由下一轮扫描快速重试。
func (s *ConcurrencyCapService) applyPlan(ctx context.Context, record *AccountConcurrencyCap, plan capStepPlan) {
	if plan.MutateCap {
		if err := s.store.SetCap(ctx, record.AccountID, plan.NextCap, plan.Reason); err != nil {
			logger.L().Warn("concurrency_cap_set_cap_failed",
				zap.Int64("account_id", record.AccountID),
				zap.Int("cap", plan.NextCap),
				zap.String("reason", plan.Reason),
				zap.Error(err),
			)
			return
		}
		if plan.Raised {
			s.metrics.IncRaise()
			logger.L().Info("concurrency_cap_raised",
				zap.Int64("account_id", record.AccountID),
				zap.Int("from_cap", record.Cap),
				zap.Int("to_cap", plan.NextCap),
			)
		}
	}
	if plan.StopProbing || plan.Reschedule == capRescheduleNone {
		return
	}
	if s.setNextProbeAt(ctx, record.AccountID, plan.NextProbeAt, plan.Reason) {
		return
	}
	if plan.MutateCap || plan.Reschedule != capRescheduleRequired {
		return
	}
	if err := s.store.SetCap(ctx, record.AccountID, plan.NextCap, plan.Reason); err != nil {
		logger.L().Warn("concurrency_cap_reschedule_fallback_failed",
			zap.Int64("account_id", record.AccountID),
			zap.String("reason", plan.Reason),
			zap.Error(err),
		)
	}
}

// setNextProbeAt 精确推进 next_probe_at；返回 false 表示 store 未提供该能力。
func (s *ConcurrencyCapService) setNextProbeAt(ctx context.Context, accountID int64, at time.Time, reason string) bool {
	rescheduler, ok := s.store.(capProbeRescheduleStore)
	if !ok || rescheduler == nil {
		logger.L().Debug("concurrency_cap_reschedule_unsupported",
			zap.Int64("account_id", accountID),
			zap.String("reason", reason),
		)
		return false
	}
	if err := rescheduler.SetNextProbeAt(ctx, accountID, at, reason); err != nil {
		logger.L().Warn("concurrency_cap_reschedule_failed",
			zap.Int64("account_id", accountID),
			zap.Time("next_probe_at", at),
			zap.String("reason", reason),
			zap.Error(err),
		)
	}
	return true
}

// shouldLogFuse 限制同账号熔断告警频率（熔断按小时复查，避免刷屏）。
func (s *ConcurrencyCapService) shouldLogFuse(accountID int64, now time.Time) bool {
	s.fuseLogMu.Lock()
	defer s.fuseLogMu.Unlock()
	if s.fuseLoggedAt == nil {
		s.fuseLoggedAt = make(map[int64]time.Time)
	}
	if last, ok := s.fuseLoggedAt[accountID]; ok && now.Sub(last) < concurrencyCapFuseLogInterval {
		return false
	}
	s.fuseLoggedAt[accountID] = now
	return true
}

// concurrencyCapRunBudgetFor 返回单轮扫描预算。
//
// 预算必须小于 leader 锁 TTL：LeaderLockCache 只提供 TryAcquire/Release（无续租），
// 若单轮超出 TTL，锁会在扫描中途过期，另一实例可能同时开始扫描。宁可少扫几个账号
// （剩余账号保持 due，下一轮继续），也不让临界区越过租约。
func concurrencyCapRunBudgetFor(settings concurrencyCapSettings) time.Duration {
	budget := concurrencyCapRunBudget
	if settings.LeaderLockTTL > 0 {
		leaseBudget := settings.LeaderLockTTL - concurrencyCapLeaderLockSafetyMargin
		if leaseBudget < concurrencyCapMinRunBudget {
			leaseBudget = concurrencyCapMinRunBudget
		}
		if leaseBudget < budget {
			budget = leaseBudget
		}
	}
	return budget
}

// currentTime 返回当前时间（时钟可注入，便于测试）。
func (s *ConcurrencyCapService) currentTime() time.Time {
	if s == nil || s.now == nil {
		return time.Now()
	}
	return s.now()
}
