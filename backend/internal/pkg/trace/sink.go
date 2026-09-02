package trace

import (
	"compress/gzip"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// record 一轮请求的完整采集结果。
type record struct {
	Version      int             `json:"version"`
	SessionID    string          `json:"session_id"`
	RequestID    string          `json:"request_id"`
	StartedAt    time.Time       `json:"started_at"`
	DurationMs   int64           `json:"duration_ms"`
	APIKeyID     int64           `json:"api_key_id,omitempty"`
	UserID       int64           `json:"user_id,omitempty"`
	GroupID      int64           `json:"group_id,omitempty"`
	Model        string          `json:"model"`
	Stream       bool            `json:"stream"`
	HTTPStatus   int             `json:"http_status"`
	Request      json.RawMessage `json:"request"`
	RequestTrunc bool            `json:"request_truncated,omitempty"`
	Response     *Response       `json:"response"`
}

// sink 异步落盘：请求路径只负责入队，磁盘 I/O 全在后台协程。
// 队列满时丢弃并计数（fail-open），绝不阻塞或反压模型请求。
type sink struct {
	cfg     *Config
	queue   chan *record
	dropped atomic.Int64
	wg      sync.WaitGroup // 仅测试与关闭排空用
}

func newSink(cfg *Config) *sink {
	s := &sink{cfg: cfg, queue: make(chan *record, cfg.QueueSize)}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		log.Printf("[trace] create dir %s failed: %v (trace disabled)", cfg.Dir, err)
		return s
	}
	for i := 0; i < cfg.Workers; i++ {
		go s.work()
	}
	log.Printf("[trace] enabled: dir=%s workers=%d queue=%d sample=%.2f",
		cfg.Dir, cfg.Workers, cfg.QueueSize, cfg.SampleRate)
	return s
}

func (s *sink) enqueue(rec *record) {
	s.wg.Add(1)
	select {
	case s.queue <- rec:
	default:
		s.wg.Done()
		if n := s.dropped.Add(1); n%1000 == 1 {
			log.Printf("[trace] queue full, dropped %d records total", n)
		}
	}
}

func (s *sink) work() {
	for rec := range s.queue {
		if err := s.write(rec); err != nil {
			log.Printf("[trace] write failed session=%s request=%s: %v", rec.SessionID, rec.RequestID, err)
		}
		s.wg.Done()
	}
}

// write 落盘路径：dir/YYYYMMDD/<sessionID>/<timestamp>-<requestID>.json.gz
// 一个会话一个目录，一轮请求一个文件，gzip 压缩（多轮请求体携带
// 全量历史，重复前缀压缩率极高）。
func (s *sink) write(rec *record) error {
	rec.Version = 1
	dir := filepath.Join(s.cfg.Dir, rec.StartedAt.Format("20060102"), sanitizeName(rec.SessionID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	name := rec.StartedAt.Format("150405.000000") + "-" + sanitizeName(rec.RequestID) + ".json.gz"
	path := filepath.Join(dir, name)

	payload, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	if _, err := gz.Write(payload); err != nil {
		return err
	}
	return gz.Close()
}

// wait 阻塞直到已入队记录全部落盘（测试与优雅退出用）。
func (s *sink) wait() { s.wg.Wait() }

// sanitizeName 防止会话 ID/请求 ID 中的路径分隔符越界。
func sanitizeName(s string) string {
	if s == "" {
		return "unknown"
	}
	b := []byte(s)
	for i, ch := range b {
		ok := ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' ||
			ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.'
		if !ok {
			b[i] = '_'
		}
	}
	if len(b) > 128 {
		b = b[:128]
	}
	return string(b)
}
