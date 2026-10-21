package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"

	sdk "github.com/coder/acp-go-sdk"

	"grimalkin/internal/core"
)

// The client's tools come from MCP servers the client provides over the ACP
// connection itself (the draft acp-go-sdk v0.13.5 implements): mcp/connect
// names a server by the id from mcpServers and returns a connection id, and
// MCP's own JSON-RPC then travels inside mcp/message requests. The SDK has no
// agent-side helper for mcp/message, so it is sent with sdk.SendRequest on
// the Server's own Connection.

const mcpProtocolVersion = "2025-06-18"

// mcpNamePattern is what the model API accepts of a tool's name. A client
// tool that breaks it would fail every request, so it fails the session start.
var mcpNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// mcpConn is one MCP-over-ACP connection.
type mcpConn struct {
	conn *sdk.Connection
	id   sdk.UnstableMcpConnectionId
}

// call sends an MCP request and returns its result.
func (c mcpConn) call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	return sdk.SendRequest[json.RawMessage](c.conn, ctx, sdk.AgentMethodMcpMessage, sdk.UnstableMessageMcpRequest{ConnectionId: c.id, Method: method, Params: params})
}

func (c mcpConn) notify(ctx context.Context, method string) error {
	return c.conn.SendNotification(ctx, sdk.AgentMethodMcpMessage, sdk.UnstableMessageMcpNotification{ConnectionId: c.id, Method: method})
}

// connectClientTools connects each of the client's ACP-transport MCP
// servers, and returns their tools. Other transports are not supported and
// are skipped. A tool named as one in reserved, or as one already listed,
// is an error.
func connectClientTools(ctx context.Context, conn *sdk.Connection, servers []sdk.McpServer, reserved map[string]bool) ([]core.Tool, error) {
	var tools []core.Tool
	taken := map[string]bool{}
	for name := range reserved {
		taken[name] = true
	}
	for _, server := range servers {
		if server.Acp == nil {
			log.Print("acp: ignoring an MCP server that isn't over ACP: only type \"acp\" is supported")
			continue
		}
		listed, err := listServerTools(ctx, conn, *server.Acp)
		if err != nil {
			return nil, fmt.Errorf("MCP server %q: %w", server.Acp.Name, err)
		}
		for _, tool := range listed {
			name := tool.Spec().Name
			if taken[name] {
				return nil, fmt.Errorf("MCP server %q: tool %q clashes with a tool grimalkin already has", server.Acp.Name, name)
			}
			taken[name] = true
			tools = append(tools, tool)
		}
	}
	return tools, nil
}

func listServerTools(ctx context.Context, conn *sdk.Connection, server sdk.McpServerAcpInline) ([]core.Tool, error) {
	connected, err := sdk.SendRequest[sdk.UnstableConnectMcpResponse](conn, ctx, sdk.ClientMethodMcpConnect, sdk.UnstableConnectMcpRequest{AcpId: sdk.UnstableMcpServerAcpId(server.Id)})
	if err != nil {
		return nil, fmt.Errorf("mcp/connect: %w", err)
	}
	mcp := mcpConn{conn: conn, id: connected.ConnectionId}
	if _, err := mcp.call(ctx, "initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "grimalkin", "version": "0"},
	}); err != nil {
		return nil, fmt.Errorf("MCP initialize: %w", err)
	}
	if err := mcp.notify(ctx, "notifications/initialized"); err != nil {
		return nil, fmt.Errorf("MCP initialized: %w", err)
	}
	var tools []core.Tool
	params := map[string]any{}
	for range maxToolPages {
		raw, err := mcp.call(ctx, "tools/list", params)
		if err != nil {
			return nil, fmt.Errorf("MCP tools/list: %w", err)
		}
		var page struct {
			Tools []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("MCP tools/list: %w", err)
		}
		for _, t := range page.Tools {
			if !mcpNamePattern.MatchString(t.Name) {
				return nil, fmt.Errorf("tool name %q isn't 1 to 64 of letters, digits, _ and -", t.Name)
			}
			schema := string(t.InputSchema)
			if len(t.InputSchema) == 0 {
				schema = `{"type":"object"}`
			} else if !json.Valid(t.InputSchema) {
				return nil, fmt.Errorf("tool %q has an input schema that isn't JSON", t.Name)
			}
			tools = append(tools, mcpTool{mcp: mcp, spec: core.ToolSpec{Name: t.Name, Description: t.Description, InputSchema: schema, NonStrict: true}})
		}
		if page.NextCursor == "" {
			return tools, nil
		}
		params = map[string]any{"cursor": page.NextCursor}
	}
	return nil, fmt.Errorf("MCP tools/list: more than %d pages", maxToolPages)
}

// shortcut: a server listing more pages than this fails, rather than looping on a cursor that never ends.
const maxToolPages = 20

// mcpTool is a tool of the client's, run by calling it over its connection.
type mcpTool struct {
	mcp  mcpConn
	spec core.ToolSpec
}

func (t mcpTool) Spec() core.ToolSpec { return t.spec }

// Summary is the tool's name and the start of its input.
func (t mcpTool) Summary(input json.RawMessage) string {
	const max = 80
	preview := strings.Join(strings.Fields(string(input)), " ")
	if r := []rune(preview); len(r) > max {
		preview = string(r[:max]) + "…"
	}
	return strings.TrimSpace(t.spec.Name + " " + preview)
}

func (t mcpTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	arguments := map[string]any{}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &arguments); err != nil {
			return "", fmt.Errorf("the input isn't a JSON object: %w", err)
		}
	}
	if arguments == nil { // the input was null
		arguments = map[string]any{}
	}
	raw, err := t.mcp.call(ctx, "tools/call", map[string]any{"name": t.spec.Name, "arguments": arguments})
	if err != nil {
		return "", fmt.Errorf("call %s: %w", t.spec.Name, err)
	}
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("call %s: %w", t.spec.Name, err)
	}
	var texts []string
	for _, c := range result.Content {
		if c.Type == "text" {
			texts = append(texts, c.Text)
		}
	}
	text := strings.Join(texts, "\n")
	if result.IsError {
		if text == "" {
			text = "the tool failed"
		}
		return "", errors.New(text)
	}
	return text, nil
}
