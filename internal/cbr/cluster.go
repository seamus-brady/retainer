package cbr

import (
	"fmt"
	"sort"
	"strings"
)

// DefaultClusterMinCases is the lower bound for a cluster to count
// as a pattern. Below 3 the signal-to-noise crashes — two cases
// matching by coincidence isn't a pattern, it's a pair.
const DefaultClusterMinCases = 3

// DefaultClusterThreshold is the structural similarity above which
// two cases get connected in the cluster graph. Lower than the
// dedup threshold (0.85) on purpose: dedup wants near-identical
// cases ("the same one twice"), clustering wants structurally
// related ones ("about the same domain + keywords"). 0.45 sits
// where shared-domain + shared-half-of-keywords pairs tend to
// land.
const DefaultClusterThreshold = 0.45

// clusterStructuralWeights are the weights used by structural
// similarity. Sum to 1.0. Different from dedup weights — clustering
// drops the intent text + intent class signals because two cases
// "about" the same thing can phrase it very differently. Domain +
// keywords + entities are the durable structural signature.
type clusterStructuralWeights struct {
	Domain   float64
	Keywords float64
	Entities float64
}

func defaultClusterWeights() clusterStructuralWeights {
	return clusterStructuralWeights{
		Domain:   0.40,
		Keywords: 0.40,
		Entities: 0.20,
	}
}

// Cluster captures one structural pattern discovered across the
// case archive. Used by the `mine_patterns` agent tool and (Phase
// 7E) by the strategies-proposal pipeline.
//
// The exemplar is the highest-confidence success case in the
// cluster, picked so the agent reading the cluster sees a concrete
// "this is what worked" rather than just structural metadata.
// Falls through to highest-confidence case overall if no success
// case exists.
type Cluster struct {
	// ID is a deterministic synthetic ID — first exemplar's case
	// ID prefix + size. Stable across runs given the same input.
	ID string
	// Size is the number of cases in the cluster.
	Size int
	// CommonDomain is the dominant domain across the cluster.
	// Empty when no single domain accounts for >= half of cases.
	CommonDomain string
	// CommonKeywords is the lowercased intersection of keywords
	// across every case in the cluster. Empty when no keyword is
	// universal.
	CommonKeywords []string
	// CommonEntities is the lowercased intersection of entities.
	CommonEntities []string
	// Exemplar is the chosen representative case — highest-
	// confidence success first, then highest-confidence overall.
	Exemplar Case
	// CaseIDs lists every case in the cluster, sorted ascending
	// for deterministic output.
	CaseIDs []string
}

// StructuralSimilarity scores how alike two cases are on the
// structural signals (domain + keywords + entities). Pure +
// symmetric. Returns a value in [0, 1].
//
// Distinct from Similarity (defined in dedup.go): clustering wants
// "share durable structural signature" while dedup wants "this is
// effectively the same record". Clustering ignores intent text
// (which varies wildly between cases that are about the same
// problem) and the solution approach (clustering across solutions
// is the whole point — cases that share a domain but tried
// different things are still in the same cluster).
func StructuralSimilarity(a, b Case) float64 {
	w := defaultClusterWeights()

	domain := 0.0
	if a.Problem.Domain != "" && equalLower(a.Problem.Domain, b.Problem.Domain) {
		domain = 1.0
	}

	keywords := jaccard(lowercaseAll(a.Problem.Keywords), lowercaseAll(b.Problem.Keywords))
	entities := jaccard(lowercaseAll(a.Problem.Entities), lowercaseAll(b.Problem.Entities))

	return w.Domain*domain + w.Keywords*keywords + w.Entities*entities
}

// FindClusters partitions cases into structural clusters via
// connected components: a graph where each case is a node and an
// edge exists between two cases scoring at or above threshold on
// StructuralSimilarity. Components below minCases are dropped.
//
// Skips redacted + superseded cases — they'd contaminate the
// pattern (a suppressed case isn't reusable knowledge) and the
// retrieval-side filtering already excludes them anyway. Cases
// without a domain end up in a "no-domain" pseudo-bucket only if
// they share enough keywords/entities with their neighbours.
//
// Result is sorted by Size descending, ties broken by CommonDomain
// alphabetical. CaseIDs within each cluster are sorted ascending.
// Deterministic given the same input.
//
// O(n²) over cases. The CBR corpus is small (low hundreds);
// when that changes we'll add an inverted-index pre-filter.
func FindClusters(cases []Case, minCases int, threshold float64) []Cluster {
	if minCases <= 0 {
		minCases = DefaultClusterMinCases
	}
	if threshold <= 0 {
		threshold = DefaultClusterThreshold
	}

	// Filter + sort by ID for stable iteration order.
	active := make([]Case, 0, len(cases))
	for _, c := range cases {
		if c.Redacted || c.SupersededBy != "" {
			continue
		}
		active = append(active, c)
	}
	sort.Slice(active, func(i, j int) bool { return active[i].ID < active[j].ID })

	if len(active) < minCases {
		return nil
	}

	// Union-find: each case starts in its own component; pairs
	// above threshold get unioned.
	parent := make([]int, len(active))
	for i := range parent {
		parent[i] = i
	}
	var find func(x int) int
	find = func(x int) int {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}

	for i := 0; i < len(active); i++ {
		for j := i + 1; j < len(active); j++ {
			if StructuralSimilarity(active[i], active[j]) >= threshold {
				union(i, j)
			}
		}
	}

	// Bucket cases by their root.
	buckets := make(map[int][]Case)
	for i, c := range active {
		root := find(i)
		buckets[root] = append(buckets[root], c)
	}

	out := make([]Cluster, 0, len(buckets))
	for _, group := range buckets {
		if len(group) < minCases {
			continue
		}
		out = append(out, buildCluster(group))
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Size != out[j].Size {
			return out[i].Size > out[j].Size
		}
		return out[i].CommonDomain < out[j].CommonDomain
	})
	return out
}

// buildCluster materialises a Cluster from a group of cases.
// Computes the common-fields summary, picks the exemplar, and
// stamps a stable ID.
func buildCluster(group []Case) Cluster {
	exemplar := pickExemplar(group)

	ids := make([]string, len(group))
	for i, c := range group {
		ids[i] = c.ID
	}
	sort.Strings(ids)

	id := exemplar.ID
	if len(id) > 8 {
		id = id[:8]
	}
	return Cluster{
		ID:             fmt.Sprintf("c-%s-%d", id, len(group)),
		Size:           len(group),
		CommonDomain:   dominantDomain(group),
		CommonKeywords: intersectLowered(group, func(c Case) []string { return c.Problem.Keywords }),
		CommonEntities: intersectLowered(group, func(c Case) []string { return c.Problem.Entities }),
		Exemplar:       exemplar,
		CaseIDs:        ids,
	}
}

// pickExemplar returns the cluster's representative case. Prefers
// success cases over partial / failure ones, and within those
// prefers higher confidence. Tiebreaker: lexicographic ID for
// determinism.
func pickExemplar(group []Case) Case {
	if len(group) == 0 {
		return Case{}
	}
	best := group[0]
	for _, c := range group[1:] {
		if exemplarRank(c) > exemplarRank(best) {
			best = c
			continue
		}
		if exemplarRank(c) == exemplarRank(best) && c.ID < best.ID {
			best = c
		}
	}
	return best
}

// exemplarRank is a comparable score: success cases rank above
// non-success regardless of confidence; within a status tier, the
// case with higher confidence wins. The 1000.0 multiplier ensures
// status dominates confidence.
func exemplarRank(c Case) float64 {
	rank := c.Outcome.Confidence
	if c.Outcome.Status == StatusSuccess {
		rank += 1000.0
	} else if c.Outcome.Status == StatusPartial {
		rank += 500.0
	}
	return rank
}

// dominantDomain returns the most-common (lowercased) domain
// across the group when it accounts for >= half of cases. Empty
// otherwise — avoids labelling a structurally-clustered group
// (matched on keywords/entities) with a misleading domain.
func dominantDomain(group []Case) string {
	if len(group) == 0 {
		return ""
	}
	counts := make(map[string]int)
	for _, c := range group {
		dom := strings.ToLower(strings.TrimSpace(c.Problem.Domain))
		if dom == "" {
			continue
		}
		counts[dom]++
	}
	var bestDom string
	bestCount := 0
	for dom, n := range counts {
		if n > bestCount {
			bestDom, bestCount = dom, n
		}
	}
	// Strict majority: more than half. For 4 cases that's >= 3,
	// for 3 cases that's >= 2. Avoids labelling a cluster's
	// "common domain" when the split is 2/2 — that's two
	// structurally-related sub-groups under a shared
	// keyword/entity signature, not a single dominant domain.
	if bestCount*2 <= len(group) {
		return ""
	}
	return bestDom
}

// intersectLowered computes the lowercase set intersection of the
// given field across the group. Returns sorted output for
// deterministic comparison.
func intersectLowered(group []Case, field func(Case) []string) []string {
	if len(group) == 0 {
		return nil
	}
	first := make(map[string]struct{})
	for _, k := range field(group[0]) {
		k = strings.ToLower(strings.TrimSpace(k))
		if k != "" {
			first[k] = struct{}{}
		}
	}
	for _, c := range group[1:] {
		next := make(map[string]struct{})
		for _, k := range field(c) {
			k = strings.ToLower(strings.TrimSpace(k))
			if _, ok := first[k]; ok {
				next[k] = struct{}{}
			}
		}
		first = next
		if len(first) == 0 {
			break
		}
	}
	out := make([]string, 0, len(first))
	for k := range first {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
