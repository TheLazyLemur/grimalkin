// Command grimalkin is a terminal coding agent. See README.md.
//
// main is the composition root: it builds the adapters and wires them to the
// core's ports. The only place concrete types meet. The Anthropic SDK reads
// its own environment variables (ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN,
// ANTHROPIC_BASE_URL, ...).
package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"grimalkin/internal/adapters/anthropic"
	"grimalkin/internal/adapters/clipboard"
	"grimalkin/internal/adapters/tools"
	"grimalkin/internal/adapters/tui"
	"grimalkin/internal/core"
)

var (
	defaultModel  = "claude-opus-5"
	maxTokens     = 32_000
	toolCatalogue = []core.Tool{
		tools.Shell{},
	}
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func run() error {
	model := os.Getenv("AGENT_MODEL")
	if model == "" {
		model = defaultModel
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}

	store := tui.NewStore()
	agent := &core.Agent{
		Model:      anthropic.New(anthropic.Config{Model: model, MaxTokens: maxTokens}),
		Transcript: store,
		System:     core.SystemPrompt(osName(), cwd),
		Tools:      toolCatalogue,
	}

	store.AddInfo(fmt.Sprintf("model %s · cwd %s", model, cwd))
	return tui.Run(agent, store, clipboard.Clipboard{}, model)
}

func osName() string {
	return strings.ToUpper(runtime.GOOS[:1]) + runtime.GOOS[1:]
}
