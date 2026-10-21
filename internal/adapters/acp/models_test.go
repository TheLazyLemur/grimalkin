package acp_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	sdk "github.com/coder/acp-go-sdk"

	"grimalkin/internal/adapters/acp"
	"grimalkin/internal/core"
)

// fakeModels is the models a session may pick, from two providers: acme's
// large (the default, at medium) and small, both with efforts; local's
// gemma, with none; and locked's vault, whose provider's key command fails.
// Each model and effort built is a scripted model of its own, scripted
// beforehand with script; a name in failing can't be built.
type fakeModels struct {
	failing map[string]string

	mu       sync.Mutex
	scripted map[string]*scriptedModel // by "name@effort"
	built    []string                  // each build, as "name@effort"
}

func newCatalogue() *fakeModels {
	return &fakeModels{
		failing:  map[string]string{"locked/vault": "providers.locked: api key command: exit status 1: vault is sealed"},
		scripted: map[string]*scriptedModel{},
	}
}

// script gives the model built for "name@effort" its replies.
func (f *fakeModels) script(build string, replies ...modelReply) *scriptedModel {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripted[build] = &scriptedModel{replies: replies}
	return f.scripted[build]
}

func (f *fakeModels) models() acp.Models {
	return acp.Models{
		List: []acp.Model{
			{Name: "acme/large", Efforts: []string{"low", "medium", "high"}},
			{Name: "acme/small", Efforts: []string{"low", "high", "max"}},
			{Name: "local/gemma"},
			{Name: "locked/vault", Efforts: []string{"low"}},
		},
		Default: "acme/large",
		Effort:  "medium",
		Build: func(name, effort string) (core.Model, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if message, ok := f.failing[name]; ok {
				return nil, errors.New(message)
			}
			build := name + "@" + effort
			f.built = append(f.built, build)
			if f.scripted[build] == nil {
				f.scripted[build] = &scriptedModel{}
			}
			return f.scripted[build], nil
		},
	}
}

func (f *fakeModels) builds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.built)
}

// connectModels connects a client to a fresh adapter offering f's models,
// starting on acme/large at medium, which is scripted with replies.
func connectModels(t *testing.T, f *fakeModels, replies ...modelReply) *session {
	t.Helper()
	return connectWith(t, nil, f.models(), f.script("acme/large@medium", replies...))
}

func (s *session) set(id sdk.SessionId, configID, value string) (sdk.SetSessionConfigOptionResponse, error) {
	return s.client.SetSessionConfigOption(context.Background(), sdk.SetSessionConfigOptionRequest{ValueId: &sdk.SetSessionConfigOptionValueId{
		SessionId: id, ConfigId: sdk.SessionConfigId(configID), Value: sdk.SessionConfigValueId(value),
	}})
}

// optionLines is each config option as "id (category) = current: values",
// a value followed by its _meta.efforts where it has them.
func optionLines(options []sdk.SessionConfigOption) []string {
	var lines []string
	for _, o := range options {
		var values []string
		for _, v := range *o.Select.Options.Ungrouped {
			value := string(v.Value)
			if efforts, ok := v.Meta["efforts"]; ok {
				value += fmt.Sprint(efforts)
			}
			if v.Name != string(v.Value) {
				value += " named " + v.Name
			}
			values = append(values, value)
		}
		lines = append(lines, fmt.Sprintf("%s (%s) = %s: %s", o.Select.Id, *o.Select.Category, o.Select.CurrentValue, strings.Join(values, " ")))
	}
	return lines
}

func TestSessionNewOffersEveryModelWithItsEffortsAndTheDefaultModelsEfforts(t *testing.T) {
	// given
	// ... an adapter offering two providers' models, one of them without efforts
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := connectModels(t, newCatalogue())
	s.initialize(t)

	// when
	// ... the client creates a session
	resp, err := s.client.NewSession(context.Background(), sdk.NewSessionRequest{Cwd: "/work", McpServers: []sdk.McpServer{}})

	// then
	// ... every model is offered with its efforts, the default current, and
	// ... the default's efforts with its effort current
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}
	want := []string{
		"model (model) = acme/large: acme/large[low medium high] acme/small[low high max] local/gemma[] locked/vault[low]",
		"thought_level (thought_level) = medium: low medium high",
	}
	if got := optionLines(resp.ConfigOptions); !slices.Equal(got, want) {
		t.Errorf("configOptions = %q, want %q", got, want)
	}
}

func TestPickingAModelMovesTheNextTurnToItAtTheNextHigherEffortItHas(t *testing.T) {
	// given
	// ... a session on acme/large at medium, which acme/small doesn't list
	f := newCatalogue()
	small := f.script("acme/small@high", modelReply{chunks: []string{"small"}})
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := connectModels(t, f)
	s.initialize(t)
	id := s.newSession(t)

	// when
	// ... the client picks acme/small, then prompts
	resp, setErr := s.set(id, "model", "acme/small")
	_, promptErr := s.client.Prompt(context.Background(), promptOf(id, "hi"))

	// then
	// ... small was built at high and answered the turn, named as itself,
	// ... and the options list its efforts
	if setErr != nil || promptErr != nil {
		t.Fatalf("set: %v; prompt: %v", setErr, promptErr)
	}
	want := []string{
		"model (model) = acme/small: acme/large[low medium high] acme/small[low high max] local/gemma[] locked/vault[low]",
		"thought_level (thought_level) = high: low high max",
	}
	if got := optionLines(resp.ConfigOptions); !slices.Equal(got, want) {
		t.Errorf("configOptions = %q, want %q", got, want)
	}
	if got := f.builds(); !slices.Equal(got, []string{"acme/small@high"}) {
		t.Errorf("builds = %q, want acme/small@high", got)
	}
	if sent := small.sent(); len(sent) != 1 || sent[0].Model != "acme/small" {
		t.Errorf("small's requests = %+v, want one for acme/small", sent)
	}
	if n := len(s.model.sent()); n != 0 {
		t.Errorf("acme/large called %d times, want 0", n)
	}
}

func TestPickingAModelThatListsTheCurrentEffortKeepsIt(t *testing.T) {
	// given
	// ... a session on acme/large at low, which acme/small lists too
	f := newCatalogue()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := connectModels(t, f)
	s.initialize(t)
	id := s.newSession(t)
	if _, err := s.set(id, "thought_level", "low"); err != nil {
		t.Fatalf("set thought_level: %v", err)
	}

	// when
	// ... the client picks acme/small
	_, err := s.set(id, "model", "acme/small")

	// then
	// ... small is built at low
	if err != nil {
		t.Fatalf("set model: %v", err)
	}
	if got := f.builds(); !slices.Equal(got, []string{"acme/large@low", "acme/small@low"}) {
		t.Errorf("builds = %q, want acme/large@low then acme/small@low", got)
	}
}

func TestPickingAModelWithNoEffortLevelsDropsTheEffortOption(t *testing.T) {
	// given
	// ... a session on acme/large
	f := newCatalogue()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := connectModels(t, f)
	s.initialize(t)
	id := s.newSession(t)

	// when
	// ... the client picks local/gemma
	resp, err := s.set(id, "model", "local/gemma")

	// then
	// ... gemma is built with no effort and only the model option is left
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	want := []string{"model (model) = local/gemma: acme/large[low medium high] acme/small[low high max] local/gemma[] locked/vault[low]"}
	if got := optionLines(resp.ConfigOptions); !slices.Equal(got, want) {
		t.Errorf("configOptions = %q, want %q", got, want)
	}
	if got := f.builds(); !slices.Equal(got, []string{"local/gemma@"}) {
		t.Errorf("builds = %q, want local/gemma with no effort", got)
	}
}

func TestPickingAModelWhoseKeyFailsErrorsWithTheCommandsMessageAndKeepsTheModel(t *testing.T) {
	// given
	// ... a session on acme/large, and locked/vault whose provider's key command fails
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := connectModels(t, newCatalogue(), modelReply{chunks: []string{"large"}})
	s.initialize(t)
	id := s.newSession(t)

	// when
	// ... the client picks locked/vault, then prompts
	_, setErr := s.set(id, "model", "locked/vault")
	_, promptErr := s.client.Prompt(context.Background(), promptOf(id, "hi"))

	// then
	// ... the pick errored with the command's message, and acme/large answered the turn
	if setErr == nil || !strings.Contains(setErr.Error(), "vault is sealed") {
		t.Errorf("set err = %v, want the key command's message", setErr)
	}
	if promptErr != nil {
		t.Fatalf("prompt: %v", promptErr)
	}
	if sent := s.model.sent(); len(sent) != 1 || sent[0].Model != "acme/large" {
		t.Errorf("acme/large's requests = %+v, want one", sent)
	}
}

func TestPickingAnEffortRebuildsTheModelAtIt(t *testing.T) {
	// given
	// ... a session on acme/large at medium
	f := newCatalogue()
	high := f.script("acme/large@high", modelReply{chunks: []string{"high"}})
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := connectModels(t, f)
	s.initialize(t)
	id := s.newSession(t)

	// when
	// ... the client picks high, then prompts
	resp, setErr := s.set(id, "thought_level", "high")
	_, promptErr := s.client.Prompt(context.Background(), promptOf(id, "hi"))

	// then
	// ... high is current, and acme/large built at high answered the turn under the same name
	if setErr != nil || promptErr != nil {
		t.Fatalf("set: %v; prompt: %v", setErr, promptErr)
	}
	if got := optionLines(resp.ConfigOptions)[1]; got != "thought_level (thought_level) = high: low medium high" {
		t.Errorf("thought_level = %q, want high current", got)
	}
	if sent := high.sent(); len(sent) != 1 || sent[0].Model != "acme/large" {
		t.Errorf("acme/large@high's requests = %+v, want one for acme/large", sent)
	}
}

func TestAnUnknownOptionOrValueIsInvalidParams(t *testing.T) {
	for _, c := range []struct{ name, configID, value string }{
		{"unknown option", "mode", "ask"},
		{"unknown model", "model", "acme/huge"},
		{"effort the model doesn't list", "thought_level", "max"},
	} {
		t.Run(c.name, func(t *testing.T) {
			// given
			// ... a session on acme/large
			f := newCatalogue()
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			s := connectModels(t, f)
			s.initialize(t)
			id := s.newSession(t)

			// when
			// ... the client sets the option
			_, err := s.set(id, c.configID, c.value)

			// then
			// ... it is invalid params and nothing was built
			if code := requestCode(t, err); code != -32602 {
				t.Errorf("code = %d, want -32602", code)
			}
			if got := f.builds(); len(got) != 0 {
				t.Errorf("builds = %q, want none", got)
			}
		})
	}
}

func TestLoadRestoresTheSavedModelAndEffort(t *testing.T) {
	// given
	// ... a session switched to acme/small at high, then left
	first := newCatalogue()
	first.script("acme/small@high", modelReply{chunks: []string{"first"}})
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	one := connectModels(t, first)
	one.initialize(t)
	id := one.newSession(t)
	if _, err := one.set(id, "model", "acme/small"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, err := one.client.Prompt(context.Background(), promptOf(id, "one")); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	second := newCatalogue()
	small := second.script("acme/small@high", modelReply{chunks: []string{"second"}})
	two := connectModels(t, second)
	two.initialize(t)

	// when
	// ... a new process loads the session, then the client prompts
	resp, loadErr := two.client.LoadSession(context.Background(), sdk.LoadSessionRequest{SessionId: id, Cwd: "/work", McpServers: []sdk.McpServer{}})
	_, promptErr := two.client.Prompt(context.Background(), promptOf(id, "two"))

	// then
	// ... acme/small at high is current again and answered the turn
	if loadErr != nil || promptErr != nil {
		t.Fatalf("load: %v; prompt: %v", loadErr, promptErr)
	}
	want := []string{
		"model (model) = acme/small: acme/large[low medium high] acme/small[low high max] local/gemma[] locked/vault[low]",
		"thought_level (thought_level) = high: low high max",
	}
	if got := optionLines(resp.ConfigOptions); !slices.Equal(got, want) {
		t.Errorf("configOptions = %q, want %q", got, want)
	}
	if n := len(small.sent()); n != 1 {
		t.Errorf("acme/small called %d times, want 1", n)
	}
}

func TestLoadWhoseSavedModelCantBeBuiltStaysOnTheDefault(t *testing.T) {
	// given
	// ... a session switched to acme/small, loaded by a process where acme's key now fails
	first := newCatalogue()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	one := connectModels(t, first)
	one.initialize(t)
	id := one.newSession(t)
	if _, err := one.set(id, "model", "acme/small"); err != nil {
		t.Fatalf("set: %v", err)
	}
	second := newCatalogue()
	second.failing["acme/small"] = "providers.acme: api key command: exit status 1"
	two := connectModels(t, second, modelReply{chunks: []string{"large"}})
	two.initialize(t)

	// when
	// ... the second process loads the session, then the client prompts
	resp, loadErr := two.client.LoadSession(context.Background(), sdk.LoadSessionRequest{SessionId: id, Cwd: "/work", McpServers: []sdk.McpServer{}})
	_, promptErr := two.client.Prompt(context.Background(), promptOf(id, "hi"))

	// then
	// ... the load succeeded on acme/large at medium, which answered the turn
	if loadErr != nil || promptErr != nil {
		t.Fatalf("load: %v; prompt: %v", loadErr, promptErr)
	}
	if got := optionLines(resp.ConfigOptions)[1]; got != "thought_level (thought_level) = medium: low medium high" {
		t.Errorf("thought_level = %q, want acme/large's at medium", got)
	}
	if n := len(two.model.sent()); n != 1 {
		t.Errorf("acme/large called %d times, want 1", n)
	}
}
