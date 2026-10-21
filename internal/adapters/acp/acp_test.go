package acp_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	sdk "github.com/coder/acp-go-sdk"

	"grimalkin/internal/adapters/acp"
	"grimalkin/internal/core"
)

// scriptedModel is a core.Model that takes the next scripted reply on each
// call, and records the requests it was sent.
type scriptedModel struct {
	replies []modelReply

	mu       sync.Mutex
	requests []core.ModelRequest
}

type modelReply struct {
	chunks []string // streamed before the response
	err    error    // non-nil: the call fails with this error
	// hold, when set, runs once the call has started and may block: it gets
	// the call's context, and a non-nil error from it fails the call.
	hold func(ctx context.Context) error
}

func (m *scriptedModel) Stream(ctx context.Context, req core.ModelRequest, onText func(string)) (core.ModelResponse, error) {
	m.mu.Lock()
	if len(m.requests) >= len(m.replies) {
		m.mu.Unlock()
		panic("scripted model called more times than scripted")
	}
	reply := m.replies[len(m.requests)]
	m.requests = append(m.requests, req)
	m.mu.Unlock()

	if reply.hold != nil {
		if err := reply.hold(ctx); err != nil {
			return core.ModelResponse{}, err
		}
	}
	for _, chunk := range reply.chunks {
		onText(chunk)
	}
	if reply.err != nil {
		return core.ModelResponse{}, reply.err
	}
	return core.ModelResponse{StopReason: core.StopEndTurn, Content: json.RawMessage(`"assistant"`)}, nil
}

func (m *scriptedModel) sent() []core.ModelRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.requests)
}

// recordingClient is the ACP client side: it records every session update.
// Any other client method panics, since slice 1 has no use for one.
type recordingClient struct {
	sdk.Client

	mu      sync.Mutex
	updates []sdk.SessionNotification
}

func (c *recordingClient) SessionUpdate(_ context.Context, n sdk.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updates = append(c.updates, n)
	return nil
}

// replyChunks is the text of each agent_message_chunk received, in order.
func (c *recordingClient) replyChunks() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var chunks []string
	for _, n := range c.updates {
		chunks = append(chunks, n.Update.AgentMessageChunk.Content.Text.Text)
	}
	return chunks
}

// session is an ACP client wired to the adapter over in-memory pipes, the
// adapter driving a real core.Agent on a scripted model.
type session struct {
	client *sdk.ClientSideConnection
	model  *scriptedModel
	got    *recordingClient
}

// newSession connects a client to a fresh adapter whose sessions are kept
// in a new state directory.
func newSession(t *testing.T, replies ...modelReply) *session {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	return connect(t, replies...)
}

// connect connects a client to a fresh adapter and core.Agent, as a new
// process would be, on the state directory the test already set.
func connect(t *testing.T, replies ...modelReply) *session {
	t.Helper()
	model := &scriptedModel{replies: replies}
	server := acp.New()
	agent := &core.Agent{Model: model, Transcript: server, History: server, System: "system"}

	toAgentR, toAgentW := io.Pipe()
	toClientR, toClientW := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(agent, toAgentR, toClientW)
	}()
	got := &recordingClient{}
	client := sdk.NewClientSideConnection(got, toAgentW, toClientR)
	t.Cleanup(func() {
		_ = toAgentW.Close()
		_ = toClientW.Close()
		<-done
	})
	return &session{client: client, model: model, got: got}
}

func (s *session) initialize(t *testing.T) sdk.InitializeResponse {
	t.Helper()
	resp, err := s.client.Initialize(context.Background(), sdk.InitializeRequest{ProtocolVersion: sdk.ProtocolVersionNumber})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return resp
}

func (s *session) newSession(t *testing.T) sdk.SessionId {
	t.Helper()
	resp, err := s.client.NewSession(context.Background(), sdk.NewSessionRequest{Cwd: "/work", McpServers: []sdk.McpServer{}})
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}
	return resp.SessionId
}

func promptOf(id sdk.SessionId, text string) sdk.PromptRequest {
	return sdk.PromptRequest{SessionId: id, Prompt: []sdk.ContentBlock{sdk.TextBlock(text)}}
}

// started is a hold that signals a call has begun, then waits to be released
// or for the call's context to be cancelled.
func started(began chan<- struct{}, release <-chan struct{}) func(context.Context) error {
	return func(ctx context.Context) error {
		close(began)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// promptAsync sends a prompt on its own goroutine and returns the channel its
// outcome arrives on.
type outcome struct {
	resp sdk.PromptResponse
	err  error
}

func promptAsync(s *session, id sdk.SessionId, text string) <-chan outcome {
	out := make(chan outcome, 1)
	go func() {
		resp, err := s.client.Prompt(context.Background(), promptOf(id, text))
		out <- outcome{resp, err}
	}()
	return out
}

func await[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting")
		panic("unreachable")
	}
}

func TestInitializeAdvertisesLoadSessionOnly(t *testing.T) {
	// given
	// ... a client connected to the adapter
	s := newSession(t)

	// when
	// ... the client initializes
	resp := s.initialize(t)

	// then
	// ... protocol version 1 with loadSession, but no MCP-over-ACP and no steering
	if resp.ProtocolVersion != 1 {
		t.Errorf("protocolVersion = %d, want 1", resp.ProtocolVersion)
	}
	caps := resp.AgentCapabilities
	if !caps.LoadSession || caps.McpCapabilities.Acp || caps.Meta != nil {
		t.Errorf("capabilities = %+v, want loadSession only", caps)
	}
}

func TestSessionNewReturnsAFilenameSafeIdAndRefusesASecond(t *testing.T) {
	// given
	// ... an initialized connection
	s := newSession(t)
	s.initialize(t)

	// when
	// ... the client creates a session, then another
	id := s.newSession(t)
	_, secondErr := s.client.NewSession(context.Background(), sdk.NewSessionRequest{Cwd: "/work", McpServers: []sdk.McpServer{}})

	// then
	// ... the first has a lowercase hex id and the second errors
	if len(id) != 32 || !isLowerHex(string(id)) {
		t.Errorf("sessionId = %q, want 32 lowercase hex characters", id)
	}
	if secondErr == nil {
		t.Error("second session/new succeeded, want an error")
	}
}

func isLowerHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func TestPromptStreamsReplyTextAndEndsTurn(t *testing.T) {
	// given
	// ... a session whose model replies in two chunks
	s := newSession(t, modelReply{chunks: []string{"Hel", "lo"}})
	s.initialize(t)
	id := s.newSession(t)

	// when
	// ... the client prompts
	resp, err := s.client.Prompt(context.Background(), promptOf(id, "hi"))

	// then
	// ... each chunk arrived as an agent_message_chunk, the prompt text reached the model and the turn ended
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if resp.StopReason != sdk.StopReasonEndTurn {
		t.Errorf("stopReason = %q, want end_turn", resp.StopReason)
	}
	if got := s.got.replyChunks(); !slices.Equal(got, []string{"Hel", "lo"}) {
		t.Errorf("chunks = %q, want [Hel lo]", got)
	}
	if got := s.got.updates[0].SessionId; got != id {
		t.Errorf("update sessionId = %q, want %q", got, id)
	}
	if sent := s.model.sent(); len(sent) != 1 || sent[0].Messages[0].Text != "hi" {
		t.Errorf("model requests = %+v, want one carrying the prompt text", sent)
	}
}

func TestModelIsOfferedNoTools(t *testing.T) {
	// given
	// ... a session
	s := newSession(t, modelReply{chunks: []string{"ok"}})
	s.initialize(t)
	id := s.newSession(t)

	// when
	// ... the client prompts
	_, err := s.client.Prompt(context.Background(), promptOf(id, "hi"))

	// then
	// ... the model's request listed no tools
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if tools := s.model.sent()[0].Tools; len(tools) != 0 {
		t.Errorf("tools = %+v, want none", tools)
	}
}

func TestCancelEndsThePromptCancelled(t *testing.T) {
	// given
	// ... a prompt whose model call is in flight
	began := make(chan struct{})
	s := newSession(t, modelReply{hold: started(began, nil)})
	s.initialize(t)
	id := s.newSession(t)
	pending := promptAsync(s, id, "hi")
	await(t, began)

	// when
	// ... the client cancels the session
	err := s.client.Cancel(context.Background(), sdk.CancelNotification{SessionId: id})

	// then
	// ... the prompt answers cancelled
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	got := await(t, pending)
	if got.err != nil || got.resp.StopReason != sdk.StopReasonCancelled {
		t.Errorf("prompt = %+v, %v; want stopReason cancelled", got.resp, got.err)
	}
}

func TestDiscardedTurnAnswersWithTheReasonAsAnError(t *testing.T) {
	// given
	// ... a model call that fails
	s := newSession(t, modelReply{err: errors.New("invalid x-api-key")})
	s.initialize(t)
	id := s.newSession(t)

	// when
	// ... the client prompts
	_, err := s.client.Prompt(context.Background(), promptOf(id, "hi"))

	// then
	// ... the prompt errors with the discard reason
	var reqErr *sdk.RequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("err = %v, want a JSON-RPC error", err)
	}
	if want := "turn discarded: invalid x-api-key"; reqErr.Message != want {
		t.Errorf("message = %q, want %q", reqErr.Message, want)
	}
}

func TestPromptWhileATurnRunsIsRefused(t *testing.T) {
	// given
	// ... a prompt whose model call is in flight
	began := make(chan struct{})
	release := make(chan struct{})
	s := newSession(t, modelReply{hold: started(began, release), chunks: []string{"done"}})
	s.initialize(t)
	id := s.newSession(t)
	first := promptAsync(s, id, "first")
	await(t, began)

	// when
	// ... the client prompts again, then the first turn finishes
	_, secondErr := s.client.Prompt(context.Background(), promptOf(id, "second"))
	close(release)

	// then
	// ... the second prompt errored and the first still ended its turn
	if secondErr == nil {
		t.Error("second prompt succeeded, want an error")
	}
	got := await(t, first)
	if got.err != nil || got.resp.StopReason != sdk.StopReasonEndTurn {
		t.Errorf("first prompt = %+v, %v; want end_turn", got.resp, got.err)
	}
	if n := len(s.model.sent()); n != 1 {
		t.Errorf("model called %d times, want 1", n)
	}
}
