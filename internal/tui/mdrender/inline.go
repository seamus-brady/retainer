package mdrender

import (
	"fmt"
	"strings"

	"github.com/yuin/goldmark/ast"
)

// inlineText walks the inline children of a block node and returns
// the rendered text with ANSI styling applied. Used by every block
// renderer that emits inline content (paragraphs, headings, list
// items, table cells, blockquotes).
func inlineText(ctx *renderCtx, node ast.Node) string {
	var b strings.Builder
	walkInline(ctx, node, &b)
	return b.String()
}

// walkInline recurses through inline children of `parent`, appending
// styled text to `b`. Each inline node type emits a small wrapper of
// ANSI escapes around its content; nested emphasis composes
// correctly because the escapes balance with their close codes.
func walkInline(ctx *renderCtx, parent ast.Node, b *strings.Builder) {
	for c := parent.FirstChild(); c != nil; c = c.NextSibling() {
		switch n := c.(type) {
		case *ast.Text:
			b.WriteString(string(n.Segment.Value(ctx.src)))
			if n.HardLineBreak() {
				b.WriteString("\n")
			} else if n.SoftLineBreak() {
				// Soft breaks become spaces so the wrapper can
				// reflow them. Markdown's "two-trailing-spaces" hard
				// break is preserved separately.
				b.WriteString(" ")
			}
		case *ast.String:
			b.Write(n.Value)
		case *ast.CodeSpan:
			b.WriteString(ansiCode)
			walkInline(ctx, n, b)
			b.WriteString(ansiReset)
		case *ast.Emphasis:
			// Level 1 = italic, level 2 = bold (markdown convention).
			if n.Level >= 2 {
				b.WriteString(ansiBold)
				walkInline(ctx, n, b)
				b.WriteString(ansiBoldReset)
			} else {
				b.WriteString(ansiItalic)
				walkInline(ctx, n, b)
				b.WriteString(ansiItalicReset)
			}
		case *ast.Link:
			label := inlineText(ctx, n)
			url := string(n.Destination)
			// `text (url)` form. Underline the label when terminals
			// support it; URL stays plain so it's copyable.
			if label == url || label == "" {
				fmt.Fprintf(b, "%s%s%s", ansiUnderline, url, ansiUnderlineReset)
			} else {
				fmt.Fprintf(b, "%s%s%s (%s)", ansiUnderline, label, ansiUnderlineReset, url)
			}
		case *ast.AutoLink:
			url := string(n.URL(ctx.src))
			fmt.Fprintf(b, "%s%s%s", ansiUnderline, url, ansiUnderlineReset)
		case *ast.Image:
			// Terminals don't render images. Emit the alt text + URL
			// so the operator at least sees what was referenced.
			alt := inlineText(ctx, n)
			url := string(n.Destination)
			if alt != "" {
				fmt.Fprintf(b, "[image: %s] (%s)", alt, url)
			} else {
				fmt.Fprintf(b, "[image] (%s)", url)
			}
		case *ast.RawHTML:
			// Most chat output won't have raw HTML; if it does, drop
			// the tags rather than letting them through as literal
			// text. Goldmark stores the segments per-tag; we ignore
			// open/close tags entirely.
		default:
			// Goldmark extensions (Strikethrough lives here) carry
			// their own node types; check by name to avoid the
			// extension/ast import dependency leaking up.
			if !renderExtensionInline(ctx, c, b) {
				// Fallback: recurse into the unknown node so any
				// nested text leaks through.
				walkInline(ctx, c, b)
			}
		}
	}
}
