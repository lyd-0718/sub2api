-- 二开：账号恢复（CNRecovery）CAS 专用乐观锁字段。
--
-- 原先用 updated_at 做 CAS：余额探测、标签/改名等无关更新都会推进它，
-- 造成恢复尝试白耗（影响行数 0）并白白消耗重试预算。recovery_version 只在
-- 恢复成功路径 +1，无关写入不影响恢复判定。
--
-- 纯追加列：NOT NULL DEFAULT 0 在 PG11+ 是元数据级变更（不重写表）；旧代码
-- 忽略新列，回滚无需恢复数据库。

ALTER TABLE accounts ADD COLUMN IF NOT EXISTS recovery_version INT NOT NULL DEFAULT 0;

COMMENT ON COLUMN accounts.recovery_version IS 'Optimistic lock version for account error recovery CAS operations';
