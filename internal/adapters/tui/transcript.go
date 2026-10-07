package tui

import (
	"fmt"
	"strings"
	"sync"

	"grimalkin/internal/core"
)

// foldedTailLines is how much of a collapsed tool output stays visible. The
// last lines are kept because builds, tests and greps end with the result.
const foldedTailLines = 4

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
	changed chan struct{}
}

func NewStore() *Store {
	return &Store{changed: make(chan struct{}, 1)}
}

func (s *Store) AddInfo(text string) {
	s.add(entryInfo, text)
	s.notify()
}

func (s *Store) addUser(prompt string) {
	s.add(entryUser, prompt)
	s.notify()
}

func (s *Store) Show(event core.Event) {
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
		s.add(entryCommand, event.Summary)
	case core.ToolFinished:
		if event.IsError {
			s.add(entryError, event.Output)
		} else {
			s.add(entryOutput, event.Output)
		}
	case core.SkillLoaded:
		s.add(entryCommand, "skill "+event.Name)
		s.add(entryOutput, event.Content)
		s.mu.Lock()
		s.entries[len(s.entries)-1].hideAll = true
		s.mu.Unlock()
	case core.TurnDiscarded:
		s.add(entryError, event.Reason+" (turn discarded)")
	default:
		panic(fmt.Sprintf("unhandled event %T", event))
	}
	s.notify()
}

func (s *Store) add(kind entryKind, text string) {
	s.mu.Lock()
	e := &entry{kind: kind}
	e.text.WriteString(text)
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
	}
	return lines
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
	return screenLine{cells: cells, prefixLen: len(cells)}
}

// renderOutput draws tool output. Expanded, it is all there behind a gutter.
// Collapsed, it is dimmer and indented under the command, and only its last
// lines show, after a faint count of what's hidden.
func renderOutput(e *entry, text string, width int) []screenLine {
	text = strings.TrimRight(text, "\n")
	if e.expanded {
		return wrapPlainText(text, width, colorShellOutput, 0, gutter(), gutter())
	}
	all := strings.Split(text, "\n")
	visible := all
	if e.hideAll {
		visible = nil
	} else if len(all) > foldedTailLines {
		visible = all[len(all)-foldedTailLines:]
	}
	var lines []screenLine
	if hidden := len(all) - len(visible); hidden > 0 {
		label := fmt.Sprintf("··· %d lines", hidden)
		if len(visible) > 0 {
			label += " above"
		}
		lines = append(lines, foldMarker(label, width))
	}
	if len(visible) > 0 {
		indent := styledCells("  ", 0, 0)
		lines = append(lines, wrapPlainText(strings.Join(visible, "\n"), width, colorFolded, 0, indent, indent)...)
	}
	return lines
}

// foldMarker counts hidden lines. Its cells count as prefix, so yanks skip it.
func foldMarker(label string, width int) screenLine {
	cells := styledCells("  "+label, colorFaint, 0)
	for cellsWidth(cells) > width {
		cells = cells[:len(cells)-1]
	}
	return screenLine{cells: cells, prefixLen: len(cells)}
}
