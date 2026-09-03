package service

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func cn429TestAccount(extra map[string]any) *Account {
	return &Account{
		Platform:    PlatformKimi,
		Credentials: map[string]any{"account_mode": AccountModeCoding},
		Extra:       extra,
	}
}

func cn429Snapshot(updatedAt time.Time, fiveHUsed any, fiveHReset time.Time, weeklyUsed any, weeklyReset time.Time) map[string]any {
	return map[string]any{
		"kimi_usage_updated_at":    updatedAt.UTC().Format(time.RFC3339),
		"kimi_5h_used_percent":     fiveHUsed,
		"kimi_5h_reset_at":         fiveHReset.UTC().Format(time.RFC3339),
		"kimi_weekly_used_percent": weeklyUsed,
		"kimi_weekly_reset_at":     weeklyReset.UTC().Format(time.RFC3339),
	}
}

func TestCNCodingPlan429Cooldown_Transient429GetsShortCooldown(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	// 5h 窗口用量 2%（远未耗尽），窗口重置点在 3 小时后。
	acc := cn429TestAccount(cn429Snapshot(now.Add(-5*time.Minute), 2.0, now.Add(3*time.Hour), 1.0, now.Add(72*time.Hour)))

	until, exhausted, ok := cnCodingPlan429Cooldown(acc, now, 85, 30*time.Minute, 60*time.Second)

	require.True(t, ok)
	require.False(t, exhausted)
	require.WithinDuration(t, now.Add(60*time.Second), until, 2*time.Second)
}

func TestCNCodingPlan429Cooldown_ExhaustedWindowParksUntilReset(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	windowReset := now.Add(2*time.Hour + 30*time.Minute)
	// 5h 用量 96%（≥85 阈值）且窗口未重置 → 真耗尽。
	acc := cn429TestAccount(cn429Snapshot(now.Add(-2*time.Minute), 96.0, windowReset, 10.0, now.Add(72*time.Hour)))

	until, exhausted, ok := cnCodingPlan429Cooldown(acc, now, 85, 30*time.Minute, 60*time.Second)

	require.True(t, ok)
	require.True(t, exhausted)
	require.Equal(t, windowReset.UTC(), until.UTC())
}

func TestCNCodingPlan429Cooldown_WeeklyExhaustionAlsoParks(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	weeklyReset := now.Add(48 * time.Hour)
	// 5h 未耗尽但 weekly 92% → 耗尽；停到较早的 5h 重置点（现有行为，避免过度停调）。
	acc := cn429TestAccount(cn429Snapshot(now.Add(-2*time.Minute), 10.0, now.Add(3*time.Hour), 92.0, weeklyReset))

	until, exhausted, ok := cnCodingPlan429Cooldown(acc, now, 85, 30*time.Minute, 60*time.Second)

	require.True(t, ok)
	require.True(t, exhausted)
	require.Equal(t, now.Add(3*time.Hour).UTC(), until.UTC())
}

func TestCNCodingPlan429Cooldown_StaleSnapshotIsNotEvidence(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	// 用量 99% 但快照 1 小时未刷新（超过 30min 有效期）→ 不算耗尽，短冷却。
	acc := cn429TestAccount(cn429Snapshot(now.Add(-time.Hour), 99.0, now.Add(3*time.Hour), 99.0, now.Add(72*time.Hour)))

	until, exhausted, ok := cnCodingPlan429Cooldown(acc, now, 85, 30*time.Minute, 60*time.Second)

	require.True(t, ok)
	require.False(t, exhausted)
	require.WithinDuration(t, now.Add(60*time.Second), until, 2*time.Second)
}

func TestCNCodingPlan429Cooldown_HighUsageButWindowAlreadyReset(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	// 用量 99% 但窗口重置点已过去 → 旧窗口快照，不算耗尽。
	// cnProviderQuotaSnapshotReset 无未来重置点 → ok=false，走默认 429 逻辑。
	acc := cn429TestAccount(cn429Snapshot(now.Add(-2*time.Minute), 99.0, now.Add(-10*time.Minute), 99.0, now.Add(-time.Hour)))

	_, _, ok := cnCodingPlan429Cooldown(acc, now, 85, 30*time.Minute, 60*time.Second)
	require.False(t, ok)
}

func TestCNCodingPlan429Cooldown_MissingSnapshotFallsBack(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	// 无快照 → 无未来重置点 → ok=false。
	acc := cn429TestAccount(map[string]any{})
	_, _, ok := cnCodingPlan429Cooldown(acc, now, 85, 30*time.Minute, 60*time.Second)
	require.False(t, ok)
}

func TestCNCodingPlan429Cooldown_UsedPercentStoredAsString(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	// Extra 反序列化可能产出 string/数字混合，两种都要解析正确。
	acc := cn429TestAccount(cn429Snapshot(now.Add(-time.Minute), "90", now.Add(3*time.Hour), "5", now.Add(72*time.Hour)))

	_, exhausted, ok := cnCodingPlan429Cooldown(acc, now, 85, 30*time.Minute, 60*time.Second)
	require.True(t, ok)
	require.True(t, exhausted)
}

func TestCN429ConfigHelpers(t *testing.T) {
	// 无配置 → 默认值 60s / 85。
	s := &RateLimitService{}
	require.Equal(t, 60*time.Second, s.cn429TransientCooldown())
	require.Equal(t, 85.0, s.cn429QuotaExhaustedPercent())

	// 显式配置覆盖默认值。
	cfg := &config.Config{}
	cfg.Gateway.CNProviders.RateLimitCooldownSeconds = 120
	cfg.Gateway.CNProviders.QuotaExhaustedPercent = 70
	s = &RateLimitService{cfg: cfg}
	require.Equal(t, 120*time.Second, s.cn429TransientCooldown())
	require.Equal(t, 70.0, s.cn429QuotaExhaustedPercent())
}
