package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"grimalkin/internal/core"
)

type shownEntry struct {
	kind entryKind
	text string
}

func shownEntries(s *Store) []shownEntry {
	var out []shownEntry
	for _, e := range s.entries {
		out = append(out, shownEntry{e.kind, e.text.String()})
	}
	return out
}

func TestTurnEventsBecomeEntries(t *testing.T) {
	// given
	// ... a turn that streams a reply, runs a tool with long output, fails a tool, loads a skill, replies again and is discarded
	var longOutput []string
	for i := range 25 {
		longOutput = append(longOutput, fmt.Sprint(i))
	}
	events := []core.Event{
		core.ReplyStarted{}, core.ReplyText{Text: "Look"}, core.ReplyText{Text: "ing."},
		core.ToolStarted{ID: "1", Name: "shell", Summary: "seq 25"},
		core.ToolFinished{ID: "1", Output: strings.Join(longOutput, "\n")},
		core.ToolStarted{ID: "2", Name: "shell", Summary: "nope"},
		core.ToolFinished{ID: "2", Output: "failed to run command: boom", IsError: true},
		core.SkillLoaded{Name: "review", Content: "<skill_content name=\"review\">\nbody\n</skill_content>"},
		core.ReplyStarted{}, core.ReplyText{Text: "Again"},
		core.TurnDiscarded{Reason: "cancelled"},
	}
	store := NewStore()

	// when
	// ... the events are shown
	for _, event := range events {
		store.Show(event)
	}

	// then
	// ... each reply gets its own entry, output is cut to 20 lines, and errors are marked
	want := []shownEntry{
		{entryReply, "Looking."},
		{entryCommand, "seq 25"},
		{entryOutput, strings.Join(longOutput[:20], "\n") + "\n… 5 more lines"},
		{entryCommand, "nope"},
		{entryError, "failed to run command: boom"},
		{entryCommand, "skill review"},
		{entryOutput, "<skill_content name=\"review\">\nbody\n</skill_content>"},
		{entryReply, "Again"},
		{entryError, "cancelled (turn discarded)"},
	}
	if got := shownEntries(store); !slices.Equal(got, want) {
		t.Errorf("entries = %+v\nwant %+v", got, want)
	}
}

func TestADividerSeparatesEachTurnFromTheLast(t *testing.T) {
	// given
	// ... startup info and two turns
	store := NewStore()
	store.AddInfo("model x")
	store.addUser("one")
	store.Show(core.ReplyStarted{})
	store.Show(core.ReplyText{Text: "first"})
	store.addUser("two")
	store.Show(core.ReplyStarted{})
	store.Show(core.ReplyText{Text: "second"})

	// when
	// ... the transcript is rendered 10 columns wide
	lines := store.render(10)

	// then
	// ... a short centred hairline sits before the second turn only, and is left out of yanks
	var texts []string
	for _, line := range lines {
		texts = append(texts, lineText(line))
	}
	divider := "   ───"
	want := []string{"model x", "", "› one", "", "first", "", divider, "", "› two", "", "second", ""}
	if !slices.Equal(texts, want) {
		t.Fatalf("lines %q\nwant %q", texts, want)
	}
	if lines[6].prefixLen != len(lines[6].cells) || lines[6].startsEntry {
		t.Errorf("divider %+v", lines[6])
	}
}

func TestToolOutputHangsUnderItsCommandBehindAGutter(t *testing.T) {
	// given
	// ... a shell call and its output
	store := NewStore()
	store.Show(core.ToolStarted{ID: "1", Name: "shell", Summary: "ls"})
	store.Show(core.ToolFinished{ID: "1", Output: "a\nb"})

	// when
	// ... the transcript is rendered
	lines := store.render(20)

	// then
	// ... the output follows the command directly, unshaded, and a yank of it drops the gutter
	var texts []string
	for _, line := range lines {
		texts = append(texts, lineText(line))
		if line.fill != 0 {
			t.Errorf("line %q is shaded", lineText(line))
		}
	}
	want := []string{"$ ls", "│ a", "│ b", ""}
	if !slices.Equal(texts, want) {
		t.Fatalf("lines %q\nwant %q", texts, want)
	}
	if got := string(yankText(lines, position{1, 0}, position{2, endOfLine}, true)); got != "a\nb" {
		t.Errorf("yanked %q", got)
	}
}
