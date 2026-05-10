package mdrender

// ANSI escape sequences for terminal styling. Conservative set —
// bold + italic + underline + dim are well-supported across modern
// terminals. Foreground colour is deliberately left out for the V1
// renderer; pure-structure styling reads cleanly on every theme
// without per-terminal colour tuning.
//
// Each pair has a balanced reset so nested emphasis composes
// correctly.
const (
	// ansiBold / ansiBoldReset toggle bold weight (SGR 1 / 22).
	ansiBold      = "\x1b[1m"
	ansiBoldReset = "\x1b[22m"

	// ansiItalic / ansiItalicReset toggle italic (SGR 3 / 23). Some
	// terminals render italic as a colour shift; that's still a
	// useful visual cue.
	ansiItalic      = "\x1b[3m"
	ansiItalicReset = "\x1b[23m"

	// ansiUnderline / ansiUnderlineReset toggle underline (SGR 4 /
	// 24). Used for link labels.
	ansiUnderline      = "\x1b[4m"
	ansiUnderlineReset = "\x1b[24m"

	// ansiDim / ansiDimReset is a faint-text variant (SGR 2 / 22).
	// Used for inline code, blockquote prefix, code-block borders.
	ansiDim      = "\x1b[2m"
	ansiDimReset = "\x1b[22m"

	// ansiCode wraps inline code spans. Terminals without dim
	// support fall back to the surrounding plain text — readable.
	ansiCode  = "\x1b[2m"
	ansiReset = "\x1b[22m"
)
