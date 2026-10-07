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
	// ... the markers are gone, bold text is bold and code has the code colour
	if len(lines) != 1 || lineText(lines[0]) != "bold and code" {
		t.Fatalf("lines %q", lineText(lines[0]))
	}
	if lines[0].cells[0].fg&attrBold == 0 || lines[0].cells[9].fg.color() != colorInlineCode.color() {
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

func TestFencedCodeSitsBehindAGutterThatYanksSkip(t *testing.T) {
	// given
	// ... a fenced code block
	text := "```go\nx := 1\n```"

	// when
	// ... it is rendered
	lines := renderMarkdown(text, 80)

	// then
	// ... the fences are dropped, the row has a gutter and no shading, and a yank gets only the code
	if len(lines) != 1 || lineText(lines[0]) != "│ x := 1" || lines[0].fill != 0 {
		t.Fatalf("lines %+v", lines)
	}
	if got := string(yankText(lines, position{0, 0}, position{0, endOfLine}, true)); got != "x := 1" {
		t.Errorf("yanked %q", got)
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
	if len(lines) != 2 || lineText(lines[0]) != "· one two" || lineText(lines[1]) != "  three" {
		t.Errorf("lines %q", []string{lineText(lines[0]), lineText(lines[len(lines)-1])})
	}
}

func TestYankKeepsBlankLinesInsideCodeBlocks(t *testing.T) {
	// given
	// ... a fenced code block with a blank line, whose blank row is all gutter
	lines := renderMarkdown("```\na\n\nb\n```", 40)

	// when
	// ... the whole block is yanked
	got := string(yankText(lines, position{0, 0}, position{len(lines) - 1, endOfLine}, true))

	// then
	// ... the blank line survives
	if got != "a\n\nb" {
		t.Errorf("yanked %q", got)
	}
}
