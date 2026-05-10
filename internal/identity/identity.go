// Package identity loads and renders the agent's persona + session
// preamble. Persona is verbatim first-person character text; session
// preamble is a template with {{slots}} and OMIT-IF rules that gets
// re-rendered each cycle with current context (clock, future sensorium
// data, future memory references).
//
// Files live at <workspace>/config/identity/persona.md and
// session_preamble.md. Missing files use the embedded defaults so the
// agent always has a character.
//
// Subset implemented in this slice (sensorium / memory references not yet
// landed):
//   - Slot substitution: {{key}} → context value; line dropped if any slot
//     is unresolved (so template syntax doesn't leak into the prompt)
//   - OMIT IF EMPTY: line dropped if blank (or trailing colon) after
//     substitution + tag removal — meaningful once sensorium lines like
//     "Recent threads: {{thread_count}} [OMIT IF EMPTY]" exist
//
// Other Springdrift OMIT-IF conditions (ZERO / THREADS EXIST / FACTS EXIST
// / NO PROFILE) land alongside the subsystems they reference.
package identity

import (
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	IdentitySubdir   = "identity"
	PersonaFilename  = "persona.md"
	PreambleFilename = "session_preamble.md"

	omitIfEmptyTag = "[OMIT IF EMPTY]"
	omitIfZeroTag  = "[OMIT IF ZERO]"
)

//go:embed defaults/persona.md
var embeddedPersona []byte

//go:embed defaults/session_preamble.md
var embeddedPreamble []byte

// EmbeddedFiles returns a copy of the embedded identity defaults, keyed by
// filename. Used by `retainer init` to seed a workspace.
func EmbeddedFiles() map[string][]byte {
	return map[string][]byte{
		PersonaFilename:  copyBytes(embeddedPersona),
		PreambleFilename: copyBytes(embeddedPreamble),
	}
}

func copyBytes(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// Identity is the loaded persona + preamble template.
type Identity struct {
	Persona  string
	Preamble string
	Source   string // "<workspace>" if both files loaded from disk; "<embedded defaults>" if neither
}

// Context provides the slot values for one render. AgentName + Workspace
// are static for a session; Now is per-cycle.
type Context struct {
	AgentName string
	Workspace string
	Now       time.Time
}

// Load reads identity files from <configDir>/identity/. Missing files use
// the embedded defaults. Returns the Identity and any I/O error other
// than NotExist.
func Load(configDir string) (*Identity, error) {
	identityDir := filepath.Join(configDir, IdentitySubdir)

	persona, personaSrc, err := loadOrDefault(filepath.Join(identityDir, PersonaFilename), embeddedPersona)
	if err != nil {
		return nil, fmt.Errorf("identity: load persona: %w", err)
	}
	preamble, preambleSrc, err := loadOrDefault(filepath.Join(identityDir, PreambleFilename), embeddedPreamble)
	if err != nil {
		return nil, fmt.Errorf("identity: load preamble: %w", err)
	}

	source := personaSrc
	if preambleSrc != personaSrc {
		source = personaSrc + " + " + preambleSrc
	}

	return &Identity{
		Persona:  persona,
		Preamble: preamble,
		Source:   source,
	}, nil
}

func loadOrDefault(path string, fallback []byte) (string, string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return string(fallback), "<embedded defaults>", nil
	}
	if err != nil {
		return "", "", err
	}
	return string(data), path, nil
}

// BuildSystemPrompt assembles the final system prompt from the static
// per-cycle Context. The Curator (which queries memory) uses Render
// directly to inject memory-derived slots — this method is the simple
// path for callers that don't have a curator (tests, init-time wiring).
func (i *Identity) BuildSystemPrompt(ctx Context) string {
	return i.Render(ContextToSlots(ctx))
}

// Render assembles the final system prompt from a caller-built slot map.
// Persona text comes first, verbatim except for slot substitution;
// preamble follows after a blank line, with slot substitution + OMIT-IF
// rules applied.
//
// The Curator builds the slot map by merging ContextToSlots with
// memory-derived values (recent narrative, persistent fact count, etc.)
// before calling Render. Slots not present in the map are treated as
// unresolved — lines containing them are dropped (or trigger OMIT IF
// EMPTY in the preamble).
func (i *Identity) Render(slots map[string]string) string {
	persona, preamble := i.RenderParts(slots)

	var b strings.Builder
	b.WriteString(persona)
	if strings.TrimSpace(preamble) != "" {
		b.WriteString("\n\n")
		b.WriteString(preamble)
	}
	return b.String()
}

// RenderParts returns the persona and preamble sections as separate
// strings, each with slot substitution applied (and OMIT-IF rules on the
// preamble half). Used by callers that need to compose them with
// additional content — the curator inserts available_skills /
// bootstrap_skills between persona and preamble, and the cache-aware
// system-prompt assembly puts persona + skills in the cacheable Stable
// half while keeping preamble in the per-cycle Dynamic half.
func (i *Identity) RenderParts(slots map[string]string) (persona, preamble string) {
	return renderVerbatim(i.Persona, slots), renderTemplate(i.Preamble, slots)
}

// ContextToSlots converts a static per-cycle Context into the base slot
// map. Curators add memory-derived slots on top before calling Render.
func ContextToSlots(ctx Context) map[string]string {
	return map[string]string{
		"agent_name": ctx.AgentName,
		"workspace":  ctx.Workspace,
		"date":       ctx.Now.Format("2006-01-02"),
		"time":       ctx.Now.Format("15:04 MST"),
	}
}

// renderVerbatim does slot substitution only. Lines with unresolved
// slots are dropped to avoid leaking template syntax. No OMIT-IF rules —
// persona is character text, not a template.
func renderVerbatim(text string, slots map[string]string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		substituted, unresolved := substituteSlots(line, slots)
		if unresolved {
			continue
		}
		out = append(out, substituted)
	}
	return strings.Join(out, "\n")
}

// renderTemplate applies slot substitution + OMIT-IF rules.
func renderTemplate(text string, slots map[string]string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		substituted, unresolved := substituteSlots(line, slots)
		if unresolved {
			continue
		}
		cleaned, drop := applyOmitRules(substituted)
		if drop {
			continue
		}
		out = append(out, cleaned)
	}
	return strings.Join(out, "\n")
}

var slotPattern = regexp.MustCompile(`\{\{([^{}]+)\}\}`)

// substituteSlots replaces {{key}} with slots[key]. Returns the substituted
// line and a flag indicating whether any slot was unresolved (missing key
// or empty value).
func substituteSlots(line string, slots map[string]string) (string, bool) {
	unresolved := false
	out := slotPattern.ReplaceAllStringFunc(line, func(match string) string {
		key := strings.TrimSpace(match[2 : len(match)-2])
		val, ok := slots[key]
		if !ok || val == "" {
			unresolved = true
			return match
		}
		return val
	})
	return out, unresolved
}

// applyOmitRules strips OMIT-IF tags from a line and decides whether to
// drop it. Supports:
//
//	[OMIT IF EMPTY] — drops the line when the rendered text is whitespace-
//	                  only or ends with a trailing ":" (e.g. "Threads: " with
//	                  nothing after).
//	[OMIT IF ZERO]  — drops the line when the rendered text starts with
//	                  "0 " or contains " 0 " (e.g. "Active threads: 0").
//	                  Mirrors Springdrift identity.gleam:contains_zero_count.
func applyOmitRules(line string) (string, bool) {
	if strings.Contains(line, omitIfZeroTag) {
		cleaned := strings.TrimSpace(strings.ReplaceAll(line, omitIfZeroTag, ""))
		if containsZeroCount(cleaned) {
			return "", true
		}
		return cleaned, false
	}
	if strings.Contains(line, omitIfEmptyTag) {
		cleaned := strings.TrimSpace(strings.ReplaceAll(line, omitIfEmptyTag, ""))
		if isEmpty(cleaned) {
			return "", true
		}
		return cleaned, false
	}
	return line, false
}

// containsZeroCount returns true when s starts with "0 " or contains " 0 "
// (mid-string). Mirrors Springdrift's `contains_zero_count` exactly so a
// counted slot that resolved to zero ("Active threads: 0") drops the
// whole line.
func containsZeroCount(s string) bool {
	trimmed := strings.TrimSpace(s)
	return strings.HasPrefix(trimmed, "0 ") || strings.Contains(trimmed, " 0 ")
}

func isEmpty(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return true
	}
	if strings.HasSuffix(s, ":") {
		return true
	}
	return false
}
