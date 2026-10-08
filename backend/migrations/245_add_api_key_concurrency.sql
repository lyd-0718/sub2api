-- Administrator-managed per-key concurrency; 0 preserves unlimited legacy behavior.
-- Additive only: older binaries ignore the column, and rollback images leave it intact.
ALTER TABLE api_keys
    ADD COLUMN IF NOT EXISTS concurrency INTEGER NOT NULL DEFAULT 0;

ALTER TABLE api_keys
    DROP CONSTRAINT IF EXISTS api_keys_concurrency_check;

ALTER TABLE api_keys
    ADD CONSTRAINT api_keys_concurrency_check CHECK (concurrency >= 0);

COMMENT ON COLUMN api_keys.concurrency IS 'Administrator-managed active model request limit (0 = unlimited)';

-- Keep the durable invalidation fallback in sync with the new auth field.
CREATE OR REPLACE FUNCTION enqueue_api_key_auth_cache_invalidation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        PERFORM enqueue_auth_cache_invalidation(OLD.key);
        RETURN OLD;
    END IF;

    IF OLD.key IS DISTINCT FROM NEW.key
       OR OLD.status IS DISTINCT FROM NEW.status
       OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at
       OR OLD.user_id IS DISTINCT FROM NEW.user_id
       OR OLD.group_id IS DISTINCT FROM NEW.group_id
       OR OLD.concurrency IS DISTINCT FROM NEW.concurrency
       OR OLD.ip_whitelist IS DISTINCT FROM NEW.ip_whitelist
       OR OLD.ip_blacklist IS DISTINCT FROM NEW.ip_blacklist
       OR OLD.expires_at IS DISTINCT FROM NEW.expires_at THEN
        PERFORM enqueue_auth_cache_invalidation(OLD.key);
        IF NEW.deleted_at IS NULL AND NEW.key IS DISTINCT FROM OLD.key THEN
            PERFORM enqueue_auth_cache_invalidation(NEW.key);
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
