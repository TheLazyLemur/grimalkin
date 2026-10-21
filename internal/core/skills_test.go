package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

type mockSkills struct {
	skills  []Skill
	render  func(name, arguments string) (string, error)
	renders []string // "name|arguments" per call
}

func (m *mockSkills) List() []Skill { return m.skills }

func (m *mockSkills) Render(_ context.Context, name, arguments string) (string, error) {
	m.renders = append(m.renders, name+"|"+arguments)
	return m.render(name, arguments)
}

// skillLibrary has a skill for both, one only the model may load and one
// only the user may.
func skillLibrary() *mockSkills {
	return &mockSkills{
		skills: []Skill{
			{Name: "review", Description: "Review <diffs> & more", ModelInvocable: true, UserInvocable: true},
			{Name: "lore", Description: "Background", ModelInvocable: true},
			{Name: "deploy", Description: "Ship it", UserInvocable: true},
		},
		render: func(name, arguments string) (string, error) {
			return "<skill_content name=\"" + name + "\">" + arguments + "</skill_content>", nil
		},
	}
}

func skillCall(id, name, arguments string) ToolCall {
	input, _ := json.Marshal(map[string]string{"name": name, "arguments": arguments})
	return toolCall(id, SkillToolName, string(input))
}

func TestTheSystemPromptListsOnlySkillsTheModelMayLoad(t *testing.T) {
	// given
	// ... a library with a user-only skill and a description with markup in it

	// when
	// ... the system prompt is built
	prompt := SystemPrompt("Darwin", "/p", skillLibrary().List())

	// then
	// ... the catalog lists the model's skills, escaped, and leaves the user-only one out
	for _, want := range []string{"<available_skills>", "<name>review</name>", "<description>Review &lt;diffs&gt; &amp; more</description>", "<name>lore</name>"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if strings.Contains(prompt, "deploy") {
		t.Error("prompt lists the user-only skill")
	}
}

func TestWithNoModelSkillsThereIsNoCatalogAndNoSkillTool(t *testing.T) {
	// given
	// ... a library whose only skill the model may not load
	skills := &mockSkills{skills: []Skill{{Name: "deploy", Description: "Ship it", UserInvocable: true}}}
	model := &mockModel{replies: []mockReply{{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A1")}}}}
	a := newAgent(model, &mockTranscript{})
	a.Skills = skills

	// when
	// ... the prompt is built and a turn runs
	prompt := SystemPrompt("Darwin", "/p", skills.List())
	a.RunTurn("hi")

	// then
	// ... neither the catalog nor the tool exist
	if strings.Contains(prompt, "Skills") {
		t.Error("prompt has a skills section")
	}
	if specs := a.toolSpecs(); len(specs) != 0 {
		t.Errorf("tools = %v", specs)
	}
}

func TestTheSkillToolOffersOnlyModelSkillsByName(t *testing.T) {
	// given
	// ... an agent with the library
	a := newAgent(&mockModel{}, &mockTranscript{})
	a.Skills = skillLibrary()

	// when
	// ... its tools are listed
	specs := a.toolSpecs()

	// then
	// ... the skill tool's name is an enum of the model's skills
	if len(specs) != 1 || specs[0].Name != SkillToolName {
		t.Fatalf("tools = %v", specs)
	}
	var schema struct {
		Properties struct{ Name struct{ Enum []string } }
	}
	if err := json.Unmarshal([]byte(specs[0].InputSchema), &schema); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(schema.Properties.Name.Enum, []string{"review", "lore"}) {
		t.Errorf("enum = %v", schema.Properties.Name.Enum)
	}
}

func TestTheModelLoadsASkillAndGetsItsRenderedContentUncapped(t *testing.T) {
	// given
	// ... a model that loads a skill with arguments, and content past the tool output cap
	big := strings.Repeat("x", maxToolResultBytes+10)
	skills := skillLibrary()
	skills.render = func(name, arguments string) (string, error) { return big, nil }
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{skillCall("toolu_1", "review", "a b")}, Content: raw("A1")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A2")}},
	}}
	transcript := &mockTranscript{}
	a := newAgent(model, transcript)
	a.Skills = skills

	// when
	// ... a turn runs
	a.RunTurn("review my diff")

	// then
	// ... the skill rendered with the arguments, the call shows as a command and the model got it all
	if !slices.Equal(skills.renders, []string{"review|a b"}) {
		t.Errorf("renders = %v", skills.renders)
	}
	if started := transcript.events[1]; started != (ToolStarted{ID: "toolu_1", Name: SkillToolName, Summary: "skill review a b"}) {
		t.Errorf("event = %#v", started)
	}
	if got := lastToolResult(model.requests, 1); got.Content != big || got.IsError {
		t.Errorf("result: %d bytes, error %v", len(got.Content), got.IsError)
	}
}

func TestTheModelCantLoadAUserOnlySkill(t *testing.T) {
	// given
	// ... a model calling the skill tool for a skill only the user may load
	skills := skillLibrary()
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{skillCall("toolu_1", "deploy", "")}, Content: raw("A1")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A2")}},
	}}
	a := newAgent(model, &mockTranscript{})
	a.Skills = skills

	// when
	// ... a turn runs
	a.RunTurn("ship")

	// then
	// ... it gets an error and nothing renders
	want := ToolResult{ToolUseID: "toolu_1", Content: `unknown skill "deploy"`, IsError: true}
	if got := lastToolResult(model.requests, 1); got != want {
		t.Errorf("result = %+v, want %+v", got, want)
	}
	if len(skills.renders) != 0 {
		t.Errorf("rendered %v", skills.renders)
	}
}

func TestARenderErrorGoesBackToTheModel(t *testing.T) {
	// given
	// ... a skill whose injected command fails
	skills := skillLibrary()
	skills.render = func(string, string) (string, error) { return "", errors.New("shell command failed") }
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{skillCall("toolu_1", "review", "")}, Content: raw("A1")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A2")}},
	}}
	a := newAgent(model, &mockTranscript{})
	a.Skills = skills

	// when
	// ... a turn runs
	a.RunTurn("review")

	// then
	// ... the model gets the error
	want := ToolResult{ToolUseID: "toolu_1", Content: "shell command failed", IsError: true}
	if got := lastToolResult(model.requests, 1); got != want {
		t.Errorf("result = %+v, want %+v", got, want)
	}
}

func TestLoadingTheSameContentAgainGivesANoteInstead(t *testing.T) {
	// given
	// ... a model that calls a tool and loads a skill twice in one response, then again with other arguments
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{toolCall("toolu_0", "list", `{}`), skillCall("toolu_1", "review", "x"), skillCall("toolu_2", "review", "x")}, Content: raw("A1")}},
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{skillCall("toolu_3", "review", "x"), skillCall("toolu_4", "review", "y")}, Content: raw("A2")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A3")}},
	}}
	skills := skillLibrary()
	var renderedBeforeTool []string
	list := &mockTool{name: "list", run: func(context.Context) (string, error) {
		renderedBeforeTool = slices.Clone(skills.renders)
		return "a.txt", nil
	}}
	a := newAgent(model, &mockTranscript{}, list)
	a.Skills = skills

	// when
	// ... a turn runs
	a.RunTurn("review")

	// then
	// ... the skills ran before the tool, and only the first copy and the changed one are sent in full
	if !slices.Equal(renderedBeforeTool, []string{"review|x", "review|x"}) {
		t.Errorf("rendered before the tool ran: %v", renderedBeforeTool)
	}
	results := append(model.requests[1][len(model.requests[1])-1].ToolResults, model.requests[2][len(model.requests[2])-1].ToolResults...)
	if results[0] != (ToolResult{ToolUseID: "toolu_0", Content: "a.txt"}) {
		t.Errorf("tool result = %+v", results[0])
	}
	full := `<skill_content name="review">`
	for i, wantFull := range []bool{true, false, false, true} {
		if got := strings.HasPrefix(results[i+1].Content, full); got != wantFull {
			t.Errorf("skill result %d = %q, want full: %v", i+1, results[i+1].Content, wantFull)
		}
	}
	if !strings.Contains(results[2].Content, "already loaded") {
		t.Errorf("note = %q", results[2].Content)
	}
}

func TestADiscardedTurnTakesItsSkillsWithIt(t *testing.T) {
	// given
	// ... a turn that loads a skill and then fails
	model := &mockModel{replies: []mockReply{
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{skillCall("toolu_1", "review", "")}, Content: raw("A1")}},
		{err: errors.New("HTTP 500")},
		{resp: ModelResponse{StopReason: StopToolUse, ToolCalls: []ToolCall{skillCall("toolu_2", "review", "")}, Content: raw("A2")}},
		{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A3")}},
	}}
	a := newAgent(model, &mockTranscript{})
	a.Skills = skillLibrary()
	a.RunTurn("review")

	// when
	// ... the next turn loads it again
	a.RunTurn("review again")

	// then
	// ... it gets the full content, not a note
	if got := lastToolResult(model.requests, 3).Content; got != `<skill_content name="review"></skill_content>` {
		t.Errorf("result = %q", got)
	}
}

func TestTheUserLoadsASkillWithASlashCommand(t *testing.T) {
	// given
	// ... a user-only skill and a model that answers
	skills := skillLibrary()
	model := &mockModel{replies: []mockReply{{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A1")}}}}
	transcript := &mockTranscript{}
	a := newAgent(model, transcript)
	a.Skills = skills

	// when
	// ... the user types /deploy with arguments
	a.RunTurn("/deploy  prod \"eu west\" ")

	// then
	// ... the rendered skill is the user's message and the transcript shows it loaded
	content := `<skill_content name="deploy">prod "eu west"</skill_content>`
	if got := model.requests[0][0]; got.Role != RoleUser || got.Text != content {
		t.Errorf("message = %+v", got)
	}
	if transcript.events[0] != (SkillLoaded{Name: "deploy", Content: content}) {
		t.Errorf("event = %#v", transcript.events[0])
	}
}

func TestSlashTextThatIsntAUserSkillIsAnOrdinaryPrompt(t *testing.T) {
	for _, prompt := range []string{"/lore", "/nope do it", "/Users/me/file.go is broken"} {
		t.Run(prompt, func(t *testing.T) {
			// given
			// ... a model that answers
			skills := skillLibrary()
			model := &mockModel{replies: []mockReply{{resp: ModelResponse{StopReason: StopEndTurn, Content: raw("A1")}}}}
			a := newAgent(model, &mockTranscript{})
			a.Skills = skills

			// when
			// ... the user sends a model-only skill, an unknown name or a path
			a.RunTurn(prompt)

			// then
			// ... it goes to the model as typed
			if got := model.requests[0][0].Text; got != prompt || len(skills.renders) != 0 {
				t.Errorf("text = %q, renders = %v", got, skills.renders)
			}
		})
	}
}

func TestAFailedSlashCommandDiscardsTheTurnWithoutCallingTheModel(t *testing.T) {
	// given
	// ... a skill that fails to render
	skills := skillLibrary()
	skills.render = func(string, string) (string, error) { return "", errors.New("exit code 2") }
	model := &mockModel{}
	transcript := &mockTranscript{}
	a := newAgent(model, transcript)
	a.Skills = skills

	// when
	// ... the user invokes it
	a.RunTurn("/review")

	// then
	// ... the turn is discarded with the reason and the model is never called
	if last := transcript.last(); last != (TurnDiscarded{Reason: "skill review: exit code 2"}) {
		t.Errorf("last event = %#v", last)
	}
	if len(model.requests) != 0 || len(a.messages) != 0 {
		t.Errorf("model calls = %d, messages = %d", len(model.requests), len(a.messages))
	}
}

func TestAToolNamedLikeTheSkillToolPanics(t *testing.T) {
	// given
	// ... an agent with skills and a tool named like the skill tool
	a := newAgent(&mockModel{}, &mockTranscript{}, &mockTool{name: SkillToolName})
	a.Skills = skillLibrary()

	// when / then
	// ... wiring panics
	defer func() {
		if recover() == nil {
			t.Error("no panic")
		}
	}()
	a.RunTurn("hi")
}
