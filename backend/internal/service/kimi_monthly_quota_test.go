//go:build unit

package service

// 【二开】Kimi Token Plan 月额度套餐（5h + 月额度）识别测试。
// Kimi 有两种套餐：5h + 周额度、5h + 月额度；/coding/v1/usages 响应样本取自 2026-10-16
// 生产实测（去掉 booster_wallet 等无关字段）。月窗口用满 100% 才停调（5h / 周仍按阈值 85）。

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

const kimiMonthlyPlanUsagesBody = `{"limits":[{"window":{"duration":300,"timeUnit":"TIME_UNIT_MINUTE"},"detail":{"limit":"100","used":"1","remaining":"99","resetTime":"2026-10-16T07:01:51.137599Z"}}],"usages":{"limit_5h":{"used_ratio":0.0089,"reset_time":"2026-10-16T07:01:50Z"},"limit_month_total":{"used_ratio":0.4607,"reset_time":"2026-11-10T00:00:00Z"},"limit_month_code":{"used_ratio":0.4607,"reset_time":"2026-11-10T00:00:00Z"}}}`

const kimiWeeklyPlanUsagesBody = `{"usage":{"limit":"100","used":"59","remaining":"41","resetTime":"2026-10-20T02:33:34.266003Z"},"limits":[{"window":{"duration":300,"timeUnit":"TIME_UNIT_MINUTE"},"detail":{"limit":"100","remaining":"100","resetTime":"2026-10-16T07:33:34.266003Z"}}],"usages":{"limit_5h":{"used_ratio":0.0003,"reset_time":"2026-10-16T07:33:34Z"},"limit_7d":{"used_ratio":0.5913,"reset_time":"2026-10-20T02:33:34Z"}}}`

const kimiMonthlyLimitForbiddenBody = `{"error":{"type":"permission_error","message":"You've reached your monthly usage limit for this billing cycle. Your quota will be refreshed in the next cycle. To continue now, purchase extra usage or upgrade your plan: https://www.kimi.com/membership/subscription?tab=quota"}}`

func tierByWindow(tiers []CNQuotaTier, window string) (CNQuotaTier, bool) {
	for _, tier := range tiers {
		if tier.Window == window {
			return tier, true
		}
	}
	return CNQuotaTier{}, false
}

func TestParseKimiUsageTiers_MonthlyPlan(t *testing.T) {
	t.Parallel()
	tiers := parseKimiUsageTiers([]byte(kimiMonthlyPlanUsagesBody))
	require.Len(t, tiers, 2, "月额度套餐只有 5h + monthly，不能再产出 weekly")

	fiveHour, ok := tierByWindow(tiers, "5h")
	require.True(t, ok)
	require.InDelta(t, 0.89, fiveHour.UsedPercent, 1e-9)
	require.Equal(t, "2026-10-16T07:01:50Z", fiveHour.ResetAt)

	monthly, ok := tierByWindow(tiers, "monthly")
	require.True(t, ok)
	require.InDelta(t, 46.07, monthly.UsedPercent, 1e-9)
	require.Equal(t, "2026-11-10T00:00:00Z", monthly.ResetAt)
}

func TestParseKimiUsageTiers_MonthlyTakesHigherOfTotalAndCode(t *testing.T) {
	t.Parallel()
	body := []byte(`{"usages":{"limit_5h":{"used_ratio":0,"reset_time":"2026-10-16T11:04:23Z"},
		"limit_month_total":{"used_ratio":0.3,"reset_time":"2026-10-28T00:00:00Z"},
		"limit_month_code":{"used_ratio":1,"reset_time":"2026-10-28T00:00:00Z"}}}`)
	monthly, ok := tierByWindow(parseKimiUsageTiers(body), "monthly")
	require.True(t, ok)
	require.InDelta(t, 100.0, monthly.UsedPercent, 1e-9, "total / code 任一用满都会被拒，取较高者")
	require.Equal(t, "2026-10-28T00:00:00Z", monthly.ResetAt)

	onlyCode := []byte(`{"usages":{"limit_month_code":{"used_ratio":"0.5","reset_time":"2026-10-28T00:00:00Z"}}}`)
	monthly, ok = tierByWindow(parseKimiUsageTiers(onlyCode), "monthly")
	require.True(t, ok)
	require.InDelta(t, 50.0, monthly.UsedPercent, 1e-9)
}

func TestParseKimiUsageTiers_WeeklyPlanPrefersUsages(t *testing.T) {
	t.Parallel()
	tiers := parseKimiUsageTiers([]byte(kimiWeeklyPlanUsagesBody))
	require.Len(t, tiers, 2)
	require.Equal(t, "5h", tiers[0].Window)
	require.InDelta(t, 0.03, tiers[0].UsedPercent, 1e-9)
	require.Equal(t, "weekly", tiers[1].Window)
	require.InDelta(t, 59.13, tiers[1].UsedPercent, 1e-9)
	require.Equal(t, "2026-10-20T02:33:34Z", tiers[1].ResetAt)
	_, hasMonthly := tierByWindow(tiers, "monthly")
	require.False(t, hasMonthly)
}

func TestParseKimiUsageTiers_EmptyBody(t *testing.T) {
	t.Parallel()
	require.Empty(t, parseKimiUsageTiers([]byte(`{}`)))
}

func TestCNQuotaExtraUpdates_KimiPlanSwitchClearsStaleWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 6, 0, 0, 0, time.UTC)

	monthly := cnQuotaExtraUpdates(PlatformKimi, parseKimiUsageTiers([]byte(kimiMonthlyPlanUsagesBody)), now)
	require.InDelta(t, 46.07, monthly["kimi_monthly_used_percent"], 1e-9)
	require.Equal(t, "2026-11-10T00:00:00Z", monthly["kimi_monthly_reset_at"])
	require.Contains(t, monthly, "kimi_weekly_used_percent")
	require.Nil(t, monthly["kimi_weekly_used_percent"], "月额度套餐清掉旧的周快照")
	require.Contains(t, monthly, "kimi_weekly_reset_at")
	require.Nil(t, monthly["kimi_weekly_reset_at"])

	weekly := cnQuotaExtraUpdates(PlatformKimi, parseKimiUsageTiers([]byte(kimiWeeklyPlanUsagesBody)), now)
	require.InDelta(t, 59.13, weekly["kimi_weekly_used_percent"], 1e-9)
	require.Contains(t, weekly, "kimi_monthly_used_percent")
	require.Nil(t, weekly["kimi_monthly_used_percent"], "周额度套餐清掉旧的月快照")
	require.Nil(t, weekly["kimi_monthly_reset_at"])

	// 只拿到 5h（或空响应）无法判断套餐 → 不清任何长窗口。
	onlyFiveHour := cnQuotaExtraUpdates(PlatformKimi, []CNQuotaTier{{Window: "5h", UsedPercent: 1}}, now)
	require.NotContains(t, onlyFiveHour, "kimi_weekly_used_percent")
	require.NotContains(t, onlyFiveHour, "kimi_monthly_used_percent")

	// 其它平台行为不变。
	zhipu := cnQuotaExtraUpdates(PlatformZhipu, []CNQuotaTier{{Window: "weekly", UsedPercent: 1}}, now)
	require.NotContains(t, zhipu, "zhipu_monthly_used_percent")
}

func TestClassifyCNUpstreamError_KimiMonthlyQuota(t *testing.T) {
	t.Parallel()
	body := []byte(kimiMonthlyLimitForbiddenBody)
	require.Equal(t, UpstreamErrorQuotaExhausted, ClassifyCNUpstreamError(PlatformKimi, http.StatusForbidden, body),
		"月额度用完必须走额度停调，不能进 403 计数被永久置 error")
	require.Equal(t, UpstreamErrorQuotaExhausted, ClassifyCNUpstreamError(PlatformKimi, http.StatusTooManyRequests, body))
	require.Equal(t, "monthly", cnQuotaWordingWindow(body))
	require.Equal(t, "", cnQuotaWordingWindow([]byte(`{"error":{"message":"monthly plan renewed"}}`)), "只有窗口标识不足以判额度耗尽")
}

func kimiMonthlyPlanExtra(now time.Time, monthlyUsed float64, monthlyReset time.Time, fiveHourUsed float64) map[string]any {
	return map[string]any{
		cnExtraKey(PlatformKimi, cnExtraSuffixUsageUpdated): now.Add(-time.Minute).Format(time.RFC3339),
		cnExtraKey(PlatformKimi, cnExtraSuffix5hUsed):       fiveHourUsed,
		cnExtraKey(PlatformKimi, cnExtraSuffix5hReset):      now.Add(2 * time.Hour).Format(time.RFC3339),
		cnExtraKey(PlatformKimi, cnExtraSuffixMonthlyUsed):  monthlyUsed,
		cnExtraKey(PlatformKimi, cnExtraSuffixMonthlyReset): monthlyReset.Format(time.RFC3339),
		// 换套餐后被清掉的周快照（JSON null）。
		cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyUsed):  nil,
		cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyReset): nil,
	}
}

func TestCNQuotaPauseWindowAt_KimiMonthly(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	monthlyReset := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)

	window, resumeAt, ok := cnQuotaPauseWindowAt(cnRecoveryKimiCodingAccount(now, kimiMonthlyPlanExtra(now, 100, monthlyReset, 0)), now, 85)
	require.True(t, ok)
	require.Equal(t, "monthly", window)
	require.True(t, monthlyReset.Equal(resumeAt), "月满按月重置点停调")

	window, resumeAt, ok = cnQuotaPauseWindowAt(cnRecoveryKimiCodingAccount(now, kimiMonthlyPlanExtra(now, 100, monthlyReset, 99)), now, 85)
	require.True(t, ok)
	require.Equal(t, "monthly", window, "月满优先于 5h 满")
	require.True(t, monthlyReset.Equal(resumeAt))

	window, resumeAt, ok = cnQuotaPauseWindowAt(cnRecoveryKimiCodingAccount(now, kimiMonthlyPlanExtra(now, 40, monthlyReset, 99)), now, 85)
	require.True(t, ok)
	require.Equal(t, "5h", window, "月未满且 5h 满按 5h")
	require.True(t, now.Add(2*time.Hour).Truncate(time.Second).Equal(resumeAt))

	window, _, ok = cnQuotaPauseWindowAt(cnRecoveryKimiCodingAccount(now, kimiMonthlyPlanExtra(now, 99.9, monthlyReset, 99)), now, 85)
	require.True(t, ok)
	require.Equal(t, "5h", window, "月窗口 100% 才算满：99.9% 不按月停，5h 满仍按 5h")

	_, _, ok = cnQuotaPauseWindowAt(cnRecoveryKimiCodingAccount(now, kimiMonthlyPlanExtra(now, 95, monthlyReset, 0)), now, 85)
	require.False(t, ok, "月用量过 85% 但没用满 → 不停调")

	_, _, ok = cnQuotaPauseWindowAt(cnRecoveryKimiCodingAccount(now, kimiMonthlyPlanExtra(now, 100, now.Add(-time.Hour), 99)), now, 85)
	require.False(t, ok, "月满但重置点已过：快照是旧窗口的，不回落到 5h")

	require.True(t, cnQuotaSnapshotHasFutureReset(cnRecoveryKimiCodingAccount(now, map[string]any{
		cnExtraKey(PlatformKimi, cnExtraSuffixMonthlyReset): monthlyReset.Format(time.RFC3339),
	}), now), "只有月重置点也算可判定")
}

func TestCNQuotaRecovery_KimiMonthly(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	monthlyReset := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)

	full := cnRecoveryKimiCodingAccount(now, kimiMonthlyPlanExtra(now, 100, monthlyReset, 0))
	recovered, known := cnQuotaRecoveredFromSnapshot(full, now, 85)
	require.True(t, known)
	require.False(t, recovered, "月额度仍满即未恢复")
	require.True(t, monthlyReset.Equal(cnQuotaNextResetAt(full, now, 85)))
	require.True(t, cnProviderQuotaSnapshotExhausted(full, now, 85, cnQuotaSnapshotMaxAge))

	notFull := cnRecoveryKimiCodingAccount(now, kimiMonthlyPlanExtra(now, 95, monthlyReset, 0))
	recovered, known = cnQuotaRecoveredFromSnapshot(notFull, now, 85)
	require.True(t, known)
	require.True(t, recovered, "月用量 95% 没用满即可归队")
	require.True(t, cnQuotaNextResetAt(notFull, now, 85).IsZero())

	rolled := cnRecoveryKimiCodingAccount(now, kimiMonthlyPlanExtra(now, 100, now.Add(-time.Hour), 0))
	recovered, known = cnQuotaRecoveredFromSnapshot(rolled, now, 85)
	require.True(t, known)
	require.True(t, recovered, "月重置点已过即已恢复")

	onlyNullResets := cnRecoveryKimiCodingAccount(now, map[string]any{
		cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyUsed):  nil,
		cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyReset): nil,
	})
	_, known = cnQuotaRecoveredFromSnapshot(onlyNullResets, now, 85)
	require.False(t, known, "被清成 null 的重置键等同缺失")
}

func TestAccountErrorRecoveryService_KimiMonthlyFullSchedulesAtMonthlyReset(t *testing.T) {
	now := time.Now()
	monthlyReset := now.Add(12 * 24 * time.Hour).Truncate(time.Second)
	account := cnRecoveryKimiCodingAccount(now.Add(-time.Hour), kimiMonthlyPlanExtra(now, 100, monthlyReset, 0))
	repo := &cnRecoveryRepoStub{
		disabled:  []*Account{account},
		byID:      map[int64]*Account{account.ID: account},
		restoreOK: true,
	}
	upstream := &cnRecoveryUpstreamStub{body: `{"id":"msg_1"}`}

	svc := newCNRecoveryTestService(t, now, repo, upstream)
	svc.runOnce()

	require.Zero(t, upstream.calls, "月额度仍满时不发验证请求")
	require.False(t, svc.due(account.ID, monthlyReset.Add(-time.Minute)))
	require.True(t, svc.due(account.ID, monthlyReset.Add(time.Minute)))
	require.Empty(t, repo.restoreCalls)
}

func TestEvaluateAccountSchedulingThreshold_KimiMonthlyPlan(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	monthlyReset := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	account := &Account{Platform: PlatformKimi, Extra: kimiMonthlyPlanExtra(now, 100, monthlyReset, 10)}

	decision := EvaluateAccountSchedulingThreshold(account, map[string]int{PlatformKimi: 85}, now)
	require.True(t, decision.ShouldPause)
	require.Equal(t, "monthly", decision.Window)
	require.InDelta(t, 100.0, decision.UsedPercent, 1e-9)
	require.NotNil(t, decision.Until)
	require.True(t, monthlyReset.Equal(*decision.Until))

	account.Extra = kimiMonthlyPlanExtra(now, 99.5, monthlyReset, 10)
	require.False(t, EvaluateAccountSchedulingThreshold(account, map[string]int{PlatformKimi: 85}, now).ShouldPause,
		"月额度没用满不停调，平台阈值 85 只管 5h / 周")

	account.Extra = kimiMonthlyPlanExtra(now, 50, monthlyReset, 90)
	decision = EvaluateAccountSchedulingThreshold(account, map[string]int{PlatformKimi: 85}, now)
	require.True(t, decision.ShouldPause)
	require.Equal(t, "5h", decision.Window, "5h 仍按平台阈值 85")
}

func TestEvaluateAccountSchedulingThreshold_OpenCodeGoMonthlyKeepsPlatformThreshold(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	account := &Account{Platform: PlatformOpenCodeGo, Extra: map[string]any{
		"opencode_go_monthly_used_percent": 90.0,
		"opencode_go_monthly_reset_at":     now.Add(10 * 24 * time.Hour).Format(time.RFC3339),
	}}
	decision := EvaluateAccountSchedulingThreshold(account, map[string]int{PlatformOpenCodeGo: 85}, now)
	require.True(t, decision.ShouldPause, "OpenCode Go 月窗口沿用上游平台阈值")
	require.Equal(t, "monthly", decision.Window)
}

func TestRateLimitService_KimiMonthly403PausesToMonthlyReset(t *testing.T) {
	t.Parallel()
	now := time.Now()
	monthlyReset := now.Add(12 * 24 * time.Hour)
	// 快照滞后：月用量还停在 99.6%（未到 100% 判满线），但上游已明确回月额度用满。
	account := cn403CodingAccount(kimiMonthlyPlanExtra(now, 99.6, monthlyReset, 5))
	repo := &cn403RepoStub{}
	svc := newCN403TestService(repo)
	counter := svc.openAI403CounterCache.(*openAI403CounterCacheStub)

	shouldDisable := svc.HandleUpstreamError(context.Background(), account, http.StatusForbidden, http.Header{}, []byte(kimiMonthlyLimitForbiddenBody))

	require.True(t, shouldDisable, "月额度用满的请求仍必须换号")
	require.Len(t, repo.rateLimitedIfLater, 1)
	require.WithinDuration(t, monthlyReset, repo.rateLimitedIfLater[0], 2*time.Second, "按月重置点停调，不是 60s 短冷却")
	require.Zero(t, repo.setErrorCalls, "月额度用满不得永久禁用账号")
	require.Equal(t, []int64{openAI403DisableThreshold}, counter.counts, "月额度文案命中不得消耗 403 计数")
}

func TestRateLimitService_KimiMonthly403WithoutSnapshotUsesShortCooldown(t *testing.T) {
	t.Parallel()
	account := cn403CodingAccount(nil)
	repo := &cn403RepoStub{}
	svc := newCN403TestService(repo)

	before := time.Now()
	require.True(t, svc.HandleUpstreamError(context.Background(), account, http.StatusForbidden, http.Header{}, []byte(kimiMonthlyLimitForbiddenBody)))
	require.Len(t, repo.rateLimitedIfLater, 1)
	require.WithinDuration(t, before.Add(60*time.Second), repo.rateLimitedIfLater[0], 3*time.Second, "没有月重置点 → 短冷却等快照刷新")
	require.Zero(t, repo.setErrorCalls)
}

// newCN403FullThresholdTestService 对齐生产配置：gateway.cn_providers.quota_exhausted_percent = 100。
func newCN403FullThresholdTestService(repo AccountRepository) *RateLimitService {
	cfg := &config.Config{}
	cfg.Gateway.CNProviders.QuotaExhaustedPercent = 100
	svc := NewRateLimitService(repo, nil, cfg, nil, nil)
	svc.SetOpenAI403CounterCache(&openAI403CounterCacheStub{counts: []int64{openAI403DisableThreshold}})
	return svc
}

func TestRateLimitService_CNQuota403WordingWindowCoversLaggingSnapshot(t *testing.T) {
	t.Parallel()
	now := time.Now()
	weeklyReset := now.Add(3 * 24 * time.Hour)
	fiveHourReset := now.Add(2 * time.Hour)

	cases := map[string]struct {
		extra map[string]any
		body  string
		want  time.Time
	}{
		"周文案 + 快照周还停在 99.5% → 按周重置点": {
			extra: map[string]any{
				"kimi_usage_updated_at":    now.Format(time.RFC3339),
				"kimi_weekly_used_percent": 99.5,
				"kimi_weekly_reset_at":     weeklyReset.Format(time.RFC3339),
				"kimi_5h_used_percent":     10.0,
				"kimi_5h_reset_at":         fiveHourReset.Format(time.RFC3339),
			},
			body: cn403WeeklyQuotaBody,
			want: weeklyReset,
		},
		"5h 文案 + 快照 5h 还停在 99% → 按 5h 重置点": {
			extra: map[string]any{
				"kimi_usage_updated_at":    now.Format(time.RFC3339),
				"kimi_weekly_used_percent": 40.0,
				"kimi_weekly_reset_at":     weeklyReset.Format(time.RFC3339),
				"kimi_5h_used_percent":     99.0,
				"kimi_5h_reset_at":         fiveHourReset.Format(time.RFC3339),
			},
			body: cn403FiveHourQuotaBody,
			want: fiveHourReset,
		},
		"5h 文案但快照周已用满 → 取较晚的周重置点": {
			extra: map[string]any{
				"kimi_usage_updated_at":    now.Format(time.RFC3339),
				"kimi_weekly_used_percent": 100.0,
				"kimi_weekly_reset_at":     weeklyReset.Format(time.RFC3339),
				"kimi_5h_used_percent":     99.0,
				"kimi_5h_reset_at":         fiveHourReset.Format(time.RFC3339),
			},
			body: cn403FiveHourQuotaBody,
			want: weeklyReset,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			account := cn403CodingAccount(tc.extra)
			repo := &cn403RepoStub{}
			svc := newCN403FullThresholdTestService(repo)

			require.True(t, svc.HandleUpstreamError(context.Background(), account, http.StatusForbidden, http.Header{}, []byte(tc.body)))
			require.Len(t, repo.rateLimitedIfLater, 1)
			require.WithinDuration(t, tc.want, repo.rateLimitedIfLater[0], 2*time.Second)
			require.Zero(t, repo.setErrorCalls)
		})
	}
}

func TestCNQuotaPauseWindowAt_FullThresholdOnlyStopsAtHundred(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	account := cnRecoveryKimiCodingAccount(now, map[string]any{
		"kimi_weekly_used_percent": 99.0,
		"kimi_weekly_reset_at":     now.Add(48 * time.Hour).Format(time.RFC3339),
		"kimi_5h_used_percent":     99.0,
		"kimi_5h_reset_at":         now.Add(2 * time.Hour).Format(time.RFC3339),
	})
	_, _, ok := cnQuotaPauseWindowAt(account, now, 100)
	require.False(t, ok, "生产判满线 100：99% 不停调")

	account.Extra["kimi_weekly_used_percent"] = 100.0
	window, _, ok := cnQuotaPauseWindowAt(account, now, 100)
	require.True(t, ok)
	require.Equal(t, "weekly", window)
}
