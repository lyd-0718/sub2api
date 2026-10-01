-- Add injection_mode to custom_models for flexible system prompt placement
-- 为自定义模型添加 injection_mode 字段，支持 prepend / append / replace

ALTER TABLE custom_models 
    ADD COLUMN IF NOT EXISTS injection_mode VARCHAR(20) NOT NULL DEFAULT 'prepend';

-- Create check constraint for valid modes
ALTER TABLE custom_models 
    ADD CONSTRAINT check_injection_mode 
    CHECK (injection_mode IN ('prepend', 'append', 'replace'));

COMMENT ON COLUMN custom_models.injection_mode IS 'System prompt injection strategy: prepend (before group prompt), append (after group prompt), or replace (override group prompt)';
