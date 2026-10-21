package tui

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"grimalkin/internal/core"
)

type fakeAgent struct {
	prompts   []string
	cancelled int
	// childPrompts is each turn started on a child, as "number: prompt".
	childPrompts   []string
	childCancelled []int
	// refuse, when set, is what StartChildTurn answers.
	refuse error
}

func (f *fakeAgent) StartTurn(prompt string) func() {
	return func() { f.prompts = append(f.prompts, prompt) }
}
func (f *fakeAgent) Cancel() { f.cancelled++ }
func (f *fakeAgent) CancelChild(number int) {
	f.childCancelled = append(f.childCancelled, number)
}

func (f *fakeAgent) StartChildTurn(number int, prompt string) (func(), error) {
	if f.refuse != nil {
		return nil, f.refuse
	}
	f.childPrompts = append(f.childPrompts, fmt.Sprintf("%d: %s", number, prompt))
	return func() {}, nil
}

// testLabels is the status bar's label for each model the tests' children
// run on.
var testLabels = map[string]string{
	"fake/child":       "child · effort n/a",
	"anthropic/sonnet": "sonnet · effort n/a",
	"sonnet":           "sonnet · effort n/a",
}

type nopClipboard struct{}

func (nopClipboard) Copy(string) {}

func typeText(m *model, text string) {
	for _, char := range text {
		m.handle(uv.KeyPressEvent{Code: char, Text: string(char)})
	}
}

func TestCtrlCCancelsARunningTurnAndQuitsOnceItHasFinished(t *testing.T) {
	// given
	// ... a prompt sent to the agent
	agent := &fakeAgent{}
	m := newModel(agent, NewStore(), nopClipboard{}, "fake", testLabels, nil)
	m.handle(uv.WindowSizeEvent{Width: 40, Height: 12})
	typeText(m, "hi")
	m.handle(uv.KeyPressEvent{Code: uv.KeyEnter})

	// when
	// ... Ctrl-C is pressed during the turn, the turn finishes, and Ctrl-C is pressed again
	m.handle(uv.KeyPressEvent{Code: 'c', Mod: uv.ModCtrl})
	quitDuringTurn := m.quit
	m.finishTurn(<-m.turnDone)
	m.handle(uv.KeyPressEvent{Code: 'c', Mod: uv.ModCtrl})

	// then
	// ... the turn ran with the prompt, the first Ctrl-C cancelled it without quitting, and the second quit
	if len(agent.prompts) != 1 || agent.prompts[0] != "hi" {
		t.Errorf("prompts = %q", agent.prompts)
	}
	if agent.cancelled != 1 || quitDuringTurn {
		t.Errorf("cancelled %d times, quit during turn %v", agent.cancelled, quitDuringTurn)
	}
	if !m.quit {
		t.Error("Ctrl-C after the turn did not quit")
	}
}

// drawn is the screen as rows of text, and where the cursor was put.
func drawn(m *model) (rows []string, cursor uv.Position) {
	buf := uv.NewScreenBuffer(m.width, m.height)
	cursor, _ = m.draw(buf)
	return strings.Split(buf.String(), "\n"), cursor
}

func screenRows(m *model) []string {
	rows, _ := drawn(m)
	return rows
}

// statusText is the status bar as it is drawn, margin included.
func statusText(m *model) string {
	buf := uv.NewScreenBuffer(m.width, 1)
	m.drawLine(buf, 0, m.statusBar())
	return buf.String()
}

func TestContentSitsInsideMarginsWithTheCursorOnIt(t *testing.T) {
	// given
	// ... a wide, tall terminal with a reply and some typed input
	store := NewStore()
	m := newModel(&fakeAgent{}, store, nopClipboard{}, "fake", testLabels, nil)
	store.AddInfo("hello")
	m.handle(uv.WindowSizeEvent{Width: 80, Height: 20})
	typeText(m, "hi")

	// when
	// ... the screen is drawn
	rows, cursor := drawn(m)

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
	if last != "  › hi" || cursor.X != 6 || cursor.Y != 19 {
		t.Errorf("input %q, cursor %+v", last, cursor)
	}
}

func TestNarrowTerminalsDropTheMarginsAndKeepTheTurnState(t *testing.T) {
	// given
	// ... a narrow terminal with a turn running
	m := newModel(&fakeAgent{}, NewStore(), nopClipboard{}, "a-long-model-name", testLabels, nil)
	m.handle(uv.WindowSizeEvent{Width: 24, Height: 8})
	typeText(m, "hi")
	m.handle(uv.KeyPressEvent{Code: uv.KeyEnter})

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
				m := newModel(&fakeAgent{}, NewStore(), nopClipboard{}, strings.Repeat("長いモデル", 10), testLabels, nil)
				m.width, m.height = width, 20
				m.focus, m.editor.mode, m.transcript.visual = mode.focus, mode.editor, mode.visual
				m.statusNote = "pattern not found: " + strings.Repeat("漢字", 30)
				m.busy = busy
				status := strings.TrimSpace(statusText(m))
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
	m := newModel(&fakeAgent{}, NewStore(), nopClipboard{}, "fake", testLabels, nil)
	m.handle(uv.WindowSizeEvent{Width: 80, Height: 24})
	typeText(m, strings.Repeat("漢字 abc ", 12)+"\nnext")
	for _, size := range []struct{ width, height int }{
		{80, 24}, {60, 16}, {59, 15}, {30, 8}, {24, 8}, {10, 4}, {120, 40},
	} {
		m.handle(uv.WindowSizeEvent{Width: size.width, Height: size.height})
		rows, cursor := drawn(m)
		if len(rows) != size.height {
			t.Errorf("%+v: got %d rows", size, len(rows))
		}
		for _, row := range rows {
			if ansi.StringWidth(row) > size.width-m.margin() {
				t.Errorf("%+v: overflowing row %q", size, row)
			}
		}
		if cursor.X < m.margin() || cursor.X >= size.width-m.margin() || cursor.Y < 0 || cursor.Y >= size.height {
			t.Errorf("%+v: cursor outside content: %+v", size, cursor)
		}
		if cursor.X != ansi.StringWidth(rows[cursor.Y]) {
			t.Errorf("%+v: cursor %+v not at the end of input %q", size, cursor, rows[cursor.Y])
		}
	}
}

func TestTranscriptCursorSearchAndSelectionRespectMargins(t *testing.T) {
	store := NewStore()
	store.AddInfo("漢字 hello")
	m := newModel(&fakeAgent{}, store, nopClipboard{}, "fake", testLabels, nil)
	m.handle(uv.WindowSizeEvent{Width: 80, Height: 24})
	m.focusTranscript()
	m.transcript.cursor = position{0, 3} // h, after two double-width characters and a space
	m.layout()
	_, cursor := drawn(m)
	if cursor.X != 7 || cursor.Y != 1 {
		t.Fatalf("transcript cursor %+v", cursor)
	}

	m.handle(uv.KeyPressEvent{Code: 'V', Text: "V"})
	m.handle(uv.KeyPressEvent{Code: 'y', Text: "y"})
	if got := string(m.register); got != "漢字 hello" {
		t.Errorf("yank contains presentation padding: %q", got)
	}

	m.handle(uv.KeyPressEvent{Code: '/', Text: "/"})
	typeText(m, "hello")
	rows, cursor := drawn(m)
	if cursor.X != 8 || cursor.Y != 23 || rows[23] != "  /hello" {
		t.Errorf("search cursor %+v, rows %q", cursor, rows)
	}
	m.handle(uv.KeyPressEvent{Code: uv.KeyEnter})
	if m.transcript.cursor != (position{0, 3}) {
		t.Errorf("search landed at %+v", m.transcript.cursor)
	}
	if got := lineText(m.lines[0]); got != "漢字 hello" {
		t.Errorf("rendering altered searchable content: %q", got)
	}
}

func TestLongSearchKeepsItsTailAndCursorVisible(t *testing.T) {
	m := newModel(&fakeAgent{}, NewStore(), nopClipboard{}, "fake", testLabels, nil)
	m.handle(uv.WindowSizeEvent{Width: 30, Height: 16})
	m.focusTranscript()
	m.handle(uv.KeyPressEvent{Code: '?', Text: "?"})
	query := strings.Repeat("漢字", 20) + "needle"
	typeText(m, query)
	rows, cursor := drawn(m)
	row := rows[15]
	if !strings.HasPrefix(row, " ?") || !strings.HasSuffix(row, "needle") || ansi.StringWidth(row) >= 29 {
		t.Errorf("search row %q", row)
	}
	if cursor.X != ansi.StringWidth(row) || cursor.Y != 15 {
		t.Errorf("cursor %+v, row %q", cursor, row)
	}
	if string(m.transcript.search.typed) != query {
		t.Errorf("search scrolling changed the query")
	}
}

func pressKeys(m *model, keys string) {
	for _, char := range keys {
		m.handle(uv.KeyPressEvent{Code: char, Text: string(char)})
	}
}

func foldModel(t *testing.T) *model {
	t.Helper()
	store := NewStore()
	store.Show(0, core.ToolStarted{ID: "1", Name: "shell", Summary: "seq 10"})
	store.Show(0, core.ToolFinished{ID: "1", Output: "1\n2\n3\n4\n5\n6\n7\n8\n9\n10"})
	store.Show(0, core.ToolStarted{ID: "2", Name: "shell", Summary: "seq 6"})
	store.Show(0, core.ToolFinished{ID: "2", Output: "1\n2\n3\n4\n5\n6"})
	m := newModel(&fakeAgent{}, store, nopClipboard{}, "fake", testLabels, nil)
	m.handle(uv.WindowSizeEvent{Width: 60, Height: 40})
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

func TestCollapsingFromDeepInsideOutputLandsOnItsCommandInView(t *testing.T) {
	// given
	// ... a 200-line output expanded, a long reply after it, the cursor 150 lines into the output
	store := NewStore()
	var out []string
	for i := range 200 {
		out = append(out, fmt.Sprint(i))
	}
	store.Show(0, core.ToolStarted{ID: "1", Name: "shell", Summary: "seq 200"})
	store.Show(0, core.ToolFinished{ID: "1", Output: strings.Join(out, "\n")})
	store.Show(0, core.ReplyStarted{})
	store.Show(0, core.ReplyText{Text: strings.Repeat("x\n", 60)})
	m := newModel(&fakeAgent{}, store, nopClipboard{}, "fake", testLabels, nil)
	m.handle(uv.WindowSizeEvent{Width: 60, Height: 40})
	m.focusTranscript()
	pressKeys(m, "zR")
	m.transcript.jumpTo(m.lines, m.transcriptHeight, position{151, 0})

	// when
	// ... za folds it
	pressKeys(m, "za")

	// then
	// ... the cursor is on the command and the folded block is on screen
	if m.transcript.cursor.line != 0 || lineText(m.lines[0]) != "$ seq 200" {
		t.Errorf("cursor %+v", m.transcript.cursor)
	}
	if m.transcript.scrollTop != 0 {
		t.Errorf("scrollTop %d", m.transcript.scrollTop)
	}
}

func TestFoldingAllKeepsACursorOnADividerOnThatDivider(t *testing.T) {
	// given
	// ... a long collapsed output in turn one, the cursor on the divider before turn two
	store := NewStore()
	var out []string
	for i := range 100 {
		out = append(out, fmt.Sprint(i))
	}
	store.addUser(0, "one")
	store.Show(0, core.ToolStarted{ID: "1", Name: "shell", Summary: "seq 100"})
	store.Show(0, core.ToolFinished{ID: "1", Output: strings.Join(out, "\n")})
	store.addUser(0, "two")
	m := newModel(&fakeAgent{}, store, nopClipboard{}, "fake", testLabels, nil)
	m.handle(uv.WindowSizeEvent{Width: 60, Height: 40})
	m.focusTranscript()
	divider := slices.IndexFunc(m.lines, func(l screenLine) bool {
		return l.decoration && strings.Contains(lineText(l), "──")
	})
	m.transcript.jumpTo(m.lines, m.transcriptHeight, position{divider, 0})

	// when
	// ... zR, then zM
	pressKeys(m, "zR")
	afterExpand := m.lines[m.transcript.cursor.line]
	pressKeys(m, "zM")
	afterCollapse := m.lines[m.transcript.cursor.line]

	// then
	// ... the cursor is on the divider both times
	if !strings.Contains(lineText(afterExpand), "──") || !strings.Contains(lineText(afterCollapse), "──") {
		t.Errorf("cursor on %q then %q", lineText(afterExpand), lineText(afterCollapse))
	}
}

func TestZAfterAnotherPendingKeyCancelsLikeBefore(t *testing.T) {
	for _, keys := range []string{"yza", "gza"} {
		// given
		// ... collapsed output with the cursor on its command
		m := foldModel(t)
		m.transcript.jumpTo(m.lines, m.transcriptHeight, position{0, 0})

		// when
		// ... y or g, then z, then a
		pressKeys(m, keys)

		// then
		// ... nothing folds and a enters insert, as it did before folds
		if m.lines[0].fold.expanded || m.focus != focusInput {
			t.Errorf("%s: expanded %v, focus %v", keys, m.lines[0].fold.expanded, m.focus)
		}
	}
}

func TestZeroWidthRunesInANoteCannotPushTheStateOff(t *testing.T) {
	// given
	// ... a long note of Thai syllables with tone marks while a turn runs
	m := newModel(&fakeAgent{}, NewStore(), nopClipboard{}, "fake", testLabels, nil)
	m.width, m.height = 80, 20
	m.statusNote = "pattern not found: " + strings.Repeat("ก\u0e48", 40)
	m.busy = true

	// when
	// ... the status bar is drawn
	status := statusText(m)

	// then
	// ... the note is cut and the full state still shows
	if !strings.HasSuffix(status, "working · ctrl-c to stop") || !strings.Contains(status, "…") {
		t.Errorf("status %q", status)
	}
}

type fakeRepoMap struct {
	text    string
	live    bool
	indexed int
}

func (f *fakeRepoMap) Indicator() (string, bool) { return f.text, f.live }

// Index starts a build, as the real map does.
func (f *fakeRepoMap) Index() string {
	f.indexed++
	f.text, f.live = "map …", true
	return "index: building"
}

func TestTheStatusBarShowsALiveIndicatorAndStopsPollingWhenItSettles(t *testing.T) {
	// given
	// ... an indicator reporting a build in progress
	indicator := &fakeRepoMap{text: "map 42%", live: true}
	m := newModel(&fakeAgent{}, NewStore(), nopClipboard{}, "fake", testLabels, indicator)
	m.handle(uv.WindowSizeEvent{Width: 80, Height: 10})

	// when
	// ... the UI starts, a poll finds the build further on, and the next finds it done
	m.readIndicator()
	started := m.indicatorDue != nil
	atStart := statusText(m)
	indicator.text = "map 87%"
	m.readIndicator()
	whileLive := m.indicatorDue != nil
	later := statusText(m)
	indicator.text, indicator.live = "map ✓", false
	m.readIndicator()
	settled := m.indicatorDue != nil
	atEnd := statusText(m)

	// then
	// ... the progress shows beside the model name and follows each poll
	if !strings.Contains(atStart, "map 42%  fake") || !strings.Contains(later, "map 87%") || !strings.Contains(atEnd, "map ✓") {
		t.Errorf("status %q, then %q, then %q", atStart, later, atEnd)
	}
	// ... and another poll is scheduled only while it is live
	if !started || !whileLive || settled {
		t.Errorf("poll due: at start %v, while live %v, once settled %v", started, whileLive, settled)
	}
}

func TestIndexBuildsTheRepoMapWithoutATurn(t *testing.T) {
	// given
	// ... a repo map that has not been built and is not being polled
	agent := &fakeAgent{}
	store := NewStore()
	repoMap := &fakeRepoMap{}
	m := newModel(agent, store, nopClipboard{}, "fake", testLabels, repoMap)
	m.handle(uv.WindowSizeEvent{Width: 80, Height: 10})
	m.readIndicator()

	// when
	// ... the user sends /index
	typeText(m, "/index")
	m.handle(uv.KeyPressEvent{Code: uv.KeyEnter})

	// then
	// ... the map is built and its line shown, with nothing sent to the model
	want := []shownEntry{{entryInfo, "index: building"}}
	if got := shownEntries(store, 0); repoMap.indexed != 1 || len(agent.prompts) != 0 || m.busy || !slices.Equal(got, want) {
		t.Errorf("indexed %d times, prompts %q, busy %v, entries %+v", repoMap.indexed, agent.prompts, m.busy, got)
	}
	// ... and the status bar follows the build from then on
	if status := statusText(m); m.indicatorDue == nil || !strings.Contains(status, "map …") {
		t.Errorf("polling %v, status %q", m.indicatorDue != nil, status)
	}
}

func TestIndexWithoutARepoMapSaysHowToTurnItOn(t *testing.T) {
	// given
	// ... no repo map
	agent := &fakeAgent{}
	store := NewStore()
	m := newModel(agent, store, nopClipboard{}, "fake", testLabels, nil)
	m.handle(uv.WindowSizeEvent{Width: 80, Height: 10})

	// when
	// ... the user sends /index
	typeText(m, "/index")
	m.handle(uv.KeyPressEvent{Code: uv.KeyEnter})

	// then
	// ... an info line names the setting, and nothing is sent to the model
	got := shownEntries(store, 0)
	if len(agent.prompts) != 0 || len(got) != 1 || !strings.Contains(got[0].text, `set "map": true`) {
		t.Errorf("prompts %q, entries %+v", agent.prompts, got)
	}
}

func TestTheIndicatorIsDroppedBeforeTheModeOrState(t *testing.T) {
	// given
	// ... a narrow terminal and a long indicator
	m := newModel(&fakeAgent{}, NewStore(), nopClipboard{}, "fake", testLabels, &fakeRepoMap{text: "map 99% and much more", live: true})
	m.width, m.height = 24, 10
	m.readIndicator()

	// when
	// ... the status bar is drawn
	status := strings.TrimSpace(statusText(m))

	// then
	// ... the mode and state keep their place and the indicator gives way
	if !strings.HasPrefix(status, "insert") || !strings.HasSuffix(status, "ready") || strings.Contains(status, "map") {
		t.Errorf("status %q", status)
	}
}

func scrolledUpModel() *model {
	store := NewStore()
	store.Show(0, core.ReplyStarted{})
	store.Show(0, core.ReplyText{Text: strings.Repeat("x\n", 60)})
	m := newModel(&fakeAgent{}, store, nopClipboard{}, "fake", testLabels, nil)
	m.handle(uv.WindowSizeEvent{Width: 40, Height: 12})
	m.focusTranscript()
	pressKeys(m, "gg")
	return m
}

func TestTypingWhileScrolledUpLeavesTheTranscriptWhereItWas(t *testing.T) {
	// given
	// ... a long transcript scrolled to its top
	m := scrolledUpModel()

	// when
	// ... a message is typed and more output arrives
	pressKeys(m, "ihello")
	m.store.Show(0, core.ReplyText{Text: "more\n"})
	m.layout()

	// then
	// ... the input has the text and the transcript still shows its top
	if string(m.editor.text) != "hello" || m.transcript.scrollTop != 0 {
		t.Errorf("input %q, scrollTop %d", string(m.editor.text), m.transcript.scrollTop)
	}
}

func TestSendingFromAScrolledUpTranscriptGoesToTheBottomAndFollows(t *testing.T) {
	// given
	// ... a message typed with the transcript scrolled to its top
	m := scrolledUpModel()
	pressKeys(m, "ihello")

	// when
	// ... it is sent and the reply arrives
	m.handle(uv.KeyPressEvent{Code: uv.KeyEnter})
	m.store.Show(0, core.ReplyText{Text: "more\n"})
	m.layout()

	// then
	// ... the transcript shows its last line
	if want := len(m.lines) - m.transcriptHeight; m.transcript.scrollTop != want {
		t.Errorf("scrollTop %d, want %d", m.transcript.scrollTop, want)
	}
}

func TestLeavingTheInputForAScrolledUpTranscriptKeepsTheCursor(t *testing.T) {
	// given
	// ... a message being typed with the transcript scrolled to its top
	m := scrolledUpModel()
	pressKeys(m, "ihello")

	// when
	// ... Esc and k go back to the transcript
	m.handle(uv.KeyPressEvent{Code: uv.KeyEscape})
	pressKeys(m, "k")

	// then
	// ... the cursor is where it was left, not on the last line
	if m.focus != focusTranscript || m.transcript.cursor.line != 0 || m.transcript.scrollTop != 0 {
		t.Errorf("focus %v, cursor %+v, scrollTop %d", m.focus, m.transcript.cursor, m.transcript.scrollTop)
	}
}

func TestAResultArrivingAboveASelectionLeavesItOnTheSameLines(t *testing.T) {
	// given
	// ... two running commands, the second selected linewise in the transcript
	store := NewStore()
	store.Show(0, core.ToolStarted{ID: "1", Name: "shell", Summary: "make"})
	store.Show(0, core.ToolStarted{ID: "2", Name: "shell", Summary: "ls"})
	m := newModel(&fakeAgent{}, store, nopClipboard{}, "fake", testLabels, nil)
	m.handle(uv.WindowSizeEvent{Width: 60, Height: 40})
	m.focusTranscript()
	pressKeys(m, "V")

	// when
	// ... the first command's output arrives above it, and the selection is yanked
	store.Show(0, core.ToolFinished{ID: "1", Output: "a\nb\nc"})
	m.layout()
	pressKeys(m, "y")

	// then
	// ... the second command is what is yanked
	if got := string(m.register); got != "ls" {
		t.Errorf("yanked %q", got)
	}
}

func TestAResultArrivingAboveAScrolledUpViewLeavesItShowingTheSameLines(t *testing.T) {
	// given
	// ... a running command, then enough lines to scroll it off the top, and a message being typed
	store := NewStore()
	store.Show(0, core.ToolStarted{ID: "1", Name: "shell", Summary: "make"})
	for i := range 30 {
		store.AddInfo(fmt.Sprint(i))
	}
	m := newModel(&fakeAgent{}, store, nopClipboard{}, "fake", testLabels, nil)
	m.handle(uv.WindowSizeEvent{Width: 40, Height: 12})
	m.focusTranscript()
	pressKeys(m, "gg")
	m.handle(uv.KeyPressEvent{Code: 'd', Mod: uv.ModCtrl})
	pressKeys(m, "i")
	before := screenRows(m)[:m.transcriptHeight]

	// when
	// ... the command's output arrives
	store.Show(0, core.ToolFinished{ID: "1", Output: "a\nb\nc"})
	m.layout()

	// then
	// ... the transcript shows the lines it showed before, none of them the command's
	after := screenRows(m)[:m.transcriptHeight]
	if !slices.Equal(after, before) || slices.Contains(before, " $ make") {
		t.Errorf("rows %q\nwere %q", after, before)
	}
}

func TestMovingOnlyTheCursorReachesTheTerminalInTheSameFrame(t *testing.T) {
	// given
	// ... a terminal screen already showing a frame with the cursor on it
	var out bytes.Buffer
	scr := uv.NewTerminalScreen(&out, nil)
	scr.EnterAltScreen()
	scr.Resize(40, 5)
	if err := present(scr, uv.Pos(1, 1), true); err != nil {
		t.Fatal(err)
	}
	out.Reset()

	// when
	// ... the next frame changes nothing but where the cursor is
	err := present(scr, uv.Pos(20, 4), true)

	// then
	// ... the move to the new place is written now, not held back until the frame after
	if want := ansi.CursorPosition(21, 5); err != nil || !strings.Contains(out.String(), want) {
		t.Errorf("err %v, wrote %q, want it to contain %q", err, out.String(), want)
	}
}

func TestAChildsLineGrowingAboveTheCursorLeavesItOnTheSameLine(t *testing.T) {
	// given
	// ... a narrow screen, a waiting child's line, and the cursor on a line with wide runes below it
	store := NewStore()
	store.Show(0, core.ChildWaiting{Child: 1, Model: "sonnet"})
	store.AddInfo("漢字 below")
	m := newModel(&fakeAgent{}, store, nopClipboard{}, "fake", testLabels, nil)
	m.handle(uv.WindowSizeEvent{Width: 30, Height: 12})
	m.focusTranscript()

	// when
	// ... the child gets a slot, so its line gains a count and wraps, and the cursor's line is yanked
	store.Show(0, core.ChildStarted{Child: 1, Model: "sonnet"})
	m.layout()
	pressKeys(m, "Vy")

	// then
	// ... the status line takes two rows and the line under the cursor is the one yanked
	rows := screenRows(m)[:2]
	if want := []string{" agent 1 · sonnet · 0 tools ·", " running"}; !slices.Equal(rows, want) || string(m.register) != "漢字 below" {
		t.Errorf("rows %q, yanked %q", rows, m.register)
	}
}

// childModel is a parent with a reply and one running child that has replied.
func childModel() *model {
	store := NewStore()
	store.Show(0, core.ReplyStarted{})
	store.Show(0, core.ReplyText{Text: "from the parent"})
	store.Show(0, core.ChildStarted{Child: 1, Model: "anthropic/sonnet"})
	store.Show(1, core.ReplyStarted{})
	store.Show(1, core.ReplyText{Text: "from the child"})
	m := newModel(&fakeAgent{}, store, nopClipboard{}, "fake", testLabels, nil)
	m.handle(uv.WindowSizeEvent{Width: 60, Height: 20})
	return m
}

func TestAShownChildsBufferIsDrawnAndItsEventsAppearAsTheyArrive(t *testing.T) {
	// given
	// ... a parent and a running child
	m := childModel()

	// when
	// ... the child's buffer is shown, and then more of its reply arrives
	m.show(1)
	shown := strings.Join(screenRows(m), "\n")
	m.store.Show(1, core.ReplyText{Text: ", and more"})
	m.layout()
	after := strings.Join(screenRows(m), "\n")

	// then
	// ... the screen has the child's transcript and not the parent's, then the new text
	if !strings.Contains(shown, "from the child") || strings.Contains(shown, "from the parent") || strings.Contains(shown, "agent 1") {
		t.Errorf("screen with the child shown:\n%s", shown)
	}
	if !strings.Contains(after, "from the child, and more") {
		t.Errorf("screen after the child's event:\n%s", after)
	}
}

func TestEventsForABufferThatIsNotShownLeaveTheScreenAlone(t *testing.T) {
	// given
	// ... a parent and a running child, the parent's buffer shown
	m := childModel()
	parentBefore := screenRows(m)

	// when
	// ... the child's events arrive, then its buffer is shown and the parent's arrive
	m.store.Show(1, core.ToolStarted{ID: "1", Name: "shell", Summary: "ls"})
	m.store.Show(1, core.ToolFinished{ID: "1", Output: "a.go"})
	m.layout()
	parentAfter := screenRows(m)
	m.show(1)
	childBefore := screenRows(m)
	m.store.Show(0, core.ReplyText{Text: ", again"})
	m.layout()
	childAfter := screenRows(m)

	// then
	// ... neither screen changed
	if !slices.Equal(parentAfter, parentBefore) {
		t.Errorf("parent's screen changed:\n%s", strings.Join(parentAfter, "\n"))
	}
	if !slices.Equal(childAfter, childBefore) {
		t.Errorf("child's screen changed:\n%s", strings.Join(childAfter, "\n"))
	}
}

// twoLongBuffers is a parent and a child that have each run the same two
// tools with folded output, taller together than the screen, with the
// transcript focused.
func twoLongBuffers() *model {
	store := NewStore()
	store.Show(0, core.ChildStarted{Child: 1, Model: "anthropic/sonnet"})
	for agent := range 2 {
		store.Show(agent, core.ToolStarted{ID: "1", Name: "shell", Summary: "seq 10"})
		store.Show(agent, core.ToolFinished{ID: "1", Output: "1\n2\n3\n4\n5\n6\n7\n8\n9\n10"})
		store.Show(agent, core.ToolStarted{ID: "2", Name: "shell", Summary: "seq 30"})
		store.Show(agent, core.ToolFinished{ID: "2", Output: strings.Repeat("line\n", 30)})
	}
	m := newModel(&fakeAgent{}, store, nopClipboard{}, "fake", testLabels, nil)
	m.handle(uv.WindowSizeEvent{Width: 60, Height: 12})
	m.focusTranscript()
	return m
}

func TestEachBufferKeepsItsOwnCursorScrollAndFolds(t *testing.T) {
	// given
	// ... the parent's buffer with everything expanded and the cursor part way down it
	m := twoLongBuffers()
	pressKeys(m, "zRgg"+strings.Repeat("j", 15))
	parentRows, parentCursor := drawn(m)

	// when
	// ... the child's buffer is shown and left at its top with one fold open, then each is shown again
	m.show(1)
	pressKeys(m, "ggjza")
	childRows, childCursor := drawn(m)
	m.show(0)
	parentRowsBack, parentCursorBack := drawn(m)
	m.show(1)
	childRowsBack, childCursorBack := drawn(m)

	// then
	// ... the two differ, and each comes back as it was left
	if slices.Equal(parentRows, childRows) || parentCursor == childCursor {
		t.Fatalf("setup: both buffers look the same:\n%s", strings.Join(childRows, "\n"))
	}
	if !slices.Equal(parentRowsBack, parentRows) || parentCursorBack != parentCursor {
		t.Errorf("parent came back with cursor %+v, want %+v:\n%s", parentCursorBack, parentCursor, strings.Join(parentRowsBack, "\n"))
	}
	if !slices.Equal(childRowsBack, childRows) || childCursorBack != childCursor {
		t.Errorf("child came back with cursor %+v, want %+v:\n%s", childCursorBack, childCursor, strings.Join(childRowsBack, "\n"))
	}
}

func TestAHiddenBuffersCursorStaysOnItsEntryWhenOutputArrivesAboveIt(t *testing.T) {
	// given
	// ... a child with two calls running, the cursor on the second command, and the parent's buffer shown
	store := NewStore()
	store.Show(0, core.ChildStarted{Child: 1, Model: "anthropic/sonnet"})
	store.Show(1, core.ToolStarted{ID: "1", Name: "shell", Summary: "first"})
	store.Show(1, core.ToolStarted{ID: "2", Name: "shell", Summary: "second"})
	m := newModel(&fakeAgent{}, store, nopClipboard{}, "fake", testLabels, nil)
	m.handle(uv.WindowSizeEvent{Width: 60, Height: 20})
	m.show(1)
	m.focusTranscript()
	pressKeys(m, "G")
	pressKeys(m, "k")
	before := lineText(m.lines[m.transcript.cursor.line])
	m.show(0)

	// when
	// ... the first call's output arrives and the child's buffer is shown again
	m.store.Show(1, core.ToolFinished{ID: "1", Output: "a\nb\nc"})
	m.layout()
	m.show(1)

	// then
	// ... the cursor is still on the second command
	if got := lineText(m.lines[m.transcript.cursor.line]); before != "$ second" || got != "$ second" {
		t.Errorf("cursor was on %q, now on %q", before, got)
	}
}

// runCommand types an ex command from the transcript and presses Enter.
func runCommand(m *model, command string) {
	pressKeys(m, ":"+command)
	m.handle(uv.KeyPressEvent{Code: uv.KeyEnter})
}

func TestColonOpensACommandLineThatEscLeavesAndEnterRuns(t *testing.T) {
	// given
	// ... a parent and a child, the parent's transcript focused
	m := childModel()
	m.focusTranscript()

	// when
	// ... a command is typed and left with Esc, then typed again and run with Enter
	pressKeys(m, ":b 1")
	typing, cursor := drawn(m)
	m.handle(uv.KeyPressEvent{Code: uv.KeyEscape})
	afterEsc := strings.Join(screenRows(m), "\n")
	pressKeys(m, ":b 1")
	m.handle(uv.KeyPressEvent{Code: uv.KeyEnter})
	afterEnter := strings.Join(screenRows(m), "\n")

	// then
	// ... the command line showed what was typed with the cursor after it, Esc did nothing, and Enter ran it
	if row := typing[len(typing)-1]; row != "  :b 1" || cursor != uv.Pos(6, len(typing)-1) {
		t.Errorf("command row %q, cursor %+v", row, cursor)
	}
	if !strings.Contains(afterEsc, "from the parent") || strings.Contains(afterEsc, ":b 1") {
		t.Errorf("screen after Esc:\n%s", afterEsc)
	}
	if !strings.Contains(afterEnter, "from the child") || strings.Contains(afterEnter, ":b 1") {
		t.Errorf("screen after Enter:\n%s", afterEnter)
	}
}

// listingModel is a parent with a child in each state, numbered in the
// order waiting, running, finished, failed, cancelled, the transcript focused.
func listingModel(width, height int, childModelName, childLabel string) *model {
	store := NewStore()
	store.Show(0, core.ChildWaiting{Child: 1, Model: childModelName})
	for child := 2; child <= 5; child++ {
		store.Show(0, core.ChildStarted{Child: child, Model: childModelName})
	}
	store.Show(0, core.ChildFinished{Child: 3})
	store.Show(0, core.ChildFailed{Child: 4, Reason: "boom"})
	store.Show(0, core.ChildCancelled{Child: 5})
	m := newModel(&fakeAgent{}, store, nopClipboard{}, "parent · effort high", map[string]string{childModelName: childLabel}, nil)
	m.handle(uv.WindowSizeEvent{Width: width, Height: height})
	m.focusTranscript()
	return m
}

func TestLsListsTheParentThenEachChildWithItsModelAndState(t *testing.T) {
	// given
	// ... a parent with a child in each state
	m := listingModel(60, 24, "fake/child", "child · effort low")
	entries := shownEntries(m.store, 0)

	// when
	// ... :ls is run
	runCommand(m, "ls")
	rows := screenRows(m)

	// then
	// ... the rows above the status bar list buffer 0, marked as shown, then each child, and no transcript gained an entry
	want := []string{
		"  % 0 · parent · effort high · ready",
		"    1 · child · effort low · waiting",
		"    2 · child · effort low · running",
		"    3 · child · effort low · finished",
		"    4 · child · effort low · failed",
		"    5 · child · effort low · cancelled",
	}
	status := len(rows) - 2
	if got := rows[status-len(want) : status]; !slices.Equal(got, want) {
		t.Errorf("listing %q\nwant %q", got, want)
	}
	if got := shownEntries(m.store, 0); !slices.Equal(got, entries) {
		t.Errorf("parent entries %v\nwere %v", got, entries)
	}
}

func TestAKeyDismissesTheListingAndDoesNothingElse(t *testing.T) {
	// given
	// ... the listing on screen
	m := listingModel(60, 24, "fake/child", "child · effort low")
	before := screenRows(m)
	runCommand(m, "ls")

	// when
	// ... q, which quits from the transcript, is pressed
	pressKeys(m, "q")

	// then
	// ... the screen is as it was before the listing and grimalkin has not quit
	if got := screenRows(m); !slices.Equal(got, before) || m.quit {
		t.Errorf("quit %v, screen:\n%s", m.quit, strings.Join(got, "\n"))
	}
}

func TestTheListingStaysWhileTheNextCommandIsTyped(t *testing.T) {
	// given
	// ... the listing on screen
	m := listingModel(60, 24, "fake/child", "child · effort low")
	runCommand(m, "ls")

	// when
	// ... another command is begun, then run
	pressKeys(m, ":b 2")
	whileTyping := strings.Join(screenRows(m), "\n")
	m.handle(uv.KeyPressEvent{Code: uv.KeyEnter})
	afterwards := strings.Join(screenRows(m), "\n")

	// then
	// ... the listing was there to read while typing and is gone once the command has run
	if !strings.Contains(whileTyping, "2 · child · effort low · running") {
		t.Errorf("screen while typing:\n%s", whileTyping)
	}
	if strings.Contains(afterwards, "· running") {
		t.Errorf("screen afterwards:\n%s", afterwards)
	}
}

func TestTheListingFitsANarrowScreenAndCountsWhatItLeavesOut(t *testing.T) {
	// given
	// ... a narrow, short terminal and children whose model name has wide runes
	m := listingModel(16, 8, "漢字/モデル名前", "モデル名前 · effort low")

	// when
	// ... :ls is run
	runCommand(m, "ls")
	rows := screenRows(m)

	// then
	// ... no row overflows, the status bar and input keep their rows, and the last listing row counts the rest
	for _, row := range rows {
		if ansi.StringWidth(row) > 16 {
			t.Errorf("row %q overflows", row)
		}
	}
	if !strings.HasPrefix(rows[len(rows)-2], "view") || rows[len(rows)-1] != "›" {
		t.Errorf("rows %q", rows)
	}
	if got := rows[len(rows)-3]; got != "··· 2 more" {
		t.Errorf("last listing row %q in %q", got, rows)
	}
}

func TestBShowsAChildsBufferWhetherItIsRunningOrHasEnded(t *testing.T) {
	// given
	// ... a running child and a finished one, each with a reply
	store := NewStore()
	store.Show(0, core.ChildStarted{Child: 1, Model: "fake/child"})
	store.Show(0, core.ChildStarted{Child: 2, Model: "fake/child"})
	store.Show(1, core.ReplyStarted{})
	store.Show(1, core.ReplyText{Text: "still going"})
	store.Show(2, core.ReplyStarted{})
	store.Show(2, core.ReplyText{Text: "all done"})
	store.Show(0, core.ChildFinished{Child: 2})
	m := newModel(&fakeAgent{}, store, nopClipboard{}, "fake", testLabels, nil)
	m.handle(uv.WindowSizeEvent{Width: 60, Height: 20})
	m.focusTranscript()

	// when
	// ... :b 1 is run, then :b 2
	runCommand(m, "b 1")
	first := strings.Join(screenRows(m), "\n")
	runCommand(m, "b 2")
	second := strings.Join(screenRows(m), "\n")

	// then
	// ... each shows that child's transcript and nothing of the other's or the parent's
	if !strings.Contains(first, "still going") || strings.Contains(first, "all done") || strings.Contains(first, "agent 1") {
		t.Errorf("screen after :b 1:\n%s", first)
	}
	if !strings.Contains(second, "all done") || strings.Contains(second, "still going") || strings.Contains(second, "agent 1") {
		t.Errorf("screen after :b 2:\n%s", second)
	}
}

func TestBZeroShowsTheParentsBufferAgain(t *testing.T) {
	// given
	// ... a child's buffer shown
	m := childModel()
	m.focusTranscript()
	before := screenRows(m)
	runCommand(m, "b 1")

	// when
	// ... :b 0 is run
	runCommand(m, "b 0")

	// then
	// ... the screen is the parent's, as it was
	if got := screenRows(m); !slices.Equal(got, before) {
		t.Errorf("screen:\n%s", strings.Join(got, "\n"))
	}
}

func TestAnUnknownBufferOrCommandIsAnErrorInTheStatusBarAndNothingElse(t *testing.T) {
	errors := map[string]string{
		"b 7": "no buffer 7",
		"b":   "not a command: b",
		"wq":  "not a command: wq",
	}
	for command, want := range errors {
		// given
		// ... a parent and a child, the parent's transcript focused
		m := childModel()
		m.focusTranscript()
		before := screenRows(m)

		// when
		// ... the command is run
		runCommand(m, command)
		rows := screenRows(m)

		// then
		// ... the status bar says why, and every other row is as it was
		status := len(rows) - 2
		if !strings.Contains(rows[status], want) {
			t.Errorf(":%s: status %q, want %q in it", command, rows[status], want)
		}
		rows[status] = before[status]
		if !slices.Equal(rows, before) {
			t.Errorf(":%s: screen changed:\n%s", command, strings.Join(rows, "\n"))
		}
	}
}

func TestColonTypedIntoTheInputIsAnOrdinaryCharacter(t *testing.T) {
	// given
	// ... a new session, which starts inserting
	m := childModel()

	// when
	// ... text with a colon in it is typed
	typeText(m, "a:b 1")
	rows := screenRows(m)

	// then
	// ... the input holds it and the parent's buffer is still shown
	if got := rows[len(rows)-1]; got != "  › a:b 1" || !strings.Contains(strings.Join(rows, "\n"), "from the parent") {
		t.Errorf("rows %q", rows)
	}
}

func TestGfOnAChildsStatusLineShowsItsBuffer(t *testing.T) {
	states := map[string][]core.Event{
		"waiting":  {core.ChildWaiting{Child: 1, Model: "fake/child"}},
		"running":  {core.ChildStarted{Child: 1, Model: "fake/child"}},
		"finished": {core.ChildStarted{Child: 1, Model: "fake/child"}, core.ChildFinished{Child: 1}},
	}
	for state, events := range states {
		// given
		// ... a parent's reply, then a child's status line with the cursor on it
		store := NewStore()
		store.Show(0, core.ReplyStarted{})
		store.Show(0, core.ReplyText{Text: "from the parent"})
		for _, event := range events {
			store.Show(0, event)
		}
		m := newModel(&fakeAgent{}, store, nopClipboard{}, "fake", testLabels, nil)
		m.handle(uv.WindowSizeEvent{Width: 60, Height: 20})
		m.focusTranscript()
		pressKeys(m, "gg}")
		on := lineText(m.lines[m.transcript.cursor.line])

		// when
		// ... gf is pressed, and then the child replies
		pressKeys(m, "gf")
		store.Show(1, core.ReplyStarted{})
		store.Show(1, core.ReplyText{Text: "from the child"})
		m.layout()
		screen := strings.Join(screenRows(m), "\n")

		// then
		// ... the screen is the child's buffer
		if !strings.HasPrefix(on, "agent 1 · fake/child") || !strings.HasSuffix(on, state) {
			t.Fatalf("%s: setup: cursor on %q", state, on)
		}
		if !strings.Contains(screen, "from the child") || strings.Contains(screen, "from the parent") {
			t.Errorf("%s: screen:\n%s", state, screen)
		}
	}
}

func TestGfOnAnyOfAChildsStatusLinesShowsTheSameBuffer(t *testing.T) {
	// given
	// ... a child that finished one run and is on its second, so has two status lines
	store := NewStore()
	store.Show(0, core.ChildStarted{Child: 1, Model: "fake/child"})
	store.Show(1, core.ReplyStarted{})
	store.Show(1, core.ReplyText{Text: "from the child"})
	store.Show(0, core.ChildFinished{Child: 1})
	store.Show(0, core.ChildStarted{Child: 1, Model: "fake/child"})
	m := newModel(&fakeAgent{}, store, nopClipboard{}, "fake", testLabels, nil)
	m.handle(uv.WindowSizeEvent{Width: 60, Height: 20})
	m.focusTranscript()

	// when
	// ... gf is pressed on the first line, and after going back to the parent, on the second
	pressKeys(m, "gg")
	onFirst := lineText(m.lines[m.transcript.cursor.line])
	pressKeys(m, "gf")
	fromFirst := screenRows(m)
	runCommand(m, "b 0")
	pressKeys(m, "}")
	onSecond := lineText(m.lines[m.transcript.cursor.line])
	pressKeys(m, "gf")
	fromSecond := screenRows(m)

	// then
	// ... both show the child's one buffer
	if !strings.HasSuffix(onFirst, "finished") || !strings.HasSuffix(onSecond, "running") {
		t.Fatalf("setup: cursor on %q, then %q", onFirst, onSecond)
	}
	if !strings.Contains(strings.Join(fromFirst, "\n"), "from the child") || !slices.Equal(fromSecond, fromFirst) {
		t.Errorf("from the first line:\n%s\nfrom the second:\n%s", strings.Join(fromFirst, "\n"), strings.Join(fromSecond, "\n"))
	}
}

func TestGfOffAStatusLineChangesNothing(t *testing.T) {
	// given
	// ... a parent and a child, the cursor on the parent's reply
	m := childModel()
	m.focusTranscript()
	pressKeys(m, "gg")
	before, cursorBefore := drawn(m)

	// when
	// ... gf is pressed
	pressKeys(m, "gf")
	after, cursorAfter := drawn(m)

	// then
	// ... the parent's buffer is still shown, the cursor where it was
	if lineText(m.lines[m.transcript.cursor.line]) != "from the parent" {
		t.Fatalf("setup: cursor on %q", lineText(m.lines[m.transcript.cursor.line]))
	}
	if !slices.Equal(after, before) || cursorAfter != cursorBefore {
		t.Errorf("cursor %+v, was %+v, screen:\n%s", cursorAfter, cursorBefore, strings.Join(after, "\n"))
	}
}

// agentsModel is a parent that has replied, child 1 finished with a reply and
// child 2 running, with the given buffer shown and the input focused.
func agentsModel(agent *fakeAgent, shown int) *model {
	store := NewStore()
	store.Show(0, core.ReplyStarted{})
	store.Show(0, core.ReplyText{Text: "from the parent"})
	store.Show(0, core.ChildStarted{Child: 1, Model: "fake/child"})
	store.Show(1, core.ReplyStarted{})
	store.Show(1, core.ReplyText{Text: "first answer"})
	store.Show(0, core.ChildFinished{Child: 1})
	store.Show(0, core.ChildStarted{Child: 2, Model: "fake/child"})
	m := newModel(agent, store, nopClipboard{}, "fake", testLabels, nil)
	m.handle(uv.WindowSizeEvent{Width: 60, Height: 20})
	m.show(shown)
	return m
}

func TestEnterInAFinishedChildsBufferSendsTheInputToThatChild(t *testing.T) {
	// given
	// ... a finished child's buffer shown, and a prompt typed
	agent := &fakeAgent{}
	m := agentsModel(agent, 1)
	parentBefore := shownEntries(m.store, 0)
	typeText(m, "and then?")

	// when
	// ... Enter is pressed, and the child's turn runs and answers
	m.handle(uv.KeyPressEvent{Code: uv.KeyEnter})
	m.store.Show(0, core.ChildStarted{Child: 1, Model: "fake/child"})
	m.store.Show(1, core.ReplyStarted{})
	m.store.Show(1, core.ReplyText{Text: "second answer"})
	m.store.Show(0, core.ChildFinished{Child: 1})
	m.finishTurn(<-m.turnDone)
	m.layout()
	rows := screenRows(m)

	// then
	// ... the child got the prompt, its buffer shows the prompt and the answer, and the input is empty
	if want := []string{"1: and then?"}; !slices.Equal(agent.childPrompts, want) {
		t.Errorf("child prompts %q, want %q", agent.childPrompts, want)
	}
	want := []shownEntry{{entryReply, "first answer"}, {entryUser, "and then?"}, {entryReply, "second answer"}}
	if got := shownEntries(m.store, 1); !slices.Equal(got, want) {
		t.Errorf("child entries %v\nwant %v", got, want)
	}
	screen := strings.Join(rows, "\n")
	if !strings.Contains(screen, "› and then?") || !strings.Contains(screen, "second answer") || rows[len(rows)-1] != "  ›" {
		t.Errorf("screen:\n%s", screen)
	}
	// ... and the parent was sent nothing and gained only the new run's status line
	wantParent := append(parentBefore, shownEntry{entryInfo, "agent 1 · fake/child · 0 tools · finished"})
	if got := shownEntries(m.store, 0); len(agent.prompts) != 0 || !slices.Equal(got, wantParent) {
		t.Errorf("parent prompts %q, entries %v\nwant %v", agent.prompts, got, wantParent)
	}
}

func TestEnterInABusyChildsBufferDoesNothingAndKeepsTheInput(t *testing.T) {
	busy := map[string]core.Event{
		"waiting": core.ChildWaiting{Child: 3, Model: "fake/child"},
		"running": core.ChildStarted{Child: 3, Model: "fake/child"},
	}
	for state, event := range busy {
		// given
		// ... the buffer of a child that is waiting or running, and a prompt typed
		agent := &fakeAgent{}
		m := agentsModel(agent, 0)
		m.store.Show(0, event)
		m.show(3)
		typeText(m, "hurry up")

		// when
		// ... Enter is pressed
		m.handle(uv.KeyPressEvent{Code: uv.KeyEnter})
		rows := screenRows(m)

		// then
		// ... nothing is sent and the input still holds the text
		if len(agent.childPrompts) != 0 || len(agent.prompts) != 0 || rows[len(rows)-1] != "  › hurry up" {
			t.Errorf("%s: child prompts %q, prompts %q, input row %q", state, agent.childPrompts, agent.prompts, rows[len(rows)-1])
		}
	}
}

func TestARefusedChildTurnKeepsTheInputAndSaysWhyInTheStatusBar(t *testing.T) {
	// given
	// ... a finished child's buffer shown, a prompt typed, and a core that will refuse the turn
	agent := &fakeAgent{refuse: errors.New("agent 1 is busy")}
	m := agentsModel(agent, 1)
	before := shownEntries(m.store, 1)
	typeText(m, "and then?")

	// when
	// ... Enter is pressed
	m.handle(uv.KeyPressEvent{Code: uv.KeyEnter})
	rows := screenRows(m)

	// then
	// ... the status bar gives the reason, the input keeps its text and the buffer gains nothing
	if !strings.Contains(rows[len(rows)-2], "agent 1 is busy") || rows[len(rows)-1] != "  › and then?" {
		t.Errorf("rows %q", rows[len(rows)-2:])
	}
	if got := shownEntries(m.store, 1); !slices.Equal(got, before) {
		t.Errorf("child entries %v", got)
	}
}

func TestCtrlCInARunningChildsBufferCancelsThatChildAlone(t *testing.T) {
	// given
	// ... the parent's turn running with two children running, and the first one's buffer shown
	agent := &fakeAgent{}
	m := agentsModel(agent, 0)
	typeText(m, "go")
	m.handle(uv.KeyPressEvent{Code: uv.KeyEnter})
	m.store.Show(0, core.ChildStarted{Child: 3, Model: "fake/child"})
	m.show(2)

	// when
	// ... Ctrl-C is pressed
	m.handle(uv.KeyPressEvent{Code: 'c', Mod: uv.ModCtrl})

	// then
	// ... only that child is cancelled: not the parent's turn, not the other child, and grimalkin stays open
	if !slices.Equal(agent.childCancelled, []int{2}) || agent.cancelled != 0 || m.quit {
		t.Errorf("children cancelled %v, parent cancelled %d times, quit %v", agent.childCancelled, agent.cancelled, m.quit)
	}
}

func TestACancelledChildsBufferStaysAndShowsItsTurnCancelled(t *testing.T) {
	// given
	// ... a running child's buffer shown, and Ctrl-C pressed
	m := agentsModel(&fakeAgent{}, 2)
	m.handle(uv.KeyPressEvent{Code: 'c', Mod: uv.ModCtrl})

	// when
	// ... the core reports the turn discarded and the child cancelled
	m.store.Show(2, core.TurnDiscarded{Reason: "cancelled"})
	m.store.Show(0, core.ChildCancelled{Child: 2})
	m.layout()
	rows := screenRows(m)

	// then
	// ... the child's buffer is still on screen, says so, and the status bar is back to ready
	screen := strings.Join(rows, "\n")
	if !strings.Contains(screen, "cancelled (turn discarded)") || strings.Contains(screen, "from the parent") || !strings.Contains(rows[len(rows)-2], "ready") {
		t.Errorf("screen:\n%s", screen)
	}
}

func TestCtrlCInAnIdleChildsBufferQuitsWhenNothingIsRunning(t *testing.T) {
	// given
	// ... a finished child's buffer shown, with no turn running anywhere
	agent := &fakeAgent{}
	m := childModel()
	m.agent = agent
	m.store.Show(0, core.ChildFinished{Child: 1})
	m.show(1)

	// when
	// ... Ctrl-C is pressed
	m.handle(uv.KeyPressEvent{Code: 'c', Mod: uv.ModCtrl})

	// then
	// ... grimalkin quits and nothing is cancelled
	if !m.quit || agent.cancelled != 0 || len(agent.childCancelled) != 0 {
		t.Errorf("quit %v, cancelled %d, children cancelled %v", m.quit, agent.cancelled, agent.childCancelled)
	}
}

func TestCtrlCInAnIdleBufferWhileAnotherAgentRunsDoesNothingButSaySo(t *testing.T) {
	// given
	// ... the parent's turn running, and a finished child's buffer shown
	agent := &fakeAgent{}
	m := agentsModel(agent, 0)
	typeText(m, "go")
	m.handle(uv.KeyPressEvent{Code: uv.KeyEnter})
	m.show(1)

	// when
	// ... Ctrl-C is pressed
	m.handle(uv.KeyPressEvent{Code: 'c', Mod: uv.ModCtrl})

	// then
	// ... nothing is cancelled, grimalkin stays open, and the status bar says other agents are running
	if m.quit || agent.cancelled != 0 || len(agent.childCancelled) != 0 {
		t.Errorf("quit %v, cancelled %d, children cancelled %v", m.quit, agent.cancelled, agent.childCancelled)
	}
	if status := statusText(m); !strings.Contains(status, "other agents are running") {
		t.Errorf("status %q", status)
	}
}

func TestTheStatusBarShowsTheShownAgentsStateWhateverTheOthersDo(t *testing.T) {
	// given
	// ... the parent idle, child 1 finished and child 2 running
	m := agentsModel(&fakeAgent{}, 0)

	// when
	// ... each buffer is shown; then the parent's turn starts and the finished child's is shown again
	parentIdle := statusText(m)
	m.show(2)
	childRunning := statusText(m)
	m.show(0)
	typeText(m, "go")
	m.handle(uv.KeyPressEvent{Code: uv.KeyEnter})
	parentRunning := statusText(m)
	m.show(1)
	childIdle := statusText(m)

	// then
	// ... each status is the shown agent's own
	if !strings.Contains(parentIdle, "ready") || !strings.Contains(childRunning, "working") {
		t.Errorf("idle parent %q, running child %q", parentIdle, childRunning)
	}
	if !strings.Contains(parentRunning, "working") || !strings.Contains(childIdle, "ready") {
		t.Errorf("running parent %q, idle child %q", parentRunning, childIdle)
	}
}

func TestEnterInTheParentsBufferSendsToTheParentWhileAUsersChildTurnRuns(t *testing.T) {
	// given
	// ... a turn the user started on a child still running, and the parent's buffer shown again
	agent := &fakeAgent{}
	m := agentsModel(agent, 1)
	typeText(m, "to the child")
	m.handle(uv.KeyPressEvent{Code: uv.KeyEnter})
	working := statusText(m)
	m.show(0)
	ready := statusText(m)

	// when
	// ... a prompt is typed and sent
	typeText(m, "to the parent")
	m.handle(uv.KeyPressEvent{Code: uv.KeyEnter})
	<-m.turnDone
	<-m.turnDone

	// then
	// ... the child's buffer was working and the parent's ready, and each agent got its own prompt
	if !strings.Contains(working, "working") || !strings.Contains(ready, "ready") {
		t.Errorf("child status %q, parent status %q", working, ready)
	}
	if !slices.Equal(agent.prompts, []string{"to the parent"}) || !slices.Equal(agent.childPrompts, []string{"1: to the child"}) {
		t.Errorf("prompts %q, child prompts %q", agent.prompts, agent.childPrompts)
	}
	if got := shownEntries(m.store, 0); got[len(got)-1] != (shownEntry{entryUser, "to the parent"}) {
		t.Errorf("parent entries %v", got)
	}
}

func TestIndexInAChildsBufferBuildsTheMapAndSendsNothing(t *testing.T) {
	// given
	// ... a finished child's buffer shown, and a repo map
	agent := &fakeAgent{}
	m := agentsModel(agent, 1)
	repoMap := &fakeRepoMap{}
	m.repoMap = repoMap

	// when
	// ... the user sends /index
	typeText(m, "/index")
	m.handle(uv.KeyPressEvent{Code: uv.KeyEnter})

	// then
	// ... the map is built, its line is on screen, and no agent is sent anything
	screen := strings.Join(screenRows(m), "\n")
	if repoMap.indexed != 1 || len(agent.childPrompts) != 0 || len(agent.prompts) != 0 || !strings.Contains(screen, "index: building") {
		t.Errorf("indexed %d times, child prompts %q, prompts %q, screen:\n%s", repoMap.indexed, agent.childPrompts, agent.prompts, screen)
	}
}

func TestTheStatusBarShowsTheShownBuffersModelAndEffort(t *testing.T) {
	// given
	// ... a parent, and children on another model with an effort, on the parent's model as a fork is, and on a model with no effort
	store := NewStore()
	store.Show(0, core.ChildStarted{Child: 1, Model: "other/glm"})
	store.Show(0, core.ChildStarted{Child: 2, Model: "fake/parent"})
	store.Show(0, core.ChildWaiting{Child: 3, Model: "other/plain"})
	labels := map[string]string{
		"fake/parent": "parent · effort high",
		"other/glm":   "glm · effort low",
		"other/plain": "plain · effort n/a",
	}
	m := newModel(&fakeAgent{}, store, nopClipboard{}, "parent · effort high", labels, nil)
	m.handle(uv.WindowSizeEvent{Width: 80, Height: 20})

	// when
	// ... each child's buffer is shown, then the parent's again
	m.show(1)
	otherModel := statusText(m)
	m.show(2)
	parentsModel := statusText(m)
	m.show(3)
	noEffort := statusText(m)
	m.show(0)
	parent := statusText(m)

	// then
	// ... the status bar has the shown agent's model and effort each time
	if !strings.Contains(otherModel, " glm · effort low ") || !strings.Contains(noEffort, " plain · effort n/a ") {
		t.Errorf("other model %q, no effort %q", otherModel, noEffort)
	}
	if !strings.Contains(parentsModel, " parent · effort high ") || !strings.Contains(parent, " parent · effort high ") {
		t.Errorf("child on the parent's model %q, parent %q", parentsModel, parent)
	}
}

func TestAChildOnAModelWithNoLabelWiredIsAPanic(t *testing.T) {
	// given
	// ... a child whose model the composition root gave no label for
	store := NewStore()
	store.Show(0, core.ChildStarted{Child: 1, Model: "other/unwired"})
	m := newModel(&fakeAgent{}, store, nopClipboard{}, "fake", testLabels, nil)
	m.handle(uv.WindowSizeEvent{Width: 80, Height: 20})
	m.show(1)
	var panicked any

	// when
	// ... the status bar is drawn
	func() {
		defer func() { panicked = recover() }()
		statusText(m)
	}()

	// then
	// ... it panics, naming the model
	if !strings.Contains(fmt.Sprint(panicked), "other/unwired") {
		t.Errorf("panicked with %v", panicked)
	}
}
