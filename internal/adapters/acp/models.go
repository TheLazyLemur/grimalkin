package acp

import (
	"context"
	"fmt"
	"log"
	"slices"

	sdk "github.com/coder/acp-go-sdk"

	"grimalkin/internal/core"
)

// Models are the models a session may run on, offered to the client as the
// model and thought_level config options, and the one it starts on.
type Models struct {
	List    []Model // every model the client may pick, in the order offered
	Default string  // the model a session starts on, by its Name in List
	Effort  string  // the default's effort: one of its Efforts, or empty when it has none
	// Build builds the named model at effort (empty when it has no effort
	// levels). It is called when the client picks a model or an effort, and
	// when a load restores one other than the default. It may run the model's
	// provider's key command, and its error is then the command's.
	Build func(name, effort string) (core.Model, error)
}

// Model is one model the client may pick.
type Model struct {
	Name    string   // provider/model, as Agent.SetModel takes it
	Efforts []string // lowest first; nil when it has no effort levels
}

// choice is a session's model and effort. It is also a session file's model
// record: the session runs on it from there on.
type choice struct {
	Name   string `json:"name"`
	Effort string `json:"effort,omitempty"`
}

const (
	configModel  sdk.SessionConfigId = "model"
	configEffort sdk.SessionConfigId = "thought_level"
)

// check panics unless the default is listed with an effort it takes.
func (m Models) check() {
	model, ok := m.find(m.Default)
	if !ok || !model.takes(m.Effort) || m.Build == nil {
		panic(fmt.Sprintf("models: default %q at effort %q is not one listed, or there is no Build", m.Default, m.Effort))
	}
}

func (m Models) find(name string) (Model, bool) {
	i := slices.IndexFunc(m.List, func(model Model) bool { return model.Name == name })
	if i < 0 {
		return Model{}, false
	}
	return m.List[i], true
}

// takes says whether effort is one this model can run at: one of its
// efforts, or none when it has no levels.
func (m Model) takes(effort string) bool {
	if len(m.Efforts) == 0 {
		return effort == ""
	}
	return slices.Contains(m.Efforts, effort)
}

// switchedEffort is the effort a session at effort on a model listing from
// takes on a model listing to: effort when to lists it, else the next level
// above it in from that to lists, else to's highest; none when to has no
// levels. It is config's rule for a subagent entry's inherited effort, but
// for a model with none, which takes to's highest here.
func switchedEffort(effort string, from, to []string) string {
	if len(to) == 0 {
		return ""
	}
	if at := slices.Index(from, effort); at >= 0 {
		for _, level := range from[at:] {
			if slices.Contains(to, level) {
				return level
			}
		}
	}
	return to[len(to)-1]
}

// configOptions is the session's config options for c: every model, each
// with its efforts in _meta so the client needn't switch to it to learn
// them, and c's model's efforts when it has any.
func (m Models) configOptions(c choice) []sdk.SessionConfigOption {
	models := make(sdk.SessionConfigSelectOptionsUngrouped, len(m.List))
	for i, model := range m.List {
		efforts := model.Efforts
		if efforts == nil {
			efforts = []string{} // [] on the wire: no levels
		}
		models[i] = sdk.SessionConfigSelectOption{Name: model.Name, Value: sdk.SessionConfigValueId(model.Name), Meta: map[string]any{"efforts": efforts}}
	}
	category := sdk.SessionConfigOptionCategoryModel
	options := []sdk.SessionConfigOption{{Select: &sdk.SessionConfigOptionSelect{
		Id: configModel, Name: "Model", Category: &category,
		CurrentValue: sdk.SessionConfigValueId(c.Name),
		Options:      sdk.SessionConfigSelectOptions{Ungrouped: &models},
	}}}
	current, ok := m.find(c.Name)
	if !ok {
		panic("config options for an unlisted model " + c.Name)
	}
	if len(current.Efforts) == 0 {
		return options
	}
	efforts := make(sdk.SessionConfigSelectOptionsUngrouped, len(current.Efforts))
	for i, effort := range current.Efforts {
		efforts[i] = sdk.SessionConfigSelectOption{Name: effort, Value: sdk.SessionConfigValueId(effort)}
	}
	thought := sdk.SessionConfigOptionCategoryThoughtLevel
	return append(options, sdk.SessionConfigOption{Select: &sdk.SessionConfigOptionSelect{
		Id: configEffort, Name: "Effort", Category: &thought,
		CurrentValue: sdk.SessionConfigValueId(c.Effort),
		Options:      sdk.SessionConfigSelectOptions{Ungrouped: &efforts},
	}})
}

// SetConfigOption switches the session's model or effort from its next turn
// on, saving the choice so a load restores it. A model that can't be built,
// as when its provider's key command fails, leaves the session as it was.
func (s *Server) SetConfigOption(_ context.Context, p sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
	if p.ValueId == nil {
		return sdk.SetSessionConfigOptionResponse{}, sdk.NewInvalidParams(map[string]any{"error": "every option takes a value id"})
	}
	set := p.ValueId
	s.configMu.Lock()
	defer s.configMu.Unlock()
	s.mu.Lock()
	if s.sessionID == "" || set.SessionId != s.sessionID {
		s.mu.Unlock()
		return sdk.SetSessionConfigOptionResponse{}, sdk.NewInvalidParams(map[string]any{"error": "unknown session " + string(set.SessionId)})
	}
	current := s.current
	s.mu.Unlock()

	value := string(set.Value)
	running, _ := s.models.find(current.Name)
	var next choice
	switch set.ConfigId {
	case configModel:
		model, ok := s.models.find(value)
		if !ok {
			return sdk.SetSessionConfigOptionResponse{}, sdk.NewInvalidParams(map[string]any{"error": "unknown model " + value})
		}
		next = choice{Name: model.Name, Effort: switchedEffort(current.Effort, running.Efforts, model.Efforts)}
	case configEffort:
		if !slices.Contains(running.Efforts, value) {
			return sdk.SetSessionConfigOptionResponse{}, sdk.NewInvalidParams(map[string]any{"error": fmt.Sprintf("%s has no effort %q", current.Name, value)})
		}
		next = choice{Name: current.Name, Effort: value}
	default:
		return sdk.SetSessionConfigOptionResponse{}, sdk.NewInvalidParams(map[string]any{"error": "unknown config option " + string(set.ConfigId)})
	}
	// Built without s.mu held: it may run a key command.
	model, err := s.models.Build(next.Name, next.Effort)
	if err != nil {
		return sdk.SetSessionConfigOptionResponse{}, &sdk.RequestError{Code: -32603, Message: err.Error(), Data: map[string]any{"error": err.Error()}}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.save(record{Model: &next}); err != nil {
		return sdk.SetSessionConfigOptionResponse{}, sdk.NewInternalError(map[string]any{"error": err.Error()})
	}
	s.current = next
	s.agent.SetModel(model, next.Name)
	return sdk.SetSessionConfigOptionResponse{ConfigOptions: s.models.configOptions(next)}, nil
}

// restoreModel switches agent to a loaded session's saved model, and returns
// what the session runs on. A saved model the config no longer lists at that
// effort, or one that can't be built, leaves the default, with a note on
// stderr: the session still loads, and its options say which model it is on.
func (s *Server) restoreModel(agent Agent, saved *choice) choice {
	def := choice{Name: s.models.Default, Effort: s.models.Effort}
	if saved == nil || *saved == def {
		return def
	}
	if model, ok := s.models.find(saved.Name); !ok || !model.takes(saved.Effort) {
		log.Printf("acp: the session's model %s at effort %q is not in the config; using %s", saved.Name, saved.Effort, def.Name)
		return def
	}
	model, err := s.models.Build(saved.Name, saved.Effort)
	if err != nil {
		log.Printf("acp: the session's model %s can't be used (%v); using %s", saved.Name, err, def.Name)
		return def
	}
	agent.SetModel(model, saved.Name)
	return *saved
}
