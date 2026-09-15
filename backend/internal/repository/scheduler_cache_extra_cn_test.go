package repository

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFilterSchedulerExtraKeepsCNQuotaSnapshotKeys 国产供应商 Coding Plan 的
// 5h / weekly 用量快照必须完整进调度投影。
//
// 键名与写入侧（service.cnProviderQuotaService 的 cnExtraKey(provider, suffix)）
// 逐字对应；裁掉任何一个键都会让 cnProviderThresholdCandidates 恒为 nil，
// 额度耗尽后的阈值停调/续停永不生效（只剩 403 时刻的一次性判定）。
func TestFilterSchedulerExtraKeepsCNQuotaSnapshotKeys(t *testing.T) {
	suffixes := []string{
		"5h_used_percent",
		"5h_reset_at",
		"weekly_used_percent",
		"weekly_reset_at",
		"usage_updated_at",
	}
	extra := map[string]any{"openai_passthrough": true}
	for _, provider := range []string{"kimi", "zhipu", "minimax"} {
		for _, suffix := range suffixes {
			extra[provider+"_"+suffix] = fmt.Sprintf("%s-%s", provider, suffix)
		}
	}
	// 非白名单键仍必须被裁掉：投影只带调度需要的字段，凭据类键不得外泄。
	extra["kimi_api_key_secret"] = "should-not-leak"

	filtered := filterSchedulerExtra(extra)

	for _, provider := range []string{"kimi", "zhipu", "minimax"} {
		for _, suffix := range suffixes {
			key := provider + "_" + suffix
			require.Equal(t, extra[key], filtered[key], "调度投影必须保留 %s", key)
		}
	}
	require.NotContains(t, filtered, "kimi_api_key_secret")
	require.Equal(t, true, filtered["openai_passthrough"])
}
