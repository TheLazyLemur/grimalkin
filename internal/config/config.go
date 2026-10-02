// Package config loads ~/.config/grimalkin/config.json: named providers, each
// an endpoint speaking a wire format, and the user's choice of provider, model
// and effort. See docs/specs/provider-config.md.
//
// Everything in the file is validated, not just the selection; only the
// selected provider's key command is run.
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

const (
	WireAnthropicMessages = "anthropic-messages"

	// AuthClaudeOAuth signs in with a Claude Pro/Max account instead of a key.
	AuthClaudeOAuth = "claude-oauth"
)

var thinkingValues = []string{"adaptive", "enabled", "disabled"}

// Selection is the resolved choice: everything needed to build the adapter
// for one model.
type Selection struct {
	Provider string
	Wire     string
	BaseURL  string
	APIKey   string // empty when Auth is set
	Auth     string // empty when APIKey is set
	Model    string
	Thinking string // empty for wires without a thinking setting
	Effort   string // empty when the model has no effort levels

	// SkillDirs are the directories to load skills from, as written: a
	// leading ~ and relative paths are left to the caller. Earlier wins.
	SkillDirs []string
}

type file struct {
	Provider  string              `json:"provider"`
	Model     string              `json:"model"`
	Effort    string              `json:"effort"`
	Providers map[string]provider `json:"providers"`
	SkillDirs []string            `json:"skillDirs"`
}

type provider struct {
	Wire     string           `json:"wire"`
	BaseURL  string           `json:"baseUrl"`
	APIKey   string           `json:"apiKey"`
	Auth     string           `json:"auth"`
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

func Load(path string) (Selection, error) {
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
	sel, err := f.resolve()
	if err != nil {
		return Selection{}, fmt.Errorf("%s: %w", path, err)
	}
	return sel, nil
}

func (f file) resolve() (Selection, error) {
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

	p, ok := f.Providers[f.Provider]
	if !ok {
		return Selection{}, fmt.Errorf("provider %q is not declared", f.Provider)
	}
	m, ok := p.Models[f.Model]
	if !ok {
		return Selection{}, fmt.Errorf("model %q is not listed under provider %q", f.Model, f.Provider)
	}
	thinking := p.Thinking
	if m.Thinking != nil {
		thinking = *m.Thinking
	}
	efforts := p.Efforts
	if m.Efforts != nil {
		efforts = m.Efforts
	}
	effort := ""
	if efforts != nil {
		if !slices.Contains(efforts, f.Effort) {
			return Selection{}, fmt.Errorf("effort %q is not one of %s's efforts: %s",
				f.Effort, f.Model, strings.Join(efforts, ", "))
		}
		effort = f.Effort
	}
	key := ""
	if p.Auth == "" {
		var err error
		if key, err = resolveKey(p.APIKey); err != nil {
			return Selection{}, fmt.Errorf("providers.%s: %w", f.Provider, err)
		}
	}
	return Selection{
		Provider:  f.Provider,
		Wire:      p.Wire,
		BaseURL:   p.BaseURL,
		APIKey:    key,
		Auth:      p.Auth,
		Model:     f.Model,
		Thinking:  thinking,
		Effort:    effort,
		SkillDirs: f.SkillDirs,
	}, nil
}

// validate returns errors prefixed with ": " or ".models.<name>: ", so the
// caller can put the provider's path in front.
func (p provider) validate() error {
	for _, field := range []struct{ name, value string }{
		{"wire", p.Wire}, {"baseUrl", p.BaseURL},
	} {
		if field.value == "" {
			return fmt.Errorf(": missing %q", field.name)
		}
	}
	switch {
	case p.APIKey == "" && p.Auth == "":
		return errors.New(`: missing "apiKey" or "auth"`)
	case p.APIKey != "" && p.Auth != "":
		return errors.New(`: "apiKey" and "auth" are exclusive`)
	case p.Auth != "" && p.Auth != AuthClaudeOAuth:
		return fmt.Errorf(": unknown auth %q", p.Auth)
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
// command. The command shares the terminal so it can prompt; only its stdout
// is captured.
func resolveKey(key string) (string, error) {
	command, isCommand := strings.CutPrefix(key, "!")
	if !isCommand {
		return key, nil
	}
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Stdin = os.Stdin
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
