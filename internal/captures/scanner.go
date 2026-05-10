package captures

import (
	"strings"
	"time"
)

// commitmentPhrases is the heuristic-scanner's vocabulary —
// substrings that signal the agent has committed to deferred work.
// Precision-tuned: each phrase is a strong commitment signal when
// it appears, with low false-positive risk on conversational
// text.
//
// Curated from SD's empirical capture list + the Mistral 5/1
// scheduled-email incident vocabulary. Lower-cased for case-
// insensitive match.
//
// Excluded on purpose:
//   - "I'll be" (I'll be honest / I'll be quick — not commitments)
//   - "I will" (too generic without a verb context)
//   - "let me" (too generic; "let me check" is included instead)
//   - hedged phrases ("I might", "I could", "perhaps I could")
//
// LLM-based extraction (planned for v1.1) will replace this list
// with semantic understanding, which is why this stays conservative
// — false positives erode operator trust faster than misses.
var commitmentPhrases = []string{
	"i'll send",
	"i'll check",
	"i'll follow up",
	"i'll get back to you",
	"i'll let you know",
	"i'll look into",
	"i'll review",
	"i'll find out",
	"i'll add",
	"i'll create",
	"i'll set up",
	"i'll schedule",
	"i'll remind",
	"i will send",
	"i will check",
	"i will follow up",
	"i will look into",
	"i will let you know",
	"i'm going to",
	"let me check",
	"let me look",
	"let me find",
	"by tomorrow",
	"by friday",
	"by monday",
	"by next week",
	"first thing tomorrow",
	"as soon as",
}

// commitmentExcerptChars caps how much surrounding context the
// scanner pulls when it captures a phrase. Wide enough that the
// agent reading `<captures>` can recognise what they promised; not
// so wide that the JSONL gets bulky.
const commitmentExcerptChars = 160

// ScanReply walks `text` looking for commitment phrases and
// returns one Capture per match. Pure function — does no I/O,
// no logging. Caller is responsible for persistence and dedupe.
//
// Detection is line-aware: the same phrase appearing twice in
// different lines yields two captures. Same line, same phrase,
// only the first match is captured (the offset uniquely names it
// in MakeID, so re-running the scanner on identical text gives
// the same set of IDs).
//
// `now` is stamped onto every capture's CreatedAt so a fresh-
// detection batch shares one timestamp; the worker injects a
// deterministic clock for tests.
func ScanReply(cycleID string, text string, now time.Time) []Capture {
	if cycleID == "" || strings.TrimSpace(text) == "" {
		return nil
	}
	low := strings.ToLower(text)
	var out []Capture
	seenInThisScan := map[string]bool{}
	for _, phrase := range commitmentPhrases {
		offset := 0
		for offset < len(low) {
			idx := strings.Index(low[offset:], phrase)
			if idx < 0 {
				break
			}
			absolute := offset + idx
			id := MakeID(cycleID, phrase, absolute)
			if !seenInThisScan[id] {
				seenInThisScan[id] = true
				out = append(out, Capture{
					SchemaVersion: SchemaVersion,
					ID:            id,
					CreatedAt:     now,
					Timestamp:     now,
					SourceCycleID: cycleID,
					Text:          excerptAround(text, absolute, commitmentExcerptChars),
					Source:        SourceAgentSelf,
					Status:        StatusPending,
				})
			}
			offset = absolute + len(phrase)
		}
	}
	return out
}

// excerptAround returns up to `width` chars of `text` centred on
// `at`, trimmed at sentence boundaries when possible. The result
// is the phrase plus a slice of immediate context — enough that
// the agent re-reading the capture knows what was promised.
func excerptAround(text string, at int, width int) string {
	if width <= 0 {
		return ""
	}
	half := width / 2
	start := at - half
	if start < 0 {
		start = 0
	}
	end := at + half
	if end > len(text) {
		end = len(text)
	}
	chunk := text[start:end]
	chunk = strings.TrimSpace(chunk)
	// Prefer breaking at the nearest sentence boundary on each
	// side so the excerpt reads cleanly rather than truncating
	// mid-word. Cheap heuristic — fall back to the raw window.
	if start > 0 {
		if i := strings.IndexAny(chunk, ".!?\n"); i >= 0 && i < len(chunk)/3 {
			chunk = strings.TrimSpace(chunk[i+1:])
		}
	}
	if end < len(text) {
		if i := strings.LastIndexAny(chunk, ".!?\n"); i > len(chunk)*2/3 {
			chunk = strings.TrimSpace(chunk[:i+1])
		}
	}
	if len(chunk) > width {
		chunk = chunk[:width] + "…"
	}
	return chunk
}
