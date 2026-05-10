package mdrender

import (
	"fmt"
	"strings"

	"github.com/mattn/go-runewidth"
	"github.com/yuin/goldmark/ast"
)

// renderHeading emits a heading line. We don't have actual font
// sizes in a terminal; bold + a leading marker per level keeps the
// hierarchy visible without going overboard.
//
//	# H1   → "▎ HEADING\n"
//	## H2  → "▎▎ HEADING\n"
//	### H3 → "  HEADING\n" (bold only — markers stop being useful)
func renderHeading(ctx *renderCtx, h *ast.Heading) {
	body := inlineText(ctx, h)
	body = collapseWhitespace(body)

	var prefix string
	switch h.Level {
	case 1:
		prefix = "▎ "
	case 2:
		prefix = "▎▎ "
	default:
		// H3+ uses bold only — fewer markers, but the bold text
		// against surrounding paragraphs is enough cue.
	}
	ctx.out.WriteString(prefix)
	ctx.out.WriteString(ansiBold)
	ctx.out.WriteString(body)
	ctx.out.WriteString(ansiBoldReset)
	ctx.out.WriteString("\n")
}

// renderParagraph emits an inline-rendered paragraph, word-wrapped
// to the renderer's width. Soft line breaks inside the source
// become spaces (handled in walkInline) so the wrapper can re-flow.
func renderParagraph(ctx *renderCtx, p *ast.Paragraph) {
	body := inlineText(ctx, p)
	body = wrapText(body, ctx.width)
	ctx.out.WriteString(body)
	ctx.out.WriteString("\n")
}

// renderList walks a list's children (each a *ast.ListItem) and
// emits one marker-prefixed item per line. Nested lists indent.
func renderList(ctx *renderCtx, list *ast.List) {
	ctx.listDepth++
	defer func() { ctx.listDepth-- }()

	indent := strings.Repeat("  ", ctx.listDepth-1)
	itemNum := 0
	for c := list.FirstChild(); c != nil; c = c.NextSibling() {
		item, ok := c.(*ast.ListItem)
		if !ok {
			continue
		}
		itemNum++
		marker := "• "
		if list.IsOrdered() {
			marker = fmt.Sprintf("%d. ", itemNum)
		}
		renderListItem(ctx, item, indent, marker)
	}
}

// renderListItem emits a single list item with marker + indent.
// First-line content goes inline; subsequent block content (nested
// lists, paragraphs) renders below at one more indent level.
func renderListItem(ctx *renderCtx, item *ast.ListItem, indent, marker string) {
	prefix := indent + marker
	continuation := indent + strings.Repeat(" ", runewidth.StringWidth(marker))
	for c := item.FirstChild(); c != nil; c = c.NextSibling() {
		switch n := c.(type) {
		case *ast.Paragraph, *ast.TextBlock:
			body := inlineText(ctx, n)
			body = wrapTextWithIndent(body, ctx.width, continuation)
			// First line of the body replaces the continuation
			// indent with the marker prefix.
			ctx.out.WriteString(prefix)
			if strings.HasPrefix(body, continuation) {
				ctx.out.WriteString(body[len(continuation):])
			} else {
				ctx.out.WriteString(body)
			}
			ctx.out.WriteString("\n")
		case *ast.List:
			// Nested list — recurse. The list renderer adds its
			// own depth-based indent.
			renderBlock(ctx, n)
		default:
			// Unrecognised — emit verbatim from the inline walker.
			body := inlineText(ctx, n)
			ctx.out.WriteString(prefix)
			ctx.out.WriteString(body)
			ctx.out.WriteString("\n")
		}
		// After the first child, marker becomes blank so subsequent
		// lines line up under the content.
		prefix = continuation
	}
}

// renderFencedCodeBlock emits a fenced code block as plain
// monospace text framed by a left border. Glamour used a colored
// background; we keep it simpler with a `│` left edge so embedded
// content (e.g. our table renderer's pre-rendered table) stays
// scannable.
func renderFencedCodeBlock(ctx *renderCtx, b *ast.FencedCodeBlock) {
	body := codeBlockBody(ctx, b)
	emitCodeBlock(ctx, body)
}

// renderIndentedCodeBlock handles 4-space indented code blocks
// (less common in chat output but still valid markdown). Same
// rendering as fenced blocks.
func renderIndentedCodeBlock(ctx *renderCtx, b *ast.CodeBlock) {
	body := codeBlockBody(ctx, b)
	emitCodeBlock(ctx, body)
}

// codeBlockBody concatenates the code block's raw lines into a
// single string. Goldmark exposes the literal source per line.
func codeBlockBody(ctx *renderCtx, b ast.Node) string {
	var sb strings.Builder
	lines := b.Lines()
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		sb.Write(seg.Value(ctx.src))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// emitCodeBlock writes the body with a `│ ` left border per line.
// Mirrors the convention of blockquotes but with dim styling so the
// border reads as decoration rather than text.
func emitCodeBlock(ctx *renderCtx, body string) {
	for _, line := range strings.Split(body, "\n") {
		ctx.out.WriteString(ansiDim)
		ctx.out.WriteString("│ ")
		ctx.out.WriteString(ansiDimReset)
		ctx.out.WriteString(line)
		ctx.out.WriteString("\n")
	}
}

// renderBlockquote prefixes each line of the contained block(s)
// with a dim `│ `. Nested blockquotes get one prefix per level.
func renderBlockquote(ctx *renderCtx, bq *ast.Blockquote) {
	ctx.blockquoteDepth++
	defer func() { ctx.blockquoteDepth-- }()

	// Render children into a side buffer so we can prefix each
	// rendered line. Children might be paragraphs, lists, nested
	// blockquotes — the prefix wraps whatever they emit.
	side := &strings.Builder{}
	saved := ctx.out
	ctx.out = side
	renderChildren(ctx, bq, blockSeparatorBetween)
	ctx.out = saved

	prefix := strings.Repeat(ansiDim+"│ "+ansiDimReset, ctx.blockquoteDepth)
	for _, line := range strings.Split(strings.TrimRight(side.String(), "\n"), "\n") {
		ctx.out.WriteString(prefix)
		ctx.out.WriteString(line)
		ctx.out.WriteString("\n")
	}
}

// renderThematicBreak emits a horizontal rule scaled to width.
func renderThematicBreak(ctx *renderCtx) {
	w := ctx.width
	if w <= 0 || w > 80 {
		w = 60
	}
	ctx.out.WriteString(ansiDim)
	ctx.out.WriteString(strings.Repeat("─", w))
	ctx.out.WriteString(ansiDimReset)
	ctx.out.WriteString("\n")
}
