package acp_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	sdk "github.com/coder/acp-go-sdk"

	"grimalkin/internal/core"
)

var hearth = sdk.McpServer{Acp: &sdk.McpServerAcpInline{Id: "srv1", Name: "hearth", Type: "acp"}}

var greetTool = map[string]any{
	"name":        "greet",
	"description": "Greets someone.",
	"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"who": map[string]any{"type": "string"}}},
}

// mcpServer answers an MCP server's requests: it lists tools, and answers a
// tools/call with whatever call returns.
func mcpServer(tools []map[string]any, call func(name string, args map[string]any) any) func(string, string, map[string]any) (any, error) {
	return func(_, method string, params map[string]any) (any, error) {
		switch method {
		case "initialize":
			return map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "hearth", "version": "1"}}, nil
		case "tools/list":
			return map[string]any{"tools": tools}, nil
		case "tools/call":
			return call(params["name"].(string), params["arguments"].(map[string]any)), nil
		}
		return nil, errors.New("unexpected MCP method " + method)
	}
}

func textResult(text string, isError bool) any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": isError}
}

func (s *session) newWith(servers ...sdk.McpServer) (sdk.SessionId, error) {
	servers = append([]sdk.McpServer{}, servers...) // mcpServers is required: [] on the wire, not null
	resp, err := s.client.NewSession(context.Background(), sdk.NewSessionRequest{Cwd: "/work", McpServers: servers})
	return resp.SessionId, err
}

func toolNames(req core.ModelRequest) []string {
	var names []string
	for _, tool := range req.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func TestSessionNewConnectsTheClientsServerAndOffersItsToolsBesideTheShell(t *testing.T) {
	// given
	// ... a client with an ACP MCP server listing one tool, and an agent with the shell
	s := newToolSession(t, []core.Tool{&fakeTool{name: "shell"}}, modelReply{chunks: []string{"ok"}})
	s.got.mcp = mcpServer([]map[string]any{greetTool}, nil)
	s.initialize(t)

	// when
	// ... the client creates a session naming the server, then prompts
	id, err := s.newWith(hearth)
	_, promptErr := s.client.Prompt(context.Background(), promptOf(id, "hi"))

	// then
	// ... the agent connected to the server by its id, initialized MCP and listed its tools before answering, and the model was offered both tools
	if err != nil || promptErr != nil {
		t.Fatalf("session/new: %v; prompt: %v", err, promptErr)
	}
	if !slices.Equal(s.got.connects, []string{"srv1"}) {
		t.Errorf("mcp/connect for %q, want [srv1]", s.got.connects)
	}
	if got, want := s.got.mcpMethods(), []string{"initialize", "notifications/initialized", "tools/list"}; !slices.Equal(got, want) {
		t.Errorf("mcp/message methods = %q, want %q", got, want)
	}
	if got := s.got.messages[0].connectionID; got != "conn-srv1" {
		t.Errorf("mcp/message connectionId = %q, want the one mcp/connect returned", got)
	}
	req := s.model.sent()[0]
	if got := toolNames(req); !slices.Equal(got, []string{"shell", "greet"}) {
		t.Errorf("tools offered = %q, want shell and greet", got)
	}
	if spec := req.Tools[1]; spec.Description != "Greets someone." || !strings.Contains(spec.InputSchema, `"who"`) || !spec.NonStrict {
		t.Errorf("greet spec = %+v, want the listed description and schema, non-strict", spec)
	}
}

func TestClientToolCallAsksPermissionThenGoesOutAsAToolsCall(t *testing.T) {
	// given
	// ... a session with the client's greet tool, whose call the client answers with text, and a client that allows
	s := newToolSession(t, nil,
		modelReply{calls: []core.ToolCall{{ID: "t1", Name: "greet", Input: []byte(`{"who":"Ada"}`)}}},
		modelReply{chunks: []string{"done"}})
	s.got.mcp = mcpServer([]map[string]any{greetTool}, func(string, map[string]any) any {
		return map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "hello"},
			map[string]any{"type": "image", "data": "x", "mimeType": "image/png"},
			map[string]any{"type": "text", "text": "Ada"}}}
	})
	allow := choosing(sdk.PermissionOptionKindAllowOnce)
	var callsWhenAsked []string
	s.got.answerWith(func(ctx context.Context, req sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
		callsWhenAsked = s.got.mcpMethods()
		return allow(ctx, req)
	})
	s.initialize(t)
	id, err := s.newWith(hearth)
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}

	// when
	// ... the client prompts and the model calls the tool
	_, err = s.client.Prompt(context.Background(), promptOf(id, "greet Ada"))

	// then
	// ... permission was asked first, as kind other with the tool's name, then a tools/call went out with the arguments, and the model got the text blocks joined
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if slices.Contains(callsWhenAsked, "tools/call") {
		t.Errorf("tools/call was sent before permission was asked: %q", callsWhenAsked)
	}
	req := s.got.requests[0].ToolCall
	if *req.Kind != sdk.ToolKindOther || !reflect.DeepEqual(req.Meta, map[string]any{"toolName": "greet"}) || *req.Title != `greet {"who":"Ada"}` {
		t.Errorf("permission request = %+v, want kind other, toolName greet, and the title", req)
	}
	call := s.got.messages[len(s.got.messages)-1]
	if call.method != "tools/call" || !reflect.DeepEqual(call.params, map[string]any{"name": "greet", "arguments": map[string]any{"who": "Ada"}}) {
		t.Errorf("last mcp/message = %+v, want a tools/call of greet with its arguments", call)
	}
	if got := lastResults(s.model); !reflect.DeepEqual(got, []core.ToolResult{{ToolUseID: "t1", Content: "hello\nAda"}}) {
		t.Errorf("model got results %+v, want the joined text", got)
	}
}

func TestClientToolResultMarkedIsErrorReachesTheModelAsAnError(t *testing.T) {
	// given
	// ... a session with the client's greet tool, whose call the client answers with isError
	s := newToolSession(t, nil,
		modelReply{calls: []core.ToolCall{{ID: "t1", Name: "greet", Input: []byte(`{}`)}}},
		modelReply{chunks: []string{"done"}})
	s.got.mcp = mcpServer([]map[string]any{greetTool}, func(string, map[string]any) any { return textResult("no such person", true) })
	s.got.answerWith(choosing(sdk.PermissionOptionKindAllowOnce))
	s.initialize(t)
	id, err := s.newWith(hearth)
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}

	// when
	// ... the client prompts and the model calls the tool
	_, err = s.client.Prompt(context.Background(), promptOf(id, "greet"))

	// then
	// ... the model got the text as an error result
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if got := lastResults(s.model); !reflect.DeepEqual(got, []core.ToolResult{{ToolUseID: "t1", Content: "no such person", IsError: true}}) {
		t.Errorf("model got results %+v, want the text as an error", got)
	}
}

func TestSessionLoadReconnectsTheClientsServerAndListsItsToolsAgain(t *testing.T) {
	// given
	// ... a saved session, and a new process with a client offering the same server
	first := newSession(t, modelReply{chunks: []string{"first"}})
	first.initialize(t)
	id, err := first.newWith()
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}
	if _, err := first.client.Prompt(context.Background(), promptOf(id, "one")); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	second := connect(t, modelReply{chunks: []string{"second"}})
	second.got.mcp = mcpServer([]map[string]any{greetTool}, nil)
	second.initialize(t)

	// when
	// ... the new process loads the session naming the server, and the client prompts
	_, loadErr := second.client.LoadSession(context.Background(), sdk.LoadSessionRequest{SessionId: id, Cwd: "/work", McpServers: []sdk.McpServer{hearth}})
	_, promptErr := second.client.Prompt(context.Background(), promptOf(id, "two"))

	// then
	// ... it connected and listed again, and the model was offered the tool
	if loadErr != nil || promptErr != nil {
		t.Fatalf("load: %v; prompt: %v", loadErr, promptErr)
	}
	if !slices.Equal(second.got.connects, []string{"srv1"}) {
		t.Errorf("mcp/connect for %q, want [srv1]", second.got.connects)
	}
	if got := toolNames(second.model.sent()[0]); !slices.Equal(got, []string{"greet"}) {
		t.Errorf("tools offered = %q, want greet", got)
	}
}

func TestClientToolNamedLikeAnAgentToolFailsTheSessionStart(t *testing.T) {
	// given
	// ... an agent with the shell, and a client whose server lists a tool named shell
	s := newToolSession(t, []core.Tool{&fakeTool{name: "shell"}})
	s.got.mcp = mcpServer([]map[string]any{{"name": "shell", "inputSchema": map[string]any{"type": "object"}}}, nil)
	s.initialize(t)

	// when
	// ... the client creates a session naming the server
	_, err := s.newWith(hearth)

	// then
	// ... session/new fails naming the tool
	if err == nil || !strings.Contains(err.Error(), `tool \"shell\" clashes`) {
		t.Errorf("err = %v, want one naming the clash with shell", err)
	}
}

func TestFailedMcpConnectFailsTheSessionStartWithTheError(t *testing.T) {
	// given
	// ... a client whose mcp/connect fails
	s := newSession(t)
	s.got.connect = func(string) (string, error) { return "", errors.New("no such server srv1") }
	s.initialize(t)

	// when
	// ... the client creates a session naming the server, then again with none
	_, err := s.newWith(hearth)
	_, retryErr := s.newWith()

	// then
	// ... session/new fails with the client's error, and left no session behind
	if err == nil || !strings.Contains(err.Error(), "no such server srv1") {
		t.Errorf("err = %v, want one carrying the client's error", err)
	}
	if retryErr != nil {
		t.Errorf("session/new after the failure: %v, want it to succeed", retryErr)
	}
}

func TestMcpServerOfAnotherTransportIsIgnored(t *testing.T) {
	// given
	// ... a client naming only a stdio server
	s := newSession(t)
	s.initialize(t)
	stdio := sdk.McpServer{Stdio: &sdk.McpServerStdio{Name: "fs", Command: "fs-server", Args: []string{}, Env: []sdk.EnvVariable{}}}

	// when
	// ... the client creates a session
	_, err := s.newWith(stdio)

	// then
	// ... it succeeds without connecting anything
	if err != nil || len(s.got.connects) != 0 {
		t.Errorf("session/new = %v, connects = %q; want success and no connect", err, s.got.connects)
	}
}
