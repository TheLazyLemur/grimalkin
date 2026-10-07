package tui

import (
	"fmt"
	"strings"
	"sync"

	"grimalkin/internal/core"
)

// maxOutputLines caps tool output shown in the transcript. The model gets all
// of it (up to the core's cap).
const maxOutputLines = 20

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

	lines         []screenLine
	renderedLen   int
	renderedWidth int
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
			s.add(entryOutput, firstLines(event.Output, maxOutputLines))
		}
	case core.SkillLoaded:
		s.add(entryCommand, "skill "+event.Name)
		s.add(entryOutput, firstLines(event.Content, maxOutputLines))
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

func (s *Store) render(width int) (lines []screenLine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	turnSeen := false
	for _, e := range s.entries {
		if e.text.Len() == 0 {
			continue
		}
		if e.kind == entryUser {
			if turnSeen {
				lines = append(lines, turnDivider(width), screenLine{})
			}
			turnSeen = true
		}
		if e.lines == nil || e.renderedLen != e.text.Len() || e.renderedWidth != width {
			e.lines, e.renderedLen, e.renderedWidth = renderEntry(e.kind, e.text.String(), width), e.text.Len(), width
		}
		first := len(lines)
		lines = append(lines, e.lines...)
		if e.kind != entryOutput && len(lines) > first {
			lines[first].startsEntry = true
		}
	}
	return lines
}

func renderEntry(kind entryKind, text string, width int) []screenLine {
	var lines []screenLine
	switch kind {
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
		lines = wrapPlainText(text, width, colorShellOutput, 0, gutter(), gutter())
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

func firstLines(s string, count int) string {
	end := 0
	for range count {
		newline := strings.IndexByte(s[end:], '\n')
		if newline < 0 {
			return s
		}
		end += newline + 1
	}
	remaining := strings.Count(s[end:], "\n") + 1
	return fmt.Sprintf("%s… %d more lines", s[:end], remaining)
}
