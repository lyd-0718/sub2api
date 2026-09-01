// Package trace 提供按会话（Session）留存完整请求/响应链路的能力。
//
// 设计目标：对上游 sub2api 代码零侵入——全部配置通过 TRACE_* 环境变量注入，
// 不修改官方 config 结构体；唯一接入点是路由层注册的一行中间件。
//
// 采集内容：请求体原文（含历史消息、工具定义、thinking 配置）、
// 重组后的完整响应（正文、thinking 块及签名、工具调用及参数）、
// 会话归属（Claude Code metadata.user_id 中的 session_id）、状态与耗时。
// 数据以 gzip 压缩的 JSON 文件按 日期/会话ID 归档落盘，供蒸馏导出使用。
package trace

import (
	"os"
	"strconv"
	"strings"
)

// Config 采集配置，全部来自环境变量，零配置文件改动。
type Config struct {
	Enabled      bool     // TRACE_ENABLED，默认 false
	Dir          string   // TRACE_DIR，默认 data/traces
	MaxBodyBytes int64    // TRACE_MAX_BODY_BYTES，单条请求/响应采集上限，默认 32MiB
	QueueSize    int      // TRACE_QUEUE_SIZE，异步落盘队列长度，默认 1024
	Workers      int      // TRACE_WORKERS，落盘协程数，默认 2
	SampleRate   float64  // TRACE_SAMPLE_RATE，采样率 0~1，默认 1
	APIKeyIDs    []int64  // TRACE_API_KEY_IDS，逗号分隔白名单；空 = 全部 key
	Paths        []string // TRACE_PATHS，采集路径，默认 /v1/messages,/messages
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt64(key string, def int64) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(key)), 10, 64)
	if err != nil || v <= 0 {
		return def
	}
	return v
}

func envFloat(key string, def float64) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(key)), 64)
	if err != nil || v < 0 || v > 1 {
		return def
	}
	return v
}

// LoadConfig 从环境变量加载配置。
func LoadConfig() *Config {
	cfg := &Config{
		Enabled:      strings.EqualFold(envOr("TRACE_ENABLED", ""), "true") || envOr("TRACE_ENABLED", "") == "1",
		Dir:          envOr("TRACE_DIR", "data/traces"),
		MaxBodyBytes: envInt64("TRACE_MAX_BODY_BYTES", 32<<20),
		QueueSize:    int(envInt64("TRACE_QUEUE_SIZE", 1024)),
		Workers:      int(envInt64("TRACE_WORKERS", 2)),
		SampleRate:   envFloat("TRACE_SAMPLE_RATE", 1.0),
		Paths:        []string{"/v1/messages", "/messages", "/v1/chat/completions", "/chat/completions"},
	}
	if v := strings.TrimSpace(os.Getenv("TRACE_PATHS")); v != "" {
		cfg.Paths = strings.Split(v, ",")
		for i := range cfg.Paths {
			cfg.Paths[i] = strings.TrimSpace(cfg.Paths[i])
		}
	}
	if v := strings.TrimSpace(os.Getenv("TRACE_API_KEY_IDS")); v != "" {
		for _, s := range strings.Split(v, ",") {
			if id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
				cfg.APIKeyIDs = append(cfg.APIKeyIDs, id)
			}
		}
	}
	return cfg
}

func (c *Config) pathAllowed(path string) bool {
	for _, p := range c.Paths {
		if p == path {
			return true
		}
	}
	return false
}

func (c *Config) apiKeyAllowed(id int64) bool {
	if len(c.APIKeyIDs) == 0 {
		return true
	}
	for _, k := range c.APIKeyIDs {
		if k == id {
			return true
		}
	}
	return false
}
