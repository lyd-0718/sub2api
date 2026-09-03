package service

import (
	"context"
	"fmt"
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
//   - Coding Plan 滚动窗口耗尽（429）的冷却终点应是真实的窗口重置时间（已由
//     CNProviderQuotaService 落入 account.Extra 快照），而非默认的秒级兜底。

// cnBalanceExtraSuffixLow 标记账号响应过「余额不足」，供余额检测任务区分
// 「确属余额不足」与「尚未探测」。
const cnBalanceExtraSuffixLow = "balance_low"

// cnBalanceLowReasonPrefix 是余额不足临时停调 reason 的稳定前缀。
// 周期余额检测任务据此识别「是我们停调的」并在余额恢复后安全清除——不会误清
// 其他子系统（阈值/限流/401）写入的临时停调。
const cnBalanceLowReasonPrefix = "cn_balance_low"

const kimiConcurrentRequestLimitMessage = "You've reached your concurrent request limit. Please wait for your ongoing requests to finish and try again."

const cnConcurrencyLimitReasonPrefix = "cn_concurrency_limit"

func isCNProviderConcurrencyLimit403(account *Account, upstreamMsg string) bool {
	return account != nil && account.Platform == PlatformKimi &&
		strings.TrimSpace(upstreamMsg) == kimiConcurrentRequestLimitMessage
}

func (s *RateLimitService) handleCNProviderConcurrencyLimit403(
	ctx context.Context,
	account *Account,
) {
	until := time.Now().Add(time.Duration(openAI403CooldownMinutesDefault) * time.Minute)
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

// cnProviderQuotaSnapshotReset 读取 Coding Plan 账号快照中最早一个仍在未来的窗口
// 重置时间（5h / weekly）。429 多数由 5h 滚动窗口触发，取较早的重置点可避免
// 把账号冷却到 weekly 重置（可达数天）的过度停调；如果确是 weekly 窗口耗尽，
// 周期额度探测刷新快照后阈值评估会再次停调到正确的时间点。
// 无快照或均已过期返回 nil。
func cnProviderQuotaSnapshotReset(account *Account, now time.Time) *time.Time {
	if account == nil || !account.IsCNProvider() || !account.IsCodingPlan() || len(account.Extra) == 0 {
		return nil
	}
	provider := account.Platform
	var earliest *time.Time
	for _, suffix := range []string{cnExtraSuffix5hReset, cnExtraSuffixWeeklyReset} {
		t := parseSchedulingResetAt(account.Extra[cnExtraKey(provider, suffix)])
		if t == nil || !t.After(now) {
			continue
		}
		if earliest == nil || t.Before(*earliest) {
			earliest = t
		}
	}
	return earliest
}

// cnProviderQuotaSnapshotExhausted 判断账号快照是否给出「窗口耗尽」证据：
// 5h 或 weekly 窗口用量 ≥ threshold 且该窗口尚未重置。快照缺失/过期（超过
// maxAge 未刷新）时不认定耗尽——瞬时并发 429 不应按窗口耗尽长停调。
func cnProviderQuotaSnapshotExhausted(account *Account, now time.Time, threshold float64, maxAge time.Duration) bool {
	if account == nil || len(account.Extra) == 0 || threshold <= 0 {
		return false
	}
	provider := account.Platform
	updatedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(fmt.Sprint(account.Extra[cnExtraKey(provider, cnExtraSuffixUsageUpdated)])))
	if err != nil {
		return false
	}
	if maxAge > 0 && now.Sub(updatedAt) > maxAge {
		return false
	}
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

// cn429TransientCooldown 返回无耗尽证据时 429 的短冷却时长。
func (s *RateLimitService) cn429TransientCooldown() time.Duration {
	seconds := 60
	if s != nil && s.cfg != nil && s.cfg.Gateway.CNProviders.RateLimitCooldownSeconds > 0 {
		seconds = s.cfg.Gateway.CNProviders.RateLimitCooldownSeconds
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

// cnCodingPlan429Cooldown 决定 Coding Plan 账号 429 的冷却终点。
// 返回 (冷却终点, 是否窗口耗尽, 是否可判定)。窗口无未来重置点时 ok=false
// （调用方继续走默认 429 逻辑）。有耗尽证据 → 停到最早窗口重置点；
// 无证据（瞬时并发限流）→ 短冷却，避免号池因误停而缩编。
func cnCodingPlan429Cooldown(account *Account, now time.Time, threshold float64, maxAge, transient time.Duration) (time.Time, bool, bool) {
	until := cnProviderQuotaSnapshotReset(account, now)
	if until == nil {
		return time.Time{}, false, false
	}
	if cnProviderQuotaSnapshotExhausted(account, now, threshold, maxAge) {
		return *until, true, true
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
	if account.IsCodingPlan() {
		if cooldownUntil, exhausted, ok := cnCodingPlan429Cooldown(account, time.Now(), s.cn429QuotaExhaustedPercent(), 30*time.Minute, s.cn429TransientCooldown()); ok {
			s.notifyAccountSchedulingBlocked(account, cooldownUntil, "429")
			if err := s.accountRepo.SetRateLimited(ctx, account.ID, cooldownUntil); err != nil {
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
