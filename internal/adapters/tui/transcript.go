package tui

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	"grimalkin/internal/core"
)

// foldedTailRows is how much of a collapsed tool output stays visible, in
// screen rows. The last rows are kept because builds, tests and greps end
// with the result.
const foldedTailRows = 4

type entryKind int

const (
	entryInfo entryKind = iota
	entryUser
	entryReply
	entryCommand
	entryOutput
	entryError
)

type entry struct {
	kind entryKind
	text strings.Builder

	// expanded shows a whole output entry; collapsed shows its tail, or
	// nothing at all when hideAll is set (skill content).
	expanded bool
	hideAll  bool

	lines            []screenLine
	renderedLen      int
	renderedWidth    int
	renderedExpanded bool
}

type Store struct {
	mu      sync.Mutex
	entries []*entry
	reply   *entry
	// running holds the tool calls that have started and not finished, by
	// call ID, so each result lands under its own command.
	running map[string]runningCall
	// children holds each child's latest status line, by child number.
	children map[int]*childStatus
	changed  chan struct{}
}

type runningCall struct {
	// command is nil for an agent call, which shows as its child's status
	// line alone.
	command *entry
	// skill marks a skill tool call, whose content folds like a skill
	// loaded with /name.
	skill bool
}

func NewStore() *Store {
	return &Store{
		changed:  make(chan struct{}, 1),
		running:  map[string]runningCall{},
		children: map[int]*childStatus{},
	}
}

func (s *Store) AddInfo(text string) {
	s.add(entryInfo, text)
	s.notify()
}

func (s *Store) addUser(prompt string) {
	s.add(entryUser, prompt)
	s.notify()
}

func (s *Store) Show(agent int, event core.Event) {
	// Only the parent's events are shown until each agent has a buffer.
	if agent != 0 {
		return
	}
	switch event := event.(type) {
	case core.ReplyStarted:
		s.add(entryReply, "")
		s.mu.Lock()
		s.reply = s.entries[len(s.entries)-1]
		s.mu.Unlock()
	case core.ReplyText:
		s.mu.Lock()
		if s.reply == nil {
			panic("reply text before ReplyStarted")
		}
		s.reply.text.WriteString(event.Text)
		s.mu.Unlock()
	case core.ToolStarted:
		s.startCall(event)
	case core.ToolFinished:
		s.finishCall(event)
	case core.SkillLoaded:
		s.add(entryCommand, "skill "+event.Name)
		s.addHidden(event.Content)
	case core.TurnDiscarded:
		s.add(entryError, event.Reason+" (turn discarded)")
	case core.ChildWaiting:
		s.beginChild(event.Child, event.Model, childWaiting)
	case core.ChildStarted:
		s.beginChild(event.Child, event.Model, childRunning)
	case core.ChildProgress:
		s.progressChild(event.Child, event.Tools)
	case core.ChildFinished:
		s.endChild(event.Child, childFinished)
	case core.ChildFailed:
		// The reason goes to the model, as the call's result.
		s.endChild(event.Child, childFailed)
	case core.ChildCancelled:
		s.endChild(event.Child, childCancelled)
	default:
		panic(fmt.Sprintf("unhandled event %T", event))
	}
	s.notify()
}

// startCall adds a tool call's command and records the call as running, in
// one step: calls can start at once, and each must record its own command.
func (s *Store) startCall(event core.ToolStarted) {
	command := &entry{kind: entryCommand}
	command.text.WriteString(event.Summary)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.running[event.ID]; ok {
		panic(fmt.Sprintf("tool call %q started twice", event.ID))
	}
	if event.Name == core.AgentToolName {
		s.running[event.ID] = runningCall{}
		return
	}
	s.entries = append(s.entries, command)
	s.running[event.ID] = runningCall{command: command, skill: event.Name == core.SkillToolName}
}

// finishCall puts a tool call's result under its command rather than at the
// end, since calls run at once finish in any order.
func (s *Store) finishCall(event core.ToolFinished) {
	result := &entry{kind: entryOutput}
	result.text.WriteString(event.Output)
	s.mu.Lock()
	defer s.mu.Unlock()
	call, ok := s.running[event.ID]
	if !ok {
		panic(fmt.Sprintf("tool call %q finished without starting", event.ID))
	}
	delete(s.running, event.ID)
	if call.command == nil {
		return
	}
	switch {
	case event.IsError:
		result.kind = entryError
	case call.skill:
		result.hideAll = true
	}
	s.entries = slices.Insert(s.entries, slices.Index(s.entries, call.command)+1, result)
}

const (
	childWaiting   = "waiting"
	childRunning   = "running"
	childFinished  = "finished"
	childFailed    = "failed"
	childCancelled = "cancelled"
)

// childStatus is one run of a child and the line that shows it.
type childStatus struct {
	line   *entry
	number int
	model  string
	state  string
	// started is whether the child has had a slot, and so has a tool count.
	started bool
	tools   int
}

func (c *childStatus) ended() bool {
	return c.state != childWaiting && c.state != childRunning
}

// redraw rewrites the line in place. The entry's rendering is dropped by
// hand, since a new count can leave the text the same length.
func (c *childStatus) redraw() {
	text := fmt.Sprintf("agent %d · %s", c.number, c.model)
	if c.started {
		text += " · " + plural(c.tools, "tool")
	}
	c.line.text.Reset()
	c.line.text.WriteString(text + " · " + c.state)
	c.line.lines = nil
}

// beginChild shows a child as waiting or running. A waiting child that gets
// a slot keeps its line; a child run again after it ended gets a new one, so
// the earlier run stays as it ended.
func (s *Store) beginChild(number int, model, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	child := s.children[number]
	switch {
	case child != nil && child.state == childWaiting && state == childRunning:
	case child == nil || child.ended():
		child = &childStatus{line: &entry{kind: entryInfo}, number: number}
		s.children[number] = child
		s.entries = append(s.entries, child.line)
	default:
		panic(fmt.Sprintf("child %d is %s while already %s", number, state, child.state))
	}
	child.model, child.state, child.started = model, state, state == childRunning
	child.redraw()
}

func (s *Store) progressChild(number, tools int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	child := s.children[number]
	if child == nil || child.state != childRunning {
		panic(fmt.Sprintf("progress from child %d, which is not running", number))
	}
	child.tools = tools
	child.redraw()
}

func (s *Store) endChild(number int, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	child := s.children[number]
	if child == nil || child.ended() {
		panic(fmt.Sprintf("child %d %s without waiting or running", number, state))
	}
	child.state = state
	child.redraw()
}

func (s *Store) add(kind entryKind, text string) {
	s.addEntry(&entry{kind: kind}, text)
}

// addHidden adds skill content, which collapses to its line count alone.
func (s *Store) addHidden(text string) {
	s.addEntry(&entry{kind: entryOutput, hideAll: true}, text)
}

func (s *Store) addEntry(e *entry, text string) {
	e.text.WriteString(text)
	s.mu.Lock()
	s.entries = append(s.entries, e)
	s.mu.Unlock()
}

func (s *Store) notify() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// toggleFold expands or collapses one output entry.
func (s *Store) toggleFold(e *entry) {
	s.mu.Lock()
	e.expanded = !e.expanded
	s.mu.Unlock()
}

// setAllFolds expands or collapses every output entry.
func (s *Store) setAllFolds(expanded bool) {
	s.mu.Lock()
	for _, e := range s.entries {
		if e.kind == entryOutput {
			e.expanded = expanded
		}
	}
	s.mu.Unlock()
}

func (s *Store) render(width int) (lines []screenLine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	turnSeen := false
	for i, e := range s.entries {
		if e.text.Len() == 0 {
			continue
		}
		if e.kind == entryUser {
			if turnSeen {
				lines = append(lines, turnDivider(width), screenLine{})
			}
			turnSeen = true
		}
		if e.lines == nil || e.renderedLen != e.text.Len() || e.renderedWidth != width || e.renderedExpanded != e.expanded {
			e.lines = renderEntry(e, width)
			e.renderedLen, e.renderedWidth, e.renderedExpanded = e.text.Len(), width, e.expanded
		}
		var fold *entry
		switch {
		case e.kind == entryOutput:
			fold = e
		case e.kind == entryCommand && i+1 < len(s.entries) && s.entries[i+1].kind == entryOutput:
			fold = s.entries[i+1]
		}
		first := len(lines)
		lines = append(lines, e.lines...)
		for j := first; j < len(lines); j++ {
			lines[j].owner, lines[j].fold = e, fold
		}
		if e.kind != entryOutput && len(lines) > first {
			lines[first].startsEntry = true
		}
		// A command draws no trailing blank so its output can hang under it;
		// when nothing will, the blank goes here instead.
		if e.kind == entryCommand && !followedByResult(s.entries, i) {
			lines = append(lines, screenLine{owner: e, fold: fold})
		}
	}
	return lines
}

func followedByResult(entries []*entry, i int) bool {
	if i+1 >= len(entries) {
		return false
	}
	next := entries[i+1]
	return (next.kind == entryOutput || next.kind == entryError) && next.text.Len() > 0
}

func renderEntry(e *entry, width int) []screenLine {
	var lines []screenLine
	text := e.text.String()
	switch e.kind {
	case entryInfo:
		lines = wrapPlainText(text, width, colorInfo, 0, nil, nil)
	case entryUser:
		marker := styledCells("› ", colorUser|attrBold, 0)
		lines = wrapPlainText(text, width, 0, 0, marker, styledCells("  ", 0, 0))
	case entryReply:
		lines = renderMarkdown(text, width)
	case entryCommand:
		// No trailing blank: the command sits directly above its output.
		prompt := styledCells("$ ", colorUser, 0)
		return wrapPlainText(text, width, colorShellCommand, 0, prompt, styledCells("  ", 0, 0))
	case entryOutput:
		lines = renderOutput(e, text, width)
	case entryError:
		lines = wrapPlainText(text, width, colorError, 0, nil, nil)
	}
	return append(lines, screenLine{})
}

// turnDivider is a short, centred hairline: a pause between turns rather
// than a wall. Its cells count as prefix, so yanks skip it.
func turnDivider(width int) screenLine {
	length := min(width, max(3, width/6))
	cells := styledCells(strings.Repeat(" ", (width-length)/2)+strings.Repeat("─", length), colorFaint, 0)
	return screenLine{cells: cells, prefixLen: len(cells), decoration: true}
}

// renderOutput draws tool output. Expanded, it is all there behind a gutter.
// Collapsed, it is dimmer and indented under the command, and only its last
// screen rows show, after a faint count of what's hidden.
func renderOutput(e *entry, text string, width int) []screenLine {
	text = strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), " \t\r\n")
	if text == "" {
		return nil
	}
	if e.expanded {
		return wrapPlainText(text, width, colorShellOutput, 0, gutter(), gutter())
	}
	lineCount := strings.Count(text, "\n") + 1
	if e.hideAll {
		return []screenLine{foldMarker("··· "+plural(lineCount, "line"), width)}
	}
	indent := styledCells("  ", 0, 0)
	rows := wrapPlainText(text, width, colorFolded, 0, indent, indent)
	// Folding a single row away saves nothing: the marker takes its place.
	if len(rows) <= foldedTailRows+1 {
		return rows
	}
	hidden, visible := rows[:len(rows)-foldedTailRows], rows[len(rows)-foldedTailRows:]
	whole := 0
	for _, row := range hidden {
		if row.breakBefore == breakHard {
			whole++
		}
	}
	label := "··· " + plural(whole, "line") + " above"
	if visible[0].breakBefore != breakHard {
		// The first visible row continues a line that starts above.
		whole--
		if whole == 0 {
			label = "··· part of 1 line above"
		} else {
			label = "··· " + plural(whole, "line") + " and part of 1 above"
		}
	}
	return append([]screenLine{foldMarker(label, width)}, visible...)
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// foldMarker counts hidden lines. It is a decoration, so yanks skip it.
func foldMarker(label string, width int) screenLine {
	cells := styledCells("  "+label, colorFaint, 0)
	for cellsWidth(cells) > width {
		cells = cells[:len(cells)-1]
	}
	return screenLine{cells: cells, prefixLen: len(cells), decoration: true}
}
