package core

import (
	"context"
	"fmt"
	"sync"
)

const maxModelCalls = 50

type Agent struct {
	Model      Model
	Transcript Transcript
	System     string
	Tools      []Tool
	Skills     Skills // optional

	messages []Message

	mu     sync.Mutex
	cancel context.CancelFunc
}

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

func (a *Agent) RunTurn(prompt string) {
	a.checkWiring()
	ctx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	a.cancel = cancel
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.cancel = nil
		a.mu.Unlock()
		cancel()
	}()

	checkpoint := len(a.messages)
	text := prompt
	if name, arguments, ok := a.userSkill(prompt); ok {
		content, err := a.loadSkill(ctx, name, arguments, nil)
		if err != nil {
			reason := fmt.Sprintf("skill %s: %v", name, err)
			if ctx.Err() != nil {
				reason = "cancelled"
			}
			a.discardTurn(checkpoint, reason)
			return
		}
		a.Transcript.Show(SkillLoaded{Name: name, Content: content})
		text = content
	}
	a.messages = append(a.messages, Message{Role: RoleUser, Text: text})

	specs := a.toolSpecs()
	for range maxModelCalls {
		a.Transcript.Show(ReplyStarted{})
		req := ModelRequest{System: a.System, Tools: specs, Messages: a.messages}
		resp, err := a.Model.Stream(ctx, req, func(text string) { a.Transcript.Show(ReplyText{text}) })
		if err != nil {
			reason := err.Error()
			if ctx.Err() != nil {
				reason = "cancelled"
			}
			a.discardTurn(checkpoint, reason)
			return
		}
		a.messages = append(a.messages, Message{Role: RoleAssistant, Content: resp.Content})

		switch resp.StopReason {
		case StopEndTurn:
			return
		case StopToolUse:
			if len(resp.ToolCalls) == 0 {
				panic("stop_reason tool_use without tool calls")
			}
			results := make([]ToolResult, len(resp.ToolCalls))
			for i, call := range resp.ToolCalls {
				results[i] = a.runTool(ctx, call, results[:i])
				if ctx.Err() != nil {
					a.discardTurn(checkpoint, "cancelled")
					return
				}
			}
			a.messages = append(a.messages, Message{Role: RoleUser, ToolResults: results})
		case StopOther:
			a.discardTurn(checkpoint, "stopped: "+resp.StopDetail)
			return
		default:
			panic(fmt.Sprintf("unknown stop reason %d", resp.StopReason))
		}
	}
	a.discardTurn(checkpoint, fmt.Sprintf("gave up after %d model calls", maxModelCalls))
}

func (a *Agent) checkWiring() {
	if a.Model == nil || a.Transcript == nil {
		panic("agent ports not wired")
	}
	names := map[string]bool{}
	for _, tool := range a.Tools {
		name := tool.Spec().Name
		if names[name] {
			panic(fmt.Sprintf("two tools named %q", name))
		}
		names[name] = true
	}
	if a.Skills != nil && names[skillToolName] {
		panic(fmt.Sprintf("tool %q clashes with the skill tool", skillToolName))
	}
}

func (a *Agent) discardTurn(checkpoint int, reason string) {
	a.Transcript.Show(TurnDiscarded{reason})
	a.messages = a.messages[:checkpoint]
}
