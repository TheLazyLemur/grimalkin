// Command grimalkin is a terminal coding agent. See README.md.
//
// main is the composition root: it loads the config, builds the adapters and
// wires them to the core's ports. The only place concrete types meet.
package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"grimalkin/internal/adapters/anthropic"
	"grimalkin/internal/adapters/clipboard"
	"grimalkin/internal/adapters/skills"
	"grimalkin/internal/adapters/tools"
	"grimalkin/internal/adapters/tui"
	"grimalkin/internal/config"
	"grimalkin/internal/core"
)

var maxTokens = 32_000

// toolCatalogue is every tool the model gets. search joins the shell when the
// config has a search block, and is then available in every project.
func toolCatalogue(sel config.Selection, cwd string) []core.Tool {
	catalogue := []core.Tool{tools.Shell{}}
	if sel.Search != nil {
		catalogue = append(catalogue, tools.NewSearch(cwd, sel.Search.APIKey, sel.Search.Model))
	}
	return catalogue
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func run() error {
	path, err := config.DefaultPath()
	if err != nil {
		return err
	}
	sel, err := config.Load(path)
	if err != nil {
		return err
	}
	if sel.Wire != config.WireAnthropicMessages {
		panic(fmt.Sprintf("no adapter for wire %q", sel.Wire))
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}

	library, warnings, err := skills.Load(sel.SkillDirs, cwd)
	if err != nil {
		return err
	}

	store := tui.NewStore()
	agent := &core.Agent{
		Model: anthropic.New(anthropic.Config{
			BaseURL:   sel.BaseURL,
			APIKey:    sel.APIKey,
			Model:     sel.Model,
			MaxTokens: maxTokens,
			Thinking:  sel.Thinking,
			Effort:    sel.Effort,
		}),
		Transcript: store,
		System:     core.SystemPrompt(osName(), cwd, library.List()),
		Tools:      toolCatalogue(sel, cwd),
		Skills:     library,
	}

	effort := sel.Effort
	if effort == "" {
		effort = "n/a"
	}
	label := fmt.Sprintf("%s · effort %s", sel.Model, effort)
	store.AddInfo(fmt.Sprintf("provider %s · model %s · cwd %s", sel.Provider, sel.Model, cwd))
	if list := library.List(); len(list) > 0 {
		names := make([]string, len(list))
		for i, s := range list {
			names[i] = s.Name
		}
		store.AddInfo("skills: " + strings.Join(names, ", "))
	}
	if sel.Search != nil {
		store.AddInfo("search: on · " + sel.Search.Model + " · sends excerpts of keyword-matched files to TypeSafe")
	}
	for _, w := range warnings {
		store.AddInfo(w)
	}
	return tui.Run(agent, store, clipboard.Clipboard{}, label)
}

func osName() string {
	return strings.ToUpper(runtime.GOOS[:1]) + runtime.GOOS[1:]
}
