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

	// child is the number of the child a status line is about, which gf
	// opens. Zero on every other entry.
	child int

	lines            []screenLine
	renderedLen      int
	renderedWidth    int
	renderedExpanded bool
}

// Store holds one transcript per agent, its buffer, by agent number. The
// parent's is there from the start; a child's from the first event that names
// the child, for the rest of the session.
type Store struct {
	mu      sync.Mutex
	buffers map[int]*buffer
	// children holds each child's latest status line, by child number. The
	// lines themselves are entries in the parent's buffer.
	children map[int]*childStatus
	changed  chan struct{}
}

// parentAgent is the number the core gives the parent.
const parentAgent = 0

// buffer is one agent's transcript.
type buffer struct {
	entries []*entry
	reply   *entry
	// running holds the tool calls that have started and not finished, by
	// call ID, so each result lands under its own command.
	running map[string]runningCall
}

func newBuffer() *buffer {
	return &buffer{running: map[string]runningCall{}}
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
		buffers:  map[int]*buffer{parentAgent: newBuffer()},
		children: map[int]*childStatus{},
	}
}

// hasBuffer reports whether an agent has a buffer to show.
func (s *Store) hasBuffer(agent int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buffers[agent] != nil
}

// buffer is an agent's buffer, which must exist. The caller holds the lock.
func (s *Store) buffer(agent int) *buffer {
	b := s.buffers[agent]
	if b == nil {
		panic(fmt.Sprintf("agent %d has no buffer", agent))
	}
	return b
}

// AddInfo adds a line of grimalkin's own to the parent's buffer.
func (s *Store) AddInfo(text string) {
	s.addInfo(parentAgent, text)
}

// addInfo adds a line of grimalkin's own to an agent's buffer.
func (s *Store) addInfo(agent int, text string) {
	s.add(agent, &entry{kind: entryInfo}, text)
	s.notify()
}

// addUser adds what the user sent an agent to its buffer.
func (s *Store) addUser(agent int, prompt string) {
	s.add(agent, &entry{kind: entryUser}, prompt)
	s.notify()
}

// Show puts a turn event in its agent's buffer. A child's lifecycle events
// are the parent's: they draw the child's status line in the parent's buffer.
func (s *Store) Show(agent int, event core.Event) {
	switch event := event.(type) {
	case core.ReplyStarted:
		s.startReply(agent)
	case core.ReplyText:
		s.mu.Lock()
		b := s.buffer(agent)
		if b.reply == nil {
			panic("reply text before ReplyStarted")
		}
		b.reply.text.WriteString(event.Text)
		s.mu.Unlock()
	case core.ToolStarted:
		s.startCall(agent, event)
	case core.ToolFinished:
		s.finishCall(agent, event)
	case core.SkillLoaded:
		s.add(agent, &entry{kind: entryCommand}, "skill "+event.Name)
		// Skill content collapses to its line count alone.
		s.add(agent, &entry{kind: entryOutput, hideAll: true}, event.Content)
	case core.TurnDiscarded:
		s.add(agent, &entry{kind: entryError}, event.Reason+" (turn discarded)")
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
func (s *Store) startCall(agent int, event core.ToolStarted) {
	command := &entry{kind: entryCommand}
	command.text.WriteString(event.Summary)
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.buffer(agent)
	if _, ok := b.running[event.ID]; ok {
		panic(fmt.Sprintf("tool call %q started twice", event.ID))
	}
	if event.Name == core.AgentToolName {
		b.running[event.ID] = runningCall{}
		return
	}
	b.entries = append(b.entries, command)
	b.running[event.ID] = runningCall{command: command, skill: event.Name == core.SkillToolName}
}

// finishCall puts a tool call's result under its command rather than at the
// end, since calls run at once finish in any order.
func (s *Store) finishCall(agent int, event core.ToolFinished) {
	result := &entry{kind: entryOutput}
	result.text.WriteString(event.Output)
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.buffer(agent)
	call, ok := b.running[event.ID]
	if !ok {
		panic(fmt.Sprintf("tool call %q finished without starting", event.ID))
	}
	delete(b.running, event.ID)
	if call.command == nil {
		return
	}
	switch {
	case event.IsError:
		result.kind = entryError
	case call.skill:
		result.hideAll = true
	}
	b.entries = slices.Insert(b.entries, slices.Index(b.entries, call.command)+1, result)
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

// childBuffer is a child's buffer as :ls lists it: the child's number, and
// the model and state of its latest run.
type childBuffer struct {
	number       int
	model, state string
}

// childBuffers lists every child's buffer, by number.
func (s *Store) childBuffers() []childBuffer {
	s.mu.Lock()
	defer s.mu.Unlock()
	var list []childBuffer
	for _, child := range s.children {
		list = append(list, childBuffer{child.number, child.model, child.state})
	}
	slices.SortFunc(list, func(a, b childBuffer) int { return a.number - b.number })
	return list
}

// childModel is the model a child runs on, as its lifecycle events name it.
func (s *Store) childModel(number int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	child := s.children[number]
	if child == nil {
		panic(fmt.Sprintf("no child %d", number))
	}
	return child.model
}

// childBusy reports whether a child is waiting or running, going by its
// lifecycle events.
func (s *Store) childBusy(number int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	child := s.children[number]
	return child != nil && !child.ended()
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
// the earlier run stays as it ended. The child's buffer is made here, on the
// first event that names it.
func (s *Store) beginChild(number int, model, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	child := s.children[number]
	switch {
	case child != nil && child.state == childWaiting && state == childRunning:
	case child == nil || child.ended():
		child = &childStatus{line: &entry{kind: entryInfo, child: number}, number: number}
		s.children[number] = child
		parent := s.buffer(parentAgent)
		parent.entries = append(parent.entries, child.line)
		if s.buffers[number] == nil {
			s.buffers[number] = newBuffer()
		}
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

// add puts an entry at the end of an agent's buffer.
func (s *Store) add(agent int, e *entry, text string) {
	e.text.WriteString(text)
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.buffer(agent)
	b.entries = append(b.entries, e)
}

// startReply adds a reply and makes it the one the agent's text goes to, in
// one step: agents reply at the same time.
func (s *Store) startReply(agent int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.buffer(agent)
	b.reply = &entry{kind: entryReply}
	b.entries = append(b.entries, b.reply)
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

// setAllFolds expands or collapses every output entry in an agent's buffer.
func (s *Store) setAllFolds(agent int, expanded bool) {
	s.mu.Lock()
	for _, e := range s.buffer(agent).entries {
		if e.kind == entryOutput {
			e.expanded = expanded
		}
	}
	s.mu.Unlock()
}

// render lays an agent's buffer out as screen lines.
func (s *Store) render(agent int, width int) (lines []screenLine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := s.buffer(agent).entries
	turnSeen := false
	for i, e := range entries {
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
		case e.kind == entryCommand && i+1 < len(entries) && entries[i+1].kind == entryOutput:
			fold = entries[i+1]
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
		if e.kind == entryCommand && !followedByResult(entries, i) {
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
