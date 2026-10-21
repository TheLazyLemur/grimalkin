package acp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"
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
	chunks []string        // streamed before the response
	calls  []core.ToolCall // non-empty: the response asks for these tools
	err    error           // non-nil: the call fails with this error
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
	if len(reply.calls) > 0 {
		return core.ModelResponse{StopReason: core.StopToolUse, ToolCalls: reply.calls, Content: json.RawMessage(`"assistant"`)}, nil
	}
	return core.ModelResponse{StopReason: core.StopEndTurn, Content: json.RawMessage(`"assistant"`)}, nil
}

func (m *scriptedModel) sent() []core.ModelRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.requests)
}

// recordingClient is the ACP client side: it records every session update and
// permission request, and answers a request as answer says. Any other client
// method panics.
type recordingClient struct {
	sdk.Client

	mu       sync.Mutex
	updates  []sdk.SessionNotification
	requests []sdk.RequestPermissionRequest
	answer   func(ctx context.Context, req sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error)

	// MCP-over-ACP: the client's servers. connect answers mcp/connect, and
	// mcp answers each mcp/message request; both default to succeeding.
	connect  func(acpID string) (string, error)
	mcp      func(connectionID, method string, params map[string]any) (any, error)
	connects []string
	messages []mcpMessage

	// raw receives the agent's answers to requests the SDK's client can't send
	// (see session.raw): those whose id starts with "raw-".
	raw chan json.RawMessage
}

// mcpMessage is an mcp/message the agent sent.
type mcpMessage struct {
	connectionID, method string
	params               map[string]any
}

func (c *recordingClient) UnstableConnectMcp(_ context.Context, req sdk.UnstableConnectMcpRequest) (sdk.UnstableConnectMcpResponse, error) {
	c.mu.Lock()
	c.connects = append(c.connects, string(req.AcpId))
	connect := c.connect
	c.mu.Unlock()
	if connect == nil {
		return sdk.UnstableConnectMcpResponse{ConnectionId: sdk.UnstableMcpConnectionId("conn-" + req.AcpId)}, nil
	}
	id, err := connect(string(req.AcpId))
	return sdk.UnstableConnectMcpResponse{ConnectionId: sdk.UnstableMcpConnectionId(id)}, err
}

// mcpMethods is the method of each mcp/message the agent sent, in order.
func (c *recordingClient) mcpMethods() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var methods []string
	for _, m := range c.messages {
		methods = append(methods, m.method)
	}
	return methods
}

// serveMcp is the plumbing the SDK's client side lacks: it doesn't dispatch
// an inbound mcp/message to the Client, so the lines the agent writes pass
// through here first. An mcp/message is answered by got.mcp, straight onto
// toAgent; everything else goes on to the SDK's client.
func (c *recordingClient) serveMcp(fromAgent io.Reader, toClient io.WriteCloser, toAgent io.Writer) {
	defer toClient.Close()
	lines := bufio.NewReader(fromAgent)
	for {
		line, err := lines.ReadBytes('\n')
		if err != nil {
			return
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				ConnectionId string         `json:"connectionId"`
				Method       string         `json:"method"`
				Params       map[string]any `json:"params"`
			} `json:"params"`
		}
		if json.Unmarshal(line, &msg) == nil && msg.Method == "" && strings.HasPrefix(string(msg.ID), `"raw-`) {
			c.raw <- line
			continue
		}
		if json.Unmarshal(line, &msg) != nil || msg.Method != sdk.ClientMethodMcpMessage {
			_, _ = toClient.Write(line)
			continue
		}
		c.mu.Lock()
		c.messages = append(c.messages, mcpMessage{msg.Params.ConnectionId, msg.Params.Method, msg.Params.Params})
		answer := c.mcp
		c.mu.Unlock()
		if msg.ID == nil { // a notification
			continue
		}
		reply := map[string]any{"jsonrpc": "2.0", "id": msg.ID}
		if result, err := answer(msg.Params.ConnectionId, msg.Params.Method, msg.Params.Params); err != nil {
			reply["error"] = map[string]any{"code": -32603, "message": err.Error()}
		} else {
			reply["result"] = result
		}
		out, _ := json.Marshal(reply)
		_, _ = toAgent.Write(append(out, '\n'))
	}
}

func (c *recordingClient) RequestPermission(ctx context.Context, req sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	answer := c.answer
	c.mu.Unlock()
	return answer(ctx, req)
}

// answerWith sets how permission requests are answered.
func (c *recordingClient) answerWith(answer func(ctx context.Context, req sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.answer = answer
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

	toAgent io.Writer
	rawSeq  int
}

// rawAnswer is the agent's answer to a request sent with session.raw.
type rawAnswer struct {
	Result map[string]any `json:"result"`
	Error  *struct {
		Code int `json:"code"`
	} `json:"error"`
}

// raw sends a request the SDK's client has no method for and waits for the
// agent's answer.
func (s *session) raw(t *testing.T, method string, params any) rawAnswer {
	t.Helper()
	s.rawSeq++
	line, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "raw-" + strconv.Itoa(s.rawSeq), "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.toAgent.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	var answer rawAnswer
	if err := json.Unmarshal(await(t, s.got.raw), &answer); err != nil {
		t.Fatal(err)
	}
	return answer
}

// newSession connects a client to a fresh adapter whose sessions are kept
// in a new state directory.
func newSession(t *testing.T, replies ...modelReply) *session {
	t.Helper()
	return newToolSession(t, nil, replies...)
}

// newToolSession is newSession with tools for the model.
func newToolSession(t *testing.T, tools []core.Tool, replies ...modelReply) *session {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	return connectTools(t, tools, replies...)
}

// connect connects a client to a fresh adapter and core.Agent, as a new
// process would be, on the state directory the test already set.
func connect(t *testing.T, replies ...modelReply) *session {
	t.Helper()
	return connectTools(t, nil, replies...)
}

func connectTools(t *testing.T, tools []core.Tool, replies ...modelReply) *session {
	t.Helper()
	only := acp.Models{
		List:    []acp.Model{{Name: "test/model"}},
		Default: "test/model",
		Build:   func(string, string) (core.Model, error) { panic("no other model to build") },
	}
	return connectWith(t, tools, only, &scriptedModel{replies: replies})
}

// connectWith connects a client to a fresh adapter offering models, whose
// core.Agent starts on model, the default.
func connectWith(t *testing.T, tools []core.Tool, models acp.Models, model *scriptedModel) *session {
	t.Helper()
	reserved := []string{core.SkillToolName, core.AgentToolName}
	for _, tool := range tools {
		reserved = append(reserved, tool.Spec().Name)
	}
	server := acp.New(models, reserved...)
	newAgent := func(clientTools []core.Tool) acp.Agent {
		all := append(slices.Clone(tools), clientTools...)
		return &core.Agent{Model: model, ModelName: models.Default, Transcript: server, History: server, Gate: server, Tools: all, System: "system"}
	}

	toAgentR, toAgentW := io.Pipe()
	toClientR, toClientW := io.Pipe()
	filteredR, filteredW := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(newAgent, toAgentR, toClientW)
	}()
	got := &recordingClient{raw: make(chan json.RawMessage, 1)}
	go got.serveMcp(toClientR, filteredW, toAgentW)
	client := sdk.NewClientSideConnection(got, toAgentW, filteredR)
	t.Cleanup(func() {
		_ = toAgentW.Close()
		_ = toClientW.Close()
		<-done
	})
	return &session{client: client, model: model, got: got, toAgent: toAgentW}
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

func TestInitializeAdvertisesLoadSessionMcpOverAcpSteeringAndClose(t *testing.T) {
	// given
	// ... a client connected to the adapter
	s := newSession(t)

	// when
	// ... the client initializes
	resp := s.initialize(t)

	// then
	// ... protocol version 1 with loadSession, MCP-over-ACP and session/close,
	// ... and steering in the top-level _meta
	if resp.ProtocolVersion != 1 {
		t.Errorf("protocolVersion = %d, want 1", resp.ProtocolVersion)
	}
	caps := resp.AgentCapabilities
	if !caps.LoadSession || !caps.McpCapabilities.Acp || caps.McpCapabilities.Http || caps.McpCapabilities.Sse || caps.Meta != nil {
		t.Errorf("capabilities = %+v, want loadSession and mcpCapabilities.acp only", caps)
	}
	if want := (sdk.SessionCapabilities{Close: &sdk.SessionCloseCapabilities{}}); !reflect.DeepEqual(caps.SessionCapabilities, want) {
		t.Errorf("sessionCapabilities = %+v, want close only", caps.SessionCapabilities)
	}
	if want := map[string]any{"steering": map[string]any{"supported": true}}; !reflect.DeepEqual(resp.Meta, want) {
		t.Errorf("_meta = %v, want %v", resp.Meta, want)
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
