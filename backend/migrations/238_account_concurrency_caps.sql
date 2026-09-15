-- 238: 账号级有效并发上限（cap）。
--
-- 与 accounts.concurrency 的关系：accounts.concurrency 是「配置并发」，本表记录被上游
-- 并发限流（403）后由治理流程下调的「有效并发上限」。有效并发 = min(配置并发, cap)；
-- 无本表记录 = 不夹帽（保持现状，非 CN 平台因此不受影响）。
--
-- 写入方：并发 403 分类器（cap=restricted）与管理接口（人工 pinned/cap）；
-- 读取方：准入路径（Redis 键 cap:{account_id} 写穿）、负载/容量软路径、探测调度器。
--
-- 字段说明：
--   cap           有效并发上限
--   version       每次写入 +1，供探测/回升流程做乐观并发判断
--   restricted_at 首次受限时间（重复降级不回退，用于「受限起始」展示与冷却计时）
--   next_probe_at 下次主动探测时间（cap >= cap_max 时为空 = 不再探测）
--   flap_events   抖动事件时间戳数组，滚动 7 天窗口判定熔断（事件自然滑出窗口）
--   pinned        人工置位：跳过自动回升与熔断，但新的并发 403 降级仍然生效
CREATE TABLE IF NOT EXISTS account_concurrency_caps (
    account_id    BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    cap           INT NOT NULL,
    version       BIGINT NOT NULL DEFAULT 1,
    restricted_at TIMESTAMPTZ,
    next_probe_at TIMESTAMPTZ,
    reason        TEXT,
    flap_events   JSONB NOT NULL DEFAULT '[]'::jsonb,
    pinned        BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE account_concurrency_caps IS
    'Account-level effective concurrency cap; absent row means no cap (do not clamp)';

-- 探测调度器按「到点的探测」取候选，部分索引只覆盖仍需探测的记录。
CREATE INDEX IF NOT EXISTS idx_account_concurrency_caps_next_probe_at
    ON account_concurrency_caps (next_probe_at)
    WHERE next_probe_at IS NOT NULL;
