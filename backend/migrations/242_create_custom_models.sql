-- Create custom_models and custom_model_downstream_groups tables
-- 创建自定义模型系统，支持注入 system prompt 并转发到上游 Group

-- Custom Models Table
-- 自定义模型表：虚拟模型定义，可注入 system prompt 转发到上游号池
CREATE TABLE IF NOT EXISTS custom_models (
    id BIGSERIAL PRIMARY KEY,
    model_id VARCHAR(200) NOT NULL,  -- 虚拟模型名（对外暴露）
    upstream_group_id BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,  -- 上游 Group ID（号池）
    upstream_model VARCHAR(200) NOT NULL,  -- 上游真实模型名
    system_prompt TEXT,  -- 注入的 system prompt（可为空）
    enabled BOOLEAN NOT NULL DEFAULT TRUE,  -- 是否启用
    description TEXT,  -- 说明
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ  -- 软删除
);

-- Custom Model Downstream Groups Table
-- 自定义模型下游关联表：哪些 Group 可以访问这个自定义模型
CREATE TABLE IF NOT EXISTS custom_model_downstream_groups (
    custom_model_id BIGINT NOT NULL REFERENCES custom_models(id) ON DELETE CASCADE,
    group_id BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (custom_model_id, group_id)
);

-- Indexes for custom_models
CREATE UNIQUE INDEX IF NOT EXISTS idx_custom_models_model_id_unique_active
    ON custom_models (model_id)
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_custom_models_upstream_group_id
    ON custom_models (upstream_group_id)
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_custom_models_enabled
    ON custom_models (enabled)
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_custom_models_deleted_at
    ON custom_models (deleted_at);

-- Indexes for custom_model_downstream_groups
CREATE INDEX IF NOT EXISTS idx_custom_model_downstream_groups_group_id
    ON custom_model_downstream_groups (group_id);

-- Table and column comments
COMMENT ON TABLE custom_models IS 'Custom models with injected system prompts routed to upstream groups';
COMMENT ON COLUMN custom_models.model_id IS 'Virtual model name exposed to downstream';
COMMENT ON COLUMN custom_models.upstream_group_id IS 'Upstream group ID (account pool) to route requests to';
COMMENT ON COLUMN custom_models.upstream_model IS 'Real upstream model name';
COMMENT ON COLUMN custom_models.system_prompt IS 'System prompt to inject into requests';
COMMENT ON COLUMN custom_models.enabled IS 'Whether this custom model is active';
COMMENT ON COLUMN custom_models.description IS 'Description or notes about this custom model';

COMMENT ON TABLE custom_model_downstream_groups IS 'Association table defining which groups can access each custom model';
COMMENT ON COLUMN custom_model_downstream_groups.custom_model_id IS 'Reference to custom model';
COMMENT ON COLUMN custom_model_downstream_groups.group_id IS 'Downstream group ID that can use this custom model';
