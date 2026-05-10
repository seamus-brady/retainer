package cbr

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/seamus-brady/retainer/internal/decay"
	"github.com/seamus-brady/retainer/internal/embed"
)

// Weights configures the 6-signal retrieval scoring. All values must
// be >= 0; sum is conventionally 1.0 but the merge logic
// auto-renormalises when embeddings are unavailable so non-summing-1
// weights still produce well-behaved scores.
//
// Defaults (DefaultWeights) come from SD's experiment-3 ablation and
// reflect that index-overlap + embeddings dominate retrieval quality.
type Weights struct {
	Field     float64
	Index     float64
	Recency   float64
	Domain    float64
	Embedding float64
	Utility   float64
}

// DefaultWeights ports SD's tuned defaults. Sums to 1.0.
func DefaultWeights() Weights {
	return Weights{
		Field:     0.10,
		Index:     0.25,
		Recency:   0.05,
		Domain:    0.10,
		Embedding: 0.40,
		Utility:   0.10,
	}
}

// CaseBase is the in-memory retrieval index. Owned by the librarian
// goroutine — single-writer invariant. All mutations (Retain, Remove,
// Update) come from the librarian's actor handler; reads (Retrieve)
// happen synchronously on librarian-side calls and are short-lived
// enough to hold the actor up.
//
// Storage shape:
//
//   - cases:      ID → Case (the metadata, including embedding when present)
//   - index:      lowercased token → set of case IDs (inverted index)
//
// Embeddings live on the Case itself (Case.Embedding) rather than as
// a side map, so a librarian replay rebuilds the full index without
// re-running the embedder.
type CaseBase struct {
	cases    map[string]Case
	index    map[string]map[string]struct{}
	embedder embed.Embedder
	// HalfLifeDays controls confidence decay during retrieval. Zero
	// uses decay.DefaultHalfLifeDays; negative disables decay.
	HalfLifeDays int
}

// NewCaseBase constructs an empty CaseBase. Pass nil embedder to
// disable the 6th signal — retrieval auto-renormalises weights over
// the other five.
func NewCaseBase(embedder embed.Embedder) *CaseBase {
	return &CaseBase{
		cases:        make(map[string]Case),
		index:        make(map[string]map[string]struct{}),
		embedder:     embedder,
		HalfLifeDays: decay.DefaultHalfLifeDays,
	}
}

// Count returns the number of cases that would be eligible for
// retrieval — neither redacted nor superseded. Used by the
// sensorium's <memory cases="N"/> attr so the agent's count
// reflects what it could actually see, not the audit-trail total.
func (b *CaseBase) Count() int {
	n := 0
	for _, c := range b.cases {
		if c.Redacted || c.SupersededBy != "" {
			continue
		}
		n++
	}
	return n
}

// CountIncludingRedacted is the total case-record count including
// suppressed records. Useful for diagnostic / debug telemetry; not
// surfaced to the agent.
func (b *CaseBase) CountIncludingRedacted() int { return len(b.cases) }

// All returns every case the base holds, including redacted and
// superseded records. The slice is freshly allocated and safe to
// retain. Used by housekeeping passes (dedup + prune) that need to
// scan the corpus and decide what to mark, plus by tests.
//
// Order is unspecified — Go map iteration is randomised. Callers
// that need deterministic output should sort by ID.
func (b *CaseBase) All() []Case {
	out := make([]Case, 0, len(b.cases))
	for _, c := range b.cases {
		out = append(out, c)
	}
	return out
}

// Get returns the case with the given ID, or false if absent. Returns
// suppressed cases too — curation tools need to read them by ID even
// when retrieval excludes them.
func (b *CaseBase) Get(id string) (Case, bool) {
	c, ok := b.cases[id]
	return c, ok
}

// Retain adds a case to the base. Replaces any existing case with the
// same ID (curation tools — correct_case, boost_case — use this to
// upsert). Embeddings are preserved when present on the input.
func (b *CaseBase) Retain(c Case) {
	if existing, ok := b.cases[c.ID]; ok {
		b.removeFromIndex(existing)
	}
	b.cases[c.ID] = c
	for _, tok := range CaseTokens(c) {
		set, ok := b.index[tok]
		if !ok {
			set = make(map[string]struct{})
			b.index[tok] = set
		}
		set[c.ID] = struct{}{}
	}
}

// Remove drops a case entirely. Used by tests + the (deferred)
// housekeeping dedup path. Operator curation should use SuppressCase
// (which marks Redacted) instead so the JSONL trail stays intact.
func (b *CaseBase) Remove(id string) {
	c, ok := b.cases[id]
	if !ok {
		return
	}
	b.removeFromIndex(c)
	delete(b.cases, id)
}

// removeFromIndex strips a case's tokens from the inverted index.
// Empty token sets get pruned so the index doesn't accumulate dead
// entries.
func (b *CaseBase) removeFromIndex(c Case) {
	for _, tok := range CaseTokens(c) {
		set, ok := b.index[tok]
		if !ok {
			continue
		}
		delete(set, c.ID)
		if len(set) == 0 {
			delete(b.index, tok)
		}
	}
}

// Retrieve scores every case against the query and returns up to
// query.MaxResults sorted by score descending. Suppressed cases are
// excluded.
//
// Scoring is the 6-signal weighted fusion:
//
//  1. Field score — intent/domain match + keyword/entity Jaccard +
//     status/confidence (decayed)
//  2. Inverted index — fraction of query tokens that appear in the case
//  3. Recency — newest case = 1.0, oldest = 0.0, linear ramp
//  4. Domain — 1.0 on exact match, 0.0 otherwise
//  5. Embedding cosine — when both query embed and case vector exist
//  6. Utility — Laplace-smoothed retrieval success rate
//
// When embeddings are unavailable (no embedder, or embedder errored
// on the query, or no case in the base has a vector), the embedding
// signal is dropped and the other five are renormalised to sum to
// the same total weight.
//
// Embedding errors on the query side are non-fatal — retrieval
// continues with the renormalised five-signal mix. Per-case
// embedding failures (the case has no vector but others do) score
// that case 0.0 on the embedding signal; the other signals carry it.
func (b *CaseBase) Retrieve(ctx context.Context, q Query) []Scored {
	if len(b.cases) == 0 {
		return nil
	}
	if q.MaxResults <= 0 {
		q.MaxResults = DefaultMaxResults
	}

	// Collect retrieval-eligible candidates: skip both operator-
	// suppressed (Redacted) AND housekeeper-superseded
	// (SupersededBy). The two flags carry different audit meanings
	// but the retrieval consequence is identical — neither should
	// surface in front of the agent.
	candidates := make([]Case, 0, len(b.cases))
	for _, c := range b.cases {
		if c.Redacted || c.SupersededBy != "" {
			continue
		}
		candidates = append(candidates, c)
	}
	if len(candidates) == 0 {
		return nil
	}

	weights := DefaultWeights()
	now := time.Now()

	// Field score — depends on the case's outcome.confidence which
	// gets decayed at read time.
	fieldScores := make(map[string]float64, len(candidates))
	for _, c := range candidates {
		fieldScores[c.ID] = b.fieldScore(q, c, now)
	}

	// Inverted-index hit ratio.
	indexScores := b.indexScores(q, candidates)

	// Recency rank, linear from 1.0 (newest) to 0.0 (oldest).
	recencyScores := recencyRank(candidates)

	// Domain binary match.
	domainScores := domainMatch(q, candidates)

	// Embedding cosine — drops out when no embedder or no case
	// vectors exist.
	queryVec, embeddingsAvailable := b.queryEmbedding(ctx, q, candidates)
	embedScores := embeddingScores(queryVec, candidates)

	// Utility signal from usage stats.
	utilityScores := utilityScores(candidates)

	// Effective weights — renormalise over the active signals when
	// embeddings absent.
	w := effectiveWeights(weights, embeddingsAvailable)

	scored := make([]Scored, 0, len(candidates))
	for _, c := range candidates {
		score := w.Field*fieldScores[c.ID] +
			w.Index*indexScores[c.ID] +
			w.Recency*recencyScores[c.ID] +
			w.Domain*domainScores[c.ID] +
			w.Embedding*embedScores[c.ID] +
			w.Utility*utilityScores[c.ID]
		scored = append(scored, Scored{Score: score, Case: c})
	}

	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].Score > scored[j].Score
	})

	if len(scored) > q.MaxResults {
		scored = scored[:q.MaxResults]
	}
	return scored
}

// effectiveWeights renormalises when embeddings are unavailable so
// the active five signals sum to the same total the input weights
// did. Pass-through when embeddings are available (the input weights
// stand). Keeps retrieval well-behaved across mixed corpora — some
// cases with vectors, some without — by making the embedding signal
// "zero out" rather than "skew the rest".
func effectiveWeights(w Weights, embeddingsAvailable bool) Weights {
	if embeddingsAvailable {
		return w
	}
	other := w.Field + w.Index + w.Recency + w.Domain + w.Utility
	if other == 0 {
		// Degenerate input — fall back to even weight across the
		// active signals. Avoids divide-by-zero downstream.
		return Weights{Field: 0.2, Index: 0.2, Recency: 0.2, Domain: 0.2, Utility: 0.2}
	}
	scale := (w.Field + w.Index + w.Recency + w.Domain + w.Embedding + w.Utility) / other
	return Weights{
		Field:     w.Field * scale,
		Index:     w.Index * scale,
		Recency:   w.Recency * scale,
		Domain:    w.Domain * scale,
		Embedding: 0,
		Utility:   w.Utility * scale,
	}
}

// fieldScore is the deterministic structural-similarity signal:
//
//	intent match     → 0.3
//	domain match     → 0.3
//	keyword Jaccard  → 0.2
//	entity Jaccard   → 0.1
//	status/confidence → 0.1 (success only, scaled by decayed confidence)
//
// Returns a value in [0, 1].
func (b *CaseBase) fieldScore(q Query, c Case, now time.Time) float64 {
	var score float64

	if equalLower(q.Intent, c.Problem.Intent) {
		score += 0.3
	}
	if equalLower(q.Domain, c.Problem.Domain) {
		score += 0.3
	}
	score += jaccard(lowercaseAll(q.Keywords), lowercaseAll(c.Problem.Keywords)) * 0.2
	score += jaccard(lowercaseAll(q.Entities), lowercaseAll(c.Problem.Entities)) * 0.1

	// Status/confidence component. Success only contributes; the
	// confidence is decayed via half-life so an old confident success
	// still counts but with diminishing weight.
	if c.Outcome.Status == StatusSuccess {
		eff := c.Outcome.Confidence
		hl := b.HalfLifeDays
		if hl == 0 {
			hl = decay.DefaultHalfLifeDays
		}
		if hl > 0 && !c.Timestamp.IsZero() {
			eff = decay.ConfidenceAt(eff, c.Timestamp, now, hl)
		}
		score += 0.1 * eff
	}

	return score
}

// indexScores computes the inverted-index hit ratio per candidate.
// hits / total_query_tokens, where total_query_tokens dedupes
// (query "auth auth login" counts as 2 distinct tokens, not 3).
func (b *CaseBase) indexScores(q Query, candidates []Case) map[string]float64 {
	queryTokens := queryIndexTokens(q)
	total := float64(len(queryTokens))
	scores := make(map[string]float64, len(candidates))
	if total == 0 {
		for _, c := range candidates {
			scores[c.ID] = 0
		}
		return scores
	}

	hitCounts := make(map[string]int, len(candidates))
	for _, tok := range queryTokens {
		matchSet, ok := b.index[tok]
		if !ok {
			continue
		}
		for id := range matchSet {
			hitCounts[id]++
		}
	}
	for _, c := range candidates {
		scores[c.ID] = float64(hitCounts[c.ID]) / total
	}
	return scores
}

// recencyRank returns scores in [0, 1] with the newest case at 1.0
// and the oldest at 0.0, linearly interpolated. Single-case corpora
// score 1.0.
func recencyRank(candidates []Case) map[string]float64 {
	out := make(map[string]float64, len(candidates))
	if len(candidates) == 0 {
		return out
	}
	if len(candidates) == 1 {
		out[candidates[0].ID] = 1.0
		return out
	}

	// Sort by timestamp descending — newest first.
	sorted := make([]Case, len(candidates))
	copy(sorted, candidates)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Timestamp.After(sorted[j].Timestamp)
	})
	n := float64(len(sorted) - 1)
	for i, c := range sorted {
		out[c.ID] = (n - float64(i)) / n
	}
	return out
}

// domainMatch is the binary 0/1 signal — exact (case-insensitive)
// equality on the domain field. Empty query domain produces all-zero
// scores (matches SD; an unspecified query shouldn't bias toward
// any particular domain).
func domainMatch(q Query, candidates []Case) map[string]float64 {
	out := make(map[string]float64, len(candidates))
	if q.Domain == "" {
		for _, c := range candidates {
			out[c.ID] = 0
		}
		return out
	}
	queryLower := strings.ToLower(q.Domain)
	for _, c := range candidates {
		if strings.ToLower(c.Problem.Domain) == queryLower {
			out[c.ID] = 1.0
		} else {
			out[c.ID] = 0
		}
	}
	return out
}

// queryEmbedding computes the query's embedding when an embedder is
// configured AND at least one candidate has a stored vector.
//
// Returns (vec, true) when embeddings can contribute to scoring, or
// (nil, false) when the signal should be dropped + weights
// renormalised. Embedder errors are treated as a soft drop — the
// retrieval still completes, just on five signals.
func (b *CaseBase) queryEmbedding(ctx context.Context, q Query, candidates []Case) ([]float32, bool) {
	if b.embedder == nil {
		return nil, false
	}
	hasVec := false
	for _, c := range candidates {
		if len(c.Embedding) > 0 {
			hasVec = true
			break
		}
	}
	if !hasVec {
		return nil, false
	}
	queryText := strings.TrimSpace(strings.Join([]string{q.Intent, q.Domain, strings.Join(q.Keywords, " ")}, " "))
	if queryText == "" {
		return nil, false
	}
	vec, err := b.embedder.Embed(ctx, queryText)
	if err != nil {
		return nil, false
	}
	return vec, true
}

// embeddingScores computes cosine similarity per case. Returns 0 for
// any case missing a vector. Cosine values are clamped to [0, 1] —
// negative cosines (anti-correlated vectors) score 0, since CBR
// retrieval is a relevance ranking, not a sentiment signal.
func embeddingScores(queryVec []float32, candidates []Case) map[string]float64 {
	out := make(map[string]float64, len(candidates))
	if queryVec == nil {
		for _, c := range candidates {
			out[c.ID] = 0
		}
		return out
	}
	for _, c := range candidates {
		if len(c.Embedding) == 0 {
			out[c.ID] = 0
			continue
		}
		s := cosineSimilarity(queryVec, c.Embedding)
		if s < 0 {
			s = 0
		}
		if s > 1 {
			s = 1
		}
		out[c.ID] = s
	}
	return out
}

// utilityScores reads each case's usage stats and returns the
// Laplace-smoothed retrieval success rate. Cases without stats get
// 0.5 (neutral) so a fresh case isn't biased against a heavily-used
// one.
func utilityScores(candidates []Case) map[string]float64 {
	out := make(map[string]float64, len(candidates))
	for _, c := range candidates {
		out[c.ID] = Utility(c.UsageStats)
	}
	return out
}

// CaseTokens returns the lowercased, deduplicated token set for a
// case — used both at retain-time (to populate the inverted index)
// and at retrieval-time (to compute hit counts).
func CaseTokens(c Case) []string {
	tokens := make([]string, 0, 16)
	for _, k := range c.Problem.Keywords {
		if t := strings.ToLower(strings.TrimSpace(k)); t != "" {
			tokens = append(tokens, t)
		}
	}
	for _, e := range c.Problem.Entities {
		if t := strings.ToLower(strings.TrimSpace(e)); t != "" {
			tokens = append(tokens, t)
		}
	}
	for _, t := range c.Solution.ToolsUsed {
		if t := strings.ToLower(strings.TrimSpace(t)); t != "" {
			tokens = append(tokens, t)
		}
	}
	for _, a := range c.Solution.AgentsUsed {
		if t := strings.ToLower(strings.TrimSpace(a)); t != "" {
			tokens = append(tokens, t)
		}
	}
	if t := strings.ToLower(strings.TrimSpace(c.Problem.Intent)); t != "" {
		tokens = append(tokens, t)
	}
	if t := strings.ToLower(strings.TrimSpace(c.Problem.Domain)); t != "" {
		tokens = append(tokens, t)
	}
	for _, w := range strings.Fields(strings.ToLower(c.Solution.Approach)) {
		// SD's port keeps approach words longer than 2 chars — drops
		// "of/to/in" noise without throwing away compound terms.
		if len(w) > 2 {
			tokens = append(tokens, w)
		}
	}
	if t := strings.ToLower(strings.TrimSpace(c.Problem.QueryComplexity)); t != "" {
		tokens = append(tokens, t)
	}
	return uniqueStrings(tokens)
}

// queryIndexTokens is the tokenisation for a Query — same shape as
// CaseTokens but on Query fields.
func queryIndexTokens(q Query) []string {
	tokens := make([]string, 0, 8)
	for _, k := range q.Keywords {
		if t := strings.ToLower(strings.TrimSpace(k)); t != "" {
			tokens = append(tokens, t)
		}
	}
	for _, e := range q.Entities {
		if t := strings.ToLower(strings.TrimSpace(e)); t != "" {
			tokens = append(tokens, t)
		}
	}
	if t := strings.ToLower(strings.TrimSpace(q.Intent)); t != "" {
		tokens = append(tokens, t)
	}
	if t := strings.ToLower(strings.TrimSpace(q.Domain)); t != "" {
		tokens = append(tokens, t)
	}
	if t := strings.ToLower(strings.TrimSpace(q.QueryComplexity)); t != "" {
		tokens = append(tokens, t)
	}
	return uniqueStrings(tokens)
}

// jaccard computes |intersection| / |union| over two string slices.
// Returns 0 when either side is empty (avoids divide-by-zero and
// degenerate "everything matches nothing" semantics).
func jaccard(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	setA := make(map[string]struct{}, len(a))
	for _, x := range a {
		setA[x] = struct{}{}
	}
	setB := make(map[string]struct{}, len(b))
	for _, x := range b {
		setB[x] = struct{}{}
	}
	intersect := 0
	for x := range setA {
		if _, ok := setB[x]; ok {
			intersect++
		}
	}
	union := len(setA) + len(setB) - intersect
	if union == 0 {
		return 0
	}
	return float64(intersect) / float64(union)
}

// cosineSimilarity computes the cosine of the angle between two
// vectors. Returns 0 on length mismatch or zero-magnitude vectors.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, magA, magB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		magA += float64(a[i]) * float64(a[i])
		magB += float64(b[i]) * float64(b[i])
	}
	denom := math.Sqrt(magA) * math.Sqrt(magB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}

// equalLower returns true when two strings are equal case-insensitively
// AND both non-empty (empty match is conventionally treated as no
// signal).
func equalLower(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return strings.EqualFold(a, b)
}

// lowercaseAll returns a new slice with all entries lowercased.
func lowercaseAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(s)
	}
	return out
}

// uniqueStrings preserves order and drops duplicates. Used by
// CaseTokens / queryIndexTokens so the token set is deterministic.
func uniqueStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
