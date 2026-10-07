package core

import (
	"context"
	"fmt"
)

const maxToolResultBytes = 30_000

func (a *Agent) toolSpecs() []ToolSpec {
	specs := make([]ToolSpec, 0, len(a.Tools)+1)
	for _, tool := range a.Tools {
		specs = append(specs, tool.Spec())
	}
	if names := a.modelSkillNames(); len(names) > 0 {
		specs = append(specs, skillToolSpec(names))
	}
	return specs
}

func (a *Agent) findTool(name string) Tool {
	for _, tool := range a.Tools {
		if tool.Spec().Name == name {
			return tool
		}
	}
	return nil
}

// runTool runs one call. pending holds the results of earlier calls in the
// same response.
func (a *Agent) runTool(ctx context.Context, call ToolCall, pending []ToolResult) ToolResult {
	var summary string
	var run func() (string, error)
	limit := maxToolResultBytes
	tool := a.findTool(call.Name)
	switch {
	case tool != nil:
		summary = tool.Summary(call.Input)
		run = func() (string, error) { return tool.Run(ctx, call.Input) }
	case call.Name == SkillToolName && len(a.modelSkillNames()) > 0:
		summary = skillSummary(call.Input)
		run = func() (string, error) { return a.runSkillTool(ctx, call.Input, pending) }
		limit = 0 // a skill cut short loses instructions; its parts are capped instead
	default:
		summary = call.Name
		run = func() (string, error) { return "", fmt.Errorf("unknown tool %q", call.Name) }
	}
	a.Transcript.Show(ToolStarted{ID: call.ID, Name: call.Name, Summary: summary})

	output, err := run()
	if err != nil {
		output = err.Error()
	}
	a.Transcript.Show(ToolFinished{ID: call.ID, Output: output, IsError: err != nil})
	if limit > 0 {
		output = lastBytes(output, limit)
	}
	return ToolResult{ToolUseID: call.ID, Content: output, IsError: err != nil}
}

func lastBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := len(s) - limit
	for cut < len(s) && s[cut]&0xC0 == 0x80 {
		cut++
	}
	return fmt.Sprintf("[output truncated: first %d bytes omitted]\n%s", cut, s[cut:])
}
