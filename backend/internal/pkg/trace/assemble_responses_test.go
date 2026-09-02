package trace

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

const responsesSSE = `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress","model":"gpt-5-codex"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[]}}

event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"先分析"}

event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"再动手"}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","name":"shell","call_id":"call_9","arguments":""}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"command\":"}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"\"ls -la\"}"}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":2,"item":{"id":"msg_1","type":"message","status":"in_progress","content":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":2,"delta":"列出来了"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"gpt-5-codex","output":[{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"先分析再动手"}]},{"id":"fc_1","type":"function_call","name":"shell","call_id":"call_9","arguments":"{\"command\":\"ls -la\"}"},{"id":"msg_1","type":"message","status":"completed","content":[{"type":"output_text","text":"列出来了"}]}],"usage":{"input_tokens":320,"completion_tokens":0,"output_tokens":64}}}

`

func TestAssembleResponsesSSE(t *testing.T) {
	resp := assemble([]byte(responsesSSE), false)
	if !resp.Complete {
		t.Fatal("expected complete after response.completed")
	}
	if resp.Model != "gpt-5-codex" {
		t.Fatalf("model = %q", resp.Model)
	}
	if resp.StopReason != "completed" {
		t.Fatalf("stop_reason = %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 320 || resp.Usage.OutputTokens != 64 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if len(resp.Blocks) != 3 {
		t.Fatalf("blocks = %d %+v", len(resp.Blocks), resp.Blocks)
	}
	if resp.Blocks[0].Type != "thinking" || resp.Blocks[0].Thinking != "先分析再动手" {
		t.Fatalf("thinking = %+v", resp.Blocks[0])
	}
	tool := resp.Blocks[1]
	if tool.Type != "tool_use" || tool.ToolID != "call_9" || tool.ToolName != "shell" || string(tool.ToolInput) != `{"command":"ls -la"}` {
		t.Fatalf("tool = %+v", tool)
	}
	if resp.Blocks[2].Type != "text" || resp.Blocks[2].Text != "列出来了" {
		t.Fatalf("text = %+v", resp.Blocks[2])
	}
}

func TestAssembleResponsesBrokenStream(t *testing.T) {
	// 流在 arguments 中途断开，没有 response.completed：退用 delta 累积且 incomplete
	cut := strings.Split(responsesSSE, "event: response.completed")[0]
	resp := assemble([]byte(cut), false)
	if resp.Complete {
		t.Fatal("broken stream must not be complete")
	}
	if len(resp.Blocks) != 3 {
		t.Fatalf("blocks = %d %+v", len(resp.Blocks), resp.Blocks)
	}
	if string(resp.Blocks[1].ToolInput) != `{"command":"ls -la"}` {
		t.Fatalf("tool input = %s", resp.Blocks[1].ToolInput)
	}
}

func TestAssembleResponsesNonStream(t *testing.T) {
	data := `{"id":"resp_1","object":"response","status":"completed","model":"gpt-5-codex","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":10,"output_tokens":3}}`
	resp := assemble([]byte(data), false)
	if !resp.Complete || resp.StopReason != "completed" || len(resp.Blocks) != 1 || resp.Blocks[0].Text != "done" {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestSniffThreeFormats(t *testing.T) {
	if sniffSSEFormat([]byte(sseStream)) != "anthropic" {
		t.Fatal("anthropic misdetected")
	}
	if sniffSSEFormat([]byte(openaiSSE)) != "openai" {
		t.Fatal("openai misdetected")
	}
	if sniffSSEFormat([]byte(responsesSSE)) != "responses" {
		t.Fatal("responses misdetected")
	}
}

func TestResolveSessionIDCodexHeaders(t *testing.T) {
	c := &gin.Context{Request: httptest.NewRequest(http.MethodPost, "/v1/responses", nil)}
	c.Request.Header.Set("session_id", "abc-session")
	if got := resolveSessionID(c, []byte(`{"input":"hi"}`), 1); got != "codex-abc-session" {
		t.Fatalf("session_id header = %q", got)
	}
	c2 := &gin.Context{Request: httptest.NewRequest(http.MethodPost, "/v1/responses", nil)}
	c2.Request.Header.Set("conversation_id", "conv-1")
	if got := resolveSessionID(c2, []byte(`{"input":"hi"}`), 1); got != "codex-conv-conv-1" {
		t.Fatalf("conversation_id header = %q", got)
	}
}
