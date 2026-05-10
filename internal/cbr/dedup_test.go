package cbr

import (
	"testing"
	"time"
)

// caseFixture builds a Case with sensible defaults so tests can
// override only the fields they care about.
func caseFixture(id string, mods ...func(*Case)) Case {
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
		Solution: Solution{Approach: "delegate to the researcher with brave_search"},
		Outcome:  Outcome{Status: StatusSuccess, Confidence: 0.7},
	}
	for _, m := range mods {
		m(&c)
	}
	return c
}

func TestSimilarity_IdenticalCasesScoreOne(t *testing.T) {
	a := caseFixture("a")
	b := caseFixture("b")
	got := Similarity(a, b)
	if got < 0.99 {
		t.Errorf("identical cases (sans ID) scored %.4f, want ~1.0", got)
	}
}

func TestSimilarity_Symmetric(t *testing.T) {
	a := caseFixture("a")
	b := caseFixture("b", func(c *Case) {
		c.Problem.Intent = "check tomorrow's weather"
		c.Problem.Keywords = []string{"weather", "tomorrow"}
		c.Problem.Entities = []string{"Dublin", "Cork"}
	})
	if Similarity(a, b) != Similarity(b, a) {
		t.Errorf("Similarity not symmetric: a→b=%.4f, b→a=%.4f", Similarity(a, b), Similarity(b, a))
	}
}

func TestSimilarity_DifferentCategoriesStillScore(t *testing.T) {
	// Similarity is content-only; category gating is the caller's
	// concern (FindDuplicates skips cross-category pairs).
	a := caseFixture("a")
	b := caseFixture("b", func(c *Case) { c.Category = CategoryPitfall })
	if Similarity(a, b) < 0.5 {
		t.Errorf("same content, different category should still overlap; got %.4f", Similarity(a, b))
	}
}

func TestSimilarity_DisjointFieldsScoreLow(t *testing.T) {
	a := caseFixture("a")
	b := caseFixture("b", func(c *Case) {
		c.Problem.IntentClass = IntentSystemCommand
		c.Problem.Intent = "restart the database service"
		c.Problem.Domain = "infra"
		c.Problem.Keywords = []string{"restart", "database", "service"}
		c.Problem.Entities = []string{"postgres"}
		c.Solution.Approach = "ssh to the host and run systemctl"
	})
	got := Similarity(a, b)
	if got > 0.2 {
		t.Errorf("disjoint cases scored %.4f, want < 0.2", got)
	}
}

func TestFindDuplicates_FindsAboveThreshold(t *testing.T) {
	a := caseFixture("a")
	// Near-duplicate: same intent + keywords + solution; minor
	// wording difference in Intent text only.
	b := caseFixture("b", func(c *Case) {
		c.Problem.Intent = "look up the weather forecast in Dublin"
	})
	c := caseFixture("c", func(cc *Case) {
		cc.Problem.IntentClass = IntentSystemCommand
		cc.Problem.Intent = "stop the worker process"
		cc.Problem.Keywords = []string{"stop", "process"}
		cc.Problem.Entities = []string{"worker"}
		cc.Solution.Approach = "kill -9"
	})
	pairs := FindDuplicates([]Case{a, b, c}, 0.7)
	if len(pairs) != 1 {
		t.Fatalf("expected 1 pair, got %d: %+v", len(pairs), pairs)
	}
	got := pairs[0]
	if !((got.Dominant == "a" && got.Loser == "b") || (got.Dominant == "b" && got.Loser == "a")) {
		t.Errorf("pair should link a/b; got %+v", got)
	}
	if got.Score < 0.7 {
		t.Errorf("score below threshold leaked through: %.4f", got.Score)
	}
}

func TestFindDuplicates_ThresholdSuppressesWeakMatches(t *testing.T) {
	a := caseFixture("a")
	b := caseFixture("b", func(c *Case) {
		// Share a single keyword; nothing else.
		c.Problem.IntentClass = IntentSystemCommand
		c.Problem.Intent = "do unrelated thing"
		c.Problem.Domain = "infra"
		c.Problem.Keywords = []string{"weather"}
		c.Problem.Entities = nil
		c.Solution.Approach = "totally different approach"
	})
	pairs := FindDuplicates([]Case{a, b}, 0.85)
	if len(pairs) != 0 {
		t.Errorf("weak match leaked above threshold: %+v", pairs)
	}
}

func TestFindDuplicates_SkipsCrossCategory(t *testing.T) {
	// Same content, different category. FindDuplicates must skip:
	// a Strategy and a Pitfall covering the same problem are
	// distinct kinds of knowledge.
	a := caseFixture("a")
	b := caseFixture("b", func(c *Case) { c.Category = CategoryPitfall })
	pairs := FindDuplicates([]Case{a, b}, 0.5)
	if len(pairs) != 0 {
		t.Errorf("cross-category pair leaked: %+v", pairs)
	}
}

func TestFindDuplicates_SkipsAlreadyRedacted(t *testing.T) {
	a := caseFixture("a")
	b := caseFixture("b", func(c *Case) { c.Redacted = true })
	pairs := FindDuplicates([]Case{a, b}, 0.7)
	if len(pairs) != 0 {
		t.Errorf("redacted case included in dedup: %+v", pairs)
	}
}

func TestFindDuplicates_SkipsAlreadySuperseded(t *testing.T) {
	a := caseFixture("a")
	b := caseFixture("b", func(c *Case) { c.SupersededBy = "some-other-id" })
	pairs := FindDuplicates([]Case{a, b}, 0.7)
	if len(pairs) != 0 {
		t.Errorf("superseded case included in dedup: %+v", pairs)
	}
}

func TestFindDuplicates_LoserCannotBecomeDominantInSameSweep(t *testing.T) {
	// Three similar cases. After A, B → A wins / B loses, B should
	// not appear as a dominant in any subsequent pair.
	a := caseFixture("a", func(c *Case) { c.Outcome.Confidence = 0.9 })
	b := caseFixture("b", func(c *Case) { c.Outcome.Confidence = 0.7 })
	c := caseFixture("c", func(cc *Case) { cc.Outcome.Confidence = 0.5 })
	pairs := FindDuplicates([]Case{a, b, c}, 0.7)
	for _, p := range pairs {
		if p.Dominant == "b" {
			t.Errorf("B was previously a loser, should not become dominant: %+v", p)
		}
	}
}

func TestFindDuplicates_DeterministicAcrossInputOrder(t *testing.T) {
	a := caseFixture("a")
	b := caseFixture("b")
	pairs1 := FindDuplicates([]Case{a, b}, 0.5)
	pairs2 := FindDuplicates([]Case{b, a}, 0.5)
	if len(pairs1) != len(pairs2) {
		t.Fatalf("different result lengths: %d vs %d", len(pairs1), len(pairs2))
	}
	for i := range pairs1 {
		if pairs1[i] != pairs2[i] {
			t.Errorf("non-deterministic result at index %d: %+v vs %+v", i, pairs1[i], pairs2[i])
		}
	}
}

func TestChooseDominant_HigherConfidenceWins(t *testing.T) {
	a := caseFixture("a", func(c *Case) { c.Outcome.Confidence = 0.9 })
	b := caseFixture("b", func(c *Case) { c.Outcome.Confidence = 0.5 })
	dominant, loser := ChooseDominant(a, b)
	if dominant.ID != "a" || loser.ID != "b" {
		t.Errorf("higher-confidence didn't win: dominant=%s loser=%s", dominant.ID, loser.ID)
	}
}

func TestChooseDominant_OperatorBoostBreaksConfidenceTie(t *testing.T) {
	a := caseFixture("a", func(c *Case) {
		c.Outcome.Confidence = 0.7
		c.UsageStats = &UsageStats{HelpfulCount: 3}
	})
	b := caseFixture("b", func(c *Case) {
		c.Outcome.Confidence = 0.7
	})
	dominant, _ := ChooseDominant(a, b)
	if dominant.ID != "a" {
		t.Errorf("operator-validated case should win on confidence tie; got dominant=%s", dominant.ID)
	}
}

func TestChooseDominant_RecencyBreaksRemainingTies(t *testing.T) {
	a := caseFixture("a", func(c *Case) { c.Timestamp = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC) })
	b := caseFixture("b", func(c *Case) { c.Timestamp = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC) })
	dominant, _ := ChooseDominant(a, b)
	if dominant.ID != "b" {
		t.Errorf("newer case should win when confidence + operator stats tie; got %s", dominant.ID)
	}
}

func TestChooseDominant_SymmetricInputOrder(t *testing.T) {
	a := caseFixture("a", func(c *Case) { c.Outcome.Confidence = 0.9 })
	b := caseFixture("b", func(c *Case) { c.Outcome.Confidence = 0.5 })
	d1, l1 := ChooseDominant(a, b)
	d2, l2 := ChooseDominant(b, a)
	if d1.ID != d2.ID || l1.ID != l2.ID {
		t.Errorf("ChooseDominant not symmetric: (a,b)=%s/%s vs (b,a)=%s/%s", d1.ID, l1.ID, d2.ID, l2.ID)
	}
}

