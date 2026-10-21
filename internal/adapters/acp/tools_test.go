package acp_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	sdk "github.com/coder/acp-go-sdk"

	"grimalkin/internal/core"
)

// fakeTool is a core.Tool that returns out, and records the inputs it ran on.
type fakeTool struct {
	name string
	out  string

	mu  sync.Mutex
	ran []string
}

func (f *fakeTool) Spec() core.ToolSpec { return core.ToolSpec{Name: f.name} }

func (f *fakeTool) Summary(input json.RawMessage) string { return f.name + " " + string(input) }

func (f *fakeTool) Run(_ context.Context, input json.RawMessage) (string, error) {
	f.mu.Lock()
	f.ran = append(f.ran, string(input))
	f.mu.Unlock()
	return f.out, nil
}

func (f *fakeTool) inputs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ran)
}

// choosing answers a permission request by selecting its option of kind.
func choosing(kind sdk.PermissionOptionKind) func(context.Context, sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
	return func(_ context.Context, req sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
		for _, o := range req.Options {
			if o.Kind == kind {
				return sdk.RequestPermissionResponse{Outcome: sdk.RequestPermissionOutcome{Selected: &sdk.RequestPermissionOutcomeSelected{OptionId: o.OptionId, Outcome: "selected"}}}, nil
			}
		}
		panic("no option of kind " + string(kind))
	}
}

func shellCall(id, command string) core.ToolCall {
	return core.ToolCall{ID: id, Name: "shell", Input: json.RawMessage(`{"command":"` + command + `"}`)}
}

func (c *recordingClient) toolCalls() []sdk.SessionUpdateToolCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	var calls []sdk.SessionUpdateToolCall
	for _, n := range c.updates {
		if n.Update.ToolCall != nil {
			calls = append(calls, *n.Update.ToolCall)
		}
	}
	return calls
}

func (c *recordingClient) toolUpdates() []sdk.SessionToolCallUpdate {
	c.mu.Lock()
	defer c.mu.Unlock()
	var updates []sdk.SessionToolCallUpdate
	for _, n := range c.updates {
		if n.Update.ToolCallUpdate != nil {
			updates = append(updates, *n.Update.ToolCallUpdate)
		}
	}
	return updates
}

// lastResults is the tool results in the last request sent to the model.
func lastResults(m *scriptedModel) []core.ToolResult {
	sent := m.sent()
	messages := sent[len(sent)-1].Messages
	return messages[len(messages)-1].ToolResults
}

func TestToolCallIsAnnouncedPendingAndAskedForPermission(t *testing.T) {
	// given
	// ... a session whose model calls the shell, and a client that allows it
	shell := &fakeTool{name: "shell", out: "a.txt"}
	s := newToolSession(t, []core.Tool{shell}, modelReply{calls: []core.ToolCall{shellCall("t1", "ls")}}, modelReply{chunks: []string{"done"}})
	s.got.answerWith(choosing(sdk.PermissionOptionKindAllowOnce))
	s.initialize(t)
	id := s.newSession(t)

	// when
	// ... the client prompts
	_, err := s.client.Prompt(context.Background(), promptOf(id, "list"))

	// then
	// ... a pending tool_call and a permission request carried the id, title, kind, input and tool name, and offered allow once and reject once
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	wantInput := map[string]any{"command": "ls"}
	wantMeta := map[string]any{"toolName": "shell"}
	calls := s.got.toolCalls()
	if len(calls) != 1 {
		t.Fatalf("tool_call updates = %d, want 1", len(calls))
	}
	call := calls[0]
	if call.ToolCallId != "t1" || call.Title != `shell {"command":"ls"}` || call.Kind != sdk.ToolKindExecute || call.Status != sdk.ToolCallStatusPending ||
		!reflect.DeepEqual(call.RawInput, wantInput) || !reflect.DeepEqual(call.Meta, wantMeta) {
		t.Errorf("tool_call = %+v, want t1 pending execute with the title, input and tool name", call)
	}
	if len(s.got.requests) != 1 {
		t.Fatalf("permission requests = %d, want 1", len(s.got.requests))
	}
	req := s.got.requests[0]
	tc := req.ToolCall
	if req.SessionId != id || tc.ToolCallId != "t1" || *tc.Title != call.Title || *tc.Kind != sdk.ToolKindExecute ||
		!reflect.DeepEqual(tc.RawInput, wantInput) || !reflect.DeepEqual(tc.Meta, wantMeta) {
		t.Errorf("request = %+v, want the call's session, id, title, kind, input and tool name", req)
	}
	var options []string
	for _, o := range req.Options {
		options = append(options, string(o.Kind)+" "+o.Name)
	}
	if want := []string{"allow_once Allow", "reject_once Deny"}; !slices.Equal(options, want) {
		t.Errorf("options = %q, want %q", options, want)
	}
}

func TestAllowedToolRunsThenReportsInProgressAndItsOutput(t *testing.T) {
	// given
	// ... a session whose model calls the shell, and a client that allows it
	shell := &fakeTool{name: "shell", out: "a.txt"}
	s := newToolSession(t, []core.Tool{shell}, modelReply{calls: []core.ToolCall{shellCall("t1", "ls")}}, modelReply{chunks: []string{"done"}})
	s.got.answerWith(choosing(sdk.PermissionOptionKindAllowOnce))
	s.initialize(t)
	id := s.newSession(t)

	// when
	// ... the client prompts
	resp, err := s.client.Prompt(context.Background(), promptOf(id, "list"))

	// then
	// ... the tool ran, the updates went pending, in_progress, completed with the output, and the model got the output
	if err != nil || resp.StopReason != sdk.StopReasonEndTurn {
		t.Fatalf("prompt = %+v, %v; want end_turn", resp, err)
	}
	if got := shell.inputs(); !slices.Equal(got, []string{`{"command":"ls"}`}) {
		t.Errorf("tool ran on %q, want the call's input once", got)
	}
	want := []string{"tool_call t1 pending", "tool_update t1 in_progress", "tool_update t1 completed", "agent: done"}
	if got := s.got.shown(); !slices.Equal(got, want) {
		t.Errorf("updates = %q, want %q", got, want)
	}
	last := s.got.toolUpdates()[1]
	if len(last.Content) != 1 || last.Content[0].Content == nil || last.Content[0].Content.Content.Text.Text != "a.txt" {
		t.Errorf("completed update content = %+v, want the text a.txt", last.Content)
	}
	if got := lastResults(s.model); !reflect.DeepEqual(got, []core.ToolResult{{ToolUseID: "t1", Content: "a.txt"}}) {
		t.Errorf("model got results %+v, want a.txt for t1", got)
	}
}

func TestDeniedToolDoesNotRunAndTheModelGetsTheDenialAsAnError(t *testing.T) {
	// given
	// ... a session whose model calls the shell, and a client that denies it
	shell := &fakeTool{name: "shell", out: "a.txt"}
	s := newToolSession(t, []core.Tool{shell}, modelReply{calls: []core.ToolCall{shellCall("t1", "rm")}}, modelReply{chunks: []string{"ok"}})
	s.got.answerWith(choosing(sdk.PermissionOptionKindRejectOnce))
	s.initialize(t)
	id := s.newSession(t)

	// when
	// ... the client prompts
	resp, err := s.client.Prompt(context.Background(), promptOf(id, "delete"))

	// then
	// ... the tool never ran, the update ended failed with the reason, and the model got it as an error result
	if err != nil || resp.StopReason != sdk.StopReasonEndTurn {
		t.Fatalf("prompt = %+v, %v; want end_turn", resp, err)
	}
	if got := shell.inputs(); len(got) != 0 {
		t.Errorf("tool ran on %q, want it not to run", got)
	}
	want := []string{"tool_call t1 pending", "tool_update t1 failed", "agent: ok"}
	if got := s.got.shown(); !slices.Equal(got, want) {
		t.Errorf("updates = %q, want %q", got, want)
	}
	if text := s.got.toolUpdates()[0].Content[0].Content.Content.Text.Text; text != "denied by the user" {
		t.Errorf("failed update content = %q, want the denial", text)
	}
	if got := lastResults(s.model); !reflect.DeepEqual(got, []core.ToolResult{{ToolUseID: "t1", Content: "denied by the user", IsError: true}}) {
		t.Errorf("model got results %+v, want the denial as an error for t1", got)
	}
}

func TestClientThatErrorsThePermissionRequestDeniesTheCall(t *testing.T) {
	// given
	// ... a session whose model calls the shell, and a client that fails the permission request
	shell := &fakeTool{name: "shell", out: "a.txt"}
	s := newToolSession(t, []core.Tool{shell}, modelReply{calls: []core.ToolCall{shellCall("t1", "ls")}}, modelReply{chunks: []string{"ok"}})
	s.got.answerWith(func(context.Context, sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
		return sdk.RequestPermissionResponse{}, errors.New("no user here")
	})
	s.initialize(t)
	id := s.newSession(t)

	// when
	// ... the client prompts
	_, err := s.client.Prompt(context.Background(), promptOf(id, "list"))

	// then
	// ... the tool never ran and the model got the client's error as an error result
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if got := shell.inputs(); len(got) != 0 {
		t.Errorf("tool ran on %q, want it not to run", got)
	}
	got := lastResults(s.model)
	if len(got) != 1 || !got[0].IsError || !strings.Contains(got[0].Content, "no user here") {
		t.Errorf("model got results %+v, want an error result carrying the client's error", got)
	}
}

func TestCancelWhilePermissionIsPendingEndsThePromptCancelledWithoutRunningTheTool(t *testing.T) {
	// given
	// ... a prompt whose tool call waits on a permission request the client never answers
	shell := &fakeTool{name: "shell", out: "a.txt"}
	asked := make(chan struct{})
	unanswered := make(chan struct{})
	t.Cleanup(func() { close(unanswered) })
	s := newToolSession(t, []core.Tool{shell}, modelReply{calls: []core.ToolCall{shellCall("t1", "ls")}})
	s.got.answerWith(func(context.Context, sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
		close(asked)
		<-unanswered
		return sdk.RequestPermissionResponse{}, errors.New("never answered")
	})
	s.initialize(t)
	id := s.newSession(t)
	pending := promptAsync(s, id, "list")
	await(t, asked)

	// when
	// ... the client cancels the session
	err := s.client.Cancel(context.Background(), sdk.CancelNotification{SessionId: id})

	// then
	// ... the prompt answers cancelled and the tool never ran
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	got := await(t, pending)
	if got.err != nil || got.resp.StopReason != sdk.StopReasonCancelled {
		t.Errorf("prompt = %+v, %v; want stopReason cancelled", got.resp, got.err)
	}
	if ran := shell.inputs(); len(ran) != 0 {
		t.Errorf("tool ran on %q, want it not to run", ran)
	}
}

func TestToolCallsInOneResponseAskPermissionIndependently(t *testing.T) {
	// given
	// ... a model that calls the shell twice in one response, and a client that answers neither request until it has both
	shell := &fakeTool{name: "shell", out: "ok"}
	arrived := make(chan string, 2)
	release := make(chan struct{})
	s := newToolSession(t, []core.Tool{shell},
		modelReply{calls: []core.ToolCall{shellCall("t1", "one"), shellCall("t2", "two")}},
		modelReply{chunks: []string{"done"}})
	allow := choosing(sdk.PermissionOptionKindAllowOnce)
	s.got.answerWith(func(ctx context.Context, req sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
		arrived <- string(req.ToolCall.ToolCallId)
		<-release
		return allow(ctx, req)
	})
	s.initialize(t)
	id := s.newSession(t)
	pending := promptAsync(s, id, "go")

	// when
	// ... both requests have arrived, then the client allows them
	first, second := await(t, arrived), await(t, arrived)
	close(release)

	// then
	// ... each call had its own request, and both ran
	got := await(t, pending)
	if got.err != nil || got.resp.StopReason != sdk.StopReasonEndTurn {
		t.Fatalf("prompt = %+v, %v; want end_turn", got.resp, got.err)
	}
	if ids := []string{first, second}; !(slices.Contains(ids, "t1") && slices.Contains(ids, "t2")) {
		t.Errorf("requests were for %q, want t1 and t2", ids)
	}
	if ran := shell.inputs(); len(ran) != 2 {
		t.Errorf("tool ran %d times, want 2", len(ran))
	}
}

func TestKeptTurnWithToolCallsReplaysInTheOrderItWasShown(t *testing.T) {
	// given
	// ... a kept turn with reply text, a tool call, then more reply text, left by its process
	shell := &fakeTool{name: "shell", out: "a.txt"}
	first := newToolSession(t, []core.Tool{shell},
		modelReply{chunks: []string{"let me look"}, calls: []core.ToolCall{shellCall("t1", "ls")}},
		modelReply{chunks: []string{"found it"}})
	first.got.answerWith(choosing(sdk.PermissionOptionKindAllowOnce))
	first.initialize(t)
	id := first.newSession(t)
	if _, err := first.client.Prompt(context.Background(), promptOf(id, "look")); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	second := connect(t)
	second.initialize(t)

	// when
	// ... a new process loads the session
	err := second.load(id)

	// then
	// ... the prompt, reply, tool call, its result and the rest of the reply replayed in order, with the call's details
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := []string{"user: look", "agent: let me look", "tool_call t1 pending", "tool_update t1 completed", "agent: found it"}
	if got := second.got.shown(); !slices.Equal(got, want) {
		t.Errorf("updates = %q, want %q", got, want)
	}
	call := second.got.toolCalls()[0]
	if call.Title != `shell {"command":"ls"}` || call.Kind != sdk.ToolKindExecute ||
		!reflect.DeepEqual(call.RawInput, map[string]any{"command": "ls"}) || !reflect.DeepEqual(call.Meta, map[string]any{"toolName": "shell"}) {
		t.Errorf("replayed tool_call = %+v, want the live one's title, kind, input and tool name", call)
	}
	if text := second.got.toolUpdates()[0].Content[0].Content.Content.Text.Text; text != "a.txt" {
		t.Errorf("replayed output = %q, want a.txt", text)
	}
}

// writeSession replaces a session's file with lines, as the next process
// would find it.
func writeSession(t *testing.T, id sdk.SessionId, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(sessionsDir(), string(id)+".jsonl"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readSessionFile(t *testing.T, id sdk.SessionId) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(sessionsDir(), string(id)+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSessionWithATornLastLineLoadsAndKeepsGoing(t *testing.T) {
	// given
	// ... a session with a kept turn whose file then gets half a record, the process having died mid-write
	first := newSession(t, modelReply{chunks: []string{"first"}})
	first.initialize(t)
	id := first.newSession(t)
	if _, err := first.client.Prompt(context.Background(), promptOf(id, "one")); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	writeSession(t, id, readSessionFile(t, id)+`{"turn":2,"display":{"kind":"pro`)
	second := connect(t, modelReply{chunks: []string{"second"}})
	second.initialize(t)

	// when
	// ... a new process loads it and the client prompts, then another process loads it again
	loadErr := second.load(id)
	_, promptErr := second.client.Prompt(context.Background(), promptOf(id, "two"))
	third := connect(t)
	third.initialize(t)
	thirdErr := third.load(id)

	// then
	// ... the kept turn replayed without the torn record, and what was appended after it is readable
	if loadErr != nil || promptErr != nil || thirdErr != nil {
		t.Fatalf("load: %v; prompt: %v; later load: %v", loadErr, promptErr, thirdErr)
	}
	if got, want := second.got.shown(), []string{"user: one", "agent: first", "agent: second"}; !slices.Equal(got, want) {
		t.Errorf("updates = %q, want %q", got, want)
	}
	if got, want := third.got.shown(), []string{"user: one", "agent: first", "user: two", "agent: second"}; !slices.Equal(got, want) {
		t.Errorf("later load updates = %q, want %q", got, want)
	}
}

func TestSessionWithABadLineInTheMiddleFailsToLoad(t *testing.T) {
	// given
	// ... a session file with a record that isn't JSON between two good ones
	first := newSession(t, modelReply{chunks: []string{"first"}})
	first.initialize(t)
	id := first.newSession(t)
	if _, err := first.client.Prompt(context.Background(), promptOf(id, "one")); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	good := readSessionFile(t, id)
	writeSession(t, id, good+"{\"turn\":2,\"disp\n"+good)
	second := connect(t)
	second.initialize(t)

	// when
	// ... a new process loads it
	err := second.load(id)

	// then
	// ... the load fails
	if err == nil {
		t.Error("load succeeded, want an error")
	}
}
