package mdrender

import (
	"strings"

	"github.com/mattn/go-runewidth"
)

// wrapText word-wraps text to width visual columns. ANSI escape
// sequences embedded in the input are passed through but don't
// count toward visual width — so a paragraph styled with bold
// (`\x1b[1m...\x1b[22m`) wraps based on the visible characters.
//
// width ≤ 0 disables wrapping; the input returns unchanged.
func wrapText(text string, width int) string {
	if width <= 0 {
		return text
	}
	return wrapTextWithIndent(text, width, "")
}

// wrapTextWithIndent wraps text to width with a continuation
// indent applied to every line after the first. The first line
// has no indent — the caller prefixes whatever marker (`- `,
// `[1] `, etc.) it wants.
func wrapTextWithIndent(text string, width int, indent string) string {
	if width <= 0 {
		return text
	}
	indentWidth := runewidth.StringWidth(indent)
	contentWidth := width - indentWidth
	if contentWidth <= 0 {
		contentWidth = 1
	}

	// Split on existing newlines first; each paragraph wraps
	// independently. This preserves explicit line breaks in the
	// source.
	paras := strings.Split(text, "\n")
	var out strings.Builder
	for i, para := range paras {
		if i > 0 {
			out.WriteByte('\n')
		}
		w := contentWidth
		if i == 0 {
			w = width // first line has no indent
		}
		out.WriteString(wrapLine(para, w, indent, i > 0))
	}
	return out.String()
}

// wrapLine wraps a single line of text to width. The leading
// argument controls whether to prepend the indent (true for every
// line except the first when called from wrapTextWithIndent).
func wrapLine(line string, width int, indent string, leading bool) string {
	words := strings.Fields(line)
	if len(words) == 0 {
		if leading {
			return indent
		}
		return ""
	}
	var out strings.Builder
	if leading {
		out.WriteString(indent)
	}
	col := 0
	for i, word := range words {
		ww := visibleWidth(word)
		spaceBefore := 0
		if i > 0 {
			spaceBefore = 1
		}
		if col+spaceBefore+ww > width && col > 0 {
			// New line.
			out.WriteByte('\n')
			out.WriteString(indent)
			out.WriteString(word)
			col = ww
			continue
		}
		if i > 0 {
			out.WriteByte(' ')
			col++
		}
		out.WriteString(word)
		col += ww
	}
	return out.String()
}

// visibleWidth returns the visual width of s, ignoring ANSI escape
// sequences (CSI ... [m). Used by the wrapper so styled text wraps
// based on what the user sees.
func visibleWidth(s string) int {
	if !strings.Contains(s, "\x1b[") {
		return runewidth.StringWidth(s)
	}
	// Strip ANSI escapes for measurement only; the original string
	// is what gets emitted.
	stripped := stripANSI(s)
	return runewidth.StringWidth(stripped)
}

// stripANSI removes CSI escape sequences (`\x1b[...m`) from s. Used
// only for width measurement; the original styled string is what
// the renderer emits.
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		if i+1 < len(s) && s[i] == '\x1b' && s[i+1] == '[' {
			// Skip until 'm' or end.
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			if j < len(s) {
				j++ // include 'm'
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// collapseWhitespace replaces runs of whitespace (spaces, tabs,
// newlines) with single spaces. Used by heading rendering so a
// soft-break inside the heading source flattens to a single line.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
