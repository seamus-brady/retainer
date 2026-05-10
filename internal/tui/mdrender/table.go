package mdrender

import (
	"fmt"
	"strings"

	"github.com/mattn/go-runewidth"
	"github.com/yuin/goldmark/ast"
	extast "github.com/yuin/goldmark/extension/ast"
)

// minUsableColumnWidth is the floor below which proportional column
// shrinking gives up on tabular layout and switches to records
// mode. Six visual columns is the sweet spot — wide enough to show
// short identifiers and one ellipsis-truncated content cell;
// narrower than this produces unreadable column headers like `Add…`
// and `Pri…`.
const minUsableColumnWidth = 6

// renderExtensionBlock dispatches block-level extension nodes that
// goldmark emits via its extension package. We check by reflect-
// free type assertion against the table-extension types.
//
// Returns true when the node was handled (the dispatcher should
// not fall through to the unknown-block path).
func renderExtensionBlock(ctx *renderCtx, node ast.Node) bool {
	switch n := node.(type) {
	case *extast.Table:
		renderTable(ctx, n)
		return true
	}
	return false
}

// renderExtensionInline handles inline extensions (Strikethrough
// today). Returns true when the node was handled.
func renderExtensionInline(ctx *renderCtx, node ast.Node, b *strings.Builder) bool {
	switch node.(type) {
	case *extast.Strikethrough:
		// Most terminals lack strikethrough; render with a leading
		// `~~` cue + ANSI strikethrough (SGR 9) for the ones that do.
		b.WriteString("\x1b[9m")
		walkInline(ctx, node, b)
		b.WriteString("\x1b[29m")
		return true
	}
	return false
}

// renderTable renders a goldmark Table AST node. Tables always come
// out in a code-block-style frame so the alignment we compute is
// what the operator sees — no terminal re-flow inside.
//
// Two layouts:
//
//   - Tabular: traditional `| col | col |` rows, proportional
//     column shrinking when natural width exceeds the viewport.
//   - Records: each row becomes a labeled paragraph
//     (`[N] HeaderValue\n    Field2: …\n`). Used when proportional
//     shrinking would push columns below minUsableColumnWidth.
//
// Both layouts wrap the output in a `│ ` left-border prefix so the
// table reads as a code-block-equivalent piece of content.
func renderTable(ctx *renderCtx, table *extast.Table) {
	header, body := extractTableRows(ctx, table)
	if len(header) == 0 {
		return
	}

	colWidths := computeColumnWidths(header, body)
	width := ctx.width
	if width <= 0 {
		width = 0 // disables shrinking
	}

	if shouldUseRecordsMode(colWidths, width) {
		emitFramedBlock(ctx, renderTableRecords(header, body, width))
		return
	}

	if width > 0 {
		colWidths = shrinkColumnsToFit(colWidths, width)
		header = truncateRow(header, colWidths)
		for k := range body {
			body[k] = truncateRow(body[k], colWidths)
		}
	}
	emitFramedBlock(ctx, renderAlignedTable(header, body, colWidths))
}

// extractTableRows pulls the header + body cells out of a Table
// AST. Cells are inline-rendered (so emphasis + code spans survive)
// and trimmed of surrounding whitespace.
func extractTableRows(ctx *renderCtx, table *extast.Table) (header []string, body [][]string) {
	for c := table.FirstChild(); c != nil; c = c.NextSibling() {
		switch row := c.(type) {
		case *extast.TableHeader:
			header = extractRowCells(ctx, row)
		case *extast.TableRow:
			body = append(body, extractRowCells(ctx, row))
		}
	}
	if len(header) == 0 {
		return nil, nil
	}
	// Normalise rows to header column count.
	cols := len(header)
	for i := range body {
		for len(body[i]) < cols {
			body[i] = append(body[i], "")
		}
		if len(body[i]) > cols {
			body[i] = body[i][:cols]
		}
	}
	return header, body
}

// extractRowCells walks a single header / body row, returning each
// cell's plain-text content. We intentionally drop ANSI styling
// from cell content — column-width math needs the visual width, and
// embedded escapes break that. The trade-off is no inline
// emphasis rendering inside table cells; for chat output that's
// nearly always fine.
func extractRowCells(ctx *renderCtx, row ast.Node) []string {
	var cells []string
	for c := row.FirstChild(); c != nil; c = c.NextSibling() {
		cell, ok := c.(*extast.TableCell)
		if !ok {
			continue
		}
		// Use the source-text walker rather than walkInline so we
		// get plain content without ANSI escapes.
		text := plainInlineText(ctx, cell)
		cells = append(cells, strings.TrimSpace(text))
	}
	return cells
}

// plainInlineText is a stripped-down version of inlineText that
// emits no ANSI codes — used inside table cells where styling
// breaks alignment math. Bold / italic / code lose their styling;
// links keep their label only (URL dropped for compactness).
func plainInlineText(ctx *renderCtx, node ast.Node) string {
	var b strings.Builder
	for c := node.FirstChild(); c != nil; c = c.NextSibling() {
		switch n := c.(type) {
		case *ast.Text:
			b.WriteString(string(n.Segment.Value(ctx.src)))
		case *ast.String:
			b.Write(n.Value)
		case *ast.CodeSpan, *ast.Emphasis:
			b.WriteString(plainInlineText(ctx, n))
		case *ast.Link:
			b.WriteString(plainInlineText(ctx, n))
		case *ast.AutoLink:
			b.WriteString(string(n.URL(ctx.src)))
		default:
			// Recurse for unknown wrappers.
			b.WriteString(plainInlineText(ctx, n))
		}
	}
	return b.String()
}

// shouldUseRecordsMode returns true when proportional shrinking
// would push any column below minUsableColumnWidth. At that point
// the tabular layout becomes unreadable and we transpose to records.
func shouldUseRecordsMode(natural []int, maxWidth int) bool {
	if maxWidth <= 0 || len(natural) == 0 {
		return false
	}
	if tableNaturalWidth(natural) <= maxWidth {
		return false
	}
	shrunk := shrinkColumnsToFit(natural, maxWidth)
	for _, w := range shrunk {
		if w < minUsableColumnWidth {
			return true
		}
	}
	return false
}


// isEmptyMarker returns true for cells that look like
// "Not specified" / "n/a" / "—" — content the operator wouldn't
// gain from seeing in records mode. Trims output to what's
// actually informative.
func isEmptyMarker(value string) bool {
	v := strings.ToLower(strings.TrimSpace(value))
	switch v {
	case "", "-", "—", "–", "n/a", "na", "not specified", "none":
		return true
	}
	return false
}

// computeColumnWidths returns the rune-aware max content width per
// column across the header row and every body row.
func computeColumnWidths(header []string, body [][]string) []int {
	widths := make([]int, len(header))
	for i, c := range header {
		widths[i] = runewidth.StringWidth(c)
	}
	for _, row := range body {
		for i, c := range row {
			if i >= len(widths) {
				break
			}
			if w := runewidth.StringWidth(c); w > widths[i] {
				widths[i] = w
			}
		}
	}
	return widths
}

// tableNaturalWidth returns the visual width of a tabular layout
// at given column widths. Layout is `| col | col | col |`:
// 1 (leading pipe) + sum(width + 3) per column.
func tableNaturalWidth(widths []int) int {
	w := 1
	for _, c := range widths {
		w += c + 3
	}
	return w
}

// shrinkColumnsToFit reduces each column proportionally so the
// total table width fits within maxWidth. Each column floors at
// minUsableColumnWidth-1 (the records-mode threshold check kicks
// in before this shrinks below readability).
func shrinkColumnsToFit(widths []int, maxWidth int) []int {
	if len(widths) == 0 || maxWidth <= 0 {
		return widths
	}
	overhead := 1 + 3*len(widths)
	available := maxWidth - overhead
	if available <= 0 {
		out := make([]int, len(widths))
		for i := range out {
			out[i] = 3
		}
		return out
	}
	total := 0
	for _, w := range widths {
		total += w
	}
	if total <= available {
		return widths
	}
	out := make([]int, len(widths))
	for i, w := range widths {
		scaled := int(float64(w) * float64(available) / float64(total))
		if scaled < 3 {
			scaled = 3
		}
		out[i] = scaled
	}
	return out
}

// truncateRow trims each cell to its column's allotted width,
// appending `…` when truncation occurs.
func truncateRow(row []string, widths []int) []string {
	out := make([]string, len(row))
	for i, cell := range row {
		if i >= len(widths) {
			out[i] = cell
			continue
		}
		out[i] = truncateCell(cell, widths[i])
	}
	return out
}

func truncateCell(cell string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	if runewidth.StringWidth(cell) <= maxWidth {
		return cell
	}
	return runewidth.Truncate(cell, maxWidth, "…")
}

// renderTableRecords emits each body row as a labeled paragraph.
// Format:
//
//	[1] <first-column-value>
//	    Header2: Value
//	    Header3: Value
//
// First column doubles as the "title" of the record (typically the
// most identifying field — Address, Name, etc.). Empty cells are
// skipped to keep output compact.
//
// Long values are word-wrapped under a continuation indent that
// aligns with the field's value column.
func renderTableRecords(header []string, body [][]string, maxWidth int) string {
	if len(header) == 0 {
		return ""
	}
	indent := "    "
	labelWidth := 0
	for _, h := range header[1:] {
		if w := runewidth.StringWidth(h); w > labelWidth {
			labelWidth = w
		}
	}
	valueIndent := indent + strings.Repeat(" ", labelWidth+2) // "Label: "

	var b strings.Builder
	for i, row := range body {
		if i > 0 {
			b.WriteString("\n")
		}
		title := ""
		if len(row) > 0 {
			title = row[0]
		}
		fmt.Fprintf(&b, "[%d] %s\n", i+1, title)
		for j := 1; j < len(header); j++ {
			label := header[j]
			value := ""
			if j < len(row) {
				value = row[j]
			}
			if strings.TrimSpace(value) == "" || isEmptyMarker(value) {
				continue
			}
			pad := strings.Repeat(" ", labelWidth-runewidth.StringWidth(label))
			fmt.Fprintf(&b, "%s%s%s: ", indent, label, pad)
			wrapWidth := maxWidth
			if wrapWidth <= 0 {
				wrapWidth = 9999
			}
			wrapped := wrapTextWithIndent(value, wrapWidth, valueIndent)
			lines := strings.SplitN(wrapped, "\n", 2)
			b.WriteString(strings.TrimPrefix(lines[0], valueIndent))
			b.WriteString("\n")
			if len(lines) == 2 {
				b.WriteString(lines[1])
				b.WriteString("\n")
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderAlignedTable produces the tabular form: `| col | col |` rows
// at the given column widths.
func renderAlignedTable(header []string, body [][]string, widths []int) string {
	var b strings.Builder
	b.WriteString(renderAlignedRow(header, widths))
	b.WriteByte('\n')
	b.WriteString(renderSeparator(widths))
	for _, row := range body {
		b.WriteByte('\n')
		b.WriteString(renderAlignedRow(row, widths))
	}
	return b.String()
}

func renderAlignedRow(row []string, widths []int) string {
	var b strings.Builder
	b.WriteByte('|')
	for i, cell := range row {
		if i >= len(widths) {
			break
		}
		b.WriteByte(' ')
		b.WriteString(cell)
		gap := widths[i] - runewidth.StringWidth(cell)
		for k := 0; k < gap; k++ {
			b.WriteByte(' ')
		}
		b.WriteString(" |")
	}
	return b.String()
}

func renderSeparator(widths []int) string {
	var b strings.Builder
	b.WriteByte('|')
	for _, w := range widths {
		b.WriteByte('-')
		for k := 0; k < w; k++ {
			b.WriteByte('-')
		}
		b.WriteString("-|")
	}
	return b.String()
}

// emitFramedBlock writes a multi-line content block with a dim
// `│ ` left border per line — same convention as code blocks +
// blockquotes so tables visually group with other "preformatted"
// content.
func emitFramedBlock(ctx *renderCtx, content string) {
	for _, line := range strings.Split(content, "\n") {
		ctx.out.WriteString(ansiDim)
		ctx.out.WriteString("│ ")
		ctx.out.WriteString(ansiDimReset)
		ctx.out.WriteString(line)
		ctx.out.WriteString("\n")
	}
}
