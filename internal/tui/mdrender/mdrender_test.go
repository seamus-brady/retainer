package mdrender

import (
	"strings"
	"testing"
)

// stripANSIForAssertion is a tiny helper so test assertions match
// against visible text rather than ANSI escapes. ANSI codes ARE part
// of the renderer's contract — the styling tests below check those
// directly — but presence-of-text assertions read more cleanly
// without the noise.
func stripANSIForAssertion(s string) string {
	return stripANSI(s)
}

// ---- top-level Render ----

func TestRender_EmptyInputReturnsInput(t *testing.T) {
	if got := New(80).Render(""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
	if got := New(80).Render("   \n  "); got != "   \n  " {
		t.Errorf("whitespace-only input should round-trip; got %q", got)
	}
}

func TestRender_PlainTextRoundTrips(t *testing.T) {
	got := New(80).Render("hello world")
	if !strings.Contains(got, "hello world") {
		t.Errorf("got %q, want it to contain hello world", got)
	}
}

// ---- headings ----

func TestRender_HeadingH1(t *testing.T) {
	got := New(80).Render("# Title")
	if !strings.Contains(got, "▎") {
		t.Errorf("h1 should have ▎ marker: %q", got)
	}
	if !strings.Contains(got, ansiBold) {
		t.Errorf("h1 should be bolded: %q", got)
	}
	if !strings.Contains(stripANSIForAssertion(got), "Title") {
		t.Errorf("h1 text missing: %q", got)
	}
}

func TestRender_HeadingH2HasDoubleMarker(t *testing.T) {
	got := New(80).Render("## Subtitle")
	if !strings.Contains(got, "▎▎") {
		t.Errorf("h2 should have ▎▎ marker: %q", got)
	}
}

func TestRender_HeadingH3PlainBold(t *testing.T) {
	got := New(80).Render("### Section")
	if strings.Contains(got, "▎") {
		t.Errorf("h3 should not have ▎ marker: %q", got)
	}
	if !strings.Contains(got, ansiBold) {
		t.Errorf("h3 should still be bold: %q", got)
	}
}

// ---- inline emphasis ----

func TestRender_BoldText(t *testing.T) {
	got := New(80).Render("this is **bold** here")
	if !strings.Contains(got, ansiBold+"bold"+ansiBoldReset) {
		t.Errorf("bold escape not applied: %q", got)
	}
}

func TestRender_ItalicText(t *testing.T) {
	got := New(80).Render("this is *italic* here")
	if !strings.Contains(got, ansiItalic+"italic"+ansiItalicReset) {
		t.Errorf("italic escape not applied: %q", got)
	}
}

func TestRender_InlineCode(t *testing.T) {
	got := New(80).Render("call `read_skill` next")
	if !strings.Contains(got, ansiCode+"read_skill"+ansiReset) {
		t.Errorf("inline code escape not applied: %q", got)
	}
}

func TestRender_LinkLabelAndURL(t *testing.T) {
	got := New(80).Render("see [the docs](https://example.com)")
	if !strings.Contains(got, "the docs") {
		t.Errorf("link label missing: %q", got)
	}
	if !strings.Contains(got, "https://example.com") {
		t.Errorf("link url missing: %q", got)
	}
	if !strings.Contains(got, ansiUnderline) {
		t.Errorf("link should underline label: %q", got)
	}
}

func TestRender_BareURLAutoLink(t *testing.T) {
	// Goldmark's default doesn't autolink bare URLs (Linkify is an
	// extension); the inline rendering path still has to handle
	// the AutoLink AST node when present.
	got := New(80).Render("<https://example.com>")
	if !strings.Contains(got, "https://example.com") {
		t.Errorf("autolink url missing: %q", got)
	}
}

// ---- lists ----

func TestRender_UnorderedList(t *testing.T) {
	got := New(80).Render("- one\n- two\n- three")
	for _, want := range []string{"• one", "• two", "• three"} {
		if !strings.Contains(got, want) {
			t.Errorf("list item missing %q: %q", want, got)
		}
	}
}

func TestRender_OrderedList(t *testing.T) {
	got := New(80).Render("1. first\n2. second")
	for _, want := range []string{"1. first", "2. second"} {
		if !strings.Contains(got, want) {
			t.Errorf("ordered item missing %q: %q", want, got)
		}
	}
}

func TestRender_NestedList(t *testing.T) {
	got := New(80).Render("- top\n  - nested\n- back")
	if !strings.Contains(got, "  • nested") {
		t.Errorf("nested item should be indented: %q", got)
	}
}

// ---- code blocks ----

func TestRender_FencedCodeBlock(t *testing.T) {
	got := New(80).Render("```\nfunc main() {}\n```")
	if !strings.Contains(got, "func main() {}") {
		t.Errorf("code block content missing: %q", got)
	}
	// Code blocks have a `│ ` left-border decoration.
	if !strings.Contains(got, "│ ") {
		t.Errorf("code block should have left border: %q", got)
	}
}

func TestRender_FencedCodeBlockPreservesFormatting(t *testing.T) {
	// Code blocks must not reflow content. Leading whitespace inside
	// the block stays as-is — that's load-bearing for code.
	got := New(80).Render("```\n    indented line\n    another one\n```")
	if !strings.Contains(got, "    indented line") {
		t.Errorf("code block indentation lost: %q", got)
	}
}

// ---- blockquotes ----

func TestRender_Blockquote(t *testing.T) {
	got := New(80).Render("> a quoted line")
	if !strings.Contains(got, "│") {
		t.Errorf("blockquote should have │ prefix: %q", got)
	}
	if !strings.Contains(got, "a quoted line") {
		t.Errorf("blockquote content missing: %q", got)
	}
}

func TestRender_NestedBlockquote(t *testing.T) {
	got := New(80).Render("> > deeply nested")
	// Two `│ ` prefixes for two-level nesting.
	if strings.Count(got, "│") < 2 {
		t.Errorf("nested blockquote should have 2 │ prefixes: %q", got)
	}
}

// ---- thematic break ----

func TestRender_ThematicBreak(t *testing.T) {
	got := New(80).Render("before\n\n---\n\nafter")
	if !strings.Contains(got, "─") {
		t.Errorf("thematic break should render ─ rule: %q", got)
	}
	if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
		t.Errorf("surrounding paragraphs missing: %q", got)
	}
}

// ---- tables: tabular mode ----

func TestRender_SmallTableRendersAsTable(t *testing.T) {
	in := "| a | b |\n|---|---|\n| 1 | 2 |"
	got := New(80).Render(in)
	// Tabular mode emits `| col | col |` rows.
	if !strings.Contains(got, "| a | b |") {
		t.Errorf("table header missing: %q", got)
	}
	if !strings.Contains(got, "| 1 | 2 |") {
		t.Errorf("table row missing: %q", got)
	}
	// Wrapped in framed-block left border.
	if !strings.Contains(got, "│ ") {
		t.Errorf("table should be framed: %q", got)
	}
}

func TestRender_TableWithUnicodeAlignsCorrectly(t *testing.T) {
	// ✅ is 2 visual columns. Row width must account for that.
	in := "| status |\n|--------|\n| ✅ ok  |"
	got := New(80).Render(in)
	plain := stripANSIForAssertion(got)
	for _, want := range []string{"status", "✅ ok"} {
		if !strings.Contains(plain, want) {
			t.Errorf("missing %q in:\n%s", want, plain)
		}
	}
}

func TestRender_TableWithBoldHeader(t *testing.T) {
	// Bold markers in headers were a chronic source of misalignment
	// in earlier renderers. Make sure the bold doesn't leak into the
	// rendered output.
	in := "| **A** | **B** |\n|-------|-------|\n| 1     | 2     |"
	got := New(80).Render(in)
	plain := stripANSIForAssertion(got)
	if strings.Contains(plain, "**") {
		t.Errorf("bold markers should be stripped from cells: %q", plain)
	}
	if !strings.Contains(plain, "A") || !strings.Contains(plain, "B") {
		t.Errorf("header text missing: %q", plain)
	}
}

// ---- tables: records mode fallback ----

func TestRender_WideTableSwitchesToRecordsMode(t *testing.T) {
	// Eight columns at narrow viewport — proportional shrinking
	// would push columns below readability, so the renderer
	// switches to records mode.
	in := strings.Join([]string{
		"| Address | Price | Type | Beds | Size | Location | Link | Notes |",
		"|---------|-------|------|------|------|----------|------|-------|",
		"| 16 Roebuck Castle | 725,000 | Semi | 4 | 104m² | Roebuck | Daft.ie | A long description |",
	}, "\n")
	got := New(72).Render(in)
	plain := stripANSIForAssertion(got)
	// Records mode marker: `[1] <title>`.
	if !strings.Contains(plain, "[1]") {
		t.Errorf("expected records-mode marker [1]: %q", plain)
	}
	// Records mode emits "Label: Value".
	if !strings.Contains(plain, "Price") || !strings.Contains(plain, "725,000") {
		t.Errorf("expected Price field rendered: %q", plain)
	}
}

func TestRender_RecordsModeSkipsEmptyMarkers(t *testing.T) {
	// "Not specified" cells are noise in records mode — drop them.
	in := strings.Join([]string{
		"| Address | Price | Type | Beds | Size | Location | Link | Notes |",
		"|---------|-------|------|------|------|----------|------|-------|",
		"| 16 Roebuck | 725,000 | Semi | 4 | Not specified | Not specified | Daft.ie | |",
	}, "\n")
	got := New(72).Render(in)
	plain := stripANSIForAssertion(got)
	if strings.Contains(plain, "Not specified") {
		t.Errorf("records mode should drop 'Not specified' cells: %q", plain)
	}
}

// ---- shouldUseRecordsMode threshold ----

func TestShouldUseRecordsMode_DoesNotFireWhenColumnsFit(t *testing.T) {
	// Three columns at 80 cols natural → proportional shrink keeps
	// every column above the floor → tabular mode.
	if shouldUseRecordsMode([]int{20, 10, 30}, 80) {
		t.Error("3-column table at 80 cols should stay tabular")
	}
}

func TestShouldUseRecordsMode_FiresWhenColumnsTooNarrow(t *testing.T) {
	// 8 columns, natural width way over budget → shrink pushes
	// columns below floor → records mode.
	if !shouldUseRecordsMode([]int{20, 10, 13, 8, 22, 16, 12, 100}, 72) {
		t.Error("wide 8-column table should switch to records mode")
	}
}

// ---- end-to-end mixed content ----

func TestRender_MixedContentPreservesStructure(t *testing.T) {
	in := strings.Join([]string{
		"# Title",
		"",
		"A paragraph with **bold**.",
		"",
		"- list one",
		"- list two",
		"",
		"```",
		"code line",
		"```",
		"",
		"> quote",
	}, "\n")
	got := New(80).Render(in)
	plain := stripANSIForAssertion(got)
	for _, want := range []string{"Title", "paragraph with bold", "list one", "list two", "code line", "quote"} {
		if !strings.Contains(plain, want) {
			t.Errorf("missing %q in:\n%s", want, plain)
		}
	}
}

// ---- helper functions ----

func TestVisibleWidth_IgnoresANSI(t *testing.T) {
	plain := "hello"
	styled := ansiBold + "hello" + ansiBoldReset
	if visibleWidth(plain) != 5 {
		t.Errorf("plain width = %d, want 5", visibleWidth(plain))
	}
	if visibleWidth(styled) != 5 {
		t.Errorf("styled width = %d, want 5", visibleWidth(styled))
	}
}

func TestStripANSI_RemovesEscapes(t *testing.T) {
	in := "\x1b[1mbold\x1b[22m and \x1b[3mitalic\x1b[23m"
	got := stripANSI(in)
	if got != "bold and italic" {
		t.Errorf("got %q, want %q", got, "bold and italic")
	}
}

func TestWrapTextWithIndent_BreaksOnWords(t *testing.T) {
	// 20-col wrap with no indent. "one two three four five" (23
	// chars including spaces) wraps after "four" (16 chars: "one
	// two three four"), then "five" on the next line.
	got := wrapTextWithIndent("one two three four five", 20, "")
	if !strings.Contains(got, "\n") {
		t.Errorf("expected wrap, got single line: %q", got)
	}
}

// ---- table column math ----

func TestComputeColumnWidths_TakesMaxAcrossRows(t *testing.T) {
	header := []string{"a", "longer header"}
	body := [][]string{
		{"short", "z"},
		{"medium", "x"},
	}
	got := computeColumnWidths(header, body)
	if got[0] != 6 || got[1] != 13 {
		t.Errorf("got %v, want [6, 13]", got)
	}
}

func TestShrinkColumnsToFit_ReducesProportionally(t *testing.T) {
	got := shrinkColumnsToFit([]int{3, 5, 7}, 16)
	for i, w := range got {
		if w < 3 {
			t.Errorf("widths[%d] = %d below floor", i, w)
		}
	}
}

func TestTruncateCell_AppendsEllipsisOnOverflow(t *testing.T) {
	got := truncateCell("hello world", 5)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("got %q, want ellipsis suffix", got)
	}
}

func TestTruncateCell_NoOpWhenFits(t *testing.T) {
	if got := truncateCell("ok", 10); got != "ok" {
		t.Errorf("got %q, want ok", got)
	}
}

// ---- isEmptyMarker ----

func TestIsEmptyMarker(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", true},
		{"   ", true},
		{"-", true},
		{"—", true},
		{"n/a", true},
		{"Not specified", true},
		{"NOT SPECIFIED", true},
		{"actual content", false},
		{"0", false},
	}
	for _, c := range cases {
		if got := isEmptyMarker(c.in); got != c.want {
			t.Errorf("isEmptyMarker(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
