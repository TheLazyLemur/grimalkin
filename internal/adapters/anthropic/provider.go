package anthropic

import (
	"context"
	"errors"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"grimalkin/internal/core"
)

type Config struct {
	Model     string
	MaxTokens int
}

type Provider struct {
	client anthropic.Client
	config Config
}

func New(config Config, opts ...option.RequestOption) *Provider {
	if config.Model == "" || config.MaxTokens <= 0 {
		panic("provider not configured")
	}
	return &Provider{client: anthropic.NewClient(opts...), config: config}
}

func (p *Provider) Stream(ctx context.Context, req core.ModelRequest, onText func(string)) (core.ModelResponse, error) {
	stream := p.client.Messages.NewStreaming(ctx, p.buildParams(req))
	defer stream.Close()

	msg := anthropic.Message{}
	sawStop := false
	for stream.Next() {
		event := stream.Current()
		if err := msg.Accumulate(event); err != nil {
			return core.ModelResponse{}, fmt.Errorf("accumulate: %w", err)
		}
		switch ev := event.AsAny().(type) {
		case anthropic.ContentBlockDeltaEvent:
			if delta, ok := ev.Delta.AsAny().(anthropic.TextDelta); ok {
				onText(delta.Text)
			}
		case anthropic.MessageStopEvent:
			sawStop = true
		}
	}
	if err := stream.Err(); err != nil {
		return core.ModelResponse{}, describeError(err)
	}
	if !sawStop {
		return core.ModelResponse{}, errors.New("stream ended without message_stop")
	}

	var calls []core.ToolCall
	for _, block := range msg.Content {
		if block.Type == "tool_use" {
			calls = append(calls, core.ToolCall{ID: block.ID, Name: block.Name, Input: block.Input})
		}
	}
	return core.ModelResponse{
		StopReason: stopReason(msg.StopReason),
		StopDetail: string(msg.StopReason),
		ToolCalls:  calls,
		Content:    msg.ToParam(),
	}, nil
}

func stopReason(r anthropic.StopReason) core.StopReason {
	switch r {
	case anthropic.StopReasonEndTurn:
		return core.StopEndTurn
	case anthropic.StopReasonToolUse:
		return core.StopToolUse
	default:
		return core.StopOther
	}
}

func (p *Provider) buildParams(req core.ModelRequest) anthropic.MessageNewParams {
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(p.config.Model),
		MaxTokens: int64(p.config.MaxTokens),
		System:    []anthropic.TextBlockParam{{Text: req.System}},
	}
	for _, tool := range req.Tools {
		var schema anthropic.ToolInputSchemaParam
		if err := schema.UnmarshalJSON([]byte(tool.InputSchema)); err != nil {
			panic(fmt.Sprintf("tool %s: bad input schema: %v", tool.Name, err))
		}
		params.Tools = append(params.Tools, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name:        tool.Name,
			Description: anthropic.String(tool.Description),
			Strict:      anthropic.Bool(true),
			InputSchema: schema,
		}})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case core.RoleAssistant:
			content, ok := m.Content.(anthropic.MessageParam)
			if !ok {
				panic(fmt.Sprintf("assistant message content is %T, not this adapter's", m.Content))
			}
			params.Messages = append(params.Messages, content)
		case core.RoleUser:
			if len(m.ToolResults) == 0 {
				params.Messages = append(params.Messages, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Text)))
				continue
			}
			blocks := make([]anthropic.ContentBlockParamUnion, len(m.ToolResults))
			for i, r := range m.ToolResults {
				blocks[i] = anthropic.NewToolResultBlock(r.ToolUseID, r.Content, r.IsError)
			}
			params.Messages = append(params.Messages, anthropic.NewUserMessage(blocks...))
		}
	}
	return params
}

func describeError(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return fmt.Errorf("HTTP %d: %w", apiErr.StatusCode, err)
	}
	return err
}
