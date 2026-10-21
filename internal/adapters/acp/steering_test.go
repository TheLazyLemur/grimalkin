package acp_test

import (
	"context"
	"slices"
	"testing"

	sdk "github.com/coder/acp-go-sdk"

	"grimalkin/internal/core"
)

func steerParams(id sdk.SessionId, text string) map[string]any {
	return map[string]any{
		"sessionId": id,
		"prompt":    []sdk.ContentBlock{sdk.TextBlock(text)},
		"_meta":     map[string]any{"steering": map[string]any{"idleBehavior": "promptRequired"}},
	}
}

// pausedAtPermission answers permission requests by signalling asked, waiting
// for release, then allowing: a tool round the client holds open.
func pausedAtPermission(s *session, asked chan<- struct{}, release <-chan struct{}) {
	allow := choosing(sdk.PermissionOptionKindAllowOnce)
	s.got.answerWith(func(ctx context.Context, req sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
		close(asked)
		<-release
		return allow(ctx, req)
	})
}

func TestSteerDuringAToolRoundIsInjectedAndTheNextModelCallCarriesIt(t *testing.T) {
	// given
	// ... a turn held open at its tool call's permission request
	shell := &fakeTool{name: "shell", out: "a.txt"}
	asked, release := make(chan struct{}), make(chan struct{})
	s := newToolSession(t, []core.Tool{shell}, modelReply{calls: []core.ToolCall{shellCall("t1", "ls")}}, modelReply{chunks: []string{"done"}})
	pausedAtPermission(s, asked, release)
	s.initialize(t)
	id := s.newSession(t)
	pending := promptAsync(s, id, "list")
	await(t, asked)

	// when
	// ... the client steers, then the tool round finishes
	answer := s.raw(t, "_session/steering", steerParams(id, "use -a"))
	close(release)

	// then
	// ... the steer was injected, and the second model call has it with the tool's result
	if got := await(t, pending); got.err != nil {
		t.Fatalf("prompt: %v", got.err)
	}
	if answer.Error != nil || answer.Result["outcome"] != "injected" {
		t.Errorf("steer answered %+v, want injected", answer)
	}
	sent := s.model.sent()
	last := sent[1].Messages[len(sent[1].Messages)-1]
	if last.Text != "use -a" || len(last.ToolResults) != 1 {
		t.Errorf("second model call ends with %+v, want the tool result and the steer", last)
	}
	if got := s.got.shown(); slices.Contains(got, "user: use -a") {
		t.Errorf("updates = %q, want the steer not echoed", got)
	}
}

func TestSteerWhileIdleIsPromptRequiredAndNeverReachesTheModel(t *testing.T) {
	// given
	// ... a session with no turn running
	s := newSession(t, modelReply{chunks: []string{"ok"}})
	s.initialize(t)
	id := s.newSession(t)

	// when
	// ... the client steers
	answer := s.raw(t, "_session/steering", steerParams(id, "hello"))

	// then
	// ... the answer is promptRequired with its reason, and the model was never called
	if answer.Error != nil || answer.Result["outcome"] != "promptRequired" || answer.Result["reason"] != "noRunningTurn" {
		t.Errorf("steer answered %+v, want promptRequired / noRunningTurn", answer)
	}
	if n := len(s.model.sent()); n != 0 {
		t.Errorf("model called %d times, want 0", n)
	}
}

func TestSteerWithNoTextIsInvalidParams(t *testing.T) {
	// given
	// ... a session
	s := newSession(t)
	s.initialize(t)
	id := s.newSession(t)

	// when
	// ... the client steers with an empty text block
	answer := s.raw(t, "_session/steering", steerParams(id, ""))

	// then
	// ... the request is invalid params
	if answer.Error == nil || answer.Error.Code != -32602 {
		t.Errorf("steer answered %+v, want error -32602", answer)
	}
}

func TestSteerForAnotherSessionIsInvalidParams(t *testing.T) {
	// given
	// ... a session
	s := newSession(t)
	s.initialize(t)
	s.newSession(t)

	// when
	// ... the client steers a session that isn't this one
	answer := s.raw(t, "_session/steering", steerParams("0123456789abcdef0123456789abcdef", "hi"))

	// then
	// ... the request is invalid params
	if answer.Error == nil || answer.Error.Code != -32602 {
		t.Errorf("steer answered %+v, want error -32602", answer)
	}
}

// cancelledAfterSteer runs a turn that is steered and then cancelled while
// the model call is in flight, and returns its session and id.
func cancelledAfterSteer(t *testing.T, replies ...modelReply) (*session, sdk.SessionId) {
	t.Helper()
	began := make(chan struct{})
	s := newSession(t, append([]modelReply{{chunks: []string{"partial"}, hold: started(began, nil)}}, replies...)...)
	s.initialize(t)
	id := s.newSession(t)
	pending := promptAsync(s, id, "hi")
	await(t, began)
	if answer := s.raw(t, "_session/steering", steerParams(id, "actually, stop")); answer.Result["outcome"] != "injected" {
		t.Fatalf("steer answered %+v, want injected", answer)
	}
	_ = s.client.Cancel(context.Background(), sdk.CancelNotification{SessionId: id})
	if got := await(t, pending); got.err != nil || got.resp.StopReason != sdk.StopReasonCancelled {
		t.Fatalf("prompt = %+v, %v; want cancelled", got.resp, got.err)
	}
	return s, id
}

func TestSteerAcceptedThenCancelledReachesTheNextPromptsModelCall(t *testing.T) {
	// given
	// ... a steer accepted, then the turn cancelled
	s, id := cancelledAfterSteer(t, modelReply{chunks: []string{"ok"}})

	// when
	// ... the client prompts again
	_, err := s.client.Prompt(context.Background(), promptOf(id, "next"))

	// then
	// ... the model's call has the steer joined with the prompt, and not the cancelled turn's prompt
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	want := []string{"user: actually, stop\n\nnext"}
	if got := conversation(s.model.sent()[1].Messages); !slices.Equal(got, want) {
		t.Errorf("model saw %q, want %q", got, want)
	}
}

func TestSteerAcceptedThenCancelledReplaysAsAUserChunkAndSeedsTheHistory(t *testing.T) {
	// given
	// ... a steer accepted, then the turn cancelled, left by its process
	_, id := cancelledAfterSteer(t)
	second := connect(t, modelReply{chunks: []string{"ok"}})
	second.initialize(t)

	// when
	// ... a new process loads the session, then the client prompts
	loadErr := second.load(id)
	_, promptErr := second.client.Prompt(context.Background(), promptOf(id, "next"))

	// then
	// ... the replay shows the steer and not the cancelled turn's prompt or reply, and the model saw the steer
	if loadErr != nil || promptErr != nil {
		t.Fatalf("load: %v; prompt: %v", loadErr, promptErr)
	}
	want := []string{"user: actually, stop", "agent: ok"}
	if got := second.got.shown(); !slices.Equal(got, want) {
		t.Errorf("updates = %q, want %q", got, want)
	}
	wantSent := []string{"user: actually, stop\n\nnext"}
	if got := conversation(second.model.sent()[0].Messages); !slices.Equal(got, wantSent) {
		t.Errorf("model saw %q, want %q", got, wantSent)
	}
}

func TestKeptTurnWithASteerReplaysItInTheOrderItHappened(t *testing.T) {
	// given
	// ... a kept turn steered during its tool round, left by its process
	shell := &fakeTool{name: "shell", out: "a.txt"}
	asked, release := make(chan struct{}), make(chan struct{})
	first := newToolSession(t, []core.Tool{shell},
		modelReply{chunks: []string{"let me look"}, calls: []core.ToolCall{shellCall("t1", "ls")}},
		modelReply{chunks: []string{"found it"}})
	pausedAtPermission(first, asked, release)
	first.initialize(t)
	id := first.newSession(t)
	pending := promptAsync(first, id, "look")
	await(t, asked)
	first.raw(t, "_session/steering", steerParams(id, "use -a"))
	close(release)
	if got := await(t, pending); got.err != nil {
		t.Fatalf("prompt: %v", got.err)
	}
	second := connect(t)
	second.initialize(t)

	// when
	// ... a new process loads the session
	err := second.load(id)

	// then
	// ... the prompt, reply, tool call, its result, the steer and the rest of the reply replayed in order
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := []string{"user: look", "agent: let me look", "tool_call t1 pending", "tool_update t1 completed", "user: use -a", "agent: found it"}
	if got := second.got.shown(); !slices.Equal(got, want) {
		t.Errorf("updates = %q, want %q", got, want)
	}
}
