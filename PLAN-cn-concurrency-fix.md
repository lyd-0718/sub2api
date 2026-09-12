# 方案：kimi 并发超限根因修复（粘性爆发整形 + AIMD 自适应并发帽 + 池空兜底）

> 状态：待评审（未经实施）。替代/吸收 PLAN-429-resilience.md（DeepSeek 版）的评审结论：
> 其 A 层（池空等待）降级为本方案第三期兜底；B 层（探针解停）废弃（吃套餐配额、小探针测不出大报文容量、30s 停车下无收益）。

## 一、背景与证据（生产实测，全部已核实）

**事故**：2026-09-11 晚，`account_select_failed`（用户可见失败）9,795 次（09-10 仅 465 次）；
并发超限停车 8,394 次（09-10 为 1,062 次），所有账号均匀分布 = 全池弹跳签名。

**容量对比**（09-11 17:00-24:00，usage_logs）：

| 指标 | 数值 |
|---|---|
| 活跃 API Key | 4-6 个 |
| 平均在途并发 | 2.7 ~ 6.0 |
| 池容量（14 账号 × 动态上限 3-4） | ~42-56 |

**平均需求仅为容量的 1/8——不是容量问题。**

**根因链**（代码已坐实）：

```
omp 子代理爆发（同一会话一分钟内 7-10 个并行请求，实测 21:05/22:07 各 10 发）
  → 粘性路由把同会话请求全部钉在同一账号
    （gateway_scheduling.go:524 粘性 gate 检查平台/配额/RPM 等 9 项，唯独不查该账号当前在途数；
      账号并发配置 100 = 不设防，全部 acquire 成功）
  → 10 条并发砸向 kimi 单账号，撞 3-4 的动态上限
  → cn_concurrency_limit → 账号停车 30s
  → 故障转移爆发跟随粘性绑定砸下一个账号 → 再停 → 级联
  → 池内全部账号轮转停车 → 选择失败 → 用户可见错误
```

**关键约束**（DeepSeek 版已正确指出，本方案继承）：kimi 并发上限是动态的（09-09 实测 11-16，
09-10 起 3-4），**任何固定数值调参都会过期**，机制必须自适应。

## 二、总览（三期，各自独立开关、独立上线、独立回滚）

| 期 | 机制 | 治什么 | 风险 |
|---|---|---|---|
| 一 | 粘性让位：粘性账号在途 ≥ 阈值时溢出请求转投他号 | 根因（爆发集中） | 低 |
| 二 | AIMD 自适应并发帽：从超限信号学习动态上限 | 强身（不再依赖固定值） | 中 |
| 三 | 池空有界等待（DeepSeek A 修正版） | 兜底（真·全池打满） | 中 |

## 三、第一期：粘性让位（burst yield）

### 逻辑

粘性命中时增加一项检查——粘性账号当前在途数；超过阈值则跳过粘性，落入负载均衡层：

```
粘性账号命中（Layer 1.5 / tryStickySessionHit）时：
  inflight = GetAccountConcurrency(stickyAccountID)
  if inflight >= sticky_burst_yield_threshold（默认 3，可配，0=关闭）:
      跳过粘性 → 走正常负载均衡（Layer 2 挑最闲账号）
      记录日志 sticky_burst_yield（account_id, inflight, session）
  else:
      维持粘性（现状）
  粘性绑定本身不删除——账号闲下来后，后续请求自动回粘
```

### 改动点（3 处，同型）

1. `backend/internal/service/gateway_scheduling.go:524` —— Layer 1.5 粘性 gate（无路由时），在 `tryAcquireAccountSlot` 之前加在途检查
2. `backend/internal/service/gateway_scheduling.go:344` —— Layer 1.5 路由内粘性检查（`sticky.layer1_5_checking`），同样加在途检查
3. `backend/internal/service/openai_gateway_scheduling.go` `tryStickySessionHit`（~:931）—— OpenAI 路径同型 gate

### 配置

```
gateway.scheduling.sticky_burst_yield_threshold = 3   # 0 或负数 = 关闭（回现状）
```

### 缓存影响（已评估，可接受）

溢出的请求落在别的账号上，吃不到粘性账号的前缀缓存——但这些请求不发出去就是报错，
缓存损失 < 失败损失。且爆发过后粘性恢复，后续轮次回到缓存热账号。

### 单测

- 粘性账号在途 2（< 3）→ 粘性命中（现状不变）
- 在途 3（≥ 3）→ 让位，选中非粘性账号；粘性绑定未删除
- 阈值配 0 → 关闭，行为完全回现状
- 在途查询失败（Redis 抖动）→ fail-open 维持粘性（查询失败不该打散缓存局部性）

## 四、第二期：AIMD 自适应并发帽

### 逻辑（TCP 拥塞控制同款）

```
乘性下降：handleCNProviderConcurrencyLimit403 收到超限时
  N = 该账号当时实际在途数（GetAccountConcurrency）
  cap = max(1, N-1)
  写入 Redis：cn_adaptive_cap:{accountID} = cap，TTL 30 分钟

加性上升：该账号每成功完成 M=20 个请求（且未再触发超限）
  cap += 1，直至回到配置硬顶（100）

槽位获取：tryAcquireAccountSlot 的 maxConcurrency 改为 min(配置值, AIMD 帽)
AIMD 帽查询失败 → fail-open 用配置值
```

### 改动点

1. `backend/internal/service/ratelimit_cn_providers.go` `handleCNProviderConcurrencyLimit403` —— 增加写帽（我们自有的文件，冲突面不变）
2. `backend/internal/service/concurrency_service.go` —— 新增帽的读写（AddCapIfMissing/ReadCap），Redis 实现
3. `gateway_scheduling.go` / `openai_gateway_scheduling.go` 两个 `tryAcquireAccountSlot` —— 取 min(配置, 帽)

### 与现有 30s 停车的关系

保留。停车是"已经撞了"的反应；AIMD 帽是"别再撞"的预防。帽生效后停车触发率应大幅下降——
`cn_provider_concurrency_limited` 日志频率是二期验收指标。

### 单测

- 超限 → 帽 = 在途-1；下限 1
- 连续 M 次成功 → 帽 +1；不超过配置硬顶
- TTL 过期 → 帽消失，恢复配置值
- Redis 故障 → fail-open

## 五、第三期：池空有界等待（DeepSeek A 修正版）

> 前两期上线后观察一周，若 `account_select_failed` 已 <100/天，本期可不做或降优先级。

修正点（相对 DeepSeek 原案）：

1. **位置**：不做服务层新方法+逐入口切换。包在 failover loop 的选择重试点
   （`failover_loop.go` 选择失败分支），一处覆盖 chat/completions、/v1/messages、Gemini 全入口
2. **内存**：等待者上限不按个数按字节（等待中的请求挂着完整报文，8MB 级 × 100 = 800MB/组 不可接受）
3. **预算**：先实测 omp/Codex 客户端断连容忍，再定（起步 20s，非流式 10s）
4. **惊群**：停车到期归队账号 5s 内降权（避免 waiter 全部砸向刚归队账号再次超限）
5. 保留 DeepSeek 原案的：ctx 取消即退出、抖动 sleep、独立开关、观测日志键（pool_wait_*）

## 六、观测指标与验收口径

```
现有日志键：cn_provider_concurrency_limited（停车）、account_select_failed（用户可见失败）
新增日志键：sticky_burst_yield（让位次数）、cn_adaptive_cap_set / _raised（帽调整）

验收：
  一期上线后：同分钟同账号 ≥7 发的记录消失；停车 8,394/天 → 目标 <500/天
  二期上线后：停车 → <100/天
  最终：account_select_failed 9,795/天 → <100/天
```

## 七、merge 冲突面变化（更新 TRACE.md）

- 新增：`gateway_scheduling.go`（粘性 gate 在途检查，~10 行）、`openai_gateway_scheduling.go`（同型，~10 行）
- 已有：`ratelimit_cn_providers.go`、`config/config.go`（纯追加）
- 新增：`concurrency_service.go`（AIMD 帽读写，纯追加方法）

## 八、风险表

| 风险 | 缓解 |
|---|---|
| 粘性让位降低缓存命中率 | 阈值默认 3 只对生爆发生效；让位不删绑定，闲时自动回粘 |
| 在途数读 Redis 增加调度延迟 | GetAccountConcurrency 已有 batch 缓存路径；失败 fail-open |
| AIMD 帽学习错误（把正常高峰当超限） | 只在收到明确 cn_concurrency_limit 文案时降帽；TTL 30 分钟自愈 |
| 三期全开后行为复杂难排查 | 每期独立开关；关键动作全部有日志键 |

## 九、留给评审的问题

1. 粘性让位阈值默认 3 是否合理？（kimi 动态上限下限 3-4，留 1 的余量）
2. AIMD 的加性上升步幅（M=20 次成功 +1）是否太保守/激进？
3. 第三期是否真的需要——如果一二期上线后 account_select_failed 已达标，是否直接放弃三期？
