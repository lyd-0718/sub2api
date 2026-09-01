package trace

import (
	"strings"
	"testing"
)

const sseStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-4-6","usage":{"input_tokens":1500,"cache_read_input_tokens":800}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"先分析"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"问题"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig123"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"ls -la\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"执行完成"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}

event: message_stop
data: {"type":"message_stop"}

`

func TestAssembleSSEFullChain(t *testing.T) {
	resp := assemble([]byte(sseStream), false)
	if !resp.Complete {
		t.Fatal("expected complete")
	}
	if resp.Model != "claude-opus-4-6" {
		t.Fatalf("model = %q", resp.Model)
	}
	if resp.StopReason != "end_turn" {
		t.Fatalf("stop_reason = %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 1500 || resp.Usage.OutputTokens != 42 || resp.Usage.CacheReadTokens != 800 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if len(resp.Blocks) != 3 {
		t.Fatalf("blocks = %d", len(resp.Blocks))
	}
	if resp.Blocks[0].Type != "thinking" || resp.Blocks[0].Thinking != "先分析问题" || resp.Blocks[0].Signature != "sig123" {
		t.Fatalf("thinking block = %+v", resp.Blocks[0])
	}
	tool := resp.Blocks[1]
	if tool.Type != "tool_use" || tool.ToolID != "toolu_1" || tool.ToolName != "Bash" {
		t.Fatalf("tool block = %+v", tool)
	}
	if string(tool.ToolInput) != `{"command":"ls -la"}` {
		t.Fatalf("tool input = %s", tool.ToolInput)
	}
	if resp.Blocks[2].Type != "text" || resp.Blocks[2].Text != "执行完成" {
		t.Fatalf("text block = %+v", resp.Blocks[2])
	}
}

func TestAssembleSSEErrorEvent(t *testing.T) {
	data := `event: error
data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}

`
	resp := assemble([]byte(data), false)
	if resp.Complete {
		t.Fatal("error response must not be complete")
	}
	if resp.Error != "Overloaded" {
		t.Fatalf("error = %q", resp.Error)
	}
}

func TestAssembleSSEEmptyKeepaliveDeltas(t *testing.T) {
	// sub2api 为 Claude Code 注入的空 delta keepalive 应合并为空串，不破坏重组
	data := `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"你好"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}

event: message_stop
data: {"type":"message_stop"}

`
	resp := assemble([]byte(data), false)
	if !resp.Complete || len(resp.Blocks) != 1 || resp.Blocks[0].Text != "你好" {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestAssembleNonStreamJSON(t *testing.T) {
	data := `{"id":"msg_1","type":"message","model":"claude-opus-4-6","stop_reason":"tool_use","content":[{"type":"text","text":"hi"},{"type":"tool_use","id":"t1","name":"Read","input":{"path":"/a"}}],"usage":{"input_tokens":10,"output_tokens":5}}`
	resp := assemble([]byte(data), false)
	if !resp.Complete {
		t.Fatal("non-stream message must be complete")
	}
	if len(resp.Blocks) != 2 || resp.Blocks[1].ToolName != "Read" {
		t.Fatalf("blocks = %+v", resp.Blocks)
	}
}

func TestAssembleTruncated(t *testing.T) {
	resp := assemble([]byte(sseStream), true)
	if resp.Complete {
		t.Fatal("truncated response must not be complete")
	}
	if !resp.Truncated {
		t.Fatal("truncated flag missing")
	}
}

func TestAssembleEmpty(t *testing.T) {
	resp := assemble(nil, false)
	if resp.Complete || len(resp.Blocks) != 0 {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestAssembleToolInputBroken(t *testing.T) {
	// 流在工具参数中途断开：参数 JSON 不完整时该轮不可用
	data := `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"Bash"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

`
	resp := assemble([]byte(data), false)
	if !strings.Contains(string(resp.Blocks[0].ToolInput), "_truncated") {
		t.Fatalf("tool input = %s", resp.Blocks[0].ToolInput)
	}
}
