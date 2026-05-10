// Package mdrender is Retainer's own markdown-to-terminal
// renderer. It replaces the previous use of charmbracelet/glamour,
// whose table renderer produced unreadable output at typical chat
// viewport widths.
//
// We parse markdown with goldmark (industry-standard CommonMark +
// table extension) and walk the AST ourselves, emitting ANSI-styled
// text for each node type. Tables get our own column-aligned
// renderer with a records-mode fallback when the natural width
// exceeds the viewport. Everything else (headings, paragraphs,
// lists, code blocks, blockquotes, inline emphasis / code / links)
// is rendered with conservative ANSI styling — bold + dim + italic
// where the terminal can show them, plain text otherwise.
//
// The package is deliberately focused on what shows up in chat
// replies: no images, no HTML pass-through, no syntax highlighting.
// Add features when chat output starts demanding them, not before.
package mdrender

import (
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
)

// Renderer renders markdown to ANSI-styled terminal text. Width
// bounds the wrap target for paragraphs, list items, and the
// table renderer. A renderer is cheap to construct; build a fresh
// one when the viewport width changes.
type Renderer struct {
	width int
	md    goldmark.Markdown
}

// New constructs a Renderer at the given visual width. Width ≤ 0
// disables wrapping (useful for tests that want to inspect raw
// output without re-flow).
func New(width int) *Renderer {
	md := goldmark.New(
		goldmark.WithExtensions(extension.Table, extension.Strikethrough),
	)
	return &Renderer{width: width, md: md}
}

// Render parses the input as markdown and returns ANSI-styled
// terminal output. Errors during parsing return the raw input —
// goldmark is permissive enough that genuine parse failures are
// rare; falling back to plain text is better than dropping the
// content entirely.
func (r *Renderer) Render(input string) string {
	if strings.TrimSpace(input) == "" {
		return input
	}
	src := []byte(input)
	root := r.md.Parser().Parse(text.NewReader(src))
	if root == nil {
		return input
	}

	var out strings.Builder
	ctx := &renderCtx{
		src:   src,
		width: r.width,
		out:   &out,
	}
	renderBlock(ctx, root)

	// Trim trailing blank lines — goldmark's document renderer often
	// finishes with a paragraph break we don't need.
	return strings.TrimRight(out.String(), "\n") + "\n"
}

// renderCtx carries the rendering state down through the AST walk.
// Block renderers append directly to `out`; inline renderers return
// a styled string the block renderer wraps and emits.
type renderCtx struct {
	src   []byte // original source — referenced by inline Text nodes
	width int
	out   *strings.Builder
	// listDepth is incremented inside nested lists so item rendering
	// can pick the right indent + marker.
	listDepth int
	// blockquoteDepth is incremented inside nested blockquotes for
	// multiple `│` prefixes per line.
	blockquoteDepth int
}

// renderBlock dispatches on block-level node type. Document is the
// root; everything else hands off to the per-type renderer.
func renderBlock(ctx *renderCtx, node ast.Node) {
	switch n := node.(type) {
	case *ast.Document:
		renderChildren(ctx, n, blockSeparatorBetween)
	case *ast.Heading:
		renderHeading(ctx, n)
	case *ast.Paragraph:
		renderParagraph(ctx, n)
	case *ast.List:
		renderList(ctx, n)
	case *ast.ListItem:
		// ListItems are handled by their parent List — skipped here
		// to avoid double-rendering.
	case *ast.FencedCodeBlock:
		renderFencedCodeBlock(ctx, n)
	case *ast.CodeBlock:
		renderIndentedCodeBlock(ctx, n)
	case *ast.Blockquote:
		renderBlockquote(ctx, n)
	case *ast.ThematicBreak:
		renderThematicBreak(ctx)
	default:
		// Fall through for table extension nodes + anything we
		// don't model explicitly. Tables are handled in table.go's
		// renderTable; the dispatcher there checks the node kind by
		// name to avoid an import cycle on extension/ast.
		if !renderExtensionBlock(ctx, node) {
			// Unknown block — render its inline children verbatim
			// so we don't drop content.
			text := inlineText(ctx, node)
			if text != "" {
				ctx.out.WriteString(text)
				ctx.out.WriteString("\n")
			}
		}
	}
}

// renderChildren walks each child of node, calling renderBlock and
// inserting the configured separator between them. The separator is
// what produces the visual paragraph break between blocks; intra-
// block content is appended without separator.
func renderChildren(ctx *renderCtx, node ast.Node, separator string) {
	first := true
	for c := node.FirstChild(); c != nil; c = c.NextSibling() {
		if !first {
			ctx.out.WriteString(separator)
		}
		first = false
		renderBlock(ctx, c)
	}
}

// blockSeparatorBetween is the gap between two block-level nodes —
// a single blank line. Markdown convention is one blank line between
// paragraphs/blocks; matches what users expect in chat.
const blockSeparatorBetween = "\n"
