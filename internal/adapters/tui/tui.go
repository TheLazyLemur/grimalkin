package tui

import (
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/ultraviolet/screen"
	"github.com/charmbracelet/x/ansi"
)

type Agent interface {
	RunTurn(prompt string)
	Cancel()
}

type Clipboard interface {
	Copy(text string)
}

// RepoMap is the search tool's repo map as the UI sees it. Indicator is a
// short piece of state for the status bar, such as the build's progress: the
// TUI polls it every second while live is true, and an empty text shows
// nothing. Index builds the map because the user typed /index, and returns a
// line to show them.
type RepoMap interface {
	Indicator() (text string, live bool)
	Index() string
}

// indexCommand builds the repo map. The UI handles it: it never reaches the
// model.
const indexCommand = "/index"

// indicatorPoll is how often a live indicator is read.
const indicatorPoll = time.Second

// Run starts the UI and returns when the user quits. repoMap may be nil.
func Run(agent Agent, store *Store, clipboard Clipboard, modelName string, repoMap RepoMap) (err error) {
	if agent == nil || store == nil || clipboard == nil {
		panic("tui ports not wired")
	}
	m := newModel(agent, store, clipboard, modelName, repoMap)

	term := uv.DefaultTerminal()
	scr := term.Screen()
	scr.EnterAltScreen()
	scr.EnableBracketedPaste()
	// Shift+Enter only differs from Enter in terminals that speak the Kitty
	// keyboard protocol or xterm's modifyOtherKeys, so ask for both.
	scr.SetKeyboardEnhancements(&uv.KeyboardEnhancements{DisambiguateEscapeCodes: true})
	scr.WriteString(ansi.SetModifyOtherKeys2)
	if err := term.Start(); err != nil {
		return err
	}
	// Deferred, so a panic restores the terminal before it is reported.
	defer func() {
		scr.WriteString(ansi.ResetModifyOtherKeys)
		if stopErr := term.Stop(); err == nil {
			err = stopErr
		}
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	if repoMap != nil {
		m.readIndicator()
	}
	for !m.quit {
		select {
		case ev := <-term.Events():
			if size, ok := ev.(uv.WindowSizeEvent); ok {
				scr.Resize(size.Width, size.Height)
			}
			m.handle(ev)
		case <-store.changed:
			m.layout()
		case panicked := <-m.turnDone:
			m.finishTurn(panicked)
		case <-m.indicatorDue:
			m.readIndicator()
		case <-signals:
			return nil
		}

		cursor, shown := m.draw(scr)
		if err := present(scr, cursor, shown); err != nil {
			return err
		}
	}
	return nil
}

// present sends what was drawn, and the cursor, to the terminal.
func present(scr *uv.TerminalScreen, cursor uv.Position, shown bool) error {
	scr.SetCursorPosition(cursor.X, cursor.Y)
	if shown != scr.CursorVisible() {
		if shown {
			scr.ShowCursor()
		} else {
			scr.HideCursor()
		}
	}
	scr.Render()
	if err := scr.Flush(); err != nil {
		return err
	}
	// Flush queues the cursor's move in the renderer, and only Render hands
	// the renderer's output on. Without a second pass a frame that moves
	// nothing but the cursor shows it one frame late.
	scr.Render()
	return scr.Flush()
}

func newModel(agent Agent, store *Store, clipboard Clipboard, modelName string, repoMap RepoMap) *model {
	m := &model{
		agent:     agent,
		store:     store,
		clipboard: clipboard,
		modelName: modelName,
		repoMap:   repoMap,
		turnDone:  make(chan string, 1),
	}
	m.editor.reset()
	return m
}

type focus int

const (
	focusInput focus = iota
	focusTranscript
)

type model struct {
	agent     Agent
	store     *Store
	clipboard Clipboard
	modelName string
	repoMap   RepoMap

	indicatorText string
	// indicatorDue fires when a live indicator is to be read again. Nil
	// while it isn't live.
	indicatorDue <-chan time.Time

	editor     editor
	transcript transcriptView
	focus      focus
	register   []rune
	statusNote string
	busy       bool
	quit       bool
	// turnDone gets a value when a turn ends: what it panicked with, or "".
	turnDone chan string

	width, height int

	lines            []screenLine
	transcriptHeight int
	inputRows        []screenLine
	inputCursor      position
}

// readIndicator reads the indicator now and, while it is live, again after
// indicatorPoll.
func (m *model) readIndicator() {
	text, live := m.repoMap.Indicator()
	m.indicatorText = text
	m.indicatorDue = nil
	if live {
		m.indicatorDue = time.After(indicatorPoll)
	}
}

// handle applies a terminal event and lays the screen out again.
func (m *model) handle(ev uv.Event) {
	switch ev := ev.(type) {
	case uv.WindowSizeEvent:
		m.width, m.height = ev.Width, ev.Height
	case uv.PasteEvent:
		if m.focus == focusInput && m.editor.mode == modeInsert {
			text := strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(ev.Content)
			m.editor.insertText([]rune(text))
		}
	case uv.KeyPressEvent:
		m.handleKey(translateKey(ev))
	}
	m.layout()
}

// finishTurn ends the turn runTurn started. A turn that panicked panics again
// here, on the UI's goroutine, where Run restores the terminal first.
func (m *model) finishTurn(panicked string) {
	if panicked != "" {
		panic(panicked)
	}
	m.busy = false
}

func (m *model) handleKey(char rune, key keyCode) {
	m.statusNote = ""
	if m.focus == focusInput {
		m.transcript.visual = visualOff
	}
	if key == keyCtrlC {
		if m.busy {
			m.agent.Cancel()
		} else {
			m.quit = true
		}
		return
	}
	if m.focus == focusTranscript {
		m.handleTranscriptKey(char, key)
		return
	}
	m.handleInputKey(char, key)
}

func (m *model) handleTranscriptKey(char rune, key keyCode) {
	switch m.transcript.handleKey(m.lines, m.transcriptHeight, &m.register, char, key) {
	case viewInsert:
		m.focus = focusInput
		m.editor.saveUndo()
		m.editor.startInsert(len(m.editor.text))
	case viewFocusInput:
		m.focus = focusInput
		m.editor.mode = modeNormal
		m.editor.clampCursor()
	case viewYanked:
		m.clipboard.Copy(string(m.register))
		m.statusNote = fmt.Sprintf("yanked %d chars", len(m.register))
	case viewQuit:
		m.quit = true
	case viewToggleFold:
		if len(m.lines) > 0 {
			if fold := m.lines[m.transcript.cursor.line].fold; fold != nil {
				m.refold(func() { m.store.toggleFold(fold) })
			}
		}
	case viewExpandAll:
		m.refold(func() { m.store.setAllFolds(true) })
	case viewCollapseAll:
		m.refold(func() { m.store.setAllFolds(false) })
	}
}

// refold changes folds, then puts the cursor back where it was relative to
// an anchor entry: the one under the cursor, or for a line that belongs to
// none (a divider), the next entry. If that spot no longer exists because the
// block collapsed, the cursor goes to the top of the block, with the block in
// view.
func (m *model) refold(change func()) {
	if len(m.lines) == 0 {
		change()
		return
	}
	cursor := m.transcript.cursor.line
	anchor, delta := lineAnchor(m.lines, cursor)
	inside := m.lines[cursor].owner == anchor && anchor != nil

	change()
	m.lines = m.store.render(m.contentWidth())
	if anchor == nil {
		m.transcript.clampAndScroll(m.lines, m.transcriptHeight)
		return
	}
	start, end := ownedRange(m.lines, anchor)
	if start < 0 {
		m.transcript.clampAndScroll(m.lines, m.transcriptHeight)
		return
	}
	for end > start && len(m.lines[end].cells) == 0 {
		end--
	}
	target := start + delta
	if inside && target > end {
		// Collapsed under the cursor: go to the block's command, if it has one.
		target = start
		if anchor.kind == entryOutput && start > 0 && m.lines[start-1].fold == anchor {
			target = start - 1
			for target > 0 && m.lines[target-1].owner == m.lines[target].owner {
				target--
			}
		}
	}
	target = clamp(target, 0, len(m.lines)-1)
	m.transcript.jumpTo(m.lines, m.transcriptHeight, position{target, 0})
}

// lineAnchor names a line by an entry and how far the line is from that
// entry's first: the entry it belongs to, or for a line that belongs to none
// (a divider), the next entry.
func lineAnchor(lines []screenLine, line int) (anchor *entry, delta int) {
	anchorLine := line
	for anchorLine < len(lines) && lines[anchorLine].owner == nil {
		anchorLine++
	}
	if anchorLine == len(lines) {
		for anchorLine = line; anchorLine > 0 && lines[anchorLine].owner == nil; anchorLine-- {
		}
	}
	anchor = lines[anchorLine].owner
	start, _ := ownedRange(lines, anchor)
	return anchor, line - start
}

// movedLine is where a line of old went in lines: as far into the same entry
// as it was. A line old doesn't have stays put.
func movedLine(old, lines []screenLine, line int) int {
	if line < 0 || line >= len(old) {
		return line
	}
	anchor, delta := lineAnchor(old, line)
	start, _ := ownedRange(lines, anchor)
	if start < 0 {
		return line
	}
	return start + delta
}

// ownedRange is the first and last line rendered from e, or -1, -1.
func ownedRange(lines []screenLine, e *entry) (start, end int) {
	start, end = -1, -1
	if e == nil {
		return
	}
	for i, line := range lines {
		if line.owner == e {
			if start < 0 {
				start = i
			}
			end = i
		}
	}
	return
}

func (m *model) handleInputKey(char rune, key keyCode) {
	if key == keyNewline {
		if m.editor.mode == modeInsert {
			m.editor.handleKey(&m.register, '\n', keyNone)
		}
		return
	}

	if key == keyPageUp || key == keyCtrlB {
		m.focusTranscript()
		m.transcript.handleKey(m.lines, m.transcriptHeight, &m.register, 0, key)
		return
	}

	switch m.editor.handleKey(&m.register, char, key) {
	case editorFocusTranscript:
		m.focusTranscript()
	case editorSend:
		m.send()
	}
}

func (m *model) focusTranscript() {
	m.focus = focusTranscript
	if m.atBottom() {
		m.transcript.focusLastLine(m.lines, m.transcriptHeight)
	}
}

// atBottom reports whether the transcript showed its last line when it was
// last laid out. Only then does it follow new output while the input has
// focus, so typing leaves a scrolled-up transcript where it is.
func (m *model) atBottom() bool {
	return m.transcript.scrollTop >= len(m.lines)-m.transcriptHeight
}

func (m *model) send() {
	if len(m.editor.text) == 0 || m.busy {
		return
	}
	prompt := string(m.editor.text)
	// Sending goes back to the bottom, wherever the transcript was left.
	m.transcript.scrollTop = len(m.lines)
	if strings.TrimSpace(prompt) == indexCommand {
		m.editor.reset()
		m.index()
		return
	}
	m.editor.reset()
	m.store.addUser(prompt)
	m.busy = true
	go m.runTurn(prompt)
}

// runTurn runs a turn on its own goroutine and reports its end on turnDone.
func (m *model) runTurn(prompt string) {
	panicked := ""
	defer func() {
		if r := recover(); r != nil {
			panicked = fmt.Sprintf("turn panicked: %v\n\n%s", r, debug.Stack())
		}
		m.turnDone <- panicked
	}()
	m.agent.RunTurn(prompt)
}

// index builds the repo map and starts following its progress.
func (m *model) index() {
	if m.repoMap == nil {
		m.store.AddInfo(`index: there is no repo map · set "map": true in the config's search block`)
		return
	}
	m.store.AddInfo(m.repoMap.Index())
	m.readIndicator()
}

// margin is the empty space either side of the content. It shrinks to
// nothing on narrow terminals so text keeps the room.
func (m *model) margin() int {
	switch {
	case m.width >= 60:
		return 2
	case m.width >= 30:
		return 1
	}
	return 0
}

// topPadding is the blank row above the transcript, dropped on short terminals.
func (m *model) topPadding() int {
	if m.height >= 16 {
		return 1
	}
	return 0
}

func (m *model) contentWidth() int {
	return max(m.width-2*m.margin(), 10)
}

func (m *model) layout() {
	follow := m.focus == focusInput && m.atBottom()
	m.inputRows, m.inputCursor = layoutInput(&m.editor, m.contentWidth(), max(1, m.height/3))
	if m.transcript.search.typing {
		m.inputRows = m.inputRows[:1]
	}
	m.transcriptHeight = max(1, m.height-1-len(m.inputRows)-m.topPadding())
	old := m.lines
	m.lines = m.store.render(m.contentWidth())
	// A tool's result goes in under its command, which can be above the
	// cursor: keep the cursor, the selection and the view on what they were on.
	m.transcript.cursor.line = movedLine(old, m.lines, m.transcript.cursor.line)
	m.transcript.anchor.line = movedLine(old, m.lines, m.transcript.anchor.line)
	m.transcript.scrollTop = movedLine(old, m.lines, m.transcript.scrollTop)
	if follow {
		m.transcript.scrollTop = max(0, len(m.lines)-m.transcriptHeight)
	} else {
		m.transcript.clampAndScroll(m.lines, m.transcriptHeight)
	}
	if m.transcript.note != "" {
		m.statusNote = m.transcript.note
		m.transcript.note = ""
	}
}

func layoutInput(e *editor, width, maxRows int) (rows []screenLine, cursor position) {
	const promptWidth = 2
	newRow := func(first bool) screenLine {
		if first {
			return screenLine{
				cells:     []cell{{'›', colorUser | attrBold, 0}, {' ', 0, 0}},
				prefixLen: promptWidth,
			}
		}
		return screenLine{cells: []cell{{' ', 0, 0}, {' ', 0, 0}}, prefixLen: promptWidth}
	}
	var all []screenLine
	row, col := newRow(true), promptWidth
	nextRow := func() {
		all = append(all, row)
		row, col = newRow(false), promptWidth
	}
	for i, char := range e.text {
		if char == '\n' {
			if i == e.cursor {
				cursor = position{len(all), col}
			}
			nextRow()
			continue
		}
		charWidth := max(1, runeWidth(char))
		if col+charWidth > width {
			nextRow()
		}
		if i == e.cursor {
			cursor = position{len(all), col}
		}
		row.cells = append(row.cells, cell{char, 0, 0})
		col += charWidth
	}
	if e.cursor >= len(e.text) {
		if col >= width {
			nextRow()
		}
		cursor = position{len(all), col}
	}
	all = append(all, row)

	visible := min(len(all), maxRows)
	if cursor.line < e.scrollRow {
		e.scrollRow = cursor.line
	}
	if cursor.line >= e.scrollRow+visible {
		e.scrollRow = cursor.line - visible + 1
	}
	e.scrollRow = clamp(e.scrollRow, 0, len(all)-visible)
	cursor.line -= e.scrollRow
	return all[e.scrollRow : e.scrollRow+visible], cursor
}

// draw paints the whole screen and reports where the cursor goes, if it is
// shown.
func (m *model) draw(scr uv.Screen) (cursor uv.Position, shown bool) {
	screen.Clear(scr)
	if m.width < 10 || m.height < 4 {
		return cursor, false
	}
	left, top := m.margin(), m.topPadding()
	m.drawTranscript(scr, top)
	inputTop := top + m.transcriptHeight
	m.drawLine(scr, inputTop, m.statusBar())
	inputTop++

	if m.transcript.search.typing {
		prompt := m.searchPrompt()
		m.drawLine(scr, inputTop, prompt)
		return uv.Pos(left+cellsWidth(prompt.cells), inputTop), true
	}

	m.drawInput(scr, inputTop)
	switch {
	case m.focus == focusInput:
		return uv.Pos(left+m.inputCursor.col, inputTop+m.inputCursor.line), true
	case len(m.lines) > 0:
		at := m.transcript.cursor
		x := cellsWidth(m.lines[at.line].cells[:at.col])
		return uv.Pos(left+x, top+at.line-m.transcript.scrollTop), true
	}
	return cursor, false
}

func (m *model) drawTranscript(scr uv.Screen, top int) {
	pattern := m.transcript.search.pattern
	ignoreCase := ignoresCase(pattern)
	selecting := m.focus == focusTranscript && m.transcript.visual != visualOff
	selStart, selEnd := m.transcript.selection()

	for y := range m.transcriptHeight {
		i := m.transcript.scrollTop + y
		if i >= len(m.lines) {
			break
		}
		line := m.lines[i]
		cells := slices.Clone(line.cells)
		for _, start := range matchStarts(cells, pattern, ignoreCase) {
			for j := start; j < start+len(pattern); j++ {
				cells[j].fg, cells[j].bg = colorMatchText, colorMatchBg
			}
		}
		if selecting && i >= selStart.line && i <= selEnd.line {
			from, to := 0, len(cells)
			if i == selStart.line {
				from = selStart.col
			}
			if i == selEnd.line && selEnd.col != endOfLine {
				to = min(len(cells), selEnd.col+1)
			}
			for j := from; j < to; j++ {
				cells[j].fg, cells[j].bg = line.cells[j].fg, colorSelectionBg
			}
			if m.transcript.visual == visualLine {
				line.fill = colorSelectionBg
			}
		}
		line.cells = cells
		m.drawLine(scr, top+y, line)
	}
}

// statusBar is a single quiet line: the mode in its accent colour, a
// hairline, then the model and what it's doing.
func (m *model) statusBar() screenLine {
	mode, modeColor := "view", colorModeView
	switch m.transcript.visual {
	case visualChar:
		mode = "visual"
	case visualLine:
		mode = "v-line"
	}
	if m.focus == focusInput {
		switch m.editor.mode {
		case modeInsert:
			mode, modeColor = "insert", colorModeInsert
		case modeNormal:
			mode, modeColor = "normal", colorModeNormal
		}
	}

	width := m.contentWidth()
	state, stateColor := "ready", colorDim
	if m.busy {
		state, stateColor = "working · ctrl-c to stop", colorBusy
	}
	// Reserve the mode and activity before giving room to notes or the model.
	// Compact labels keep the activity visible even at the minimum width.
	budget := width - len(mode) - 2
	if m.busy {
		for _, compact := range []string{"working · ctrl-c", "working", "run"} {
			if ansi.StringWidth(state) <= budget {
				break
			}
			state = compact
		}
	} else if len(state) > budget {
		state = "ok"
	}
	left := styledCells(mode, modeColor|attrBold, 0)
	right := append(styledCells(" ", 0, 0), styledCells(state, stateColor, 0)...)
	if m.statusNote != "" {
		room := width - cellsWidth(left) - cellsWidth(right) - 3
		if room > 0 {
			// Cut by cells, the unit the bar is laid out in, so the note can
			// never push the state off the line.
			note := styledCells(m.statusNote, colorDim|attrItalic, 0)
			if cellsWidth(note) > room {
				for len(note) > 0 && cellsWidth(note)+1 > room {
					note = note[:len(note)-1]
				}
				note = append(note, cell{'…', colorDim | attrItalic, 0})
			}
			left = append(left, styledCells("  ", 0, 0)...)
			left = append(left, note...)
		}
	}
	if cellsWidth(left)+cellsWidth(right) < width {
		left = append(left, cell{' ', 0, 0})
	}
	modelLabel := styledCells(" "+m.modelName+" ", colorDim, 0)
	if m.modelName != "" && cellsWidth(left)+cellsWidth(modelLabel)+cellsWidth(right) < width {
		right = append(modelLabel, right...)
	}
	// Background state goes last: it is the first thing dropped for room.
	indicatorLabel := styledCells(" "+m.indicatorText+" ", colorDim, 0)
	if m.indicatorText != "" && cellsWidth(left)+cellsWidth(indicatorLabel)+cellsWidth(right) < width {
		right = append(indicatorLabel, right...)
	}
	bar := left
	for range max(0, width-cellsWidth(left)-cellsWidth(right)) {
		bar = append(bar, cell{'─', colorFaint, 0})
	}
	bar = append(bar, right...)
	return screenLine{cells: bar}
}

func (m *model) searchPrompt() screenLine {
	prompt := '/'
	if m.transcript.search.typedBackward {
		prompt = '?'
	}
	cells := []cell{{prompt, colorUser | attrBold, 0}}
	typed := styledCells(string(m.transcript.search.typed), 0, 0)
	// Keep the prompt and the insertion cursor visible as the query grows.
	remaining := cellsWidth(typed)
	for len(typed) > 0 && remaining > m.contentWidth()-2 {
		remaining -= cellsWidth(typed[:1])
		typed = typed[1:]
	}
	return screenLine{cells: append(cells, typed...)}
}

func (m *model) drawInput(scr uv.Screen, top int) {
	for y, row := range m.inputRows {
		if m.focus != focusInput {
			row.cells = slices.Clone(row.cells)
			for j := row.prefixLen; j < len(row.cells); j++ {
				row.cells[j].fg = colorDim
			}
		}
		m.drawLine(scr, top+y, row)
	}
}

func cellsWidth(cells []cell) int {
	width := 0
	for _, c := range cells {
		width += max(1, runeWidth(c.char))
	}
	return width
}

// drawLine draws a line on row y, inside the margins. Margins are added here,
// never stored in cells, so yanks and searches can't see them.
func (m *model) drawLine(scr uv.Screen, y int, line screenLine) {
	x := m.margin()
	end := x + m.contentWidth()
	for _, c := range line.cells {
		charWidth := max(1, runeWidth(c.char))
		if x+charWidth > end {
			break
		}
		bg := c.bg
		if bg == 0 {
			bg = line.fill
		}
		char := c.char
		if runeWidth(char) <= 0 {
			char = ' '
		}
		scr.SetCell(x, y, &uv.Cell{Content: string(char), Width: charWidth, Style: cellStyle(c.fg, bg)})
		x += charWidth
	}
	if line.fill == 0 {
		return
	}
	for ; x < end; x++ {
		scr.SetCell(x, y, &uv.Cell{Content: " ", Width: 1, Style: cellStyle(0, line.fill)})
	}
}

func cellStyle(fg, bg attr) uv.Style {
	var style uv.Style
	if fg&attrBold != 0 {
		style.Attrs |= uv.AttrBold
	}
	if fg&attrItalic != 0 {
		style.Attrs |= uv.AttrItalic
	}
	if fg&attrUnderline != 0 {
		style.Underline = uv.UnderlineSingle
	}
	if color := fg.color(); color != 0 {
		style.Fg = ansi.IndexedColor(color)
	}
	if color := bg.color(); color != 0 {
		style.Bg = ansi.IndexedColor(color)
	}
	return style
}
