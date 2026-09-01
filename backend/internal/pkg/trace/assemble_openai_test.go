package trace

import "testing"

const openaiSSE = `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"kimi-k3","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"kimi-k3","choices":[{"index":0,"delta":{"reasoning_content":"先想想"},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"kimi-k3","choices":[{"index":0,"delta":{"reasoning_content":"再决定"},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"kimi-k3","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"command\":"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"kimi-k3","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"kimi-k3","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":120,"completion_tokens":33}}

data: [DONE]

`

func TestAssembleOpenAISSE(t *testing.T) {
	resp := assemble([]byte(openaiSSE), false)
	if !resp.Complete {
		t.Fatal("expected complete after [DONE]")
	}
	if resp.Model != "kimi-k3" {
		t.Fatalf("model = %q", resp.Model)
	}
	if resp.StopReason != "tool_calls" {
		t.Fatalf("stop_reason = %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 120 || resp.Usage.OutputTokens != 33 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if len(resp.Blocks) != 2 {
		t.Fatalf("blocks = %d %+v", len(resp.Blocks), resp.Blocks)
	}
	if resp.Blocks[0].Type != "thinking" || resp.Blocks[0].Thinking != "先想想再决定" {
		t.Fatalf("thinking = %+v", resp.Blocks[0])
	}
	tool := resp.Blocks[1]
	if tool.Type != "tool_use" || tool.ToolID != "call_1" || tool.ToolName != "Bash" || string(tool.ToolInput) != `{"command":"ls"}` {
		t.Fatalf("tool = %+v", tool)
	}
}

func TestAssembleOpenAIJSON(t *testing.T) {
	data := `{"id":"chatcmpl-1","object":"chat.completion","model":"kimi-k3","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","reasoning_content":"嗯","content":"你好","tool_calls":null}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`
	resp := assemble([]byte(data), false)
	if !resp.Complete || resp.StopReason != "stop" {
		t.Fatalf("resp = %+v", resp)
	}
	if len(resp.Blocks) != 2 || resp.Blocks[0].Thinking != "嗯" || resp.Blocks[1].Text != "你好" {
		t.Fatalf("blocks = %+v", resp.Blocks)
	}
}

func TestAssembleOpenAIError(t *testing.T) {
	data := `{"error":{"message":"rate limit"}}`
	resp := assemble([]byte(data), false)
	if resp.Complete || resp.Error != "rate limit" {
		t.Fatalf("resp = %+v", resp)
	}
}
