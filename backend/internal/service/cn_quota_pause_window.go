package service

import (
	"fmt"
	"strings"
	"time"
)

// Coding Plan 额度耗尽后的停调窗口判定。
//
// 需求方规则（长窗口优先，不是取最早重置点）：
//
//	月额度用满（100%）          → 按【月窗口重置时间】停调（Kimi 5h + 月额度套餐，见 cnMonthlyQuotaExhaustedPercent）
//	周额度已满（≥ 阈值）        → 按【周窗口重置时间】停调
//	周/月未满 且 5h 额度已满     → 按【5h 窗口重置时间】停调
//	都未满                       → 不停调
//
// 注意不要把停调终点改回「5h/weekly 中取最早的未来重置点」：那是为「避免过度停调」
// 的反向取舍，周满时会停到 5h 重置点 → 5h 重置后立刻再撞 403，形成停调/放行抖动。
// 本函数必须按实际把请求打回的那个窗口停调（周满看周），因此独立实现。

const (
	// cnQuotaExhaustedReasonPrefix 是按窗口停调（长停）的稳定 reason 前缀。
	cnQuotaExhaustedReasonPrefix = "cn_quota_exhausted"
	// cnQuotaRefreshReasonPrefix 是快照缺失时短冷却的稳定 reason 前缀：
	// 语义是「额度已耗尽但恢复时间未知，等下一轮周期额度探测刷新快照」。
	cnQuotaRefreshReasonPrefix = "cn_quota_refresh"
	// cnQuotaSnapshotMaxAge 是快照可用于「窗口耗尽」判定的最大时长。
	// 更旧的快照只描述过去，不构成当前耗尽的证据（瞬时并发限流不应按窗口长停）。
	cnQuotaSnapshotMaxAge = 30 * time.Minute
)

// cnQuotaWindow 描述一个额度快照窗口在 account.Extra 中的键后缀。
type cnQuotaWindow struct {
	window      string
	usedSuffix  string
	resetSuffix string
}

// cnQuotaWindows 是 CN Coding Plan 额度快照的全部窗口，按停调优先级排序（周期越长越先判）。
// 【二开】Kimi 两种套餐（5h + 周 / 5h + 月）各只写其中一个长窗口，缺失的窗口读出来是 0 用量、
// 无重置点，自然跳过；zhipu/minimax 不写 monthly，行为不变。
var cnQuotaWindows = []cnQuotaWindow{
	{"monthly", cnExtraSuffixMonthlyUsed, cnExtraSuffixMonthlyReset},
	{"weekly", cnExtraSuffixWeeklyUsed, cnExtraSuffixWeeklyReset},
	{"5h", cnExtraSuffix5hUsed, cnExtraSuffix5hReset},
}

// cnMonthlyQuotaExhaustedPercent 是月窗口的判满阈值：用满 100% 才停调。
// 【二开】需求方 2026-10-16 拍板：月额度按 85% 提前停调会白白浪费整月剩余额度；
// 月额度撞 403 已被分类器识别为额度耗尽、按月重置点停调（不进 403 计数、不会永久 error），
// 代价只是一次失败转移。5h / 周窗口仍用可配置阈值（默认 85）。
const cnMonthlyQuotaExhaustedPercent = 100.0

// exhaustedPercent 返回该窗口的判满阈值：月窗口固定 100，其它窗口用调用方传入的阈值。
func (w cnQuotaWindow) exhaustedPercent(threshold float64) float64 {
	if w.window == "monthly" {
		return cnMonthlyQuotaExhaustedPercent
	}
	return threshold
}

// cnQuotaWindowByName 按窗口名（"monthly" / "weekly" / "5h"）查找快照窗口定义。
func cnQuotaWindowByName(name string) (cnQuotaWindow, bool) {
	for _, w := range cnQuotaWindows {
		if w.window == name {
			return w, true
		}
	}
	return cnQuotaWindow{}, false
}

// cnQuotaWindowFutureReset 返回指定窗口仍在未来的重置点（缺失/已过返回 false）。
func cnQuotaWindowFutureReset(account *Account, resetSuffix string, now time.Time) (time.Time, bool) {
	if account == nil {
		return time.Time{}, false
	}
	reset := parseSchedulingResetAt(account.Extra[cnExtraKey(account.Platform, resetSuffix)])
	if reset == nil || !reset.After(now) {
		return time.Time{}, false
	}
	return *reset, true
}

// cnQuotaPauseWindow 返回额度耗尽后的停调窗口与恢复时间（阈值取
// gateway.cn_providers.quota_exhausted_percent，默认 85）。
// ok=false 表示快照没有给出「已满且仍未重置」的窗口——调用方必须退化为短冷却，
// 绝不能落回 403 计数。
func (s *RateLimitService) cnQuotaPauseWindow(account *Account) (window string, resumeAt time.Time, ok bool) {
	return cnQuotaPauseWindowAt(account, time.Now(), s.cn429QuotaExhaustedPercent())
}

// cnQuotaPauseWindowAt 是 cnQuotaPauseWindow 的纯函数内核（便于表驱动单测）。
//
// 分档顺序即优先级：月、周窗口先判。某窗口用量 ≥ 阈值但重置点缺失/已过期时立即停止下探
// （而不是继续看 5h）：用量高 + 无未来重置点说明快照是旧窗口的，此时任何停调终点都
// 没有依据——交给调用方短冷却 + 等快照刷新，避免用另一个窗口的重置点凑一个错误的终点。
func cnQuotaPauseWindowAt(account *Account, now time.Time, thresholdPercent float64) (window string, resumeAt time.Time, ok bool) {
	if account == nil || !account.IsCNProvider() || thresholdPercent <= 0 {
		return "", time.Time{}, false
	}
	for _, candidate := range cnQuotaWindows {
		used := schedulingPercentValue(account.Extra[cnExtraKey(account.Platform, candidate.usedSuffix)])
		if used < candidate.exhaustedPercent(thresholdPercent) {
			continue
		}
		reset := parseSchedulingResetAt(account.Extra[cnExtraKey(account.Platform, candidate.resetSuffix)])
		if reset == nil || !reset.After(now) {
			return "", time.Time{}, false
		}
		return candidate.window, *reset, true
	}
	return "", time.Time{}, false
}

// cnQuotaSnapshotHasFutureReset 报告快照里是否至少有一个窗口仍带未来重置点。
// 429 路径用它区分两种「不可判定」：快照缺失（回落默认 429 逻辑）与快照存在但未耗尽
// （短冷却）。payg 账号无窗口快照，恒 false。
func cnQuotaSnapshotHasFutureReset(account *Account, now time.Time) bool {
	if account == nil || !account.IsCNProvider() || !account.IsCodingPlan() {
		return false
	}
	for _, w := range cnQuotaWindows {
		if reset := parseSchedulingResetAt(account.Extra[cnExtraKey(account.Platform, w.resetSuffix)]); reset != nil && reset.After(now) {
			return true
		}
	}
	return false
}

// cnQuotaSnapshotFresh 判断用量快照是否在 maxAge 内刷新过（锚点是
// usage_updated_at，由 CNProviderQuotaService 写入）。缺键/不可解析视为不新鲜。
func cnQuotaSnapshotFresh(account *Account, now time.Time, maxAge time.Duration) bool {
	if maxAge <= 0 {
		return true
	}
	if account == nil {
		return false
	}
	raw := strings.TrimSpace(fmt.Sprint(account.Extra[cnExtraKey(account.Platform, cnExtraSuffixUsageUpdated)]))
	updatedAt, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return false
	}
	return now.Sub(updatedAt) <= maxAge
}
