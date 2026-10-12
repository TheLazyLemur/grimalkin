package tui

import (
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/ultraviolet/screen"
	"github.com/charmbracelet/x/ansi"
)

// Agent is the parent agent. RunTurn blocks until the parent's turn ends and
// Cancel cancels it, with every child it started. StartChildTurn claims a
// child for a turn the user starts on it: it returns at once, with an error
// when the child can't take one, and otherwise run, which must be called
// once and blocks until that turn ends. CancelChild cancels the turn one
// child is running or waiting to run, whoever started it.
type Agent interface {
	RunTurn(prompt string)
	Cancel()
	StartChildTurn(number int, prompt string) (run func(), err error)
	CancelChild(number int)
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

// Run starts the UI and returns when the user quits. modelName is the
// parent's model and effort as the status bar writes them. childLabels gives
// the same for every model a child may run on, the parent's included, by the
// name the core's lifecycle events carry. repoMap may be nil.
func Run(agent Agent, store *Store, clipboard Clipboard, modelName string, childLabels map[string]string, repoMap RepoMap) (err error) {
	if agent == nil || store == nil || clipboard == nil {
		panic("tui ports not wired")
	}
	m := newModel(agent, store, clipboard, modelName, childLabels, repoMap)

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

func newModel(agent Agent, store *Store, clipboard Clipboard, modelName string, childLabels map[string]string, repoMap RepoMap) *model {
	m := &model{
		agent:       agent,
		store:       store,
		clipboard:   clipboard,
		modelName:   modelName,
		childLabels: childLabels,
		repoMap:     repoMap,
		turnDone:    make(chan turnEnd, 1),
		childTurns:  map[int]bool{},
		hidden:      map[int]bufferView{},
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
	// childLabels is the label of each model a child may run on, by the
	// name its lifecycle events carry.
	childLabels map[string]string
	repoMap     RepoMap

	indicatorText string
	// indicatorDue fires when a live indicator is to be read again. Nil
	// while it isn't live.
	indicatorDue <-chan time.Time

	editor editor
	// shown is the number of the agent whose buffer is on screen. transcript
	// and lines are that buffer's; show changes it.
	shown      int
	transcript transcriptView
	// hidden holds the view of each buffer that has been shown and is not now.
	hidden map[int]bufferView
	focus  focus
	// command is the ex command line, open while typing is set.
	command struct {
		typing bool
		typed  []rune
	}
	// listing is whether :ls's list of buffers is over the transcript.
	listing    bool
	register   []rune
	statusNote string
	// busy is whether the parent's turn is running. childTurns holds the
	// children running a turn the user started, from Enter until the turn
	// ends, which is before the core's first event for it and so sooner
	// than the store knows.
	busy       bool
	childTurns map[int]bool
	quit       bool
	// turnDone gets a value when a turn the user started ends.
	turnDone chan turnEnd

	width, height int

	lines            []screenLine
	transcriptHeight int
	inputRows        []screenLine
	inputCursor      position
}

// bufferView is where the user left a buffer: the cursor and scroll, and the
// lines they index, so the cursor can be put back on the entry it was on
// after events arrived while the buffer was hidden.
type bufferView struct {
	transcript transcriptView
	lines      []screenLine
}

// show puts an agent's buffer on screen, as it was left. A buffer shown for
// the first time starts at its top, or follows its end while the input has
// focus. A selection ends with the switch; the search pattern goes along.
func (m *model) show(agent int) {
	if !m.store.hasBuffer(agent) {
		panic(fmt.Sprintf("agent %d has no buffer to show", agent))
	}
	if agent == m.shown {
		return
	}
	m.transcript.visual, m.transcript.pendingKey, m.transcript.pendingWindowCmd = visualOff, 0, false
	m.hidden[m.shown] = bufferView{m.transcript, m.lines}
	next := m.hidden[agent]
	delete(m.hidden, agent)
	next.transcript.search = m.transcript.search
	m.shown, m.transcript, m.lines = agent, next.transcript, next.lines
	m.layout()
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

// turnEnd is the end of a turn the user started: whose it was, and what it
// panicked with, or "".
type turnEnd struct {
	agent    int
	panicked string
}

// finishTurn ends a turn runTurn started. A turn that panicked panics again
// here, on the UI's goroutine, where Run restores the terminal first.
func (m *model) finishTurn(end turnEnd) {
	if end.panicked != "" {
		panic(end.panicked)
	}
	if end.agent == parentAgent {
		m.busy = false
		return
	}
	if !m.childTurns[end.agent] {
		panic(fmt.Sprintf("agent %d ended a turn the user did not start", end.agent))
	}
	delete(m.childTurns, end.agent)
}

// label is an agent's model and effort, as the status bar and :ls write
// them. A child on a model with no label was wired wrongly: the composition
// root gives one for every model the core may run a child on.
func (m *model) label(agent int) string {
	if agent == parentAgent {
		return m.modelName
	}
	name := m.store.childModel(agent)
	label, ok := m.childLabels[name]
	if !ok {
		panic(fmt.Sprintf("agent %d runs on %s, which has no label", agent, name))
	}
	return label
}

// shownBusy reports whether the shown agent has a turn: running, or for a
// child, waiting to run. The input, Enter, Ctrl-C and the status bar's state
// all go by it.
func (m *model) shownBusy() bool {
	if m.shown == parentAgent {
		return m.busy
	}
	return m.childTurns[m.shown] || m.store.childBusy(m.shown)
}

func (m *model) handleKey(char rune, key keyCode) {
	m.statusNote = ""
	if m.focus == focusInput {
		m.transcript.visual = visualOff
	}
	if key == keyCtrlC {
		m.interrupt()
		return
	}
	if m.command.typing {
		m.handleCommandKey(char, key)
		return
	}
	// Any key puts the listing away and does nothing else, so a habitual q
	// can't quit. A colon keeps it, to be read while the next command is typed.
	if m.listing && char != ':' {
		m.listing = false
		return
	}
	if m.focus == focusTranscript {
		m.handleTranscriptKey(char, key)
		return
	}
	m.handleInputKey(char, key)
}

// interrupt is Ctrl-C: it cancels the shown agent's turn, and quits only
// when no agent has one. The children the parent started run inside its
// turn, so the parent's turn and the user's turns on children are all there
// is to check.
func (m *model) interrupt() {
	switch {
	case m.shownBusy() && m.shown == parentAgent:
		m.agent.Cancel()
	case m.shownBusy():
		m.agent.CancelChild(m.shown)
	case m.busy || len(m.childTurns) > 0:
		m.statusNote = "other agents are running · :ls to see them"
	default:
		m.quit = true
	}
}

func (m *model) handleCommandKey(char rune, key keyCode) {
	c := &m.command
	switch {
	case key == keyEsc, key == keyBackspace && len(c.typed) == 0:
		c.typing, m.listing = false, false
	case key == keyEnter:
		c.typing, m.listing = false, false
		m.runCommand(strings.TrimSpace(string(c.typed)))
	case key == keyBackspace:
		c.typed = c.typed[:len(c.typed)-1]
	case char != 0:
		c.typed = append(c.typed, char)
	}
}

// runCommand runs an ex command: ls lists the buffers and b N shows one.
// Anything else is an error in the status bar.
func (m *model) runCommand(command string) {
	if command == "" {
		return
	}
	if command == "ls" {
		m.listing = true
		return
	}
	if number, ok := strings.CutPrefix(command, "b"); ok {
		if agent, err := strconv.Atoi(strings.TrimSpace(number)); err == nil {
			if !m.store.hasBuffer(agent) {
				m.statusNote = fmt.Sprintf("no buffer %d", agent)
				return
			}
			m.show(agent)
			return
		}
	}
	m.statusNote = "not a command: " + command
}

// listingRows is what :ls shows, under a hairline: the parent as buffer 0,
// then each child, with its model and state. % marks the shown buffer. It is
// read from the store on each draw, so states change as they happen. Rows
// that don't fit the transcript's height are counted on the last one.
func (m *model) listingRows() []screenLine {
	row := func(agent int, model, state string) screenLine {
		mark := " "
		if agent == m.shown {
			mark = "%"
		}
		return screenLine{cells: styledCells(fmt.Sprintf("%s %d · %s · %s", mark, agent, model, state), colorInfo, 0)}
	}
	state := "ready"
	if m.busy {
		state = childRunning
	}
	rows := []screenLine{
		{cells: styledCells(strings.Repeat("─", m.contentWidth()), colorFaint, 0)},
		row(parentAgent, m.label(parentAgent), state),
	}
	for _, child := range m.store.childBuffers() {
		rows = append(rows, row(child.number, m.label(child.number), child.state))
	}
	if extra := len(rows) - m.transcriptHeight; extra > 0 {
		rows = append(rows[:m.transcriptHeight-1], screenLine{cells: styledCells(fmt.Sprintf("··· %d more", extra+1), colorFaint, 0)})
	}
	return rows
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
	case viewCommand:
		m.command.typing, m.command.typed = true, nil
	case viewGoToFile:
		// A status line stands for its child's buffer, as a path does a file.
		if owner := m.lines[m.transcript.cursor.line].owner; owner != nil && owner.child != 0 {
			m.show(owner.child)
		}
	case viewToggleFold:
		if len(m.lines) > 0 {
			if fold := m.lines[m.transcript.cursor.line].fold; fold != nil {
				m.refold(func() { m.store.toggleFold(fold) })
			}
		}
	case viewExpandAll:
		m.refold(func() { m.store.setAllFolds(m.shown, true) })
	case viewCollapseAll:
		m.refold(func() { m.store.setAllFolds(m.shown, false) })
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
	m.lines = m.store.render(m.shown, m.contentWidth())
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
	if len(m.editor.text) == 0 || m.shownBusy() {
		return
	}
	prompt := string(m.editor.text)
	run := func() { m.agent.RunTurn(prompt) }
	switch {
	case strings.TrimSpace(prompt) == indexCommand:
		run = nil
	case m.shown != parentAgent:
		var err error
		if run, err = m.agent.StartChildTurn(m.shown, prompt); err != nil {
			m.statusNote = err.Error()
			return
		}
		m.childTurns[m.shown] = true
	default:
		m.busy = true
	}
	// Sending goes back to the bottom, wherever the transcript was left.
	m.transcript.scrollTop = len(m.lines)
	m.editor.reset()
	if run == nil {
		m.index()
		return
	}
	m.store.addUser(m.shown, prompt)
	go m.runTurn(m.shown, run)
}

// runTurn runs an agent's turn on its own goroutine and reports its end on
// turnDone.
func (m *model) runTurn(agent int, run func()) {
	end := turnEnd{agent: agent}
	defer func() {
		if r := recover(); r != nil {
			end.panicked = fmt.Sprintf("turn panicked: %v\n\n%s", r, debug.Stack())
		}
		m.turnDone <- end
	}()
	run()
}

// index builds the repo map and starts following its progress. Its line goes
// in the shown buffer, where the user typed the command.
func (m *model) index() {
	if m.repoMap == nil {
		m.store.addInfo(m.shown, `index: there is no repo map · set "map": true in the config's search block`)
		return
	}
	m.store.addInfo(m.shown, m.repoMap.Index())
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
	if m.transcript.search.typing || m.command.typing {
		m.inputRows = m.inputRows[:1]
	}
	m.transcriptHeight = max(1, m.height-1-len(m.inputRows)-m.topPadding())
	old := m.lines
	m.lines = m.store.render(m.shown, m.contentWidth())
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
	if m.listing {
		rows := m.listingRows()
		for i, row := range rows {
			y := inputTop - len(rows) + i
			screen.ClearArea(scr, uv.Rect(0, y, m.width, 1))
			m.drawLine(scr, y, row)
		}
	}
	m.drawLine(scr, inputTop, m.statusBar())
	inputTop++

	if m.transcript.search.typing || m.command.typing {
		prompt := m.promptLine()
		m.drawLine(scr, inputTop, prompt)
		return uv.Pos(left+cellsWidth(prompt.cells), inputTop), true
	}

	m.drawInput(scr, inputTop)
	switch {
	case m.listing:
		// The listing may cover the transcript's cursor.
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
	busy := m.shownBusy()
	state, stateColor := "ready", colorDim
	if busy {
		state, stateColor = "working · ctrl-c to stop", colorBusy
	}
	// Reserve the mode and activity before giving room to notes or the model.
	// Compact labels keep the activity visible even at the minimum width.
	budget := width - len(mode) - 2
	if busy {
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
	shownLabel := m.label(m.shown)
	modelLabel := styledCells(" "+shownLabel+" ", colorDim, 0)
	if shownLabel != "" && cellsWidth(left)+cellsWidth(modelLabel)+cellsWidth(right) < width {
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

// promptLine is the search or command line being typed, in place of the input.
func (m *model) promptLine() screenLine {
	prompt, text := '/', m.transcript.search.typed
	switch {
	case m.command.typing:
		prompt, text = ':', m.command.typed
	case m.transcript.search.typedBackward:
		prompt = '?'
	}
	cells := []cell{{prompt, colorUser | attrBold, 0}}
	typed := styledCells(string(text), 0, 0)
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
