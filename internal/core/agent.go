package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

const maxModelCalls = 50

type Agent struct {
	Model      Model
	Transcript Transcript
	System     string
	Tools      []Tool
	Skills     Skills  // optional
	History    History // optional; a child never has one
	Gate       Gate    // optional; asked before every tool call. A child never has one

	// SubagentTools is the tool list of the children this agent starts.
	// The agent tool is offered only when it is non-nil.
	SubagentTools []Tool
	// ModelName is what Model is called, as provider/model: in a child's
	// status line, in the agent tool's model parameter, and on each assistant
	// message, so a model can tell its own from another's. Required.
	ModelName string
	// SubagentModels are the other models a child may be started on, in the
	// order the agent tool offers them. Empty: children run on Model.
	SubagentModels []SubagentModel

	number   int    // 0 for the parent
	parent   *Agent // nil for the parent
	messages []Message
	forked   bool // a child's: it started as a copy of the parent
	// unanswered is a fork's: the results its next message must carry for
	// the calls its history ends with, until a turn of its is kept.
	unanswered []ToolResult

	mu        sync.Mutex
	cancel    context.CancelFunc
	children  []*Agent   // child N is children[N-1]
	slots     childSlots // the parent's: which of its children may run
	running   bool       // a child's: claimed by a call until its turn ends
	toolCalls int        // a child's: tool calls started in its running turn
	steering  bool       // the parent's: its running turn takes steers
	held      []string   // steers the running turn hasn't taken yet
	nextModel Model      // the parent's: what the next turn switches to, if set
	nextName  string
}

// SubagentModel is a model children may run on beside the parent's.
type SubagentModel struct {
	Name  string // provider/model, as the agent tool's model parameter takes it
	Model Model
}

// errCancelled is the discard reason of a turn whose context was cancelled.
var errCancelled = errors.New("cancelled")

// SystemPrompt is the system prompt, with a catalog of the skills the model
// may load.
func SystemPrompt(osName, cwd string, skills []Skill) string {
	p := `You are a coding agent in a terminal on %s. Working directory: %s. 
The user chose brevity over narration. You should:

Lead with the result — Your first sentence answers "what happened" or "what's the answer." No preamble ("Let me...", "Now I'll...") and no closing recap of what you already said.
Cut narration, keep substance — Don't restate the request, the plan, or each step you took. Report outcomes, decisions, and anything the user must act on.
Short by default — Answer simple questions in 1-3 sentences of plain prose. Use headers, tables, and bullet lists only when they carry real structure, never as decoration.
State things plainly — Skip hedging boilerplate. Mention a caveat only when it changes what the user should do next.
Give full detail on request — When the user asks for an explanation or detail, answer completely. Conciseness never means withholding requested information.
Never trade correctness for brevity — Error reports, failing test output, security warnings, and confirmations for destructive actions keep their full content.
Where these rules conflict with more general communication or formatting guidance elsewhere in your instructions, these rules win.
`
	return fmt.Sprintf(p, osName, cwd) + skillCatalog(skills)
}

func (a *Agent) Cancel() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
	}
}

// SetModel switches the agent to model, called name as ModelName is, from
// the next turn on: a running turn ends on the model it started with. It may
// be called while a turn runs. A child can't be switched.
func (a *Agent) SetModel(model Model, name string) {
	if a.parent != nil {
		panic(fmt.Sprintf("agent %d switched", a.number))
	}
	if model == nil || name == "" {
		panic("model switch without a model or a name")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextModel, a.nextName = model, name
}

// RunTurn runs a turn that starts with prompt and returns when it has ended.
func (a *Agent) RunTurn(prompt string) { a.StartTurn(prompt)() }

// StartTurn makes the turn that starts with prompt cancellable and returns at
// once with run, which runs it and returns when it has ended. Cancel from this
// call on ends the turn, even before run has started it, so a caller that runs
// the turn on another goroutine starts it first. run must be called, once.
func (a *Agent) StartTurn(prompt string) (run func()) {
	a.mu.Lock()
	if a.nextModel != nil {
		a.Model, a.ModelName = a.nextModel, a.nextName
		a.nextModel, a.nextName = nil, ""
	}
	a.mu.Unlock()
	a.checkWiring()
	ctx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	a.cancel = cancel
	a.steering = true
	a.mu.Unlock()
	return func() { a.runTurn(ctx, cancel, prompt) }
}

func (a *Agent) runTurn(ctx context.Context, cancel context.CancelFunc, prompt string) {
	defer func() {
		a.mu.Lock()
		a.cancel = nil
		a.steering = false // already, unless the turn panicked
		a.mu.Unlock()
		cancel()
	}()

	text := prompt
	if name, arguments, ok := a.userSkill(prompt); ok {
		content, err := a.loadSkill(ctx, name, arguments, nil)
		if err != nil {
			reason := fmt.Errorf("skill %s: %w", name, err)
			if ctx.Err() != nil {
				reason = errCancelled
			}
			a.discardTurn(len(a.messages), nil, reason)
			return
		}
		a.show(SkillLoaded{Name: name, Content: content})
		text = content
	}
	a.turn(ctx, text)
}

// turn runs one turn that starts with text as the user's message, and
// returns the text of the response that ended it. A turn that fails is
// discarded from the history, but for its steers, and returns the reason,
// errCancelled when ctx was cancelled.
func (a *Agent) turn(ctx context.Context, text string) (string, error) {
	checkpoint := len(a.messages)
	a.messages = append(a.messages, Message{Role: RoleUser, Text: text, ToolResults: a.unanswered})
	var steered []string // the steers the turn took, which outlive it

	specs := a.toolSpecs()
	for range maxModelCalls {
		a.show(ReplyStarted{})
		req := ModelRequest{System: a.System, Tools: specs, Messages: conversation(a.messages), Model: a.ModelName}
		// Content is opaque, so the streamed text is the only record of
		// what a response said.
		var reply strings.Builder
		resp, err := a.Model.Stream(ctx, req, func(text string) {
			reply.WriteString(text)
			a.show(ReplyText{text})
		})
		if err != nil {
			if ctx.Err() != nil {
				err = errCancelled
			}
			return "", a.discardTurn(checkpoint, steered, err)
		}
		a.messages = append(a.messages, Message{Role: RoleAssistant, Content: resp.Content, Model: a.ModelName})

		switch resp.StopReason {
		case StopEndTurn:
			// Steers that came during the call get an answer in this turn.
			if held := a.takeSteers(true); len(held) > 0 {
				steered = append(steered, held...)
				a.messages = append(a.messages, Message{Role: RoleUser, Text: a.deliver(held)})
				continue
			}
			a.unanswered = nil
			if a.History != nil {
				a.History.Keep(slices.Clone(a.messages[checkpoint:]))
			}
			return reply.String(), nil
		case StopToolUse:
			if len(resp.ToolCalls) == 0 {
				panic("stop_reason tool_use without tool calls")
			}
			results := a.runTools(ctx, resp.ToolCalls)
			if ctx.Err() != nil {
				return "", a.discardTurn(checkpoint, steered, errCancelled)
			}
			held := a.takeSteers(false)
			steered = append(steered, held...)
			a.messages = append(a.messages, Message{Role: RoleUser, ToolResults: results, Text: a.deliver(held)})
		case StopOther:
			return "", a.discardTurn(checkpoint, steered, errors.New("stopped: "+resp.StopDetail))
		default:
			panic(fmt.Sprintf("unknown stop reason %d", resp.StopReason))
		}
	}
	return "", a.discardTurn(checkpoint, steered, fmt.Errorf("gave up after %d model calls", maxModelCalls))
}

// Seed starts the conversation from messages History was given, and must be
// called before the first turn. It fails, leaving the history empty, unless
// the messages are what the agent could have kept: each assistant's, which
// has content, answering a user's. A user's may follow a user's, as the next
// prompt follows a discarded turn's steers, or end the conversation, as the
// steers do until then; one that follows a user's has no tool results.
func (a *Agent) Seed(messages []Message) error {
	if len(a.messages) != 0 {
		panic("agent seeded after a turn")
	}
	for i, m := range messages {
		afterUser := i > 0 && messages[i-1].Role == RoleUser
		switch {
		case m.Role == RoleAssistant && (!afterUser || len(m.Content) == 0):
			return fmt.Errorf("message %d: an assistant's must answer the user's and have content", i)
		case m.Role == RoleUser && afterUser && len(m.ToolResults) > 0:
			return fmt.Errorf("message %d: tool results after the user's", i)
		case m.Role != RoleUser && m.Role != RoleAssistant:
			return fmt.Errorf("message %d: unknown role %d", i, m.Role)
		}
	}
	a.messages = slices.Clone(messages)
	return nil
}

func (a *Agent) checkWiring() {
	if a.Model == nil || a.Transcript == nil || a.ModelName == "" {
		panic("agent ports or model name not wired")
	}
	a.checkToolNames(a.Tools)
	if a.SubagentTools != nil {
		a.checkToolNames(a.SubagentTools)
	}
	a.checkSubagentModels()
}

// checkSubagentModels panics unless every allowed model has a model and a
// name of its own, which the parent's may not be: a name picks one model.
func (a *Agent) checkSubagentModels() {
	if len(a.SubagentModels) > 0 && a.SubagentTools == nil {
		panic("subagent models wired without subagent tools")
	}
	names := map[string]bool{a.ModelName: true}
	for _, m := range a.SubagentModels {
		if m.Name == "" || m.Model == nil {
			panic(fmt.Sprintf("subagent model %q wired without a name or a model", m.Name))
		}
		if names[m.Name] {
			panic(fmt.Sprintf("two models named %q", m.Name))
		}
		names[m.Name] = true
	}
}

// checkToolNames panics when two of tools share a name, or one has the name
// of a tool the core adds. No list may hold the agent tool: the parent's has
// the core's, and a child's has none.
func (a *Agent) checkToolNames(tools []Tool) {
	names := map[string]bool{}
	for _, tool := range tools {
		name := tool.Spec().Name
		if names[name] {
			panic(fmt.Sprintf("two tools named %q", name))
		}
		names[name] = true
	}
	if a.Skills != nil && names[SkillToolName] {
		panic(fmt.Sprintf("tool %q clashes with the skill tool", SkillToolName))
	}
	if a.SubagentTools != nil && names[AgentToolName] {
		panic(fmt.Sprintf("tool %q clashes with the agent tool", AgentToolName))
	}
}

// show reports an event of this agent's to the transcript.
func (a *Agent) show(event Event) {
	a.Transcript.Show(a.number, event)
}

// discardTurn drops the turn's messages and returns reason. The steers it
// took and those still held stay, as one user message History is given.
func (a *Agent) discardTurn(checkpoint int, steered []string, reason error) error {
	a.mu.Lock()
	held := a.held
	a.held, a.steering = nil, false
	a.mu.Unlock()
	a.deliver(held)
	a.show(TurnDiscarded{reason.Error()})
	a.messages = a.messages[:checkpoint]
	if steered = append(steered, held...); len(steered) > 0 {
		kept := Message{Role: RoleUser, Text: joinTexts(steered...)}
		a.messages = append(a.messages, kept)
		if a.History != nil {
			a.History.Keep([]Message{kept})
		}
	}
	return reason
}
