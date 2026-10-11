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

// webProviders builds the web provider for each name config accepts.
var webProviders = map[string]func(key string) tools.WebProvider{
	"ollama": func(key string) tools.WebProvider { return tools.NewOllamaWeb(key) },
}

// toolCatalogue is every tool the model gets. search joins the shell when the
// config has a search block, and is then available in every project. The
// search tool is returned too (nil without one), for its repo map. The web
// tools join when the config has a usable web provider.
func toolCatalogue(sel config.Selection, cwd string) ([]core.Tool, *tools.Search) {
	catalogue := []core.Tool{tools.Shell{}}
	var search *tools.Search
	if sel.Search != nil {
		search = tools.NewSearch(cwd, sel.Search.APIKey, sel.Search.Model, sel.Search.Map)
		catalogue = append(catalogue, search)
	}
	if sel.Web != nil {
		build, ok := webProviders[sel.Web.Provider]
		if !ok {
			panic(fmt.Sprintf("no adapter for web provider %q", sel.Web.Provider))
		}
		provider := build(sel.Web.APIKey)
		catalogue = append(catalogue, tools.NewWebSearch(provider), tools.NewWebFetch(provider))
	}
	return catalogue, search
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
	catalogue, search := toolCatalogue(sel, cwd)
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
		Tools:      catalogue,
		Skills:     library,
		// Children get the shell only; they share Skills, so the core gives
		// them the skill tool. The core never offers them the agent tool.
		SubagentTools: []core.Tool{tools.Shell{}},
		ModelName:     sel.Provider + "/" + sel.Model,
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
	if sel.SearchOff != "" {
		store.AddInfo(sel.SearchOff)
	}
	if sel.Search != nil {
		info := "search: on · " + sel.Search.Model + " · sends excerpts of shortlisted files to TypeSafe"
		switch {
		case sel.Search.Map && !search.MapBuildsUnasked():
			info += " · repo map: not a git repository, so /index builds it"
		case sel.Search.MapAtStartup:
			info += " · repo map building in the background: sends the start of every new or changed code and config file"
		case sel.Search.Map:
			info += " · repo map on: the first search sends the start of every new or changed code and config file"
		}
		store.AddInfo(info)
		if sel.Search.MapAtStartup {
			// Build the repo map now, so it is likely ready by the first
			// search. After the line above, so nothing is sent unannounced.
			search.StartMap()
		}
	}
	store.AddInfo(sel.WebInfo)
	for _, w := range warnings {
		store.AddInfo(w)
	}
	// The repo map's progress shows in the status bar, and /index builds it.
	var repoMap tui.RepoMap
	if search != nil && sel.Search.Map {
		repoMap = search
	}
	return tui.Run(agent, store, clipboard.Clipboard{}, label, repoMap)
}

func osName() string {
	return strings.ToUpper(runtime.GOOS[:1]) + runtime.GOOS[1:]
}
