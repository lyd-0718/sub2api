-- 二开：自定义模型系统提示词支持 prepend / append / replace 三种注入模式。
--
-- prepend：插在组级系统提示词之前（历史行为，默认值）
-- append：插在组级系统提示词之后
-- replace：整体替换组级系统提示词
--
-- 采用 DROP + ADD（与 240z 等迁移一致）保证可重跑；列上有 NOT NULL DEFAULT，
-- 存量行自动填充 'prepend'，旧代码忽略该列。

ALTER TABLE custom_models
    ADD COLUMN IF NOT EXISTS injection_mode VARCHAR(20) NOT NULL DEFAULT 'prepend';

ALTER TABLE custom_models
    DROP CONSTRAINT IF EXISTS custom_models_injection_mode_check;

ALTER TABLE custom_models
    ADD CONSTRAINT custom_models_injection_mode_check
    CHECK (injection_mode IN ('prepend', 'append', 'replace'));

COMMENT ON COLUMN custom_models.injection_mode IS 'System prompt injection strategy: prepend (before group prompt), append (after group prompt), or replace (override group prompt)';
