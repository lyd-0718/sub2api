package trace

import (
	"bytes"

	"github.com/gin-gonic/gin"
)

// teeWriter 包装 gin.ResponseWriter：写给客户端的字节原样通过，
// 同时复制一份到内存缓冲（超过 limit 后停止复制并标记截断）。
// Size()/Status() 等状态方法委托给底层 writer，
// 保证 handler 中基于 c.Writer.Size() 的 failover 判断不受影响。
type teeWriter struct {
	gin.ResponseWriter
	buf       bytes.Buffer
	limit     int64
	truncated bool
}

func newTeeWriter(w gin.ResponseWriter, limit int64) *teeWriter {
	return &teeWriter{ResponseWriter: w, limit: limit}
}

func (w *teeWriter) Write(p []byte) (int, error) {
	w.capture(p)
	return w.ResponseWriter.Write(p)
}

func (w *teeWriter) WriteString(s string) (int, error) {
	w.capture([]byte(s))
	return w.ResponseWriter.WriteString(s)
}

func (w *teeWriter) capture(p []byte) {
	if w.truncated {
		return
	}
	if remaining := w.limit - int64(w.buf.Len()); remaining < int64(len(p)) {
		if remaining > 0 {
			w.buf.Write(p[:remaining])
		}
		w.truncated = true
		return
	}
	w.buf.Write(p)
}

func (w *teeWriter) Bytes() []byte    { return w.buf.Bytes() }
func (w *teeWriter) Truncated() bool  { return w.truncated }
