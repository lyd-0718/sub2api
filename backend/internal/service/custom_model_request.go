package service

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ApplyCustomModelRequest expands exactly one custom model before provider
// dispatch. Unknown fields and structured client prompts are preserved.
// System prompt injection mode: prepend (before), append (after), replace (override).
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
				block, _ := json.Marshal(map[string]string{"type": "text", "text": prompt})
				request["system"], _ = json.Marshal(append([]json.RawMessage{block}, blocks...))
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
			message, _ := json.Marshal(map[string]string{"role": "system", "content": prompt})
			request["messages"], _ = json.Marshal(append([]json.RawMessage{message}, messages...))
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
		if err := applyCustomGeminiRequest(request, resolution.UpstreamModel, prompt); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("custom models are not supported on this endpoint")
	}
	return json.Marshal(request)
}

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

func applyCustomGeminiRequest(request map[string]json.RawMessage, model, prompt string) error {
	// countTokens can wrap a full GenerateContentRequest. Both its model and
	// system prompt must agree with the rewritten URL.
	for _, key := range []string{"generateContentRequest", "generate_content_request"} {
		if raw, ok := request[key]; ok {
			var nested map[string]json.RawMessage
			if err := json.Unmarshal(raw, &nested); err != nil || nested == nil {
				return fmt.Errorf("generateContentRequest must be an object")
			}
			nested["model"], _ = json.Marshal("models/" + strings.TrimPrefix(model, "models/"))
			if err := applyCustomGeminiRequest(nested, model, prompt); err != nil {
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
	instruction["parts"], _ = json.Marshal(append([]json.RawMessage{part}, parts...))
	delete(request, "system_instruction")
	request["systemInstruction"], _ = json.Marshal(instruction)
	return nil
}
