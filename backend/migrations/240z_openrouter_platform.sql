-- 二开：新增 OpenRouter 平台（openrouter，API Key 按量付费、多协议）。
--
-- 1. user_platform_quotas.platform CHECK
-- 2. composite_model_routes.target_platform CHECK（composite 显式路由可指向 openrouter）
--
-- 渠道监控（channel_monitors / templates.provider）不支持 openrouter，约束保持不变。
-- 新约束是 238_opencode_go_platform.sql 的超集，必须保留 minimax / opencode_go。
--
-- 合并上游时注意：上游每新增一个平台都会用 DROP + ADD 重建这两个约束（列表里没有
-- openrouter）。若库里已有 platform='openrouter' 的行，上游迁移会在 ADD CONSTRAINT 时失败；
-- 合并时须在上游新迁移的列表里补上 'openrouter'（见 TRACE.md）。

ALTER TABLE user_platform_quotas
    DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check;

ALTER TABLE user_platform_quotas
    ADD CONSTRAINT user_platform_quotas_platform_check
    CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'grok',
                        'kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go', 'openrouter'));

ALTER TABLE composite_model_routes
    DROP CONSTRAINT IF EXISTS composite_model_routes_target_platform_check;

ALTER TABLE composite_model_routes
    ADD CONSTRAINT composite_model_routes_target_platform_check
    CHECK (target_platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'grok',
                               'kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go', 'openrouter'));
