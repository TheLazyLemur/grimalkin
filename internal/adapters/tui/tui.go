package tui

import (
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

type Agent interface {
	RunTurn(prompt string)
	Cancel()
}

type Clipboard interface {
	Copy(text string)
}

func Run(agent Agent, store *Store, clipboard Clipboard, modelName string) error {
	if agent == nil || store == nil || clipboard == nil {
		panic("tui ports not wired")
	}
	_, err := tea.NewProgram(newModel(agent, store, clipboard, modelName)).Run()
	return err
}

func newModel(agent Agent, store *Store, clipboard Clipboard, modelName string) *model {
	m := &model{
		agent:     agent,
		store:     store,
		clipboard: clipboard,
		modelName: modelName,
		styles:    map[[2]attr]lipgloss.Style{},
	}
	m.editor.reset()
	return m
}

type focus int

const (
	focusInput focus = iota
	focusTranscript
)

type storeChangedMsg struct{}

type turnFinishedMsg struct{}

type model struct {
	agent     Agent
	store     *Store
	clipboard Clipboard
	modelName string

	editor     editor
	transcript transcriptView
	focus      focus
	register   []rune
	statusNote string
	busy       bool

	width, height int

	lines            []screenLine
	transcriptHeight int
	inputRows        []screenLine
	inputCursor      position

	styles map[[2]attr]lipgloss.Style
}

func (m *model) Init() tea.Cmd {
	return m.waitForStoreChange()
}

func (m *model) waitForStoreChange() tea.Cmd {
	return func() tea.Msg {
		<-m.store.changed
		return storeChangedMsg{}
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case storeChangedMsg:
		cmd = m.waitForStoreChange()
	case turnFinishedMsg:
		m.busy = false
	case tea.PasteMsg:
		if m.focus == focusInput && m.editor.mode == modeInsert {
			text := strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(msg.Content)
			m.editor.insertText([]rune(text))
		}
	case tea.KeyPressMsg:
		cmd = m.handleKey(translateKey(msg))
	}
	m.layout()
	return m, cmd
}

func (m *model) handleKey(char rune, key keyCode) tea.Cmd {
	m.statusNote = ""
	if m.focus == focusInput {
		m.transcript.visual = visualOff
	}
	if key == keyCtrlC {
		if !m.busy {
			return tea.Quit
		}
		m.agent.Cancel()
		return nil
	}
	if m.focus == focusTranscript {
		return m.handleTranscriptKey(char, key)
	}
	return m.handleInputKey(char, key)
}

func (m *model) handleTranscriptKey(char rune, key keyCode) tea.Cmd {
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
		return tea.Quit
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
	return nil
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
	anchorLine := cursor
	for anchorLine < len(m.lines) && m.lines[anchorLine].owner == nil {
		anchorLine++
	}
	if anchorLine == len(m.lines) {
		for anchorLine = cursor; anchorLine > 0 && m.lines[anchorLine].owner == nil; anchorLine-- {
		}
	}
	anchor := m.lines[anchorLine].owner
	oldStart, _ := ownedRange(m.lines, anchor)
	delta := cursor - oldStart
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

func (m *model) handleInputKey(char rune, key keyCode) tea.Cmd {
	if key == keyNewline {
		if m.editor.mode == modeInsert {
			m.editor.handleKey(&m.register, '\n', keyNone)
		}
		return nil
	}

	if key == keyPageUp || key == keyCtrlB {
		m.focusTranscript()
		m.transcript.handleKey(m.lines, m.transcriptHeight, &m.register, 0, key)
		return nil
	}

	switch m.editor.handleKey(&m.register, char, key) {
	case editorFocusTranscript:
		m.focusTranscript()
	case editorSend:
		return m.send()
	}
	return nil
}

func (m *model) focusTranscript() {
	m.focus = focusTranscript
	m.transcript.focusLastLine(m.lines, m.transcriptHeight)
}

func (m *model) send() tea.Cmd {
	if len(m.editor.text) == 0 || m.busy {
		return nil
	}
	prompt := string(m.editor.text)
	m.editor.reset()
	m.store.addUser(prompt)
	m.busy = true
	agent := m.agent
	return func() tea.Msg {
		agent.RunTurn(prompt)
		return turnFinishedMsg{}
	}
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
	m.inputRows, m.inputCursor = layoutInput(&m.editor, m.contentWidth(), max(1, m.height/3))
	if m.transcript.search.typing {
		m.inputRows = m.inputRows[:1]
	}
	m.transcriptHeight = max(1, m.height-1-len(m.inputRows)-m.topPadding())
	m.lines = m.store.render(m.contentWidth())
	if m.focus == focusInput {
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

func (m *model) View() tea.View {
	view := tea.View{AltScreen: true}
	if m.width < 10 || m.height < 4 {
		return view
	}
	left, top := m.margin(), m.topPadding()
	rows := make([]string, top, m.height)
	rows = append(rows, m.renderTranscript()...)
	rows = append(rows, m.renderStatusBar())
	inputTop := len(rows)

	if m.transcript.search.typing {
		prompt := m.searchPrompt()
		rows = append(rows, m.renderLine(prompt))
		view.Content = strings.Join(rows, "\n")
		view.Cursor = tea.NewCursor(left+cellsWidth(prompt.cells), inputTop)
		return view
	}

	rows = append(rows, m.renderInput()...)
	view.Content = strings.Join(rows, "\n")
	switch {
	case m.focus == focusInput:
		view.Cursor = tea.NewCursor(left+m.inputCursor.col, inputTop+m.inputCursor.line)
	case len(m.lines) > 0:
		cursor := m.transcript.cursor
		x := cellsWidth(m.lines[cursor.line].cells[:cursor.col])
		view.Cursor = tea.NewCursor(left+x, top+cursor.line-m.transcript.scrollTop)
	}
	return view
}

func (m *model) renderTranscript() []string {
	pattern := m.transcript.search.pattern
	ignoreCase := ignoresCase(pattern)
	selecting := m.focus == focusTranscript && m.transcript.visual != visualOff
	selStart, selEnd := m.transcript.selection()

	rows := make([]string, 0, m.transcriptHeight)
	for y := range m.transcriptHeight {
		i := m.transcript.scrollTop + y
		if i >= len(m.lines) {
			rows = append(rows, "")
			continue
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
		rows = append(rows, m.renderLine(line))
	}
	return rows
}

// renderStatusBar is a single quiet line: the mode in its accent colour, a
// hairline, then the model and what it's doing.
func (m *model) renderStatusBar() string {
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
	bar := left
	for range max(0, width-cellsWidth(left)-cellsWidth(right)) {
		bar = append(bar, cell{'─', colorFaint, 0})
	}
	bar = append(bar, right...)
	return m.renderLine(screenLine{cells: bar})
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

func (m *model) renderInput() []string {
	rows := make([]string, 0, len(m.inputRows))
	for _, row := range m.inputRows {
		if m.focus != focusInput {
			row.cells = slices.Clone(row.cells)
			for j := row.prefixLen; j < len(row.cells); j++ {
				row.cells[j].fg = colorDim
			}
		}
		rows = append(rows, m.renderLine(row))
	}
	return rows
}

func cellsWidth(cells []cell) int {
	width := 0
	for _, c := range cells {
		width += max(1, runeWidth(c.char))
	}
	return width
}

// renderLine draws a line inside the margins. Margins are added here, never
// stored in cells, so yanks and searches can't see them.
func (m *model) renderLine(line screenLine) string {
	var out strings.Builder
	width := m.contentWidth()
	out.WriteString(strings.Repeat(" ", m.margin()))
	var run []rune
	var runFg, runBg attr
	flush := func() {
		if len(run) > 0 {
			out.WriteString(m.style(runFg, runBg).Render(string(run)))
			run = run[:0]
		}
	}
	x := 0
	for _, c := range line.cells {
		charWidth := max(1, runeWidth(c.char))
		if x+charWidth > width {
			break
		}
		bg := c.bg
		if bg == 0 {
			bg = line.fill
		}
		if c.fg != runFg || bg != runBg {
			flush()
			runFg, runBg = c.fg, bg
		}
		char := c.char
		if runeWidth(char) <= 0 {
			char = ' '
		}
		run = append(run, char)
		x += charWidth
	}
	flush()
	if line.fill != 0 && x < width {
		out.WriteString(m.style(0, line.fill).Render(strings.Repeat(" ", width-x)))
	}
	return out.String()
}

func (m *model) style(fg, bg attr) lipgloss.Style {
	key := [2]attr{fg, bg}
	if style, ok := m.styles[key]; ok {
		return style
	}
	style := lipgloss.NewStyle().
		Bold(fg&attrBold != 0).
		Italic(fg&attrItalic != 0).
		Underline(fg&attrUnderline != 0)
	if color := fg.color(); color != 0 {
		style = style.Foreground(lipgloss.ANSIColor(color))
	}
	if color := bg.color(); color != 0 {
		style = style.Background(lipgloss.ANSIColor(color))
	}
	m.styles[key] = style
	return style
}
