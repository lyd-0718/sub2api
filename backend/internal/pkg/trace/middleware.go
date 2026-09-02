package trace

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

var (
	cfgOnce sync.Once
	cfg     *Config
	sinkRef *sink
)

// Middleware 返回会话 trace 采集中间件。
// 注册在 ApiKeyAuth 之后：此时 gin context 中已有鉴权后的 APIKey。
func Middleware() gin.HandlerFunc {
	cfgOnce.Do(func() {
		cfg = LoadConfig()
		if cfg.Enabled {
			sinkRef = newSink(cfg)
		}
	})
	loaded, s := cfg, sinkRef
	return func(c *gin.Context) {
		handle(c, loaded, s)
	}
}

// handle 是采集中间件的核心逻辑，独立出来便于测试注入配置与 sink。
func handle(c *gin.Context, cfg *Config, s *sink) {
	if cfg == nil || !cfg.Enabled || c.Request.Method != http.MethodPost || !cfg.pathAllowed(c.Request.URL.Path) {
		c.Next()
		return
	}

	var apiKeyID, userID int64
	var groupID *int64
	if apiKey, ok := middleware.GetAPIKeyFromContext(c); ok && apiKey != nil {
		if !cfg.apiKeyAllowed(apiKey.ID) {
			c.Next()
			return
		}
		apiKeyID, userID, groupID = apiKey.ID, apiKey.UserID, apiKey.GroupID
	} else if len(cfg.APIKeyIDs) > 0 {
		c.Next()
		return
	}

	if cfg.SampleRate < 1 && rand.Float64() >= cfg.SampleRate {
		c.Next()
		return
	}

	// 读取请求体并原样归还，下游 handler 无感知。
	body, truncated := readBody(c.Request, cfg.MaxBodyBytes)
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Request.ContentLength = int64(len(body))

	rec := &record{
		SessionID: resolveSessionID(c, body, apiKeyID),
		RequestID: requestID(c),
		Model:     gjson.GetBytes(body, "model").String(),
		Stream:    gjson.GetBytes(body, "stream").Bool(),
		APIKeyID:  apiKeyID,
		UserID:    userID,
		StartedAt: time.Now(),
		Request:   body,
	}
	if groupID != nil {
		rec.GroupID = *groupID
	}

	tw := newTeeWriter(c.Writer, cfg.MaxBodyBytes)
	c.Writer = tw

	c.Next()

	rec.DurationMs = time.Since(rec.StartedAt).Milliseconds()
	rec.HTTPStatus = tw.Status()
	rec.RequestTrunc = truncated
	rec.Response = assemble(tw.Bytes(), tw.Truncated())
	s.enqueue(rec)
}

// readBody 读完请求体；超过 limit 时截断并标记。
func readBody(r *http.Request, limit int64) ([]byte, bool) {
	if r.Body == nil {
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, false
	}
	if int64(len(data)) > limit {
		return data[:limit], true
	}
	return data, false
}

// 优先 X-Session-Id 自定义头（自家客户端可显式指定）；
// 其次 Claude Code metadata.user_id 中的 session UUID（协议事实，非猜测）；
// 缺失时退化为 apiKeyID + 首条 user 消息内容的哈希：同一会话的首轮请求
// 内容相同因此 hash 稳定，足以把同会话的轮次归并到一起。
func resolveSessionID(c *gin.Context, body []byte, apiKeyID int64) string {
	if sid := strings.TrimSpace(c.GetHeader("X-Session-Id")); sid != "" {
		return sid
	}
	// Codex CLI 自带会话头
	if sid := strings.TrimSpace(c.GetHeader("session_id")); sid != "" {
		return "codex-" + sid
	}
	if cid := strings.TrimSpace(c.GetHeader("conversation_id")); cid != "" {
		return "codex-conv-" + cid
	}
	if uid := gjson.GetBytes(body, "metadata.user_id").String(); uid != "" {
		if parsed := service.ParseMetadataUserID(uid); parsed != nil && parsed.SessionID != "" {
			return parsed.SessionID
		}
	}
	// OpenAI 格式客户端：user 字段次之
	if u := strings.TrimSpace(gjson.GetBytes(body, "user").String()); u != "" {
		return "user-" + u
	}
	// 取第一条 user 角色消息做哈希输入。注意不能用 messages[0]：
	// OpenAI 格式客户端（如 omp）的 messages[0] 是恒定的系统提示词，
	// 会导致同 key 下所有会话哈希相同、错误并组。
	first := firstUserMessageHashInput(body)
	sum := sha256.Sum256([]byte(strconv.FormatInt(apiKeyID, 10) + "|" + first))
	return "anon-" + hex.EncodeToString(sum[:8])
}

// firstUserMessageHashInput 返回第一条 user 消息的原始 JSON；
// 没有 user 消息时退回 messages[0]（部分客户端首轮只有一条）。
func firstUserMessageHashInput(body []byte) string {
	fallback := ""
	for _, msg := range gjson.GetBytes(body, "messages").Array() {
		if !msg.IsObject() {
			continue
		}
		if fallback == "" {
			fallback = msg.Get("content").Raw
		}
		if msg.Get("role").String() == "user" {
			return msg.Get("content").Raw
		}
	}
	return fallback
}

func requestID(c *gin.Context) string {
	if c.Request != nil {
		if v, ok := c.Request.Context().Value(ctxkey.ClientRequestID).(string); ok && v != "" {
			return v
		}
	}
	return hex.EncodeToString([]byte(strconv.FormatInt(time.Now().UnixNano(), 10)))
}
