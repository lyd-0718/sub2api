# Session Trace 二开说明

按会话留存完整对话链路（用户输入 / 模型输出 / 思考链 / 工具调用），用于后续蒸馏微调。

> **这个 fork 是什么**：`Wei-Shaw/sub2api` 官方版（当前合并到 **v0.2.14**，2026-10-08）+ 六个自研/增强模块——
> ① 本文档讲的 **Session Trace 录制**；② **CN（kimi 等国产 Coding Plan）账号并发受限治理**（403 三分类、额度耗尽停调自动恢复、历史 error 账号自动归队），设计文档见 **`PLAN-cn-cap-v6.md`**；③ **账号级 TLS 指纹出站**；④ **OpenRouter 平台 + 供应商路由**；⑤ **自定义模型**（独立模型定义、系统提示词及 prepend/append/replace 注入模式、上游号池和多下游分组绑定，使用说明见 [README_CN.md](README_CN.md#自定义模型)）；⑥ **API Key 独立并发上限**（管理员设置，HTTP/SSE、Responses WebSocket 轮次及 Live 共享上限，使用及升级说明见 [README_CN.md](README_CN.md#api-key-并发限制)；2026-10-08 已部署到生产）。
> 部署分支：fork 的 **`trace` 分支**（GitHub 默认分支已设为 trace）。
> 当前生产镜像：`sub2api-trace:0.2.14-c1eecf318`（2026-10-16 部署，Kimi 月额度套餐识别 + 额度用满 100% 才停调 + 账号页/渠道监控页月额度标签统一为 30d；功能提交 `dcefc432d`、标签提交 `b80d9c885` / `c1eecf318`，无数据库迁移）。回滚直接把 image 改回 `sub2api-trace:0.2.14-1ed1267fb`（或只撤标签改回 `0.2.14-dcefc432d`）后 `up -d` 即可。服务器现保留 `0.2.14-c1eecf318` / `0.2.14-b80d9c885` / `0.2.14-dcefc432d` / `0.2.14-1ed1267fb`（2026-10-16 已删 `0.2.14-d627bbf56`、`0.2.14-ef40f1c`、`0.2.11-111379b8f`、`0.2.11-80ab6d0`）。上一版说明：`sub2api-trace:0.2.14-1ed1267fb`（2026-10-08 部署，含 Key 独立并发限制及管理员 API 密钥页直接编辑入口；源码提交 `1ed1267fbb2e04b9d6aab8d09b80a00119c06b25`）。回滚镜像 `sub2api-trace:0.2.14-d627bbf56` 已保留，回滚后后端并发限制仍有效，只恢复旧界面入口。入口为 Nginx 容器 `mc-blizo-proxy-1`，通过共享 Docker 网络反代至 `sub2api:8080`；本机维护映射仍为 `127.0.0.1:8081 → 8080`。本次仅更新网关镜像，未改 `.env`、端口、网络、Nginx 或证书配置，未重建代理。
> 历史镜像清理：2026-10-08 已删除 `0.2.8-57932a0`；要回滚到更早的版本（如下文的 `dc9713e`、`0.2.7-*`），先按对应提交重新构建出同名镜像，再改 image。

## 这套东西是什么

sub2api 官方镜像 + 一个"录音笔"中间件。所有经过 `/v1/messages`（Claude Code）、`/v1/chat/completions`（OpenAI 格式）、`/v1/responses`（Codex CLI）的请求，请求体和响应体被原样复制一份，按会话归档成 gzip 压缩的 JSON 文件。

**对官方代码的改动只有 3 行**（`backend/internal/server/routes/gateway.go`：1 行 import + 2 行注册中间件），其余全部是独立目录 `backend/internal/pkg/trace/` 下的新文件。

## 数据存在哪

服务器：`/opt/sub2api/data/traces/key-<API Key ID>/<日期YYYYMMDD>/<会话ID>/<时间戳>-<请求ID>.json.gz`

（v0.2.x 起为 key/日期/会话 三级；更早的部署是 日期/会话 两级，翻旧数据时注意。）

每个文件一轮请求，结构：

```json
{
  "session_id": "...",          // 会话ID
  "model": "...", "http_status": 200, "duration_ms": 17311,
  "request":  { ... },           // 完整请求体：messages 全量历史、tools、system
  "response": {
    "complete": true,            // 流正常结束才 true；导出时只取 true 的
    "stop_reason": "end_turn",
    "blocks": [
      {"type": "thinking", "thinking": "...", "signature": "..."},
      {"type": "tool_use", "tool_name": "Bash", "tool_input": {...}},
      {"type": "text", "text": "..."}
    ],
    "usage": {...}
  }
}
```

## 会话 ID 怎么来的（按优先级）

1. `X-Session-Id` 请求头（自家客户端可显式指定，最准）
2. Codex CLI 的 `session_id` / `conversation_id` 请求头
3. Claude Code 请求体 `metadata.user_id` 里的 session UUID（协议自带）
4. OpenAI 请求体的 `user` 字段
5. 兜底：`anon-<hash(apiKeyID + 第一条 user 消息)>`；注意不能用 messages[0]——omp 等客户端的 messages[0] 是恒定系统提示词，会把同 key 所有会话并错

## 工作原理（极简）

- **请求进来**：中间件读一份请求体再原样归还，下游 handler 无感知。
- **响应出去**：`gin.ResponseWriter` 包一层 tee，写给客户端的每个字节同时复制到内存缓冲（超 32MB 截断标记）。
- **请求结束**：SSE 流重组成结构化 blocks。三种格式（Anthropic / OpenAI chat / OpenAI Responses）统一归一成 `text/thinking/tool_use`；Responses 流以 `response.completed` 的全量对象为权威，流中断退化为 delta 累积并标 `complete=false`。重组完扔进异步队列。
- **落盘**：后台协程写 gzip 文件。队列满丢弃计数，**绝不阻塞转发**（fail-open）。

## 已知特性（蒸馏数据相关）

- 请求体的 `messages` 是**累增**的（客户端每轮重发全量历史）——有意保留，任何一轮单独拿出来都是一条完整样本；去重在导出时做。
- **上下文压缩**（Claude Code auto-compact）会导致某轮历史突然变短。导出器将按"前缀检测"找压缩边界，逐轮导出，不按"会话最后一轮"导出。
- 截断/报错/客户端断连的轮次 `complete=false`，导出时过滤。

## 配置（服务器 `/opt/sub2api/docker-compose.yml` 环境变量）

| 变量 | 默认 | 说明 |
|---|---|---|
| `TRACE_ENABLED` | false | 总开关 |
| `TRACE_DIR` | data/traces | 落盘目录 |
| `TRACE_PATHS` | 三个端点 | 采集路径白名单 |
| `TRACE_API_KEY_IDS` | 空=全部 | 只采指定 key（隐私控制） |
| `TRACE_SAMPLE_RATE` | 1.0 | 采样率 |
| `TRACE_MAX_BODY_BYTES` | 32MB | 单轮采集上限 |
| `TRACE_QUEUE_SIZE` / `TRACE_WORKERS` | 1024 / 2 | 异步队列 |

改完 `docker compose up -d sub2api` 生效。

## 本地查看工具

`/opt/sub2api/traceview.py`（本地副本 `~/Downloads/sub2api-traces/traceview.py`）：

```bash
python3 traceview.py <会话目录> [轮次N] [user|think|tool|text]
```

## 代码位置与更新流程

- Fork：`github.com/lyd-0718/sub2api`，分支 `trace`（当前已合并官方 `v0.2.14`（2026-10-08）；历史合并点 `v0.2.11` / `v0.2.8` / `v0.2.7` / `v0.2.5` / `v0.2.3`）
- 本地：`~/Desktop/sub2api`
- 服务器构建目录：`/opt/sub2api-trace`
- 部署配置：`/opt/sub2api/docker-compose.yml`（只改 image tag，其他不动）

**跟进官方更新：**

```bash
cd ~/Desktop/sub2api
git fetch https://github.com/Wei-Shaw/sub2api.git --tags
git checkout trace
git merge upstream/main   # 注意：合 main 而非 tag——上游先打 tag 后 bump VERSION，
                          # 合 tag 会带进旧 VERSION（v0.2.1 曾因此界面显示 0.2.0）
# 合并后验证（v0.2.5 实测命令）：
cd backend && go build ./... && go vet ./...
go test ./internal/config/ ./internal/repository/ ./internal/server/... ./cmd/server/ ./internal/handler/ ./migrations/ -count=1
go test -tags unit ./internal/service/ -count=1 && go test ./internal/service/ -count=1   # service 包两种标签各跑一遍
cd ../frontend && npx vitest run && npx vue-tsc --noEmit   # i18n 完整性/类型检查在镜像构建里会跑，本地先跑
git push origin trace   # 若上游改了 .github/workflows，gh 的 OAuth token 缺 workflow scope 会被拒：
                        # 改用 SSH 推送 git push git@github.com:lyd-0718/sub2api.git trace
```

（v0.2.5 合并实录：197 个提交只有 1 个冲突——`ratelimit_cn_providers.go` 的额度快照辅助函数：我方删旧函数换周满分档、上游给 OpenCodeGo 加月度窗口。双边保留解决：CN 平台走 `cnQuotaPauseWindow` 新规则，OpenCodeGo 走旧函数。上游 v0.2.5 自带 2 个挂掉的前端测试（ChannelMonitorView.grok 的供应商计数、GroupsView.codexManifest 的 Pinia），干净 upstream/main 上同样挂，不是合并问题，不要替上游修——修了反而制造未来的合并噪音。）

（v0.2.7 合并实录 2026-09-19：71 个提交 2 个文件冲突——上游 PR #7340（kimi 配额耗尽 403 停车）与我方 CN 治理模块正面撞车，同一功能两种实现。解决：`handle403`/`applyCNProviderReactive429`/`handleCNProviderConcurrencyLimit403` 保留我方（分类器收口早退 + 周/5h 分档 + 并发短冷却防级联停车，上游 10 分钟冷却会缩号池）；上游新增 `cooldownCNProviderToQuotaSnapshotReset` 双边保留（OpenCodeGo 429 分支依赖，CN Coding Plan 仍走我方 `cnCodingPlan429Cooldown` 分档）；删上游 `ratelimit_service_cn_quota_403_test.go`（测被删的 3 参同名符号，Go 无重载）。v0.2.5 时代那 2 个上游坏前端测试已被上游修复，本轮 297/2231 全过。教训：`go test -tags unit` 出现 FAIL 先落盘复跑再定位——本轮首轮 unit 挂、复跑 0 失败，是 flaky 不是合并问题。）

（v0.2.8 合并实录 2026-09-24：237 个提交 6 个文件冲突。`wire_gen.go` 3 处均为双方往同一行加参数：`ProvideGrokQuotaService` 保留我方 TLS 指纹参数 + 上游新增 `openAIReferralClient`；`ProvideAdminHandlers` 用上游行（+`openCodeGoUsageService`）后接我方 trace/CN 手工接线；`provideCleanup` 用上游参数表末尾追加 `accountErrorRecoveryService`。Gemini 5 个文件与上游 #7432 撞车，取上游版本再叠我方三处语义（见第 9 项）。**非文本冲突**：上游把 `claude.DefaultHeaders` 从 map 变量改成函数，我方 `account_error_recovery_service.go` 的 range 编译失败，改为 `claude.DefaultHeaders()`——合并后先 `go build` 再看测试。）

（v0.2.11 合并实录 2026-10-01：131 个提交 6 个文件冲突，无新增数据库迁移。`wire_gen.go` 2 处、`handler/wire.go` 1 处、`openai_gateway_handler.go` 1 处均为双方往同一位置加参数/逻辑，双边保留：`ProvideAdminHandlers` 末尾追加 `claudeResetCreditService`；`ProvideOpenAIGatewayHandler` 同时保留我方 `customModels` 与上游 `compositeResolver`；WebSocket 首轮路由合流为「自定义模型解析 → composite 路由决策（`wsRouteModel`）→ `ensureCompositeTargetPlatform(wsRouteModel)`」。`openai_gateway_scheduling.go` 1 处：上游 account_model 归属校验与我方 `openAIAccountPlatformMatches` 合流。`antigravity_gateway_compat_test.go` 2 处、`EditAccountModal.spec.ts` 1 处：双方各自新增不同测试，双边保留。教训：①编辑器把 `file.go:行范围` 形式的路径当新文件写出，曾产生 5 个冒号垃圾文件、真实文件冲突标记被误提交——合并后先 `grep -rn "^<<<<<<<\|^>>>>>>>"` 自查；②`go build ... | head` 的退出码是 head 的，验证退出码不要接管道。**2026-10-02 合并后修复**：上游 account_model 归属校验（composite 路由来源=账号映射时，账号须显式映射本次请求的模型）与自定义模型冲突——客户端书写的是自定义别名（`jailbreak/glm`，2026-10-02 改名 `jailbreak/glm-5.3`），账号映射里只有解析后的上游模型（`z-ai/glm-5.3`），全部候选账号被判 `account_model_not_owned` → pool=0 → 503；Anthropic 侧 `gateway_scheduling.go` 在自定义模型特性里已有兼容，本次按同法补齐 OpenAI 侧两处（`openai_account_scheduler.go` / `openai_gateway_scheduling.go`），并加回归测试 `TestCustomModelOwnershipCheckUsesResolvedUpstreamModel`。）

（v0.2.14 合并实录 2026-10-08：43 个提交（v0.2.12–v0.2.14）24 个文件 36 处冲突，全部是上游新平台 `typesafe`（TypeSafe / Jev System One）与我方 `openrouter` 在同一批平台列表里各加一项——后端 `group_handler.go` oneof ×3、`channel_handler.go`、`channel_service.go`、`composite_platform.go`、`scheduler_snapshot_service.go`（`schedulerSnapshotPlatforms()` 数组长度 11 → 12）、`admin_group.go`、`gateway_handler.go`，前端平台常量/类型/颜色/i18n/账号弹窗等。一律双边保留；沿用上游设计：TypeSafe 不进 composite 静态候选（`jev-latest` 只走 `/v1/systemone`），`compositeAvailableModels` 里仍受 `includeSystemOne` 控制。`router.go` 合流：网关路由保留我方 `customModels` 参数 + 上游支付路由新增 `redisClient`。`wire_gen.go` 无冲突，合并后用 `go run -mod=mod github.com/google/wire/cmd/wire` 重新生成，与自动合并结果逐字一致（跑完还原 go.mod/go.sum）。**迁移**：上游 `241_add_typesafe_platform.sql` 用 DROP + ADD 重建 `user_platform_quotas` / `composite_model_routes` 平台 CHECK，列表无 openrouter——按第 10 项「合并上游必查」在两处列表补 `'openrouter'`，并同步改上游测试 `migrations/typesafe_platform_migration_test.go` 的期望串（两处）。另一个迁移 `241_add_payment_order_bonus_amount.sql` 纯追加列。**顺带修复**：`repository/account_repo_upstream_billing_probe_update_test.go` 的 `updatedAccountRows` 漏了迁移 243 的 `recovery_version` 列（32 值对 33 列，sqlmock panic），合并前的 trace 上就挂，v0.2.11 那轮没跑 repository 包——已补 0。**上游自带坏测试（不修）**：前端 `settings.authSourceDefaults.spec.ts` 3 个（平台配额仍按 5 个平台断言，上游已加 typesafe）、`UseKeyModal.spec.ts` 3 个（Codex 配置 `[features]` 断言），干净 upstream/main 上同样失败。）

**merge 冲突面（trace 分支对上游的全部改动）：**

1. `backend/internal/server/routes/gateway.go`：3 行（trace 中间件注册）。
2. **Wire 装配**：Trace、账号用量导出、CN 账号归队和自定义模型均在 `internal/service/wire.go` / `internal/handler/wire.go` / `cmd/server/wire.go` 声明依赖与清理；`cmd/server/wire_gen.go` 由 Wire 生成，不再依赖生成后手工补回服务。
3. **429 证据停车**（`backend/internal/service/ratelimit_cn_providers.go`）：套餐号 429 按额度快照分级，瞬时 429 只短冷却 60s；配置 `gateway.cn_providers.rate_limit_cooldown_seconds` / `quota_exhausted_percent`。上游若重写此文件，保留我方 `cnCodingPlan429Cooldown` 分支逻辑。
4. `config/config.go`：纯追加（CNProviders 2 字段 + defaults），一般自动合并。
5. 前端：`TraceView.vue` / `AccountUsageExportView.vue` / `api/traceAdmin.ts` / i18n / 侧边栏入口（AppSidebar.vue），均为新增文件或纯追加。
6. **OpenRouter 余额探测**（2026-09-14 新增）：上游文件 `cn_provider_balance_service.go` 仅 4 处小改（`CNProviderBalanceEntry.Label` 字段、`cnBalanceURL` 的 openrouter 分支、deepseek 解析分流、快照写入 label）；识别/解析/`/key` 限额查询全在新文件 `cn_provider_balance_openrouter.go`（自有文件，零冲突）。前端 `CNProviderBalanceCell.vue` + `api/admin/cnProviders.ts` + zh/en i18n 各一处小改（label 前缀渲染，无 label 时行为不变）。
7. **CN 并发受限治理**（2026-09-16 定稿，设计见 `PLAN-cn-cap-v6.md`）：自有文件零冲突——`service/ratelimit_classifier.go`（403 三分类）、`service/cn_quota_pause_window.go`（周满看周窗口分档）、`service/account_error_recovery_service.go`（历史 error 账号自动归队：本地快照 reset_at 判定恢复、零额度探测，最小请求点火验证防鉴权死账号死循环，验证后重读 recovery_version 再 CAS）。迁移 `243_account_recovery_version.sql` 新增 `accounts.recovery_version`（恢复 CAS 专用乐观锁；原先用 `updated_at`，余额/标签等无关更新会推高它造成假冲突、白耗重试预算。v5 时代的 238/240 已废弃删除；账号并发由需求方手动管理，系统不碰）。上游文件改动：`repository/scheduler_cache.go`（白名单 +15 个 CN 额度键）；`service/ratelimit_service.go`（CN 403 分类器闸 + 幂等去重）；`service/ratelimit_cn_providers.go`（并发 403 → 30s 停车不进 403 计数；额度分支走窗口分档）；`service/gateway_forward.go` / `openai_ws_forwarder_support.go` / `openai_gateway_passthrough.go` / `openai_gateway_messages.go` / `openai_gateway_chat_completions.go` / `openai_gateway_upstream_errors.go`（流内/WS/SSE bare error 在 `openAIStream403AccountFailure`/`shouldFailover` 判定**之前**接分类器）；`service/setting_update.go`（默认阈值表 +kimi:85）；`repository/account_repo.go` + `service/account_service.go`（+`ListCNQuotaDisabled`/`RestoreRecoveredAccount` 及接口签名）；`config/config.go`（`cn_providers` +3 个归队配置键，纯追加）；`cmd/server/wire_gen.go`（归队服务手工接线 4 行，注释标记）；`server/routes/trace_admin.go`（nil 守卫，顺带修 trace 模块既有 panic）。前端：与上游基线完全一致（v5 时代的 cap 展示与 kimi 新建默认值均已按需求方决定回退）。**注意**：v5 时代的 cap 限速器（表/store/夹帽/管理接口）、探测回升/熔断、迁移 238/240、前端 cap 相关改动已于 2026-09-16 按需求方决定整体移除，如在上游看到相关设计文档残留以本节为准。
    - **Kimi 月额度套餐识别**（2026-10-16）：Kimi Token Plan 分两种套餐——5h + 周额度、5h + 月额度。`/coding/v1/usages` 的 `usages` 对象按键名区分窗口（`limit_5h` / `limit_7d` / `limit_month_total` + `limit_month_code`，`used_ratio` 为 0-1 比例）；月额度套餐不返回旧的 `usage` 字段。此前解析只认 `limits[].detail`（5h）和 `usage`（周），月额度完全读不到；月额度用完的 403 文案（"You've reached your monthly usage limit for this billing cycle…"）也不被分类器认出，落进 403 计数 3 次后永久 error（账号 135/185 即因此卡死，归队服务每 6h 发一次注定失败的验证请求）。改动：`parseKimiUsageTiers` 优先读 `usages`（月窗口取 total/code 较高者），缺失时回落旧字段；`cnQuotaExtraUpdates` 对 kimi 写入月/周其中一种时把另一种旧快照置 null（换套餐后不残留）；分类器加月文案（monthly / billing cycle）；新增 `cnQuotaWindows`（月 → 周 → 5h，长窗口优先）统一给停调分档、429 冷却、归队判定、恢复排点使用；**判满线一律 100%（需求方 2026-10-16 拍板：提前停调就是浪费额度）**——生产 `config.yaml` 的 `gateway.cn_providers.quota_exhausted_percent: 100`、设置表 `account_scheduling_thresholds.kimi=100`（= 阈值停调关闭，只在撞到额度 403 时按重置点停调）早已如此；代码另把月窗口判满线写死 100（`cnMonthlyQuotaExhaustedPercent`），防止有人把阈值调回 85 后按月提前停掉一整月。判满线 100 下快照会滞后（还停在 99.x%），因此额度 403 的文案窗口（月/周/5h）在快照里有未来重置点时也作为停调依据，与快照分档取较晚者，避免 60s 短冷却后反复撞 403；阈值停调候选对所有 CN 平台读 monthly；`scheduler_cache.go` 白名单 +`kimi_monthly_used_percent` / `kimi_monthly_reset_at`；前端额度条月窗口用紫色。测试 `service/kimi_monthly_quota_test.go`（样本取自生产实测响应）。
8. **账号级 TLS 指纹出站**（2026-09-21，核心 `ad5fe7a`；通用前端 `722b510` / `997f45a`）：TLS 指纹开关不再绑定平台、账号类型或模型；`service/http_upstream_port.go` 的 `doAccountHTTPUpstream` 统一真实流量、账号测试、额度/余额探测与 error 恢复。OpenAI-compatible、Gemini、Antigravity、Grok、Bedrock、CN Provider、Ollama HTTP/SSE 路径均接入；开关关闭直接保持原 `Do`，开启才走 `DoWithTLS`。`repository/http_upstream.go` 按账号 + profile 内容摘要隔离连接池，模板变更不复用旧 ClientHello；HTTPS 代理不再静默降级为 Go 指纹（明确报错，支持直连/HTTP CONNECT/SOCKS5）。前端创建/编辑页的 TLS 卡片已移出 Anthropic-only 容器；任意平台都能显示、读取和保存开关，移除每请求随机 profile，只允许固定内置或指定模板。生产验收：x5m5x 专属组非流式/流式均 200，流式完整 `[DONE]`；12,032 token 首写后二次命中 10,823 cached tokens，4 笔成功、0 错误；最终 Kimi 编辑弹窗已视觉确认开关可见且为开启状态。
9. **Gemini 裸名按 effort 选变体**（2026-09-23 `f2d1cb7`；v0.2.8 合并后改为叠加在上游实现之上）：Antigravity 上游只有 `gemini-3.x-flash-low/-medium/-high/-tiered`，裸名 404。上游 v0.2.8（#7432）已把选档下沉到 `getMappedModelForThinkingLevel` 覆盖全部入口，我方沿用其结构，只在 `service/antigravity_gemini_thinking_variant.go` 保留三处差异：①裸名自映射不阻断推导（上游仍尊重 credentials 里的 `gemini-3.8-flash → 自身`，而后台按 `DefaultAntigravityModelMapping` 建号必带这条，生产 3 个号全中招，上游版本在我们这里裸名照样 404；删 `accountRawModelMappingHasKey`）；②兼容层按 effort 选档：新增 `geminiThinkingLevelFromOpenAIBody`（reasoning_effort / reasoning.effort）与 `geminiThinkingLevelFromClaudeBody`（output_config.effort 优先，再落上游 `geminiThinkingLevelFromClaudeThinking`）——上游只看 thinking，Chat 的 low 不生成 thinking、minimal 拿默认大预算，二者都会落到 -high；③`trimGeminiThinkingVariantSuffix` 供 `service/group_model_allowlist.go` 候选形式使用：白名单写裸名即放行各变体，列表只展示裸名。调用点改动：`antigravity_gateway_compat.go`（OpenAI effort 优先）、`antigravity_gateway_claude.go`（传入 body）各一处。配套配置：分组 7/9/12 白名单的 gemini 只写 `gemini-3.8-flash`。上游若吸收以上语义，以上游为准删掉我方差异。
10. **OpenRouter 平台 + 供应商路由**（2026-09-24）：新增平台 `openrouter`（API Key、只有按量付费、adaptive 多协议；默认 `https://openrouter.ai/api/v1`，Anthropic 协议 `https://openrouter.ai/api`），一个账号统一管理 OpenRouter 上的所有模型，取代"按模型厂商选 DeepSeek/GLM 平台再填 OpenRouter 地址"的旧用法。
    - **定位**：加入 `IsMultiProtocolAPIKeyProvider`，**不加入** `IsCNProvider`——不套 Coding Plan 额度、国产 403/429 分类器、阈值停调、账号归队、DeepSeek 专用请求改写（reasoning 占位、input_image 别名）；需要的能力逐处用 `IsOpenRouter()` 显式接入：402 余额不足可恢复停调、403 累计冷却、count_tokens 本地估算、账号测试、上游模型同步、请求头覆写、余额 `/credits` 与余额低自动停调、Responses 探针、developer→system、工具 schema null 修复、Codex 客户端工具改写、`max_output_tokens` 保留（后几项迁移前挂 deepseek 平台时已生效，保持行为不变）。`GetAccountMode` 对 openrouter 恒为 payg。
    - **供应商路由**：`account.extra.openrouter_provider_routing = {enabled, models: {上游模型: [首选, 备选]}}`，出站前在三个构造点（`openai_gateway_cc_pipeline.go` / `openai_gateway_messages_anthropic_native.go` / `openai_gateway_forward.go` 各 1 行）注入 OpenRouter 的 `provider: {order, allow_fallbacks: true}`：平时固定首选保住缓存，首选挂了转备选，两家都挂交还 OpenRouter 自动路由。只看目标主机是否 openrouter.ai（与平台无关）；客户端自带 `provider` 时不覆盖。受管字段：只由 `GET/PUT /admin/cn-providers/accounts/:id/openrouter-routing` 写入，`admin_account.go` 通用编辑保留原值。后台入口：账号操作菜单"供应商路由"（`OpenRouterRoutingModal.vue`，下拉选项实时取 OpenRouter 公开的 `/models/{id}/endpoints`：价格、缓存价、量化、可用率、是否支持工具）。
    - **composite 跨平台账号池**：composite 按"哪个平台的账号模型映射显式声明了该模型"定目标平台，多平台同时声明判歧义并拒绝。规则：OpenRouter 与**恰好一个**其他平台同时声明时归属那个平台，且显式映射了该模型的 OpenRouter 账号加入其账号池（按优先级 / 负载一起调度、互为故障转移）；只有 OpenRouter 声明时归属 OpenRouter；两个非 OpenRouter 平台仍判歧义。实现：`gateway_service.go` 模型归属 +1 行，`openai_gateway_scheduling.go` `listSchedulableAccounts` 追加账号 + 资格检查、`openai_account_scheduler.go` 粘性 / 候选两处把平台等值判断换成 `openAIAccountPlatformMatches`。只在 composite 请求生效。生产 `deepseek/deepseek-v4.1-flash` 靠此继续在 sota-deepseek（优先级 1）与 OpenRouter 账号之间分流。
    - **自有文件**：`service/openrouter_platform.go`（默认地址 / 默认模型 / 跨平台账号池）、`service/openrouter_provider_routing.go`、`handler/admin/cn_provider_openrouter_routing.go`、`migrations/240z_openrouter_platform.sql`；前端 `components/account/OpenRouterRoutingModal.vue`、`api/admin/openrouterRouting.ts`。
    - **上游文件改动**（绝大多数是平台列表 / switch 加一项 openrouter）：后端 `domain/constants.go`、`service/domain_constants.go`、`account.go`、`routes/gateway.go`、`routes/admin.go`（2 条路由）、`openai_gateway_handler.go`、`openai_gateway_scheduling.go`、`openai_account_scheduler.go`、`gateway_service.go`、`scheduler_snapshot_service.go`、`composite_platform.go`（含 `openrouter/` 前缀识别）、`handler/admin/group_handler.go`（oneof）、`handler/gateway_handler.go`、`admin_group.go`、`channel_service.go`、`handler/admin/channel_handler.go`、`openai_codex_models_service.go`、`openai_gateway_forward.go`、`openai_gateway_count_tokens.go`、`openai_gateway_cc_pipeline.go`、`openai_gateway_messages_anthropic_native.go`（Anthropic 地址版本感知拼接）、`anthropic_apikey_auth.go`、`account_test_service.go`、`upstream_models.go`、`account_header_override.go`、`ratelimit_service.go`、`cn_provider_balance_service.go` / `cn_provider_balance_check_service.go` / `cn_provider_balance_openrouter.go`、`openai_chat_roles.go`、`openai_responses_tool_schema.go`、`openai_apikey_responses_probe.go`、`handler/admin/account_handler.go`、`admin_account.go`、`config/config.go`（URL 白名单 +openrouter.ai）；前端 `types/index.ts`、`constants/platforms.ts`、`credentialsBuilder.ts`（+`cnPaygOnlyPlatform` / `isOpenRouterAccount`）、`CreateAccountModal.vue`、`EditAccountModal.vue`、`CnBaseUrlPresets.vue`、`ModelWhitelistSelector.vue`、`useModelWhitelist.ts`、`PlatformIcon.vue`、`PlatformTypeBadge.vue`、`GroupBadge.vue`、`utils/platformColors.ts`、`GroupsView.vue`、`ChannelsView.vue`、`UseKeyModal.vue`、`utils/keyGroupProviders.ts`、`AccountUsageCell.vue`、`AccountActionMenu.vue`、`AccountsView.vue`、两个仪表盘平台标签表、i18n（zh/en）。
    - **合并上游必查**：①上游每加一个平台都会写新迁移，用 DROP + ADD 重建 `user_platform_quotas` / `composite_model_routes` 的平台 CHECK，列表里没有 `openrouter`——库里一旦有 platform='openrouter' 的行，上游迁移在 ADD CONSTRAINT 时失败、服务起不来；合并时要在上游新迁移的列表里补 `'openrouter'`（目前这两张表还没有 openrouter 行，一旦配了指向 openrouter 的 composite 显式路由或用户平台配额就会触发）。②上游在各处平台列表加新平台时，要顺手核对 openrouter 仍在（`go test` 里 `TestCompositeGroupSchedulerHasAllCanonicalPlatformBuckets` / `TestMatchingPlatforms` / 前端 `platforms.spec.ts` 会报漏项）。③若上游自己推出 OpenRouter 平台，以上游实现为准迁移，再对照本项补差。
    - **生产落地（2026-09-24）**：账号 22 `openrouter-deepseek`、38 `openrouter-glm`（原名 `openrout` / `openruot-glm`，同日改名）由 deepseek 平台迁到 openrouter（清掉 `deepseek_balance*` 旧快照）；供应商路由按推荐配置：`z-ai/glm-5.3-flash` Wafer → Relace、`z-ai/glm-5.3` Friendli → Wafer、`deepseek/deepseek-v4.1-flash` Fireworks → DeepSeek（`z-ai/glm-5.3-flashx` 只有 Z.AI 一家，不设；2026-09-26 起下线该模型：已从分组 12 允许列表与账号 38 映射删除，备份 `backups/drop_flashx_20260926-203335.tsv`，请求返回 404 `model_not_found`；代码里的 OpenRouter 预设模型列表仍保留它，只是建账号时的候选项）。验收：三组 `/models` 与迁移前一致；GLM 走 38（openrouter），回包 `provider` 字段迁移前为 InferenceNet（自动分配），配置后 glm-5.3-flash 连续命中 Wafer、glm-5.3 连续命中 Friendli；DeepSeek 仍优先 sota-deepseek。**回包里的 `provider` 字段会原样透传给客户端**，可直接用来核对路由是否生效。
    - **转发与缓存验收（2026-09-24 14:12，约 3000 token 长提示词连发两次）**：OpenRouter 的 `/chat/completions`、`/messages`（Anthropic 格式）、`/responses` 三个端点都接受 `provider` 参数，经网关三种入站均 200 且落在首选（非 chat 端点回包无 `provider` 字段，用 `GET https://openrouter.ai/api/v1/generation?id=<gen-id>` 查 `provider_name`）。第二次请求缓存命中：glm-5.3-flash @ Wafer 3264/3270（三种入站都命中，且同一前缀跨入站共享缓存）；deepseek-v4.1-flash @ Fireworks 3156/3285；glm-5.3 @ Friendli 间隔约 4 秒未命中、8 秒后命中——**Friendli 缓存异步建立，需几秒**（Wafer / Phala / Sail Research 5 秒内均命中），GLM 5.3 若跑秒级连续调用的 Agent，可在"供应商路由"里把首选换成 Wafer。usage_logs 正确记账缓存（同一请求命中后成本 $0.00049 → $0.00010）。


11. **自定义模型**：独立于 composite 的 CRUD、模型发现与单跳请求解析。核心文件 `service/custom_model*.go`、`repository/custom_model_repo.go`、`routes/custom_models.go`、`handler/custom_model*.go`，界面 `CustomModelsView.vue`。迁移 `242_create_custom_models.sql` 新增两张表、`244_custom_model_injection_mode.sql` 新增 `injection_mode` 列（prepend/append/replace，默认 prepend；相对客户端系统提示词：chat 并入第一条 system 消息、messages/gemini 对 block 数组生效、replace 覆盖客户端提示词）。Trace 中间件记录的是注入之后的最终请求体；保留原生模型目录、composite 路由和下游计费身份。配置可保存自定义模型引用，但运行时遇到第二个自定义模型即拒绝，不做递归解析。
    - **本地验收**：隔离 PostgreSQL / Redis、实际嵌入式服务和本地受控上游；Chat、Messages、Responses、Gemini、token 计数及 SSE 转发，WebSocket 连续两轮与停用后拒绝。用量表核对下游分组 / Key 与上游账号分离。真实浏览器完成中英文页面、创建、多组选择、编辑清空、启停和删除。此记录不表示已部署生产环境。
    - **生产落地（2026-10-02）**：三个 jailbreak 模型上线——`jailbreak/glm-5.3`（→ `z-ai/glm-5.3`，2026-10-02 由 `jailbreak/glm` 改名；旧名即失效并返回 400）、`jailbreak/k3`（→ `k3`）、`jailbreak/glm-5.3-flash`（→ `z-ai/glm-5.3-flash`），均 prepend、下游绑定分组 12。分组 12 模型白名单追加通配 `jailbreak/*`（原值备份 `/opt/sub2api/backups/group12_model_allowlist_pre_20261002.tsv`）——白名单准入只看客户端模型名、且发生在自定义模型解析之前，不放行会直接 404；改动经 SQL + 清 `apikey:auth:*`（组 12 三把 Key）+ `group_changed` outbox 事件生效，已用 `GET /v1/models` 验证三个模型均出现在列表中。（2026-10-02 改名复核：`jailbreak/glm` → `jailbreak/glm-5.3`，列表与实测调用均通过；原名备份 `/opt/sub2api/backups/custom_model_1_pre_rename_20261002.txt`。）

（kimi 缓存保活模块已于 2026-09-04 移除：实测有用但探测费相对省下的冷启动费性价比不高。历史见 git log。）
（`x-session-id` 粘性路由曾作为第 4 条改动，v0.2.1 合并时确认为重复代码已删除——上游名单的 `openCodeSessionIDHeader` 常量值就是 `X-Session-Id`。）

**服务器部署（标准流程，2026-10-08 按 v0.2.14 实际部署复核）：**

```bash
ssh relay
# 1) 全新构建目录（不要增量解压！tar 不删除已移除的文件，残留旧文件会编译失败）
rm -rf /opt/sub2api-trace && mkdir -p /opt/sub2api-trace && cd /opt/sub2api-trace
curl -sL https://github.com/lyd-0718/sub2api/archive/refs/heads/trace.tar.gz | tar xz --strip-components=1
# 2) 构建新镜像（约 3-4 分钟，构建期间旧服务照常运行）；tag 用 <版本>-<短提交>
docker build -t sub2api-trace:<版本>-<短提交> .
# 3) 备份（迁移前必做）：
cp /opt/sub2api/docker-compose.yml /opt/sub2api/docker-compose.yml.bak-$(date +%Y%m%d)
docker exec sub2api-postgres pg_dump -U sub2api sub2api | gzip > /opt/sub2api/backup-pre-<版本>-$(date +%Y%m%d-%H%M).sql.gz
#    （pg 用户/库名都是 sub2api，不是 postgres；data 目录 2.7G+ 不整备，回滚不依赖它）
# 4) 改 /opt/sub2api/docker-compose.yml 的 image tag（服务器是 GNU sed，别带 macOS 的 -i ''）：
sed -i 's|image: sub2api-trace:.*|image: sub2api-trace:<版本>-<短提交>|' /opt/sub2api/docker-compose.yml
cd /opt/sub2api && docker compose up -d sub2api
# 5) 验收清单：
#    - curl http://127.0.0.1:8081/health → 200（注意宿主机端口是 8081，不是容器内 8080；
#      域名走前面的反代）
#    - docker logs 确认迁移 applied（schema_migrations 表可查 filename）
#    - docker logs 看到 [CNRecovery] started (interval=10m0s)（CN 归队服务在线）
#    - 真实流量几分钟后 traces/ 下生成新会话文件、后台页面可打开
```

**Key 并发限制上线（2026-10-08）：**

- 源码归档应用到 `/opt/sub2api-build-d627bbf56`，远端构建 `sub2api-trace:0.2.14-d627bbf56`；镜像 ID `sha256:92b0c78052d39223b3120dead90c92b5014032b785edaee698dce38aa42dc4d8`。对齐原生产 `v0.2.14` 后，service、handler、admin、DTO、repository、routes 与 migrations 的相关 unit 回归全部通过；镜像构建中的前端类型检查、locale 检查和生产构建通过。
- 备份目录 `/opt/sub2api-backups/key-concurrency-d627bbf56/`：`database.sql.gz`（93,319,246 字节，已通过 `gzip -t`）、`docker-compose.yml`、`sub2api.env`、`nginx.conf`、容器状态及配置校验和。备份含敏感配置，仅用于受控恢复。
- 仅替换 Compose 的镜像行，执行 `docker compose --env-file /opt/sub2api/.env -f /opt/sub2api/docker-compose.yml up -d --no-deps --wait --wait-timeout 120 sub2api`；Nginx 代理容器 ID 未变化，`.env` 与 Nginx 配置校验和不变，展开后的端口与网络配置不变。
- 迁移 `245_add_api_key_concurrency.sql` 于 `2026-10-08 13:06:16 +08` 自动应用。验收时原有 22 个 Key 全部 `concurrency=0`，`qqqcumber`（ID 16）仍为 `0`，未自动启用限流。
- 本机及公网 Nginx `/health` 均返回 200 / `{"status":"ok"}`。用 5 分钟有效期的临时 Key、只属于该 Key 的 Redis 满槽状态，通过公网 Chat 路由验证 `429` / `gateway_concurrency_limit`，消息明确为 `api_key` 并发超限；未进入模型上游。临时 Key 和槽位已删除。
- 回滚只需将 Compose 的镜像行恢复为 `sub2api-trace:0.2.14-ef40f1c`，再从 `/opt/sub2api` 以原 `.env` 执行 `up -d --no-deps --wait sub2api`。迁移 245 的新增列及触发器兼容旧代码，不必删除新增列或恢复整库；旧镜像不会执行 Key 上限。
- **API 密钥页入口改进**：管理员在并发列旁直接点击编辑图标，独立修改和保存上限，不必进入用户管理。普通用户仍只读，保存仍调用管理员接口。隔离真实界面/API 验证了保存后刷新保持、设为 0 清除、负数禁用保存、后端 404 时保留输入，以及普通用户无编辑入口 / 管理接口 403 / 普通修改接口不能清除上限。
- **直接编辑入口生产发布**：镜像 `sub2api-trace:0.2.14-1ed1267fb`，ID `sha256:3be7a00dd789e7fbbee4e897aef9e03f59b1d6186366d42a7a84065e05a28693`，源码目录 `/opt/sub2api-build-1ed1267fb`。发布前备份 `/opt/sub2api-backups/keyui-1ed1267fb/`，数据库压缩备份 93,352,251 字节并通过完整性检查。无新数据库迁移；仅更换镜像并重建网关，`.env`、Nginx 配置校验和、端口/网络及本次发布前后的代理容器 ID 均保持不变。所有 Key 的上限与发布前快照一致，`qqqcumber` 仍为 `0`。本机和公网健康正常，新 `KeysView-BjMGZRIL.js` 资源经 Nginx 返回 200。前端类型检查、locale 完整性和生产构建通过。回滚到 `0.2.14-d627bbf56` 不需恢复数据库。


回滚 `9a945b1`（OpenRouter 平台）：**先改账号、再换镜像**——旧代码不认识 `openrouter` 平台，直接换镜像会让 OpenRouter 账号无法调度。① SQL：`update accounts set platform='deepseek', updated_at=now() where id in (22,38)`，并给这两个账号各插一条 `scheduler_outbox` 的 `account_changed`、给分组 7/9/12 各插一条 `group_changed`；② image 改回 `sub2api-trace:0.2.8-dc9713e`（该镜像已清理，需先按提交 `dc9713e` 重新构建）后 `up -d`。迁移 `240z` 只是扩大 CHECK 取值，旧代码无感，不用回退；`extra.openrouter_provider_routing` 对旧代码无影响。迁移前账号快照：`/opt/sub2api/backups/openrouter_accounts_pre_migration_20260924-124745.tsv`；部署前库备份：`/opt/sub2api/backup-pre-0.2.8-9a945b1-20260924-1246.sql.gz`。

回滚 `0.2.14-c1eecf318` / `0.2.14-b80d9c885` / `0.2.14-dcefc432d`（2026-10-16 部署，Kimi 月额度；后两个只多前端标签改动）：**无数据库迁移**，只是账号 `extra` 里多了 `kimi_monthly_*` 键、月额度套餐账号的 `kimi_weekly_*` 被置 null，旧代码忽略/当缺失处理 → 直接把 image 改回 `sub2api-trace:0.2.14-1ed1267fb`，在 `/opt/sub2api` 执行 `docker compose up -d --no-deps --wait sub2api` 即可。部署前库备份：`/opt/sub2api/backup-pre-0.2.14-dcefc432d-20261016-1445.sql.gz`（已 `gzip -t`）；compose 备份：`docker-compose.yml.bak-20261016-1445`。部署验收：health 200、`[CNRecovery] started`、启动后无错误日志；手动刷新后 135/185 快照为 `kimi_monthly_used_percent=100`、`kimi_monthly_reset_at=2026-10-28T00:00:00Z`、周快照置 null，elsa-2 为月 46.07%（周快照置 null），周额度账号照常写周；归队服务对 135/185 输出 `deferred (quota_still_full), next attempt at 2026-10-28T00:00:00Z`，不再每 6h 发注定失败的验证请求。二进制内嵌前端已确认 `windowMonthly:"30d"`（账号页）。渠道监控页 `MonitorQuotaView` 原先没有映射 `monthly` 窗口、直接显示原始 token，c1eecf318 补映射到 `monitorCommon.quota.windows.monthly` 并把中英文改为 `30d`。注意 `zh/admin/overview.ts` 里的 `platformQuota.windowMonthly: '月'` 是用户「平台限额」日/周/月消费上限的标签，与 Kimi 额度无关，不要改。

回滚 `0.2.14-ef40f1c`（2026-10-08 部署）：两个迁移都对旧代码无害——`241_add_payment_order_bonus_amount.sql` 纯追加 `payment_orders.bonus_amount` 列；`241_add_typesafe_platform.sql` 只把平台 CHECK 扩成含 typesafe + openrouter 的超集（已核对两个约束都同时含二者）——直接改回 `sub2api-trace:0.2.11-111379b8f` 后 `up -d` 即可，无需恢复数据库。部署前库备份：`/opt/sub2api/backup-pre-0.2.14-ef40f1c-20261008-1101.sql.gz`；compose 备份：`docker-compose.yml.bak-20261008-1101`。部署验收：health 200、两个 241 迁移 applied、`[CNRecovery] started`、三组 `/v1/models` 与部署前一致（组 12 含 3 个 jailbreak 自定义模型）；实测 `gemini-3.8-flash` + low → `gemini-3.8-flash-low`、`k3`、`z-ai/glm-5.3-flash`（OpenRouter 账号 38）、`jailbreak/k3`（→ k3）均 200；部署后无错误日志、trace 正常落盘。上游 v0.2.12 起重置密码 token 改为哈希存储：升级前已发出、尚未使用的重置密码链接失效，用户需重新申请。

回滚 `0.2.11-111379b8f`（2026-10-02 修复版）：纯代码修复（自定义模型在 OpenAI 网关调度被 account_model 归属校验误杀 → 503），**无数据库迁移** → 直接换回 `sub2api-trace:0.2.11-80ab6d0`（或 `0.2.8-57932a0`）后 `up -d` 即可。compose 备份：`docker-compose.yml.bak-20261002-fix1`。验收：`jailbreak/glm-5.3`（→ z-ai/glm-5.3 @ Friendli；当时名 `jailbreak/glm`，同日改名）、`jailbreak/k3`（→ k3，prompt_tokens≈8365）、`jailbreak/glm-5.3-flash`（→ z-ai/glm-5.3-flash @ Wafer，返回 OK）三发均 200。

回滚 `0.2.11-80ab6d0`（2026-10-02 部署）：迁移 242/243/244 均为纯追加（242 新增 custom_models 两张表、243 `accounts.recovery_version` 列、244 `custom_models.injection_mode` 列 + CHECK），旧代码忽略新表/新列 → 直接改回 `sub2api-trace:0.2.8-57932a0` 后 `up -d` 即可，无需恢复数据库。部署前库备份：`/opt/sub2api/backup-pre-0.2.11-20261002-0020.sql.gz`；compose 备份：`docker-compose.yml.bak-20261002`。**本镜像由本地提交 `80ab6d05d` 的源码归档在服务器直接构建**（构建时该提交尚未推送 GitHub，推送后即对齐）。部署验收：health 200、242/243/244 迁移 applied、`[CNRecovery] started (interval=10m0s)`、无 ERROR 日志、trace 正常落盘。

回滚：v0.2.8（`dc9713e`）带 3 个纯追加迁移（238b 审核日志 `engine_meta` 列、239 渠道定价 `reasoning_effort_multipliers` 列、240 联盟流水 `operation_id` 列 + 部分唯一索引），旧代码忽略新列，直接改回 `sub2api-trace:0.2.7-f2d1cb7` 后 `up -d` 即可，无需恢复数据库；Gemini 白名单语义两版一致。部署前库备份：`/opt/sub2api/backup-pre-0.2.8-dc9713e-20260924-1054.sql.gz`。再往前回滚：`f2d1cb7`（Gemini 裸名选变体）无数据库迁移，但**分组白名单依赖新代码**——白名单只写了裸名 `gemini-3.8-flash`，旧代码不会放行 `-high` 等变体。回滚镜像到 `sub2api-trace:0.2.7-004afb6` 时，须同时用 `/opt/sub2api/backups/groups_model_allowlist_20260923-183332.tsv` 恢复分组 7/9/12 的 `model_allowlist` 并清 `apikey:auth:*` 缓存（或在后台逐个分组保存一次，自动失效缓存）。更早的 TLS 指纹改动同样无迁移，账号 `extra.enable_tls_fingerprint` 会被旧代码忽略。

## 测试

- **Trace 模块**：`backend/internal/pkg/trace/` 下 18 个测试覆盖：三种格式 SSE 重组（含 Responses 断流退化）、思考链/工具参数分片合并、格式嗅探、错误事件、截断、空 keepalive、会话 ID 五级优先级、中间件端到端（验证客户端收到的响应零改动）。
- **CN 治理模块**：`service/ratelimit_classifier_test.go`（分类器 17 行表测：kimi 精确/宽松、周/5h 额度、鉴权、未知、HTML、空 body、非 CN、裸 5h 不匹配）、`service/kimi_monthly_quota_test.go`（Kimi 月额度套餐：usages 解析、换套餐清旧快照、月文案分类、月满按月停调/归队/阈值、判满线 100、文案窗口补快照滞后）、`service/cn_quota_pause_window_test.go`（周满分档、快照缺失、阈值配置）、`service/ratelimit_cn_403_side_effects_test.go`（额度 403 永不进禁用计数、并发 403 只停车、幂等合并、WS/流内落点）、`service/account_error_recovery_service_test.go`（reset_at 已过即恢复、仍满零请求退避、无快照走验证仲裁、CAS 顺序与冲突预算、leader 锁）。完整验证命令见上文「跟进官方更新」。
- **Gemini 裸名选变体**：`service/antigravity_gemini_thinking_variant_test.go`（三种协议的档位推导、后缀裁剪、自映射仍推导）、`service/antigravity_gateway_compat_test.go`（Chat/Responses/Messages 端到端断言上游 model：low/minimal→-low、medium→-medium、xhigh/缺省→-high、显式变体原样）、`service/group_model_allowlist_test.go`（裸名条目放行变体但列表只出裸名；单变体条目不外扩）。
- **OpenRouter 平台 + 供应商路由**：`service/openrouter_platform_test.go`（平台默认地址 / 协议 / 只按量、composite 模型归属规则、跨平台账号池匹配，以及端到端调度：优先 DeepSeek 中转，排除后接管到 OpenRouter 账号，未映射模型的 OpenRouter 账号与非 composite 分组不入池）、`service/openrouter_provider_routing_test.go`（配置清洗、注入条件、endpoints 解析）、`service/openrouter_provider_routing_forward_test.go`（unit 标签：三种入站经 adaptive 与平台默认地址都带上 provider，Anthropic 误填 `/api/v1` 不拼出 `/v1/v1/messages`）；前端 `openrouter.credentialsBuilder.spec.ts`、`OpenRouterRoutingModal.spec.ts`。
- **账号级 TLS 指纹**：`service/account_tls_fingerprint_policy_test.go` 覆盖跨平台开关、OpenAI-compatible 真实出站与关闭回退；`repository/http_upstream_test.go` 覆盖 profile 变更重建连接、跨账号连接隔离、HTTPS 代理禁止静默降级。验证：`go test ./...`、`go test -tags unit ./internal/service/`、`go build ./...`、`go vet ./...`、前端 297/2231、`vue-tsc --noEmit` 全过；生产隔离组实测见上文第 8 项。

## 后台管理功能（已上线）

- **会话 Trace 页**（/admin/traces）：会话列表（含 API Key 名称）、统计卡、按会话下载（热数据现场压缩、已归档直取）、手动/自动归档。定时归档配置存 `data/trace-settings.json`，进程内调度器，默认每天 03:00、只留今天为热数据。
- **账号用量导出页**（/admin/account-usage-export）：usage_logs 按 账号×周期×模型 聚合，自定义时间范围，CSV（UTF-8 BOM）。费用按独立定价表（`data/export-pricing.json`，/admin/export-pricing 读写）计算，**与系统计费完全隔离**；未定价模型费用列显示 "-"。
- 接线方式：`cmd/server/wire_gen.go` 里手工构造两个 service 挂到 `AdminHandlers`（重新跑 wire codegen 需补回这 4 行，文件内有注释标记）。

## 待做（第二阶段）

- **导出器**：扫 trace 目录 → 逐轮样本（压缩边界前缀检测）→ 训练用 JSONL（thinking 转 `<think>`、tool_use 转目标模型 function-calling 格式、过滤 `complete=false`）
- 可选：按会话保留策略、管理后台查看页
