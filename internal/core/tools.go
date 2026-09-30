package core

import (
	"context"
	"fmt"
)

const maxToolResultBytes = 30_000

func (a *Agent) toolSpecs() []ToolSpec {
	specs := make([]ToolSpec, len(a.Tools))
	for i, tool := range a.Tools {
		specs[i] = tool.Spec()
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

func (a *Agent) runTool(ctx context.Context, call ToolCall) ToolResult {
	tool := a.findTool(call.Name)
	summary := call.Name
	if tool != nil {
		summary = tool.Summary(call.Input)
	}
	a.Transcript.Show(ToolStarted{ID: call.ID, Name: call.Name, Summary: summary})

	var output string
	var err error
	if tool == nil {
		err = fmt.Errorf("unknown tool %q", call.Name)
	} else {
		output, err = tool.Run(ctx, call.Input)
	}
	if err != nil {
		output = err.Error()
	}
	a.Transcript.Show(ToolFinished{ID: call.ID, Output: output, IsError: err != nil})
	return ToolResult{ToolUseID: call.ID, Content: lastBytes(output, maxToolResultBytes), IsError: err != nil}
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
