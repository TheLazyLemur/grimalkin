// Package core is the domain core: conversation, turn loop and tool rules,
// plus the ports the adapters implement. No I/O here, and no imports from the
// rest of the application.
package core

import (
	"context"
	"encoding/json"
)

type Role int

const (
	RoleUser Role = iota
	RoleAssistant
)

type ToolSpec struct {
	Name        string
	Description string
	InputSchema string // JSON Schema
}

type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

type ToolResult struct {
	ToolUseID string
	Content   string
	IsError   bool
}

type Message struct {
	Role        Role
	Text        string
	ToolResults []ToolResult
	Content     any
}

type ModelRequest struct {
	System   string
	Tools    []ToolSpec
	Messages []Message
}

type StopReason int

const (
	StopEndTurn StopReason = iota + 1
	StopToolUse
	StopOther
)

type ModelResponse struct {
	StopReason StopReason
	StopDetail string // the provider's own reason, shown when StopReason is StopOther
	ToolCalls  []ToolCall
	Content    any
}

type Model interface {
	Stream(ctx context.Context, req ModelRequest, onText func(string)) (ModelResponse, error)
}

type Tool interface {
	Spec() ToolSpec
	Summary(input json.RawMessage) string
	Run(ctx context.Context, input json.RawMessage) (string, error)
}

type Transcript interface {
	Show(Event)
}
