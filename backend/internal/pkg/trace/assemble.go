package trace

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Block 响应中的一个语义内容块。
type Block struct {
	Type string `json:"type"` // text | thinking | tool_use

	// text 块
	Text string `json:"text,omitempty"`

	// thinking 块：保留 signature，蒸馏导出时可选择还原或转 <think> 标签
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	// tool_use 块：input 由 input_json_delta 分片重组为完整 JSON
	ToolID    string          `json:"tool_id,omitempty"`
	ToolName  string          `json:"tool_name,omitempty"`
	ToolInput json.RawMessage `json:"tool_input,omitempty"`
}

// Usage 响应中观测到的 token 用量（仅随 trace 记录，与计费系统无关）。
type Usage struct {
	InputTokens          int `json:"input_tokens,omitempty"`
	OutputTokens         int `json:"output_tokens,omitempty"`
	CacheCreationTokens  int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadTokens      int `json:"cache_read_input_tokens,omitempty"`
}

// Response 一次请求重组后的完整响应。
type Response struct {
	Model      string  `json:"model,omitempty"`
	StopReason string  `json:"stop_reason,omitempty"`
	Blocks     []Block `json:"blocks,omitempty"`
	Usage      Usage   `json:"usage,omitempty"`
	Error      string  `json:"error,omitempty"`
	// Complete: 流式收到 message_stop 或非流式拿到完整 JSON 且未截断。
	// 蒸馏导出只应采用 complete=true 的轮次。
	Complete bool `json:"complete"`
	// Truncated: 采集缓冲超限，响应不完整。
	Truncated bool `json:"truncated,omitempty"`
}

// assemble 把采集到的响应字节重组为结构化 Response。
// 自动识别非流式 JSON 与 Anthropic SSE 流；无法识别时保留空响应。
// 该函数是纯函数，便于测试。
func assemble(data []byte, truncated bool) *Response {
	resp := &Response{Truncated: truncated}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return resp
	}
		if trimmed[0] == '{' {
		// 非流式：OpenAI chat.completion 带 choices，Anthropic message 带 type
		if bytes.Contains(trimmed, []byte(`"choices"`)) {
			assembleOpenAIJSON(trimmed, resp)
		} else {
			assembleJSON(trimmed, resp)
		}
	} else if bytes.Contains(trimmed, []byte("\nevent:")) || bytes.HasPrefix(trimmed, []byte("event:")) {
		// Anthropic SSE 每事件带 event: 行；OpenAI SSE 只有 data: 行
		assembleSSE(trimmed, resp)
	} else {
		assembleOpenAISSE(trimmed, resp)
	}
	if truncated {
		resp.Complete = false
	}
	return resp
}

// assembleJSON 处理非流式响应：单个完整 message JSON。
func assembleJSON(data []byte, resp *Response) {
	var msg struct {
		Type       string `json:"type"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Thinking  string          `json:"thinking"`
			Signature string          `json:"signature"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
		} `json:"content"`
		Usage Usage `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		resp.Error = "unparseable response body"
		return
	}
	resp.Model = msg.Model
	resp.StopReason = msg.StopReason
	resp.Usage = msg.Usage
	if msg.Error != nil {
		resp.Error = msg.Error.Message
		return
	}
	for _, c := range msg.Content {
		resp.Blocks = append(resp.Blocks, Block{
			Type:      c.Type,
			Text:      c.Text,
			Thinking:  c.Thinking,
			Signature: c.Signature,
			ToolID:    c.ID,
			ToolName:  c.Name,
			ToolInput: c.Input,
		})
	}
	// 非流式完整 JSON 即完整响应；error 类型除外（上面已 return）。
	resp.Complete = msg.Type == "message"
}

// assembleSSE 处理 Anthropic SSE 流：按事件状态机重组内容块。
// 空 delta keepalive（sub2api 为 Claude Code 注入的占位 delta）自然合并为空串，
// error 事件记录后保持 complete=false。
func assembleSSE(data []byte, resp *Response) {
	type openBlock struct {
		idx    int
		partial strings.Builder // tool_use 的 input_json_delta 累积
	}
	blocks := map[int]*Block{}
	order := []int{}
	partials := map[int]*strings.Builder{}

	for _, raw := range bytes.Split(data, []byte("\n")) {
		line := strings.TrimSpace(string(raw))
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				Signature   string `json:"signature"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			ContentBlock struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				Name      string `json:"name"`
				Text      string `json:"text"`
				Thinking  string `json:"thinking"`
				Signature string `json:"signature"`
			} `json:"content_block"`
			Message struct {
				Model string `json:"model"`
				Usage Usage  `json:"usage"`
			} `json:"message"`
			Usage *Usage `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}

		switch event.Type {
		case "message_start":
			resp.Model = event.Message.Model
			mergeUsage(resp, &event.Message.Usage)

		case "content_block_start":
			b := &Block{
				Type:      event.ContentBlock.Type,
				Text:      event.ContentBlock.Text,
				Thinking:  event.ContentBlock.Thinking,
				Signature: event.ContentBlock.Signature,
				ToolID:    event.ContentBlock.ID,
				ToolName:  event.ContentBlock.Name,
			}
			blocks[event.Index] = b
			order = append(order, event.Index)
			partials[event.Index] = &strings.Builder{}

		case "content_block_delta":
			b, ok := blocks[event.Index]
			if !ok {
				continue
			}
			switch event.Delta.Type {
			case "text_delta":
				b.Text += event.Delta.Text
			case "thinking_delta":
				b.Thinking += event.Delta.Thinking
			case "signature_delta":
				b.Signature += event.Delta.Signature
			case "input_json_delta":
				if sb, ok := partials[event.Index]; ok {
					sb.WriteString(event.Delta.PartialJSON)
				}
			}

		case "content_block_stop":
			if sb, ok := partials[event.Index]; ok {
				if b := blocks[event.Index]; b != nil && b.Type == "tool_use" && sb.Len() > 0 {
					input := []byte(sb.String())
					if json.Valid(input) {
						b.ToolInput = json.RawMessage(input)
					} else {
						// 参数 JSON 不完整（流中断），标记不完整
						b.ToolInput = json.RawMessage(`{"_truncated":true}`)
						resp.Complete = false
					}
				}
			}

		case "message_delta":
			if event.Delta.StopReason != "" {
				resp.StopReason = event.Delta.StopReason
			}
			if event.Usage != nil {
				mergeUsage(resp, event.Usage)
			}

		case "message_stop":
			resp.Complete = true

		case "error":
			if event.Error != nil {
				resp.Error = event.Error.Message
			}
			resp.Complete = false
		}
	}

	for _, idx := range order {
		resp.Blocks = append(resp.Blocks, *blocks[idx])
	}
}

// mergeUsage 合并非零用量字段（message_start 与 message_delta 各报一部分）。
func mergeUsage(resp *Response, u *Usage) {
	if u.InputTokens > 0 {
		resp.Usage.InputTokens = u.InputTokens
	}
	if u.OutputTokens > 0 {
		resp.Usage.OutputTokens = u.OutputTokens
	}
	if u.CacheCreationTokens > 0 {
		resp.Usage.CacheCreationTokens = u.CacheCreationTokens
	}
	if u.CacheReadTokens > 0 {
		resp.Usage.CacheReadTokens = u.CacheReadTokens
	}
}
