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

// Tool is called from several goroutines at once: a response's calls run at
// the same time, two of them possibly to the same tool.
type Tool interface {
	Spec() ToolSpec
	Summary(input json.RawMessage) string
	Run(ctx context.Context, input json.RawMessage) (string, error)
}

// Transcript shows an event of the numbered agent's, the parent being 0.
// Show is called from several goroutines at once while tool calls overlap.
type Transcript interface {
	Show(agent int, event Event)
}

// Skill is a skill's catalog entry: what the model and the user can see
// before it is loaded.
type Skill struct {
	Name        string
	Description string
	// ModelInvocable is false when the skill opts out of model activation:
	// it is left out of the catalog and the skill tool.
	ModelInvocable bool
	// UserInvocable is false when the user can't load it with /name.
	UserInvocable bool
}

// Skills is the skill library. Render returns a skill's instructions with
// arguments expanded, ready to put in front of the model; an error aborts
// that activation.
type Skills interface {
	List() []Skill
	Render(ctx context.Context, name, arguments string) (string, error)
}
