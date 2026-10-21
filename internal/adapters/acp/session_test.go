package acp_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	sdk "github.com/coder/acp-go-sdk"

	"grimalkin/internal/core"
)

func (s *session) load(id sdk.SessionId) error {
	_, err := s.client.LoadSession(context.Background(), sdk.LoadSessionRequest{SessionId: id, Cwd: "/work", McpServers: []sdk.McpServer{}})
	return err
}

// shown is each session update received, as "user: text", "agent: text",
// "tool_call id status" or "tool_update id status".
func (c *recordingClient) shown() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var lines []string
	for _, n := range c.updates {
		switch {
		case n.Update.UserMessageChunk != nil:
			lines = append(lines, "user: "+n.Update.UserMessageChunk.Content.Text.Text)
		case n.Update.AgentMessageChunk != nil:
			lines = append(lines, "agent: "+n.Update.AgentMessageChunk.Content.Text.Text)
		case n.Update.ToolCall != nil:
			lines = append(lines, "tool_call "+string(n.Update.ToolCall.ToolCallId)+" "+string(n.Update.ToolCall.Status))
		case n.Update.ToolCallUpdate != nil:
			lines = append(lines, "tool_update "+string(n.Update.ToolCallUpdate.ToolCallId)+" "+string(*n.Update.ToolCallUpdate.Status))
		default:
			lines = append(lines, "other update")
		}
	}
	return lines
}

// conversation is a request's messages as "user: text" or "assistant: content".
func conversation(messages []core.Message) []string {
	var lines []string
	for _, m := range messages {
		switch m.Role {
		case core.RoleUser:
			lines = append(lines, "user: "+m.Text)
		case core.RoleAssistant:
			lines = append(lines, "assistant: "+string(m.Content))
		}
	}
	return lines
}

func sessionsDir() string {
	return filepath.Join(os.Getenv("XDG_STATE_HOME"), "grimalkin", "sessions")
}

func requestCode(t *testing.T, err error) int {
	t.Helper()
	var reqErr *sdk.RequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("err = %v, want a JSON-RPC error", err)
	}
	return reqErr.Code
}

func TestSessionFileIsPrivateUnderTheStateDir(t *testing.T) {
	// given
	// ... a session
	s := newSession(t, modelReply{chunks: []string{"ok"}})
	s.initialize(t)
	id := s.newSession(t)

	// when
	// ... the client prompts
	_, err := s.client.Prompt(context.Background(), promptOf(id, "hi"))

	// then
	// ... the session's file is 0600 in a 0700 sessions directory
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	dir, err := os.Stat(sessionsDir())
	if err != nil {
		t.Fatalf("stat sessions dir: %v", err)
	}
	if mode := dir.Mode().Perm(); mode != 0o700 {
		t.Errorf("sessions dir mode = %o, want 700", mode)
	}
	file, err := os.Stat(filepath.Join(sessionsDir(), string(id)+".jsonl"))
	if err != nil {
		t.Fatalf("stat session file: %v", err)
	}
	if mode := file.Mode().Perm(); mode != 0o600 {
		t.Errorf("session file mode = %o, want 600", mode)
	}
}

func TestLoadReplaysTheSessionAndThePromptContinuesIt(t *testing.T) {
	// given
	// ... a session with two kept turns, in a process that is then left
	first := newSession(t, modelReply{chunks: []string{"fir", "st"}}, modelReply{chunks: []string{"second"}})
	first.initialize(t)
	id := first.newSession(t)
	if _, err := first.client.Prompt(context.Background(), promptOf(id, "one")); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if _, err := first.client.Prompt(context.Background(), promptOf(id, "two")); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	second := connect(t, modelReply{chunks: []string{"third"}})
	second.initialize(t)

	// when
	// ... a new process loads the session, then the client prompts
	loadErr := second.load(id)
	_, promptErr := second.client.Prompt(context.Background(), promptOf(id, "three"))

	// then
	// ... the kept turns were replayed in order before the answer, and the model saw them before the new prompt
	if loadErr != nil || promptErr != nil {
		t.Fatalf("load: %v; prompt: %v", loadErr, promptErr)
	}
	want := []string{"user: one", "agent: first", "user: two", "agent: second", "agent: third"}
	if got := second.got.shown(); !slices.Equal(got, want) {
		t.Errorf("updates = %q, want %q", got, want)
	}
	wantSent := []string{"user: one", `assistant: "assistant"`, "user: two", `assistant: "assistant"`, "user: three"}
	if got := conversation(second.model.sent()[0].Messages); !slices.Equal(got, wantSent) {
		t.Errorf("model saw %q, want %q", got, wantSent)
	}
}

func TestTurnsNotKeptAreNeitherReplayedNorRemembered(t *testing.T) {
	// given
	// ... a kept turn, a discarded turn that streamed text, a kept turn, then a cancelled turn
	began := make(chan struct{})
	first := newSession(t,
		modelReply{chunks: []string{"first"}},
		modelReply{chunks: []string{"partial"}, err: errors.New("overloaded")},
		modelReply{chunks: []string{"third"}},
		modelReply{hold: started(began, nil)},
	)
	first.initialize(t)
	id := first.newSession(t)
	_, _ = first.client.Prompt(context.Background(), promptOf(id, "one"))
	_, _ = first.client.Prompt(context.Background(), promptOf(id, "two"))
	_, _ = first.client.Prompt(context.Background(), promptOf(id, "three"))
	pending := promptAsync(first, id, "four")
	await(t, began)
	_ = first.client.Cancel(context.Background(), sdk.CancelNotification{SessionId: id})
	if got := await(t, pending); got.resp.StopReason != sdk.StopReasonCancelled {
		t.Fatalf("fourth prompt = %+v, %v; want cancelled", got.resp, got.err)
	}
	second := connect(t, modelReply{chunks: []string{"fifth"}})
	second.initialize(t)

	// when
	// ... a new process loads the session, then the client prompts
	loadErr := second.load(id)
	_, promptErr := second.client.Prompt(context.Background(), promptOf(id, "five"))

	// then
	// ... only the kept turns were replayed, and only they reached the model
	if loadErr != nil || promptErr != nil {
		t.Fatalf("load: %v; prompt: %v", loadErr, promptErr)
	}
	want := []string{"user: one", "agent: first", "user: three", "agent: third", "agent: fifth"}
	if got := second.got.shown(); !slices.Equal(got, want) {
		t.Errorf("updates = %q, want %q", got, want)
	}
	wantSent := []string{"user: one", `assistant: "assistant"`, "user: three", `assistant: "assistant"`, "user: five"}
	if got := conversation(second.model.sent()[0].Messages); !slices.Equal(got, wantSent) {
		t.Errorf("model saw %q, want %q", got, wantSent)
	}
}

func TestTurnsAfterALoadAreAppendedToTheSession(t *testing.T) {
	// given
	// ... a session with a kept turn, loaded by a second process that discards a turn and keeps one
	first := newSession(t, modelReply{chunks: []string{"first"}})
	first.initialize(t)
	id := first.newSession(t)
	_, _ = first.client.Prompt(context.Background(), promptOf(id, "one"))
	second := connect(t,
		modelReply{err: errors.New("overloaded")},
		modelReply{chunks: []string{"third"}},
	)
	second.initialize(t)
	if err := second.load(id); err != nil {
		t.Fatalf("load: %v", err)
	}
	_, _ = second.client.Prompt(context.Background(), promptOf(id, "two"))
	_, _ = second.client.Prompt(context.Background(), promptOf(id, "three"))
	third := connect(t)
	third.initialize(t)

	// when
	// ... a third process loads the session
	err := third.load(id)

	// then
	// ... both processes' kept turns are replayed, and the second's discarded one is not
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := []string{"user: one", "agent: first", "user: three", "agent: third"}
	if got := third.got.shown(); !slices.Equal(got, want) {
		t.Errorf("updates = %q, want %q", got, want)
	}
}

func TestLoadRefusesAnIdSessionNewCouldNotHaveMade(t *testing.T) {
	// given
	// ... a saved session at the path "../x" would reach from the sessions directory
	s := newSession(t)
	s.initialize(t)
	saved := `{"turn":1,"display":{"kind":"prompt","text":"planted"}}` + "\n" +
		`{"turn":1,"messages":[{"Role":0,"Text":"planted"},{"Role":1,"Content":"\"assistant\""}]}` + "\n"
	if err := os.MkdirAll(sessionsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionsDir(), "..", "x.jsonl"), []byte(saved), 0o600); err != nil {
		t.Fatal(err)
	}

	// when
	// ... the client loads session "../x"
	err := s.load("../x")

	// then
	// ... the request is refused as invalid params and nothing is replayed
	if code := requestCode(t, err); code != -32602 {
		t.Errorf("code = %d, want -32602", code)
	}
	if got := s.got.shown(); len(got) != 0 {
		t.Errorf("updates = %q, want none", got)
	}
}

func TestLoadOfAnUnknownSessionErrorsAndLeavesNoSession(t *testing.T) {
	// given
	// ... a connection with no saved sessions
	s := newSession(t)
	s.initialize(t)

	// when
	// ... the client loads a well-formed id nothing was saved under, then creates a session
	loadErr := s.load("0123456789abcdef0123456789abcdef")
	_, newErr := s.client.NewSession(context.Background(), sdk.NewSessionRequest{Cwd: "/work", McpServers: []sdk.McpServer{}})

	// then
	// ... the load errors, and the process still has no session
	if loadErr == nil {
		t.Error("load succeeded, want an error")
	}
	if newErr != nil {
		t.Errorf("session/new after the failed load: %v", newErr)
	}
}

func TestLoadAfterNewIsRefused(t *testing.T) {
	// given
	// ... a saved session, and a new process that has created its own
	first := newSession(t, modelReply{chunks: []string{"first"}})
	first.initialize(t)
	id := first.newSession(t)
	_, _ = first.client.Prompt(context.Background(), promptOf(id, "one"))
	second := connect(t)
	second.initialize(t)
	second.newSession(t)

	// when
	// ... the client loads the saved session
	err := second.load(id)

	// then
	// ... the load is refused and nothing is replayed
	if err == nil {
		t.Error("load succeeded, want an error")
	}
	if got := second.got.shown(); len(got) != 0 {
		t.Errorf("updates = %q, want none", got)
	}
}

func TestNewOrLoadAfterALoadIsRefused(t *testing.T) {
	// given
	// ... a new process that has loaded a saved session
	first := newSession(t, modelReply{chunks: []string{"first"}})
	first.initialize(t)
	id := first.newSession(t)
	_, _ = first.client.Prompt(context.Background(), promptOf(id, "one"))
	second := connect(t)
	second.initialize(t)
	if err := second.load(id); err != nil {
		t.Fatalf("load: %v", err)
	}

	// when
	// ... the client creates a session, then loads one again
	_, newErr := second.client.NewSession(context.Background(), sdk.NewSessionRequest{Cwd: "/work", McpServers: []sdk.McpServer{}})
	loadErr := second.load(id)

	// then
	// ... both are refused
	if newErr == nil {
		t.Error("session/new succeeded, want an error")
	}
	if loadErr == nil {
		t.Error("second session/load succeeded, want an error")
	}
}

func TestAFailedWriteMakesTheNextPromptError(t *testing.T) {
	// given
	// ... a turn whose session file turns read-only while it runs, then writable again
	began := make(chan struct{})
	release := make(chan struct{})
	s := newSession(t, modelReply{hold: started(began, release), chunks: []string{"first"}})
	s.initialize(t)
	id := s.newSession(t)
	file := filepath.Join(sessionsDir(), string(id)+".jsonl")
	pending := promptAsync(s, id, "one")
	await(t, began)
	if err := os.Chmod(file, 0o400); err != nil {
		t.Fatal(err)
	}
	close(release)
	if got := await(t, pending); got.err != nil {
		t.Fatalf("first prompt: %v", got.err)
	}
	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatal(err)
	}

	// when
	// ... the client prompts again
	_, err := s.client.Prompt(context.Background(), promptOf(id, "two"))

	// then
	// ... the prompt errors without calling the model
	if err == nil {
		t.Error("prompt succeeded, want an error")
	}
	if n := len(s.model.sent()); n != 1 {
		t.Errorf("model called %d times, want 1", n)
	}
}

func TestCloseOfASessionWithNoTurnsDeletesItsFile(t *testing.T) {
	// given
	// ... a session that switched models but never prompted
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := connectModels(t, newCatalogue())
	s.initialize(t)
	id := s.newSession(t)
	if _, err := s.set(id, "model", "acme/small"); err != nil {
		t.Fatalf("set: %v", err)
	}

	// when
	// ... the client closes it
	_, err := s.client.CloseSession(context.Background(), sdk.CloseSessionRequest{SessionId: id})

	// then
	// ... its file is gone
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sessionsDir(), string(id)+".jsonl")); !os.IsNotExist(err) {
		t.Errorf("stat session file: %v, want it not to exist", err)
	}
}

func TestCloseOfASessionWithATurnKeepsItsFileAndEndsTheSession(t *testing.T) {
	// given
	// ... a session with a kept turn
	s := newSession(t, modelReply{chunks: []string{"first"}})
	s.initialize(t)
	id := s.newSession(t)
	if _, err := s.client.Prompt(context.Background(), promptOf(id, "one")); err != nil {
		t.Fatalf("prompt: %v", err)
	}

	// when
	// ... the client closes it, then prompts
	_, closeErr := s.client.CloseSession(context.Background(), sdk.CloseSessionRequest{SessionId: id})
	_, promptErr := s.client.Prompt(context.Background(), promptOf(id, "two"))

	// then
	// ... the file is kept and the prompt is refused without calling the model
	if closeErr != nil {
		t.Fatalf("close: %v", closeErr)
	}
	if _, err := os.Stat(filepath.Join(sessionsDir(), string(id)+".jsonl")); err != nil {
		t.Errorf("stat session file: %v, want it kept", err)
	}
	if code := requestCode(t, promptErr); code != -32602 {
		t.Errorf("prompt code = %d, want -32602", code)
	}
	if n := len(s.model.sent()); n != 1 {
		t.Errorf("model called %d times, want 1", n)
	}
}

func TestCloseDuringATurnCancelsItFirst(t *testing.T) {
	// given
	// ... a prompt whose model call is in flight
	began := make(chan struct{})
	s := newSession(t, modelReply{hold: started(began, nil)})
	s.initialize(t)
	id := s.newSession(t)
	pending := promptAsync(s, id, "hi")
	await(t, began)

	// when
	// ... the client closes the session
	_, err := s.client.CloseSession(context.Background(), sdk.CloseSessionRequest{SessionId: id})

	// then
	// ... the close succeeded and the prompt answered cancelled
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	got := await(t, pending)
	if got.err != nil || got.resp.StopReason != sdk.StopReasonCancelled {
		t.Errorf("prompt = %+v, %v; want stopReason cancelled", got.resp, got.err)
	}
}
