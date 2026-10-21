package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Port mocks.

type mockReply struct {
	text string // streamed before the response
	resp ModelResponse
	err  error // non-nil: the call fails with this error
}

// mockModel is called by several children at once, each taking the next
// scripted reply.
type mockModel struct {
	replies []mockReply
	// hold, when set, runs before a call takes its reply and may block; an
	// error from it fails the call without using a reply.
	hold func(ctx context.Context, req ModelRequest) error

	mu       sync.Mutex
	requests [][]Message    // the messages each call received
	calls    []ModelRequest // each call's whole request
}

func (m *mockModel) Stream(ctx context.Context, req ModelRequest, onText func(string)) (ModelResponse, error) {
	if m.hold != nil {
		if err := m.hold(ctx, req); err != nil {
			return ModelResponse{}, err
		}
	}
	reply := m.next(req)
	if reply.text != "" {
		onText(reply.text)
	}
	if reply.err != nil {
		return ModelResponse{}, reply.err
	}
	return reply.resp, nil
}

func (m *mockModel) next(req ModelRequest) mockReply {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.requests) >= len(m.replies) {
		panic("mock model called more times than scripted")
	}
	reply := m.replies[len(m.requests)]
	m.requests = append(m.requests, slices.Clone(req.Messages))
	m.calls = append(m.calls, req)
	return reply
}

type mockTool struct {
	name string
	run  func(ctx context.Context) (string, error)

	mu     sync.Mutex
	inputs []string
}

func (m *mockTool) Spec() ToolSpec { return ToolSpec{Name: m.name} }

func (m *mockTool) Summary(input json.RawMessage) string { return m.name + " " + string(input) }

func (m *mockTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	m.mu.Lock()
	m.inputs = append(m.inputs, string(input))
	m.mu.Unlock()
	return m.run(ctx)
}

func returning(content string, err error) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return content, err }
}

// await waits for done, or fails after long enough that it was never coming.
func await(done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("timed out waiting")
	}
}

// panicOf runs f and returns what it panicked with, or nil.
func panicOf(f func()) (panicked any) {
	defer func() { panicked = recover() }()
	f()
	return nil
}

// mockTranscript is shown events by several tool calls at once.
type mockTranscript struct {
	mu     sync.Mutex
	events []Event
	agents []int // the agent number passed with each event

	watched Event
	seen    chan struct{}
}

func (m *mockTranscript) Show(agent int, event Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, event)
	m.agents = append(m.agents, agent)
	if m.seen != nil && event == m.watched {
		close(m.seen)
		m.seen = nil
	}
}

// watch returns a channel closed when event is next shown.
func (m *mockTranscript) watch(event Event) <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := make(chan struct{})
	m.watched, m.seen = event, seen
	return seen
}

func (m *mockTranscript) last() Event {
	return m.events[len(m.events)-1]
}

// mockHistory records the messages of each turn the conversation keeps.
type mockHistory struct{ kept []Message }

func (m *mockHistory) Keep(messages []Message) { m.kept = append(m.kept, messages...) }

// mockGate allows the calls allow says yes to, and records every call it
// is asked about. allow may block on ctx, as a person answering would.
type mockGate struct {
	allow func(ctx context.Context, call ToolCall) (bool, string)

	mu    sync.Mutex
	calls []ToolCall
}

func (m *mockGate) Allow(ctx context.Context, call ToolCall) (bool, string) {
	m.mu.Lock()
	m.calls = append(m.calls, call)
	m.mu.Unlock()
	return m.allow(ctx, call)
}

func toolCall(id, name, input string) ToolCall {
	return ToolCall{ID: id, Name: name, Input: json.RawMessage(input)}
}

// raw is a model's content as the core holds it: JSON it never reads.
func raw(s string) json.RawMessage { return json.RawMessage(strconv.Quote(s)) }

func newAgent(model *mockModel, transcript *mockTranscript, tools ...Tool) *Agent {
	return &Agent{Model: model, Transcript: transcript, Tools: tools, ModelName: parentModelName}
}

func lastToolResult(requests [][]Message, call int) ToolResult {
	req := requests[call]
	return req[len(req)-1].ToolResults[0]
}

func TestAToolCallIsShownRunAndItsResultSentBackToTheModel(t *testing.T) {
	// given
	// ... a model that calls a tool once, then ends its turn with streamed text
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_1", "list", `{"dir":"."}`)}, Content: raw("A1")}},
		{text: "done", resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A2")}},
	}}
	tool := &mockTool{name: "list", run: returning("a.txt", nil)}
	transcript := &mockTranscript{}
	a := newAgent(model, transcript, tool)

	// when
	// ... the user asks for something
	a.RunTurn("list files")

	// then
	// ... the transcript sees each step, the tool got the call's input and its result went back to the model
	want := []Event{
		ReplyStarted{},
		ToolStarted{ID: "toolu_1", Name: "list", Summary: `list {"dir":"."}`, Input: json.RawMessage(`{"dir":"."}`)},
		ToolFinished{ID: "toolu_1", Output: "a.txt"},
		ReplyStarted{},
		ReplyText{Text: "done"},
	}
	if !reflect.DeepEqual(transcript.events, want) {
		t.Errorf("events = %#v\nwant %#v", transcript.events, want)
	}
	if !slices.Equal(tool.inputs, []string{`{"dir":"."}`}) {
		t.Errorf("tool inputs = %v", tool.inputs)
	}
	if got := lastToolResult(model.requests, 1); got != (ToolResult{ToolUseID: "toolu_1", Content: "a.txt"}) {
		t.Errorf("tool result = %+v", got)
	}
	if len(a.messages) != 4 {
		t.Errorf("len(messages) = %d, want 4", len(a.messages))
	}
}

func TestAGateSeesAnAllowedCallBeforeItRunsAndTheCallRunsAsBefore(t *testing.T) {
	// given
	// ... a gate that allows everything, on an agent whose model calls a tool once
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_1", "list", `{"dir":"."}`)}, Content: raw("A1")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A2")}},
	}}
	tool := &mockTool{name: "list", run: returning("a.txt", nil)}
	transcript := &mockTranscript{}
	gate := &mockGate{allow: func(context.Context, ToolCall) (bool, string) { return true, "" }}
	a := newAgent(model, transcript, tool)
	a.Gate = gate

	// when
	// ... a turn runs
	a.RunTurn("list files")

	// then
	// ... the gate saw the call's id, name and input, the tool ran and its result went back
	if want := []ToolCall{toolCall("toolu_1", "list", `{"dir":"."}`)}; !reflect.DeepEqual(gate.calls, want) {
		t.Errorf("gate calls = %#v, want %#v", gate.calls, want)
	}
	if !slices.Equal(tool.inputs, []string{`{"dir":"."}`}) {
		t.Errorf("tool inputs = %v", tool.inputs)
	}
	if got := lastToolResult(model.requests, 1); got != (ToolResult{ToolUseID: "toolu_1", Content: "a.txt"}) {
		t.Errorf("tool result = %+v", got)
	}
}

func TestADeniedCallDoesNotRunAndTheModelGetsTheReasonAsAnError(t *testing.T) {
	// given
	// ... a gate that denies with a reason
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_1", "list", `{}`)}, Content: raw("A1")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A2")}},
	}}
	tool := &mockTool{name: "list", run: returning("a.txt", nil)}
	transcript := &mockTranscript{}
	a := newAgent(model, transcript, tool)
	a.Gate = &mockGate{allow: func(context.Context, ToolCall) (bool, string) { return false, "user said no" }}

	// when
	// ... a turn runs
	a.RunTurn("list files")

	// then
	// ... the tool never ran, the transcript shows the call failing and the model got the reason as an error
	if len(tool.inputs) != 0 {
		t.Errorf("tool ran with %v", tool.inputs)
	}
	if got := transcript.events[2]; got != (ToolFinished{ID: "toolu_1", Output: "user said no", IsError: true}) {
		t.Errorf("event = %#v", got)
	}
	if got := lastToolResult(model.requests, 1); got != (ToolResult{ToolUseID: "toolu_1", Content: "user said no", IsError: true}) {
		t.Errorf("tool result = %+v", got)
	}
}

func TestCancellingTheTurnWhileTheGateIsBlockedEndsItCancelled(t *testing.T) {
	// given
	// ... a gate that blocks until its context is cancelled
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_1", "list", `{}`)}, Content: raw("A1")}},
	}}
	tool := &mockTool{name: "list", run: returning("a.txt", nil)}
	transcript := &mockTranscript{}
	a := newAgent(model, transcript, tool)
	asked := make(chan struct{})
	a.Gate = &mockGate{allow: func(ctx context.Context, call ToolCall) (bool, string) {
		close(asked)
		<-ctx.Done()
		return true, ""
	}}

	// when
	// ... the turn is cancelled once the gate is asked
	done := make(chan struct{})
	go func() { a.RunTurn("go"); close(done) }()
	asked2 := await(asked)
	a.Cancel()
	finished := await(done)

	// then
	// ... the turn ended discarded as cancelled, and the tool never ran
	if asked2 != nil || finished != nil {
		t.Fatalf("gate asked: %v, turn finished: %v", asked2, finished)
	}
	if last := transcript.last(); last != (TurnDiscarded{Reason: "cancelled"}) {
		t.Errorf("last event = %#v", last)
	}
	if len(tool.inputs) != 0 {
		t.Errorf("tool ran with %v", tool.inputs)
	}
}

func TestAToolErrorGoesBackToTheModelAsAnErrorResult(t *testing.T) {
	// given
	// ... a tool that fails
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_1", "list", `{}`)}, Content: raw("A1")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A2")}},
	}}
	a := newAgent(model, &mockTranscript{}, &mockTool{name: "list", run: returning("", errors.New("no such dir"))})

	// when
	// ... a turn runs
	a.RunTurn("go")

	// then
	// ... the model gets the error as an error result and the turn carries on
	want := ToolResult{ToolUseID: "toolu_1", Content: "no such dir", IsError: true}
	if got := lastToolResult(model.requests, 1); got != want {
		t.Errorf("tool result = %+v, want %+v", got, want)
	}
}

func TestAnUnknownToolIsAnsweredWithAnErrorWithoutRunningAnything(t *testing.T) {
	// given
	// ... a model that calls a tool that doesn't exist
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_1", "browser", `{}`)}, Content: raw("A1")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A2")}},
	}}
	tool := &mockTool{name: "list", run: returning("", nil)}
	a := newAgent(model, &mockTranscript{}, tool)

	// when
	// ... a turn runs
	a.RunTurn("go")

	// then
	// ... the model gets an error result and no tool runs
	want := ToolResult{ToolUseID: "toolu_1", Content: `unknown tool "browser"`, IsError: true}
	if got := lastToolResult(model.requests, 1); got != want {
		t.Errorf("tool result = %+v, want %+v", got, want)
	}
	if len(tool.inputs) != 0 {
		t.Errorf("tool ran with %v", tool.inputs)
	}
}

func TestToolCallsInOneResponseRunAtTheSameTimeAndAnswerInCallOrder(t *testing.T) {
	// given
	// ... a response calling two tools that each wait for the other to start, the first also for the second to end
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_1", "slow", `{}`), toolCall("toolu_2", "quick", `{}`)}, Content: raw("A1")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A2")}},
	}}
	slowStarted, quickStarted, quickEnded := make(chan struct{}), make(chan struct{}), make(chan struct{})
	slow := &mockTool{name: "slow", run: func(context.Context) (string, error) {
		close(slowStarted)
		return "slow done", errors.Join(await(quickStarted), await(quickEnded))
	}}
	quick := &mockTool{name: "quick", run: func(context.Context) (string, error) {
		defer close(quickEnded)
		close(quickStarted)
		return "quick done", await(slowStarted)
	}}
	a := newAgent(model, &mockTranscript{}, slow, quick)

	// when
	// ... a turn runs
	a.RunTurn("go")

	// then
	// ... both ran to the end and their results went back in the order they were called
	want := []ToolResult{{ToolUseID: "toolu_1", Content: "slow done"}, {ToolUseID: "toolu_2", Content: "quick done"}}
	if got := model.requests[1][len(model.requests[1])-1].ToolResults; !slices.Equal(got, want) {
		t.Errorf("tool results = %+v\nwant %+v", got, want)
	}
}

func TestCancellingDuringConcurrentToolCallsStopsThemAllAndDiscardsTheTurn(t *testing.T) {
	// given
	// ... two tools that run until their context is cancelled, and the user cancelling once both are running
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_1", "wait", `{}`), toolCall("toolu_2", "cancel", `{}`)}, Content: raw("A1")}},
	}}
	transcript := &mockTranscript{}
	a := newAgent(model, transcript)
	waiting := make(chan struct{})
	stopped := make(chan error, 2)
	wait := &mockTool{name: "wait", run: func(ctx context.Context) (string, error) {
		close(waiting)
		stopped <- await(ctx.Done())
		return "", ctx.Err()
	}}
	cancel := &mockTool{name: "cancel", run: func(ctx context.Context) (string, error) {
		started := await(waiting)
		a.Cancel()
		stopped <- errors.Join(started, await(ctx.Done()))
		return "", ctx.Err()
	}}
	a.Tools = []Tool{wait, cancel}

	// when
	// ... a turn runs
	a.RunTurn("go")

	// then
	// ... both tools saw the cancellation, the model wasn't called again and the turn is gone
	if first, second := <-stopped, <-stopped; first != nil || second != nil {
		t.Errorf("tools stopped with %v and %v, want cancelled", first, second)
	}
	if len(model.requests) != 1 {
		t.Errorf("model called %d times", len(model.requests))
	}
	if last := transcript.last(); last != (TurnDiscarded{Reason: "cancelled"}) {
		t.Errorf("last event = %#v", last)
	}
	if len(a.messages) != 0 {
		t.Errorf("len(messages) = %d, want 0", len(a.messages))
	}
}

func TestCancellingAfterStartTurnButBeforeItRunsDiscardsTheTurn(t *testing.T) {
	// given
	// ... a model that fails once its context is cancelled, and a turn started but not yet run
	model := &mockModel{
		replies: []mockReply{{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A1")}}},
		hold:    func(ctx context.Context, _ ModelRequest) error { return ctx.Err() },
	}
	transcript := &mockTranscript{}
	a := newAgent(model, transcript)
	run := a.StartTurn("go")

	// when
	// ... the user cancels, and then the turn runs
	a.Cancel()
	run()

	// then
	// ... the turn is discarded as cancelled
	if last := transcript.last(); last != (TurnDiscarded{Reason: "cancelled"}) {
		t.Errorf("last event = %#v", last)
	}
	if len(a.messages) != 0 {
		t.Errorf("len(messages) = %d, want 0", len(a.messages))
	}
}

func TestCancellingWhileIdleDoesNotCancelTheNextTurn(t *testing.T) {
	// given
	// ... a model that fails once its context is cancelled, and a cancel with no turn pending
	model := &mockModel{
		replies: []mockReply{{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A1")}}},
		hold:    func(ctx context.Context, _ ModelRequest) error { return ctx.Err() },
	}
	transcript := &mockTranscript{}
	a := newAgent(model, transcript)
	a.Cancel()

	// when
	// ... a turn runs
	a.RunTurn("go")

	// then
	// ... it ran to its end
	if last := transcript.last(); last == (TurnDiscarded{Reason: "cancelled"}) {
		t.Errorf("turn was cancelled")
	}
	if len(a.messages) != 2 {
		t.Errorf("len(messages) = %d, want 2", len(a.messages))
	}
}

func TestAPanicInOneOfSeveralToolCallsStopsTheOthersAndPanicsTheTurn(t *testing.T) {
	// given
	// ... a tool that panics while another runs until its context is cancelled
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_1", "wait", `{}`), toolCall("toolu_2", "broken", `{}`)}, Content: raw("A1")}},
	}}
	waiting := make(chan struct{})
	stopped := make(chan error, 1)
	wait := &mockTool{name: "wait", run: func(ctx context.Context) (string, error) {
		close(waiting)
		stopped <- await(ctx.Done())
		return "", ctx.Err()
	}}
	broken := &mockTool{name: "broken", run: func(context.Context) (string, error) {
		<-waiting
		panic("nil map")
	}}
	a := newAgent(model, &mockTranscript{}, wait, broken)

	// when
	// ... a turn runs
	panicked := panicOf(func() { a.RunTurn("go") })

	// then
	// ... the other tool was stopped and the turn panicked with the tool's panic
	if err := <-stopped; err != nil {
		t.Errorf("the other tool: %v, want cancelled", err)
	}
	if !strings.Contains(fmt.Sprint(panicked), "nil map") {
		t.Errorf("panic = %v", panicked)
	}
}

func TestAFailedModelCallDiscardsTheTurnFromTheHistory(t *testing.T) {
	// given
	// ... a model that runs a tool and then fails on the follow-up call
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_1", "list", `{}`)}, Content: raw("A1")}},
		{err: errors.New("HTTP 500: overloaded")},
	}}
	transcript := &mockTranscript{}
	a := newAgent(model, transcript, &mockTool{name: "list", run: returning("", nil)})

	// when
	// ... a turn runs
	a.RunTurn("go")

	// then
	// ... the error is shown and none of the turn's messages are kept
	if last := transcript.last(); last != (TurnDiscarded{Reason: "HTTP 500: overloaded"}) {
		t.Errorf("last event = %#v", last)
	}
	if len(a.messages) != 0 {
		t.Errorf("len(messages) = %d, want 0", len(a.messages))
	}
}

func TestLongToolOutputIsSentToTheModelTruncatedFromTheFront(t *testing.T) {
	// given
	// ... a tool whose output is larger than the cap and ends with an error line
	big := strings.Repeat("x", maxToolResultBytes) + "\nFAILED"
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_1", "make", `{}`)}, Content: raw("A1")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A2")}},
	}}
	a := newAgent(model, &mockTranscript{}, &mockTool{name: "make", run: returning(big, nil)})

	// when
	// ... a turn runs
	a.RunTurn("build")

	// then
	// ... the start is cut, marked, and the tail with the error is kept
	content := lastToolResult(model.requests, 1).Content
	if !strings.HasPrefix(content, "[output truncated: first ") || !strings.HasSuffix(content, "\nFAILED") {
		t.Errorf("content = %q…%q", content[:40], content[len(content)-20:])
	}
}

func TestEveryEventOfTheParentsTurnReportsAgentZero(t *testing.T) {
	// given
	// ... a turn that loads a skill, streams text, runs a tool and is then stopped by the provider
	model := &mockModel{replies: []mockReply{
		{text: "looking", resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_1", "list", `{}`)}, Content: raw("A1")}},
		{resp: ModelResponse{StopReason: StopOther, StopDetail: "refusal"}},
	}}
	transcript := &mockTranscript{}
	a := newAgent(model, transcript, &mockTool{name: "list", run: returning("a.txt", nil)})
	a.Skills = skillLibrary()

	// when
	// ... the parent runs it
	a.RunTurn("/deploy prod")

	// then
	// ... an event of every kind was shown, each as agent 0
	var kinds []string
	for _, event := range transcript.events {
		kinds = append(kinds, fmt.Sprintf("%T", event))
	}
	wantKinds := []string{
		"core.SkillLoaded", "core.ReplyStarted", "core.ReplyText", "core.ToolStarted",
		"core.ToolFinished", "core.ReplyStarted", "core.TurnDiscarded",
	}
	if !slices.Equal(kinds, wantKinds) {
		t.Errorf("events = %v\nwant %v", kinds, wantKinds)
	}
	if !slices.Equal(transcript.agents, make([]int, len(wantKinds))) {
		t.Errorf("agents = %v, want all 0", transcript.agents)
	}
}

func TestHistoryIsToldOfEveryMessageTheConversationKeepsAndNoneItDiscards(t *testing.T) {
	// given
	// ... a turn kept after a tool call, a turn the provider fails after a tool call, a turn kept, and a fourth turn
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_1", "list", `{}`)}, Content: raw("A1")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A2")}},
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_2", "list", `{}`)}, Content: raw("B1")}},
		{err: errors.New("HTTP 500")},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A3")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A4")}},
	}}
	a := newAgent(model, &mockTranscript{}, &mockTool{name: "list", run: returning("a.txt", nil)})
	history := &mockHistory{}
	a.History = history

	// when
	// ... the four turns run
	a.RunTurn("one")
	a.RunTurn("two")
	a.RunTurn("three")
	a.RunTurn("four")

	// then
	// ... the history holds, in order, exactly the conversation the last request started with, and the last turn,
	// ... each assistant's message naming the model that wrote it
	kept := history.kept[:len(history.kept)-2]
	next := model.requests[len(model.requests)-1]
	if want := next[:len(next)-1]; !reflect.DeepEqual(kept, want) {
		t.Errorf("kept = %+v\nwant %+v", kept, want)
	}
	if want := []Message{{Role: RoleUser, Text: "four"}, {Role: RoleAssistant, Content: raw("A4"), Model: parentModelName}}; !reflect.DeepEqual(history.kept[len(kept):], want) {
		t.Errorf("last turn kept = %+v\nwant %+v", history.kept[len(kept):], want)
	}
}

func TestAnAgentSeededWithASavedConversationContinuesIt(t *testing.T) {
	// given
	// ... an agent seeded with a saved conversation of one turn with a tool call, its first response
	// ... saved before messages named their model and its second written by another model
	saved := []Message{
		{Role: RoleUser, Text: "one"},
		{Role: RoleAssistant, Content: raw("A1")},
		{Role: RoleUser, ToolResults: []ToolResult{{ToolUseID: "toolu_1", Content: "a.txt"}}},
		{Role: RoleAssistant, Content: raw("A2"), Model: "other/model"},
	}
	model := &mockModel{replies: []mockReply{{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A3")}}}}
	a := newAgent(model, &mockTranscript{})
	if err := a.Seed(saved); err != nil {
		t.Fatal(err)
	}

	// when
	// ... a turn runs
	a.RunTurn("two")

	// then
	// ... the model sees the saved conversation, each message naming the model it named, and then the new prompt
	want := append(slices.Clone(saved), Message{Role: RoleUser, Text: "two"})
	if got := model.requests[0]; !reflect.DeepEqual(got, want) {
		t.Errorf("request = %+v\nwant %+v", got, want)
	}
}

func TestSeedingWithMessagesNoTurnCouldHaveKeptFails(t *testing.T) {
	// given
	// ... a conversation with an assistant's message that answers another assistant's, as no kept turn has
	saved := []Message{{Role: RoleUser, Text: "one"}, {Role: RoleAssistant, Content: raw("A1")}, {Role: RoleAssistant, Content: raw("A2")}}
	a := newAgent(&mockModel{}, &mockTranscript{})

	// when
	// ... an agent is seeded with it
	err := a.Seed(saved)

	// then
	// ... seeding fails and the agent's history stays empty
	if err == nil {
		t.Error("err = nil, want one")
	}
	if len(a.messages) != 0 {
		t.Errorf("len(messages) = %d, want 0", len(a.messages))
	}
}

func TestAModelSwitchDuringATurnTakesEffectAtTheNextTurn(t *testing.T) {
	// given
	// ... an agent on one model whose turn calls a tool, during which the model is switched to another
	old := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_1", "list", `{}`)}, Content: raw("A1")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A2")}},
	}}
	next := &mockModel{replies: []mockReply{{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A3")}}}}
	var a *Agent
	switching := &mockTool{name: "list", run: func(context.Context) (string, error) {
		a.SetModel(next, "other/model")
		return "a.txt", nil
	}}
	a = newAgent(old, &mockTranscript{}, switching)

	// when
	// ... that turn runs, and then another
	a.RunTurn("one")
	a.RunTurn("two")

	// then
	// ... the first turn ran on the old model to its end, and the second went to the new one by name,
	// ... carrying the old model's messages under the old model's name
	if len(old.calls) != 2 || len(next.calls) != 1 {
		t.Fatalf("calls: old %d, new %d; want 2 and 1", len(old.calls), len(next.calls))
	}
	if old.calls[1].Model != parentModelName || next.calls[0].Model != "other/model" {
		t.Errorf("request models = %q then %q, want %q then %q", old.calls[1].Model, next.calls[0].Model, parentModelName, "other/model")
	}
	sent := next.calls[0].Messages
	if sent[1].Model != parentModelName || sent[3].Model != parentModelName {
		t.Errorf("messages = %+v, want the assistant's named %q", sent, parentModelName)
	}
}
