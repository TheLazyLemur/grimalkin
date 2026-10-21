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
	NonStrict   bool   // the schema isn't strict-compatible (a client's tool); zero keeps strict
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

// Message is one message of a conversation. A user message has Text,
// ToolResults, or both, and then the results come before the text; an
// assistant message has only Content, the provider's own form of it as JSON.
// The core never reads Content, and a Message is plain data, so a
// conversation saved with encoding/json goes back to the provider as it was.
type Message struct {
	Role        Role
	Text        string
	ToolResults []ToolResult
	Content     json.RawMessage
	// Model is an assistant message's: the name of the model that wrote it,
	// as Agent.ModelName. Empty in one saved before messages named it.
	Model string
}

type ModelRequest struct {
	System   string
	Tools    []ToolSpec
	Messages []Message
	// Model is the name of the model the request goes to, as Message.Model
	// has it. Another model's content, or content of no named model, may carry
	// what this one can't verify, such as thinking signatures; the model
	// drops that.
	Model string
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
	Content    json.RawMessage // the provider's, kept as Message.Content
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

// Gate decides whether a tool call may run. Allow is called after the call's
// ToolStarted and before the tool runs, and may block for as long as a person
// takes to answer; ctx is the turn's, and Allow must return once it is done.
// A call that is not allowed doesn't run: the model gets reason as an error
// result. Allow is called from several goroutines at once.
type Gate interface {
	Allow(ctx context.Context, call ToolCall) (ok bool, reason string)
}

// Transcript shows an event of the numbered agent's, the parent being 0.
// Show is called from several goroutines at once while tool calls overlap.
type Transcript interface {
	Show(agent int, event Event)
}

// History records the conversation as the agent keeps it. Keep is called on
// the turn's goroutine when a turn ends and is kept, with that turn's
// messages in order. A discarded turn's messages are never passed, but for
// its steers: when it had any, Keep is called after its TurnDiscarded with
// one user message holding them all, which the next prompt follows. So what
// History has been given is always the whole conversation, ready for
// Agent.Seed.
type History interface {
	Keep(messages []Message)
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
