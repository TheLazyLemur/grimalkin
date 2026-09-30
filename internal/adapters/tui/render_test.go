package tui

import "testing"

func lineText(line screenLine) string {
	chars := make([]rune, len(line.cells))
	for i, c := range line.cells {
		chars[i] = c.char
	}
	return string(chars)
}

func TestInlineMarkersStyleTextAndAreRemoved(t *testing.T) {
	// given
	// ... a paragraph with bold and inline code
	text := "**bold** and `code`"

	// when
	// ... it is rendered
	lines := renderMarkdown(text, 80)

	// then
	// ... the markers are gone, bold text is bold and code has the code background
	if len(lines) != 1 || lineText(lines[0]) != "bold and code" {
		t.Fatalf("lines %q", lineText(lines[0]))
	}
	if lines[0].cells[0].fg&attrBold == 0 || lines[0].cells[9].bg != colorInlineCodeBg {
		t.Errorf("cells %+v", lines[0].cells)
	}
}

func TestLoneAsterisksStayLiteral(t *testing.T) {
	// given
	// ... arithmetic with spaced asterisks
	text := "2 * 3 * 4"

	// when
	// ... it is rendered
	lines := renderMarkdown(text, 80)

	// then
	// ... the asterisks are kept and nothing is italic
	if lineText(lines[0]) != "2 * 3 * 4" || lines[0].cells[4].fg&attrItalic != 0 {
		t.Errorf("line %q, cells %+v", lineText(lines[0]), lines[0].cells)
	}
}

func TestFencedCodeFillsItsRowsWithTheCodeBackground(t *testing.T) {
	// given
	// ... a fenced code block
	text := "```go\nx := 1\n```"

	// when
	// ... it is rendered
	lines := renderMarkdown(text, 80)

	// then
	// ... the fences are dropped and the code row is indented and filled
	if len(lines) != 1 || lineText(lines[0]) != "  x := 1" || lines[0].fill != colorCodeBg {
		t.Errorf("lines %+v", lines)
	}
}

func TestBulletsWrapAtWordsWithAHangingIndent(t *testing.T) {
	// given
	// ... a bullet longer than the width
	text := "- one two three"

	// when
	// ... it is rendered 9 columns wide
	lines := renderMarkdown(text, 9)

	// then
	// ... it breaks between words and continues under the text, not the bullet
	if len(lines) != 2 || lineText(lines[0]) != "• one two" || lineText(lines[1]) != "  three" {
		t.Errorf("lines %q", []string{lineText(lines[0]), lineText(lines[len(lines)-1])})
	}
}
