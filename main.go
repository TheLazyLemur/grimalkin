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

	"grimalkin/internal/adapters/acp"
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

// newModel builds the adapter for a provider's wire, or says there is none.
func newModel(wire, baseURL, key, model, thinking, effort string) (core.Model, error) {
	if wire != config.WireAnthropicMessages {
		return nil, fmt.Errorf("no adapter for wire %q", wire)
	}
	return anthropic.New(anthropic.Config{
		BaseURL:   baseURL,
		APIKey:    key,
		Model:     model,
		MaxTokens: maxTokens,
		Thinking:  thinking,
		Effort:    effort,
	}), nil
}

// subagentModels builds the model for each entry of the config's subagents
// block, in its order, named provider/model as the agent tool offers them.
func subagentModels(entries []config.SubagentModel) ([]core.SubagentModel, error) {
	var models []core.SubagentModel
	for i, e := range entries {
		model, err := newModel(e.Wire, e.BaseURL, e.APIKey, e.Model, e.Thinking, e.Effort)
		if err != nil {
			return nil, fmt.Errorf("subagents.models[%d] (%s/%s): %w", i, e.Provider, e.Model, err)
		}
		models = append(models, core.SubagentModel{Name: e.Provider + "/" + e.Model, Model: model})
	}
	return models, nil
}

// subagentModelsInfo is the startup line naming the models children may run
// on beside the session's.
func subagentModelsInfo(entries []config.SubagentModel) string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Provider + "/" + e.Model
		if e.Effort != "" {
			names[i] += " (effort " + e.Effort + ")"
		}
	}
	return "subagent models: " + strings.Join(names, ", ")
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// runACP serves one ACP session on stdin and stdout, with no tools: the
// permission gate that must stand in front of them is not built yet. stdout
// carries protocol messages only, so nothing is printed to it here. The
// server saves the session's kept turns as the agent's History.
func runACP(model core.Model, cwd string) error {
	server := acp.New()
	agent := &core.Agent{
		Model:      model,
		Transcript: server,
		History:    server,
		System:     core.SystemPrompt(osName(), cwd, nil),
	}
	return server.Serve(agent, os.Stdin, os.Stdout)
}

func run() error {
	args := os.Args[1:]
	acpMode := len(args) == 1 && args[0] == "acp"
	if len(args) > 0 && !acpMode {
		return fmt.Errorf("usage: grimalkin [acp]")
	}
	path, err := config.DefaultPath()
	if err != nil {
		return err
	}
	// An ACP agent's stdin is the protocol stream, so key commands get none.
	load := config.Load
	if acpMode {
		load = config.LoadHeadless
	}
	sel, err := load(path)
	if err != nil {
		return err
	}
	model, err := newModel(sel.Wire, sel.BaseURL, sel.APIKey, sel.Model, sel.Thinking, sel.Effort)
	if err != nil {
		panic(err) // config accepted a wire this file has no adapter for
	}
	children, err := subagentModels(sel.Subagents)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}
	if acpMode {
		return runACP(model, cwd)
	}

	library, warnings, err := skills.Load(sel.SkillDirs, cwd)
	if err != nil {
		return err
	}

	store := tui.NewStore()
	catalogue, search := toolCatalogue(sel, cwd)
	agent := &core.Agent{
		Model:      model,
		Transcript: store,
		System:     core.SystemPrompt(osName(), cwd, library.List()),
		Tools:      catalogue,
		Skills:     library,
		// Children get the shell only; they share Skills, so the core gives
		// them the skill tool. The core never offers them the agent tool.
		SubagentTools:  []core.Tool{tools.Shell{}},
		ModelName:      sel.Provider + "/" + sel.Model,
		SubagentModels: children,
	}

	effort := sel.Effort
	if effort == "" {
		effort = "n/a"
	}
	label := fmt.Sprintf("%s · effort %s", sel.Model, effort)
	// A child's buffer shows its own model's label. The session's goes in
	// last: a child on the session's model, a fork included, runs as the
	// session does, whatever a subagent entry of the same name says.
	childLabels := map[string]string{}
	for _, e := range sel.Subagents {
		effort := e.Effort
		if effort == "" {
			effort = "n/a"
		}
		childLabels[e.Provider+"/"+e.Model] = fmt.Sprintf("%s · effort %s", e.Model, effort)
	}
	childLabels[sel.Provider+"/"+sel.Model] = label
	store.AddInfo(fmt.Sprintf("provider %s · model %s · cwd %s", sel.Provider, sel.Model, cwd))
	if len(sel.Subagents) > 0 {
		store.AddInfo(subagentModelsInfo(sel.Subagents))
	}
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
	return tui.Run(agent, store, clipboard.Clipboard{}, label, childLabels, repoMap)
}

func osName() string {
	return strings.ToUpper(runtime.GOOS[:1]) + runtime.GOOS[1:]
}
