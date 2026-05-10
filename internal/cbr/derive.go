package cbr

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// Classify is the deterministic category classifier the archivist
// runs to label each derived case. Rules in priority order — first
// match wins. The function is pure: same input → same output across
// runs.
//
// Conversation cycles get an EMPTY category — stored for audit, not
// surfaced as a pattern. This is the load-bearing fix from the
// memory-and-logging audit: pleasantries and acks shouldn't pollute
// CBR retrieval as Pitfall just because the judge's "did it
// address the request" test fails on "hi there" → "Hello".
//
// Categories (when classification is not Conversation):
//
//   - Pitfall          — outcome failure with pitfalls listed
//   - Troubleshooting  — outcome failure with no pitfalls (we
//     learned what didn't work but not what to watch for)
//   - DomainKnowledge  — outcome partial (some work done; not a
//     reusable strategy)
//   - CodePattern      — solution.approach contains code markers
//     (".go", "regex", "snippet" etc.)
//   - Troubleshooting  — problem.intent contains diagnostic verbs
//     ("debug", "fix", "diagnose", ...)
//   - Strategy         — solution has agents OR tools recorded
//   - DomainKnowledge  — fallback (factual knowledge, no
//     procedural shape)
//
// Mirrors SD's `narrative/archivist.gleam:assign_category`. The
// verb list is deliberately small — every classifier rule is
// operator-auditable from this source file.
func Classify(p Problem, s Solution, o Outcome) Category {
	// Conversation → no category. Stored for audit, doesn't
	// pollute retrieval as a pattern.
	if p.IntentClass == IntentConversation {
		return ""
	}
	switch o.Status {
	case StatusFailure:
		if len(o.Pitfalls) > 0 {
			return CategoryPitfall
		}
		return CategoryTroubleshooting
	case StatusPartial:
		// Partial cycles produce DomainKnowledge — they taught
		// the agent something about the domain even if the
		// outcome was incomplete.
		return CategoryDomainKnowledge
	}
	// Status is success (or unknown — fall through to success-
	// path rules so old records produced before StatusPartial
	// landed still classify sensibly).
	approach := strings.ToLower(s.Approach)
	for _, marker := range codePatternMarkers {
		if strings.Contains(approach, marker) {
			return CategoryCodePattern
		}
	}
	intent := strings.ToLower(p.Intent)
	for _, verb := range troubleshootingVerbs {
		if strings.Contains(intent, verb) {
			return CategoryTroubleshooting
		}
	}
	if len(s.AgentsUsed) > 0 || len(s.ToolsUsed) > 0 {
		return CategoryStrategy
	}
	return CategoryDomainKnowledge
}

// codePatternMarkers are substrings whose presence in solution.approach
// flips classification to CategoryCodePattern. Lowercased; we lower-
// case the approach text before comparison.
var codePatternMarkers = []string{
	"function ", "func ", "regex", "regexp",
	"snippet", "yaml", "json schema",
	".go", ".ts", ".tsx", ".js", ".py", ".rs",
	"sql query", "shell script",
}

// troubleshootingVerbs are intent substrings that trigger the
// troubleshooting category. Lowercased.
var troubleshootingVerbs = []string{
	"debug", "fix", "diagnose", "investigate",
	"trace", "troubleshoot", "repair",
}

// DeriveProblem builds a Problem from a cycle's user text. Heuristic-
// only — no LLM call. Future enrichment (LLM-driven intent / domain /
// entities extraction) lands as a higher-quality producer; the case
// schema doesn't change.
//
// What this version does:
//
//   - UserInput: trimmed, capped to 600 chars.
//   - Intent: first sentence (up to first period or 80 chars,
//     whichever comes first), lowercased. Kept short so the
//     intent-match signal isn't drowned by noise.
//   - Keywords: tokenised words longer than 3 chars, lowercased,
//     deduplicated, capped at 8 — enough to feed the inverted index
//     and Jaccard signal without bloating storage.
//   - Domain / Entities / QueryComplexity: empty. Wait on the LLM
//     enricher.
func DeriveProblem(userInput string) Problem {
	trimmed := strings.TrimSpace(userInput)
	if len(trimmed) > 600 {
		trimmed = trimmed[:600]
	}
	intent := firstSentence(trimmed, 80)
	keywords := topKeywords(trimmed, 8)
	return Problem{
		UserInput: trimmed,
		Intent:    strings.ToLower(intent),
		Keywords:  keywords,
	}
}

// DeriveSolution builds a Solution from a cycle's reply text + the
// agents/tools the cog tracked. Heuristic — approach is the first
// chunk of the reply.
func DeriveSolution(replyText string, agentsUsed, toolsUsed []string) Solution {
	approach := firstSentence(strings.TrimSpace(replyText), 200)
	return Solution{
		Approach:   approach,
		AgentsUsed: dedupeNonEmpty(agentsUsed),
		ToolsUsed:  dedupeNonEmpty(toolsUsed),
	}
}

// DeriveOutcome maps a cycle status into a Case outcome. Confidence
// uses sensible defaults: 0.85 for success (high but not certain),
// 0.4 for failure (still has lessons; not zero).
func DeriveOutcome(success bool, assessment string) Outcome {
	if success {
		return Outcome{
			Status:     StatusSuccess,
			Confidence: 0.85,
			Assessment: strings.TrimSpace(assessment),
		}
	}
	return Outcome{
		Status:     StatusFailure,
		Confidence: 0.40,
		Assessment: strings.TrimSpace(assessment),
	}
}

// NewCaseID returns a fresh UUID for a new case. Wrapped here so test
// fixtures can stub it via a function variable in future, and so the
// archivist doesn't have to import uuid directly.
func NewCaseID() string {
	return uuid.NewString()
}

// NewCase builds a Case from the derived parts plus identity fields.
// Sets timestamp to `now`; classifies via Classify; stamps the
// embedding fields when one is provided.
//
// Caller is responsible for providing the embedding (and embedder ID)
// — the cbr package doesn't run an embedder itself, since embedding
// is an I/O / heavy-compute operation that belongs in the archivist's
// goroutine, not in pure data layer.
func NewCase(
	sourceNarrativeID string,
	problem Problem,
	solution Solution,
	outcome Outcome,
	embedding []float32,
	embedderID string,
	now time.Time,
) Case {
	return Case{
		ID:                NewCaseID(),
		Timestamp:         now,
		SchemaVersion:     SchemaVersion,
		Problem:           problem,
		Solution:          solution,
		Outcome:           outcome,
		SourceNarrativeID: sourceNarrativeID,
		Category:          Classify(problem, solution, outcome),
		Embedding:         embedding,
		EmbedderID:        embedderID,
	}
}

// firstSentence returns the chunk before the first period, exclamation
// or question mark, capped at maxLen. Falls back to a maxLen-truncate
// when no sentence terminator is found.
func firstSentence(s string, maxLen int) string {
	for i, r := range s {
		if r == '.' || r == '!' || r == '?' {
			if i == 0 {
				continue
			}
			out := s[:i]
			if len(out) > maxLen {
				out = out[:maxLen]
			}
			return out
		}
	}
	if len(s) > maxLen {
		return s[:maxLen]
	}
	return s
}

// topKeywords returns up to limit keyword tokens from the input —
// alphanumeric words longer than 3 chars, lowercased, deduplicated,
// stopwords stripped.
//
// "Top" today means insertion order — first occurrences come first.
// A future enricher with TF-IDF over the workspace corpus would sort
// by salience; insertion order is the simplest defensible default.
func topKeywords(s string, limit int) []string {
	if s == "" {
		return nil
	}
	out := make([]string, 0, limit)
	seen := make(map[string]struct{}, limit)
	for _, raw := range tokenise(s) {
		if len(out) >= limit {
			break
		}
		w := strings.ToLower(raw)
		if len(w) <= 3 {
			continue
		}
		if _, banned := stopwords[w]; banned {
			continue
		}
		if _, dup := seen[w]; dup {
			continue
		}
		seen[w] = struct{}{}
		out = append(out, w)
	}
	return out
}

// tokenise splits on whitespace + punctuation, keeping alphanumeric
// runs. Cheap and good enough for the heuristic enricher; the LLM
// path can do better when it lands.
func tokenise(s string) []string {
	out := make([]string, 0, 16)
	current := strings.Builder{}
	flush := func() {
		if current.Len() > 0 {
			out = append(out, current.String())
			current.Reset()
		}
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			current.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return out
}

// stopwords is a small banned set — common English function words
// that the inverted index gets no signal from. Ports a subset of
// SD's list, kept short so the file stays auditable.
var stopwords = map[string]struct{}{
	"about": {}, "after": {}, "again": {}, "against": {}, "before": {},
	"being": {}, "below": {}, "between": {}, "could": {}, "doing": {},
	"during": {}, "each": {}, "from": {}, "have": {}, "having": {},
	"here": {}, "into": {}, "more": {}, "other": {}, "should": {},
	"some": {}, "such": {}, "than": {}, "that": {}, "their": {},
	"them": {}, "then": {}, "there": {}, "these": {}, "they": {},
	"this": {}, "those": {}, "through": {}, "very": {}, "were": {},
	"what": {}, "when": {}, "where": {}, "which": {}, "while": {},
	"with": {}, "would": {}, "your": {},
}

// dedupeNonEmpty preserves order and drops both duplicates and empty
// strings.
func dedupeNonEmpty(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
