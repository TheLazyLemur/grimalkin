package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

// Port mocks.

type mockReply struct {
	text string // streamed before the response
	resp ModelResponse
	err  error // non-nil: the call fails with this error
}

type mockModel struct {
	replies  []mockReply
	requests [][]Message // the messages each call received
}

func (m *mockModel) Stream(_ context.Context, req ModelRequest, onText func(string)) (ModelResponse, error) {
	if len(m.requests) >= len(m.replies) {
		panic("mock model called more times than scripted")
	}
	reply := m.replies[len(m.requests)]
	m.requests = append(m.requests, slices.Clone(req.Messages))
	if reply.text != "" {
		onText(reply.text)
	}
	if reply.err != nil {
		return ModelResponse{}, reply.err
	}
	return reply.resp, nil
}

type mockTool struct {
	name   string
	run    func(ctx context.Context) (string, error)
	inputs []string
}

func (m *mockTool) Spec() ToolSpec { return ToolSpec{Name: m.name} }

func (m *mockTool) Summary(input json.RawMessage) string { return m.name + " " + string(input) }

func (m *mockTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	m.inputs = append(m.inputs, string(input))
	return m.run(ctx)
}

func returning(content string, err error) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return content, err }
}

type mockTranscript struct {
	events []Event
}

func (m *mockTranscript) Show(event Event) {
	m.events = append(m.events, event)
}

func (m *mockTranscript) last() Event {
	return m.events[len(m.events)-1]
}

func toolCall(id, name, input string) ToolCall {
	return ToolCall{ID: id, Name: name, Input: json.RawMessage(input)}
}

func newAgent(model *mockModel, transcript *mockTranscript, tools ...Tool) *Agent {
	return &Agent{Model: model, Transcript: transcript, Tools: tools}
}

func lastToolResult(requests [][]Message, call int) ToolResult {
	req := requests[call]
	return req[len(req)-1].ToolResults[0]
}

func TestAToolCallIsShownRunAndItsResultSentBackToTheModel(t *testing.T) {
	// given
	// ... a model that calls a tool once, then ends its turn with streamed text
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_1", "list", `{"dir":"."}`)}, Content: "A1"}},
		{text: "done", resp: ModelResponse{StopReason: StopEndTurn, Content: "A2"}},
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
		ToolStarted{ID: "toolu_1", Name: "list", Summary: `list {"dir":"."}`},
		ToolFinished{ID: "toolu_1", Output: "a.txt"},
		ReplyStarted{},
		ReplyText{"done"},
	}
	if !slices.Equal(transcript.events, want) {
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

func TestAToolErrorGoesBackToTheModelAsAnErrorResult(t *testing.T) {
	// given
	// ... a tool that fails
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_1", "list", `{}`)}, Content: "A1"}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: "A2"}},
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
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_1", "browser", `{}`)}, Content: "A1"}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: "A2"}},
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

func TestCancellingDuringAToolStopsItAndDiscardsTheTurn(t *testing.T) {
	// given
	// ... a tool that runs until its context is cancelled, and the user cancelling while it runs
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_1", "wait", `{}`), toolCall("toolu_2", "wait", `{}`)}, Content: "A1"}},
	}}
	transcript := &mockTranscript{}
	a := newAgent(model, transcript)
	tool := &mockTool{name: "wait", run: func(ctx context.Context) (string, error) {
		a.Cancel()
		<-ctx.Done()
		return "", ctx.Err()
	}}
	a.Tools = []Tool{tool}

	// when
	// ... a turn runs
	a.RunTurn("go")

	// then
	// ... the tool saw the cancellation, the second call never ran, the model wasn't called again and the turn is gone
	if len(tool.inputs) != 1 || len(model.requests) != 1 {
		t.Errorf("tool ran %d times, model called %d times", len(tool.inputs), len(model.requests))
	}
	if last := transcript.last(); last != (TurnDiscarded{"cancelled"}) {
		t.Errorf("last event = %#v", last)
	}
	if len(a.messages) != 0 {
		t.Errorf("len(messages) = %d, want 0", len(a.messages))
	}
}

func TestAFailedModelCallDiscardsTheTurnFromTheHistory(t *testing.T) {
	// given
	// ... a model that runs a tool and then fails on the follow-up call
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_1", "list", `{}`)}, Content: "A1"}},
		{err: errors.New("HTTP 500: overloaded")},
	}}
	transcript := &mockTranscript{}
	a := newAgent(model, transcript, &mockTool{name: "list", run: returning("", nil)})

	// when
	// ... a turn runs
	a.RunTurn("go")

	// then
	// ... the error is shown and none of the turn's messages are kept
	if last := transcript.last(); last != (TurnDiscarded{"HTTP 500: overloaded"}) {
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
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_1", "make", `{}`)}, Content: "A1"}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: "A2"}},
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
