package trace

import (
	"bytes"
	"encoding/json"
	"strings"
)

// OpenAI chat.completions 格式的响应重组。
// 输出结构与 Anthropic 版本完全一致（Block: text/thinking/tool_use），
// 蒸馏导出器无需区分来源协议：
//   - choices[].delta.content          → text 块
//   - choices[].delta.reasoning_content → thinking 块（kimi/deepseek 等推理模型）
//   - choices[].delta.tool_calls        → tool_use 块（arguments 分片按 index 重组）
//   - finish_reason                     → StopReason
//   - usage(prompt/completion_tokens)   → Usage(Input/OutputTokens)

// assembleOpenAIJSON 处理非流式 chat.completion。
func assembleOpenAIJSON(data []byte, resp *Response) {
	var msg struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		resp.Error = "unparseable response body"
		return
	}
	resp.Model = msg.Model
	if msg.Error != nil {
		resp.Error = msg.Error.Message
		return
	}
	if msg.Usage != nil {
		resp.Usage.InputTokens = msg.Usage.PromptTokens
		resp.Usage.OutputTokens = msg.Usage.CompletionTokens
	}
	if len(msg.Choices) > 0 {
		ch := msg.Choices[0]
		resp.StopReason = ch.FinishReason
		appendOpenAIMessage(resp, ch.Message.Content, ch.Message.ReasoningContent, ch.Message.ToolCalls)
	}
	resp.Complete = msg.Object == "chat.completion"
}

// assembleOpenAISSE 处理 chat.completion.chunk 流。
func assembleOpenAISSE(data []byte, resp *Response) {
	var text, reasoning strings.Builder
	type toolAcc struct {
		id, name string
		args     strings.Builder
	}
	tools := map[int]*toolAcc{}
	toolOrder := []int{}

	for _, raw := range bytes.Split(data, []byte("\n")) {
		line := strings.TrimSpace(string(raw))
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			resp.Complete = true
			continue
		}
		var chunk struct {
			Model   string `json:"model"`
			Choices []struct {
				FinishReason string `json:"finish_reason"`
				Delta        struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			resp.Error = chunk.Error.Message
			resp.Complete = false
			continue
		}
		if chunk.Model != "" {
			resp.Model = chunk.Model
		}
		if chunk.Usage != nil {
			resp.Usage.InputTokens = chunk.Usage.PromptTokens
			resp.Usage.OutputTokens = chunk.Usage.CompletionTokens
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		ch := chunk.Choices[0]
		if ch.FinishReason != "" {
			resp.StopReason = ch.FinishReason
		}
		text.WriteString(ch.Delta.Content)
		reasoning.WriteString(ch.Delta.ReasoningContent)
		for _, tc := range ch.Delta.ToolCalls {
			acc, ok := tools[tc.Index]
			if !ok {
				acc = &toolAcc{}
				tools[tc.Index] = acc
				toolOrder = append(toolOrder, tc.Index)
			}
			if tc.ID != "" {
				acc.id = tc.ID
			}
			if tc.Function.Name != "" {
				acc.name = tc.Function.Name
			}
			acc.args.WriteString(tc.Function.Arguments)
		}
	}

	appendOpenAIMessage(resp, text.String(), reasoning.String(), nil)
	for _, idx := range toolOrder {
		acc := tools[idx]
		b := Block{Type: "tool_use", ToolID: acc.id, ToolName: acc.name}
		if args := acc.args.String(); args != "" {
			if json.Valid([]byte(args)) {
				b.ToolInput = json.RawMessage(args)
			} else {
				b.ToolInput = json.RawMessage(`{"_truncated":true}`)
				resp.Complete = false
			}
		}
		resp.Blocks = append(resp.Blocks, b)
	}
}

// appendOpenAIMessage 按 thinking → text 顺序产出块，与 Anthropic 块序一致。
func appendOpenAIMessage(resp *Response, content, reasoning string, toolCalls []struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}) {
	if reasoning != "" {
		resp.Blocks = append(resp.Blocks, Block{Type: "thinking", Thinking: reasoning})
	}
	if content != "" {
		resp.Blocks = append(resp.Blocks, Block{Type: "text", Text: content})
	}
	for _, tc := range toolCalls {
		b := Block{Type: "tool_use", ToolID: tc.ID, ToolName: tc.Function.Name}
		if tc.Function.Arguments != "" && json.Valid([]byte(tc.Function.Arguments)) {
			b.ToolInput = json.RawMessage(tc.Function.Arguments)
		}
		resp.Blocks = append(resp.Blocks, b)
	}
}
