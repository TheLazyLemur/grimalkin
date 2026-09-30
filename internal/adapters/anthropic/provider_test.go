package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"grimalkin/internal/core"
)

const toolUseSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":0}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Listing."}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"shell","input":{}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls\"}"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":1}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":9}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

type wireRequest struct {
	System   []struct{ Text string }
	Tools    []map[string]any
	Messages []struct {
		Role    string
		Content []map[string]any
	}
}

func TestAConversationIsEncodedForTheAPIAndAStreamedToolCallDecoded(t *testing.T) {
	// given
	// ... a server streaming a text block and a shell call, and a conversation
	// ... with a prompt, an earlier assistant turn and its tool result
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, toolUseSSE)
	}))
	defer server.Close()
	p := New(Config{Model: "m", MaxTokens: 100}, option.WithAPIKey("test-key"), option.WithBaseURL(server.URL))
	req := core.ModelRequest{System: "be brief", Tools: []core.ToolSpec{{Name: "shell", Description: "run a command", InputSchema: `{"type":"object","properties":{"command":{"type":"string"}},"required":["command"],"additionalProperties":false}`}}, Messages: []core.Message{
		{Role: core.RoleUser, Text: "list files"},
		{Role: core.RoleAssistant, Content: anthropic.NewAssistantMessage(anthropic.NewTextBlock("ok"))},
		{Role: core.RoleUser, ToolResults: []core.ToolResult{{ToolUseID: "toolu_0", Content: "a.txt"}}},
	}}
	var streamed strings.Builder

	// when
	// ... the request is streamed
	resp, err := p.Stream(context.Background(), req, func(s string) { streamed.WriteString(s) })

	// then
	// ... the body carries each message in API form, text streamed out, and the tool call came back
	if err != nil {
		t.Fatal(err)
	}
	var wire wireRequest
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("request body: %v\n%s", err, body)
	}
	if len(wire.Messages) != 3 {
		t.Fatalf("len(messages) = %d, want 3", len(wire.Messages))
	}
	if m := wire.Messages[1]; m.Role != "assistant" || m.Content[0]["text"] != "ok" {
		t.Errorf("assistant message = %+v", m)
	}
	if r := wire.Messages[2].Content[0]; r["type"] != "tool_result" || r["tool_use_id"] != "toolu_0" {
		t.Errorf("tool result = %+v", r)
	}
	tool := wire.Tools[0]
	schema, _ := tool["input_schema"].(map[string]any)
	if tool["name"] != "shell" || tool["strict"] != true || schema["additionalProperties"] != false {
		t.Errorf("tool = %+v", tool)
	}
	if wire.System[0].Text != "be brief" {
		t.Errorf("system = %+v", wire.System)
	}
	if streamed.String() != "Listing." {
		t.Errorf("streamed = %q", streamed.String())
	}
	if resp.StopReason != core.StopToolUse || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "shell" {
		t.Fatalf("response = %+v", resp)
	}
	var input struct{ Command string }
	if err := json.Unmarshal(resp.ToolCalls[0].Input, &input); err != nil || input.Command != "ls" {
		t.Errorf("tool input = %s", resp.ToolCalls[0].Input)
	}
	content, _ := json.Marshal(resp.Content)
	if !strings.Contains(string(content), `"toolu_1"`) {
		t.Errorf("content = %s", content)
	}
}
