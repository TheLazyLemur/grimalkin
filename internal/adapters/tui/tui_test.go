package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"grimalkin/internal/core"
)

type fakeAgent struct {
	prompts   []string
	cancelled int
}

func (f *fakeAgent) RunTurn(prompt string) { f.prompts = append(f.prompts, prompt) }
func (f *fakeAgent) Cancel()               { f.cancelled++ }

type nopClipboard struct{}

func (nopClipboard) Copy(string) {}

func typeText(m *model, text string) {
	for _, char := range text {
		m.Update(tea.KeyPressMsg{Code: char, Text: string(char)})
	}
}

func TestCtrlCCancelsARunningTurnAndQuitsOnceItHasFinished(t *testing.T) {
	// given
	// ... a prompt sent to the agent
	agent := &fakeAgent{}
	m := newModel(agent, NewStore(), nopClipboard{}, "fake")
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	typeText(m, "hi")
	_, runTurn := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	// when
	// ... Ctrl-C is pressed during the turn, the turn finishes, and Ctrl-C is pressed again
	_, duringTurn := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	m.Update(runTurn())
	_, afterTurn := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})

	// then
	// ... the turn ran with the prompt, the first Ctrl-C cancelled it without quitting, and the second quit
	if len(agent.prompts) != 1 || agent.prompts[0] != "hi" {
		t.Errorf("prompts = %q", agent.prompts)
	}
	if agent.cancelled != 1 || duringTurn != nil {
		t.Errorf("cancelled %d times, command during turn %v", agent.cancelled, duringTurn)
	}
	if afterTurn == nil {
		t.Fatal("no command after the turn")
	}
	if _, quit := afterTurn().(tea.QuitMsg); !quit {
		t.Errorf("after the turn Ctrl-C gave %T, want tea.QuitMsg", afterTurn())
	}
}

func screenRows(m *model) []string {
	return strings.Split(ansi.Strip(m.View().Content), "\n")
}

func TestContentSitsInsideMarginsWithTheCursorOnIt(t *testing.T) {
	// given
	// ... a wide, tall terminal with a reply and some typed input
	store := NewStore()
	m := newModel(&fakeAgent{}, store, nopClipboard{}, "fake")
	store.AddInfo("hello")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	typeText(m, "hi")

	// when
	// ... the screen is drawn
	view := m.View()
	rows := screenRows(m)

	// then
	// ... a blank row tops the screen, every row fits and starts with the margin, and the cursor follows the input
	if len(rows) != 20 || strings.TrimSpace(rows[0]) != "" || rows[1] != "  hello" {
		t.Fatalf("rows %q", rows)
	}
	for _, row := range rows {
		if ansi.StringWidth(row) > 80 || (row != "" && !strings.HasPrefix(row, "  ")) {
			t.Errorf("row %q breaks the margins", row)
		}
	}
	last := rows[len(rows)-1]
	if last != "  › hi" || view.Cursor.X != 6 || view.Cursor.Y != 19 {
		t.Errorf("input %q, cursor %+v", last, view.Cursor.Position)
	}
}

func TestNarrowTerminalsDropTheMarginsAndKeepTheTurnState(t *testing.T) {
	// given
	// ... a narrow terminal with a turn running
	m := newModel(&fakeAgent{}, NewStore(), nopClipboard{}, "a-long-model-name")
	m.Update(tea.WindowSizeMsg{Width: 24, Height: 8})
	typeText(m, "hi")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	// when
	// ... the screen is drawn
	rows := screenRows(m)

	// then
	// ... nothing overflows, there's no top padding, and the status line keeps the mode and the state over the model
	for _, row := range rows {
		if ansi.StringWidth(row) > 24 {
			t.Errorf("row %q overflows", row)
		}
	}
	status := rows[len(rows)-2]
	if !strings.HasPrefix(status, "insert") || !strings.Contains(status, "working") || strings.Contains(status, "model") {
		t.Errorf("rows %q", rows)
	}
}

func TestStatusKeepsModeAndActivityAheadOfLongNotesAndModelNames(t *testing.T) {
	modes := []struct {
		name   string
		focus  focus
		editor editMode
		visual visualMode
	}{
		{"insert", focusInput, modeInsert, visualOff},
		{"normal", focusInput, modeNormal, visualOff},
		{"view", focusTranscript, modeNormal, visualOff},
		{"visual", focusTranscript, modeNormal, visualChar},
		{"v-line", focusTranscript, modeNormal, visualLine},
	}
	for _, width := range []int{10, 12, 24, 30, 59, 60, 80, 120} {
		for _, mode := range modes {
			for _, busy := range []bool{false, true} {
				m := newModel(&fakeAgent{}, NewStore(), nopClipboard{}, strings.Repeat("長いモデル", 10))
				m.width, m.height = width, 20
				m.focus, m.editor.mode, m.transcript.visual = mode.focus, mode.editor, mode.visual
				m.statusNote = "pattern not found: " + strings.Repeat("漢字", 30)
				m.busy = busy
				status := strings.TrimSpace(ansi.Strip(m.renderStatusBar()))
				if !strings.HasPrefix(status, mode.name+" ") || ansi.StringWidth(status) > m.contentWidth() {
					t.Errorf("width %d, mode %s, busy %v: %q", width, mode.name, busy, status)
				}
				if busy {
					if !strings.Contains(status, "working") && !strings.HasSuffix(status, "run") {
						t.Errorf("width %d: activity hidden in %q", width, status)
					}
					if m.contentWidth() >= 24 && !strings.Contains(status, "ctrl-c") {
						t.Errorf("width %d: cancellation hint hidden in %q", width, status)
					}
				} else if !strings.HasSuffix(status, "ready") && !strings.HasSuffix(status, "ok") {
					t.Errorf("width %d: idle state hidden in %q", width, status)
				}
			}
		}
	}
}

func TestResizeKeepsWrappedInputAndCursorInsideTheContent(t *testing.T) {
	m := newModel(&fakeAgent{}, NewStore(), nopClipboard{}, "fake")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	typeText(m, strings.Repeat("漢字 abc ", 12)+"\nnext")
	for _, size := range []struct{ width, height int }{
		{80, 24}, {60, 16}, {59, 15}, {30, 8}, {24, 8}, {10, 4}, {120, 40},
	} {
		m.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
		view, rows := m.View(), screenRows(m)
		if len(rows) != size.height {
			t.Errorf("%+v: got %d rows", size, len(rows))
		}
		for _, row := range rows {
			if ansi.StringWidth(row) > size.width-m.margin() {
				t.Errorf("%+v: overflowing row %q", size, row)
			}
		}
		cursor := view.Cursor
		if cursor.X < m.margin() || cursor.X >= size.width-m.margin() || cursor.Y < 0 || cursor.Y >= size.height {
			t.Errorf("%+v: cursor outside content: %+v", size, cursor.Position)
		}
		if cursor.X != ansi.StringWidth(rows[cursor.Y]) {
			t.Errorf("%+v: cursor %+v not at the end of input %q", size, cursor.Position, rows[cursor.Y])
		}
	}
}

func TestTranscriptCursorSearchAndSelectionRespectMargins(t *testing.T) {
	store := NewStore()
	store.AddInfo("漢字 hello")
	m := newModel(&fakeAgent{}, store, nopClipboard{}, "fake")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.focusTranscript()
	m.transcript.cursor = position{0, 3} // h, after two double-width characters and a space
	m.layout()
	view := m.View()
	if view.Cursor.X != 7 || view.Cursor.Y != 1 {
		t.Fatalf("transcript cursor %+v", view.Cursor.Position)
	}

	m.Update(tea.KeyPressMsg{Code: 'V', Text: "V"})
	m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	if got := string(m.register); got != "漢字 hello" {
		t.Errorf("yank contains presentation padding: %q", got)
	}

	m.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	typeText(m, "hello")
	view = m.View()
	if view.Cursor.X != 8 || view.Cursor.Y != 23 || screenRows(m)[23] != "  /hello" {
		t.Errorf("search cursor %+v, rows %q", view.Cursor.Position, screenRows(m))
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.transcript.cursor != (position{0, 3}) {
		t.Errorf("search landed at %+v", m.transcript.cursor)
	}
	if got := lineText(m.lines[0]); got != "漢字 hello" {
		t.Errorf("rendering altered searchable content: %q", got)
	}
}

func TestLongSearchKeepsItsTailAndCursorVisible(t *testing.T) {
	m := newModel(&fakeAgent{}, NewStore(), nopClipboard{}, "fake")
	m.Update(tea.WindowSizeMsg{Width: 30, Height: 16})
	m.focusTranscript()
	m.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
	query := strings.Repeat("漢字", 20) + "needle"
	typeText(m, query)
	view := m.View()
	row := screenRows(m)[15]
	if !strings.HasPrefix(row, " ?") || !strings.HasSuffix(row, "needle") || ansi.StringWidth(row) >= 29 {
		t.Errorf("search row %q", row)
	}
	if view.Cursor.X != ansi.StringWidth(row) || view.Cursor.Y != 15 {
		t.Errorf("cursor %+v, row %q", view.Cursor.Position, row)
	}
	if string(m.transcript.search.typed) != query {
		t.Errorf("search scrolling changed the query")
	}
}

func pressKeys(m *model, keys string) {
	for _, char := range keys {
		m.Update(tea.KeyPressMsg{Code: char, Text: string(char)})
	}
}

func foldModel(t *testing.T) *model {
	t.Helper()
	store := NewStore()
	store.Show(core.ToolStarted{ID: "1", Name: "shell", Summary: "seq 10"})
	store.Show(core.ToolFinished{ID: "1", Output: "1\n2\n3\n4\n5\n6\n7\n8\n9\n10"})
	store.Show(core.ToolStarted{ID: "2", Name: "shell", Summary: "seq 6"})
	store.Show(core.ToolFinished{ID: "2", Output: "1\n2\n3\n4\n5\n6"})
	m := newModel(&fakeAgent{}, store, nopClipboard{}, "fake")
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 40})
	m.focusTranscript()
	return m
}

func TestZaOnACommandTogglesItsOutputAndKeepsTheCursorOnIt(t *testing.T) {
	// given
	// ... two collapsed outputs, the cursor on the first command
	m := foldModel(t)
	m.transcript.jumpTo(m.lines, m.transcriptHeight, position{0, 0})

	// when
	// ... za is pressed, then za again
	pressKeys(m, "za")
	expanded := renderedTexts(m.store, m.contentWidth())
	cursorAfterExpand := m.transcript.cursor
	pressKeys(m, "za")

	// then
	// ... only the first block expands, the cursor stays on its command, and it collapses back
	if len(expanded) != 12+7 || expanded[10] != "│ 10" || expanded[13] != "  ··· 2 lines above" {
		t.Errorf("expanded lines %q", expanded)
	}
	if cursorAfterExpand != (position{0, 0}) || m.transcript.cursor != (position{0, 0}) {
		t.Errorf("cursor after expand %+v, after collapse %+v", cursorAfterExpand, m.transcript.cursor)
	}
	if len(m.lines) != 7+7 {
		t.Errorf("collapsed lines %q", renderedTexts(m.store, m.contentWidth()))
	}
	if m.focus != focusTranscript {
		t.Errorf("za's a left VIEW")
	}
}

func TestZaInsideOutputKeepsTheCursorOnTheSameBlock(t *testing.T) {
	// given
	// ... the cursor on the second block's last visible output line
	m := foldModel(t)
	m.transcript.jumpTo(m.lines, m.transcriptHeight, position{12, 0})
	if lineText(m.lines[12]) != "  6" {
		t.Fatalf("setup lines %q", renderedTexts(m.store, m.contentWidth()))
	}

	// when
	// ... za is pressed
	pressKeys(m, "za")

	// then
	// ... the second block expands and the cursor is still inside it
	line := m.lines[m.transcript.cursor.line]
	if line.owner == nil || line.owner.kind != entryOutput || !line.owner.expanded || !strings.HasPrefix(lineText(line), "│") {
		t.Errorf("cursor on %q", lineText(line))
	}
	if m.lines[0].fold.expanded {
		t.Errorf("first block expanded too")
	}
}

func TestZRAndZMExpandAndCollapseEverything(t *testing.T) {
	// given
	// ... two collapsed outputs
	m := foldModel(t)

	// when
	// ... zR, then zM
	pressKeys(m, "zR")
	expanded := renderedTexts(m.store, m.contentWidth())
	pressKeys(m, "zM")
	collapsed := renderedTexts(m.store, m.contentWidth())

	// then
	// ... all output shows behind gutters, then all is folded again
	if len(expanded) != 12+8 || strings.Contains(strings.Join(expanded, "\n"), "···") {
		t.Errorf("expanded %q", expanded)
	}
	if want := "  ··· 2 lines above"; collapsed[8] != want {
		t.Errorf("collapsed %q", collapsed)
	}
}

func TestZFollowedByAnotherKeyDoesNothing(t *testing.T) {
	// given
	// ... the transcript focused
	m := foldModel(t)
	before := m.transcript.cursor

	// when
	// ... z then j
	pressKeys(m, "zj")

	// then
	// ... j was swallowed by the pending z, nothing folded or moved
	if m.transcript.cursor != before || m.lines[0].fold.expanded {
		t.Errorf("cursor %+v", m.transcript.cursor)
	}
}
