package core

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
)

const maxToolResultBytes = 30_000

func (a *Agent) toolSpecs() []ToolSpec {
	if a.forked {
		// The parent's, agent included, or the fork misses its cache.
		return a.parent.toolSpecs()
	}
	specs := make([]ToolSpec, 0, len(a.Tools)+2)
	for _, tool := range a.Tools {
		specs = append(specs, tool.Spec())
	}
	if names := a.modelSkillNames(); len(names) > 0 {
		specs = append(specs, skillToolSpec(names))
	}
	if a.SubagentTools != nil {
		specs = append(specs, a.agentToolSpec())
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

// runTools runs a response's calls and returns their results in call order.
// Skill calls run first and one at a time, because each is deduplicated
// against the ones before it. The rest then run at the same time, agent
// calls among them, each with the child it claimed in call order. A
// cancelled ctx leaves results incomplete.
func (a *Agent) runTools(ctx context.Context, calls []ToolCall) []ToolResult {
	results := make([]ToolResult, len(calls))
	hasSkillTool := len(a.modelSkillNames()) > 0
	var loaded []ToolResult
	var rest []int
	for i, call := range calls {
		if !hasSkillTool || call.Name != SkillToolName {
			rest = append(rest, i)
			continue
		}
		results[i] = a.runSkillCall(ctx, call, loaded)
		if ctx.Err() != nil {
			return results
		}
		loaded = append(loaded, results[i])
	}

	// A panic in one call stops the others and is raised again on the
	// turn's goroutine, where a panic in a call run in turn would be.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Claimed here, not in the calls' goroutines, so children are numbered
	// in the order the model wrote the calls. Every claim is run below.
	claims := map[int]childClaim{}
	for _, i := range rest {
		if a.SubagentTools != nil && calls[i].Name == AgentToolName {
			claims[i] = a.claimChild(ctx, calls[i], calls)
		}
	}
	var wg sync.WaitGroup
	var once sync.Once
	var panicked any
	for _, i := range rest {
		wg.Go(func() {
			defer func() {
				if r := recover(); r != nil {
					once.Do(func() {
						panicked = fmt.Sprintf("tool %s panicked: %v\n\n%s", calls[i].Name, r, debug.Stack())
					})
					cancel()
				}
			}()
			if claim, ok := claims[i]; ok {
				results[i] = a.runAgentCall(ctx, calls[i], claim)
				return
			}
			results[i] = a.runTool(ctx, calls[i])
		})
	}
	wg.Wait()
	if panicked != nil {
		panic(panicked)
	}
	return results
}

func (a *Agent) runTool(ctx context.Context, call ToolCall) ToolResult {
	if a.forked && call.Name == AgentToolName {
		return a.runCall(ctx, call, AgentToolName+" "+string(call.Input), maxToolResultBytes, func() (string, error) {
			return "", errForkStartsSubagent
		})
	}
	tool := a.findTool(call.Name)
	if tool == nil {
		return a.runCall(ctx, call, call.Name, maxToolResultBytes, func() (string, error) {
			return "", fmt.Errorf("unknown tool %q", call.Name)
		})
	}
	return a.runCall(ctx, call, tool.Summary(call.Input), maxToolResultBytes, func() (string, error) {
		return tool.Run(ctx, call.Input)
	})
}

// runSkillCall runs a call of the skill tool. loaded holds the results of
// the skill calls before it in the same response. A skill cut short loses
// instructions, so its result has no cap; its parts are capped instead.
func (a *Agent) runSkillCall(ctx context.Context, call ToolCall, loaded []ToolResult) ToolResult {
	return a.runCall(ctx, call, skillSummary(call.Input), 0, func() (string, error) {
		return a.runSkillTool(ctx, call.Input, loaded)
	})
}

// runCall shows a call starting and finishing around run, and caps its
// output at limit bytes unless limit is 0.
func (a *Agent) runCall(ctx context.Context, call ToolCall, summary string, limit int, run func() (string, error)) ToolResult {
	a.show(ToolStarted{ID: call.ID, Name: call.Name, Summary: summary, Input: call.Input})
	a.countToolCall()

	output, err := a.gated(ctx, call, run)
	if err != nil {
		output = err.Error()
	}
	a.show(ToolFinished{ID: call.ID, Output: output, IsError: err != nil})
	if limit > 0 {
		output = lastBytes(output, limit)
	}
	return ToolResult{ToolUseID: call.ID, Content: output, IsError: err != nil}
}

// gated runs run once a.Gate, if any, allows the call. A turn cancelled while
// the gate waits ends the call as a cancelled run does, with ctx's error.
func (a *Agent) gated(ctx context.Context, call ToolCall, run func() (string, error)) (string, error) {
	if a.Gate == nil {
		return run()
	}
	ok, reason := a.Gate.Allow(ctx, call)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if !ok {
		if reason == "" {
			reason = "not allowed"
		}
		return "", errors.New(reason)
	}
	return run()
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
