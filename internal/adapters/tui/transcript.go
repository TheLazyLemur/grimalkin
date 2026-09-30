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
	for _, e := range s.entries {
		if e.text.Len() == 0 {
			continue
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
		style := colorUser | attrBold
		lines = wrapPlainText(text, width, style, 0, styledCells("› ", style, 0), styledCells("  ", 0, 0))
	case entryReply:
		lines = renderMarkdown(text, width)
	case entryCommand:
		style := colorShellCommand | attrBold
		lines = append(lines, screenLine{fill: colorShellBg})
		lines = append(lines, wrapPlainText(text, width, style, colorShellBg, styledCells(" $ ", style, colorShellBg), styledCells("   ", 0, colorShellBg))...)
		return lines
	case entryOutput:
		padding := styledCells("   ", 0, colorShellBg)
		lines = wrapPlainText(text, width, colorShellOutput, colorShellBg, padding, padding)
		lines = append(lines, screenLine{fill: colorShellBg})
	case entryError:
		lines = wrapPlainText(text, width, colorError, 0, nil, nil)
	}
	return append(lines, screenLine{})
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
