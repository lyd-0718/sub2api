package service

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// 国产供应商（kimi/zhipu/deepseek）的响应式冷却辅助。
//
// 与 openai/anthropic 不同：
//   - 余额不足是「可恢复」状态（充值/检测恢复后自动重新调度），不能走 handleAuthError
//     永久置 status=error。这里改为 SetTempUnschedulable，由 CN 余额检测周期任务
//     （cn_provider_balance_check_service.go）在余额恢复后 ClearTempUnschedulable。
//   - Coding Plan 滚动窗口耗尽（429/403）的冷却终点应是真实的窗口重置时间（已由
//     CNProviderQuotaService 落入 account.Extra 快照），并按「周满看周、周未满看 5h」
//     分档（cn_quota_pause_window.go），而非默认的秒级兜底或「取最早重置点」。
//   - 并发超限与额度耗尽的 403 由 ratelimit_classifier.go 统一分类后进入本文件：
//     并发 → cap=1 + 30s 临时停车；额度 → 按窗口停调。两者都不得进入 403 计数。

// cnBalanceExtraSuffixLow 标记账号响应过「余额不足」，供余额检测任务区分
// 「确属余额不足」与「尚未探测」。
const cnBalanceExtraSuffixLow = "balance_low"

// cnBalanceLowReasonPrefix 是余额不足临时停调 reason 的稳定前缀。
// 周期余额检测任务据此识别「是我们停调的」并在余额恢复后安全清除——不会误清
// 其他子系统（阈值/限流/401）写入的临时停调。
const cnBalanceLowReasonPrefix = "cn_balance_low"

const kimiConcurrentRequestLimitMessage = "You've reached your concurrent request limit. Please wait for your ongoing requests to finish and try again."

const cnConcurrencyLimitReasonPrefix = "cn_concurrency_limit"

// cnConcurrencyCapReason 是撞并发 403 后把有效并发降为 1 时写入 cap store 的原因
// （管理端展示受限原因时按它区分自动降级与人工覆盖）。
const cnConcurrencyCapReason = "concurrent_403"

func (s *RateLimitService) handleCNProviderConcurrencyLimit403(
	ctx context.Context,
	account *Account,
) {
	// 撞并发 403 → 该账号有效并发降为受限档（cap），等待后续探测阶梯回升。
	// SetCap 是幂等的（同值重写只推进版本号），去重由调用方的副作用幂等键负责。
	restricted := 1
	if s.cfg != nil && s.cfg.Gateway.ConcurrencyCap.Restricted > 0 {
		restricted = s.cfg.Gateway.ConcurrencyCap.Restricted
	}
	switch {
	case s.concurrencyCapStore == nil:
		// store 未接线（wire 漏注入）时不能让 cap 静默不写：显式暴露配置缺口。
		slog.Warn("cn_concurrency_cap_store_missing", "account_id", account.ID, "platform", account.Platform)
	default:
		// flap 口径②：从更高档位被打回受限档（回升后 72h 内又撞）也计 flap；
		// 原本就在受限档的重复撞击、以及首次受限（无记录）不计。
		if record, err := s.concurrencyCapStore.GetCap(ctx, account.ID); err == nil && record != nil && record.Cap > restricted {
			if _, ferr := s.concurrencyCapStore.RecordFlap(ctx, account.ID, time.Now()); ferr != nil {
				slog.Warn("cn_concurrency_cap_flap_failed", "account_id", account.ID, "error", ferr)
			}
		}
		if err := s.concurrencyCapStore.SetCap(ctx, account.ID, restricted, cnConcurrencyCapReason); err != nil {
			slog.Warn("cn_concurrency_cap_set_failed", "account_id", account.ID, "error", err)
		}
	}
	// 并发超限是秒级瞬时信号（在途流结束即释放槽位），用短冷却而非 403 默认的
	// 10 分钟——长冷却会引发级联停车（停 A → 压 B → B 也超限），号池快速缩编。
	until := time.Now().Add(s.cnConcurrencyLimitCooldown())
	reason := cnConcurrencyLimitReasonPrefix + ": " + kimiConcurrentRequestLimitMessage
	s.notifyAccountSchedulingBlocked(account, until, cnConcurrencyLimitReasonPrefix)
	if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, reason); err != nil {
		slog.Warn("cn_concurrency_limit_set_temp_unschedulable_failed", "account_id", account.ID, "error", err)
		return
	}
	slog.Info("cn_provider_concurrency_limited",
		"account_id", account.ID,
		"platform", account.Platform,
		"until", until.UTC(),
	)
}

// handleCNProviderQuotaExhausted403 处理「额度耗尽」文案命中的 403（HTTP 与流内同口径）：
// 按窗口分档停调到真实重置点（周满按周、否则 5h）；快照缺失/无未来重置点时退化为
// 60s 短冷却 + 等下一轮周期额度探测刷新快照。
//
// 本函数是「额度类命中即早退」的落点：调用方必须 return，绝不能再进入 handleOpenAI403
// 计数——额度耗尽是必然随窗口重置恢复的状态，计数到 3 次会把账号永久置 error，
// 那正是本方案要消除的永久卡死。
func (s *RateLimitService) handleCNProviderQuotaExhausted403(ctx context.Context, account *Account, body []byte) {
	wordingWindow := cnQuotaWordingWindow(body)
	if window, resumeAt, ok := s.cnQuotaPauseWindow(account); ok {
		s.notifyAccountSchedulingBlocked(account, resumeAt, cnQuotaExhaustedReasonPrefix)
		if err := s.accountRepo.SetRateLimitedIfLater(ctx, account.ID, resumeAt); err != nil {
			slog.Warn("cn_quota_exhausted_set_rate_limited_failed", "account_id", account.ID, "error", err)
			return
		}
		slog.Info("cn_provider_quota_exhausted_paused",
			"account_id", account.ID,
			"platform", account.Platform,
			"window", window,
			"wording_window", wordingWindow,
			"reset_at", resumeAt.UTC(),
		)
		return
	}
	// 快照缺失/过期：恢复时间未知。短冷却 + 等周期额度探测刷新快照（探测目标覆盖全部
	// active coding 账号），下一轮拿到快照后按窗口正常停调。绝不能落回 403 计数。
	until := time.Now().Add(s.cn429TransientCooldown())
	s.notifyAccountSchedulingBlocked(account, until, cnQuotaRefreshReasonPrefix)
	if err := s.accountRepo.SetRateLimitedIfLater(ctx, account.ID, until); err != nil {
		slog.Warn("cn_quota_refresh_set_rate_limited_failed", "account_id", account.ID, "error", err)
		return
	}
	slog.Warn("cn_provider_quota_pause_snapshot_missing",
		"account_id", account.ID,
		"platform", account.Platform,
		"wording_window", wordingWindow,
		"cooldown_until", until.UTC(),
		"reason", cnQuotaRefreshReasonPrefix,
	)
}

// cnBalanceLowReason 构造余额不足临时停调的 reason（带稳定前缀）。
func cnBalanceLowReason(upstreamMsg string) string {
	if upstreamMsg = strings.TrimSpace(upstreamMsg); upstreamMsg != "" {
		return cnBalanceLowReasonPrefix + ": " + upstreamMsg
	}
	return cnBalanceLowReasonPrefix + ": 余额不足，账号临时停调"
}

// cnProviderResponseIndicatesInsufficientBalance 通过响应体文案识别余额不足
// （智谱 payg 无独立余额端点，仅能靠响应文案识别）。
func cnProviderResponseIndicatesInsufficientBalance(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	s := strings.ToLower(string(body))
	return strings.Contains(s, "余额不足") ||
		strings.Contains(s, "insufficient balance") ||
		strings.Contains(s, "insufficient_credit") ||
		strings.Contains(s, "balance is not enough") ||
		strings.Contains(s, "no enough balance")
}

// handleCNProviderInsufficientBalance 把余额不足标记为可恢复的临时停调：
// 写入 balance_low 快照 + SetTempUnschedulable 一个余额检测周期，
// 由周期任务在余额恢复后清除。返回前已通知调度阻塞。
func (s *RateLimitService) handleCNProviderInsufficientBalance(
	ctx context.Context,
	account *Account,
	upstreamMsg string,
) {
	msg := cnBalanceLowReason(upstreamMsg)

	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
		cnExtraKey(account.Platform, cnBalanceExtraSuffixLow): true,
	}); err != nil {
		slog.Warn("cn_balance_low_mark_failed", "account_id", account.ID, "error", err)
	}

	until := time.Now().Add(s.cnBalanceCooldownDuration())
	s.notifyAccountSchedulingBlocked(account, until, "cn_insufficient_balance")
	if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, msg); err != nil {
		slog.Warn("cn_balance_set_temp_unschedulable_failed", "account_id", account.ID, "error", err)
		return
	}
	slog.Info("cn_provider_insufficient_balance",
		"account_id", account.ID,
		"platform", account.Platform,
		"until", until.UTC(),
	)
}

// cnBalanceCooldownDuration 返回余额不足临时停调的持续时长（= 2× 余额检测周期，
// 默认 20 分钟）。周期任务会在余额恢复后提前清除，故此处只需保证冷却覆盖到下一次
// 周期检测即可。
func (s *RateLimitService) cnBalanceCooldownDuration() time.Duration {
	minutes := 10
	if s != nil && s.cfg != nil {
		if cfgMin := s.cfg.Gateway.CNProviders.BalanceCheckIntervalMinutes; cfgMin > 0 {
			minutes = cfgMin
		}
	}
	cooldown := time.Duration(minutes) * time.Minute * 2
	if cooldown < time.Minute {
		cooldown = 10 * time.Minute
	}
	return cooldown
}

// cn429TransientCooldown 返回「窗口未耗尽/恢复时间未知」时的短冷却时长
// （默认 60s）。429 的瞬时并发限流与 403 额度文案命中但快照缺失都走它。
func (s *RateLimitService) cn429TransientCooldown() time.Duration {
	seconds := 60
	if s != nil && s.cfg != nil && s.cfg.Gateway.CNProviders.RateLimitCooldownSeconds > 0 {
		seconds = s.cfg.Gateway.CNProviders.RateLimitCooldownSeconds
	}
	return time.Duration(seconds) * time.Second
}

// cnConcurrencyLimitCooldown 返回 kimi 并发超限的冷却时长（秒级瞬时信号，默认 30s）。
func (s *RateLimitService) cnConcurrencyLimitCooldown() time.Duration {
	seconds := 30
	if s != nil && s.cfg != nil && s.cfg.Gateway.CNProviders.ConcurrencyLimitCooldownSeconds > 0 {
		seconds = s.cfg.Gateway.CNProviders.ConcurrencyLimitCooldownSeconds
	}
	return time.Duration(seconds) * time.Second
}

// cn429QuotaExhaustedPercent 返回判定窗口耗尽的用量百分比阈值（默认 85）。
func (s *RateLimitService) cn429QuotaExhaustedPercent() float64 {
	if s != nil && s.cfg != nil && s.cfg.Gateway.CNProviders.QuotaExhaustedPercent > 0 {
		return s.cfg.Gateway.CNProviders.QuotaExhaustedPercent
	}
	return 85
}

// cnProviderQuotaSnapshotExhausted 判断账号快照是否给出「窗口耗尽」证据：
// 5h 或 weekly 窗口用量 ≥ threshold 且该窗口尚未重置，且快照在 maxAge 内刷新过。
// 快照缺失/过期时不认定耗尽——瞬时并发限流不应按窗口耗尽长停调。
//
// 消费方：历史账号恢复（account_error_recovery_service.go）用它判定「额度是否仍未恢复」。
// 注意它回答的是「是否仍有窗口处于耗尽状态」（任一窗口命中即为真），与停调终点无关；
// 需要停调终点一律用 cnQuotaPauseWindow（周满看周的分档规则）。
func cnProviderQuotaSnapshotExhausted(account *Account, now time.Time, threshold float64, maxAge time.Duration) bool {
	if account == nil || len(account.Extra) == 0 || threshold <= 0 {
		return false
	}
	if !cnQuotaSnapshotFresh(account, now, maxAge) {
		return false
	}
	provider := account.Platform
	for _, w := range []struct {
		usedSuffix  string
		resetSuffix string
	}{
		{cnExtraSuffix5hUsed, cnExtraSuffix5hReset},
		{cnExtraSuffixWeeklyUsed, cnExtraSuffixWeeklyReset},
	} {
		used := schedulingPercentValue(account.Extra[cnExtraKey(provider, w.usedSuffix)])
		if used < threshold {
			continue
		}
		// 用量超阈值但窗口已重置 → 快照是旧窗口的，不算耗尽。
		if reset := parseSchedulingResetAt(account.Extra[cnExtraKey(provider, w.resetSuffix)]); reset == nil || !reset.After(now) {
			continue
		}
		return true
	}
	return false
}

// cnCodingPlan429Cooldown 决定 Coding Plan 账号 429 的冷却终点。
// 返回 (冷却终点, 是否窗口耗尽, 是否可判定)。
//
// 可判定 = 快照里至少一个窗口带未来重置点（否则视同快照缺失，调用方走默认 429 逻辑）；
// 窗口耗尽 = 周/5h 用量 ≥ threshold 且该窗口尚未重置 → 停到 cnQuotaPauseWindowAt 的
// 分档结果（周满按周、周未满且 5h 满按 5h）；无耗尽证据（瞬时并发限流）→ 短冷却，
// 避免号池因误停而缩编。
func cnCodingPlan429Cooldown(account *Account, now time.Time, threshold float64, maxAge, transient time.Duration) (time.Time, bool, bool) {
	if !cnQuotaSnapshotHasFutureReset(account, now) {
		return time.Time{}, false, false
	}
	if !cnQuotaSnapshotFresh(account, now, maxAge) {
		return now.Add(transient), false, true
	}
	if _, resumeAt, ok := cnQuotaPauseWindowAt(account, now, threshold); ok {
		return resumeAt, true, true
	}
	return now.Add(transient), false, true
}

// applyCNProviderReactive429 处理国产供应商的 429 响应。
// 返回 true 表示已处理（调用方应 return），false 表示未命中、继续走默认 429 逻辑。
func (s *RateLimitService) applyCNProviderReactive429(
	ctx context.Context,
	account *Account,
	headers http.Header,
	responseBody []byte,
) bool {
	if !account.IsCNProvider() {
		return false
	}
	// 1) 余额不足文案：可恢复临时停调（含智谱 payg 这类无余额端点的场景）。
	if cnProviderResponseIndicatesInsufficientBalance(responseBody) {
		s.handleCNProviderInsufficientBalance(ctx, account, extractUpstreamErrorMessage(responseBody))
		return true
	}
	// 2) Coding Plan 429：按快照证据区分「窗口耗尽」与「瞬时并发限流」。
	// 耗尽时停到分档窗口重置点（周满按周、周未满且 5h 满按 5h），写入用
	// SetRateLimitedIfLater（单调推进），避免退回到更早的重置点。
	if account.IsCodingPlan() {
		if cooldownUntil, exhausted, ok := cnCodingPlan429Cooldown(account, time.Now(), s.cn429QuotaExhaustedPercent(), cnQuotaSnapshotMaxAge, s.cn429TransientCooldown()); ok {
			s.notifyAccountSchedulingBlocked(account, cooldownUntil, "429")
			if err := s.accountRepo.SetRateLimitedIfLater(ctx, account.ID, cooldownUntil); err != nil {
				slog.Warn("rate_limit_set_failed", "account_id", account.ID, "error", err)
				return true
			}
			slog.Info("cn_coding_plan_rate_limited",
				"account_id", account.ID,
				"platform", account.Platform,
				"reset_at", cooldownUntil,
				"quota_exhausted", exhausted,
			)
			return true
		}
	}
	return false
}
