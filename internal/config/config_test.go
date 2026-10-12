package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type raw map[string]any

func (c raw) provider(name string) raw {
	return c["providers"].(raw)[name].(raw)
}

func (c raw) model(provider, name string) raw {
	return c.provider(provider)["models"].(raw)[name].(raw)
}

// validConfig selects ollama's glm. The unselected anthropic provider's key
// command fails, so any config that loads proves it was never run.
func validConfig() raw {
	return raw{
		"provider": "ollama",
		"model":    "glm",
		"effort":   "high",
		"providers": raw{
			"anthropic": raw{
				"wire":     "anthropic-messages",
				"baseUrl":  "https://api.anthropic.com",
				"apiKey":   "!false",
				"thinking": "adaptive",
				"efforts":  []any{"low", "high"},
				"models": raw{
					"opus":  raw{},
					"haiku": raw{"thinking": "enabled"},
				},
			},
			"ollama": raw{
				"wire":     "anthropic-messages",
				"baseUrl":  "http://localhost:11434",
				"apiKey":   "ollama",
				"thinking": "enabled",
				"models": raw{
					"glm":   raw{"efforts": []any{"low", "high", "max"}},
					"flash": raw{"efforts": []any{"low", "high", "max"}},
					"gemma": raw{},
				},
			},
		},
	}
}

// subagentsBlock is a subagents block allowing the given entries.
func subagentsBlock(entries ...raw) raw {
	models := make([]any, len(entries))
	for i, e := range entries {
		models[i] = e
	}
	return raw{"models": models}
}

func writeConfig(t *testing.T, c raw) string {
	t.Helper()
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAValidConfigResolvesTheSelectedModelWithoutRunningOtherKeyCommands(t *testing.T) {
	// given
	// ... a config selecting ollama's glm, whose unselected anthropic key command would fail
	path := writeConfig(t, validConfig())

	// when
	// ... it is loaded
	sel, err := Load(path)

	// then
	// ... the selection carries ollama's endpoint and thinking, and glm's effort
	if err != nil {
		t.Fatal(err)
	}
	want := Selection{
		Provider: "ollama",
		Wire:     "anthropic-messages",
		BaseURL:  "http://localhost:11434",
		APIKey:   "ollama",
		Model:    "glm",
		Thinking: "enabled",
		Effort:   "high",
		WebInfo:  "web: off (no provider configured)",
	}
	if !reflect.DeepEqual(sel, want) {
		t.Errorf("selection = %+v, want %+v", sel, want)
	}
}

func TestAModelsThinkingOverridesItsProviders(t *testing.T) {
	// given
	// ... anthropic's haiku selected, which overrides the provider's adaptive thinking
	c := validConfig()
	c["provider"], c["model"] = "anthropic", "haiku"
	c.provider("anthropic")["apiKey"] = "sk-test"
	path := writeConfig(t, c)

	// when
	// ... it is loaded
	sel, err := Load(path)

	// then
	// ... thinking is the model's, and effort is checked against the provider's list
	if err != nil {
		t.Fatal(err)
	}
	if sel.Thinking != "enabled" || sel.Effort != "high" {
		t.Errorf("thinking, effort = %q, %q, want enabled, high", sel.Thinking, sel.Effort)
	}
}

func TestAModelWithNoEffortLevelsResolvesToNoEffort(t *testing.T) {
	// given
	// ... ollama's gemma selected, which lists no efforts, under a provider that lists none
	c := validConfig()
	c["model"] = "gemma"
	path := writeConfig(t, c)

	// when
	// ... it is loaded
	sel, err := Load(path)

	// then
	// ... the configured effort is dropped rather than rejected
	if err != nil {
		t.Fatal(err)
	}
	if sel.Effort != "" {
		t.Errorf("effort = %q, want none", sel.Effort)
	}
}

func TestAKeyCommandsOutputIsTheKeyWithTrailingWhitespaceTrimmed(t *testing.T) {
	// given
	// ... a selected provider whose key comes from a command printing trailing whitespace
	c := validConfig()
	c.provider("ollama")["apiKey"] = `!printf 'sk-1 \n\n'`
	path := writeConfig(t, c)

	// when
	// ... it is loaded
	sel, err := Load(path)

	// then
	// ... the key is the command's output, trimmed
	if err != nil {
		t.Fatal(err)
	}
	if sel.APIKey != "sk-1" {
		t.Errorf("key = %q, want sk-1", sel.APIKey)
	}
}

func TestASearchBlockResolvesItsKeyAndDefaultsTheModel(t *testing.T) {
	// given
	// ... a search block whose key comes from a command and which names no model
	c := validConfig()
	c["search"] = raw{"apiKey": `!printf 'jev-1\n'`}
	path := writeConfig(t, c)

	// when
	// ... it is loaded
	sel, err := Load(path)

	// then
	// ... search carries the trimmed key and the default model
	if err != nil {
		t.Fatal(err)
	}
	want := &Search{APIKey: "jev-1", Model: "jev-latest"}
	if !reflect.DeepEqual(sel.Search, want) {
		t.Errorf("search = %+v, want %+v", sel.Search, want)
	}
}

func TestASearchBlockCanNameItsModel(t *testing.T) {
	// given
	// ... a search block pinning a model version
	c := validConfig()
	c["search"] = raw{"apiKey": "jev-1", "model": "jev-1.13.0", "map": true, "mapAtStartup": true}
	path := writeConfig(t, c)

	// when
	// ... it is loaded
	sel, err := Load(path)

	// then
	// ... that model is used
	if err != nil {
		t.Fatal(err)
	}
	if sel.Search == nil || sel.Search.Model != "jev-1.13.0" || !sel.Search.Map || !sel.Search.MapAtStartup {
		t.Errorf("search = %+v, want model jev-1.13.0 with the map on, built at startup", sel.Search)
	}
}

func TestWithoutAJevKeyThereIsNoSearchButGrimalkinStillStarts(t *testing.T) {
	cases := []struct {
		name   string
		search raw
		want   string
	}{
		{"no apiKey", raw{"model": "jev-latest"}, `search: off (no "apiKey")`},
		{"failing key command", raw{"apiKey": "!exit 4"}, "search: off (api key command: exit status 4)"},
		{"silent key command", raw{"apiKey": "!true"}, "search: off (api key command printed nothing)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// given
			// ... a search block with no usable key
			c := validConfig()
			c["search"] = tc.search
			path := writeConfig(t, c)

			// when
			// ... it is loaded
			sel, err := Load(path)

			// then
			// ... loading succeeds without search, and says why
			if err != nil {
				t.Fatal(err)
			}
			if sel.Search != nil || sel.SearchOff != tc.want {
				t.Errorf("search = %+v, off = %q, want none and %q", sel.Search, sel.SearchOff, tc.want)
			}
		})
	}
}

func TestInvalidConfigsAreRejected(t *testing.T) {
	cases := []struct {
		name    string
		breakIt func(raw)
		want    string
	}{
		{"missing provider", func(c raw) { delete(c, "provider") }, `missing "provider"`},
		{"missing model", func(c raw) { delete(c, "model") }, `missing "model"`},
		{"missing effort", func(c raw) { delete(c, "effort") }, `missing "effort"`},
		{"missing providers", func(c raw) { delete(c, "providers") }, `missing "providers"`},
		{"undeclared provider", func(c raw) { c["provider"] = "nope" }, `provider "nope" is not declared`},
		{"unlisted model", func(c raw) { c["model"] = "nope" }, `model "nope" is not listed under provider "ollama"`},
		{"effort outside the model's efforts", func(c raw) { c["effort"] = "medium" }, `effort "medium" is not one of`},
		{"missing wire", func(c raw) { delete(c.provider("anthropic"), "wire") }, `providers.anthropic: missing "wire"`},
		{"missing baseUrl", func(c raw) { delete(c.provider("anthropic"), "baseUrl") }, `providers.anthropic: missing "baseUrl"`},
		{"missing apiKey", func(c raw) { delete(c.provider("anthropic"), "apiKey") }, `providers.anthropic: missing "apiKey"`},
		{"missing thinking", func(c raw) { delete(c.provider("anthropic"), "thinking") }, `providers.anthropic: missing "thinking"`},
		{"missing models", func(c raw) { delete(c.provider("anthropic"), "models") }, `providers.anthropic: missing "models"`},
		{"unknown wire", func(c raw) { c.provider("anthropic")["wire"] = "smoke-signals" }, `providers.anthropic: unknown wire "smoke-signals"`},
		{"bad provider thinking", func(c raw) { c.provider("anthropic")["thinking"] = "sometimes" }, `providers.anthropic: thinking "sometimes"`},
		{"empty provider efforts", func(c raw) { c.provider("anthropic")["efforts"] = []any{} }, `providers.anthropic: efforts is empty`},
		{"empty models", func(c raw) { c.provider("anthropic")["models"] = raw{} }, `providers.anthropic: models is empty`},
		{"bad model thinking", func(c raw) { c.model("anthropic", "haiku")["thinking"] = "sometimes" }, `providers.anthropic.models.haiku: thinking "sometimes"`},
		{"empty model efforts", func(c raw) { c.model("anthropic", "opus")["efforts"] = []any{} }, `providers.anthropic.models.opus: efforts is empty`},
		{"unknown top-level key", func(c raw) { c["modle"] = "x" }, `unknown field "modle"`},
		{"unknown provider key", func(c raw) { c.provider("anthropic")["base_url"] = "x" }, `unknown field "base_url"`},
		{"unknown model key", func(c raw) { c.model("anthropic", "haiku")["thinkng"] = "x" }, `unknown field "thinkng"`},
		{"failing key command", func(c raw) { c.provider("ollama")["apiKey"] = "!exit 3" }, `providers.ollama: api key command: exit status 3`},
		{"empty skill dir", func(c raw) { c["skillDirs"] = []any{"a", ""} }, `skillDirs[1] is empty`},
		{"map at startup without the map", func(c raw) { c["search"] = raw{"apiKey": "k", "mapAtStartup": true} }, `search: "mapAtStartup" needs "map": true`},
		{"unknown search key", func(c raw) { c["search"] = raw{"apiKey": "k", "baseUrl": "x"} }, `unknown field "baseUrl"`},
		{"silent key command", func(c raw) { c.provider("ollama")["apiKey"] = "!true" }, `providers.ollama: api key command printed nothing`},
		{"unknown web provider", func(c raw) { c["web"] = raw{"nope": raw{"apiKey": "k"}} }, `web.nope: unknown provider (known: ollama)`},
		{"web provider without a key", func(c raw) { c["web"] = raw{"ollama": raw{}} }, `web.ollama: missing "apiKey"`},
		{"unknown web provider key", func(c raw) { c["web"] = raw{"ollama": raw{"apiKey": "k", "baseUrl": "x"}} }, `unknown field "baseUrl"`},
		{"subagent entry without a provider", func(c raw) { c["subagents"] = subagentsBlock(raw{"model": "glm"}) }, `subagents.models[0]: missing "provider"`},
		{"subagent entry without a model", func(c raw) { c["subagents"] = subagentsBlock(raw{"provider": "ollama"}) }, `subagents.models[0]: missing "model"`},
		{"subagent entry with an undeclared provider", func(c raw) {
			c["subagents"] = subagentsBlock(raw{"provider": "ollama", "model": "flash"}, raw{"provider": "nope", "model": "glm"})
		}, `subagents.models[1]: provider "nope" is not declared`},
		{"subagent entry with an unlisted model", func(c raw) { c["subagents"] = subagentsBlock(raw{"provider": "ollama", "model": "nope"}) }, `subagents.models[0]: model "nope" is not listed under provider "ollama"`},
		{"subagent entry with an effort outside the model's efforts", func(c raw) {
			c["subagents"] = subagentsBlock(raw{"provider": "ollama", "model": "flash", "effort": "medium"})
		}, `subagents.models[0]: effort "medium" is not one of flash's efforts: low, high, max`},
		{"subagent entry with an effort for a model that has none", func(c raw) {
			c["subagents"] = subagentsBlock(raw{"provider": "ollama", "model": "gemma", "effort": "low"})
		}, `subagents.models[0]: effort "low" given, but gemma has no effort levels`},
		{"subagent entry listed twice", func(c raw) {
			c["subagents"] = subagentsBlock(raw{"provider": "ollama", "model": "gemma"}, raw{"provider": "ollama", "model": "gemma"})
		}, `subagents.models[1]: ollama/gemma is already listed`},
		{"subagent entry naming the selected model", func(c raw) {
			c["subagents"] = subagentsBlock(raw{"provider": "ollama", "model": "gemma"}, raw{"provider": "ollama", "model": "glm", "effort": "low"})
		}, `subagents.models[1]: ollama/glm is the selected model, which subagents already use`},
		{"subagent entry whose provider's key command fails", func(c raw) { c["subagents"] = subagentsBlock(raw{"provider": "anthropic", "model": "opus"}) }, `providers.anthropic: api key command: exit status 1`},
		{"unknown subagents key", func(c raw) { c["subagents"] = raw{"model": []any{}} }, `unknown field "model"`},
		{"unknown subagent entry key", func(c raw) {
			c["subagents"] = subagentsBlock(raw{"provider": "ollama", "model": "flash", "thinking": "enabled"})
		}, `unknown field "thinking"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// given
			// ... a valid config broken in one place
			c := validConfig()
			tc.breakIt(c)
			path := writeConfig(t, c)

			// when
			// ... it is loaded
			_, err := Load(path)

			// then
			// ... loading fails, naming what is wrong
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestAWebBlockResolvesItsProvidersKey(t *testing.T) {
	// given
	// ... a web block with ollama, its key from a command
	c := validConfig()
	c["web"] = raw{"ollama": raw{"apiKey": `!printf 'ow-1\n'`}}
	path := writeConfig(t, c)

	// when
	// ... it is loaded
	sel, err := Load(path)

	// then
	// ... web carries the provider and its trimmed key, and the line names it
	if err != nil {
		t.Fatal(err)
	}
	want := &Web{Provider: "ollama", APIKey: "ow-1"}
	if !reflect.DeepEqual(sel.Web, want) {
		t.Errorf("web = %+v, want %+v", sel.Web, want)
	}
	if want := "web: on · ollama · search queries and fetched URLs go to it"; sel.WebInfo != want {
		t.Errorf("info = %q, want %q", sel.WebInfo, want)
	}
}

func TestOfSeveralWebProvidersTheFirstByNameIsUsedAndTheLineSaysSo(t *testing.T) {
	// given
	// ... a second known provider, and a web block with both, ollama's key command failing
	webProviders = []string{"ollama", "brave"}
	t.Cleanup(func() { webProviders = []string{"ollama"} })
	c := validConfig()
	c["web"] = raw{"ollama": raw{"apiKey": "!false"}, "brave": raw{"apiKey": "b-1"}}
	path := writeConfig(t, c)

	// when
	// ... it is loaded
	sel, err := Load(path)

	// then
	// ... brave is used, ollama's key command never ran, and the line lists both
	if err != nil {
		t.Fatal(err)
	}
	want := &Web{Provider: "brave", APIKey: "b-1"}
	if !reflect.DeepEqual(sel.Web, want) {
		t.Errorf("web = %+v, want %+v", sel.Web, want)
	}
	if want := "web: on · brave · 2 providers configured (brave, ollama), using the first"; sel.WebInfo != want {
		t.Errorf("info = %q, want %q", sel.WebInfo, want)
	}
}

func TestWithoutAUsableWebProviderThereAreNoWebToolsButGrimalkinStillStarts(t *testing.T) {
	cases := []struct {
		name string
		web  raw
		want string
	}{
		{"empty block", raw{}, "web: off (no provider configured)"},
		{"failing key command", raw{"ollama": raw{"apiKey": "!exit 4"}}, "web: off (ollama: api key command: exit status 4)"},
		{"silent key command", raw{"ollama": raw{"apiKey": "!true"}}, "web: off (ollama: api key command printed nothing)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// given
			// ... a web block with no usable provider
			c := validConfig()
			c["web"] = tc.web
			path := writeConfig(t, c)

			// when
			// ... it is loaded
			sel, err := Load(path)

			// then
			// ... loading succeeds without web, and says why
			if err != nil {
				t.Fatal(err)
			}
			if sel.Web != nil || sel.WebInfo != tc.want {
				t.Errorf("web = %+v, info = %q, want none and %q", sel.Web, sel.WebInfo, tc.want)
			}
		})
	}
}

func TestASubagentsBlockResolvesItsEntriesInTheOrderWritten(t *testing.T) {
	// given
	// ... three allowed subagent models across both providers, anthropic's key from a command
	c := validConfig()
	c.provider("anthropic")["apiKey"] = `!printf 'sk-a\n'`
	c["subagents"] = subagentsBlock(
		raw{"provider": "ollama", "model": "flash", "effort": "low"},
		raw{"provider": "anthropic", "model": "haiku", "effort": "high"},
		raw{"provider": "ollama", "model": "gemma"},
	)
	path := writeConfig(t, c)

	// when
	// ... it is loaded
	sel, err := Load(path)

	// then
	// ... each entry carries its provider's endpoint and key, and its model's thinking and effort
	if err != nil {
		t.Fatal(err)
	}
	want := []SubagentModel{
		{Provider: "ollama", Wire: "anthropic-messages", BaseURL: "http://localhost:11434", APIKey: "ollama", Model: "flash", Thinking: "enabled", Effort: "low"},
		{Provider: "anthropic", Wire: "anthropic-messages", BaseURL: "https://api.anthropic.com", APIKey: "sk-a", Model: "haiku", Thinking: "enabled", Effort: "high"},
		{Provider: "ollama", Wire: "anthropic-messages", BaseURL: "http://localhost:11434", APIKey: "ollama", Model: "gemma", Thinking: "enabled"},
	}
	if !reflect.DeepEqual(sel.Subagents, want) {
		t.Errorf("subagents = %+v, want %+v", sel.Subagents, want)
	}
}

func TestASubagentEntryWithoutAnEffortResolvesToNoEffort(t *testing.T) {
	// given
	// ... an entry naming no effort for flash, which has effort levels
	c := validConfig()
	c["subagents"] = subagentsBlock(raw{"provider": "ollama", "model": "flash"})
	path := writeConfig(t, c)

	// when
	// ... it is loaded
	sel, err := Load(path)

	// then
	// ... the entry has no effort, not the selection's
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Subagents) != 1 || sel.Subagents[0].Effort != "" {
		t.Errorf("subagents = %+v, want one entry with no effort", sel.Subagents)
	}
}

func TestAProviderNoSubagentEntryOrSelectionUsesHasNoKeyCommandRun(t *testing.T) {
	// given
	// ... a subagents block using only ollama, while anthropic's key command would fail
	c := validConfig()
	c["subagents"] = subagentsBlock(raw{"provider": "ollama", "model": "gemma"})
	path := writeConfig(t, c)

	// when
	// ... it is loaded
	_, err := Load(path)

	// then
	// ... loading succeeds, so anthropic's key command never ran
	if err != nil {
		t.Errorf("err = %v, want none", err)
	}
}

func TestAProviderUsedBySeveralEntriesAndTheSelectionHasItsKeyCommandRunOnce(t *testing.T) {
	// given
	// ... ollama selected and named by two entries, its key command recording each run
	runs := filepath.Join(t.TempDir(), "runs")
	c := validConfig()
	c.provider("ollama")["apiKey"] = "!echo run >> '" + runs + "'; echo sk-o"
	c["subagents"] = subagentsBlock(
		raw{"provider": "ollama", "model": "flash", "effort": "low"},
		raw{"provider": "ollama", "model": "gemma"},
	)
	path := writeConfig(t, c)

	// when
	// ... it is loaded
	sel, err := Load(path)

	// then
	// ... the command ran once, and the selection and both entries carry its key
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := os.ReadFile(runs)
	if err != nil {
		t.Fatal(err)
	}
	if string(recorded) != "run\n" {
		t.Errorf("key command runs = %q, want one", recorded)
	}
	keys := []string{sel.APIKey, sel.Subagents[0].APIKey, sel.Subagents[1].APIKey}
	if want := []string{"sk-o", "sk-o", "sk-o"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("keys = %q, want %q", keys, want)
	}
}
