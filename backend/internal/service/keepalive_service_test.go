package service

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func keepaliveTestConfig(enabled bool) *config.Config {
	cfg := &config.Config{}
	cfg.Gateway.Keepalive.Enabled = enabled
	cfg.Gateway.Keepalive.IntervalSeconds = 300
	cfg.Gateway.Keepalive.MaxIdleSeconds = 3600
	cfg.Gateway.Keepalive.MinPromptTokens = 100 // 测试用小门槛
	cfg.Gateway.Keepalive.MaxBodyBytes = 1 << 20
	cfg.Gateway.Keepalive.ProbeMaxTokens = 16
	cfg.Gateway.Keepalive.MaxEntriesPerAccount = 2
	return cfg
}

func keepaliveTestAccount() *Account {
	return &Account{
		ID:          42,
		Platform:    PlatformKimi,
		Credentials: map[string]any{"account_mode": AccountModeCoding},
	}
}

func keepaliveTestCtx(apiKeyID int64) context.Context {
	gid := int64(2)
	return WithKeepaliveCaptureInfo(context.Background(), &KeepaliveCaptureInfo{
		SessionHash: "sess-abc",
		GroupID:     &gid,
		APIKeyID:    apiKeyID,
		UserID:      7,
	})
}

func keepaliveBigBody(tokens int) []byte {
	// 构造 len/3 >= tokens 的请求体（内容无关紧要，捕获只看大小与 model 字段）。
	padding := tokens*3 + 64
	return []byte(fmt.Sprintf(`{"model":"kimi-k3","messages":[{"role":"user","content":"%*s"}]}`, padding, "x"))
}

func TestKeepaliveCapture_Gates(t *testing.T) {
	ctx := keepaliveTestCtx(11)

	t.Run("disabled", func(t *testing.T) {
		s := NewKeepaliveService(keepaliveTestConfig(false), nil, nil, nil, nil)
		s.Capture(ctx, keepaliveTestAccount(), "https://api.kimi.com/v1/messages", http.Header{}, keepaliveBigBody(200))
		require.Zero(t, s.entryCount())
	})

	t.Run("non kimi account", func(t *testing.T) {
		s := NewKeepaliveService(keepaliveTestConfig(true), nil, nil, nil, nil)
		acc := &Account{ID: 1, Platform: PlatformOpenAI, Credentials: map[string]any{}}
		s.Capture(ctx, acc, "https://api.openai.com/v1/responses", http.Header{}, keepaliveBigBody(200))
		require.Zero(t, s.entryCount())
	})

	t.Run("kimi payg account not coding plan", func(t *testing.T) {
		s := NewKeepaliveService(keepaliveTestConfig(true), nil, nil, nil, nil)
		acc := &Account{ID: 1, Platform: PlatformKimi, Credentials: map[string]any{"account_mode": AccountModePayG}}
		s.Capture(ctx, acc, "https://api.kimi.com/v1/messages", http.Header{}, keepaliveBigBody(200))
		require.Zero(t, s.entryCount())
	})

	t.Run("body below token threshold", func(t *testing.T) {
		s := NewKeepaliveService(keepaliveTestConfig(true), nil, nil, nil, nil)
		s.Capture(ctx, keepaliveTestAccount(), "https://api.kimi.com/v1/messages", http.Header{}, []byte(`{"model":"kimi-k3"}`))
		require.Zero(t, s.entryCount())
	})

	t.Run("body over size cap", func(t *testing.T) {
		cfg := keepaliveTestConfig(true)
		cfg.Gateway.Keepalive.MaxBodyBytes = 100
		s := NewKeepaliveService(cfg, nil, nil, nil, nil)
		s.Capture(ctx, keepaliveTestAccount(), "https://api.kimi.com/v1/messages", http.Header{}, keepaliveBigBody(200))
		require.Zero(t, s.entryCount())
	})

	t.Run("missing capture info", func(t *testing.T) {
		s := NewKeepaliveService(keepaliveTestConfig(true), nil, nil, nil, nil)
		s.Capture(context.Background(), keepaliveTestAccount(), "https://api.kimi.com/v1/messages", http.Header{}, keepaliveBigBody(200))
		require.Zero(t, s.entryCount())
	})

	t.Run("capture succeeds and refreshes activity", func(t *testing.T) {
		s := NewKeepaliveService(keepaliveTestConfig(true), nil, nil, nil, nil)
		hdr := http.Header{"X-Api-Key": {"k"}, "Content-Length": {"123"}, "Connection": {"keep-alive"}}
		s.Capture(ctx, keepaliveTestAccount(), "https://api.kimi.com/v1/messages", hdr, keepaliveBigBody(200))
		require.Equal(t, 1, s.entryCount())
		key := keepaliveEntryKey(nil, "")
		_ = key
		s.mu.Lock()
		var e *keepaliveEntry
		for _, v := range s.entries {
			e = v
		}
		s.mu.Unlock()
		require.NotNil(t, e)
		require.Equal(t, int64(42), e.accountID)
		require.Equal(t, "kimi-k3", e.model)
		// 逐跳头已剥离，认证头保留
		require.Empty(t, e.header.Get("Content-Length"))
		require.Empty(t, e.header.Get("Connection"))
		require.Equal(t, "k", e.header.Get("X-Api-Key"))
		first := e.lastActivity
		time.Sleep(5 * time.Millisecond)
		s.Capture(ctx, keepaliveTestAccount(), "https://api.kimi.com/v1/messages", hdr, keepaliveBigBody(200))
		require.Equal(t, 1, s.entryCount(), "同会话重复捕获应覆盖而非新增")
		s.mu.Lock()
		for _, v := range s.entries {
			e = v
		}
		s.mu.Unlock()
		require.True(t, e.lastActivity.After(first), "重复捕获应刷新活动时间")
	})

	t.Run("evicts oldest beyond per-account cap", func(t *testing.T) {
		s := NewKeepaliveService(keepaliveTestConfig(true), nil, nil, nil, nil) // cap=2
		acc := keepaliveTestAccount()
		for i, sess := range []string{"s1", "s2", "s3"} {
			gid := int64(2)
			c := WithKeepaliveCaptureInfo(context.Background(), &KeepaliveCaptureInfo{SessionHash: sess, GroupID: &gid, APIKeyID: 11, UserID: 7})
			s.Capture(c, acc, "https://api.kimi.com/v1/messages", http.Header{}, keepaliveBigBody(200))
			// 错开 lastActivity，保证淘汰顺序确定
			s.mu.Lock()
			s.entries[keepaliveEntryKey(&gid, sess)].lastActivity = time.Now().Add(time.Duration(i) * time.Minute)
			s.mu.Unlock()
			_ = i
		}
		require.Equal(t, 2, s.entryCount())
		s.mu.Lock()
		_, hasS1 := s.entries[keepaliveEntryKey(func() *int64 { g := int64(2); return &g }(), "s1")]
		s.mu.Unlock()
		require.False(t, hasS1, "最老的 s1 应被淘汰")
	})
}

func TestBuildKeepaliveProbeBody(t *testing.T) {
	t.Run("messages shape keeps prefix inputs, strips output params", func(t *testing.T) {
		body := []byte(`{"model":"kimi-k3","stream":true,"stream_options":{"include_usage":true},"thinking":{"type":"enabled"},"store":true,"max_tokens":8192,"prompt_cache_key":"pck","messages":[{"role":"user","content":"hello"}],"tools":[{"name":"bash"}]}`)
		out := buildKeepaliveProbeBody(body, 16)
		require.False(t, gjson.GetBytes(out, "stream").Exists())
		require.False(t, gjson.GetBytes(out, "stream_options").Exists())
		require.False(t, gjson.GetBytes(out, "thinking").Exists())
		require.False(t, gjson.GetBytes(out, "store").Exists())
		require.Equal(t, int64(16), gjson.GetBytes(out, "max_tokens").Int())
		// 缓存键输入保持原样
		require.Equal(t, "pck", gjson.GetBytes(out, "prompt_cache_key").String())
		require.Equal(t, "hello", gjson.GetBytes(out, "messages.0.content").String())
		require.Equal(t, "bash", gjson.GetBytes(out, "tools.0.name").String())
	})

	t.Run("responses shape uses max_output_tokens", func(t *testing.T) {
		body := []byte(`{"model":"kimi-k3","max_output_tokens":4096,"max_tokens":100,"input":[{"role":"user","content":"hi"}]}`)
		out := buildKeepaliveProbeBody(body, 16)
		require.Equal(t, int64(16), gjson.GetBytes(out, "max_output_tokens").Int())
		require.False(t, gjson.GetBytes(out, "max_tokens").Exists())
		require.Equal(t, "hi", gjson.GetBytes(out, "input.0.content").String())
	})
}

func TestParseKeepaliveUsage(t *testing.T) {
	t.Run("anthropic shape converts to inclusive input", func(t *testing.T) {
		body := []byte(`{"usage":{"input_tokens":534,"output_tokens":12,"cache_read_input_tokens":858880,"cache_creation_input_tokens":0}}`)
		u := parseKeepaliveUsage(body)
		require.Equal(t, 534+858880, u.promptTotal)
		require.Equal(t, 858880, u.cacheRead)
		require.Equal(t, 12, u.completion)
	})

	t.Run("chat completions shape", func(t *testing.T) {
		body := []byte(`{"usage":{"prompt_tokens":470022,"completion_tokens":9,"prompt_tokens_details":{"cached_tokens":469504}}}`)
		u := parseKeepaliveUsage(body)
		require.Equal(t, 470022, u.promptTotal)
		require.Equal(t, 469504, u.cacheRead)
	})

	t.Run("responses shape", func(t *testing.T) {
		body := []byte(`{"usage":{"input_tokens":120000,"output_tokens":5,"input_tokens_details":{"cached_tokens":119000}}}`)
		u := parseKeepaliveUsage(body)
		require.Equal(t, 120000, u.promptTotal)
		require.Equal(t, 119000, u.cacheRead)
	})

	t.Run("missing usage", func(t *testing.T) {
		u := parseKeepaliveUsage([]byte(`{"id":"x"}`))
		require.Zero(t, u.promptTotal)
	})
}

func TestKeepaliveMissAndErrorLifecycle(t *testing.T) {
	s := NewKeepaliveService(keepaliveTestConfig(true), nil, nil, nil, nil)
	s.Capture(keepaliveTestCtx(11), keepaliveTestAccount(), "https://api.kimi.com/v1/messages", http.Header{}, keepaliveBigBody(200))
	require.Equal(t, 1, s.entryCount())

	s.mu.Lock()
	var e *keepaliveEntry
	for _, v := range s.entries {
		e = v
	}
	s.mu.Unlock()
	require.NotNil(t, e)

	// 连续 2 次 miss → 删除
	s.registerMiss(e)
	require.Equal(t, 1, s.entryCount())
	s.registerMiss(e)
	require.Zero(t, s.entryCount())

	// 重新捕获后连续 3 次错误 → 删除
	s.Capture(keepaliveTestCtx(11), keepaliveTestAccount(), "https://api.kimi.com/v1/messages", http.Header{}, keepaliveBigBody(200))
	s.registerError(e)
	s.registerError(e)
	require.Equal(t, 1, s.entryCount())
	s.registerError(e)
	require.Zero(t, s.entryCount())
}
