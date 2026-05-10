package remembrancer

import (
	"strings"

	"github.com/seamus-brady/retainer/internal/cbr"
	"github.com/seamus-brady/retainer/internal/librarian"
)

// Search returns narrative entries whose Summary or Keywords contain
// any of the provided keywords (case-insensitive substring match).
// Empty keyword slice returns the input unchanged — caller decides
// whether that's a no-op or "match all".
//
// Order is preserved (chronological if input was chronological).
func Search(entries []librarian.NarrativeEntry, keywords []string) []librarian.NarrativeEntry {
	if len(keywords) == 0 {
		return entries
	}
	lowered := make([]string, 0, len(keywords))
	for _, k := range keywords {
		k = strings.TrimSpace(strings.ToLower(k))
		if k != "" {
			lowered = append(lowered, k)
		}
	}
	if len(lowered) == 0 {
		return entries
	}
	out := make([]librarian.NarrativeEntry, 0, len(entries)/4+1)
	for _, e := range entries {
		if matchesEntry(e, lowered) {
			out = append(out, e)
		}
	}
	return out
}

// matchesEntry returns true when the entry's summary, keywords, or
// domain contain any of the lowered query terms.
func matchesEntry(e librarian.NarrativeEntry, lowered []string) bool {
	hay := strings.ToLower(e.Summary)
	for _, t := range lowered {
		if strings.Contains(hay, t) {
			return true
		}
	}
	for _, kw := range e.Keywords {
		kwl := strings.ToLower(kw)
		for _, t := range lowered {
			if strings.Contains(kwl, t) {
				return true
			}
		}
	}
	if dom := strings.ToLower(e.Domain); dom != "" {
		for _, t := range lowered {
			if strings.Contains(dom, t) {
				return true
			}
		}
	}
	return false
}

// ConnectionsResult is a cross-store summary for one topic.
// Each store reports a hit count and up to a few sample entries
// (deliberately small — the agent reads this for orientation, not
// exhaustive enumeration).
type ConnectionsResult struct {
	Topic  string
	Counts struct {
		Narrative int
		Facts     int
		Cases     int
	}
	NarrativeSamples []librarian.NarrativeEntry
	FactSamples      []librarian.Fact
	CaseSamples      []cbr.Case
}

// connectionSampleSize is how many sample records each store
// contributes to a ConnectionsResult. Three balances "enough to
// orient" against "fits in one prompt-friendly tool result".
const connectionSampleSize = 3

// FindConnections cross-references the topic across narrative,
// facts, and cases. Topic is matched case-insensitively as a
// substring against:
//
//   - narrative: Summary, Keywords, Domain (same as Search)
//   - facts:     Key + Value
//   - cases:     Problem.Intent + Domain + Keywords + Solution.Approach
//
// Returns a ConnectionsResult with full counts and the most-recent
// `connectionSampleSize` matches per store. Empty topic returns a
// zero result without scanning (cheap pre-empty-check).
func FindConnections(narrative []librarian.NarrativeEntry, facts []librarian.Fact, cases []cbr.Case, topic string) ConnectionsResult {
	out := ConnectionsResult{Topic: topic}
	topic = strings.TrimSpace(strings.ToLower(topic))
	if topic == "" {
		return out
	}

	// Narrative
	for _, e := range narrative {
		if matchesEntry(e, []string{topic}) {
			out.Counts.Narrative++
		}
	}
	// Sample: most recent matches up to the cap. Walk in reverse
	// (input is chronological, so reverse = newest first).
	for i := len(narrative) - 1; i >= 0 && len(out.NarrativeSamples) < connectionSampleSize; i-- {
		if matchesEntry(narrative[i], []string{topic}) {
			out.NarrativeSamples = append(out.NarrativeSamples, narrative[i])
		}
	}

	// Facts
	for _, f := range facts {
		if factMatches(f, topic) {
			out.Counts.Facts++
		}
	}
	for i := len(facts) - 1; i >= 0 && len(out.FactSamples) < connectionSampleSize; i-- {
		if factMatches(facts[i], topic) {
			out.FactSamples = append(out.FactSamples, facts[i])
		}
	}

	// Cases — dedup by ID for samples (same case might match on
	// multiple fields; counts already use a single matcher so
	// dedup-as-we-walk is enough).
	seen := make(map[string]struct{}, len(cases))
	for _, c := range cases {
		if caseMatches(c, topic) {
			out.Counts.Cases++
		}
	}
	for i := len(cases) - 1; i >= 0 && len(out.CaseSamples) < connectionSampleSize; i-- {
		c := cases[i]
		if !caseMatches(c, topic) {
			continue
		}
		if _, dup := seen[c.ID]; dup {
			continue
		}
		seen[c.ID] = struct{}{}
		out.CaseSamples = append(out.CaseSamples, c)
	}

	return out
}

func factMatches(f librarian.Fact, topic string) bool {
	if strings.Contains(strings.ToLower(f.Key), topic) {
		return true
	}
	if strings.Contains(strings.ToLower(f.Value), topic) {
		return true
	}
	return false
}

func caseMatches(c cbr.Case, topic string) bool {
	if strings.Contains(strings.ToLower(c.Problem.Intent), topic) {
		return true
	}
	if strings.Contains(strings.ToLower(c.Problem.Domain), topic) {
		return true
	}
	if strings.Contains(strings.ToLower(c.Solution.Approach), topic) {
		return true
	}
	for _, k := range c.Problem.Keywords {
		if strings.Contains(strings.ToLower(k), topic) {
			return true
		}
	}
	return false
}


// Stats is a count summary for a date-range consolidation. Returned
// by Consolidate; consumed by the cog tool that formats it for the
// agent.
type Stats struct {
	NarrativeEntries int
	Facts            int
	Cases            int

	// Sample excerpts — small N so the result fits in a single
	// tool_result block.
	NarrativeSamples []librarian.NarrativeEntry
	FactSamples      []librarian.Fact
	CaseSamples      []cbr.Case
}

const consolidateSampleSize = 5

// Consolidate gathers counts + sample excerpts for a date range.
// Pure aggregation — no LLM call, no persistence. The agent reads
// the result and decides whether to write a synthesis report via
// `write_consolidation_report`.
//
// When a topic is supplied, narrative + cases are filtered by
// substring match before counting (facts aren't filtered — they
// don't carry a topic shape). Empty topic counts everything.
func Consolidate(narrative []librarian.NarrativeEntry, facts []librarian.Fact, cases []cbr.Case, topic string) Stats {
	topic = strings.TrimSpace(strings.ToLower(topic))

	// Filter narrative by topic.
	filteredNarrative := narrative
	if topic != "" {
		filteredNarrative = Search(narrative, []string{topic})
	}

	// Filter cases by topic.
	filteredCases := cases
	if topic != "" {
		filteredCases = filteredCases[:0]
		for _, c := range cases {
			if caseMatches(c, topic) {
				filteredCases = append(filteredCases, c)
			}
		}
	}

	stats := Stats{
		NarrativeEntries: len(filteredNarrative),
		Facts:            len(facts),
		Cases:            len(filteredCases),
	}

	// Samples: take the most recent N of each.
	stats.NarrativeSamples = tailNarrative(filteredNarrative, consolidateSampleSize)
	stats.FactSamples = tailFacts(facts, consolidateSampleSize)
	stats.CaseSamples = tailCases(filteredCases, consolidateSampleSize)
	return stats
}

func tailNarrative(in []librarian.NarrativeEntry, n int) []librarian.NarrativeEntry {
	if len(in) <= n {
		// Copy so callers can't mutate our internal slice.
		out := make([]librarian.NarrativeEntry, len(in))
		copy(out, in)
		return out
	}
	out := make([]librarian.NarrativeEntry, n)
	copy(out, in[len(in)-n:])
	return out
}

func tailFacts(in []librarian.Fact, n int) []librarian.Fact {
	if len(in) <= n {
		out := make([]librarian.Fact, len(in))
		copy(out, in)
		return out
	}
	out := make([]librarian.Fact, n)
	copy(out, in[len(in)-n:])
	return out
}

func tailCases(in []cbr.Case, n int) []cbr.Case {
	if len(in) <= n {
		out := make([]cbr.Case, len(in))
		copy(out, in)
		return out
	}
	out := make([]cbr.Case, n)
	copy(out, in[len(in)-n:])
	return out
}
