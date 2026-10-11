package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// SkillToolName is the tool the model loads skills with.
const SkillToolName = "skill"

const skillToolDescription = "Load a skill's full instructions. Call it when a task matches one of the skills in <available_skills>, before starting the task, then follow what it returns. Don't call it for a skill the user already loaded with /name."

// skillCatalog is the system prompt section listing the skills the model may
// load. Empty when there are none, so neither the section nor the tool exists.
func skillCatalog(skills []Skill) string {
	var b strings.Builder
	for _, s := range skills {
		if !s.ModelInvocable {
			continue
		}
		fmt.Fprintf(&b, "<skill>\n<name>%s</name>\n<description>%s</description>\n</skill>\n",
			escapeXML(s.Name), escapeXML(s.Description))
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n# Skills\n\nSkills give you instructions for specific tasks. When a task matches a skill's description, call the skill tool with its name to load its instructions before you start. A user message that is a <skill_content> block is a skill the user loaded with /name: follow it.\n\n<available_skills>\n" +
		b.String() + "</available_skills>\n"
}

var xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func escapeXML(s string) string { return xmlEscaper.Replace(s) }

func (a *Agent) modelSkillNames() []string {
	if a.Skills == nil {
		return nil
	}
	var names []string
	for _, s := range a.Skills.List() {
		if s.ModelInvocable {
			names = append(names, s.Name)
		}
	}
	return names
}

func skillToolSpec(names []string) ToolSpec {
	enum, err := json.Marshal(names)
	if err != nil {
		panic(err)
	}
	return ToolSpec{
		Name:        SkillToolName,
		Description: skillToolDescription,
		InputSchema: `{"type":"object","properties":{` +
			`"name":{"type":"string","enum":` + string(enum) + `,"description":"The skill to load."},` +
			`"arguments":{"type":"string","description":"Arguments for the skill, written as the user would type them after /name. Omit if the skill takes none."}` +
			`},"required":["name"],"additionalProperties":false}`,
	}
}

type skillInput struct {
	Name      *string
	Arguments string
}

func parseSkillInput(input json.RawMessage) (skillInput, error) {
	var in skillInput
	if json.Unmarshal(input, &in) != nil || in.Name == nil {
		return skillInput{}, errors.New(`invalid input: expected {"name": string, "arguments"?: string}`)
	}
	return in, nil
}

func skillSummary(input json.RawMessage) string {
	in, err := parseSkillInput(input)
	if err != nil {
		return SkillToolName + " " + string(input)
	}
	return strings.TrimSpace(SkillToolName + " " + *in.Name + " " + in.Arguments)
}

// runSkillTool loads a skill for the model. pending holds the results of
// this response's earlier skill calls, not yet in the history.
func (a *Agent) runSkillTool(ctx context.Context, input json.RawMessage, pending []ToolResult) (string, error) {
	in, err := parseSkillInput(input)
	if err != nil {
		return "", err
	}
	if !slices.Contains(a.modelSkillNames(), *in.Name) {
		return "", fmt.Errorf("unknown skill %q", *in.Name)
	}
	return a.loadSkill(ctx, *in.Name, in.Arguments, pending)
}

// userSkill reports whether prompt invokes a user-invocable skill as
// "/name arguments". Anything else, such as a path, is an ordinary prompt.
func (a *Agent) userSkill(prompt string) (name, arguments string, ok bool) {
	rest, isCommand := strings.CutPrefix(prompt, "/")
	if !isCommand || a.Skills == nil {
		return "", "", false
	}
	end := strings.IndexFunc(rest, unicode.IsSpace)
	if end < 0 {
		end = len(rest)
	}
	name = rest[:end]
	for _, s := range a.Skills.List() {
		if s.Name == name && s.UserInvocable {
			return name, strings.TrimSpace(rest[end:]), true
		}
	}
	return "", "", false
}

// loadSkill renders a skill, or returns a short note instead when the same
// rendered content is already in the conversation. The history is the
// record, so a discarded turn takes its skills with it.
func (a *Agent) loadSkill(ctx context.Context, name, arguments string, pending []ToolResult) (string, error) {
	content, err := a.Skills.Render(ctx, name, arguments)
	if err != nil {
		return "", err
	}
	if a.inContext(content, pending) {
		return fmt.Sprintf("Skill %q is already loaded above with the same content. Follow those instructions.", name), nil
	}
	return content, nil
}

func (a *Agent) inContext(content string, pending []ToolResult) bool {
	isContent := func(r ToolResult) bool { return !r.IsError && r.Content == content }
	if slices.ContainsFunc(pending, isContent) {
		return true
	}
	for _, m := range a.messages {
		if m.Role != RoleUser {
			continue
		}
		if m.Text == content || slices.ContainsFunc(m.ToolResults, isContent) {
			return true
		}
	}
	return false
}
