package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func normalizedMigrationSQL(t *testing.T, name string) string {
	t.Helper()
	content, err := FS.ReadFile(name)
	require.NoError(t, err)
	return strings.Join(strings.Fields(string(content)), " ")
}

func TestAccountConcurrencyCapsMigration(t *testing.T) {
	sql := normalizedMigrationSQL(t, "238_account_concurrency_caps.sql")

	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS account_concurrency_caps")
	for _, column := range []string{
		"account_id BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE",
		"cap INT NOT NULL",
		"version BIGINT NOT NULL DEFAULT 1",
		"restricted_at TIMESTAMPTZ",
		"next_probe_at TIMESTAMPTZ",
		"reason TEXT",
		"flap_events JSONB NOT NULL DEFAULT '[]'::jsonb",
		"pinned BOOLEAN NOT NULL DEFAULT FALSE",
		"updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()",
	} {
		require.Contains(t, sql, column, "cap 表必须包含契约字段：%s", column)
	}
	// 探测调度器按到点时间取候选，索引是部分索引（只覆盖仍需探测的记录）。
	require.Contains(t, sql,
		"CREATE INDEX IF NOT EXISTS idx_account_concurrency_caps_next_probe_at ON account_concurrency_caps (next_probe_at) WHERE next_probe_at IS NOT NULL")
	// 迁移必须可重放。
	require.NotContains(t, sql, "DROP TABLE")
}

func TestKimiConcurrencyDefaultMigration(t *testing.T) {
	sql := normalizedMigrationSQL(t, "240_kimi_concurrency_default_3.sql")

	require.Contains(t, sql, "UPDATE accounts SET concurrency = 3")
	require.Contains(t, sql, "WHERE platform = 'kimi'")
	require.Contains(t, sql, "AND concurrency > 3")
	// 人工设过 1/2 的账号必须保持不动：条件只能是「>3」。
	require.NotContains(t, sql, "concurrency >= 3")
	require.NotContains(t, sql, "concurrency <> 3")
}
