package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// AgentToolName is the tool the model hands a task to a child with.
const AgentToolName = "agent"

const agentToolDescription = "Hand a self-contained task to a subagent, which works through it with its own tools and a fresh context, and returns only its final reply. A fresh subagent sees nothing of this conversation, so the task must say everything it needs. Set fork to start the subagent as a copy of you instead: it has this whole conversation, your tools and your model, so the task can be short. Fork when the task depends on what you have already read or worked out here; otherwise start fresh, which keeps the subagent's context small. Several calls in one response run at the same time. The result starts with [agent N]; pass that number as agent to send the same subagent a follow-up."

// agentToolModelsDescription is added when there are models to choose from.
const agentToolModelsDescription = " A fresh subagent runs on your model unless model names another; pick a smaller one for simple reading and searching."

// agentToolSpec is the agent tool as this agent offers it. The model
// parameter is there only when there are models to choose from.
func (a *Agent) agentToolSpec() ToolSpec {
	description, model := agentToolDescription, ""
	if len(a.SubagentModels) > 0 {
		enum, err := json.Marshal(a.childModelNames())
		if err != nil {
			panic(err)
		}
		description += agentToolModelsDescription
		model = `,"model":{"type":"string","enum":` + string(enum) + `,"description":"The model a fresh subagent runs on. Omit for your own. Not allowed with agent, and a fork always runs on your own."}`
	}
	return ToolSpec{
		Name:        AgentToolName,
		Description: description,
		InputSchema: `{"type":"object","properties":{` +
			`"task":{"type":"string","description":"What the subagent should do, or the follow-up when agent is given."},` +
			`"agent":{"type":"integer","description":"The number of an earlier subagent to resume, which keeps its history. Omit to start a fresh one."},` +
			`"fork":{"type":"boolean","description":"Start the subagent as a copy of you, with this conversation. Not allowed with agent."}` +
			model +
			`},"required":["task"],"additionalProperties":false}`,
	}
}

// childModelNames lists the models a fresh child may be started on, as the
// agent tool's model enum: this agent's own first, which is what omitting
// model means, then the allowed ones in the order they were wired.
func (a *Agent) childModelNames() []string {
	names := []string{a.ModelName}
	for _, m := range a.SubagentModels {
		names = append(names, m.Name)
	}
	return names
}

// childModel returns the model called name in childModelNames.
func (a *Agent) childModel(name string) (Model, bool) {
	if name == a.ModelName {
		return a.Model, true
	}
	for _, m := range a.SubagentModels {
		if m.Name == name {
			return m.Model, true
		}
	}
	return nil, false
}

// subagentNote ends a fresh child's system prompt.
const subagentNote = "\n# Subagent\n\nYou are a subagent. Another agent gave you the task below and your final reply goes back to it, not to the user. It sees nothing else of your work, so that reply must be complete: everything it needs, with paths and line numbers, and nothing that depends on text you wrote earlier.\n"

// A fork's first message answers every tool call of the response it was
// copied with, since a call must have a result before anything else is said.
// The fork's system prompt is the parent's, so its own call's result is also
// where it learns what it is.
const (
	forkOwnCallResult   = "You are the subagent this call started: a fork, with a copy of the conversation up to here. Do the task that follows. Your final reply goes back to the agent that forked you, not to the user, and it sees nothing else of your work, so that reply must be complete. You can't start subagents yourself."
	forkOtherCallResult = "This call is running in the agent that forked you. Its result will not reach you; make the call yourself if you need it."
)

// errForkStartsSubagent answers a fork's call of the agent tool, which it is
// offered only so that its tool list matches the parent's.
var errForkStartsSubagent = errors.New("subagents can't start subagents")

type agentInput struct {
	Task  *string
	Agent *int
	Model *string
	Fork  *bool
}

// childClaim is an agent call's right to run a turn of child, or the error
// that answers the call instead.
type childClaim struct {
	child *Agent
	task  string
	slot  childSlot
	// ctx is the calling turn's, cancelled as well when the child alone is.
	ctx context.Context
	err error
}

// maxRunningChildren is how many children run at once across the session.
const maxRunningChildren = 5

// childSlot is a claimed child's place among the running children, closed
// once it has one.
type childSlot chan struct{}

// childSlots hands out the session's slots. It lives in the parent, so
// every turn of every child it started is counted.
type childSlots struct {
	mu      sync.Mutex
	running int
	waiting []childSlot // in the order they were requested
}

// request returns a slot that is granted now if one is free, and otherwise
// when those requested before it have had theirs.
func (s *childSlots) request() childSlot {
	s.mu.Lock()
	defer s.mu.Unlock()
	slot := make(childSlot)
	if s.running == maxRunningChildren {
		s.waiting = append(s.waiting, slot)
		return slot
	}
	if len(s.waiting) != 0 {
		panic("a child is waiting while a slot is free")
	}
	s.running++
	close(slot)
	return slot
}

// release gives a granted slot to the child that has waited longest, or
// frees it.
func (s *childSlots) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseLocked()
}

func (s *childSlots) releaseLocked() {
	if s.running < 1 {
		panic("a child slot released that no child held")
	}
	if len(s.waiting) == 0 {
		s.running--
		return
	}
	close(s.waiting[0])
	s.waiting = s.waiting[1:]
}

// abandon gives up a slot whose child stopped waiting for it. It may have
// been granted meanwhile, and is then released.
func (s *childSlots) abandon(slot childSlot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.Index(s.waiting, slot); i >= 0 {
		s.waiting = slices.Delete(s.waiting, i, i+1)
		return
	}
	s.releaseLocked()
}

// claimChild resolves an agent call's input to the child it runs: a new one
// with the next number, or the one it resumes, and requests its slot, so
// slots go to a response's calls in call order. The child stays claimed until
// runChild ends, and can be cancelled alone from here on. call is one of
// calls, the response's, which a fork answers. It runs on the turn's
// goroutine, the only one that touches a.messages.
func (a *Agent) claimChild(ctx context.Context, call ToolCall, calls []ToolCall) childClaim {
	var in agentInput
	if json.Unmarshal(call.Input, &in) != nil || in.Task == nil {
		return childClaim{err: errors.New(`invalid input: expected {"task": string, "agent"?: integer, "model"?: string, "fork"?: boolean}`)}
	}
	fork := in.Fork != nil && *in.Fork
	switch {
	case in.Agent != nil && in.Model != nil:
		return childClaim{err: errors.New("model can't be given with agent: a resumed subagent keeps its model")}
	case in.Agent != nil && fork:
		return childClaim{err: errors.New("fork can't be given with agent: a resumed subagent keeps its history")}
	case fork && in.Model != nil && *in.Model != a.ModelName:
		return childClaim{err: fmt.Errorf("a fork runs on your own model, %q: omit model, or leave out fork to use %q", a.ModelName, *in.Model)}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if fork {
		child := a.fork(call, calls)
		a.children = append(a.children, child)
		return a.claim(ctx, child, *in.Task)
	}
	if in.Agent == nil {
		name := a.ModelName
		if in.Model != nil {
			name = *in.Model
		}
		model, ok := a.childModel(name)
		if !ok {
			return childClaim{err: fmt.Errorf("unknown model %q: expected one of %s", name, quotedList(a.childModelNames()))}
		}
		child := &Agent{
			Model:      model,
			ModelName:  name,
			Transcript: a.Transcript,
			System:     a.System + subagentNote,
			Tools:      a.SubagentTools,
			Skills:     a.Skills,
			number:     len(a.children) + 1,
			parent:     a,
			running:    true,
		}
		a.children = append(a.children, child)
		return a.claim(ctx, child, *in.Task)
	}
	return a.claimExisting(ctx, *in.Agent, *in.Task)
}

// fork returns a new child that is a copy of this agent as it handles the
// response that made call: its system prompt, tools and model, and its
// history, which ends with that response. The response's calls are answered
// by the child's first message, ahead of its task. The caller holds a.mu.
func (a *Agent) fork(call ToolCall, calls []ToolCall) *Agent {
	if len(a.messages) == 0 || a.messages[len(a.messages)-1].Role != RoleAssistant {
		panic("an agent forked outside a response's tool calls")
	}
	results := make([]ToolResult, len(calls))
	for i, c := range calls {
		results[i] = ToolResult{ToolUseID: c.ID, Content: forkOtherCallResult}
		if c.ID == call.ID {
			results[i].Content = forkOwnCallResult
		}
	}
	return &Agent{
		Model:      a.Model,
		ModelName:  a.ModelName,
		Transcript: a.Transcript,
		System:     a.System,
		Tools:      a.Tools,
		Skills:     a.Skills,
		number:     len(a.children) + 1,
		parent:     a,
		running:    true,
		forked:     true,
		messages:   slices.Clone(a.messages),
		unanswered: results,
	}
}

// claimExisting claims child number for a turn that starts with task, or
// says why it can't: no child has the number, or it has a turn already. The
// caller holds a.mu.
func (a *Agent) claimExisting(ctx context.Context, number int, task string) childClaim {
	if number < 1 || number > len(a.children) {
		return childClaim{err: fmt.Errorf("unknown agent %d", number)}
	}
	child := a.children[number-1]
	child.mu.Lock()
	defer child.mu.Unlock()
	if child.running {
		return childClaim{err: fmt.Errorf("agent %d is busy", child.number)}
	}
	child.running = true
	return a.claim(ctx, child, task)
}

// StartChildTurn claims child number for a turn the user starts on it, with
// prompt as its follow-up. It returns at once: an error when no child has
// the number or the child has a turn already, and otherwise run, which runs
// the turn and returns when it has ended. The child is busy from this call
// until run returns, so run must be called, once.
//
// The turn is run like one the agent tool starts: it waits for a slot, and
// its lifecycle is shown as this agent's. It is not part of this agent's
// turn, so CancelChild ends it and Cancel does not. The prompt is sent as it
// is; /name loads no skill here.
func (a *Agent) StartChildTurn(number int, prompt string) (run func(), err error) {
	a.mu.Lock()
	claim := a.claimExisting(context.Background(), number, prompt)
	a.mu.Unlock()
	if claim.err != nil {
		return nil, claim.err
	}
	return func() { a.runChild(context.Background(), claim) }, nil
}

// claim gives a child marked running its slot request and the context its
// turn runs under. The caller holds a.mu and may hold child.mu, which is why
// the cancel is set here and not through a method that locks.
func (a *Agent) claim(ctx context.Context, child *Agent, task string) childClaim {
	if !child.running || child.cancel != nil {
		panic(fmt.Sprintf("agent %d claimed while it has a turn", child.number))
	}
	ctx, child.cancel = context.WithCancel(ctx)
	return childClaim{child: child, task: task, slot: a.slots.request(), ctx: ctx}
}

// errCancelledByUser answers the call of a child that was cancelled alone.
var errCancelledByUser = errors.New("cancelled by user")

// CancelChild cancels the turn child number is running or waiting to run,
// and leaves this agent's turn and its other children alone. It does nothing
// when that child has no turn, or no child has the number.
func (a *Agent) CancelChild(number int) {
	a.mu.Lock()
	if number < 1 || number > len(a.children) {
		a.mu.Unlock()
		return
	}
	child := a.children[number-1]
	a.mu.Unlock()
	child.Cancel()
}

func (a *Agent) runAgentCall(ctx context.Context, call ToolCall, claim childClaim) ToolResult {
	// The cap is applied to the child's reply, below its number.
	return a.runCall(ctx, call, AgentToolName+" "+string(call.Input), 0, func() (string, error) {
		if claim.err != nil {
			return "", claim.err
		}
		return a.runChild(ctx, claim)
	})
}

// runChild runs one turn of a claimed child once it has its slot, and returns
// its number and its final reply, or as an error its number and why the turn
// was discarded. A child cancelled while it waits for the slot never starts.
// ctx is the call's; a child cancelled while it is not was cancelled alone,
// and says so.
func (a *Agent) runChild(ctx context.Context, claim childClaim) (string, error) {
	child, slot := claim.child, claim.slot
	if child.number < 1 || child.parent != a {
		panic(fmt.Sprintf("agent %d is not a child of agent %d", child.number, a.number))
	}
	defer child.endTurn()
	header := fmt.Sprintf("[agent %d]\n", child.number)
	cancelled := func() (string, error) {
		a.show(ChildCancelled{Child: child.number})
		if ctx.Err() != nil {
			return "", errors.New(header + errCancelled.Error())
		}
		return "", errors.New(header + errCancelledByUser.Error())
	}

	select {
	case <-slot:
	default:
		a.show(ChildWaiting{Child: child.number, Model: child.ModelName})
		select {
		case <-slot:
		case <-claim.ctx.Done():
			a.slots.abandon(slot)
			return cancelled()
		}
	}
	defer a.slots.release()
	// A cancelled child must not start, and the slot can be one freed by
	// the cancel that ends this child too: the select above may pick it, and
	// a cancel of ctx reaches the children's contexts one after another, so
	// a sibling can have released it before this child's is done.
	if ctx.Err() != nil || claim.ctx.Err() != nil {
		return cancelled()
	}

	child.startTurn()
	a.show(ChildStarted{Child: child.number, Model: child.ModelName})

	reply, err := child.turn(claim.ctx, claim.task)
	switch {
	case errors.Is(err, errCancelled):
		return cancelled()
	case err != nil:
		a.show(ChildFailed{Child: child.number, Reason: err.Error()})
		return "", errors.New(header + lastBytes(err.Error(), maxToolResultBytes))
	}
	a.show(ChildFinished{Child: child.number})
	return header + lastBytes(reply, maxToolResultBytes), nil
}

// startTurn begins a claimed child's count of tool calls.
func (a *Agent) startTurn() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running {
		panic(fmt.Sprintf("agent %d starts a turn it has no claim to", a.number))
	}
	a.toolCalls = 0
}

// endTurn gives up the claim on a child, so it can be resumed, and a cancel
// from then on does nothing.
func (a *Agent) endTurn() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running || a.cancel == nil {
		panic(fmt.Sprintf("agent %d ends a turn it was not running", a.number))
	}
	a.running = false
	a.cancel()
	a.cancel = nil
}

// countToolCall reports a child's tool call starting to its parent. The lock
// is held while it is shown so that counts arrive in order.
func (a *Agent) countToolCall() {
	if a.parent == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running {
		panic(fmt.Sprintf("agent %d starts a tool call outside a turn", a.number))
	}
	a.toolCalls++
	a.parent.show(ChildProgress{Child: a.number, Tools: a.toolCalls})
}

func quotedList(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = fmt.Sprintf("%q", name)
	}
	return strings.Join(quoted, ", ")
}
