package tui

import (
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// attr is a cell's colour and text attributes: an xterm 256-colour index in
// the low byte (0 is the terminal default) plus attribute bits.
type attr uint16

const (
	attrBold      attr = 0x0100
	attrUnderline attr = 0x0200
	attrItalic    attr = 0x0800
)

func (a attr) color() uint8 { return uint8(a & 0xff) }

// Palette: xterm 256-colour indices. Dark ink and moss: warm greys on the
// terminal's own background, one muted green accent, colour kept for meaning.
const (
	colorInfo         attr = 243 // stone grey
	colorUser         attr = 108 // moss
	colorError        attr = 167 // muted vermilion
	colorBusy         attr = 180 // sand
	colorDim          attr = 243
	colorFaint        attr = 238 // hairlines and gutters
	colorModeInsert   attr = 108 // moss
	colorModeNormal   attr = 109 // slate
	colorModeView     attr = 139 // wisteria
	colorMatchText    attr = 234
	colorMatchBg      attr = 144
	colorSelectionBg  attr = 237
	colorHeading1     attr = 254
	colorHeading2     attr = 251
	colorHeading3     attr = 247
	colorBullet       attr = 108
	colorQuote        attr = 246
	colorRule         attr = 238
	colorLink         attr = 109
	colorInlineCode   attr = 180
	colorCode         attr = 250
	colorShellCommand attr = 252
	colorShellOutput  attr = 245
)

// gutter marks code and tool output with a hairline instead of a shaded block.
func gutter() []cell { return styledCells("│ ", colorFaint, 0) }

type cell struct {
	char   rune
	fg, bg attr
}

type lineBreak int

const (
	breakHard lineBreak = iota
	breakAtSpace
	breakInWord
)

type screenLine struct {
	cells       []cell
	fill        attr
	prefixLen   int
	breakBefore lineBreak
	startsEntry bool
}

type lineWrapper struct {
	lines   []screenLine
	width   int
	indent  []cell
	fill    attr
	current screenLine
	column  int
}

func newLineWrapper(width int, prefix, indent []cell, fill attr) *lineWrapper {
	if width <= len(indent) {
		panic("width must leave room after the indent")
	}
	w := &lineWrapper{width: width, indent: indent, fill: fill}
	w.startLine(prefix, breakHard)
	return w
}

func (w *lineWrapper) startLine(prefix []cell, breakBefore lineBreak) {
	w.current = screenLine{
		cells:       append([]cell(nil), prefix...),
		fill:        w.fill,
		prefixLen:   len(prefix),
		breakBefore: breakBefore,
	}
	w.column = 0
	for _, c := range prefix {
		w.column += runeWidth(c.char)
	}
}

func (w *lineWrapper) write(char rune, fg, bg attr) {
	width := runeWidth(char)
	if width <= 0 {
		return
	}
	if w.column+width > w.width {
		if char == ' ' {
			w.breakLine(breakAtSpace)
			return
		}
		w.breakAtLastSpace()
	}
	w.current.cells = append(w.current.cells, cell{char, fg, bg})
	w.column += width
}

func (w *lineWrapper) breakLine(kind lineBreak) {
	w.lines = append(w.lines, w.current)
	w.startLine(w.indent, kind)
}

func (w *lineWrapper) breakAtLastSpace() {
	cells := w.current.cells
	space := -1
	for i := len(cells) - 1; i > w.current.prefixLen; i-- {
		if cells[i].char == ' ' {
			space = i
			break
		}
	}
	if space < 0 {
		w.breakLine(breakInWord)
		return
	}
	carried := append([]cell(nil), cells[space+1:]...)
	w.current.cells = cells[:space]
	w.breakLine(breakAtSpace)
	for _, c := range carried {
		w.current.cells = append(w.current.cells, c)
		w.column += runeWidth(c.char)
	}
}

func (w *lineWrapper) finish() []screenLine {
	return append(w.lines, w.current)
}

func runeWidth(char rune) int {
	if char >= 0x20 && char < 0x7f {
		return 1
	}
	return ansi.StringWidth(string(char))
}

func styledCells(s string, fg, bg attr) []cell {
	cells := make([]cell, 0, len(s))
	for _, char := range s {
		cells = append(cells, cell{char, fg, bg})
	}
	return cells
}

func wrapPlainText(text string, width int, fg, bg attr, prefix, indent []cell) []screenLine {
	w := newLineWrapper(width, prefix, indent, bg)
	const (
		outsideEscape = iota
		afterEsc
		insideCSI
	)
	escape := outsideEscape
	for _, char := range text {
		switch escape {
		case afterEsc:
			if char == '[' {
				escape = insideCSI
			} else {
				escape = outsideEscape
			}
			continue
		case insideCSI:
			if char >= 0x40 && char <= 0x7e {
				escape = outsideEscape
			}
			continue
		}
		switch {
		case char == 0x1b:
			escape = afterEsc
			continue
		case char == '\n':
			w.breakLine(breakHard)
			continue
		case char == '\t':
			char = ' '
		case char < 0x20 || char == 0x7f:
			continue
		}
		w.write(char, fg, bg)
	}
	return w.finish()
}

func renderMarkdown(text string, width int) []screenLine {
	var lines []screenLine
	inCodeBlock := false
	for rawLine := range strings.Lines(text) {
		line := strings.TrimRight(strings.TrimSuffix(rawLine, "\n"), "\r")
		trimmed := strings.TrimLeft(line, " \t\v\f\r")
		indent := strings.Repeat(" ", len(line)-len(trimmed))

		if strings.HasPrefix(trimmed, "```") {
			inCodeBlock = !inCodeBlock
			continue
		}
		if inCodeBlock {
			lines = append(
				lines,
				wrapPlainText(line, width, colorCode, 0, gutter(), gutter())...)
			continue
		}
		if trimmed == "" {
			lines = append(lines, screenLine{})
			continue
		}
		if isRule(trimmed) {
			lines = append(
				lines,
				screenLine{cells: styledCells(strings.Repeat("─", width), colorRule, 0)},
			)
			continue
		}
		if level := headingLevel(trimmed); level > 0 {
			style := attrBold | colorHeading3
			switch level {
			case 1:
				style = attrBold | colorHeading1
			case 2:
				style = attrBold | colorHeading2
			}
			lines = append(
				lines,
				renderInlineMarkdown(trimmed[level+1:], width, style, nil, nil)...)
			continue
		}
		if strings.HasPrefix(trimmed, ">") {
			body := strings.TrimLeft(trimmed[1:], " \t\v\f\r")
			bar := styledCells("│ ", colorFaint, 0)
			lines = append(
				lines,
				renderInlineMarkdown(body, width, colorQuote|attrItalic, bar, bar)...)
			continue
		}
		if isBullet(trimmed) {
			marker := styledCells(indent+"· ", colorBullet, 0)
			hangingIndent := styledCells(indent+"  ", 0, 0)
			lines = append(
				lines,
				renderInlineMarkdown(trimmed[2:], width, 0, marker, hangingIndent)...)
			continue
		}
		if markerLen := numberedMarkerLen(trimmed); markerLen > 0 {
			marker := styledCells(indent+trimmed[:markerLen], colorBullet, 0)
			hangingIndent := styledCells(indent+strings.Repeat(" ", markerLen), 0, 0)
			lines = append(
				lines,
				renderInlineMarkdown(trimmed[markerLen:], width, 0, marker, hangingIndent)...)
			continue
		}
		lines = append(lines, renderInlineMarkdown(line, width, 0, nil, nil)...)
	}
	return lines
}

func renderInlineMarkdown(s string, width int, base attr, prefix, indent []cell) []screenLine {
	w := newLineWrapper(width, prefix, indent, 0)
	bold, italic := false, false
	emphasis := func(style attr) attr {
		if bold {
			style |= attrBold
		}
		if italic {
			style |= attrItalic
		}
		return style
	}
	i := 0
	for i < len(s) {
		rest := s[i:]
		switch {
		case strings.HasPrefix(rest, "**"):
			if bold || opensSpan(s, i+2, "**") {
				bold = !bold
				i += 2
				continue
			}
		case rest[0] == '*':
			if italic || opensSpan(s, i+1, "*") {
				italic = !italic
				i++
				continue
			}
		case rest[0] == '`':
			if end := strings.IndexByte(rest[1:], '`'); end >= 0 {
				for _, char := range rest[1 : 1+end] {
					w.write(char, emphasis(colorInlineCode), 0)
				}
				i += end + 2
				continue
			}
		case rest[0] == '[':
			if textEnd := strings.Index(rest, "]("); textEnd > 0 {
				if urlEnd := strings.IndexByte(rest[textEnd:], ')'); urlEnd >= 0 {
					for _, char := range rest[1:textEnd] {
						w.write(char, colorLink, 0)
					}
					i += textEnd + urlEnd + 1
					continue
				}
			}
		}
		char, size := utf8.DecodeRuneInString(rest)
		w.write(char, emphasis(base), 0)
		i += size
	}
	return w.finish()
}

func opensSpan(s string, after int, marker string) bool {
	if after >= len(s) || s[after] == ' ' {
		return false
	}
	return strings.Contains(s[after:], marker)
}

func headingLevel(s string) int {
	level := 0
	for level < len(s) && level < 6 && s[level] == '#' {
		level++
	}
	if level == 0 || level >= len(s) || s[level] != ' ' {
		return 0
	}
	return level
}

func isBullet(s string) bool {
	return len(s) >= 2 && strings.IndexByte("-*+", s[0]) >= 0 && s[1] == ' '
}

func numberedMarkerLen(s string) int {
	digits := 0
	for digits < len(s) && s[digits] >= '0' && s[digits] <= '9' {
		digits++
	}
	if digits == 0 || digits+1 >= len(s) || s[digits] != '.' || s[digits+1] != ' ' {
		return 0
	}
	return digits + 2
}

func isRule(s string) bool {
	if len(s) < 3 {
		return false
	}
	marker := s[0]
	if marker != '-' && marker != '*' && marker != '_' {
		return false
	}
	for i := range len(s) {
		if s[i] != marker && s[i] != ' ' {
			return false
		}
	}
	return true
}
