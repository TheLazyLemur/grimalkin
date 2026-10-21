package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
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
	// isChild picks the calls to hold: a fresh child's, unless it is set.
	isChild func(ModelRequest) bool

	mu            sync.Mutex
	holding, peak int
	calls         int
}

func newChildGate() *childGate {
	return &childGate{
		held: make(chan struct{}, 100), open: make(chan struct{}),
		isChild: func(req ModelRequest) bool { return strings.HasSuffix(req.System, subagentNote) },
	}
}

func (g *childGate) hold(ctx context.Context, req ModelRequest) error {
	if !g.isChild(req) {
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
	return mockReply{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: calls, Content: raw("P1")}}
}

func TestASixthChildWaitsForOneOfFiveSlotsAndResultsComeBackInCallOrder(t *testing.T) {
	// given
	// ... a response starting six children, whose model calls are held until the test opens the gate
	done := mockReply{text: "done", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C")}}
	model := &mockModel{replies: []mockReply{
		sixAgentCalls(), done, done, done, done, done, done,
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P2")}},
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
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_1", `{"task":"find the bug"}`)}, Content: raw("P1")}},
		{text: "found it", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C1")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P2")}},
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

func TestTheAgentToolIsOfferedOnlyWithASubagentToolListAndTakesATaskANumberAndFork(t *testing.T) {
	// given
	// ... an agent with no subagent tool list and one with a list
	without := newAgent(&mockModel{}, &mockTranscript{})
	with := newParent(&mockModel{}, &mockTranscript{})

	// when
	// ... their tools are listed
	none, specs := without.toolSpecs(), with.toolSpecs()

	// then
	// ... only the second has the agent tool, whose parameters are task, agent and fork
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
	if got := slices.Sorted(maps.Keys(schema.Properties)); !slices.Equal(got, []string{"agent", "fork", "task"}) {
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
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_1", `{"task":"find the bug"}`)}, Content: raw("P1")}},
		{text: "looking", resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_c1", "shell", `{}`)}, Content: raw("C1")}},
		{text: "found it", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C2")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P2")}},
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
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_1", `{"task":"find the bug"}`)}, Content: raw("P1")}},
		{text: big, resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C1")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P2")}},
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
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_1", `{"task":"find the bug"}`)}, Content: raw("P1")}},
		{text: "found it", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C1")}},
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_2", `{"agent":1,"task":"where?"}`)}, Content: raw("P2")}},
		{text: "line 9", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C2")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P3")}},
	}}
	a := newParent(model, &mockTranscript{})

	// when
	// ... the parent's turn runs
	a.RunTurn("go")

	// then
	// ... the child's second request is its first turn followed by the new task, and its reply comes back under the same number
	want := []Message{{Role: RoleUser, Text: "find the bug"}, {Role: RoleAssistant, Content: raw("C1")}, {Role: RoleUser, Text: "where?"}}
	if got := model.requests[3]; !slices.EqualFunc(got, want, func(a, b Message) bool {
		return a.Role == b.Role && a.Text == b.Text && string(a.Content) == string(b.Content) && len(a.ToolResults) == 0
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
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_1", `{"agent":7,"task":"where?"}`)}, Content: raw("P1")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P2")}},
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
		}, Content: raw("P1")}},
		{text: "found it", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C1")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P2")}},
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
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_1", `{"task":"find the bug"}`)}, Content: raw("P1")}},
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_c1", "shell", `{}`)}, Content: raw("C1")}},
		{err: errors.New("HTTP 500: overloaded")},
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_2", `{"agent":1,"task":"try again"}`)}, Content: raw("P2")}},
		{text: "found it", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C2")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P3")}},
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
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_1", `{"task":"find the bug"}`)}, Content: raw("P1")}},
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_c1", "shell", `{}`)}, Content: raw("C1")}},
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
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_1", `{"task":"find the bug"}`)}, Content: raw("P1")}},
		{text: "looking", resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_c1", "shell", `{}`)}, Content: raw("C1")}},
		{text: "found it", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C2")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P2")}},
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
		ToolStarted{ID: "toolu_1", Name: AgentToolName, Summary: `agent {"task":"find the bug"}`, Input: json.RawMessage(`{"task":"find the bug"}`)},
		ChildStarted{Child: 1, Model: parentModelName},
		ReplyStarted{},
		ReplyText{Text: "looking"},
		ToolStarted{ID: "toolu_c1", Name: "shell", Summary: "shell {}", Input: json.RawMessage(`{}`)},
		ChildProgress{Child: 1, Tools: 1},
		ToolFinished{ID: "toolu_c1", Output: "ok"},
		ReplyStarted{},
		ReplyText{Text: "found it"},
		ChildFinished{Child: 1},
		ToolFinished{ID: "toolu_1", Output: "[agent 1]\nfound it"},
		ReplyStarted{},
	}
	wantAgents := []int{0, 0, 0, 1, 1, 1, 0, 1, 1, 1, 0, 0, 0}
	if !reflect.DeepEqual(transcript.events, want) {
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
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_1", `{"task":"find the bug"}`)}, Content: raw("P1")}},
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: shellCall("toolu_c1"), Content: raw("C1")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C2")}},
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_2", `{"agent":1,"task":"where?"}`)}, Content: raw("P2")}},
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: shellCall("toolu_c2"), Content: raw("C3")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C4")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P3")}},
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
		}, Content: raw("P1")}},
		{text: "done", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C")}},
		{text: "done", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P2")}},
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

// eventsOf returns the events shown as agent's own.
func (m *mockTranscript) eventsOf(agent int) []Event {
	var events []Event
	for i, event := range m.events {
		if m.agents[i] == agent {
			events = append(events, event)
		}
	}
	return events
}

func TestCancellingOneChildReturnsAnErrorResultWhileTheParentAndTheOtherChildGoOn(t *testing.T) {
	// given
	// ... a response starting two children, whose model calls are held until the gate opens or they are cancelled
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{
			agentCall("toolu_1", `{"task":"first"}`), agentCall("toolu_2", `{"task":"second"}`),
		}, Content: raw("P1")}},
		{text: "done", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P2")}},
	}}
	gate := newChildGate()
	model.hold = gate.hold
	transcript := &mockTranscript{}
	firstCancelled := transcript.watch(ChildCancelled{Child: 1})
	a := newParent(model, transcript)
	ended := make(chan struct{})

	// when
	// ... the turn runs, the user cancels the first child once both are held, and the gate opens once it has ended
	go func() {
		defer close(ended)
		a.RunTurn("go")
	}()
	held := gate.awaitHeld(2)
	a.CancelChild(1)
	cancelled := await(firstCancelled)
	close(gate.open)
	finished := await(ended)

	// then
	// ... the parent's next request carries the first child's cancellation as an error and the second's reply, and its turn is kept
	if held != nil || cancelled != nil || finished != nil {
		t.Fatalf("both held: %v; first cancelled: %v; turn ended: %v", held, cancelled, finished)
	}
	want := []ToolResult{
		{ToolUseID: "toolu_1", Content: "[agent 1]\ncancelled by user", IsError: true},
		{ToolUseID: "toolu_2", Content: "[agent 2]\ndone"},
	}
	if got := model.requests[2][len(model.requests[2])-1].ToolResults; !slices.Equal(got, want) {
		t.Errorf("tool results = %+v\nwant %+v", got, want)
	}
	if len(a.messages) != 4 {
		t.Errorf("len(parent's messages) = %d, want 4", len(a.messages))
	}
	// ... the first child's transcript shows its turn discarded, the parent's shows it cancelled, and its history is empty
	wantChild := []Event{ReplyStarted{}, TurnDiscarded{Reason: "cancelled"}}
	if got := transcript.eventsOf(1); !slices.Equal(got, wantChild) {
		t.Errorf("first child's events = %#v\nwant %#v", got, wantChild)
	}
	wantLifecycle := []Event{ChildStarted{Child: 1, Model: parentModelName}, ChildCancelled{Child: 1}}
	if got := transcript.lifecycleOf(1); !slices.Equal(got, wantLifecycle) {
		t.Errorf("first child's lifecycle = %#v\nwant %#v", got, wantLifecycle)
	}
	if len(a.children[0].messages) != 0 {
		t.Errorf("len(first child's messages) = %d, want 0", len(a.children[0].messages))
	}
}

func TestResumingACancelledChildContinuesFromItsHistoryBeforeTheCancelledTurn(t *testing.T) {
	// given
	// ... a child that finishes one turn, is cancelled by the user during a tool call in its second, and is resumed again
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_1", `{"task":"find the bug"}`)}, Content: raw("P1")}},
		{text: "found it", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C1")}},
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_2", `{"agent":1,"task":"where?"}`)}, Content: raw("P2")}},
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_c1", "shell", `{}`)}, Content: raw("C2")}},
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_3", `{"agent":1,"task":"again"}`)}, Content: raw("P3")}},
		{text: "line 9", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C3")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P4")}},
	}}
	a := newParent(model, &mockTranscript{})
	stopped := make(chan error, 1)
	a.SubagentTools = []Tool{&mockTool{name: "shell", run: func(ctx context.Context) (string, error) {
		a.CancelChild(1)
		stopped <- await(ctx.Done())
		return "", ctx.Err()
	}}}

	// when
	// ... the parent's turn runs
	a.RunTurn("go")

	// then
	// ... the child's tool saw the cancellation, the parent was told, and the child's third request is its first turn and the new follow-up
	if err := <-stopped; err != nil {
		t.Errorf("the child's tool: %v, want cancelled", err)
	}
	wantResult := ToolResult{ToolUseID: "toolu_2", Content: "[agent 1]\ncancelled by user", IsError: true}
	if got := lastToolResult(model.requests, 4); got != wantResult {
		t.Errorf("tool result = %+v, want %+v", got, wantResult)
	}
	want := []Message{{Role: RoleUser, Text: "find the bug"}, {Role: RoleAssistant, Content: raw("C1")}, {Role: RoleUser, Text: "again"}}
	if got := model.requests[5]; !slices.EqualFunc(got, want, func(a, b Message) bool {
		return a.Role == b.Role && a.Text == b.Text && string(a.Content) == string(b.Content) && len(a.ToolResults) == 0
	}) {
		t.Errorf("child's request after the cancelled turn = %+v\nwant %+v", got, want)
	}
	if got := lastToolResult(model.requests, 6).Content; got != "[agent 1]\nline 9" {
		t.Errorf("tool result = %q", got)
	}
}

func TestCancellingAChildWaitingForASlotReturnsAnErrorResultWithoutStartingIt(t *testing.T) {
	// given
	// ... a response starting six children, whose model calls are held until the test opens the gate
	done := mockReply{text: "done", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C")}}
	model := &mockModel{replies: []mockReply{
		sixAgentCalls(), done, done, done, done, done,
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P2")}},
	}}
	gate := newChildGate()
	model.hold = gate.hold
	transcript := &mockTranscript{}
	sixthWaiting := transcript.watch(ChildWaiting{Child: 6, Model: parentModelName})
	a := newParent(model, transcript)
	ended := make(chan struct{})

	// when
	// ... the turn runs, the user cancels the sixth child while it waits, and the gate opens once it has ended
	go func() {
		defer close(ended)
		a.RunTurn("go")
	}()
	reached := errors.Join(gate.awaitHeld(5), await(sixthWaiting))
	sixthCancelled := transcript.watch(ChildCancelled{Child: 6})
	a.CancelChild(6)
	cancelled := await(sixthCancelled)
	close(gate.open)
	finished := await(ended)

	// then
	// ... the sixth child went from waiting to cancelled without calling the model, and the parent got the error beside five replies
	if reached != nil || cancelled != nil || finished != nil {
		t.Fatalf("five held and the sixth waiting: %v; sixth cancelled: %v; turn ended: %v", reached, cancelled, finished)
	}
	wantSixth := []Event{ChildWaiting{Child: 6, Model: parentModelName}, ChildCancelled{Child: 6}}
	if got := transcript.lifecycleOf(6); !slices.Equal(got, wantSixth) {
		t.Errorf("sixth child's lifecycle = %#v\nwant %#v", got, wantSixth)
	}
	if gate.calls != 5 {
		t.Errorf("children that called the model = %d, want 5", gate.calls)
	}
	want := []ToolResult{
		{ToolUseID: "toolu_1", Content: "[agent 1]\ndone"}, {ToolUseID: "toolu_2", Content: "[agent 2]\ndone"},
		{ToolUseID: "toolu_3", Content: "[agent 3]\ndone"}, {ToolUseID: "toolu_4", Content: "[agent 4]\ndone"},
		{ToolUseID: "toolu_5", Content: "[agent 5]\ndone"},
		{ToolUseID: "toolu_6", Content: "[agent 6]\ncancelled by user", IsError: true},
	}
	if got := model.requests[6][len(model.requests[6])-1].ToolResults; !slices.Equal(got, want) {
		t.Errorf("tool results = %+v\nwant %+v", got, want)
	}
}

func TestCancellingAChildThatIsNotRunningOrANumberNoChildHasDoesNothing(t *testing.T) {
	// given
	// ... a parent whose one child has finished its turn
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_1", `{"task":"find the bug"}`)}, Content: raw("P1")}},
		{text: "found it", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C1")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P2")}},
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_2", `{"agent":1,"task":"where?"}`)}, Content: raw("P3")}},
		{text: "line 9", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C2")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P4")}},
	}}
	transcript := &mockTranscript{}
	a := newParent(model, transcript)
	a.RunTurn("go")
	shown := len(transcript.events)

	// when
	// ... the user cancels that child, the parent's own number and a number no child has, and the child is then resumed
	a.CancelChild(1)
	a.CancelChild(0)
	a.CancelChild(7)
	shownAfter := len(transcript.events)
	a.RunTurn("ask again")

	// then
	// ... nothing was shown for the cancels, the child kept its history and its next turn ran to its reply
	if shownAfter != shown {
		t.Errorf("events shown by the cancels: %#v", transcript.events[shown:shownAfter])
	}
	if got := lastToolResult(model.requests, 5); got != (ToolResult{ToolUseID: "toolu_2", Content: "[agent 1]\nline 9"}) {
		t.Errorf("tool result = %+v", got)
	}
	if got := len(model.requests[4]); got != 3 {
		t.Errorf("len(resumed child's request) = %d, want 3", got)
	}
}

// startOneChild scripts a parent turn that starts child 1, which answers
// "found it" and leaves two messages in its history.
func startOneChild() []mockReply {
	return []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_1", `{"task":"find the bug"}`)}, Content: raw("P1")}},
		{text: "found it", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C1")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P2")}},
	}
}

func sameMessages(got, want []Message) bool {
	return slices.EqualFunc(got, want, func(a, b Message) bool {
		return a.Role == b.Role && a.Text == b.Text && string(a.Content) == string(b.Content) && len(a.ToolResults) == 0
	})
}

func TestAUserTurnOnAFinishedChildFollowsItsHistoryUnderItsNumberWithItsOwnStatusLine(t *testing.T) {
	// given
	// ... a parent whose child has finished, and a model that answers the user's follow-up after one tool call
	model := &mockModel{replies: append(startOneChild(),
		mockReply{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_c1", "shell", `{}`)}, Content: raw("C2")}},
		mockReply{text: "line 9", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C3")}},
	)}
	transcript := &mockTranscript{}
	a := newParent(model, transcript, &mockTool{name: "shell", run: returning("ok", nil)})
	a.Tools = []Tool{&mockTool{name: "list", run: returning("", nil)}}
	a.RunTurn("go")
	shown := len(transcript.events)

	// when
	// ... the user runs a turn on the child
	run, err := a.StartChildTurn(1, "where?")
	run()

	// then
	// ... the child's request is its history and the follow-up, with its own prompt and tools
	if err != nil {
		t.Fatalf("StartChildTurn: %v", err)
	}
	request := model.calls[3]
	want := []Message{{Role: RoleUser, Text: "find the bug"}, {Role: RoleAssistant, Content: raw("C1")}, {Role: RoleUser, Text: "where?"}}
	if !sameMessages(model.requests[3], want) {
		t.Errorf("child's request = %+v\nwant %+v", model.requests[3], want)
	}
	if request.System != "base prompt"+subagentNote {
		t.Errorf("system = %q", request.System)
	}
	if got := toolNames(request.Tools); !slices.Equal(got, []string{"shell"}) {
		t.Errorf("child tools = %v", got)
	}
	// ... the turn's events are the child's, and its lifecycle is shown as the parent's
	wantEvents := []Event{
		ChildStarted{Child: 1, Model: parentModelName},
		ReplyStarted{},
		ToolStarted{ID: "toolu_c1", Name: "shell", Summary: "shell {}", Input: json.RawMessage(`{}`)},
		ChildProgress{Child: 1, Tools: 1},
		ToolFinished{ID: "toolu_c1", Output: "ok"},
		ReplyStarted{},
		ReplyText{Text: "line 9"},
		ChildFinished{Child: 1},
	}
	wantAgents := []int{0, 1, 1, 0, 1, 1, 1, 0}
	if got := transcript.events[shown:]; !reflect.DeepEqual(got, wantEvents) {
		t.Errorf("events = %#v\nwant %#v", got, wantEvents)
	}
	if got := transcript.agents[shown:]; !slices.Equal(got, wantAgents) {
		t.Errorf("agents = %v\nwant %v", got, wantAgents)
	}
}

func TestTheParentResumingAChildSendsTheUsersTurnOnItInItsHistory(t *testing.T) {
	// given
	// ... a child the user ran a follow-up turn on after it finished
	model := &mockModel{replies: append(startOneChild(),
		mockReply{text: "line 9", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C2")}},
		mockReply{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_2", `{"agent":1,"task":"fix it"}`)}, Content: raw("P3")}},
		mockReply{text: "fixed", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C3")}},
		mockReply{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P4")}},
	)}
	a := newParent(model, &mockTranscript{})
	a.RunTurn("go")
	run, err := a.StartChildTurn(1, "where?")
	run()

	// when
	// ... the parent resumes that child
	a.RunTurn("carry on")

	// then
	// ... the child's request holds the user's follow-up and its reply before the parent's task
	if err != nil {
		t.Fatalf("StartChildTurn: %v", err)
	}
	want := []Message{
		{Role: RoleUser, Text: "find the bug"}, {Role: RoleAssistant, Content: raw("C1")},
		{Role: RoleUser, Text: "where?"}, {Role: RoleAssistant, Content: raw("C2")},
		{Role: RoleUser, Text: "fix it"},
	}
	if !sameMessages(model.requests[5], want) {
		t.Errorf("resumed child's request = %+v\nwant %+v", model.requests[5], want)
	}
}

func TestAUserTurnDoesNotStartOnAChildTheParentIsRunningOrANumberNoChildHas(t *testing.T) {
	// given
	// ... a child started by the parent, whose model call is held until the gate opens
	model := &mockModel{replies: startOneChild()}
	gate := newChildGate()
	model.hold = gate.hold
	transcript := &mockTranscript{}
	a := newParent(model, transcript)
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		a.RunTurn("go")
	}()
	held := gate.awaitHeld(1)
	shown := len(transcript.eventsNow())

	// when
	// ... the user starts a turn on that child and on a number no child has
	runBusy, busy := a.StartChildTurn(1, "where?")
	runUnknown, unknown := a.StartChildTurn(7, "where?")
	shownAfter := len(transcript.eventsNow())
	close(gate.open)
	finished := await(ended)

	// then
	// ... neither starts, each says why, and the child's own turn is untouched
	if held != nil || finished != nil {
		t.Fatalf("child held: %v; turn ended: %v", held, finished)
	}
	if runBusy != nil || busy == nil || busy.Error() != "agent 1 is busy" {
		t.Errorf("running child: run set %t, err %v", runBusy != nil, busy)
	}
	if runUnknown != nil || unknown == nil || unknown.Error() != "unknown agent 7" {
		t.Errorf("unknown number: run set %t, err %v", runUnknown != nil, unknown)
	}
	if shownAfter != shown {
		t.Errorf("events shown by the refused turns: %d", shownAfter-shown)
	}
	if got := lastToolResult(model.requests, 2).Content; got != "[agent 1]\nfound it" {
		t.Errorf("tool result = %q", got)
	}
}

func TestWhileAUserTurnRunsOnAChildNeitherTheUserNorTheParentCanStartAnother(t *testing.T) {
	// given
	// ... a finished child the user runs a turn on, whose model call is held until the gate opens
	model := &mockModel{replies: append(startOneChild(),
		mockReply{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_2", `{"agent":1,"task":"fix it"}`)}, Content: raw("P3")}},
		mockReply{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P4")}},
		mockReply{text: "line 9", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C2")}},
	)}
	a := newParent(model, &mockTranscript{})
	a.RunTurn("go")
	gate := newChildGate()
	model.hold = gate.hold
	run, err := a.StartChildTurn(1, "where?")
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		run()
	}()
	held := gate.awaitHeld(1)

	// when
	// ... the user starts a second turn on it and the parent's model resumes it
	second, busy := a.StartChildTurn(1, "and then?")
	a.RunTurn("carry on")
	close(gate.open)
	finished := await(ended)

	// then
	// ... the second turn does not start, the parent's call is answered busy, and the first turn runs to its end
	if err != nil || held != nil || finished != nil {
		t.Fatalf("StartChildTurn: %v; turn held: %v; turn ended: %v", err, held, finished)
	}
	if second != nil || busy == nil || busy.Error() != "agent 1 is busy" {
		t.Errorf("second user turn: run set %t, err %v", second != nil, busy)
	}
	want := ToolResult{ToolUseID: "toolu_2", Content: "agent 1 is busy", IsError: true}
	if got := lastToolResult(model.requests, 4); got != want {
		t.Errorf("tool result = %+v, want %+v", got, want)
	}
	if got := len(a.children[0].messages); got != 4 {
		t.Errorf("len(child's messages) = %d, want 4", got)
	}
}

func TestCancellingAChildDuringAUserTurnDiscardsThatTurnOnly(t *testing.T) {
	// given
	// ... a finished child whose next turn calls a tool, during which the user cancels the child
	model := &mockModel{replies: append(startOneChild(),
		mockReply{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_c1", "shell", `{}`)}, Content: raw("C2")}},
	)}
	transcript := &mockTranscript{}
	a := newParent(model, transcript)
	stopped := make(chan error, 1)
	a.SubagentTools = []Tool{&mockTool{name: "shell", run: func(ctx context.Context) (string, error) {
		a.CancelChild(1)
		stopped <- await(ctx.Done())
		return "", ctx.Err()
	}}}
	a.RunTurn("go")

	// when
	// ... the user runs a turn on the child
	run, err := a.StartChildTurn(1, "where?")
	run()

	// then
	// ... the tool saw the cancellation, the turn is discarded in the child's transcript, and its history is its first turn
	if err != nil {
		t.Fatalf("StartChildTurn: %v", err)
	}
	if err := <-stopped; err != nil {
		t.Errorf("the child's tool: %v, want cancelled", err)
	}
	events := transcript.eventsOf(1)
	if last := events[len(events)-1]; last != (TurnDiscarded{Reason: "cancelled"}) {
		t.Errorf("child's last event = %#v", last)
	}
	if last := transcript.last(); last != (ChildCancelled{Child: 1}) {
		t.Errorf("last event = %#v", last)
	}
	want := []Message{{Role: RoleUser, Text: "find the bug"}, {Role: RoleAssistant, Content: raw("C1")}}
	if got := a.children[0].messages; !sameMessages(got, want) {
		t.Errorf("child's messages = %+v\nwant %+v", got, want)
	}
}

func TestAUserTurnThatFailsIsDiscardedWithItsReasonInTheChildsTranscript(t *testing.T) {
	// given
	// ... a finished child whose model fails on the user's follow-up
	model := &mockModel{replies: append(startOneChild(), mockReply{err: errors.New("HTTP 500: overloaded")})}
	transcript := &mockTranscript{}
	a := newParent(model, transcript)
	a.RunTurn("go")

	// when
	// ... the user runs a turn on the child
	run, err := a.StartChildTurn(1, "where?")
	run()

	// then
	// ... the reason is shown as the child's, the parent's transcript marks it failed, and its history is its first turn
	if err != nil {
		t.Fatalf("StartChildTurn: %v", err)
	}
	events := transcript.eventsOf(1)
	if last := events[len(events)-1]; last != (TurnDiscarded{Reason: "HTTP 500: overloaded"}) {
		t.Errorf("child's last event = %#v", last)
	}
	if last := transcript.last(); last != (ChildFailed{Child: 1, Reason: "HTTP 500: overloaded"}) {
		t.Errorf("last event = %#v", last)
	}
	want := []Message{{Role: RoleUser, Text: "find the bug"}, {Role: RoleAssistant, Content: raw("C1")}}
	if got := a.children[0].messages; !sameMessages(got, want) {
		t.Errorf("child's messages = %+v\nwant %+v", got, want)
	}
}

func TestAUserTurnWaitsForASlotWhileFiveChildrenAreRunning(t *testing.T) {
	// given
	// ... a finished child, and five more the parent starts, whose model calls are held until the gate opens
	done := mockReply{text: "done", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C")}}
	five := sixAgentCalls()
	five.resp.ToolCalls = five.resp.ToolCalls[:5]
	model := &mockModel{replies: append(startOneChild(), five, done, done, done, done, done, done, done)}
	transcript := &mockTranscript{}
	a := newParent(model, transcript)
	a.RunTurn("go")
	gate := newChildGate()
	model.hold = gate.hold
	parentEnded, userEnded := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(parentEnded)
		a.RunTurn("fan out")
	}()
	held := gate.awaitHeld(5)
	firstWaiting := transcript.watch(ChildWaiting{Child: 1, Model: parentModelName})
	before := len(transcript.lifecycleNow(1))

	// when
	// ... the user runs a turn on the finished child, and the gate opens once it is waiting
	run, err := a.StartChildTurn(1, "where?")
	go func() {
		defer close(userEnded)
		run()
	}()
	waiting := await(firstWaiting)
	close(gate.open)
	finished := errors.Join(await(parentEnded), await(userEnded))

	// then
	// ... the user's turn waited, then started and finished, and never more than five children ran at once
	if err != nil || held != nil || waiting != nil || finished != nil {
		t.Fatalf("StartChildTurn: %v; five held: %v; user turn waiting: %v; turns ended: %v", err, held, waiting, finished)
	}
	want := []Event{
		ChildWaiting{Child: 1, Model: parentModelName}, ChildStarted{Child: 1, Model: parentModelName}, ChildFinished{Child: 1},
	}
	if got := transcript.lifecycleOf(1)[before:]; !slices.Equal(got, want) {
		t.Errorf("user turn's lifecycle = %#v\nwant %#v", got, want)
	}
	if gate.peak != 5 {
		t.Errorf("most children running at once = %d, want 5", gate.peak)
	}
}

// eventsNow returns the events shown so far, while a turn may be running.
func (m *mockTranscript) eventsNow() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.events)
}

// lifecycleNow is lifecycleOf while a turn may be running.
func (m *mockTranscript) lifecycleNow(child int) []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lifecycleOf(child)
}

const smallModelName = "other/small"

// withSmallModel lets a's children run on small, as smallModelName.
func withSmallModel(a *Agent, small *mockModel) *Agent {
	a.SubagentModels = []SubagentModel{{Name: smallModelName, Model: small}}
	return a
}

func TestWithAllowedModelsTheAgentToolOffersTheParentsModelThenThoseInWiredOrder(t *testing.T) {
	// given
	// ... a parent wired with two allowed subagent models
	model := &mockModel{replies: []mockReply{{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P1")}}}}
	a := newParent(model, &mockTranscript{})
	a.SubagentModels = []SubagentModel{{Name: "zeta/scout", Model: &mockModel{}}, {Name: "alpha/big", Model: &mockModel{}}}

	// when
	// ... a turn runs
	a.RunTurn("go")

	// then
	// ... the agent tool sent to the model has a model parameter whose enum is the parent's model and the two wired, in order
	specs := model.calls[0].Tools
	var schema struct {
		Properties struct {
			Model struct{ Enum []string }
		}
		Required []string
	}
	if err := json.Unmarshal([]byte(specs[len(specs)-1].InputSchema), &schema); err != nil {
		t.Fatal(err)
	}
	if got, want := schema.Properties.Model.Enum, []string{parentModelName, "zeta/scout", "alpha/big"}; !slices.Equal(got, want) {
		t.Errorf("model enum = %v, want %v", got, want)
	}
	if !slices.Equal(schema.Required, []string{"task"}) {
		t.Errorf("required = %v", schema.Required)
	}
}

func TestAChildStartedOnAnotherModelKeepsItWhenTheParentOrTheUserResumesIt(t *testing.T) {
	// given
	// ... a parent that starts a child on an allowed model and later resumes it by number
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_1", `{"task":"find the bug","model":"other/small"}`)}, Content: raw("P1")}},
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_2", `{"agent":1,"task":"where?"}`)}, Content: raw("P2")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P3")}},
	}}
	small := &mockModel{replies: []mockReply{
		{text: "found it", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C1")}},
		{text: "line 9", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C2")}},
		{text: "yes", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C3")}},
	}}
	transcript := &mockTranscript{}
	a := withSmallModel(newParent(model, transcript), small)

	// when
	// ... the parent's turn runs, and the user then runs a turn on the child
	a.RunTurn("go")
	run, err := a.StartChildTurn(1, "sure?")
	run()

	// then
	// ... all three of the child's requests went to the allowed model, and the parent's model got only the parent's
	if err != nil {
		t.Fatalf("StartChildTurn: %v", err)
	}
	if len(small.calls) != 3 || len(model.calls) != 3 {
		t.Fatalf("requests: allowed model %d, parent's model %d; want 3 and 3", len(small.calls), len(model.calls))
	}
	if got := small.calls[0]; got.System != "base prompt"+subagentNote || got.Messages[0].Text != "find the bug" {
		t.Errorf("child's first request = %+v", got)
	}
	if got := len(small.requests[2]); got != 5 {
		t.Errorf("len(child's third request) = %d, want 5", got)
	}
	if got := lastToolResult(model.requests, 2).Content; got != "[agent 1]\nline 9" {
		t.Errorf("tool result = %q", got)
	}
	// ... and each of its turns is shown starting on that model
	turn := []Event{ChildStarted{Child: 1, Model: smallModelName}, ChildFinished{Child: 1}}
	if got, want := transcript.lifecycle(), slices.Concat(turn, turn, turn); !slices.Equal(got, want) {
		t.Errorf("lifecycle = %#v\nwant %#v", got, want)
	}
}

func TestACallWithoutAModelOrNamingTheParentsRunsTheChildOnTheParentsModel(t *testing.T) {
	// given
	// ... a parent with an allowed model, whose response starts one child with no model and one naming the parent's
	done := mockReply{text: "done", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C")}}
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{
			agentCall("toolu_1", `{"task":"dig"}`), agentCall("toolu_2", `{"task":"dig","model":"test/model"}`),
		}, Content: raw("P1")}},
		done, done,
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P2")}},
	}}
	small := &mockModel{}
	transcript := &mockTranscript{}
	a := withSmallModel(newParent(model, transcript), small)

	// when
	// ... the parent's turn runs
	a.RunTurn("go")

	// then
	// ... both children were answered by the parent's model, are shown on it, and the allowed model was never called
	want := []ToolResult{{ToolUseID: "toolu_1", Content: "[agent 1]\ndone"}, {ToolUseID: "toolu_2", Content: "[agent 2]\ndone"}}
	if got := model.requests[3][len(model.requests[3])-1].ToolResults; !slices.Equal(got, want) {
		t.Errorf("tool results = %+v\nwant %+v", got, want)
	}
	if len(small.calls) != 0 {
		t.Errorf("allowed model called %d times", len(small.calls))
	}
	if got := transcript.lifecycleOf(1)[0]; got != (ChildStarted{Child: 1, Model: parentModelName}) {
		t.Errorf("first child's start = %#v", got)
	}
	if got := transcript.lifecycleOf(2)[0]; got != (ChildStarted{Child: 2, Model: parentModelName}) {
		t.Errorf("second child's start = %#v", got)
	}
}

func TestAChildWaitingForASlotIsShownWithTheModelItWillRunOn(t *testing.T) {
	// given
	// ... a response starting six children on an allowed model, whose calls are held until the gate opens
	var calls []ToolCall
	for i := 1; i <= 6; i++ {
		calls = append(calls, agentCall(fmt.Sprintf("toolu_%d", i), `{"task":"dig","model":"other/small"}`))
	}
	done := mockReply{text: "done", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C")}}
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: calls, Content: raw("P1")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P2")}},
	}}
	small := &mockModel{replies: []mockReply{done, done, done, done, done, done}}
	gate := newChildGate()
	small.hold = gate.hold
	transcript := &mockTranscript{}
	sixthWaiting := transcript.watch(ChildWaiting{Child: 6, Model: smallModelName})
	a := withSmallModel(newParent(model, transcript), small)
	ended := make(chan struct{})

	// when
	// ... the turn runs, and the gate is opened once five children are held and the sixth is shown waiting on that model
	go func() {
		defer close(ended)
		a.RunTurn("go")
	}()
	reached := errors.Join(gate.awaitHeld(5), await(sixthWaiting))
	close(gate.open)
	finished := await(ended)

	// then
	// ... the sixth child waited and then started, named with the allowed model both times
	if reached != nil || finished != nil {
		t.Fatalf("five held and the sixth waiting: %v; turn ended: %v", reached, finished)
	}
	want := []Event{
		ChildWaiting{Child: 6, Model: smallModelName}, ChildStarted{Child: 6, Model: smallModelName}, ChildFinished{Child: 6},
	}
	if got := transcript.lifecycleOf(6); !slices.Equal(got, want) {
		t.Errorf("sixth child's lifecycle = %#v\nwant %#v", got, want)
	}
}

func TestACallResumingWithAModelOrNamingAModelNotAllowedIsAnErrorResultAndStartsNoTurn(t *testing.T) {
	// given
	// ... a parent that starts a child, then resumes it with a model and starts another on a model that is not allowed
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_1", `{"task":"find the bug"}`)}, Content: raw("P1")}},
		{text: "found it", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C1")}},
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{
			agentCall("toolu_2", `{"agent":1,"task":"where?","model":"other/small"}`),
			agentCall("toolu_3", `{"task":"dig","model":"other/huge"}`),
		}, Content: raw("P2")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P3")}},
	}}
	small := &mockModel{}
	transcript := &mockTranscript{}
	a := withSmallModel(newParent(model, transcript), small)

	// when
	// ... the parent's turn runs
	a.RunTurn("go")

	// then
	// ... both calls got an error saying why, no model was called for them and the first child had only its one turn
	want := []ToolResult{
		{ToolUseID: "toolu_2", Content: "model can't be given with agent: a resumed subagent keeps its model", IsError: true},
		{ToolUseID: "toolu_3", Content: `unknown model "other/huge": expected one of "test/model", "other/small"`, IsError: true},
	}
	if got := model.requests[3][len(model.requests[3])-1].ToolResults; !slices.Equal(got, want) {
		t.Errorf("tool results = %+v\nwant %+v", got, want)
	}
	if len(small.calls) != 0 || len(a.children) != 1 {
		t.Errorf("allowed model called %d times, children = %d; want 0 and 1", len(small.calls), len(a.children))
	}
	wantLifecycle := []Event{ChildStarted{Child: 1, Model: parentModelName}, ChildFinished{Child: 1}}
	if got := transcript.lifecycle(); !slices.Equal(got, wantLifecycle) {
		t.Errorf("lifecycle = %#v\nwant %#v", got, wantLifecycle)
	}
}

func TestAllowedModelsThatShareANameOrTakeTheParentsAreAWiringError(t *testing.T) {
	// given
	// ... allowed model lists with two entries of one name, and with the parent's own name
	small := SubagentModel{Name: smallModelName, Model: &mockModel{}}
	cases := map[string][]SubagentModel{
		"two entries with one name": {small, small},
		"the parent's own name":     {small, {Name: parentModelName, Model: &mockModel{}}},
	}
	for name, models := range cases {
		t.Run(name, func(t *testing.T) {
			a := newParent(&mockModel{}, &mockTranscript{})
			a.SubagentModels = models

			// when
			// ... a turn is run
			panicked := panicOf(func() { a.RunTurn("go") })

			// then
			// ... it panics before calling the model
			if panicked == nil {
				t.Error("no panic")
			}
		})
	}
}

// forkPlaceholders are the results a fork's first message gives a response
// that called list as toolu_1 and forked as toolu_2.
func forkPlaceholders() []ToolResult {
	return []ToolResult{{ToolUseID: "toolu_1", Content: forkOtherCallResult}, {ToolUseID: "toolu_2", Content: forkOwnCallResult}}
}

// listAndFork is a parent's response that calls list and forks a child.
func listAndFork() mockReply {
	return mockReply{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{
		toolCall("toolu_1", "list", `{}`), agentCall("toolu_2", `{"task":"check it","fork":true}`),
	}, Content: raw("P1")}}
}

// newForkingParent is a parent with a list tool of its own, a shell for its
// fresh children and skills.
func newForkingParent(model *mockModel, transcript *mockTranscript) *Agent {
	a := newParent(model, transcript, &mockTool{name: "shell", run: returning("", nil)})
	a.Tools = []Tool{&mockTool{name: "list", run: returning("a.txt", nil)}}
	a.Skills = skillLibrary()
	return a
}

func TestAForkStartsFromTheParentsConversationAndLeavesTheParentsAsAFreshChildWould(t *testing.T) {
	// given
	// ... a parent whose response calls a tool and forks a child, and which later resumes the fork
	model := &mockModel{replies: []mockReply{
		listAndFork(),
		{text: "looks fine", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("F1")}},
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_3", `{"agent":1,"task":"sure?"}`)}, Content: raw("P2")}},
		{text: "yes", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("F2")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P3")}},
	}}
	a := newForkingParent(model, &mockTranscript{})

	// when
	// ... the parent's turn runs
	a.RunTurn("go")

	// then
	// ... the fork's first request has the parent's system prompt and tools, agent included, unchanged
	parent, fork := model.calls[0], model.calls[1]
	if fork.System != parent.System || !slices.Equal(fork.Tools, parent.Tools) {
		t.Errorf("fork's system = %q, tools = %v\nwant the parent's %q, %v", fork.System, toolNames(fork.Tools), parent.System, toolNames(parent.Tools))
	}
	if got := toolNames(fork.Tools); !slices.Equal(got, []string{"list", SkillToolName, AgentToolName}) {
		t.Errorf("fork's tools = %v", got)
	}
	// ... its messages are the parent's history with the response being handled, then one message of placeholder results and the task
	if len(fork.Messages) != 3 || !sameMessages(fork.Messages[:2], []Message{{Role: RoleUser, Text: "go"}, {Role: RoleAssistant, Content: raw("P1")}}) {
		t.Fatalf("fork's messages = %+v", fork.Messages)
	}
	if last := fork.Messages[2]; last.Role != RoleUser || last.Text != "check it" || !slices.Equal(last.ToolResults, forkPlaceholders()) {
		t.Errorf("fork's last message = %+v\nwant results %+v and the task", last, forkPlaceholders())
	}
	// ... the parent's next request is its own history and the two calls' real results
	next := model.requests[2]
	wantResults := []ToolResult{{ToolUseID: "toolu_1", Content: "a.txt"}, {ToolUseID: "toolu_2", Content: "[agent 1]\nlooks fine"}}
	if len(next) != 3 || !sameMessages(next[:2], fork.Messages[:2]) || next[2].Text != "" || !slices.Equal(next[2].ToolResults, wantResults) {
		t.Errorf("parent's next request = %+v", next)
	}
	// ... and the resumed fork continues its own history with the same system prompt and tools
	resumed := model.calls[3]
	if resumed.System != parent.System || !slices.Equal(resumed.Tools, parent.Tools) {
		t.Errorf("resumed fork's system = %q, tools = %v", resumed.System, toolNames(resumed.Tools))
	}
	if got := resumed.Messages; len(got) != 5 || !slices.Equal(got[2].ToolResults, forkPlaceholders()) || !sameMessages(got[3:], []Message{{Role: RoleAssistant, Content: raw("F1")}, {Role: RoleUser, Text: "sure?"}}) {
		t.Errorf("resumed fork's messages = %+v", got)
	}
}

func TestAForkWhoseFirstTurnFailedIsResumedWithThePlaceholderResultsAgain(t *testing.T) {
	// given
	// ... a fork whose first model call fails, and a parent that then resumes it
	model := &mockModel{replies: []mockReply{
		listAndFork(),
		{err: errors.New("HTTP 500: overloaded")},
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_3", `{"agent":1,"task":"again"}`)}, Content: raw("P2")}},
		{text: "looks fine", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("F1")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P3")}},
	}}
	a := newForkingParent(model, &mockTranscript{})

	// when
	// ... the parent's turn runs
	a.RunTurn("go")

	// then
	// ... the resumed fork's request answers the copied response's calls again before the follow-up
	got := model.requests[3]
	if len(got) != 3 || !sameMessages(got[:2], []Message{{Role: RoleUser, Text: "go"}, {Role: RoleAssistant, Content: raw("P1")}}) {
		t.Fatalf("resumed fork's messages = %+v", got)
	}
	if last := got[2]; last.Text != "again" || !slices.Equal(last.ToolResults, forkPlaceholders()) {
		t.Errorf("resumed fork's last message = %+v", last)
	}
}

func TestAForksAgentCallIsAnsweredWithAnErrorAndStartsNothing(t *testing.T) {
	// given
	// ... a fork whose model calls the agent tool it was offered
	model := &mockModel{replies: []mockReply{
		listAndFork(),
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_f1", `{"task":"dig"}`)}, Content: raw("F1")}},
		{text: "looks fine", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("F2")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P2")}},
	}}
	transcript := &mockTranscript{}
	a := newForkingParent(model, transcript)

	// when
	// ... the parent's turn runs
	a.RunTurn("go")

	// then
	// ... the fork got the error as its call's result, no child was started, and the call is in the fork's transcript
	want := ToolResult{ToolUseID: "toolu_f1", Content: "subagents can't start subagents", IsError: true}
	if got := lastToolResult(model.requests, 2); got != want {
		t.Errorf("tool result = %+v, want %+v", got, want)
	}
	if len(a.children) != 1 || len(a.children[0].children) != 0 {
		t.Errorf("children = %d, the fork's = %d; want 1 and 0", len(a.children), len(a.children[0].children))
	}
	wantShown := []Event{
		ReplyStarted{},
		ToolStarted{ID: "toolu_f1", Name: AgentToolName, Summary: `agent {"task":"dig"}`, Input: json.RawMessage(`{"task":"dig"}`)},
		ToolFinished{ID: "toolu_f1", Output: "subagents can't start subagents", IsError: true},
		ReplyStarted{},
		ReplyText{Text: "looks fine"},
	}
	if got := transcript.eventsOf(1); !reflect.DeepEqual(got, wantShown) {
		t.Errorf("fork's events = %#v\nwant %#v", got, wantShown)
	}
}

func TestAForkOnAnotherModelOrOfAnEarlierChildIsAnErrorResultAndStartsNoTurn(t *testing.T) {
	// given
	// ... a parent that starts a child, then forks onto an allowed model and forks while resuming that child
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_1", `{"task":"find the bug"}`)}, Content: raw("P1")}},
		{text: "found it", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("C1")}},
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{
			agentCall("toolu_2", `{"task":"check it","fork":true,"model":"other/small"}`),
			agentCall("toolu_3", `{"agent":1,"task":"check it","fork":true}`),
		}, Content: raw("P2")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P3")}},
	}}
	small := &mockModel{}
	transcript := &mockTranscript{}
	a := withSmallModel(newParent(model, transcript), small)

	// when
	// ... the parent's turn runs
	a.RunTurn("go")

	// then
	// ... both calls got an error saying why, and no child was started or resumed
	want := []ToolResult{
		{ToolUseID: "toolu_2", Content: `a fork runs on your own model, "test/model": omit model, or leave out fork to use "other/small"`, IsError: true},
		{ToolUseID: "toolu_3", Content: "fork can't be given with agent: a resumed subagent keeps its history", IsError: true},
	}
	if got := model.requests[3][len(model.requests[3])-1].ToolResults; !slices.Equal(got, want) {
		t.Errorf("tool results = %+v\nwant %+v", got, want)
	}
	if len(small.calls) != 0 || len(a.children) != 1 {
		t.Errorf("allowed model called %d times, children = %d; want 0 and 1", len(small.calls), len(a.children))
	}
	wantLifecycle := []Event{ChildStarted{Child: 1, Model: parentModelName}, ChildFinished{Child: 1}}
	if got := transcript.lifecycle(); !slices.Equal(got, wantLifecycle) {
		t.Errorf("lifecycle = %#v\nwant %#v", got, wantLifecycle)
	}
}

func TestAForkNamingTheParentsOwnModelStarts(t *testing.T) {
	// given
	// ... a parent with an allowed model, whose response forks and names the parent's own model
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{agentCall("toolu_1", `{"task":"check it","fork":true,"model":"test/model"}`)}, Content: raw("P1")}},
		{text: "looks fine", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("F1")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P2")}},
	}}
	a := withSmallModel(newParent(model, &mockTranscript{}), &mockModel{})

	// when
	// ... the parent's turn runs
	a.RunTurn("go")

	// then
	// ... the fork ran on the parent's model from the parent's conversation, and its reply came back
	if got := model.requests[1]; len(got) != 3 || got[2].Text != "check it" {
		t.Errorf("fork's request = %+v", got)
	}
	if got := lastToolResult(model.requests, 2); got != (ToolResult{ToolUseID: "toolu_1", Content: "[agent 1]\nlooks fine"}) {
		t.Errorf("tool result = %+v", got)
	}
}

func TestASixthForkWaitsForASlotAndIsShownLikeAnyChild(t *testing.T) {
	// given
	// ... a response starting six forks, whose model calls are held until the gate opens
	var calls []ToolCall
	for i := 1; i <= 6; i++ {
		calls = append(calls, agentCall(fmt.Sprintf("toolu_%d", i), `{"task":"dig","fork":true}`))
	}
	done := mockReply{text: "done", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("F")}}
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: calls, Content: raw("P1")}},
		done, done, done, done, done, done,
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("P2")}},
	}}
	gate := newChildGate()
	gate.isChild = func(req ModelRequest) bool { return req.Messages[len(req.Messages)-1].Text == "dig" }
	model.hold = gate.hold
	transcript := &mockTranscript{}
	sixthWaiting := transcript.watch(ChildWaiting{Child: 6, Model: parentModelName})
	a := newParent(model, transcript)
	ended := make(chan struct{})

	// when
	// ... the turn runs, and the gate is opened once five forks are held and the sixth is waiting
	go func() {
		defer close(ended)
		a.RunTurn("go")
	}()
	reached := errors.Join(gate.awaitHeld(5), await(sixthWaiting))
	close(gate.open)
	finished := await(ended)

	// then
	// ... five forks ran at once and never six, and the sixth was shown waiting, started and finished
	if reached != nil || finished != nil {
		t.Fatalf("five held and the sixth waiting: %v; turn ended: %v", reached, finished)
	}
	if gate.peak != 5 {
		t.Errorf("most forks running at once = %d, want 5", gate.peak)
	}
	want := []Event{
		ChildWaiting{Child: 6, Model: parentModelName}, ChildStarted{Child: 6, Model: parentModelName}, ChildFinished{Child: 6},
	}
	if got := transcript.lifecycleOf(6); !slices.Equal(got, want) {
		t.Errorf("sixth fork's lifecycle = %#v\nwant %#v", got, want)
	}
}
