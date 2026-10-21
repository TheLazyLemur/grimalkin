package acp

import (
	"context"
	"encoding/json"
	"errors"

	sdk "github.com/coder/acp-go-sdk"
)

// handle is the connection's method handler. The SDK's AgentSideConnection
// would do this, but it keeps its Connection to itself, and mcp/message
// requests to the client can only be sent on one. Every method not listed is
// not found, as the SDK answers one the Agent doesn't implement.
func (s *Server) handle(ctx context.Context, method string, params json.RawMessage) (any, *sdk.RequestError) {
	switch method {
	case sdk.AgentMethodInitialize:
		return dispatch(ctx, params, s.Initialize)
	case sdk.AgentMethodAuthenticate:
		return dispatch(ctx, params, s.Authenticate)
	case sdk.AgentMethodSessionNew:
		return dispatch(ctx, params, s.NewSession)
	case sdk.AgentMethodSessionLoad:
		return dispatch(ctx, params, s.LoadSession)
	case sdk.AgentMethodSessionPrompt:
		return dispatch(ctx, params, s.Prompt)
	case sdk.AgentMethodSessionCancel: // a notification: the answer is dropped
		return dispatch(ctx, params, func(ctx context.Context, p sdk.CancelNotification) (struct{}, error) {
			return struct{}{}, s.Cancel(ctx, p)
		})
	}
	return nil, sdk.NewMethodNotFound(method)
}

// request is a request's params type, whose pointer validates itself.
type request[P any] interface {
	*P
	Validate() error
}

// dispatch decodes and validates params, then calls fn.
func dispatch[P any, PP request[P], R any](ctx context.Context, params json.RawMessage, fn func(context.Context, P) (R, error)) (any, *sdk.RequestError) {
	var p P
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, sdk.NewInvalidParams(map[string]any{"error": err.Error()})
	}
	if err := PP(&p).Validate(); err != nil {
		return nil, sdk.NewInvalidParams(map[string]any{"error": err.Error()})
	}
	resp, err := fn(ctx, p)
	if err != nil {
		var reqErr *sdk.RequestError
		if errors.As(err, &reqErr) {
			return nil, reqErr
		}
		return nil, sdk.NewInternalError(map[string]any{"error": err.Error()})
	}
	return resp, nil
}
