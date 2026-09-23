# Session Trace 二开说明

按会话留存完整对话链路（用户输入 / 模型输出 / 思考链 / 工具调用），用于后续蒸馏微调。

> **这个 fork 是什么**：`Wei-Shaw/sub2api` 官方版（当前合并到 **v0.2.7**，2026-09-19）+ 三个自研/增强模块——
> ① 本文档讲的 **Session Trace 录制**；② **CN（kimi 等国产 Coding Plan）账号并发受限治理**（403 三分类、额度耗尽停调自动恢复、历史 error 账号自动归队），设计文档见 **`PLAN-cn-cap-v6.md`**；③ **账号级 TLS 指纹出站**（任意账号按需勾选，解决 Cloudflare 对 Go TLS/HTTP 指纹的 403/1010 封禁）。
> 部署分支：fork 的 **`trace` 分支**（GitHub 默认分支已设为 trace）。
> 当前生产镜像：`sub2api-trace:0.2.7-f2d1cb7`（2026-09-23 部署，健康运行中；上一版 `0.2.7-004afb6`）。

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

- Fork：`github.com/lyd-0718/sub2api`，分支 `trace`（当前已合并官方 `v0.2.5`（2026-09-16）；历史上一个合并点是 `v0.2.3`）
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
git push origin trace
```

（v0.2.5 合并实录：197 个提交只有 1 个冲突——`ratelimit_cn_providers.go` 的额度快照辅助函数：我方删旧函数换周满分档、上游给 OpenCodeGo 加月度窗口。双边保留解决：CN 平台走 `cnQuotaPauseWindow` 新规则，OpenCodeGo 走旧函数。上游 v0.2.5 自带 2 个挂掉的前端测试（ChannelMonitorView.grok 的供应商计数、GroupsView.codexManifest 的 Pinia），干净 upstream/main 上同样挂，不是合并问题，不要替上游修——修了反而制造未来的合并噪音。）

（v0.2.7 合并实录 2026-09-19：71 个提交 2 个文件冲突——上游 PR #7340（kimi 配额耗尽 403 停车）与我方 CN 治理模块正面撞车，同一功能两种实现。解决：`handle403`/`applyCNProviderReactive429`/`handleCNProviderConcurrencyLimit403` 保留我方（分类器收口早退 + 周/5h 分档 + 并发短冷却防级联停车，上游 10 分钟冷却会缩号池）；上游新增 `cooldownCNProviderToQuotaSnapshotReset` 双边保留（OpenCodeGo 429 分支依赖，CN Coding Plan 仍走我方 `cnCodingPlan429Cooldown` 分档）；删上游 `ratelimit_service_cn_quota_403_test.go`（测被删的 3 参同名符号，Go 无重载）。v0.2.5 时代那 2 个上游坏前端测试已被上游修复，本轮 297/2231 全过。教训：`go test -tags unit` 出现 FAIL 先落盘复跑再定位——本轮首轮 unit 挂、复跑 0 失败，是 flaky 不是合并问题。）

**merge 冲突面（trace 分支对上游的全部改动）：**

1. `backend/internal/server/routes/gateway.go`：3 行（trace 中间件注册）。
2. `cmd/server/wire_gen.go`：手工装配若干行（trace admin 2 个 service，均有注释标记；wire codegen 重跑需补回）。
3. **429 证据停车**（`backend/internal/service/ratelimit_cn_providers.go`）：套餐号 429 按额度快照分级，瞬时 429 只短冷却 60s；配置 `gateway.cn_providers.rate_limit_cooldown_seconds` / `quota_exhausted_percent`。上游若重写此文件，保留我方 `cnCodingPlan429Cooldown` 分支逻辑。
4. `config/config.go`：纯追加（CNProviders 2 字段 + defaults），一般自动合并。
5. 前端：`TraceView.vue` / `AccountUsageExportView.vue` / `api/traceAdmin.ts` / i18n / 侧边栏入口（AppSidebar.vue），均为新增文件或纯追加。
6. **OpenRouter 余额探测**（2026-09-14 新增）：上游文件 `cn_provider_balance_service.go` 仅 4 处小改（`CNProviderBalanceEntry.Label` 字段、`cnBalanceURL` 的 openrouter 分支、deepseek 解析分流、快照写入 label）；识别/解析/`/key` 限额查询全在新文件 `cn_provider_balance_openrouter.go`（自有文件，零冲突）。前端 `CNProviderBalanceCell.vue` + `api/admin/cnProviders.ts` + zh/en i18n 各一处小改（label 前缀渲染，无 label 时行为不变）。
7. **CN 并发受限治理**（2026-09-16 定稿，设计见 `PLAN-cn-cap-v6.md`）：自有文件零冲突——`service/ratelimit_classifier.go`（403 三分类）、`service/cn_quota_pause_window.go`（周满看周窗口分档）、`service/account_error_recovery_service.go`（历史 error 账号自动归队：本地快照 reset_at 判定恢复、零额度探测，最小请求点火验证防鉴权死账号死循环，验证后重读 updated_at 再 CAS）。无数据库迁移（v5 时代的 238/240 均已废弃删除；账号并发由需求方手动管理，系统不碰）。上游文件改动：`repository/scheduler_cache.go`（白名单 +15 个 CN 额度键）；`service/ratelimit_service.go`（CN 403 分类器闸 + 幂等去重）；`service/ratelimit_cn_providers.go`（并发 403 → 30s 停车不进 403 计数；额度分支走窗口分档）；`service/gateway_forward.go` / `openai_ws_forwarder_support.go` / `openai_gateway_passthrough.go` / `openai_gateway_messages.go` / `openai_gateway_chat_completions.go` / `openai_gateway_upstream_errors.go`（流内/WS/SSE bare error 在 `openAIStream403AccountFailure`/`shouldFailover` 判定**之前**接分类器）；`service/setting_update.go`（默认阈值表 +kimi:85）；`repository/account_repo.go` + `service/account_service.go`（+`ListCNQuotaDisabled`/`RestoreRecoveredAccount` 及接口签名）；`config/config.go`（`cn_providers` +3 个归队配置键，纯追加）；`cmd/server/wire_gen.go`（归队服务手工接线 4 行，注释标记）；`server/routes/trace_admin.go`（nil 守卫，顺带修 trace 模块既有 panic）。前端：与上游基线完全一致（v5 时代的 cap 展示与 kimi 新建默认值均已按需求方决定回退）。**注意**：v5 时代的 cap 限速器（表/store/夹帽/管理接口）、探测回升/熔断、迁移 238/240、前端 cap 相关改动已于 2026-09-16 按需求方决定整体移除，如在上游看到相关设计文档残留以本节为准。
8. **账号级 TLS 指纹出站**（2026-09-21，核心 `ad5fe7a`；通用前端 `722b510` / `997f45a`）：TLS 指纹开关不再绑定平台、账号类型或模型；`service/http_upstream_port.go` 的 `doAccountHTTPUpstream` 统一真实流量、账号测试、额度/余额探测与 error 恢复。OpenAI-compatible、Gemini、Antigravity、Grok、Bedrock、CN Provider、Ollama HTTP/SSE 路径均接入；开关关闭直接保持原 `Do`，开启才走 `DoWithTLS`。`repository/http_upstream.go` 按账号 + profile 内容摘要隔离连接池，模板变更不复用旧 ClientHello；HTTPS 代理不再静默降级为 Go 指纹（明确报错，支持直连/HTTP CONNECT/SOCKS5）。前端创建/编辑页的 TLS 卡片已移出 Anthropic-only 容器；任意平台都能显示、读取和保存开关，移除每请求随机 profile，只允许固定内置或指定模板。生产验收：x5m5x 专属组非流式/流式均 200，流式完整 `[DONE]`；12,032 token 首写后二次命中 10,823 cached tokens，4 笔成功、0 错误；最终 Kimi 编辑弹窗已视觉确认开关可见且为开启状态。
9. **Gemini 裸名按 effort 选变体**（2026-09-23）：Antigravity 上游只有 `gemini-3.x-flash-low/-medium/-high/-tiered`，裸名 404；Chat/Responses/Messages 转 Gemini 时 effort 不进请求体，变体是唯一的思考深度开关。上游 `0f4d8ac` 只在 Gemini 原生路径按 thinkingConfig 选变体，且把 credentials 里的裸名自映射当"用户显式配置"跳过——而后台按 `DefaultAntigravityModelMapping` 建号会写入这条自映射，生产 3 个号全中招。我方改动（全在上游文件）：`service/antigravity_gemini_thinking_variant.go`（`resolveGeminiThinkingVariant` 改收档位参数；新增 OpenAI `reasoning_effort`/`reasoning.effort` 与 Claude `output_config.effort`/`thinking` 两个档位推导；裸名自映射不再阻断推导，只尊重映射到别的模型；删 `accountRawModelMappingHasKey`）；`antigravity_gateway_compat.go` / `antigravity_gateway_claude.go` / `antigravity_gateway_gemini.go` / `antigravity_gateway_service.go`（TestConnection）各一处调用点改走 `mapAntigravityModelWithThinkingLevel`；`service/group_model_allowlist.go`（候选形式 + Gemini 思考后缀：白名单写裸名即放行其变体，列表只展示裸名）。配套配置：分组 7/9/12 白名单的 4 个 gemini 变体换成 `gemini-3.8-flash`。上游若自行修复同一问题，以上游实现为准并核对这两点语义（自映射不阻断、白名单裸名放行变体）。


（kimi 缓存保活模块已于 2026-09-04 移除：实测有用但探测费相对省下的冷启动费性价比不高。历史见 git log。）
（`x-session-id` 粘性路由曾作为第 4 条改动，v0.2.1 合并时确认为重复代码已删除——上游名单的 `openCodeSessionIDHeader` 常量值就是 `X-Session-Id`。）

**服务器部署（标准流程，2026-09-21 按 v0.2.7 实际部署更新）：**

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

回滚：`f2d1cb7`（Gemini 裸名选变体）无数据库迁移，但**分组白名单依赖新代码**——白名单只写了裸名 `gemini-3.8-flash`，旧代码不会放行 `-high` 等变体。回滚镜像到 `sub2api-trace:0.2.7-004afb6` 时，须同时用 `/opt/sub2api/backups/groups_model_allowlist_20260923-183332.tsv` 恢复分组 7/9/12 的 `model_allowlist` 并清 `apikey:auth:*` 缓存（或在后台逐个分组保存一次，自动失效缓存）。更早的 TLS 指纹改动同样无迁移，账号 `extra.enable_tls_fingerprint` 会被旧代码忽略。

## 测试

- **Trace 模块**：`backend/internal/pkg/trace/` 下 18 个测试覆盖：三种格式 SSE 重组（含 Responses 断流退化）、思考链/工具参数分片合并、格式嗅探、错误事件、截断、空 keepalive、会话 ID 五级优先级、中间件端到端（验证客户端收到的响应零改动）。
- **CN 治理模块**：`service/ratelimit_classifier_test.go`（分类器 17 行表测：kimi 精确/宽松、周/5h 额度、鉴权、未知、HTML、空 body、非 CN、裸 5h 不匹配）、`service/cn_quota_pause_window_test.go`（周满分档、快照缺失、阈值配置）、`service/ratelimit_cn_403_side_effects_test.go`（额度 403 永不进禁用计数、并发 403 只停车、幂等合并、WS/流内落点）、`service/account_error_recovery_service_test.go`（reset_at 已过即恢复、仍满零请求退避、无快照走验证仲裁、CAS 顺序与冲突预算、leader 锁）。完整验证命令见上文「跟进官方更新」。
- **Gemini 裸名选变体**：`service/antigravity_gemini_thinking_variant_test.go`（三种协议的档位推导、后缀裁剪、自映射仍推导）、`service/antigravity_gateway_compat_test.go`（Chat/Responses/Messages 端到端断言上游 model：low/minimal→-low、medium→-medium、xhigh/缺省→-high、显式变体原样）、`service/group_model_allowlist_test.go`（裸名条目放行变体但列表只出裸名；单变体条目不外扩）。
- **账号级 TLS 指纹**：`service/account_tls_fingerprint_policy_test.go` 覆盖跨平台开关、OpenAI-compatible 真实出站与关闭回退；`repository/http_upstream_test.go` 覆盖 profile 变更重建连接、跨账号连接隔离、HTTPS 代理禁止静默降级。验证：`go test ./...`、`go test -tags unit ./internal/service/`、`go build ./...`、`go vet ./...`、前端 297/2231、`vue-tsc --noEmit` 全过；生产隔离组实测见上文第 8 项。

## 后台管理功能（已上线）

- **会话 Trace 页**（/admin/traces）：会话列表（含 API Key 名称）、统计卡、按会话下载（热数据现场压缩、已归档直取）、手动/自动归档。定时归档配置存 `data/trace-settings.json`，进程内调度器，默认每天 03:00、只留今天为热数据。
- **账号用量导出页**（/admin/account-usage-export）：usage_logs 按 账号×周期×模型 聚合，自定义时间范围，CSV（UTF-8 BOM）。费用按独立定价表（`data/export-pricing.json`，/admin/export-pricing 读写）计算，**与系统计费完全隔离**；未定价模型费用列显示 "-"。
- 接线方式：`cmd/server/wire_gen.go` 里手工构造两个 service 挂到 `AdminHandlers`（重新跑 wire codegen 需补回这 4 行，文件内有注释标记）。

## 待做（第二阶段）

- **导出器**：扫 trace 目录 → 逐轮样本（压缩边界前缀检测）→ 训练用 JSONL（thinking 转 `<think>`、tool_use 转目标模型 function-calling 格式、过滤 `complete=false`）
- 可选：按会话保留策略、管理后台查看页
