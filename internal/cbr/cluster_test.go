package cbr

import (
	"strings"
	"testing"
	"time"
)

// clusterFixture builds a Case with the structural fields the
// clusterer cares about. Defaults to a "weather forecast" shape;
// tests override per scenario.
func clusterFixture(id string, mods ...func(*Case)) Case {
	c := Case{
		ID:        id,
		Timestamp: time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC),
		Category:  CategoryStrategy,
		Problem: Problem{
			IntentClass: IntentExploration,
			Intent:      "look up the weather forecast",
			Domain:      "weather",
			Keywords:    []string{"weather", "forecast"},
			Entities:    []string{"Dublin"},
		},
		Outcome: Outcome{Status: StatusSuccess, Confidence: 0.7},
	}
	for _, m := range mods {
		m(&c)
	}
	return c
}

func TestStructuralSimilarity_IdenticalScoreOne(t *testing.T) {
	a := clusterFixture("a")
	b := clusterFixture("b")
	got := StructuralSimilarity(a, b)
	if got < 0.99 {
		t.Errorf("identical structure scored %.4f, want ~1.0", got)
	}
}

func TestStructuralSimilarity_Symmetric(t *testing.T) {
	a := clusterFixture("a")
	b := clusterFixture("b", func(c *Case) {
		c.Problem.Keywords = []string{"weather", "rain"}
	})
	if StructuralSimilarity(a, b) != StructuralSimilarity(b, a) {
		t.Errorf("not symmetric: %.4f vs %.4f",
			StructuralSimilarity(a, b), StructuralSimilarity(b, a))
	}
}

func TestStructuralSimilarity_IgnoresIntentText(t *testing.T) {
	// Intent text is the same; domain/keywords/entities differ.
	// Score should reflect ONLY domain/keywords/entities — not the
	// intent text.
	a := clusterFixture("a")
	b := clusterFixture("b", func(c *Case) {
		c.Problem.Intent = "look up the weather forecast" // identical intent text
		c.Problem.Domain = "infra"
		c.Problem.Keywords = []string{"server", "restart"}
		c.Problem.Entities = []string{"db-1"}
	})
	if got := StructuralSimilarity(a, b); got > 0.05 {
		t.Errorf("clustering must ignore intent text; got %.4f", got)
	}
}

func TestFindClusters_ConnectsSimilarCases(t *testing.T) {
	cases := []Case{
		clusterFixture("a"),
		clusterFixture("b"),
		clusterFixture("c"),
	}
	got := FindClusters(cases, 3, DefaultClusterThreshold)
	if len(got) != 1 {
		t.Fatalf("got %d clusters, want 1: %+v", len(got), got)
	}
	if got[0].Size != 3 {
		t.Errorf("size = %d, want 3", got[0].Size)
	}
	if got[0].CommonDomain != "weather" {
		t.Errorf("CommonDomain = %q, want %q", got[0].CommonDomain, "weather")
	}
}

func TestFindClusters_RespectsMinCases(t *testing.T) {
	// Two similar cases — below the default minCases of 3.
	cases := []Case{
		clusterFixture("a"),
		clusterFixture("b"),
	}
	got := FindClusters(cases, 3, DefaultClusterThreshold)
	if len(got) != 0 {
		t.Errorf("min-cases not enforced: %+v", got)
	}
}

func TestFindClusters_SeparatesUnrelatedGroups(t *testing.T) {
	// Three weather cases + three infra cases. Should produce two
	// clusters.
	cases := []Case{
		clusterFixture("w1"),
		clusterFixture("w2"),
		clusterFixture("w3"),
		clusterFixture("i1", func(c *Case) {
			c.Problem.Domain = "infra"
			c.Problem.Keywords = []string{"server", "restart"}
			c.Problem.Entities = []string{"db-1"}
		}),
		clusterFixture("i2", func(c *Case) {
			c.Problem.Domain = "infra"
			c.Problem.Keywords = []string{"server", "restart"}
			c.Problem.Entities = []string{"db-2"}
		}),
		clusterFixture("i3", func(c *Case) {
			c.Problem.Domain = "infra"
			c.Problem.Keywords = []string{"server", "restart"}
			c.Problem.Entities = []string{"db-3"}
		}),
	}
	got := FindClusters(cases, 3, DefaultClusterThreshold)
	if len(got) != 2 {
		t.Fatalf("got %d clusters, want 2: %+v", len(got), got)
	}
	domains := []string{got[0].CommonDomain, got[1].CommonDomain}
	if !contains(domains, "weather") || !contains(domains, "infra") {
		t.Errorf("domains = %v, want both weather and infra", domains)
	}
}

func TestFindClusters_SkipsRedacted(t *testing.T) {
	cases := []Case{
		clusterFixture("a"),
		clusterFixture("b", func(c *Case) { c.Redacted = true }),
		clusterFixture("c"),
	}
	got := FindClusters(cases, 2, DefaultClusterThreshold)
	for _, cl := range got {
		for _, id := range cl.CaseIDs {
			if id == "b" {
				t.Errorf("redacted case b leaked into cluster: %+v", cl)
			}
		}
	}
}

func TestFindClusters_SkipsSuperseded(t *testing.T) {
	cases := []Case{
		clusterFixture("a"),
		clusterFixture("b", func(c *Case) { c.SupersededBy = "a" }),
		clusterFixture("c"),
	}
	got := FindClusters(cases, 2, DefaultClusterThreshold)
	for _, cl := range got {
		for _, id := range cl.CaseIDs {
			if id == "b" {
				t.Errorf("superseded case b leaked into cluster: %+v", cl)
			}
		}
	}
}

func TestFindClusters_PicksSuccessExemplar(t *testing.T) {
	// Mix of success + failure with the same structure. Exemplar
	// must be a success case.
	cases := []Case{
		clusterFixture("a", func(c *Case) {
			c.Outcome.Status = StatusFailure
			c.Outcome.Confidence = 0.95 // high but failed
		}),
		clusterFixture("b", func(c *Case) {
			c.Outcome.Status = StatusSuccess
			c.Outcome.Confidence = 0.6
		}),
		clusterFixture("c", func(c *Case) {
			c.Outcome.Status = StatusFailure
			c.Outcome.Confidence = 0.9
		}),
	}
	got := FindClusters(cases, 3, DefaultClusterThreshold)
	if len(got) != 1 {
		t.Fatalf("got %d clusters, want 1", len(got))
	}
	if got[0].Exemplar.Outcome.Status != StatusSuccess {
		t.Errorf("exemplar status = %q, want success", got[0].Exemplar.Outcome.Status)
	}
	if got[0].Exemplar.ID != "b" {
		t.Errorf("exemplar = %q, want %q (only success case)", got[0].Exemplar.ID, "b")
	}
}

func TestFindClusters_FallsBackToHighestConfidenceWhenNoSuccess(t *testing.T) {
	cases := []Case{
		clusterFixture("a", func(c *Case) {
			c.Outcome.Status = StatusFailure
			c.Outcome.Confidence = 0.4
		}),
		clusterFixture("b", func(c *Case) {
			c.Outcome.Status = StatusFailure
			c.Outcome.Confidence = 0.8
		}),
		clusterFixture("c", func(c *Case) {
			c.Outcome.Status = StatusFailure
			c.Outcome.Confidence = 0.6
		}),
	}
	got := FindClusters(cases, 3, DefaultClusterThreshold)
	if got[0].Exemplar.ID != "b" {
		t.Errorf("exemplar = %q, want %q (highest confidence among failures)",
			got[0].Exemplar.ID, "b")
	}
}

func TestFindClusters_CommonKeywordsIntersection(t *testing.T) {
	cases := []Case{
		clusterFixture("a", func(c *Case) {
			c.Problem.Keywords = []string{"weather", "forecast", "Dublin"}
		}),
		clusterFixture("b", func(c *Case) {
			c.Problem.Keywords = []string{"weather", "forecast", "today"}
		}),
		clusterFixture("c", func(c *Case) {
			c.Problem.Keywords = []string{"weather", "forecast", "Cork"}
		}),
	}
	got := FindClusters(cases, 3, DefaultClusterThreshold)
	if len(got) != 1 {
		t.Fatalf("expected 1 cluster, got %+v", got)
	}
	want := []string{"forecast", "weather"}
	if !equalStrings(got[0].CommonKeywords, want) {
		t.Errorf("CommonKeywords = %v, want %v", got[0].CommonKeywords, want)
	}
}

func TestFindClusters_DominantDomainNeedsMajority(t *testing.T) {
	// 3 weather + 1 infra in one cluster. weather wins (majority).
	cases := []Case{
		clusterFixture("a"),
		clusterFixture("b"),
		clusterFixture("c"),
		clusterFixture("d", func(c *Case) {
			c.Problem.Domain = "infra"
			// Force connection to weather cluster via shared
			// keywords/entities.
			c.Problem.Keywords = []string{"weather", "forecast"}
			c.Problem.Entities = []string{"Dublin"}
		}),
	}
	got := FindClusters(cases, 3, DefaultClusterThreshold)
	if len(got) != 1 {
		t.Fatalf("got %d clusters, want 1", len(got))
	}
	if got[0].CommonDomain != "weather" {
		t.Errorf("CommonDomain = %q, want %q (weather is majority 3/4)",
			got[0].CommonDomain, "weather")
	}
}

func TestFindClusters_NoMajorityDomainEmptyString(t *testing.T) {
	// 2 weather + 2 infra in one cluster. No majority.
	cases := []Case{
		clusterFixture("a"),
		clusterFixture("b"),
		clusterFixture("c", func(c *Case) {
			c.Problem.Domain = "infra"
			c.Problem.Keywords = []string{"weather", "forecast"}
			c.Problem.Entities = []string{"Dublin"}
		}),
		clusterFixture("d", func(c *Case) {
			c.Problem.Domain = "infra"
			c.Problem.Keywords = []string{"weather", "forecast"}
			c.Problem.Entities = []string{"Dublin"}
		}),
	}
	got := FindClusters(cases, 3, DefaultClusterThreshold)
	if len(got) != 1 {
		t.Fatalf("got %d clusters, want 1", len(got))
	}
	if got[0].CommonDomain != "" {
		t.Errorf("CommonDomain = %q, want empty (no majority)", got[0].CommonDomain)
	}
}

func TestFindClusters_DeterministicAcrossInputOrder(t *testing.T) {
	cases1 := []Case{clusterFixture("a"), clusterFixture("b"), clusterFixture("c")}
	cases2 := []Case{clusterFixture("c"), clusterFixture("a"), clusterFixture("b")}
	got1 := FindClusters(cases1, 3, DefaultClusterThreshold)
	got2 := FindClusters(cases2, 3, DefaultClusterThreshold)
	if len(got1) != len(got2) {
		t.Fatalf("len mismatch: %d vs %d", len(got1), len(got2))
	}
	for i := range got1 {
		if got1[i].ID != got2[i].ID {
			t.Errorf("non-deterministic ID at %d: %q vs %q", i, got1[i].ID, got2[i].ID)
		}
		if !equalStrings(got1[i].CaseIDs, got2[i].CaseIDs) {
			t.Errorf("non-deterministic CaseIDs at %d: %v vs %v", i, got1[i].CaseIDs, got2[i].CaseIDs)
		}
	}
}

func TestFindClusters_SortsBySizeDescending(t *testing.T) {
	// Build two clusters of different sizes.
	cases := []Case{
		clusterFixture("w1"),
		clusterFixture("w2"),
		clusterFixture("w3"),
		clusterFixture("w4"),
		clusterFixture("i1", func(c *Case) {
			c.Problem.Domain = "infra"
			c.Problem.Keywords = []string{"server", "restart"}
			c.Problem.Entities = []string{"db-1"}
		}),
		clusterFixture("i2", func(c *Case) {
			c.Problem.Domain = "infra"
			c.Problem.Keywords = []string{"server", "restart"}
			c.Problem.Entities = []string{"db-2"}
		}),
		clusterFixture("i3", func(c *Case) {
			c.Problem.Domain = "infra"
			c.Problem.Keywords = []string{"server", "restart"}
			c.Problem.Entities = []string{"db-3"}
		}),
	}
	got := FindClusters(cases, 3, DefaultClusterThreshold)
	if len(got) != 2 {
		t.Fatalf("got %d clusters, want 2", len(got))
	}
	if got[0].Size <= got[1].Size {
		t.Errorf("not sorted by size desc: sizes %d,%d", got[0].Size, got[1].Size)
	}
}

func TestFindClusters_EmptyInputReturnsNil(t *testing.T) {
	if got := FindClusters(nil, 3, DefaultClusterThreshold); got != nil {
		t.Errorf("nil input: got %v", got)
	}
	if got := FindClusters([]Case{}, 3, DefaultClusterThreshold); got != nil {
		t.Errorf("empty input: got %v", got)
	}
}

func TestFindClusters_BelowMinReturnsNil(t *testing.T) {
	cases := []Case{clusterFixture("a"), clusterFixture("b")}
	if got := FindClusters(cases, 3, DefaultClusterThreshold); got != nil {
		t.Errorf("got %+v, want nil (below min)", got)
	}
}

func TestFindClusters_ClusterIDIsStableAndShortened(t *testing.T) {
	cases := []Case{
		clusterFixture("abcd1234-5678-9abc"),
		clusterFixture("efgh5678-9abc-def0"),
		clusterFixture("ijkl9abc-def0-1234"),
	}
	got := FindClusters(cases, 3, DefaultClusterThreshold)
	if len(got) != 1 {
		t.Fatalf("got %d clusters, want 1", len(got))
	}
	if !strings.HasPrefix(got[0].ID, "c-") {
		t.Errorf("ID prefix = %q, want 'c-...'", got[0].ID)
	}
	// Exemplar by ID is "abcd1234-5678-9abc" (highest confidence
	// success defaults to alphabetical tiebreaker).
	if !strings.Contains(got[0].ID, "abcd1234") {
		t.Errorf("ID = %q, want contains 'abcd1234' (exemplar prefix)", got[0].ID)
	}
}

func TestFindClusters_ZeroParametersUseDefaults(t *testing.T) {
	cases := []Case{clusterFixture("a"), clusterFixture("b"), clusterFixture("c")}
	// Both minCases=0 and threshold=0 should fall back to defaults.
	got := FindClusters(cases, 0, 0)
	if len(got) != 1 {
		t.Errorf("zero params should use defaults; got %+v", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
