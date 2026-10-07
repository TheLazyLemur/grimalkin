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
					"gemma": raw{},
				},
			},
		},
	}
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
