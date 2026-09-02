# Session Trace 二开说明

按会话留存完整对话链路（用户输入 / 模型输出 / 思考链 / 工具调用），用于后续蒸馏微调。

## 这套东西是什么

sub2api 官方镜像 + 一个"录音笔"中间件。所有经过 `/v1/messages`（Claude Code）、`/v1/chat/completions`（OpenAI 格式）、`/v1/responses`（Codex CLI）的请求，请求体和响应体被原样复制一份，按会话归档成 gzip 压缩的 JSON 文件。

**对官方代码的改动只有 3 行**（`backend/internal/server/routes/gateway.go`：1 行 import + 2 行注册中间件），其余全部是独立目录 `backend/internal/pkg/trace/` 下的新文件。

## 数据存在哪

服务器：`/opt/sub2api/data/traces/<日期>/<会话ID>/<时间戳>-<请求ID>.json.gz`

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

- Fork：`github.com/lyd-0718/sub2api`，分支 `trace`（基于官方 `v0.1.185`）
- 本地：`~/Desktop/sub2api`
- 服务器构建目录：`/opt/sub2api-trace`（每次部署重新拉 trace 分支 tarball 构建）

**跟进官方更新：**

```bash
cd ~/Desktop/sub2api
git fetch https://github.com/Wei-Shaw/sub2api.git --tags
git checkout trace
git merge v0.1.186        # 换成新 tag；冲突面只有 gateway.go 那 3 行
cd backend && go build ./... && go test ./internal/pkg/trace/
git push origin trace
```

服务器重新部署：

```bash
ssh relay
cd /opt/sub2api-trace
curl -sL https://github.com/lyd-0718/sub2api/archive/refs/heads/trace.tar.gz | tar xz --strip-components=1
docker build -t sub2api-trace:<新版本号> .
# 改 /opt/sub2api/docker-compose.yml 的 image tag，然后：
cd /opt/sub2api && docker compose up -d sub2api
```

## 测试

`backend/internal/pkg/trace/` 下 18 个测试覆盖：三种格式 SSE 重组（含 Responses 断流退化）、思考链/工具参数分片合并、格式嗅探、错误事件、截断、空 keepalive、会话 ID 五级优先级、中间件端到端（验证客户端收到的响应零改动）。

## 待做（第二阶段）

- **导出器**：扫 trace 目录 → 逐轮样本（压缩边界前缀检测）→ 训练用 JSONL（thinking 转 `<think>`、tool_use 转目标模型 function-calling 格式、过滤 `complete=false`）
- 可选：按会话保留策略、管理后台查看页
