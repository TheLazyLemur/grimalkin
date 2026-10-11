package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
)

// AgentToolName is the tool the model hands a task to a child with.
const AgentToolName = "agent"

const agentToolDescription = "Hand a self-contained task to a subagent, which works through it with its own tools and a fresh context, and returns only its final reply. The subagent sees nothing of this conversation, so the task must say everything it needs. Several calls in one response run at the same time. The result starts with [agent N]; pass that number as agent to send the same subagent a follow-up."

var agentToolSpec = ToolSpec{
	Name:        AgentToolName,
	Description: agentToolDescription,
	InputSchema: `{"type":"object","properties":{` +
		`"task":{"type":"string","description":"What the subagent should do, or the follow-up when agent is given."},` +
		`"agent":{"type":"integer","description":"The number of an earlier subagent to resume, which keeps its history. Omit to start a fresh one."}` +
		`},"required":["task"],"additionalProperties":false}`,
}

// subagentNote ends a fresh child's system prompt.
const subagentNote = "\n# Subagent\n\nYou are a subagent. Another agent gave you the task below and your final reply goes back to it, not to the user. It sees nothing else of your work, so that reply must be complete: everything it needs, with paths and line numbers, and nothing that depends on text you wrote earlier.\n"

type agentInput struct {
	Task  *string
	Agent *int
}

// childClaim is an agent call's right to run a turn of child, or the error
// that answers the call instead.
type childClaim struct {
	child *Agent
	task  string
	slot  childSlot
	err   error
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
// runChild ends.
func (a *Agent) claimChild(input json.RawMessage) childClaim {
	var in agentInput
	if json.Unmarshal(input, &in) != nil || in.Task == nil {
		return childClaim{err: errors.New(`invalid input: expected {"task": string, "agent"?: integer}`)}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if in.Agent == nil {
		child := &Agent{
			Model:      a.Model,
			Transcript: a.Transcript,
			System:     a.System + subagentNote,
			Tools:      a.SubagentTools,
			Skills:     a.Skills,
			number:     len(a.children) + 1,
			parent:     a,
			running:    true,
		}
		a.children = append(a.children, child)
		return childClaim{child: child, task: *in.Task, slot: a.slots.request()}
	}
	if *in.Agent < 1 || *in.Agent > len(a.children) {
		return childClaim{err: fmt.Errorf("unknown agent %d", *in.Agent)}
	}
	child := a.children[*in.Agent-1]
	child.mu.Lock()
	defer child.mu.Unlock()
	if child.running {
		return childClaim{err: fmt.Errorf("agent %d is busy", child.number)}
	}
	child.running = true
	return childClaim{child: child, task: *in.Task, slot: a.slots.request()}
}

func (a *Agent) runAgentCall(ctx context.Context, call ToolCall, claim childClaim) ToolResult {
	// The cap is applied to the child's reply, below its number.
	return a.runCall(call, AgentToolName+" "+string(call.Input), 0, func() (string, error) {
		if claim.err != nil {
			return "", claim.err
		}
		return a.runChild(ctx, claim.child, claim.task, claim.slot)
	})
}

// runChild runs one turn of a claimed child once it has its slot, and returns
// its number and its final reply, or as an error its number and why the turn
// was discarded. A child cancelled while it waits for the slot never starts.
func (a *Agent) runChild(ctx context.Context, child *Agent, task string, slot childSlot) (string, error) {
	if child.number < 1 || child.parent != a {
		panic(fmt.Sprintf("agent %d is not a child of agent %d", child.number, a.number))
	}
	defer child.endTurn()
	header := fmt.Sprintf("[agent %d]\n", child.number)

	select {
	case <-slot:
	default:
		a.show(ChildWaiting{Child: child.number, Model: a.ModelName})
		select {
		case <-slot:
		case <-ctx.Done():
			a.slots.abandon(slot)
			a.show(ChildCancelled{Child: child.number})
			return "", errors.New(header + errCancelled.Error())
		}
	}
	defer a.slots.release()

	child.startTurn()
	a.show(ChildStarted{Child: child.number, Model: a.ModelName})

	reply, err := child.turn(ctx, task)
	switch {
	case errors.Is(err, errCancelled):
		a.show(ChildCancelled{Child: child.number})
		return "", errors.New(header + err.Error())
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

// endTurn gives up the claim on a child, so it can be resumed.
func (a *Agent) endTurn() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running {
		panic(fmt.Sprintf("agent %d ends a turn it was not running", a.number))
	}
	a.running = false
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
