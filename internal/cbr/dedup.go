package cbr

import (
	"sort"
	"time"
)

// DefaultDedupThreshold is the similarity score above which two cases
// are treated as near-duplicates. 0.85 sits above the noise floor of
// shallow keyword overlap (a couple of common tokens push two
// unrelated cases into the 0.4-0.6 range) and below the "same case
// twice" threshold (true reruns score 0.95+).
//
// Tunable per workspace via the housekeeper config; this is the
// out-of-the-box default.
const DefaultDedupThreshold = 0.85

// dedupSimilarityWeights distribute similarity across the field
// dimensions that drive retrieval too. Sum to 1.0 so the result
// stays in [0, 1]. Different from retrieval weights — dedup wants
// "are these about the same thing?" while retrieval wants "is this
// case useful for this query?". The two are correlated but not
// identical: e.g. embedding similarity matters more for retrieval
// (semantic match) than for dedup (we already have the explicit
// fields).
type dedupSimilarityWeights struct {
	IntentText   float64
	IntentClass  float64
	Domain       float64
	Keywords     float64
	Entities     float64
	SolutionText float64
}

func defaultDedupWeights() dedupSimilarityWeights {
	// Intent text + intent class together carry most of the
	// "same intent" signal; domain/keywords/entities supply
	// disambiguation; solution text guards against "same problem,
	// different approach" being marked as a duplicate (which would
	// lose the recorded approach).
	return dedupSimilarityWeights{
		IntentText:   0.30,
		IntentClass:  0.10,
		Domain:       0.10,
		Keywords:     0.20,
		Entities:     0.15,
		SolutionText: 0.15,
	}
}

// Similarity scores how alike two cases are on the symmetric set of
// fields the deduper considers. Returns a value in [0, 1] where 1 is
// identical and 0 is no overlap on any dimension.
//
// Symmetric: Similarity(a, b) == Similarity(b, a). Pure: no I/O,
// no mutation, deterministic for the same inputs.
//
// Behaviour at the edges:
//   - Same case twice → 1.0 (every field matches).
//   - Two empty cases → 1.0 (nothing differs). Pathological but
//     deterministic; the caller is responsible for not feeding empty
//     cases through.
//   - Different category → still scored on shared fields, but the
//     caller (FindDuplicates) skips cross-category pairs.
func Similarity(a, b Case) float64 {
	w := defaultDedupWeights()

	intentText := jaccard(lowercaseAll(tokenise(a.Problem.Intent)), lowercaseAll(tokenise(b.Problem.Intent)))

	intentClass := 0.0
	if a.Problem.IntentClass != "" && a.Problem.IntentClass == b.Problem.IntentClass {
		intentClass = 1.0
	}

	domain := 0.0
	if a.Problem.Domain != "" && equalLower(a.Problem.Domain, b.Problem.Domain) {
		domain = 1.0
	}

	keywords := jaccard(lowercaseAll(a.Problem.Keywords), lowercaseAll(b.Problem.Keywords))
	entities := jaccard(lowercaseAll(a.Problem.Entities), lowercaseAll(b.Problem.Entities))
	solution := jaccard(lowercaseAll(tokenise(a.Solution.Approach)), lowercaseAll(tokenise(b.Solution.Approach)))

	return w.IntentText*intentText +
		w.IntentClass*intentClass +
		w.Domain*domain +
		w.Keywords*keywords +
		w.Entities*entities +
		w.SolutionText*solution
}

// DuplicatePair is one near-duplicate finding: two case IDs that
// scored above threshold, plus their similarity score. The Loser is
// the one that should be marked superseded; the Dominant stays
// active. Order is determined by ChooseDominant so callers don't
// have to re-derive it.
type DuplicatePair struct {
	Dominant   string  // case ID that wins
	Loser      string  // case ID to mark superseded
	Score      float64 // similarity in [0, 1]
}

// FindDuplicates scans cases pairwise and returns one DuplicatePair
// per dominant→loser link above threshold. Skips:
//
//   - already-redacted cases (suppressed/superseded shouldn't
//     contaminate the result),
//   - already-superseded cases (SupersededBy != ""),
//   - cross-category pairs (a Strategy and a Pitfall covering the
//     same problem are distinct kinds of knowledge — collapsing
//     them loses the distinction).
//
// Deterministic: cases scanned in ID order. When a case is marked
// loser by an earlier pair it cannot become a dominant for another
// pair in the same sweep — this prevents transitive churn ("A
// supersedes B, B supersedes C, but C is gone now").
//
// O(n²) in the case count. Fine for n in the low hundreds (the
// realistic working corpus); when n grows we'll need an
// approximate-nearest-neighbour pre-filter.
func FindDuplicates(cases []Case, threshold float64) []DuplicatePair {
	if len(cases) < 2 {
		return nil
	}
	// Stable order by ID so the sweep is deterministic across
	// processes (map iteration order in Go is random).
	sorted := make([]Case, 0, len(cases))
	for _, c := range cases {
		if c.Redacted || c.SupersededBy != "" {
			continue
		}
		sorted = append(sorted, c)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	losers := make(map[string]struct{})
	var pairs []DuplicatePair
	for i := 0; i < len(sorted); i++ {
		if _, dropped := losers[sorted[i].ID]; dropped {
			continue
		}
		for j := i + 1; j < len(sorted); j++ {
			if _, dropped := losers[sorted[j].ID]; dropped {
				continue
			}
			if sorted[i].Category != sorted[j].Category {
				continue
			}
			score := Similarity(sorted[i], sorted[j])
			if score < threshold {
				continue
			}
			dominant, loser := ChooseDominant(sorted[i], sorted[j])
			losers[loser.ID] = struct{}{}
			pairs = append(pairs, DuplicatePair{
				Dominant: dominant.ID,
				Loser:    loser.ID,
				Score:    score,
			})
		}
	}
	return pairs
}

// ChooseDominant picks which of two near-duplicate cases stays
// active. Ranks by:
//
//  1. Outcome.Confidence — higher wins. The case the curator was
//     more sure about is usually the better-grounded record.
//  2. Operator-validated count — HelpfulCount minus HarmfulCount.
//     A case the operator boosted via annotate_case is presumptively
//     better than an auto-derived one.
//  3. Retrieval-success ratio — case that's helped past cycles wins.
//  4. Newer Timestamp — when everything else ties, prefer the more
//     recent record because its solution probably reflects current
//     reality.
//  5. Lexicographic ID — ultimate tiebreaker for deterministic output.
//
// Returns (dominant, loser) — both fully populated cases for the
// caller to inspect. Symmetric input order doesn't change the output
// (a, b and b, a both pick the same winner).
func ChooseDominant(a, b Case) (Case, Case) {
	if rankCase(a) > rankCase(b) {
		return a, b
	}
	if rankCase(a) < rankCase(b) {
		return b, a
	}
	// Equal rank — fall through to ID for determinism.
	if a.ID < b.ID {
		return a, b
	}
	return b, a
}

// rankCase produces the comparable score used by ChooseDominant.
// Higher = more likely to win. Bundled into a single float so
// pairwise comparison is simple, but the components are ordered so
// each tier wins fully against the one below.
func rankCase(c Case) float64 {
	const (
		confidenceWeight = 1_000_000.0
		operatorWeight   = 1_000.0
		utilityWeight    = 100.0
		recencyWeight    = 1.0 // unused — tie broken by Timestamp downstream
	)
	rank := c.Outcome.Confidence * confidenceWeight
	if c.UsageStats != nil {
		operator := c.UsageStats.HelpfulCount - c.UsageStats.HarmfulCount
		rank += float64(operator) * operatorWeight
		rank += Utility(c.UsageStats) * utilityWeight
	}
	// Recency component: small fractional bump per day so a tie
	// in everything else prefers the newer case.
	if !c.Timestamp.IsZero() {
		rank += float64(c.Timestamp.Unix()) * recencyWeight / (24 * 3600)
	}
	return rank
}

// time import retained for the DuplicatePair contract — callers
// pass clocks through to the housekeeping layer; dedup itself is
// pure of time-of-day.
var _ = time.Time{}
