# 方案：kimi 并发受限治理（单批实施版 v5）

> 自包含文档（v1–v4 与其它方案文档已删除，有效设计已并入本文）。
> v4 → v5：按 2026-09-15 k3v3 复审的 12 条发现修正——额度判定链路（白名单/窗口分档/快照缺失）、CAS 与额度探测的冲突、**主动探测必须先占满 cap 槽位**（否则恒失败并锁死）、收口的平台维度、探测判定三档、pinned/解熔断、端点同源、参数与默认值订正、补 WS 落点与可复用资产。
> 需求方已拍板范围：单批；配额耗尽停调度（周满看周、周未满看 5h）自动恢复；历史账号自动归队；**并发上限统一 3**（存量 >3 的下调为 3，新建默认值前端同步改 3）；撞并发 403 → cap=1 → 冷却 72h → 主动探测 → 2 → 12h → 探测 → 3；抖动熔断；403 三分类；不做双额度/粘性改造/改 AccountTestService/accounts 加 version 列/OpenRouter 处理。

---

## 一、功能清单（单批，共 8 项）

| # | 功能 | 说明 |
|---|---|---|
| 1 | **配额耗尽 = 停止调度（可自动恢复）** | 周额度满 → 按**周窗口重置时间**停调度；周额度未满 → 按 **5 小时窗口重置时间**停调度；到点自动放行。不再因额度 403 把账号打成 `error` |
| 2 | **历史被禁用账号自动归队** | 用现有 coding-plan 额度探测接口判定额度恢复 → 发一条最小请求验证 → 原子恢复（含打开调度开关、清各种临时标记） |
| 3 | **账号级有效并发上限（cap）** | 撞并发 403 → 该账号有效并发降为 **1**；所有准入路径统一受约束；**并发上限统一 3**（存量 >3 下调为 3；新建默认前端同步改 3） |
| 4 | **受限后 72h 冷却 → 主动探测 → 阶梯回升（1→2→3）** | 探测**必须在占满当前 cap 槽位后进行**（见 §四），避免与用户流量叠加 |
| 5 | **抖动熔断** | 统一口径：**探测失败** + **回升后 72h 内再次撞并发 403** 都计 flap；滚动 7 天 ≥3 → 停止自动回升 + 告警 |
| 6 | **403 统一分类** | 并发限流 / 额度耗尽 / 鉴权 三类互斥；覆盖 HTTP 403、Anthropic 流内、Responses 流内、**WS 路径**；事件级幂等 |
| 7 | **可见性与运维入口** | 账号详情：配置并发 vs 有效并发、受限原因/起始、下次探测时间、flap 计数；管理接口可 **pinned** 覆盖与解熔断 |
| 8 | **指标与告警** | 全池有效容量、受限账号数、cap 不可判定次数、分类分布、探测结果（含 inconclusive）、回升/抖动/熔断、停车结构、队列指标 |

---

## 二、功能 1 & 2：额度耗尽 → 停调度（替代"禁用账号"）

### 2.1 判定规则（需求方指定）

```
周额度已满                    → 按【周窗口重置时间】停调度
周额度未满 且 5h 额度已满      → 按【5h 窗口重置时间】停调度
两者都未满                    → 正常调度
```

### 2.2 实现要点（v5：补三处会导致"恒不生效"的缺口）

1. **调度快照白名单必须补 CN 额度键**（否则阈值路径读不到 → 判停永不触发）
   - 现状：`filterSchedulerExtra`（`repository/scheduler_cache.go:970-1035`）白名单只含 `codex_5h_*` 等，**不含** `kimi_5h_used_percent` / `kimi_5h_reset_at` / `kimi_weekly_used_percent` / `kimi_weekly_reset_at` / `kimi_usage_updated_at`
   - 结果：`cnProviderThresholdCandidates` 恒为 nil，`ApplyAccountSchedulingThreshold` 永不命中
   - **改动：白名单补齐 kimi / zhipu / minimax 三家的 5h + weekly + updated 键（共 15 个）**
2. **窗口分档判定要新建函数，不能复用现有"取最早重置点"的辅助函数**
   - 现有 `cnProviderQuotaSnapshotReset`（`ratelimit_cn_providers.go:139-159`）返回 5h/weekly 中**最早**的未来重置点（其注释明确说是为避免把账号冷却到周窗口重置）
   - 需求方规则与之相反：**周满时必须按周窗口**停调度
   - **改动：新增 `cnQuotaPauseWindow(account)` → 返回 (窗口类型, 恢复时间)**，按"周满优先、否则 5h"分档；`SetRateLimitedIfLater(恢复时间)` 使用其返回值
3. **额度文案命中即禁止进入通用 403 计数**（快照缺失时的兜底）
   - 现状：无未来重置点时 `cnCodingPlan429Cooldown` 返回 ok=false → 调用方继续默认逻辑 → 累计 3 次 → `SetError` → 永久卡死（正是本方案要消除的）
   - **改动：分类器命中"额度类文案"就早退，绝不落回 `handleOpenAI403` 计数**；若无法确定恢复时间 → 用短冷却（60s）并**排入一次额度探测刷新**；下一轮拿到快照后转为按窗口停调

### 2.3 阈值配置（v5：订正参数名）

- 额度"满"的阈值复用现有 **`gateway.cn_providers.quota_exhausted_percent`**（`config.go:1153`，默认 85，`config.go:2472`）——**不存在 `quota_pause_threshold_percent`**（v4 文档写错，已删）
- 平台级自动停调阈值走设置项 `account_scheduling_thresholds`（`setting_update.go:551-557`）；其默认表只有 openai/anthropic/grok，**没有 kimi** → 未显式配置时 `EvaluateAccountSchedulingThreshold` 在 `account_scheduling_threshold_eval.go:49-52` 直接早退
- **改动：迁移中写入 `account_scheduling_thresholds.kimi = 85`**（否则功能 1 的"阈值停调/续停"不生效，只剩 403 时刻的一次性判定）

### 2.4 历史账号自动归队

- 查询：新增 `ListCNQuotaDisabled(platform)`——**不能复用带 `active` 过滤的 `ListByPlatform`**
- 额度恢复判定：调用现有 coding-plan 额度探测（管理页"查询额度"同源）→ 周未满且（周未满时的）5h 未满
  （探测依赖 `kimi_usage_updated_at` 的快照时，需给该键设置**最大容忍时长**（默认 10 分钟）；过期则先刷新再判，避免拿旧快照误放行）
- 验证：发一条**最小请求**（独立出站；`HTTPUpstream.Do` 原语无计费/用量/健康上报钩子；出站 URL 经 `cnValidateProbeURL` 校验；kimi 为 coding plan → **不得复用余额探测服务**）
- **原子恢复的 CAS 守卫（v5 修正）**：额度探测会写快照 → `UpdateExtra` 显式更新 `updated_at`（`account_repo.go:2618`）→ **必须在"探测 + 验证"完成后重新读取 `updated_at`**，再用它做 CAS；否则影响行数恒为 0，恢复永远失败
- CAS 失败属"并发改写"，**不消耗**探测退避预算（只重排下一轮）
- 恢复动作单事务：`status=active` + `schedulable=true` + 清 error + 清 `temp_unschedulable` + 清 rate-limit/overload，只发一次调度通知
- 退避：10m → 20m → 40m → 封顶 6h；每账号每轮 ≤3 次

---

## 三、功能 3：账号级有效并发上限（cap）

### 3.1 存储与一致性（v5 强化：多实例正确性）

- 自有表 `account_concurrency_caps`：`account_id PK`、`cap`、`version`、`restricted_at`、`next_probe_at`、`reason`、**`flap_events`**（事件时间戳数组，用于滚动 7 天窗口）、`pinned`、`updated_at`
- **写穿**：DB 为唯一真源；**写操作同步更新 Redis `cap:{account_id}` 键**（硬准入路径从 Redis 原子读取，保证多实例一致）
- **进程内缓存**：仅用于负载估算/判满等"软"路径，TTL ≤5s（多实例下广播失效只到单个进程，不能依赖）
- **与调度快照的关系**：cap 不依赖快照（独立表进不了 `filterSchedulerExtra`），判满/负载/夹帽都在服务层实时读取 cap store（Redis）+ 进程缓存

### 3.2 生效点（唯一收口，两处，且入参语义不同）

1. **准入夹帽**：`ConcurrencyService.AcquireAccountSlot`（`concurrency_service.go:342`）
   - 入参 `maxConcurrency` 来自 `account.Concurrency`（真实准入值）
   - 该函数在 `入参 <= 0` 时直接放行 → **必须先取 cap 再判断**
   - 覆盖范围：两个 `tryAcquireAccountSlot`（`gateway_scheduling.go:1181`、`openai_gateway_scheduling.go:1504`）共 **13 个上游调用点** + handler 层 `gateway_helper.go:256/337`（等待队列/立即抢槽），全部收敛于此函数
2. **负载与判满夹帽**：`getAccountsLoadBatch`（`concurrency_service.go:583`）
   - 负载率公式在 `repository/concurrency_cache.go:999-1007`
   - **注意入参不同源**：批量侧实参是 `EffectiveLoadFactor()`（`account.go:168`，`load_factor` 优先于 `concurrency`），与准入侧的 `account.Concurrency` 不同 → **两处夹帽对象分别处理**
   - 缓存 key（`accountLoadBatchCacheKey`，`concurrency_service.go:688-698`）摘要已含 `MaxConcurrency` → 夹帽后再进 key 即可自动换 key
   - "是否已满"判定（`openai_account_scheduler.go:1178-1180/1694-1696`）同样改用夹帽后的值
3. **等待队列重试**：`handler/gateway_helper.go:365-445`（v5 补充：这条路径我此前漏了）
   - 等待超时后的重试会回到准入，但 `MaxWaiting` 队列容量是独立固定值，**不受 cap 影响** → 判满时必须用夹帽后的值，否则队列永远填不满

### 3.3 平台维度与不可判定语义（v5 修正：避免误伤全平台）

- 收口函数只有 `(accountID, maxConcurrency)`，无法区分三种情况 → 明确定义：
  ```
  cap 有记录            → 夹帽 min(入参, cap)
  无记录 / 非 CN 平台    → 不夹帽（保持现状）
  cap 存储不可用         → 仅对"已知曾受限"的账号按 1；否则不夹帽
                          ← 禁止"任何 miss 都夹到 1"（会把全平台账号锁成 1）
  ```
- store 上增加平台维度（或按 accountID 与平台白名单双条件判定），把"非 CN 不受约束"落到实现里

### 3.4 默认值与一次性迁移（v5 订正）

- 数据库列默认本来就是 3（`migrations/001_init.sql:68`）；**10 来自前端新建表单初值**（`frontend/src/components/account/CreateAccountModal.vue:4597`）
- 因此需要**两件**：
  1. 一次性迁移：仅将 `platform='kimi' AND concurrency > 3` 的存量账号下调为 3（手动设过 1/2 的不动）
  2. **改前端新建表单初值 10 → 3**（否则以后新建的 kimi 账号仍是 10）；如不改前端，则本项功能只覆盖存量

---

## 四、功能 4 & 5：72h 冷却 → 主动探测 → 阶梯回升 → 熔断

```
撞并发 403 → cap=1；restricted_at=now；next_probe_at=now+72h；flap 视情况计（见口径）

72h 到 → 主动探测（目标 2 条车道）
  ├ 通过        → cap=2；next_probe_at=now+12h
  ├ 并发受限失败 → cap 保持 1；next_probe_at=now+72h；flap+1
  └ 不确定      → 不推进、不计 flap；next_probe_at=now+1h

cap=2 满 12h → 主动探测（目标 3 条车道）
  ├ 通过 → cap=3（cap_max，停止回升）
  ├ 失败 → cap=1；next_probe_at=now+72h；flap+1
  └ 不确定 → cap 保持 2；next_probe_at=now+1h

滚动 7 天 flap ≥3 → 熔断：停止自动回升（固定当前 cap 或 1）+ 告警；可用管理接口解除
```

**flap 统一口径（v5）**：① 探测失败（仅"并发受限"）；② 回升后 72h 内再次撞并发 403。其它情形不计。

**flap 存储（v5 修正）**：不用单个计数器，而是**持久化 flap 事件时间戳数组**（`flap_events: []timestamp`）；判定"滚动 7 天 ≥3"时过滤出 7 天窗口内的事件数。这样事件能自然滑出窗口，熔断才能自动解除。

### 4.1 探测必须"独占"上游并发（v5 关键修正）

- **问题**：cap=1 时账号仍在服务用户流量，上游已看到 1 条流；此时直接发 2 条探测流 → 上游看到 ≥2 → 必然 403 → 探测恒失败 → flap 三连 → 熔断 → **cap 永远停在 1，回升永不发生**
- **修正后的探测流程**：
  1. 先尝试用**真实槽位**占满 cap 条（沿用 `ConcurrencyService` 的并发原语）——占不到（说明此刻有用户流量在跑）→ **推迟本轮**（`next_probe_at += cap_probe_drain_timeout`，默认 10s，最多重试 N 次）
  2. 占满后再发"目标车道数"条并发流式探测请求（此时上游只见探测流）
  3. 探测结束后立即释放占位槽位
- 探测流不经网关计费/用量/停车副作用（使用 `HTTPUpstream.Do` 原语直连上游；该原语无计费、用量、健康上报钩子）

### 4.2 探测请求规范（v5）

- **协议/端点必须与生产产生 403 的路径同源**：kimi 有 chat / Anthropic / Responses 三条原生路径，而生产 403 绝大多数来自 **Anthropic 流式 `/v1/messages`** → 探测默认走 `/v1/messages`（可配 `cap_probe_protocol`）
- `stream=true`、`max_tokens` 极小（默认 8）、单条超时 30s
- 判定三档：**全部成功 = 通过**；任一条命中"并发受限"文案 = 失败；其余（超时/5xx/网络错误/代理异常）= **不确定**（不推进、不计 flap、1 小时后重试）
- 指标 `cap_probe_total{target,result=pass|limited|inconclusive}`

### 4.3 人工干预与自动状态机共存（v5）

- `pinned` 布尔位：置位后自动回升与熔断跳过该账号（**新撞并发 403 导致的降级仍然生效**）；管理接口 `PUT` 同时提供 pinned 置位/清位与**解除熔断**（清 flap 计数）

---

## 五、功能 6：403 统一分类（含流内与 WS）

| 分类 | 判定 | 处置 |
|---|---|---|
| 并发限流 | kimi 精确文案 + 宽松兜底（含 `concurrent request limit`） | cap=1（§三/四）；保留 30s 临时停车；**不进 403 计数** |
| 额度耗尽 | 周/5h 额度文案 | §二 停调到恢复时间；**命中即禁止进入 403 计数**（快照缺失时短冷却 60s + 排入一次额度刷新） |
| 鉴权/其它 | 其余 403 | 维持现状（计数 → 3 次禁用） |

**必须覆盖的落点（v5 补充 WS 缺口）**：
- HTTP 403 主路径 + `HandleUpstreamError` 的提前返回分支（pool/custom/temp）
- Anthropic `/v1/messages` 流内 `event: error`（现状仅 `overloaded_error` 触发副作用）
- Responses 流内 `error` / `response.failed` 两个入口
- **WS 路径**：`openai_ws_forwarder_support.go:330-358` 与 `openai_gateway_passthrough.go:1614-1620` 的 403 分支当前先过 `openAIStream403AccountFailure`（`:1450`）谓词，而 kimi 并发文案**不命中**该谓词 → 流内 403 被静默丢弃（不换号、不写 cap）→ **必须在谓词之前接入分类器**
- **幂等**：副作用去重键 = **请求 ID + 账号 + 分类**（同一请求对同一账号的同一类错误只写一次）；事件序号/上游事件 ID 仅用于重放去重，不作为副作用键。先例：WS 路径用请求内布尔量合并 `error`+`response.failed`（`openai_ws_v2_passthrough_adapter.go:1281-1292`）。

---

## 六、功能 7 & 8：可见性、指标与告警

- 管理接口：`GET /admin/accounts/:id/concurrency-cap`（配置并发、有效 cap、受限原因/起始、下次探测时间、flap 计数、pinned、是否熔断）；`PUT`（pinned 覆盖 / 解熔断）
- 容量口径落点：`group_capacity_service.go:159-166/254` 当前用 `acc.Concurrency` 汇总容量 → **改为使用有效 cap**，否则"配置 vs 有效"在容量视图里仍看不到
- 指标：`cap_effective`、受限账号数、`cap_unknown_total`、`classifier_bucket_total{class}`、`cap_probe_total{target,result}`、`cap_raise_total`、`cap_flap_total`、`cap_fuse_total`、`park_total{class}`、队列等待 p95 与排队满拒绝数
- 账号列表：受限账号显示"单车道"标记

---

## 七、改动清单

**自有新增（merge 冲突面为 0）**
- 建表脚本：`account_concurrency_caps`（含 `pinned`）
- `service/concurrency_cap_store.go`（读写/版本/进程缓存/失效/平台维度）
- `service/concurrency_cap_service.go`（降级、72h 冷却、探测编排、阶梯回升、熔断、leader 锁）
- `service/concurrency_cap_probe.go`（占槽 → 并发流式探测 → 判定三档 → 释放；独立出站）
- `service/cn_quota_pause_window.go`（**新增**：按"周满优先、否则 5h"分档的停调窗口判定）
- `service/account_error_recovery_service.go`（历史账号：探测 → 验证 → 重读 updated_at → CAS 恢复）
- `repository/account_concurrency_cap_repo.go`
- `handler/admin_concurrency_cap_handler.go`
- 指标定义、单测

**上游文件小改（登记 TRACE.md 冲突面）**

| 文件 | 改动 |
|---|---|
| `repository/scheduler_cache.go` | 白名单补 CN 额度键（kimi/zhipu/minimax × 5h/weekly/updated，共 15 个） |
| `service/concurrency_service.go` | 注入 cap store（Redis 写穿 + 进程缓存 ≤5s）；`AcquireAccountSlot` 夹帽（含 `<=0` 先取 cap）；`getAccountsLoadBatch` 夹帽（对象是 `EffectiveLoadFactor`） |
| `repository/concurrency_cache.go` | 负载率公式（:999-1007）读取夹帽后的 cap |
| `handler/gateway_helper.go` | 等待队列重试路径（:365-445）的判满用夹帽值 |
| `service/ratelimit_service.go` | 分类器收口（提前返回分支前置分类）+ 事件级幂等 |
| `service/ratelimit_cn_providers.go` | 并发分支写 cap；额度分支改用新的窗口分档函数；额度命中禁入 403 计数 |
| `service/gateway_forward.go` | Anthropic 流内 error 接入分类器 |
| `service/openai_ws_forwarder_support.go` / `openai_gateway_passthrough.go` / `openai_gateway_upstream_errors.go` | **WS 与 Responses 流内 403 在 `openAIStream403AccountFailure` 谓词之前接入分类器** |
| `service/openai_account_scheduler.go` | 判满比较用夹帽值 |
| `service/group_capacity_service.go` | 容量汇总改用有效 cap |
| `repository/account_repo.go` | 新增 `ListCNQuotaDisabled` + 原子恢复；接口补 `SetRateLimitedIfLater` |
| `config/config.go` | 参数 + 默认值 |
| `frontend/src/components/account/CreateAccountModal.vue` | 新建表单并发初值 10 → 3 |
| `wire.go` / `wire_gen.go` | 新服务装配 |

**可复用资产（实施时直接引用，勿重造）**：`HTTPUpstream.Do/DoWithTLS`（零副作用出站，`http_upstream_port.go:11-23`）、`cnValidateProbeURL`（`cn_provider_probe_url.go:27`）、流式并发模板（`account_test_service_cn_adaptive.go:52-106`）、周期任务骨架（`CNProviderBalanceCheckService.runOnce`）、进程内去重先例（`markOpenAITeamLinkedFired`）。

---

## 八、参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `concurrency_cap_enabled` | true | 总开关（不读不写） |
| `concurrency_cap_restricted` | 1 | 撞限流后的有效并发 |
| `cap_max` | 3 | 阶梯上限 |
| `cap_hold_hours` | 72 | 受限冷却（到点探测） |
| `cap_observe_hours` | 12 | cap=2 后的等待（到点探测） |
| `cap_probe_protocol` / `cap_probe_endpoint` | anthropic / `/v1/messages` | **必须与生产 403 同源** |
| `cap_probe_max_tokens` | 1（兼容失败再 4） | Kimi 最小合法值 |
| `cap_probe_timeout` | 30s | 单条探测请求超时 |
| `cap_probe_lease_ttl` | 60-90s | 探测任务租约（需续租） |
| `cap_probe_drain_timeout` | 10s | 单次占槽重试间隔；**本轮总预算 60s** |
| `cap_probe_inconclusive_retry_hours` | 1h | "不确定"结果的重试间隔 |
| `cap_fuse_flap_threshold` | 3（滚动 7 天） | 熔断阈值（`flap_events` 时间戳数组） |
| `cap_cache_ttl` | **≤5s** | 进程缓存（多实例下广播不可靠，硬准入走 Redis） |
| 复用 `gateway.cn_providers.quota_exhausted_percent` | 85 | 额度"满"阈值（**不新增参数**） |
| `account_scheduling_thresholds.kimi` | 85 | 平台停调阈值（迁移写入，否则阈值路径早退） |
| `recovery_probe_backoff` | 10m→6h，每轮 ≤3 | 历史账号恢复（CAS 冲突不消耗预算） |
| `leader_lock_ttl` | 60-90s（需续租） | 恢复/回升任务互斥（仅多实例时需要 owner token） |

---

## 九、验证与验收

**上线前测试**
- 分类器：HTTP / Anthropic 流内 / Responses 流内 / **WS** 四类路径各一次；同请求重复事件只写一次副作用
- 额度判定：周满 / 周未满且 5h 满 / 都未满 / **快照缺失**；恢复时间按窗口分档正确；快照缺失不落回 403 计数
- 白名单：调度快照能读到 CN 额度键（否则阈值停调不生效）
- 夹帽：13 个上游调用点 + handler 两处 + `入参<=0`；**无记录/非 CN 不夹帽**；存储不可用仅对已知受限账号按 1
- 探测：**占槽成功才探测**；占不到则推迟；判定三档；探测失败才计 flap
- 恢复阶梯：72h → 探测 → 2 → 12h → 探测 → 3；失败回 1 并重排 72h；flap ≥3 熔断；pinned 跳过自动回升
- 历史账号：探测会推进 `updated_at` → CAS 用**重读后的值**；恢复后出现在候选池
- 迁移：仅 `concurrency > 3` 的 kimi 存量账号下调为 3；前端新建初值同步改

**生产验收（滚动 24h，需先采基线）**
| 指标 | 基线 | 目标 |
|---|---|---|
| 并发 403 停车 | 8,394/天 | <100/天 |
| 用户可见失败（pool=0） | 9,795/天 | 显著下降（目标 <500/天，额度/容量决定余量） |
| 受限账号实际并发 | 1 | ≤ cap |
| cap 不可判定次数 | 未监控 | 0（且不得误伤非 CN 平台） |
| 历史账号自动归队 | 0（人工） | 100%（周窗口重置后） |
| flap | 未监控 | ≤2/账号/7 天（同 §四 口径） |
| 队列等待 p95 / 排队满拒绝 | **先采基线** | 不劣于基线 |

---

## 十、风险

| 风险 | 缓解 |
|---|---|
| 探测与用户流量叠加 → 恒失败 → 熔断锁死 | **占满 cap 槽位后再探测**；占不到就推迟（§4.1） |
| 收口 miss 语义误伤全平台 | 无记录/非 CN → 不夹帽；仅存储不可用且已知受限才按 1 |
| 额度判定链路不生效（白名单/阈值未配） | 白名单补 CN 键 + 迁移写入 `account_scheduling_thresholds.kimi` |
| 额度 403 快照缺失回落到 SetError | 文案命中即禁入 403 计数；短冷却 + 排入额度刷新 |
| CAS 与额度探测冲突 | 探测+验证后重读 `updated_at`；CAS 失败不消耗退避 |
| 60s 冷却期间的重复 403 | 事件级幂等（请求 ID + 事件序号） |
| 熔断后无法解除 / 人工覆盖被自动改回 | `pinned` 位 + PUT 解熔断 |
| 多实例重复探测/回升 | leader 锁（TTL 600s）；无协调后端时跳过 |
| 上游合并冲突 | 自有文件优先；上游改动保持小改并登记 TRACE.md |

---

## 十一、明确不做

1. 双额度拆分（总槽 + 流式槽）——cap 对全部请求生效
2. 粘性路由改造
3. 修改 `AccountTestService` 既有行为
4. 给 `accounts` 表加 version 列（用 `updated_at` 伪版本 + 影响行数；CAS 前重读）
5. 变更管理员手动开关流程（自动恢复只作用于 `status='error'` 账号）
6. 对 OpenRouter deepseek 账号应用本机制
7. 新增"用户流量之外的额外周期性探测"（仅保留 §4.1 的额度恢复/回升探测）
