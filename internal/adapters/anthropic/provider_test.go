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

	"grimalkin/internal/core"
)

const toolUseSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":0}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Use ls."}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Listing."}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":1}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"shell","input":{}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls\"}"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":2}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":9}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

type wireRequest struct {
	System []struct {
		Text         string
		CacheControl map[string]any `json:"cache_control"`
	}
	Tools        []map[string]any
	Thinking     map[string]any `json:"thinking"`
	OutputConfig map[string]any `json:"output_config"`
	CacheControl map[string]any `json:"cache_control"`
	Messages     []struct {
		Role    string
		Content []map[string]any
	}
}

// serve streams sse to every request and records the last request's body and
// headers.
func serve(t *testing.T, sse string) (url string, body *[]byte, header *http.Header) {
	t.Helper()
	body, header = new([]byte), new(http.Header)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*body, _ = io.ReadAll(r.Body)
		*header = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	t.Cleanup(server.Close)
	return server.URL, body, header
}

func TestAConversationIsEncodedForTheAPIAndAStreamedToolCallDecoded(t *testing.T) {
	// given
	// ... a server streaming an unsigned thinking block, a text block and a shell
	// ... call; a conversation with a prompt, an earlier assistant turn and its
	// ... tool result; and an auth token in the environment the config doesn't name
	url, bodyRef, header := serve(t, toolUseSSE)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "leaked-token")
	p := New(Config{BaseURL: url, APIKey: "test-key", Model: "m", MaxTokens: 20_000, Thinking: "enabled", Effort: "high"})
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
	// ... the body carries the configured thinking and effort and each message in
	// ... API form, tools and system are cached apart from the conversation,
	// ... only the configured key is sent, text streamed out, and the tool call
	// ... and unsigned thinking came back ready to be sent again
	if err != nil {
		t.Fatal(err)
	}
	body := *bodyRef
	if got := header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want none", got)
	}
	if got := header.Get("X-Api-Key"); got != "test-key" {
		t.Errorf("X-Api-Key = %q, want test-key", got)
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
	if wire.Thinking["type"] != "enabled" || wire.Thinking["budget_tokens"] == nil {
		t.Errorf("thinking = %+v", wire.Thinking)
	}
	if wire.OutputConfig["effort"] != "high" {
		t.Errorf("output_config = %+v", wire.OutputConfig)
	}
	if wire.System[0].Text != "be brief" || wire.System[0].CacheControl["type"] != "ephemeral" {
		t.Errorf("system = %+v", wire.System)
	}
	if wire.CacheControl["type"] != "ephemeral" {
		t.Errorf("cache_control = %+v", wire.CacheControl)
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
	if !strings.Contains(string(content), `"toolu_1"`) ||
		!strings.Contains(string(content), `{"signature":"","thinking":"Use ls.","type":"thinking"}`) {
		t.Errorf("content = %s", content)
	}
}

func TestAdaptiveThinkingWithNoEffortSendsNoOutputConfig(t *testing.T) {
	// given
	// ... a provider configured for adaptive thinking and a model with no effort levels
	url, bodyRef, _ := serve(t, toolUseSSE)
	p := New(Config{BaseURL: url, APIKey: "test-key", Model: "m", MaxTokens: 100, Thinking: "adaptive"})
	req := core.ModelRequest{Messages: []core.Message{{Role: core.RoleUser, Text: "hi"}}}

	// when
	// ... the request is streamed
	_, err := p.Stream(context.Background(), req, func(string) {})

	// then
	// ... thinking is adaptive and no output_config is sent
	if err != nil {
		t.Fatal(err)
	}
	var wire wireRequest
	if err := json.Unmarshal(*bodyRef, &wire); err != nil {
		t.Fatalf("request body: %v\n%s", err, *bodyRef)
	}
	if wire.Thinking["type"] != "adaptive" {
		t.Errorf("thinking = %+v", wire.Thinking)
	}
	if wire.OutputConfig != nil {
		t.Errorf("output_config = %+v, want none", wire.OutputConfig)
	}
}
