package anthropic

import (
	"context"
	"errors"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"grimalkin/internal/core"
)

// thinkingBudget is the budget_tokens sent with "enabled" thinking. Not
// configurable: Ollama ignores it and Claude 5.x rejects "enabled" anyway.
const thinkingBudget = 16_000

type Config struct {
	BaseURL   string
	APIKey    string
	Model     string
	MaxTokens int
	Thinking  string // "adaptive", "enabled" or "disabled"
	Effort    string // sent verbatim; empty sends none
}

type Provider struct {
	client anthropic.Client
	config Config
}

// New builds a client from config alone: the SDK's environment defaults
// (ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, profiles, ...) are switched off so
// no other credential reaches the configured endpoint.
func New(config Config) *Provider {
	if config.BaseURL == "" || config.APIKey == "" || config.Model == "" || config.MaxTokens <= 0 {
		panic("provider not configured")
	}
	if config.Thinking == "enabled" && config.MaxTokens <= thinkingBudget {
		panic(fmt.Sprintf("max tokens %d must exceed the thinking budget %d", config.MaxTokens, thinkingBudget))
	}
	thinking(config.Thinking) // panics on a value config validation should have rejected
	client := anthropic.NewClient(
		option.WithoutEnvironmentDefaults(),
		option.WithBaseURL(config.BaseURL),
		option.WithAPIKey(config.APIKey),
	)
	return &Provider{client: client, config: config}
}

func thinking(value string) anthropic.ThinkingConfigParamUnion {
	switch value {
	case "adaptive":
		return anthropic.ThinkingConfigParamUnion{OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{}}
	case "enabled":
		return anthropic.ThinkingConfigParamOfEnabled(thinkingBudget)
	case "disabled":
		return anthropic.ThinkingConfigParamUnion{OfDisabled: &anthropic.ThinkingConfigDisabledParam{}}
	}
	panic(fmt.Sprintf("unknown thinking %q", value))
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
	// Two cache breakpoints, both set on blocks: the system block caches tools
	// and system, which don't change in a session; the last message block
	// follows the end of the conversation, so each request reads the previous
	// one's prefix.
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(p.config.Model),
		MaxTokens: int64(p.config.MaxTokens),
		System:    []anthropic.TextBlockParam{{Text: req.System, CacheControl: cacheControl()}},
		Thinking:  thinking(p.config.Thinking),
	}
	if p.config.Effort != "" {
		params.OutputConfig = anthropic.OutputConfigParam{Effort: anthropic.OutputConfigEffort(p.config.Effort)}
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
	if len(params.Messages) == 0 {
		panic("request has no messages")
	}
	last := params.Messages[len(params.Messages)-1].Content
	*last[len(last)-1].GetCacheControl() = cacheControl()
	return params
}

func cacheControl() anthropic.CacheControlEphemeralParam {
	return anthropic.NewCacheControlEphemeralParam()
}

func describeError(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return fmt.Errorf("HTTP %d: %w", apiErr.StatusCode, err)
	}
	return err
}
