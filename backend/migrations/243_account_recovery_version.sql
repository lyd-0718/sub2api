-- Add recovery_version column for fine-grained CAS in account error recovery
-- 替换 updated_at 作为 CAS 字段，避免无关账号更新导致的假冲突

ALTER TABLE accounts ADD COLUMN IF NOT EXISTS recovery_version INT NOT NULL DEFAULT 0;

-- Index for error recovery service queries
CREATE INDEX IF NOT EXISTS idx_accounts_recovery_version 
    ON accounts(recovery_version) 
    WHERE status = 'error';

COMMENT ON COLUMN accounts.recovery_version IS 'Optimistic lock version for account error recovery CAS operations';
