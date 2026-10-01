package service

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ApplyCustomModelRequest expands exactly one custom model before provider
// dispatch. Unknown fields and structured client prompts are preserved.
//
// System prompt injection mode, relative to the client's own system prompt:
//
//	prepend — before the client prompt (default)
//	append  — after the client prompt
//	replace — override the client prompt entirely
func ApplyCustomModelRequest(body []byte, resolution *CustomModelResolution, protocol string) ([]byte, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil || request == nil {
		return nil, fmt.Errorf("custom models require a JSON object request")
	}
	if protocol != "gemini" {
		// Canonicalize the model field so downstream decoders cannot disagree on
		// duplicate keys or case variants after the authorized model is expanded.
		for key := range request {
			if strings.EqualFold(key, "model") {
				delete(request, key)
			}
		}
		request["model"], _ = json.Marshal(resolution.UpstreamModel)
	}
	prompt := resolution.SystemPrompt
	mode := resolution.InjectionMode
	if mode == "" {
		mode = "prepend"
	}
	switch protocol {
	case "messages":
		if prompt != "" {
			var text string
			raw := request["system"]
			if len(raw) == 0 || string(raw) == "null" {
				request["system"], _ = json.Marshal(prompt)
			} else if json.Unmarshal(raw, &text) == nil {
				request["system"], _ = json.Marshal(applyCustomPrompt(prompt, text, mode))
			} else {
				var blocks []json.RawMessage
				if err := json.Unmarshal(raw, &blocks); err != nil {
					return nil, fmt.Errorf("system must be a string or content block array")
				}
				request["system"], _ = json.Marshal(applyPromptToBlocks(blocks, prompt, mode))
			}
		}
	case "chat":
		if prompt != "" {
			var messages []json.RawMessage
			if raw := request["messages"]; len(raw) != 0 {
				if err := json.Unmarshal(raw, &messages); err != nil {
					return nil, fmt.Errorf("messages must be an array")
				}
			}
			if index := firstChatSystemMessageIndex(messages); index >= 0 {
				merged, err := mergeChatSystemMessage(messages[index], prompt, mode)
				if err != nil {
					return nil, err
				}
				messages[index] = merged
			} else {
				message, _ := json.Marshal(map[string]string{"role": "system", "content": prompt})
				messages = append([]json.RawMessage{message}, messages...)
			}
			request["messages"], _ = json.Marshal(messages)
		}
	case "responses":
		if prompt != "" {
			var instructions string
			if raw := request["instructions"]; len(raw) != 0 && string(raw) != "null" {
				if err := json.Unmarshal(raw, &instructions); err != nil {
					return nil, fmt.Errorf("instructions must be a string")
				}
			}
			request["instructions"], _ = json.Marshal(applyCustomPrompt(prompt, instructions, mode))
		}
	case "gemini":
		if err := applyCustomGeminiRequest(request, resolution.UpstreamModel, prompt, mode); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("custom models are not supported on this endpoint")
	}
	return json.Marshal(request)
}

// applyCustomPrompt 按注入模式合并两段文本提示词。
func applyCustomPrompt(prompt, original, mode string) string {
	switch mode {
	case "append":
		if original == "" {
			return prompt
		}
		return original + "\n\n" + prompt
	case "replace":
		return prompt
	default: // "prepend"
		if original == "" {
			return prompt
		}
		return prompt + "\n\n" + original
	}
}

// applyPromptToBlocks 按注入模式把提示词块并入 Anthropic 风格的 content block 数组。
func applyPromptToBlocks(blocks []json.RawMessage, prompt, mode string) []json.RawMessage {
	block, _ := json.Marshal(map[string]string{"type": "text", "text": prompt})
	switch mode {
	case "append":
		return append(blocks, block)
	case "replace":
		return []json.RawMessage{block}
	default: // "prepend"
		return append([]json.RawMessage{block}, blocks...)
	}
}

// firstChatSystemMessageIndex 返回第一条 role=system 消息的下标（-1 表示没有）。
// OpenAI chat 语义下 system 消息允许出现在任意位置，注入按第一条合并。
func firstChatSystemMessageIndex(messages []json.RawMessage) int {
	for i, raw := range messages {
		var probe struct {
			Role string `json:"role"`
		}
		if json.Unmarshal(raw, &probe) == nil && strings.EqualFold(probe.Role, "system") {
			return i
		}
	}
	return -1
}

// mergeChatSystemMessage 按模式把注入提示词并入一条 chat system 消息的 content。
func mergeChatSystemMessage(message json.RawMessage, prompt, mode string) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(message, &obj); err != nil {
		return nil, fmt.Errorf("system message must be a JSON object")
	}
	var text string
	raw := obj["content"]
	if len(raw) == 0 || string(raw) == "null" {
		obj["content"], _ = json.Marshal(prompt)
	} else if json.Unmarshal(raw, &text) == nil {
		obj["content"], _ = json.Marshal(applyCustomPrompt(prompt, text, mode))
	} else {
		var blocks []json.RawMessage
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return nil, fmt.Errorf("system message content must be a string or content block array")
		}
		obj["content"], _ = json.Marshal(applyPromptToBlocks(blocks, prompt, mode))
	}
	return json.Marshal(obj)
}

func applyCustomGeminiRequest(request map[string]json.RawMessage, model, prompt, mode string) error {
	// countTokens can wrap a full GenerateContentRequest. Both its model and
	// system prompt must agree with the rewritten URL.
	for _, key := range []string{"generateContentRequest", "generate_content_request"} {
		if raw, ok := request[key]; ok {
			var nested map[string]json.RawMessage
			if err := json.Unmarshal(raw, &nested); err != nil || nested == nil {
				return fmt.Errorf("generateContentRequest must be an object")
			}
			nested["model"], _ = json.Marshal("models/" + strings.TrimPrefix(model, "models/"))
			if err := applyCustomGeminiRequest(nested, model, prompt, mode); err != nil {
				return err
			}
			delete(request, "generate_content_request")
			request["generateContentRequest"], _ = json.Marshal(nested)
			return nil
		}
	}
	if prompt == "" {
		return nil
	}
	var instruction map[string]json.RawMessage
	raw := request["systemInstruction"]
	if len(raw) == 0 {
		raw = request["system_instruction"]
	}
	if len(raw) != 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &instruction); err != nil {
			return fmt.Errorf("systemInstruction must be an object")
		}
	}
	if instruction == nil {
		instruction = make(map[string]json.RawMessage)
	}
	var parts []json.RawMessage
	if raw := instruction["parts"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &parts); err != nil {
			return fmt.Errorf("systemInstruction.parts must be an array")
		}
	}
	part, _ := json.Marshal(map[string]string{"text": prompt})
	switch mode {
	case "append":
		parts = append(parts, part)
	case "replace":
		parts = []json.RawMessage{part}
	default: // "prepend"
		parts = append([]json.RawMessage{part}, parts...)
	}
	instruction["parts"], _ = json.Marshal(parts)
	delete(request, "system_instruction")
	request["systemInstruction"], _ = json.Marshal(instruction)
	return nil
}
