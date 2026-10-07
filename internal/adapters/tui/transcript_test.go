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
	// ... each reply gets its own entry, output is kept whole for folding, and errors are marked
	want := []shownEntry{
		{entryReply, "Looking."},
		{entryCommand, "seq 25"},
		{entryOutput, strings.Join(longOutput, "\n")},
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

func TestExpandedToolOutputHangsUnderItsCommandBehindAGutter(t *testing.T) {
	// given
	// ... a shell call and its output, expanded
	store := NewStore()
	store.Show(core.ToolStarted{ID: "1", Name: "shell", Summary: "ls"})
	store.Show(core.ToolFinished{ID: "1", Output: "a\nb\n"})
	store.setAllFolds(true)

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

func renderedTexts(store *Store, width int) []string {
	var texts []string
	for _, line := range store.render(width) {
		texts = append(texts, lineText(line))
	}
	return texts
}

func TestCollapsedOutputShowsItsLastLinesUnderACountOfTheRest(t *testing.T) {
	// given
	// ... a command with ten lines of output, and one with two
	store := NewStore()
	store.Show(core.ToolStarted{ID: "1", Name: "shell", Summary: "seq 10"})
	store.Show(core.ToolFinished{ID: "1", Output: "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n"})
	store.Show(core.ToolStarted{ID: "2", Name: "shell", Summary: "ls"})
	store.Show(core.ToolFinished{ID: "2", Output: "a\nb"})

	// when
	// ... the transcript is rendered
	lines := store.render(30)

	// then
	// ... the long one shows its last four lines under a count, the short one shows whole, both dimmed and indented
	var texts []string
	for _, line := range lines {
		texts = append(texts, lineText(line))
	}
	want := []string{"$ seq 10", "  ··· 6 lines above", "  7", "  8", "  9", "  10", "", "$ ls", "  a", "  b", ""}
	if !slices.Equal(texts, want) {
		t.Fatalf("lines %q\nwant %q", texts, want)
	}
	if lines[2].cells[2].fg != colorFolded {
		t.Errorf("collapsed output not dimmed: %+v", lines[2].cells)
	}
	if got := string(yankText(lines, position{1, 0}, position{5, endOfLine}, true)); got != "7\n8\n9\n10" {
		t.Errorf("yank took the marker or indent: %q", got)
	}
}

func TestCollapsedSkillContentShowsOnlyItsSize(t *testing.T) {
	// given
	// ... a loaded skill
	store := NewStore()
	store.Show(core.SkillLoaded{Name: "review", Content: "<skill_content>\nbody\n</skill_content>"})

	// when
	// ... the transcript is rendered collapsed, then expanded
	collapsed := renderedTexts(store, 40)
	store.setAllFolds(true)
	expanded := renderedTexts(store, 40)

	// then
	// ... collapsed hides the content behind a count; expanded shows it all
	if want := []string{"$ skill review", "  ··· 3 lines", ""}; !slices.Equal(collapsed, want) {
		t.Errorf("collapsed %q\nwant %q", collapsed, want)
	}
	if want := []string{"$ skill review", "│ <skill_content>", "│ body", "│ </skill_content>", ""}; !slices.Equal(expanded, want) {
		t.Errorf("expanded %q\nwant %q", expanded, want)
	}
}

func TestErrorsAreNeverFolded(t *testing.T) {
	// given
	// ... a failing command with long output
	store := NewStore()
	store.Show(core.ToolStarted{ID: "1", Name: "shell", Summary: "make"})
	store.Show(core.ToolFinished{ID: "1", Output: "1\n2\n3\n4\n5\n6", IsError: true})

	// when
	// ... the transcript is rendered
	texts := renderedTexts(store, 30)

	// then
	// ... every line of the error shows
	if want := []string{"$ make", "1", "2", "3", "4", "5", "6", ""}; !slices.Equal(texts, want) {
		t.Errorf("lines %q\nwant %q", texts, want)
	}
}
