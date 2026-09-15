//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func cnPauseWindowAccount(extra map[string]any) *Account {
	return &Account{
		ID:          9001,
		Platform:    PlatformKimi,
		Credentials: map[string]any{"account_mode": AccountModeCoding},
		Extra:       extra,
	}
}

// TestCNQuotaPauseWindowAt 覆盖需求方指定的分档规则：周满按周、周未满且 5h 满按 5h、
// 都未满不停调；周满但周窗口无未来重置点时不退而求其次用 5h。
func TestCNQuotaPauseWindowAt(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	weeklyReset := now.Add(48 * time.Hour)
	fiveHourReset := now.Add(3 * time.Hour)
	past := now.Add(-time.Hour)

	cases := map[string]struct {
		extra      map[string]any
		wantWindow string
		wantReset  time.Time
		wantOK     bool
	}{
		"周满按周重置点": {
			extra: map[string]any{
				"kimi_weekly_used_percent": 92.0,
				"kimi_weekly_reset_at":     weeklyReset.Format(time.RFC3339),
				"kimi_5h_used_percent":     96.0,
				"kimi_5h_reset_at":         fiveHourReset.Format(time.RFC3339),
			},
			wantWindow: "weekly",
			wantReset:  weeklyReset,
			wantOK:     true,
		},
		"周未满且 5h 满按 5h 重置点": {
			extra: map[string]any{
				"kimi_weekly_used_percent": 10.0,
				"kimi_weekly_reset_at":     weeklyReset.Format(time.RFC3339),
				"kimi_5h_used_percent":     96.0,
				"kimi_5h_reset_at":         fiveHourReset.Format(time.RFC3339),
			},
			wantWindow: "5h",
			wantReset:  fiveHourReset,
			wantOK:     true,
		},
		"周满但周重置点已过期时不回落到 5h": {
			extra: map[string]any{
				"kimi_weekly_used_percent": 92.0,
				"kimi_weekly_reset_at":     past.Format(time.RFC3339),
				"kimi_5h_used_percent":     96.0,
				"kimi_5h_reset_at":         fiveHourReset.Format(time.RFC3339),
			},
			wantOK: false,
		},
		"周满但缺周重置点时不回落到 5h": {
			extra: map[string]any{
				"kimi_weekly_used_percent": 92.0,
				"kimi_5h_used_percent":     96.0,
				"kimi_5h_reset_at":         fiveHourReset.Format(time.RFC3339),
			},
			wantOK: false,
		},
		"都未满不停调": {
			extra: map[string]any{
				"kimi_weekly_used_percent": 10.0,
				"kimi_weekly_reset_at":     weeklyReset.Format(time.RFC3339),
				"kimi_5h_used_percent":     20.0,
				"kimi_5h_reset_at":         fiveHourReset.Format(time.RFC3339),
			},
			wantOK: false,
		},
		"用量等于阈值算满": {
			extra: map[string]any{
				"kimi_weekly_used_percent": 85.0,
				"kimi_weekly_reset_at":     weeklyReset.Format(time.RFC3339),
			},
			wantWindow: "weekly",
			wantReset:  weeklyReset,
			wantOK:     true,
		},
		"字符串形式的用量同样识别": {
			extra: map[string]any{
				"kimi_5h_used_percent": "90",
				"kimi_5h_reset_at":     fiveHourReset.Format(time.RFC3339),
			},
			wantWindow: "5h",
			wantReset:  fiveHourReset,
			wantOK:     true,
		},
		"空快照不停调": {
			extra:  map[string]any{},
			wantOK: false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			window, resumeAt, ok := cnQuotaPauseWindowAt(cnPauseWindowAccount(tc.extra), now, 85)
			require.Equal(t, tc.wantOK, ok)
			require.Equal(t, tc.wantWindow, window)
			if tc.wantOK {
				require.True(t, tc.wantReset.Equal(resumeAt), "want %s got %s", tc.wantReset, resumeAt)
			}
		})
	}
}

// TestCNQuotaPauseWindowAt_Guards 非 CN 平台 / nil 账号 / 非正阈值都必须判为不可用，
// 避免把无关账号按额度窗口停调。
func TestCNQuotaPauseWindowAt_Guards(t *testing.T) {
	t.Parallel()
	now := time.Now()
	exhausted := map[string]any{
		"kimi_weekly_used_percent": 92.0,
		"kimi_weekly_reset_at":     now.Add(48 * time.Hour).Format(time.RFC3339),
	}

	openAI := cnPauseWindowAccount(exhausted)
	openAI.Platform = PlatformOpenAI
	_, _, ok := cnQuotaPauseWindowAt(openAI, now, 85)
	require.False(t, ok)

	_, _, ok = cnQuotaPauseWindowAt(nil, now, 85)
	require.False(t, ok)

	_, _, ok = cnQuotaPauseWindowAt(cnPauseWindowAccount(exhausted), now, 0)
	require.False(t, ok)
}

// TestRateLimitService_CNQuotaPauseWindow_UsesConfiguredThreshold 阈值必须来自
// gateway.cn_providers.quota_exhausted_percent（默认 85），不另设参数。
func TestRateLimitService_CNQuotaPauseWindow_UsesConfiguredThreshold(t *testing.T) {
	t.Parallel()
	now := time.Now()
	// RFC3339 只到秒：与解析回读的值比较前必须截断，否则纳秒差异导致 Equal 为假。
	resetAt := now.Add(48 * time.Hour).Truncate(time.Second)
	account := cnPauseWindowAccount(map[string]any{
		"kimi_weekly_used_percent": 86.0,
		"kimi_weekly_reset_at":     resetAt.Format(time.RFC3339),
	})

	// 无配置 → 默认 85 → 周满。
	window, resumeAt, ok := (&RateLimitService{}).cnQuotaPauseWindow(account)
	require.True(t, ok)
	require.Equal(t, "weekly", window)
	require.True(t, resetAt.Equal(resumeAt))

	// 阈值提高到 95 → 86% 不再算满。
	cfg := &config.Config{}
	cfg.Gateway.CNProviders.QuotaExhaustedPercent = 95
	_, _, ok = (&RateLimitService{cfg: cfg}).cnQuotaPauseWindow(account)
	require.False(t, ok)
}

// TestCNQuotaSnapshotFresh 快照新鲜度决定「窗口耗尽」是否成立：超过 maxAge、
// 缺 usage_updated_at、不可解析都视为不新鲜。
func TestCNQuotaSnapshotFresh(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	maxAge := 30 * time.Minute

	fresh := cnPauseWindowAccount(map[string]any{
		"kimi_usage_updated_at": now.Add(-time.Minute).Format(time.RFC3339),
	})
	require.True(t, cnQuotaSnapshotFresh(fresh, now, maxAge))

	stale := cnPauseWindowAccount(map[string]any{
		"kimi_usage_updated_at": now.Add(-time.Hour).Format(time.RFC3339),
	})
	require.False(t, cnQuotaSnapshotFresh(stale, now, maxAge))

	require.False(t, cnQuotaSnapshotFresh(cnPauseWindowAccount(map[string]any{}), now, maxAge))
	require.False(t, cnQuotaSnapshotFresh(cnPauseWindowAccount(map[string]any{"kimi_usage_updated_at": "not-a-time"}), now, maxAge))
	require.True(t, cnQuotaSnapshotFresh(cnPauseWindowAccount(nil), now, 0), "maxAge<=0 表示不做新鲜度限制")
}

// TestCNProviderQuotaSnapshotExhausted 窗口耗尽证据（任一窗口用量 ≥ 阈值且未重置，
// 且快照在 maxAge 内刷新过）。历史账号恢复用它判定「额度是否仍未恢复」，因此
// 「快照过期」必须判为不耗尽（否则陈旧快照会把本该恢复的账号继续压住）。
func TestCNProviderQuotaSnapshotExhausted(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	maxAge := 30 * time.Minute
	fresh := now.Add(-time.Minute).Format(time.RFC3339)
	stale := now.Add(-time.Hour).Format(time.RFC3339)
	future := now.Add(3 * time.Hour).Format(time.RFC3339)
	past := now.Add(-time.Minute).Format(time.RFC3339)

	exhaustedFiveHour := cnPauseWindowAccount(map[string]any{
		"kimi_usage_updated_at":    fresh,
		"kimi_5h_used_percent":     96.0,
		"kimi_5h_reset_at":         future,
		"kimi_weekly_used_percent": 10.0,
		"kimi_weekly_reset_at":     future,
	})
	require.True(t, cnProviderQuotaSnapshotExhausted(exhaustedFiveHour, now, 85, maxAge))

	exhaustedWeekly := cnPauseWindowAccount(map[string]any{
		"kimi_usage_updated_at":    fresh,
		"kimi_5h_used_percent":     10.0,
		"kimi_5h_reset_at":         future,
		"kimi_weekly_used_percent": 92.0,
		"kimi_weekly_reset_at":     future,
	})
	require.True(t, cnProviderQuotaSnapshotExhausted(exhaustedWeekly, now, 85, maxAge))

	recovered := cnPauseWindowAccount(map[string]any{
		"kimi_usage_updated_at":    fresh,
		"kimi_5h_used_percent":     20.0,
		"kimi_5h_reset_at":         future,
		"kimi_weekly_used_percent": 30.0,
		"kimi_weekly_reset_at":     future,
	})
	require.False(t, cnProviderQuotaSnapshotExhausted(recovered, now, 85, maxAge))

	staleSnapshot := cnPauseWindowAccount(map[string]any{
		"kimi_usage_updated_at": stale,
		"kimi_5h_used_percent":  99.0,
		"kimi_5h_reset_at":      future,
	})
	require.False(t, cnProviderQuotaSnapshotExhausted(staleSnapshot, now, 85, maxAge))

	windowReset := cnPauseWindowAccount(map[string]any{
		"kimi_usage_updated_at": fresh,
		"kimi_5h_used_percent":  99.0,
		"kimi_5h_reset_at":      past,
	})
	require.False(t, cnProviderQuotaSnapshotExhausted(windowReset, now, 85, maxAge))

	require.False(t, cnProviderQuotaSnapshotExhausted(nil, now, 85, maxAge))
	require.False(t, cnProviderQuotaSnapshotExhausted(recovered, now, 0, maxAge))
}
