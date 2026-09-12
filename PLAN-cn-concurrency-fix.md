# 方案：kimi 并发超限根因修复（粘性让位 + AIMD 自适应并发帽 + 半开试探恢复）

> 状态：已评审定稿（评审人：李一丁）。替代 PLAN-429-resilience.md（DeepSeek 版，已废弃删除）：
> 其 A 层（池空等待）经评估不需要——本方案闭环建成后池空场景应消失；B 层（主动探针）废弃
> （吃套餐配额、小探针测不出大报文容量），恢复职责由第三条的半开试探承担（真实流量，零额外配额）。

## 一、背景与证据（生产实测，全部已核实）

**事故**：2026-09-11 晚，`account_select_failed`（用户可见失败）9,795 次（09-10 仅 465 次）；
并发超限停车 8,394 次（09-10 为 1,062 次），所有账号均匀分布 = 全池弹跳签名。

**容量对比**（09-11 17:00-24:00，usage_logs）：活跃 key 4-6 个，平均在途并发 2.7~6.0，
池容量 ~42-56（14 账号 × 动态上限 3-4）。**平均需求仅为容量的 1/8——不是容量问题。**

**根因链**（代码已坐实）：

```
omp 子代理爆发（同一会话一分钟内 7-10 个并行请求，实测 21:05/22:07 各 10 发；
              子代理与主 agent 共用同一个 X-Session-Id）
  → 粘性路由把同会话请求全部钉在同一账号
    （gateway_scheduling.go:524 粘性 gate 检查平台/配额/RPM 等 9 项，唯独不查该账号当前在途数；
      账号并发上限（曾 100，现 10）对 3-4 的动态上游上限不设防，全部 acquire 成功）
  → 并发砸向 kimi 单账号，撞动态上限
  → cn_concurrency_limit → 账号停车
  → 故障转移爆发跟随粘性绑定砸下一个账号 → 再停 → 级联
  → 池内全部账号轮转停车 → 选择失败 → 用户可见错误
```

**关键约束**：kimi 并发上限动态变化（09-09 实测 11-16，09-10 起 3-4）。
固定上限调参必然失效：配高了防不住收紧，配低了浪费宽松期容量。**机制必须自适应。**

**补充认知**：子代理的上下文与主会话不同源，钉在主账号上连缓存都共享不到——
粘性对子代理纯坏处零收益，第一期让子代理溢流分流的代价几乎为零。

## 二、总览（三条，构成自适应闭环：不撞 → 会学 → 快醒）

| 条 | 机制 | 角色 | 风险 |
|---|---|---|---|
| 一 | 粘性让位：粘性账号在途 ≥ 阈值时溢出请求转投他号 | 不撞（根因） | 低 |
| 二 | AIMD 自适应并发帽：从超限信号学习动态上限 | 会学（强身） | 中 |
| 三 | 半开试探恢复：仅对并发超限类停车，用真实请求试探提前归队 | 快醒（恢复） | 低 |

三条各自独立开关、独立上线、独立回滚。建议顺序：一 → 三 → 二。

## 三、第一条：粘性让位（burst yield）

### 逻辑

```
粘性账号命中时：
  inflight = GetAccountConcurrency(stickyAccountID)
  if inflight >= sticky_burst_yield_threshold（默认 3，可配，0=关闭）:
      跳过粘性 → 落入负载均衡层挑最闲账号
      日志 sticky_burst_yield（account_id, inflight, session）
  else:
      维持粘性（现状）
  粘性绑定不删除——账号闲下来后，后续请求自动回粘继续吃缓存
```

### 改动点（3 处，同型）

1. `backend/internal/service/gateway_scheduling.go:524` —— Layer 1.5 粘性 gate（无路由），`tryAcquireAccountSlot` 之前加在途检查
2. `backend/internal/service/gateway_scheduling.go:344` —— Layer 1.5 路由内粘性检查，同型
3. `backend/internal/service/openai_gateway_scheduling.go` `tryStickySessionHit`（~:931）—— OpenAI 路径同型 gate

### 配置

```
gateway.scheduling.sticky_burst_yield_threshold = 3   # 0 或负数 = 关闭（回现状）
```

### 单测

- 在途 2（< 3）→ 粘性命中（现状不变）
- 在途 3（≥ 3）→ 让位选中非粘性账号；粘性绑定未删除
- 阈值配 0 → 完全回现状
- 在途查询失败（Redis 抖动）→ fail-open 维持粘性（查询失败不该打散缓存局部性）

## 四、第二条：AIMD 自适应并发帽

### 逻辑（TCP 拥塞控制同款）

```
乘性下降：收到 cn_concurrency_limit 时
  N = 该账号当时实际在途数（GetAccountConcurrency）
  cap = max(1, N-1)
  写 Redis：cn_adaptive_cap:{accountID} = cap，TTL 30 分钟

加性上升：该账号每成功完成 M=20 个请求（且未再触发超限）
  cap += 1，直至配置硬顶

槽位获取：tryAcquireAccountSlot 的 maxConcurrency = min(配置值, AIMD 帽)
帽查询失败 → fail-open 用配置值；TTL 过期 → 自动失效恢复配置值
```

### 改动点

1. `ratelimit_cn_providers.go` `handleCNProviderConcurrencyLimit403` —— 增加写帽（自有文件）
2. `concurrency_service.go` —— 新增帽读写方法（纯追加）
3. 两个 `tryAcquireAccountSlot` —— 取 min(配置, 帽)

### 与 30s 停车的关系

保留停车。停车是"已经撞了"的反应，AIMD 帽是"别再撞"的预防。帽生效后
`cn_provider_concurrency_limited` 日志频率应大幅下降——二期验收指标。

### 单测

- 超限 → 帽 = 在途-1，下限 1
- 连续 M 次成功 → 帽 +1，不超过硬顶
- TTL 过期 → 恢复配置值
- Redis 故障 → fail-open

## 五、第三条：半开试探恢复（仅并发超限类）

### 逻辑（电路断路器 half-open 模式，用真实流量，零额外配额）

```
账号因 cn_concurrency_limit 停车中：
  新请求到来且该账号是候选时，放 1 条过去试水（半开）：
    成功 → 立即清停车归队（ClearTempUnschedulable）
           恢复时机 = 上游真实恢复时机，不浪费一秒
    仍是并发超限 → 续停，试探间隔退避 5s→10s→20s→30s 封顶
    其他错误（配额/余额/鉴权文案）→ 续停且不再试探（信号变质）
  试探间隔未到 → 该账号保持跳过（现状）

只对 reason 前缀 = cn_concurrency_limit 的停车启用：
  配额耗尽（窗口时间锁死）/ 余额不足（充值才恢复）/ 鉴权错误（凭证不修永远失败）
  一律不试探——它们的恢复语义与瞬时无关，试探纯属浪费配额。
  （reason 前缀机制现成在 temp_unschedulable_reason 字段里，直接筛）
```

与主动探针的本质区别：**试探用的是本来就要发的真实请求**——不产生任何额外上游调用，
不消耗套餐 5h 窗口配额。

### 改动点

1. 调度层候选过滤处：并发超限停车的账号从"硬排除"改为"试探间隔到期则放行 1 条"
2. 成功路径：该账号请求成功 → `ClearTempUnschedulable` + 日志 `cn_concurrency_halfopen_recovered`

### 单测

- 并发超限停车 + 试探间隔到期 → 放行 1 条
- 间隔未到期 → 排除
- 试探成功 → 清停车
- 配额类停车 → 永不试探

## 六、观测指标与验收口径

```
日志键：cn_provider_concurrency_limited（现有）、account_select_failed（现有）
新增：sticky_burst_yield、cn_adaptive_cap_set / _raised、cn_concurrency_halfopen_recovered

验收（对照 09-11 基线）：
  第一条上线后：同分钟同账号 ≥7 发的爆发记录消失；停车 8,394/天 → <500/天
  第三条上线后：停车平均持续时长显著缩短（从固定 30s → 实测恢复点）
  第二条上线后：停车 → <100/天；account_select_failed → <100/天
```

## 七、merge 冲突面变化（实施时更新 TRACE.md）

- 新增改动：`gateway_scheduling.go`（粘性 gate 在途检查 + 半开放行，~20 行）、
  `openai_gateway_scheduling.go`（同型，~15 行）
- 纯追加：`config/config.go`、`concurrency_service.go`
- 已有自有文件：`ratelimit_cn_providers.go`

## 八、风险表

| 风险 | 缓解 |
|---|---|
| 粘性让位降低缓存命中率 | 阈值 3 只对生爆发生效；子代理与主会话不同源，本就拿捏不到缓存；绑定不删闲时回粘 |
| 在途数读 Redis 增加调度延迟 | 已有 batch 缓存路径；失败 fail-open |
| AIMD 学错（正常高峰误判） | 只有明确收到 cn_concurrency_limit 文案才降帽；TTL 30 分钟自愈 |
| 半开试探打醒装睡的账号 | 退避间隔 + 仅并发超限类 + 变质即停 |
| 三条叠加行为复杂 | 各自独立开关；关键动作全部有日志键 |

## 九、实施清单

- [ ] 第一条：粘性让位（3 处 gate + 配置 + 单测）
- [ ] 第三条：半开试探（调度过滤 + 成功清停 + 单测）
- [ ] 第二条：AIMD 帽（写帽 + 读帽 + min 帽 + 单测）
- [ ] TRACE.md 冲突面清单更新
- [ ] 逐条灰度上线，观察验收指标
