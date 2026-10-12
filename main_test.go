package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"grimalkin/internal/config"
	"grimalkin/internal/core"
)

func TestASubagentEntrysModelSendsToItsProviderWithItsKeyThinkingAndEffort(t *testing.T) {
	// given
	// ... an endpoint recording the request it refuses, and the models built
	// ... from two entries, the second on that endpoint
	var path, key string
	var body struct {
		Model        string
		Thinking     map[string]any `json:"thinking"`
		OutputConfig map[string]any `json:"output_config"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, key = r.URL.Path, r.Header.Get("X-Api-Key")
		data, _ := io.ReadAll(r.Body)
		json.Unmarshal(data, &body)
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)
	models, err := subagentModels([]config.SubagentModel{
		{Provider: "anthropic", Wire: config.WireAnthropicMessages, BaseURL: "http://127.0.0.1:1", APIKey: "sk-a", Model: "haiku", Thinking: "adaptive"},
		{Provider: "ollama", Wire: config.WireAnthropicMessages, BaseURL: server.URL, APIKey: "sk-o", Model: "glm-flash", Thinking: "disabled", Effort: "low"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// when
	// ... the second model is sent a prompt
	models[1].Model.Stream(context.Background(), core.ModelRequest{Messages: []core.Message{{Role: core.RoleUser, Text: "hi"}}}, func(string) {})

	// then
	// ... the models are named provider/model in order, and the request went
	// ... to the entry's endpoint with its key, model, thinking and effort
	if names := []string{models[0].Name, models[1].Name}; !reflect.DeepEqual(names, []string{"anthropic/haiku", "ollama/glm-flash"}) {
		t.Errorf("names = %q, want anthropic/haiku then ollama/glm-flash", names)
	}
	if path != "/v1/messages" || key != "sk-o" {
		t.Errorf("path, key = %q, %q, want /v1/messages, sk-o", path, key)
	}
	if body.Model != "glm-flash" || body.Thinking["type"] != "disabled" || body.OutputConfig["effort"] != "low" {
		t.Errorf("model, thinking, effort = %q, %v, %v, want glm-flash, disabled, low", body.Model, body.Thinking["type"], body.OutputConfig["effort"])
	}
}

func TestASubagentEntryWhoseWireHasNoAdapterIsAnErrorNamingTheEntry(t *testing.T) {
	// given
	// ... two entries, the second on a wire with no adapter
	entries := []config.SubagentModel{
		{Provider: "ollama", Wire: config.WireAnthropicMessages, BaseURL: "http://127.0.0.1:1", APIKey: "sk-o", Model: "glm-flash", Thinking: "enabled"},
		{Provider: "relay", Wire: "smoke-signals", BaseURL: "http://127.0.0.1:1", APIKey: "sk-r", Model: "puff"},
	}

	// when
	// ... their models are built
	_, err := subagentModels(entries)

	// then
	// ... building fails, naming the entry and its wire
	if want := `subagents.models[1] (relay/puff): no adapter for wire "smoke-signals"`; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want it to contain %q", err, want)
	}
}
