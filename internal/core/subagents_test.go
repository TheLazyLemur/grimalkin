package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
)

const parentModelName = "test/model"

func agentCall(id, input string) ToolCall {
	return toolCall(id, AgentToolName, input)
}

// newParent is an agent that may start children, which get subagentTools.
func newParent(model *mockModel, transcript *mockTranscript, subagentTools ...Tool) *Agent {
	return &Agent{
		Model: model, Transcript: transcript, System: "base prompt",
		ModelName: parentModelName, SubagentTools: append([]Tool{}, subagentTools...),
	}
}

func toolNames(specs []ToolSpec) []string {
	names := make([]string, len(specs))
	for i, spec := range specs {
		names[i] = spec.Name
	}
	return names
}

// lifecycle returns the transcript's child lifecycle events, which must all
// have been shown as the parent's.
func (m *mockTranscript) lifecycle() []Event {
	var events []Event
	for i, event := range m.events {
		switch event.(type) {
		case ChildWaiting, ChildStarted, ChildProgress, ChildFinished, ChildFailed, ChildCancelled:
			if m.agents[i] != 0 {
				panic("lifecycle event not shown as the parent's")
			}
			events = append(events, event)
		}
	}
	return events
}

// lifecycleOf returns the lifecycle events that name child.
func (m *mockTranscript) lifecycleOf(child int) []Event {
	var events []Event
	for _, event := range m.lifecycle() {
		var of int
		switch e := event.(type) {
		case ChildWaiting:
			of = e.Child
		case ChildStarted:
			of = e.Child
		case ChildProgress:
			of = e.Child
		case ChildFinished:
			of = e.Child
		case ChildFailed:
			of = e.Child
		case ChildCancelled:
			of = e.Child
		}
		if of == child {
			events = append(events, event)
		}
	}
	return events
}

// childGate holds every child's model call until it is opened or the call
// is cancelled, and counts the calls it is holding.
type childGate struct {
	held chan struct{} // receives once for each call that starts being held
	open chan struct{}

	mu            sync.Mutex
	holding, peak int
	calls         int
}

func newChildGate() *childGate {
	return &childGate{held: make(chan struct{}, 100), open: make(chan struct{})}
}

func (g *childGate) hold(ctx context.Context, req ModelRequest) error {
	if !strings.HasSuffix(req.System, subagentNote) {
		return nil
	}
	g.count(1)
	defer g.count(-1)
	g.held <- struct{}{}
	select {
	case <-g.open:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *childGate) count(change int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.holding += change
	g.peak = max(g.peak, g.holding)
	g.calls += max(change, 0)
}

// awaitHeld waits until n more calls have started being held.
func (g *childGate) awaitHeld(n int) error {
	var errs []error
	for range n {
		errs = append(errs, await(g.held))
	}
	return errors.Join(errs...)
}

// sixAgentCalls is a response that hands out six tasks at once.
func sixAgentCalls() mockReply {
	var calls []ToolCall
	for i := 1; i <= 6; i++ {
		calls = append(calls, agentCall(fmt.Sprintf("toolu_%d", i), `{"task":"dig"}`))
	}
	return mockReply{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: calls, Content: "P1"}}
}

func TestASixthChildWaitsForOneOfFiveSlotsAndResultsComeBackInCallOrder(t *testing.T) {
	// given
	// ... a response starting six children, whose model calls are held until the test opens the gate
	done := mockReply{text: "done", resp: ModelResponse{StopReason: StopEndTurn, Content: "C"}}
	model := &mockModel{replies: []mockReply{
		sixAgentCalls(), done, done, done, done, done, done,
		{resp: ModelResponse{StopReason: StopEndTurn, Content: "P2"}},
	}}
	gate := newChildGate()
	model.hold = gate.hold
	transcript := &mockTranscript{}
	sixthWaiting := transcript.watch(ChildWaiting{Child: 6, Model: parentModelName})
	a := newParent(model, transcript)
	ended := make(chan struct{})

	// when
	// ... the turn runs, and the gate is opened once five children are held and the sixth is waiting
	go func() {
		defer close(ended)
		a.RunTurn("go")
	}()
	reached := errors.Join(gate.awaitHeld(5), await(sixthWaiting))
	close(gate.open)
	finished := await(ended)

	// then
	// ... five children ran at once and never six, the sixth waited and then started, and all six answered in call order
	if reached != nil || finished != nil {
		t.Fatalf("five held and the sixth waiting: %v; turn ended: %v", reached, finished)
	}
	if gate.peak != 5 {
		t.Errorf("most children running at once = %d, want 5", gate.peak)
	}
	wantSixth := []Event{
		ChildWaiting{Child: 6, Model: parentModelName}, ChildStarted{Child: 6, Model: parentModelName}, ChildFinished{Child: 6},
	}
	if got := transcript.lifecycleOf(6); !slices.Equal(got, wantSixth) {
		t.Errorf("sixth child's lifecycle = %#v\nwant %#v", got, wantSixth)
	}
	wantFirst := []Event{ChildStarted{Child: 1, Model: parentModelName}, ChildFinished{Child: 1}}
	if got := transcript.lifecycleOf(1); !slices.Equal(got, wantFirst) {
		t.Errorf("first child's lifecycle = %#v\nwant %#v", got, wantFirst)
	}
	want := []ToolResult{
		{ToolUseID: "toolu_1", Content: "[agent 1]\ndone"}, {ToolUseID: "toolu_2", Content: "[agent 2]\ndone"},
		{ToolUseID: "toolu_3", Content: "[agent 3]\ndone"}, {ToolUseID: "toolu_4", Content: "[agent 4]\ndone"},
		{ToolUseID: "toolu_5", Content: "[agent 5]\ndone"}, {ToolUseID: "toolu_6", Content: "[agent 6]\ndone"},
	}
	if got := model.requests[7][len(model.requests[7])-1].ToolResults; !slices.Equal(got, want) {
		t.Errorf("tool results = %+v\nwant %+v", got, want)
	}
}

func TestCancellingTheParentEndsAWaitingChildWithoutStartingIt(t *testing.T) {
	// given
	// ... a response starting six children, whose model calls are held until they are cancelled
	model := &mockModel{replies: []mockReply{sixAgentCalls()}}
	gate := newChildGate()
	model.hold = gate.hold
	transcript := &mockTranscript{}
	sixthWaiting := transcript.watch(ChildWaiting{Child: 6, Model: parentModelName})
	a := newParent(model, transcript)
	ended := make(chan struct{})

	// when
	// ... the turn runs, and the user cancels it once five children are held and the sixth is waiting
	go func() {
		defer close(ended)
		a.RunTurn("go")
	}()
	reached := errors.Join(gate.awaitHeld(5), await(sixthWaiting))
	a.Cancel()
	finished := await(ended)

	// then
	// ... the sixth child went from waiting to cancelled and never called the model, and the turn is gone
	if reached != nil || finished != nil {
		t.Fatalf("five held and the sixth waiting: %v; turn ended: %v", reached, finished)
	}
	wantSixth := []Event{ChildWaiting{Child: 6, Model: parentModelName}, ChildCancelled{Child: 6}}
	if got := transcript.lifecycleOf(6); !slices.Equal(got, wantSixth) {
		t.Errorf("sixth child's lifecycle = %#v\nwant %#v", got, wantSixth)
	}
	if gate.calls != 5 {
		t.Errorf("children that called the model = %d, want 5", gate.calls)
	}
	if last := transcript.last(); last != (TurnDiscarded{Reason: "cancelled"}) {
		t.Errorf("last event = %#v", last)
	}
	if len(a.messages) != 0 {
		t.Errorf("len(messages) = %d, want 0", len(a.messages))
	}
}

func TestAnAgentCallStartsAFreshChildWithItsOwnPromptTaskAndTools(t *testing.T) {
	// given
	// ... a parent with its own tool, a subagent tool list and skills, whose model hands a task to a child
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_1", `{"task":"find the bug"}`)}, Content: "P1"}},
		{text: "found it", resp: ModelResponse{StopReason: StopEndTurn, Content: "C1"}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: "P2"}},
	}}
	a := newParent(model, &mockTranscript{}, &mockTool{name: "shell", run: returning("", nil)})
	a.Tools = []Tool{&mockTool{name: "list", run: returning("", nil)}}
	a.Skills = skillLibrary()

	// when
	// ... the parent's turn runs
	a.RunTurn("go")

	// then
	// ... the child's request has the parent's prompt with the subagent note, only the task, and the subagent tools
	child := model.calls[1]
	if child.System != "base prompt"+subagentNote {
		t.Errorf("system = %q", child.System)
	}
	if len(child.Messages) != 1 || child.Messages[0].Role != RoleUser || child.Messages[0].Text != "find the bug" {
		t.Errorf("messages = %+v", child.Messages)
	}
	if got := toolNames(child.Tools); !slices.Equal(got, []string{"shell", SkillToolName}) {
		t.Errorf("child tools = %v", got)
	}
	if got := toolNames(model.calls[0].Tools); !slices.Equal(got, []string{"list", SkillToolName, AgentToolName}) {
		t.Errorf("parent tools = %v", got)
	}
}

func TestTheAgentToolIsOfferedOnlyWithASubagentToolListAndTakesATaskAndANumber(t *testing.T) {
	// given
	// ... an agent with no subagent tool list and one with a list
	without := newAgent(&mockModel{}, &mockTranscript{})
	with := newParent(&mockModel{}, &mockTranscript{})

	// when
	// ... their tools are listed
	none, specs := without.toolSpecs(), with.toolSpecs()

	// then
	// ... only the second has the agent tool, whose parameters are task and agent
	if len(none) != 0 {
		t.Errorf("tools without a list = %v", none)
	}
	if len(specs) != 1 || specs[0].Name != AgentToolName {
		t.Fatalf("tools = %v", specs)
	}
	var schema struct {
		Properties map[string]any
		Required   []string
	}
	if err := json.Unmarshal([]byte(specs[0].InputSchema), &schema); err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(maps.Keys(schema.Properties)); !slices.Equal(got, []string{"agent", "task"}) {
		t.Errorf("properties = %v", got)
	}
	if !slices.Equal(schema.Required, []string{"task"}) {
		t.Errorf("required = %v", schema.Required)
	}
}

func TestTheParentReceivesTheChildsNumberAndItsFinalReply(t *testing.T) {
	// given
	// ... a child that says something, runs a tool and then gives its answer
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_1", `{"task":"find the bug"}`)}, Content: "P1"}},
		{text: "looking", resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_c1", "shell", `{}`)}, Content: "C1"}},
		{text: "found it", resp: ModelResponse{StopReason: StopEndTurn, Content: "C2"}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: "P2"}},
	}}
	a := newParent(model, &mockTranscript{}, &mockTool{name: "shell", run: returning("", nil)})

	// when
	// ... the parent's turn runs
	a.RunTurn("go")

	// then
	// ... the parent's call is answered with the child's number and only its last response's text
	want := ToolResult{ToolUseID: "toolu_1", Content: "[agent 1]\nfound it"}
	if got := lastToolResult(model.requests, 3); got != want {
		t.Errorf("tool result = %+v, want %+v", got, want)
	}
}

func TestAChildsLongReplyIsCutFromTheFrontBelowItsNumber(t *testing.T) {
	// given
	// ... a child whose reply is larger than the tool result cap
	big := strings.Repeat("x", maxToolResultBytes) + "\nANSWER"
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_1", `{"task":"find the bug"}`)}, Content: "P1"}},
		{text: big, resp: ModelResponse{StopReason: StopEndTurn, Content: "C1"}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: "P2"}},
	}}
	a := newParent(model, &mockTranscript{})

	// when
	// ... the parent's turn runs
	a.RunTurn("go")

	// then
	// ... the number is kept, the reply's start is cut and marked, and its end is kept
	content := lastToolResult(model.requests, 2).Content
	if !strings.HasPrefix(content, "[agent 1]\n[output truncated: first 7 bytes omitted]\nx") || !strings.HasSuffix(content, "\nANSWER") {
		t.Errorf("content = %q…%q", content[:60], content[len(content)-20:])
	}
}

func TestResumingAChildSendsTheTaskAsAFollowUpInItsHistory(t *testing.T) {
	// given
	// ... a parent that starts a child and then resumes it by number
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_1", `{"task":"find the bug"}`)}, Content: "P1"}},
		{text: "found it", resp: ModelResponse{StopReason: StopEndTurn, Content: "C1"}},
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_2", `{"agent":1,"task":"where?"}`)}, Content: "P2"}},
		{text: "line 9", resp: ModelResponse{StopReason: StopEndTurn, Content: "C2"}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: "P3"}},
	}}
	a := newParent(model, &mockTranscript{})

	// when
	// ... the parent's turn runs
	a.RunTurn("go")

	// then
	// ... the child's second request is its first turn followed by the new task, and its reply comes back under the same number
	want := []Message{{Role: RoleUser, Text: "find the bug"}, {Role: RoleAssistant, Content: "C1"}, {Role: RoleUser, Text: "where?"}}
	if got := model.requests[3]; !slices.EqualFunc(got, want, func(a, b Message) bool {
		return a.Role == b.Role && a.Text == b.Text && a.Content == b.Content && len(a.ToolResults) == 0
	}) {
		t.Errorf("child's second request = %+v\nwant %+v", got, want)
	}
	if got := lastToolResult(model.requests, 4).Content; got != "[agent 1]\nline 9" {
		t.Errorf("tool result = %q", got)
	}
}

func TestResumingANumberNoChildHasIsAnErrorResult(t *testing.T) {
	// given
	// ... a parent whose model resumes a child that was never started
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_1", `{"agent":7,"task":"where?"}`)}, Content: "P1"}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: "P2"}},
	}}
	a := newParent(model, &mockTranscript{})

	// when
	// ... the parent's turn runs
	a.RunTurn("go")

	// then
	// ... the model gets an error result and no child ran
	want := ToolResult{ToolUseID: "toolu_1", Content: "unknown agent 7", IsError: true}
	if got := lastToolResult(model.requests, 1); got != want {
		t.Errorf("tool result = %+v, want %+v", got, want)
	}
}

func TestResumingARunningChildIsAnErrorResult(t *testing.T) {
	// given
	// ... a response that starts a child and resumes that same child
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{
			agentCall("toolu_1", `{"task":"find the bug"}`), agentCall("toolu_2", `{"agent":1,"task":"where?"}`),
		}, Content: "P1"}},
		{text: "found it", resp: ModelResponse{StopReason: StopEndTurn, Content: "C1"}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: "P2"}},
	}}
	a := newParent(model, &mockTranscript{})

	// when
	// ... the parent's turn runs
	a.RunTurn("go")

	// then
	// ... the first call gets the child's reply and the second an error saying it is busy
	want := []ToolResult{
		{ToolUseID: "toolu_1", Content: "[agent 1]\nfound it"},
		{ToolUseID: "toolu_2", Content: "agent 1 is busy", IsError: true},
	}
	if got := model.requests[2][len(model.requests[2])-1].ToolResults; !slices.Equal(got, want) {
		t.Errorf("tool results = %+v\nwant %+v", got, want)
	}
}

func TestAChildsFailedTurnReturnsItsReasonAndLeavesItResumable(t *testing.T) {
	// given
	// ... a child whose model call fails after a tool call, and a parent that then resumes it
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_1", `{"task":"find the bug"}`)}, Content: "P1"}},
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_c1", "shell", `{}`)}, Content: "C1"}},
		{err: errors.New("HTTP 500: overloaded")},
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_2", `{"agent":1,"task":"try again"}`)}, Content: "P2"}},
		{text: "found it", resp: ModelResponse{StopReason: StopEndTurn, Content: "C2"}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: "P3"}},
	}}
	transcript := &mockTranscript{}
	a := newParent(model, transcript, &mockTool{name: "shell", run: returning("", nil)})

	// when
	// ... the parent's turn runs
	a.RunTurn("go")

	// then
	// ... the parent got the reason as an error result, and the resumed child started from an empty history
	want := ToolResult{ToolUseID: "toolu_1", Content: "[agent 1]\nHTTP 500: overloaded", IsError: true}
	if got := lastToolResult(model.requests, 3); got != want {
		t.Errorf("tool result = %+v, want %+v", got, want)
	}
	if !slices.Contains(transcript.lifecycle(), Event(ChildFailed{Child: 1, Reason: "HTTP 500: overloaded"})) {
		t.Errorf("lifecycle = %#v", transcript.lifecycle())
	}
	if got := model.requests[4]; len(got) != 1 || got[0].Text != "try again" {
		t.Errorf("resumed child's request = %+v", got)
	}
}

func TestCancellingTheParentsTurnCancelsItsRunningChildAndDiscardsTheTurn(t *testing.T) {
	// given
	// ... a child running a tool until its context is cancelled, and the user cancelling the parent meanwhile
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_1", `{"task":"find the bug"}`)}, Content: "P1"}},
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_c1", "shell", `{}`)}, Content: "C1"}},
	}}
	transcript := &mockTranscript{}
	a := newParent(model, transcript)
	stopped := make(chan error, 1)
	a.SubagentTools = []Tool{&mockTool{name: "shell", run: func(ctx context.Context) (string, error) {
		a.Cancel()
		stopped <- await(ctx.Done())
		return "", ctx.Err()
	}}}

	// when
	// ... the parent's turn runs
	a.RunTurn("go")

	// then
	// ... the child's tool saw the cancellation, the child ended cancelled, and both turns are gone
	if err := <-stopped; err != nil {
		t.Errorf("the child's tool: %v, want cancelled", err)
	}
	wantLifecycle := []Event{ChildStarted{Child: 1, Model: parentModelName}, ChildProgress{Child: 1, Tools: 1}, ChildCancelled{Child: 1}}
	if got := transcript.lifecycle(); !slices.Equal(got, wantLifecycle) {
		t.Errorf("lifecycle = %#v\nwant %#v", got, wantLifecycle)
	}
	if last := transcript.last(); last != (TurnDiscarded{Reason: "cancelled"}) || transcript.agents[len(transcript.agents)-1] != 0 {
		t.Errorf("last event = %#v", last)
	}
	if len(model.requests) != 2 {
		t.Errorf("model called %d times", len(model.requests))
	}
	if len(a.messages) != 0 || len(a.children[0].messages) != 0 {
		t.Errorf("messages kept: parent %d, child %d", len(a.messages), len(a.children[0].messages))
	}
}

func TestAChildsEventsReportItsNumberBetweenItsStartAndEnd(t *testing.T) {
	// given
	// ... a child that runs a tool and then answers
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_1", `{"task":"find the bug"}`)}, Content: "P1"}},
		{text: "looking", resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_c1", "shell", `{}`)}, Content: "C1"}},
		{text: "found it", resp: ModelResponse{StopReason: StopEndTurn, Content: "C2"}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: "P2"}},
	}}
	transcript := &mockTranscript{}
	a := newParent(model, transcript, &mockTool{name: "shell", run: returning("ok", nil)})

	// when
	// ... the parent's turn runs
	a.RunTurn("go")

	// then
	// ... the child's turn events carry its number, inside lifecycle events shown as the parent's
	want := []Event{
		ReplyStarted{},
		ToolStarted{ID: "toolu_1", Name: AgentToolName, Summary: `agent {"task":"find the bug"}`},
		ChildStarted{Child: 1, Model: parentModelName},
		ReplyStarted{},
		ReplyText{Text: "looking"},
		ToolStarted{ID: "toolu_c1", Name: "shell", Summary: "shell {}"},
		ChildProgress{Child: 1, Tools: 1},
		ToolFinished{ID: "toolu_c1", Output: "ok"},
		ReplyStarted{},
		ReplyText{Text: "found it"},
		ChildFinished{Child: 1},
		ToolFinished{ID: "toolu_1", Output: "[agent 1]\nfound it"},
		ReplyStarted{},
	}
	wantAgents := []int{0, 0, 0, 1, 1, 1, 0, 1, 1, 1, 0, 0, 0}
	if !slices.Equal(transcript.events, want) {
		t.Errorf("events = %#v\nwant %#v", transcript.events, want)
	}
	if !slices.Equal(transcript.agents, wantAgents) {
		t.Errorf("agents = %v\nwant %v", transcript.agents, wantAgents)
	}
}

func TestAResumedChildsTurnHasItsOwnLifecycleCountingToolsFromZero(t *testing.T) {
	// given
	// ... a child that runs a tool in its first turn and another when resumed
	shellCall := func(id string) []ToolCall { return []ToolCall{toolCall(id, "shell", `{}`)} }
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_1", `{"task":"find the bug"}`)}, Content: "P1"}},
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: shellCall("toolu_c1"), Content: "C1"}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: "C2"}},
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_2", `{"agent":1,"task":"where?"}`)}, Content: "P2"}},
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: shellCall("toolu_c2"), Content: "C3"}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: "C4"}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: "P3"}},
	}}
	transcript := &mockTranscript{}
	a := newParent(model, transcript, &mockTool{name: "shell", run: returning("", nil)})

	// when
	// ... the parent's turn runs
	a.RunTurn("go")

	// then
	// ... each of the child's turns starts, counts its one tool call and finishes
	turn := []Event{ChildStarted{Child: 1, Model: parentModelName}, ChildProgress{Child: 1, Tools: 1}, ChildFinished{Child: 1}}
	if got, want := transcript.lifecycle(), slices.Concat(turn, turn); !slices.Equal(got, want) {
		t.Errorf("lifecycle = %#v\nwant %#v", got, want)
	}
}

func TestChildrenStartedInOneResponseAreNumberedInCallOrder(t *testing.T) {
	// given
	// ... a response that hands out two tasks at once, to children that each answer the same
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{
			agentCall("toolu_1", `{"task":"first"}`), agentCall("toolu_2", `{"task":"second"}`),
		}, Content: "P1"}},
		{text: "done", resp: ModelResponse{StopReason: StopEndTurn, Content: "C"}},
		{text: "done", resp: ModelResponse{StopReason: StopEndTurn, Content: "C"}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: "P2"}},
	}}
	a := newParent(model, &mockTranscript{})

	// when
	// ... the parent's turn runs
	a.RunTurn("go")

	// then
	// ... the first call's child is 1 and the second's is 2, whichever ran first
	want := []ToolResult{{ToolUseID: "toolu_1", Content: "[agent 1]\ndone"}, {ToolUseID: "toolu_2", Content: "[agent 2]\ndone"}}
	if got := model.requests[3][len(model.requests[3])-1].ToolResults; !slices.Equal(got, want) {
		t.Errorf("tool results = %+v\nwant %+v", got, want)
	}
	if first, second := a.children[0].messages[0].Text, a.children[1].messages[0].Text; first != "first" || second != "second" {
		t.Errorf("tasks = %q, %q", first, second)
	}
}
