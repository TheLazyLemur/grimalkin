// Command grimalkin is a terminal coding agent. See README.md.
//
// main is the composition root: it loads the config, builds the adapters and
// wires them to the core's ports. The only place concrete types meet.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

var (
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
	path, err := config.DefaultPath()
	if err != nil {
		return err
	}
	creds := anthropic.NewStore(filepath.Join(filepath.Dir(path), "creds.json"))
	switch args := os.Args[1:]; {
	case len(args) == 1 && args[0] == "login":
		return login(creds)
	case len(args) > 0:
		return fmt.Errorf("usage: grimalkin [login]")
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

	var tokens *anthropic.TokenSource
	if sel.Auth == config.AuthClaudeOAuth {
		tokens, err = anthropic.NewTokenSource(creds)
		if errors.Is(err, anthropic.ErrNotSignedIn) {
			return fmt.Errorf("provider %s signs in with Claude: run `grimalkin login`", sel.Provider)
		}
		if err != nil {
			return err
		}
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
			Tokens:    tokens,
			Model:     sel.Model,
			MaxTokens: maxTokens,
			Thinking:  sel.Thinking,
			Effort:    sel.Effort,
		}),
		Transcript: store,
		System:     core.SystemPrompt(osName(), cwd, library.List()),
		Tools:      toolCatalogue,
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
	for _, w := range warnings {
		store.AddInfo(w)
	}
	return tui.Run(agent, store, clipboard.Clipboard{}, label)
}

func login(store anthropic.Store) error {
	c, err := anthropic.Login(context.Background(), openBrowser, os.Stdout)
	if err != nil {
		return err
	}
	if err := store.Save(c); err != nil {
		return err
	}
	fmt.Printf("Signed in as %s. Saved to %s\n", c.Email, store.Path())
	return nil
}

// openBrowser is best effort: Login prints the URL too.
func openBrowser(url string) {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	_ = exec.Command(opener, url).Start()
}

func osName() string {
	return strings.ToUpper(runtime.GOOS[:1]) + runtime.GOOS[1:]
}
