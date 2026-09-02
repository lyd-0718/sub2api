package trace

import (
	"bytes"
	"encoding/json"
	"strings"
)

// OpenAI Responses API（Codex CLI 使用的协议）响应重组。
// 输出结构与 Anthropic/OpenAI 版本一致（Block: text/thinking/tool_use）：
//   - output item type=message       → text 块（output_text）
//   - output item type=reasoning     → thinking 块（reasoning summary）
//   - output item type=function_call → tool_use 块（arguments 分片重组）
//   - response.completed             → complete=true + usage + model
//
// 流式重组策略：response.completed 事件的 response 对象是全量权威数据，
// 收到就以它为准重建；流中断没等到 completed 时，退用 delta 累积的部分结果
// （保持 complete=false，导出时过滤）。

// responsesOutput 是 Responses 响应对象的输出项。
type responsesOutput struct {
	Type      string `json:"type"`
	Status    string `json:"status"`
	Name      string `json:"name"`      // function_call
	Arguments string `json:"arguments"` // function_call
	CallID    string `json:"call_id"`   // function_call
	Content   []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"` // message
	Summary []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"summary"` // reasoning
}

// blocksFromResponsesObject 从完整 response 对象重建 blocks/usage/model。
// 非流式响应与流式 response.completed 共用此函数。
func blocksFromResponsesObject(data []byte, resp *Response) {
	var obj struct {
		Object string            `json:"object"`
		Status string            `json:"status"`
		Model  string            `json:"model"`
		Output []responsesOutput `json:"output"`
		Usage  *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		resp.Error = "unparseable response body"
		return
	}
	if obj.Model != "" {
		resp.Model = obj.Model
	}
	if obj.Usage != nil {
		resp.Usage.InputTokens = obj.Usage.InputTokens
		resp.Usage.OutputTokens = obj.Usage.OutputTokens
	}
	if obj.Error != nil {
		resp.Error = obj.Error.Message
		return
	}
	if obj.Status != "" {
		resp.StopReason = obj.Status // completed / incomplete / failed
	}
	resp.Blocks = resp.Blocks[:0]
	for _, item := range obj.Output {
		appendResponsesItem(resp, &item)
	}
	if obj.IncompleteDetails != nil {
		resp.Complete = false
	}
}

// appendResponsesItem 把一个 output item 追加为标准块。
func appendResponsesItem(resp *Response, item *responsesOutput) {
	switch item.Type {
	case "message":
		var text strings.Builder
		for _, c := range item.Content {
			if c.Type == "output_text" {
				text.WriteString(c.Text)
			}
		}
		if text.Len() > 0 {
			resp.Blocks = append(resp.Blocks, Block{Type: "text", Text: text.String()})
		}
	case "reasoning":
		var thinking strings.Builder
		for _, s := range item.Summary {
			thinking.WriteString(s.Text)
		}
		if thinking.Len() > 0 {
			resp.Blocks = append(resp.Blocks, Block{Type: "thinking", Thinking: thinking.String()})
		}
	case "function_call":
		b := Block{Type: "tool_use", ToolID: item.CallID, ToolName: item.Name}
		if item.Arguments != "" {
			if json.Valid([]byte(item.Arguments)) {
				b.ToolInput = json.RawMessage(item.Arguments)
			} else {
				b.ToolInput = json.RawMessage(`{"_truncated":true}`)
			}
		}
		resp.Blocks = append(resp.Blocks, b)
	}
}

// assembleResponsesSSE 处理 Responses 流式事件。
// 优先以 response.completed 的全量对象为准；收不到时退化为 delta 累积。
func assembleResponsesSSE(data []byte, resp *Response) {
	type acc struct {
		typ      string
		callID   string
		name     string
		text     strings.Builder
		thinking strings.Builder
		args     strings.Builder
	}
	items := map[int]*acc{}
	order := []int{}
	completed := false

	getItem := func(idx int) *acc {
		it, ok := items[idx]
		if !ok {
			it = &acc{}
			items[idx] = it
			order = append(order, idx)
		}
		return it
	}

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
			Type        string           `json:"type"`
			OutputIndex int              `json:"output_index"`
			Delta       string           `json:"delta"`
			Item        *responsesOutput `json:"item"`
			Response    json.RawMessage  `json:"response"`
			Error       *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}

		switch event.Type {
		case "response.output_item.added":
			if event.Item != nil {
				it := getItem(event.OutputIndex)
				it.typ = event.Item.Type
				it.callID = event.Item.CallID
				it.name = event.Item.Name
			}

		case "response.output_text.delta":
			getItem(event.OutputIndex).text.WriteString(event.Delta)

		case "response.reasoning_summary_text.delta":
			getItem(event.OutputIndex).thinking.WriteString(event.Delta)

		case "response.function_call_arguments.delta":
			getItem(event.OutputIndex).args.WriteString(event.Delta)

		case "response.completed":
			completed = true
			if len(event.Response) > 0 {
				blocksFromResponsesObject(event.Response, resp)
			}
			resp.Complete = resp.Error == "" && resp.StopReason != "failed" && resp.StopReason != "incomplete"

		case "response.failed", "response.incomplete":
			if len(event.Response) > 0 {
				blocksFromResponsesObject(event.Response, resp)
			}
			resp.Complete = false

		case "error":
			if event.Error != nil {
				resp.Error = event.Error.Message
			}
			resp.Complete = false
		}
	}

	if completed {
		return // 已用权威对象重建
	}
	// 流中断：退用 delta 累积的部分结果（complete 保持 false）
	for _, idx := range order {
		it := items[idx]
		switch {
		case it.thinking.Len() > 0:
			resp.Blocks = append(resp.Blocks, Block{Type: "thinking", Thinking: it.thinking.String()})
		case it.args.Len() > 0 || it.typ == "function_call":
			b := Block{Type: "tool_use", ToolID: it.callID, ToolName: it.name}
			if args := it.args.String(); args != "" {
				if json.Valid([]byte(args)) {
					b.ToolInput = json.RawMessage(args)
				} else {
					b.ToolInput = json.RawMessage(`{"_truncated":true}`)
				}
			}
			resp.Blocks = append(resp.Blocks, b)
		case it.text.Len() > 0:
			resp.Blocks = append(resp.Blocks, Block{Type: "text", Text: it.text.String()})
		}
	}
}
