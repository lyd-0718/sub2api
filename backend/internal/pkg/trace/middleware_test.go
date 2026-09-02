package trace

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

const sessionUserID = "user_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef_account__session_11111111-2222-3333-4444-555555555555"

func testConfig(dir string) *Config {
	return &Config{
		Enabled:      true,
		Dir:          dir,
		MaxBodyBytes: 1 << 20,
		QueueSize:    16,
		Workers:      1,
		SampleRate:   1.0,
		Paths:        []string{"/v1/messages"},
	}
}

func runTraceRequest(t *testing.T, cfg *Config, s *sink, path, body string, respond func(w http.ResponseWriter)) *httptest.ResponseRecorder {
	t.Helper()
	// 链路：trace 中间件 → 模拟下游 handler（读请求体、写 SSE 响应）
	downstream := func(c *gin.Context) {
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.Copy(io.Discard, c.Request.Body)
		respond(c.Writer)
	}
	r := gin.New()
	r.POST("/v1/messages", func(c *gin.Context) { handle(c, cfg, s) }, downstream)
	r.POST("/v1/models", func(c *gin.Context) { handle(c, cfg, s) }, downstream)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	s.wait()
	return rec
}

func readTraceFile(t *testing.T, dir string) *record {
	t.Helper()
	var found string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && strings.HasSuffix(p, ".json.gz") && found == "" {
			found = p
		}
		return nil
	})
	if found == "" {
		t.Fatal("no trace file written")
	}
	f, err := os.Open(found)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	var rec record
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	// 目录名必须是会话 ID
	if !strings.Contains(found, "11111111-2222-3333-4444-555555555555") {
		t.Fatalf("session dir wrong: %s", found)
	}
	return &rec
}

func TestMiddlewareCapturesSessionTrace(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	cfg := testConfig(dir)
	s := newSink(cfg)

	body := `{"model":"claude-opus-4-6","stream":true,"max_tokens":1024,"metadata":{"user_id":"` + sessionUserID + `"},"messages":[{"role":"user","content":"list files"}]}`

	rec := runTraceRequest(t, cfg, s, "/v1/messages", body, func(w http.ResponseWriter) {
		_, _ = io.WriteString(w, sseStream)
	})

	// 客户端必须收到完整原始 SSE（tee 不影响转发）
	if !strings.Contains(rec.Body.String(), "message_stop") {
		t.Fatal("client response damaged by tee")
	}

	got := readTraceFile(t, dir)
	if got.SessionID != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("session = %q", got.SessionID)
	}
	if got.Model != "claude-opus-4-6" || !got.Stream {
		t.Fatalf("meta = %+v", got)
	}
	if !strings.Contains(string(got.Request), "list files") {
		t.Fatal("request body not captured")
	}
	if got.Response == nil || !got.Response.Complete || len(got.Response.Blocks) != 3 {
		t.Fatalf("response = %+v", got.Response)
	}
	if got.Response.Blocks[0].Thinking != "先分析问题" {
		t.Fatalf("thinking = %+v", got.Response.Blocks[0])
	}
	if got.Response.Blocks[1].ToolName != "Bash" {
		t.Fatalf("tool = %+v", got.Response.Blocks[1])
	}
}

func TestMiddlewareSkipsNonMatchingPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	cfg := testConfig(dir)
	s := newSink(cfg)

	runTraceRequest(t, cfg, s, "/v1/models", `{}`, func(w http.ResponseWriter) {})

	if _, err := os.Stat(dir); err == nil {
		entries, _ := os.ReadDir(dir)
		if len(entries) > 0 {
			t.Fatal("non-matching path should not be captured")
		}
	}
}

func TestMiddlewareWhitelistSkipsUnknownKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	cfg := testConfig(dir)
	cfg.APIKeyIDs = []int64{999} // 白名单有值但请求无 apiKey 上下文
	s := newSink(cfg)

	runTraceRequest(t, cfg, s, "/v1/messages", `{"model":"m","messages":[]}`, func(w http.ResponseWriter) {})

	entries, _ := os.ReadDir(dir)
	if len(entries) > 0 {
		t.Fatal("request without whitelisted api key must not be captured")
	}
}

func TestResolveSessionIDFallback(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	c := &gin.Context{Request: httptest.NewRequest(http.MethodPost, "/v1/messages", nil)}
	a := resolveSessionID(c, body, 1)
	b := resolveSessionID(c, body, 1)
	if a != b || !strings.HasPrefix(a, "anon-") {
		t.Fatalf("fallback session unstable: %q vs %q", a, b)
	}
	if resolveSessionID(c, body, 2) == a {
		t.Fatal("different api keys must produce different fallback sessions")
	}
}

func TestResolveSessionIDHeader(t *testing.T) {
	c := &gin.Context{Request: httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)}
	c.Request.Header.Set("X-Session-Id", "my-conv-42")
	if got := resolveSessionID(c, []byte(`{"messages":[]}`), 1); got != "my-conv-42" {
		t.Fatalf("header session = %q", got)
	}
}

func TestFirstUserMessageSkipsSystemPrompt(t *testing.T) {
	// omp 形态：messages[0] 是恒定系统提示词，哈希输入必须跳过它
	body := []byte(`{"messages":[{"role":"system","content":"CONSTANT-SYSTEM-PROMPT"},{"role":"user","content":"session A opening"}]}`)
	c := &gin.Context{Request: httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)}
	a := resolveSessionID(c, body, 1)

	body2 := []byte(`{"messages":[{"role":"system","content":"CONSTANT-SYSTEM-PROMPT"},{"role":"user","content":"session B opening"}]}`)
	b := resolveSessionID(c, body2, 1)
	if a == b {
		t.Fatal("different first user messages must produce different sessions")
	}

	// 同会话后续轮次（历史变长，但第一条 user 消息不变）哈希必须稳定
	body3 := []byte(`{"messages":[{"role":"system","content":"CONSTANT-SYSTEM-PROMPT"},{"role":"user","content":"session A opening"},{"role":"assistant","content":"reply"},{"role":"user","content":"follow up"}]}`)
	if resolveSessionID(c, body3, 1) != a {
		t.Fatal("later turns of same session must keep the same hash")
	}
}
