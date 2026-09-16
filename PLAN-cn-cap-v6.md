# 方案：kimi 并发受限治理（实施版 v6，当前线上口径）

> 自包含文档。v5 及其前的方案文档已删除。
> **v5 → v6（2026-09-16 需求方拍板）**：
> 1. **砍掉动态伸缩**：主动探测、72h/12h 阶梯回升、抖动熔断全部移除。理由：受限账号上游**从不自然恢复**（生产零案例），探测回升是为"会恢复"设计的，实际不发生。
> 2. **砍掉 cap 限速器**：需求方已将全部 kimi 账号配置并发**手动压到 1**（上游流式限额实测就是 1），配置并发即最终值，无需再夹帽。撞并发 403 只做 **30s 临时停车**，不进 403 计数。
> 保留：额度停调、历史归队、403 三分类、可见性日志。

---

## 一、功能清单（当前实际保留的 3+1 项）

| # | 功能 | 说明 |
|---|---|---|
| 1 | **配额耗尽 = 停止调度（自动恢复）** | 周额度满 → 按**周窗口重置时间**停调；周未满但 5h 满 → 按 **5h 重置时间**停调；到点自动放行。**绝不**再因额度 403 把账号打成 `error` 永久卡死 |
| 2 | **历史被禁用账号自动归队** | 周期扫描 status='error' 的 CN 账号 → **读本地快照 reset_at 判定恢复（零探测）** → 最小请求点火验证 → **重读 updated_at 后 CAS 原子恢复** |
| 3 | **403 统一分类** | 并发限流 / 额度耗尽 / 鉴权 三类互斥；覆盖 HTTP 403、Anthropic 流内、Responses 流内、WS；副作用幂等键 = 请求 ID + 账号 + 分类 |
| + | **并发 403 处置**（简化） | 30s 临时停车（秒级瞬时信号），**不进** 403 连续计数。所有 kimi 账号配置并发已为 1，无降级概念 |

---

## 二、功能 1：额度耗尽 → 停调

### 判定规则

```
周额度已满（≥ 阈值）            → 按【周窗口重置时间】停调
周额度未满 且 5h 额度已满        → 按【5h 窗口重置时间】停调
两者都未满                      → 正常调度
快照缺失/过期/无未来重置点       → 60s 短冷却 + 等下轮周期额度探测刷新；【绝不】落回 403 计数
```

### 实现要点

- **阈值 85% 是缓冲线不是用尽线**（`gateway.cn_providers.quota_exhausted_percent` 与 `account_scheduling_thresholds.kimi`，均可配置）：额度快照 10 分钟一刷是滞后的，等 100% 再停必裸撞 403；而撞一次的代价是账号数天受限，远大于 15% 的额度余量
- **调度快照白名单已补 15 个 CN 额度键**（kimi/zhipu/minimax × 5h/weekly/updated，`repository/scheduler_cache.go`），否则阈值停调在选号链路上根本不生效
- **窗口分档用独立函数 `cnQuotaPauseWindow`**（周满优先）：不能复用旧的"取最早重置点"辅助函数（语义相反）
- 额度文案命中即**早退**，永不进入 `handleOpenAI403` 连续计数

## 三、功能 2：历史账号自动归队

- 周期任务（10 分钟间隔，leader 锁互斥），`ListCNQuotaDisabled` 拉 `status='error'` 的 CN 账号
- **恢复判定零探测**：直接读本地额度快照的 `reset_at`——reset_at 已过 = 窗口已重置，是确定事实（管理前端"额度刷新倒计时"读的就是它）；两窗口 reset 都过了 = 已恢复；任一窗口 reset 在未来 = 还没恢复，零上游调用按退避重排；快照新旧不影响判定（reset_at 是时间点事实，不是采样值）
- **最小请求点火验证**（不是探额度）：防"鉴权已死"账号被误恢复形成恢复-再死循环；快照里连窗口重置键都没有的账号，也由它仲裁（`HTTPUpstream.Do` 零副作用出站 + `cnValidateProbeURL` 校验）
- **CAS 恢复（关键顺序）**：验证**之后重读** `updated_at` 再 CAS；单事务置 `status='active'` + `schedulable=true` + 清 error/temp_unschedulable/rate-limit/overload，只发一次调度通知
- 退避 10m→20m→40m→封顶 6h；CAS 冲突（并发改写）不消耗退避预算
- 配置：`gateway.cn_providers.error_recovery_enabled`（true）/ `error_recovery_backoff` / `error_recovery_leader_lock_ttl`（90s）

## 四、功能 3：403 统一分类

| 分类 | 判定 | 处置 |
|---|---|---|
| 并发限流 | kimi 精确文案 + 宽松兜底（含 `concurrent request limit`） | 30s 临时停车；**不进 403 计数** |
| 额度耗尽 | 窗口 token（weekly/7-day/5-hour…）+ 耗尽语义双命中 | §二 停调到恢复时间；**命中即禁入 403 计数** |
| 鉴权/其它 | 其余 403 | 维持现状（计数 → 3 次禁用） |

- 分类器是 CN 平台限定（kimi/zhipu/minimax），双 token 设计；**不含裸 "5h" 子串**（会被 request_id 误命中）
- 落点：HTTP 主路径 + `HandleUpstreamError` 提前返回分支（分类器闸在最前）+ Anthropic 流内 error + Responses 流内（error/response.failed，谓词**之前**）+ WS（谓词**之前**）+ passthrough/messages/chat_completions 三条 SSE 路径的 bare error（`shouldFailover` 判定**之前**——额度文案不含 retry 标记会被原样透传，副作用不能丢）
- 幂等：副作用去重键 = 请求 ID + 账号 + 分类；error+response.failed 双事件合并为一次

## 五、已移除（v6 砍掉的，防回潮）

- ~~账号级 cap 限速器~~（表/store/夹帽/管理接口/前端展示）：配置并发已统一为 1，无意义
- ~~主动探测、72h/12h 阶梯回升、抖动熔断~~：上游受限账号从不自然恢复，探测无对象
- ~~迁移 238（cap 表）~~：已删除，不会建表
- ~~迁移 240（kimi 存量并发兜底）~~：已删除——需求方已手动调好全部账号并发，系统不再碰账号并发设置
- ~~前端新建默认 1~~：已回退——需求方明确不需要，新建表单维持上游共享初值
- 前端最终态：**与上游基线完全一致，零改动**

---

## 六、改动清单（最终态）

**自有新增（零冲突）**
- `service/ratelimit_classifier.go`（403 分类器）
- `service/cn_quota_pause_window.go`（窗口分档）
- `service/account_error_recovery_service.go`（历史归队）
- 各配套单测

**上游文件小改（已登记 TRACE.md 第 7 条）**
- `repository/scheduler_cache.go`：白名单 +15 键
- `service/ratelimit_service.go`：分类器闸 + 幂等去重
- `service/ratelimit_cn_providers.go`：并发分支 30s 停车不计数；额度分支窗口分档
- `service/gateway_forward.go` / `openai_ws_forwarder_support.go` / `openai_gateway_passthrough.go` / `openai_gateway_messages.go` / `openai_gateway_chat_completions.go` / `openai_gateway_upstream_errors.go`：流内/WS/SSE 分类器落点
- `service/setting_update.go`：默认阈值表 +kimi:85
- `repository/account_repo.go` + `service/account_service.go`：`ListCNQuotaDisabled` / `RestoreRecoveredAccount`（+接口签名）
- `config/config.go`：`cn_providers` +3 个归队配置键
- `cmd/server/wire_gen.go`：归队服务手工接线（4 行，注释标记）
- `server/routes/trace_admin.go`：nil 守卫（顺带修 trace 模块既有 panic）
- 前端 `CreateAccountModal.vue`：kimi 新建默认并发 1

## 七、验收

- 分类器表测：kimi 精确/宽松、周/5h 额度、鉴权、未知、HTML、空 body、非 CN、裸 5h 不匹配
- 额度判定：周满看周 / 周未满看 5h / 快照缺失短冷却不回落 403 计数
- 并发 403：30s 停车、不消耗 403 计数、WS/流内/SSE 各落点均生效、同请求双事件合并
- 归队：探测推进 updated_at 后重读再 CAS；CAS 失败不耗退避；恢复后出现在候选池
- 前端：kimi 新建默认 1、其它平台保持 10；vitest + vue-tsc + i18n 完整性
- 生产观察（上线后）：并发 403 停车次数、额度停调账号数、归队成功数
