// Package config loads ~/.config/grimalkin/config.json: named providers, each
// an endpoint speaking a wire format, and the user's choice of provider, model
// and effort. See docs/specs/provider-config.md.
//
// Everything in the file is validated, not just the selection; a provider's
// key command is run only when the selection or a subagent entry uses it.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode"
)

const WireAnthropicMessages = "anthropic-messages"

var thinkingValues = []string{"adaptive", "enabled", "disabled"}

// Selection is the resolved choice: everything needed to build the adapter
// for one model.
type Selection struct {
	Provider string
	Wire     string
	BaseURL  string
	APIKey   string
	Model    string
	Thinking string // empty for wires without a thinking setting
	Effort   string // empty when the model has no effort levels

	// Subagents are the models the config's subagents block allows children
	// to run on, in the order written. Nil without the block.
	Subagents []SubagentModel

	// SkillDirs are the directories to load skills from, as written: a
	// leading ~ and relative paths are left to the caller. Earlier wins.
	SkillDirs []string

	// SearchOff says why a configured search block gave no search tool: no
	// key, or a key command that failed or printed nothing. Empty otherwise.
	SearchOff string

	// Search configures the search tool. Nil when the config has no search
	// block, and the tool is left out.
	Search *Search

	// Web configures the web tools. Nil when no usable provider is
	// configured, and the tools are left out.
	Web *Web

	// WebInfo is the startup line for the web tools: the provider in use,
	// or why there is none.
	WebInfo string
}

// SubagentModel is one resolved entry of the subagents block: everything
// needed to build the adapter for a model children may run on.
type SubagentModel struct {
	Provider string
	Wire     string
	BaseURL  string
	APIKey   string
	Model    string
	Thinking string
	Effort   string // empty when the model has no effort levels, or the parent has none to take
}

// Web is the resolved web block: the provider in use and its key.
type Web struct {
	Provider string
	APIKey   string
}

// webProviders are the names a web block may hold. main.go has an adapter
// for each. A variable so tests can add a second.
var webProviders = []string{"ollama"}

// Search is the resolved search block: the Jev key and model.
type Search struct {
	APIKey string
	Model  string
	// Map turns on the prototype repo map, built in memory in the background.
	Map bool
	// MapAtStartup builds the map when grimalkin starts, not at the first
	// search. Only with Map.
	MapAtStartup bool
}

// DefaultSearchModel is used when the search block names no model.
const DefaultSearchModel = "jev-latest"

type file struct {
	Provider  string              `json:"provider"`
	Model     string              `json:"model"`
	Effort    string              `json:"effort"`
	Providers map[string]provider `json:"providers"`
	SkillDirs []string            `json:"skillDirs"`
	Search    *search             `json:"search"`
	Web       map[string]web      `json:"web"`
	Subagents *subagents          `json:"subagents"`
}

type subagents struct {
	Models []subagentEntry `json:"models"`
}

type subagentEntry struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Effort   string `json:"effort"`
}

type web struct {
	APIKey string `json:"apiKey"`
}

type search struct {
	APIKey       string `json:"apiKey"`
	Model        string `json:"model"`
	Map          bool   `json:"map"`
	MapAtStartup bool   `json:"mapAtStartup"`
}

type provider struct {
	Wire     string           `json:"wire"`
	BaseURL  string           `json:"baseUrl"`
	APIKey   string           `json:"apiKey"`
	Thinking string           `json:"thinking"`
	Efforts  []string         `json:"efforts"` // nil when absent; empty is an error
	Models   map[string]model `json:"models"`
}

type model struct {
	Thinking *string  `json:"thinking"`
	Efforts  []string `json:"efforts"`
}

// DefaultPath is ~/.config/grimalkin/config.json on every platform.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".config", "grimalkin", "config.json"), nil
}

// Load reads the config. Key commands share the terminal's stdin, so they can
// prompt.
func Load(path string) (Selection, error) { return load(path, os.Stdin) }

// LoadHeadless is Load for a process whose stdin is not a terminal, such as
// an ACP agent's protocol stream: key commands get no stdin.
func LoadHeadless(path string) (Selection, error) { return load(path, nil) }

func load(path string, stdin *os.File) (Selection, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Selection{}, fmt.Errorf("read config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f file
	if err := dec.Decode(&f); err != nil {
		return Selection{}, fmt.Errorf("%s: %w", path, err)
	}
	sel, err := f.resolve(stdin)
	if err != nil {
		return Selection{}, fmt.Errorf("%s: %w", path, err)
	}
	return sel, nil
}

func (f file) resolve(stdin *os.File) (Selection, error) {
	for _, field := range []struct{ name, value string }{
		{"provider", f.Provider}, {"model", f.Model}, {"effort", f.Effort},
	} {
		if field.value == "" {
			return Selection{}, fmt.Errorf("missing %q", field.name)
		}
	}
	if f.Providers == nil {
		return Selection{}, errors.New(`missing "providers"`)
	}
	names := make([]string, 0, len(f.Providers))
	for name := range f.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := f.Providers[name].validate(); err != nil {
			return Selection{}, fmt.Errorf("providers.%s%w", name, err)
		}
	}

	for i, dir := range f.SkillDirs {
		if dir == "" {
			return Selection{}, fmt.Errorf("skillDirs[%d] is empty", i)
		}
	}

	if f.Search != nil && f.Search.MapAtStartup && !f.Search.Map {
		return Selection{}, errors.New(`search: "mapAtStartup" needs "map": true`)
	}

	webNames := make([]string, 0, len(f.Web))
	for name := range f.Web {
		webNames = append(webNames, name)
	}
	sort.Strings(webNames)
	for _, name := range webNames {
		if !slices.Contains(webProviders, name) {
			return Selection{}, fmt.Errorf("web.%s: unknown provider (known: %s)", name, strings.Join(webProviders, ", "))
		}
		if f.Web[name].APIKey == "" {
			return Selection{}, fmt.Errorf(`web.%s: missing "apiKey"`, name)
		}
	}

	p, thinking, efforts, err := f.lookup(f.Provider, f.Model)
	if err != nil {
		return Selection{}, err
	}
	effort := ""
	if efforts != nil {
		if !slices.Contains(efforts, f.Effort) {
			return Selection{}, fmt.Errorf("effort %q is not one of %s's efforts: %s",
				f.Effort, f.Model, strings.Join(efforts, ", "))
		}
		effort = f.Effort
	}
	// Every subagent entry is checked before any key command runs, so a
	// mistake in the block is not reported after a password prompt.
	var subagentModels []SubagentModel
	if f.Subagents != nil {
		subagentModels, err = f.subagentModels(effort, efforts)
		if err != nil {
			return Selection{}, err
		}
	}
	// A provider's key command runs once, however many of the selection and
	// the subagent entries use it.
	keys := map[string]string{}
	providerKey := func(name string) (string, error) {
		if key, ok := keys[name]; ok {
			return key, nil
		}
		key, err := resolveKey(f.Providers[name].APIKey, stdin)
		if err != nil {
			return "", fmt.Errorf("providers.%s: %w", name, err)
		}
		keys[name] = key
		return key, nil
	}
	key, err := providerKey(f.Provider)
	if err != nil {
		return Selection{}, err
	}
	for i := range subagentModels {
		subagentModels[i].APIKey, err = providerKey(subagentModels[i].Provider)
		if err != nil {
			return Selection{}, err
		}
	}
	// Search is optional: without a key the model simply doesn't get the
	// tool, and the reason is reported rather than refusing to start.
	var searchSel *Search
	var searchOff string
	if f.Search != nil {
		var searchKey string
		var err error
		if f.Search.APIKey == "" {
			err = errors.New(`no "apiKey"`)
		} else {
			searchKey, err = resolveKey(f.Search.APIKey, stdin)
		}
		if err != nil {
			searchOff = fmt.Sprintf("search: off (%v)", err)
		} else {
			model := f.Search.Model
			if model == "" {
				model = DefaultSearchModel
			}
			searchSel = &Search{APIKey: searchKey, Model: model, Map: f.Search.Map, MapAtStartup: f.Search.MapAtStartup}
		}
	}
	// The web tools are optional in the same way. One provider serves them:
	// the first by name, and only its key command is run.
	var webSel *Web
	webInfo := "web: off (no provider configured)"
	if len(webNames) > 0 {
		name := webNames[0]
		webKey, err := resolveKey(f.Web[name].APIKey, stdin)
		switch {
		case err != nil:
			webInfo = fmt.Sprintf("web: off (%s: %v)", name, err)
		case len(webNames) > 1:
			webSel = &Web{Provider: name, APIKey: webKey}
			webInfo = fmt.Sprintf("web: on · %s · %d providers configured (%s), using the first", name, len(webNames), strings.Join(webNames, ", "))
		default:
			webSel = &Web{Provider: name, APIKey: webKey}
			webInfo = fmt.Sprintf("web: on · %s · search queries and fetched URLs go to it", name)
		}
	}
	return Selection{
		Provider:  f.Provider,
		Wire:      p.Wire,
		BaseURL:   p.BaseURL,
		APIKey:    key,
		Model:     f.Model,
		Thinking:  thinking,
		Effort:    effort,
		Subagents: subagentModels,
		SkillDirs: f.SkillDirs,
		Search:    searchSel,
		SearchOff: searchOff,
		Web:       webSel,
		WebInfo:   webInfo,
	}, nil
}

// lookup finds a declared provider and one of its models, and returns the
// provider with the model's resolved thinking and efforts: the model's own
// where it overrides, else the provider's. Efforts are nil when the model has
// no effort levels.
func (f file) lookup(providerName, modelName string) (provider, string, []string, error) {
	p, ok := f.Providers[providerName]
	if !ok {
		return provider{}, "", nil, fmt.Errorf("provider %q is not declared", providerName)
	}
	m, ok := p.Models[modelName]
	if !ok {
		return provider{}, "", nil, fmt.Errorf("model %q is not listed under provider %q", modelName, providerName)
	}
	thinking := p.Thinking
	if m.Thinking != nil {
		thinking = *m.Thinking
	}
	efforts := p.Efforts
	if m.Efforts != nil {
		efforts = m.Efforts
	}
	return p, thinking, efforts, nil
}

// subagentModels resolves the subagents block's entries in order, without
// their keys. Unlike the top-level effort, an entry's effort is never
// ignored: it names one model, so an effort that model cannot take is a
// mistake. An entry naming none takes the parent's: parentEffort, one of
// parentEfforts, or empty and nil when the parent's model has no levels.
func (f file) subagentModels(parentEffort string, parentEfforts []string) ([]SubagentModel, error) {
	resolved := make([]SubagentModel, 0, len(f.Subagents.Models))
	seen := map[[2]string]bool{}
	for i, e := range f.Subagents.Models {
		at := fmt.Sprintf("subagents.models[%d]", i)
		if e.Provider == "" {
			return nil, fmt.Errorf(`%s: missing "provider"`, at)
		}
		if e.Model == "" {
			return nil, fmt.Errorf(`%s: missing "model"`, at)
		}
		p, thinking, efforts, err := f.lookup(e.Provider, e.Model)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", at, err)
		}
		// The selected model is every child's default, so an entry for it
		// would name the same model twice, perhaps with another effort.
		if e.Provider == f.Provider && e.Model == f.Model {
			return nil, fmt.Errorf("%s: %s/%s is the selected model, which subagents already use", at, e.Provider, e.Model)
		}
		if seen[[2]string{e.Provider, e.Model}] {
			return nil, fmt.Errorf("%s: %s/%s is already listed", at, e.Provider, e.Model)
		}
		seen[[2]string{e.Provider, e.Model}] = true
		effort := e.Effort
		switch {
		case e.Effort == "":
			effort = inheritedEffort(parentEffort, parentEfforts, efforts)
		case efforts == nil:
			return nil, fmt.Errorf("%s: effort %q given, but %s has no effort levels", at, e.Effort, e.Model)
		case !slices.Contains(efforts, e.Effort):
			return nil, fmt.Errorf("%s: effort %q is not one of %s's efforts: %s",
				at, e.Effort, e.Model, strings.Join(efforts, ", "))
		}
		resolved = append(resolved, SubagentModel{
			Provider: e.Provider,
			Wire:     p.Wire,
			BaseURL:  p.BaseURL,
			Model:    e.Model,
			Thinking: thinking,
			Effort:   effort,
		})
	}
	return resolved, nil
}

// inheritedEffort is the effort of an entry that names none: the parent's
// when the child's model lists it, else the next level above it that the
// child's model lists, else the child's model's highest. "Above" is read from
// the parent's model's list and "highest" from the child's, both taken to be
// written lowest first. Empty when either model has no effort levels.
func inheritedEffort(parentEffort string, parentEfforts, efforts []string) string {
	if efforts == nil || parentEfforts == nil {
		return ""
	}
	at := slices.Index(parentEfforts, parentEffort)
	if at < 0 {
		panic(fmt.Sprintf("parent effort %q is not one of its model's efforts", parentEffort))
	}
	for _, level := range parentEfforts[at:] {
		if slices.Contains(efforts, level) {
			return level
		}
	}
	return efforts[len(efforts)-1]
}

// validate returns errors prefixed with ": " or ".models.<name>: ", so the
// caller can put the provider's path in front.
func (p provider) validate() error {
	for _, field := range []struct{ name, value string }{
		{"wire", p.Wire}, {"baseUrl", p.BaseURL}, {"apiKey", p.APIKey},
	} {
		if field.value == "" {
			return fmt.Errorf(": missing %q", field.name)
		}
	}
	if p.Models == nil {
		return errors.New(`: missing "models"`)
	}
	if p.Wire != WireAnthropicMessages {
		return fmt.Errorf(": unknown wire %q", p.Wire)
	}
	if p.Thinking == "" {
		return errors.New(`: missing "thinking"`)
	}
	if err := checkThinking(p.Thinking); err != nil {
		return fmt.Errorf(": %w", err)
	}
	if p.Efforts != nil && len(p.Efforts) == 0 {
		return errors.New(": efforts is empty")
	}
	if len(p.Models) == 0 {
		return errors.New(": models is empty")
	}
	names := make([]string, 0, len(p.Models))
	for name := range p.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		m := p.Models[name]
		if m.Thinking != nil {
			if err := checkThinking(*m.Thinking); err != nil {
				return fmt.Errorf(".models.%s: %w", name, err)
			}
		}
		if m.Efforts != nil && len(m.Efforts) == 0 {
			return fmt.Errorf(".models.%s: efforts is empty", name)
		}
	}
	return nil
}

func checkThinking(value string) error {
	if !slices.Contains(thinkingValues, value) {
		return fmt.Errorf("thinking %q is not one of %s", value, strings.Join(thinkingValues, ", "))
	}
	return nil
}

// resolveKey returns a literal key, or runs a key starting with "!" as a shell
// command. The command gets stdin (none when nil) so it can prompt; only its
// stdout is captured.
func resolveKey(key string, stdin *os.File) (string, error) {
	command, isCommand := strings.CutPrefix(key, "!")
	if !isCommand {
		return key, nil
	}
	cmd := exec.Command("/bin/sh", "-c", command)
	if stdin != nil { // a nil *os.File in the interface would not mean no stdin
		cmd.Stdin = stdin
	}
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("api key command: %w", err)
	}
	key = strings.TrimRightFunc(string(out), unicode.IsSpace)
	if key == "" {
		return "", errors.New("api key command printed nothing")
	}
	return key, nil
}
