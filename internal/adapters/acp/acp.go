// Package acp is the inbound adapter that lets an ACP client drive the core
// over JSON-RPC, as the TUI drives it with a keyboard: it runs a turn per
// session/prompt and turns the core's events into session/update
// notifications. See docs/specs/acp.md.
package acp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"strings"
	"sync"

	sdk "github.com/coder/acp-go-sdk"

	"grimalkin/internal/core"
)

// Agent is the part of the core this adapter drives, satisfied by
// *core.Agent. Its Transcript, History and Gate must be the Server that
// drives it.
type Agent interface {
	StartTurn(prompt string) (run func())
	Cancel()
	Seed(messages []core.Message) error
}

// NewAgent builds the agent for a session, once the client's tools are
// known: it must offer them beside its own.
type NewAgent func(clientTools []core.Tool) Agent

// Server is the ACP agent for one session per process. It is also the core's
// Transcript and History: wire it as the Agent's, then Serve that Agent.
type Server struct {
	mu        sync.Mutex
	reserved  map[string]bool // names the agent's own tools have; a client tool can't take one
	conn      *sdk.Connection
	newAgent  NewAgent
	agent     Agent         // nil until session/new or session/load
	starting  bool          // a session/new or session/load is connecting the client's tools
	sessionID sdk.SessionId // empty until session/new or session/load
	path      string        // the session's file, set with sessionID
	saveErr   error         // the first failed write; the session can't be saved after it
	turns     int           // the last turn number used in the session's file
	turn      *turn         // the running turn, nil when idle
	writeMu   sync.Mutex    // keeps session/update notifications in Show order
}

// turn is what the adapter learns while a turn runs.
type turn struct {
	n         int               // its number in the session's file
	reply     strings.Builder   // the reply text shown since the last record, saved when the turn is kept
	records   []record          // what the turn showed, in order, saved when it is kept
	summaries map[string]string // each tool call's summary by id, its title to the client
	cancelled bool              // session/cancel arrived
	discarded bool              // the core discarded the turn
	reason    string            // why, when discarded
}

// shown records what the client was shown, after the reply text shown before
// it, so the order is kept. s.mu must be held.
func (s *Server) shown(d display) {
	s.flushReply()
	s.turn.records = append(s.turn.records, record{Turn: s.turn.n, Display: &d})
}

// flushReply moves the reply text shown so far into the records. s.mu must
// be held.
func (s *Server) flushReply() {
	if s.turn.reply.Len() > 0 {
		s.turn.records = append(s.turn.records, record{Turn: s.turn.n, Display: &display{Kind: displayReply, Text: s.turn.reply.String()}})
		s.turn.reply.Reset()
	}
}

var (
	_ core.Transcript = (*Server)(nil)
	_ core.History    = (*Server)(nil)
	_ core.Gate       = (*Server)(nil)
)

// New is a Server whose agent's own tools are named reserved: a client tool
// with one of those names, or two with the same, is refused.
func New(reserved ...string) *Server {
	s := &Server{reserved: map[string]bool{}}
	for _, name := range reserved {
		s.reserved[name] = true
	}
	return s
}

// Serve speaks ACP on in and out until the client disconnects, building the
// session's agent with newAgent. The Connection is the SDK's, not an
// AgentSideConnection: that one can't send the mcp/message requests client
// tools need, so Server dispatches the requests itself (see handle).
func (s *Server) Serve(newAgent NewAgent, in io.Reader, out io.Writer) error {
	s.mu.Lock()
	s.newAgent = newAgent
	s.mu.Unlock()
	conn := sdk.NewConnection(s.handle, out, in)
	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()
	<-conn.Done()
	return nil
}

func (s *Server) Initialize(context.Context, sdk.InitializeRequest) (sdk.InitializeResponse, error) {
	// Later slices turn on steering as they are built.
	return sdk.InitializeResponse{
		ProtocolVersion:   sdk.ProtocolVersionNumber,
		AgentCapabilities: sdk.AgentCapabilities{LoadSession: true, McpCapabilities: sdk.McpCapabilities{Acp: true}},
	}, nil
}

func (s *Server) Authenticate(context.Context, sdk.AuthenticateRequest) (sdk.AuthenticateResponse, error) {
	return sdk.AuthenticateResponse{}, nil
}

func (s *Server) NewSession(ctx context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
	if err := s.beginStart(); err != nil {
		return sdk.NewSessionResponse{}, err
	}
	defer s.endStart()
	agent, err := s.buildAgent(ctx, p.McpServers)
	if err != nil {
		return sdk.NewSessionResponse{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Filename-safe: the session's file will be named after it.
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err)
	}
	id := hex.EncodeToString(raw[:])
	path, err := sessionPath(id)
	if err == nil {
		var f *os.File
		if f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
			err = f.Close()
		}
	}
	if err != nil {
		return sdk.NewSessionResponse{}, sdk.NewInternalError(map[string]any{"error": "create the session's file: " + err.Error()})
	}
	s.agent, s.sessionID, s.path = agent, sdk.SessionId(id), path
	return sdk.NewSessionResponse{SessionId: s.sessionID}, nil
}

// beginStart claims the one session this process has, until endStart: the
// client's tools are connected without s.mu held, as that waits on the client.
func (s *Server) beginStart() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionID != "" || s.starting {
		return sdk.NewInvalidRequest(map[string]any{"error": "this process already has a session"})
	}
	s.starting = true
	return nil
}

func (s *Server) endStart() {
	s.mu.Lock()
	s.starting = false
	s.mu.Unlock()
}

// buildAgent connects the client's tools and builds the agent that has them.
func (s *Server) buildAgent(ctx context.Context, servers []sdk.McpServer) (Agent, error) {
	s.mu.Lock()
	conn, newAgent := s.conn, s.newAgent
	s.mu.Unlock()
	tools, err := connectClientTools(ctx, conn, servers, s.reserved)
	if err != nil {
		return nil, sdk.NewInternalError(map[string]any{"error": err.Error()})
	}
	return newAgent(tools), nil
}

// LoadSession seeds the core with a saved session's kept turns and replays
// what the client was shown of them, then answers.
func (s *Server) LoadSession(ctx context.Context, p sdk.LoadSessionRequest) (sdk.LoadSessionResponse, error) {
	id := string(p.SessionId)
	if !sessionIDPattern.MatchString(id) {
		return sdk.LoadSessionResponse{}, sdk.NewInvalidParams(map[string]any{"error": "not a session id: " + id})
	}
	if err := s.beginStart(); err != nil {
		return sdk.LoadSessionResponse{}, err
	}
	defer s.endStart()
	agent, err := s.buildAgent(ctx, p.McpServers)
	if err != nil {
		return sdk.LoadSessionResponse{}, err
	}
	s.mu.Lock()
	saved, path, err := s.restore(agent, id)
	if err != nil {
		s.mu.Unlock()
		return sdk.LoadSessionResponse{}, err
	}
	s.agent, s.sessionID, s.path, s.turns = agent, p.SessionId, path, saved.lastTurn
	s.mu.Unlock()

	for _, d := range saved.shown {
		s.update(d.update())
	}
	return sdk.LoadSessionResponse{}, nil
}

// restore reads session id and seeds agent with it. s.mu must be held.
func (s *Server) restore(agent Agent, id string) (saved, string, error) {
	path, err := sessionPath(id)
	if err != nil {
		return saved{}, "", sdk.NewInternalError(map[string]any{"error": err.Error()})
	}
	got, err := readSession(path)
	if errors.Is(err, fs.ErrNotExist) {
		return saved{}, "", sdk.NewInvalidParams(map[string]any{"error": "unknown session " + id})
	}
	if err != nil {
		return saved{}, "", sdk.NewInternalError(map[string]any{"error": "read session " + id + ": " + err.Error()})
	}
	if err := agent.Seed(got.messages); err != nil {
		return saved{}, "", sdk.NewInternalError(map[string]any{"error": "restore session " + id + ": " + err.Error()})
	}
	return got, path, nil
}

// save appends records to the session's file. After a write fails, nothing
// more is written and every save returns that failure, so the file never
// has a hole in it. s.mu must be held.
func (s *Server) save(records ...record) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	if err := appendRecords(s.path, records...); err != nil {
		s.saveErr = fmt.Errorf("session %s can't be saved: %w", s.sessionID, err)
		log.Print("acp: ", s.saveErr)
	}
	return s.saveErr
}

// Keep saves a kept turn: what it showed, then its messages.
func (s *Server) Keep(messages []core.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn == nil {
		panic("turn kept with no turn running")
	}
	s.flushReply()
	// A failure is logged and refuses the next prompt; this turn still ends.
	_ = s.save(append(s.turn.records, record{Turn: s.turn.n, Messages: messages})...)
}

func (s *Server) Prompt(_ context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
	text := promptText(p.Prompt)
	if text == "" {
		return sdk.PromptResponse{}, sdk.NewInvalidParams(map[string]any{"error": "the prompt has no text"})
	}
	s.mu.Lock()
	if s.sessionID == "" || p.SessionId != s.sessionID {
		s.mu.Unlock()
		return sdk.PromptResponse{}, sdk.NewInvalidParams(map[string]any{"error": "unknown session " + string(p.SessionId)})
	}
	if s.turn != nil {
		s.mu.Unlock()
		return sdk.PromptResponse{}, sdk.NewInvalidRequest(map[string]any{"error": "a turn is already running"})
	}
	s.turns++
	t := &turn{n: s.turns, summaries: map[string]string{}}
	// Refused when the session can't be saved: it couldn't be resumed.
	if err := s.save(record{Turn: t.n, Display: &display{Kind: displayPrompt, Text: text}}); err != nil {
		s.mu.Unlock()
		return sdk.PromptResponse{}, sdk.NewInternalError(map[string]any{"error": err.Error()})
	}
	s.turn = t
	// Started before the lock is released, so a Cancel that sees the turn
	// finds it cancellable.
	run := s.agent.StartTurn(text)
	s.mu.Unlock()

	run()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.turn = nil
	switch {
	case t.cancelled:
		return sdk.PromptResponse{StopReason: sdk.StopReasonCancelled}, nil
	case t.discarded:
		return sdk.PromptResponse{}, &sdk.RequestError{
			Code:    -32603,
			Message: "turn discarded: " + t.reason,
			Data:    map[string]any{"reason": t.reason},
		}
	}
	return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
}

// promptText is the prompt's text blocks, and its resource links as their
// URIs: the baseline every agent supports. Nothing else is advertised.
func promptText(blocks []sdk.ContentBlock) string {
	var parts []string
	for _, b := range blocks {
		switch {
		case b.Text != nil:
			parts = append(parts, b.Text.Text)
		case b.ResourceLink != nil:
			parts = append(parts, b.ResourceLink.Uri)
		}
	}
	return strings.Join(parts, "\n")
}

func (s *Server) Cancel(_ context.Context, p sdk.CancelNotification) error {
	s.mu.Lock()
	if p.SessionId != s.sessionID || s.turn == nil {
		s.mu.Unlock()
		return nil
	}
	// Flagged before the core is cancelled, so the turn's end sees it.
	s.turn.cancelled = true
	agent := s.agent
	s.mu.Unlock()
	agent.Cancel()
	return nil
}

// Show is called from several goroutines at once while tool calls overlap.
func (s *Server) Show(agent int, event core.Event) {
	if agent != 0 {
		return // children are not built for ACP
	}
	switch e := event.(type) {
	case core.ReplyText:
		s.mu.Lock()
		if s.turn == nil {
			panic("reply text with no turn running")
		}
		s.turn.reply.WriteString(e.Text)
		s.mu.Unlock()
		s.update(sdk.UpdateAgentMessageText(e.Text))
	case core.ToolStarted:
		input := e.Input
		if !json.Valid(input) { // the model wrote it; the session file must stay readable
			input = nil
		}
		d := display{Kind: displayToolStart, ToolID: e.ID, Name: e.Name, Summary: e.Summary, Input: input}
		s.mu.Lock()
		if s.turn == nil {
			panic("tool started with no turn running")
		}
		s.turn.summaries[e.ID] = e.Summary
		s.shown(d)
		s.mu.Unlock()
		s.update(d.update())
	case core.ToolFinished:
		d := display{Kind: displayToolEnd, ToolID: e.ID, Text: e.Output, Failed: e.IsError}
		s.mu.Lock()
		if s.turn == nil {
			panic("tool finished with no turn running")
		}
		s.shown(d)
		s.mu.Unlock()
		s.update(d.update())
	case core.TurnDiscarded:
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.turn == nil {
			panic("turn discarded with no turn running")
		}
		s.turn.discarded, s.turn.reason = true, e.Reason
	}
}

// Allow asks the client whether a tool call may run. A call is allowed only
// by the client selecting the allow option; anything else, from a denial to
// a failed request, stops it.
func (s *Server) Allow(ctx context.Context, call core.ToolCall) (bool, string) {
	s.mu.Lock()
	conn, id := s.conn, s.sessionID
	if s.turn == nil {
		panic("tool call gated with no turn running")
	}
	title := s.turn.summaries[call.ID]
	s.mu.Unlock()
	kind := toolKind(call.Name)
	req := sdk.RequestPermissionRequest{
		SessionId: id,
		ToolCall: sdk.ToolCallUpdate{
			ToolCallId: sdk.ToolCallId(call.ID),
			Title:      &title,
			Kind:       &kind,
			RawInput:   rawInput(call.Input),
			Meta:       map[string]any{"toolName": call.Name},
		},
		Options: []sdk.PermissionOption{
			{OptionId: optionAllow, Name: "Allow", Kind: sdk.PermissionOptionKindAllowOnce},
			{OptionId: optionReject, Name: "Deny", Kind: sdk.PermissionOptionKindRejectOnce},
		},
	}
	resp, err := sdk.SendRequest[sdk.RequestPermissionResponse](conn, ctx, sdk.ClientMethodSessionRequestPermission, req)
	switch {
	case ctx.Err() != nil:
		return false, "cancelled"
	case err != nil:
		return false, "permission request failed: " + err.Error()
	case resp.Outcome.Selected == nil:
		return false, "cancelled"
	case resp.Outcome.Selected.OptionId == optionAllow:
		s.update(sdk.UpdateToolCall(sdk.ToolCallId(call.ID), sdk.WithUpdateStatus(sdk.ToolCallStatusInProgress)))
		return true, ""
	case resp.Outcome.Selected.OptionId == optionReject:
		return false, "denied by the user"
	}
	// An option we never offered: not an allow.
	return false, "the client answered with an unknown option " + string(resp.Outcome.Selected.OptionId)
}

const (
	optionAllow  sdk.PermissionOptionId = "allow"
	optionReject sdk.PermissionOptionId = "reject"
)

// toolKind is the ACP kind of a grimalkin tool, by the name the model uses.
func toolKind(name string) sdk.ToolKind {
	switch name {
	case "shell":
		return sdk.ToolKindExecute
	case "search":
		return sdk.ToolKindSearch
	case "web_search", "web_fetch":
		return sdk.ToolKindFetch
	case core.SkillToolName:
		return sdk.ToolKindRead
	}
	return sdk.ToolKindOther
}

// rawInput is a tool call's input for the wire, or nil when it has none.
func rawInput(input json.RawMessage) any {
	if len(input) == 0 {
		return nil
	}
	return input
}

// update is what the client is sent for a thing it was shown, live or on
// replay. A tool's start is pending until the client allows it.
func (d display) update() sdk.SessionUpdate {
	switch d.Kind {
	case displayPrompt:
		return sdk.UpdateUserMessageText(d.Text)
	case displayReply:
		return sdk.UpdateAgentMessageText(d.Text)
	case displayToolStart:
		return sdk.StartToolCall(sdk.ToolCallId(d.ToolID), d.Summary,
			sdk.WithStartKind(toolKind(d.Name)),
			sdk.WithStartStatus(sdk.ToolCallStatusPending),
			sdk.WithStartRawInput(rawInput(d.Input)),
			func(tc *sdk.SessionUpdateToolCall) { tc.Meta = map[string]any{"toolName": d.Name} })
	case displayToolEnd:
		status := sdk.ToolCallStatusCompleted
		if d.Failed {
			status = sdk.ToolCallStatusFailed
		}
		opts := []sdk.ToolCallUpdateOpt{sdk.WithUpdateStatus(status)}
		if d.Text != "" {
			opts = append(opts, sdk.WithUpdateContent([]sdk.ToolCallContent{sdk.ToolContent(sdk.TextBlock(d.Text))}))
		}
		return sdk.UpdateToolCall(sdk.ToolCallId(d.ToolID), opts...)
	}
	panic("display kind readSession let through: " + d.Kind)
}

func (s *Server) update(u sdk.SessionUpdate) {
	s.mu.Lock()
	conn, id := s.conn, s.sessionID
	s.mu.Unlock()
	if conn == nil || id == "" {
		panic("session update before a connection and session")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := conn.SendNotification(context.Background(), sdk.ClientMethodSessionUpdate, sdk.SessionNotification{SessionId: id, Update: u}); err != nil {
		log.Print(fmt.Errorf("acp: session/update: %w", err))
	}
}
