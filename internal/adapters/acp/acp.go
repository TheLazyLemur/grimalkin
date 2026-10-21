// Package acp is the inbound adapter that lets an ACP client drive the core
// over JSON-RPC, as the TUI drives it with a keyboard: it runs a turn per
// session/prompt and turns the core's events into session/update
// notifications. See docs/specs/acp.md.
package acp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"

	sdk "github.com/coder/acp-go-sdk"

	"grimalkin/internal/core"
)

// Agent is the part of the core this adapter drives, satisfied by
// *core.Agent. Its Transcript must be the Server that drives it.
type Agent interface {
	StartTurn(prompt string) (run func())
	Cancel()
}

// Server is the ACP agent for one session per process. It is also the core's
// Transcript: wire it as the Agent's, then Serve that Agent.
type Server struct {
	mu        sync.Mutex
	conn      *sdk.AgentSideConnection
	agent     Agent
	sessionID sdk.SessionId // empty until session/new
	turn      *turn         // the running turn, nil when idle
	writeMu   sync.Mutex    // keeps session/update notifications in Show order
}

// turn is what the adapter learns while a turn runs.
type turn struct {
	cancelled bool   // session/cancel arrived
	discarded bool   // the core discarded the turn
	reason    string // why, when discarded
}

var (
	_ sdk.Agent       = (*Server)(nil)
	_ core.Transcript = (*Server)(nil)
)

func New() *Server { return &Server{} }

// Serve speaks ACP on in and out until the client disconnects.
func (s *Server) Serve(agent Agent, in io.Reader, out io.Writer) error {
	s.mu.Lock()
	s.agent = agent
	s.mu.Unlock()
	conn := sdk.NewAgentSideConnection(s, out, in)
	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()
	<-conn.Done()
	return nil
}

func (s *Server) Initialize(context.Context, sdk.InitializeRequest) (sdk.InitializeResponse, error) {
	// Baseline only: later slices turn on loadSession, MCP-over-ACP and
	// steering as they are built.
	return sdk.InitializeResponse{ProtocolVersion: sdk.ProtocolVersionNumber}, nil
}

func (s *Server) Authenticate(context.Context, sdk.AuthenticateRequest) (sdk.AuthenticateResponse, error) {
	return sdk.AuthenticateResponse{}, nil
}

func (s *Server) NewSession(context.Context, sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionID != "" {
		return sdk.NewSessionResponse{}, sdk.NewInvalidRequest(map[string]any{"error": "this process already has a session"})
	}
	// Filename-safe: the session's file will be named after it.
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err)
	}
	s.sessionID = sdk.SessionId(hex.EncodeToString(raw[:]))
	return sdk.NewSessionResponse{SessionId: s.sessionID}, nil
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
	t := &turn{}
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
		s.update(sdk.UpdateAgentMessageText(e.Text))
	case core.TurnDiscarded:
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.turn == nil {
			panic("turn discarded with no turn running")
		}
		s.turn.discarded, s.turn.reason = true, e.Reason
	}
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
	if err := conn.SessionUpdate(context.Background(), sdk.SessionNotification{SessionId: id, Update: u}); err != nil {
		log.Print(fmt.Errorf("acp: session/update: %w", err))
	}
}

// Not supported in this slice.

func (s *Server) SetSessionMode(context.Context, sdk.SetSessionModeRequest) (sdk.SetSessionModeResponse, error) {
	return sdk.SetSessionModeResponse{}, sdk.NewMethodNotFound(sdk.AgentMethodSessionSetMode)
}

func (s *Server) SetSessionConfigOption(context.Context, sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
	return sdk.SetSessionConfigOptionResponse{}, sdk.NewMethodNotFound(sdk.AgentMethodSessionSetConfigOption)
}

func (s *Server) ListSessions(context.Context, sdk.ListSessionsRequest) (sdk.ListSessionsResponse, error) {
	return sdk.ListSessionsResponse{}, sdk.NewMethodNotFound(sdk.AgentMethodSessionList)
}

func (s *Server) ResumeSession(context.Context, sdk.ResumeSessionRequest) (sdk.ResumeSessionResponse, error) {
	return sdk.ResumeSessionResponse{}, sdk.NewMethodNotFound(sdk.AgentMethodSessionResume)
}

func (s *Server) CloseSession(context.Context, sdk.CloseSessionRequest) (sdk.CloseSessionResponse, error) {
	return sdk.CloseSessionResponse{}, sdk.NewMethodNotFound(sdk.AgentMethodSessionClose)
}

func (s *Server) Logout(context.Context, sdk.LogoutRequest) (sdk.LogoutResponse, error) {
	return sdk.LogoutResponse{}, sdk.NewMethodNotFound(sdk.AgentMethodLogout)
}
