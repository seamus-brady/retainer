package cbr

import (
	"testing"
	"time"
)

// pruneFixture builds a Case that satisfies IsPrunable's gate by
// default (failure status, no pitfalls, old enough). Tests override
// to test rule violations.
func pruneFixture(id string, mods ...func(*Case)) Case {
	c := Case{
		ID:        id,
		Timestamp: time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC),
		Category:  CategoryPitfall,
		Outcome:   Outcome{Status: StatusFailure},
	}
	for _, m := range mods {
		m(&c)
	}
	return c
}

func TestIsPrunable_FailureWithoutLessonOldEnough(t *testing.T) {
	now := time.Date(2026, 5, 5, 9, 0, 0, 0, time.UTC) // ~34d after fixture
	c := pruneFixture("a")
	if !IsPrunable(c, now, DefaultPruneMaxFailureAge) {
		t.Errorf("clean prune candidate not flagged")
	}
}

func TestIsPrunable_NotFailureNeverPrunes(t *testing.T) {
	now := time.Date(2026, 5, 5, 9, 0, 0, 0, time.UTC)
	for _, status := range []Status{StatusSuccess, StatusPartial} {
		c := pruneFixture("a", func(cc *Case) { cc.Outcome.Status = status })
		if IsPrunable(c, now, DefaultPruneMaxFailureAge) {
			t.Errorf("status=%q flagged for prune (must not be)", status)
		}
	}
}

func TestIsPrunable_HasPitfallsRescuesCase(t *testing.T) {
	now := time.Date(2026, 5, 5, 9, 0, 0, 0, time.UTC)
	c := pruneFixture("a", func(cc *Case) {
		cc.Outcome.Pitfalls = []string{"don't ssh as root again"}
	})
	if IsPrunable(c, now, DefaultPruneMaxFailureAge) {
		t.Errorf("case with pitfall should not be prunable — pitfall IS the lesson")
	}
}

func TestIsPrunable_HelpfulCountRescuesCase(t *testing.T) {
	now := time.Date(2026, 5, 5, 9, 0, 0, 0, time.UTC)
	c := pruneFixture("a", func(cc *Case) {
		cc.UsageStats = &UsageStats{HelpfulCount: 1}
	})
	if IsPrunable(c, now, DefaultPruneMaxFailureAge) {
		t.Errorf("operator-validated case must not be auto-prunable")
	}
}

func TestIsPrunable_TooRecent(t *testing.T) {
	now := time.Date(2026, 4, 5, 9, 0, 0, 0, time.UTC) // 4d after fixture
	c := pruneFixture("a")
	if IsPrunable(c, now, DefaultPruneMaxFailureAge) {
		t.Errorf("too-recent failure flagged for prune")
	}
}

func TestIsPrunable_AlreadyRedacted(t *testing.T) {
	now := time.Date(2026, 5, 5, 9, 0, 0, 0, time.UTC)
	c := pruneFixture("a", func(cc *Case) { cc.Redacted = true })
	if IsPrunable(c, now, DefaultPruneMaxFailureAge) {
		t.Errorf("redacted case re-flagged")
	}
}

func TestIsPrunable_AlreadySuperseded(t *testing.T) {
	now := time.Date(2026, 5, 5, 9, 0, 0, 0, time.UTC)
	c := pruneFixture("a", func(cc *Case) { cc.SupersededBy = "other-id" })
	if IsPrunable(c, now, DefaultPruneMaxFailureAge) {
		t.Errorf("superseded case re-flagged")
	}
}

func TestIsPrunable_CategoryEmptySkipped(t *testing.T) {
	now := time.Date(2026, 5, 5, 9, 0, 0, 0, time.UTC)
	c := pruneFixture("a", func(cc *Case) { cc.Category = "" })
	if IsPrunable(c, now, DefaultPruneMaxFailureAge) {
		t.Errorf("categoryless (Conversation) case flagged for prune")
	}
}

func TestIsPrunable_ZeroOrNegativeMaxAgeReturnsFalse(t *testing.T) {
	now := time.Date(2026, 5, 5, 9, 0, 0, 0, time.UTC)
	c := pruneFixture("a")
	for _, age := range []time.Duration{0, -1 * time.Second, -24 * time.Hour} {
		if IsPrunable(c, now, age) {
			t.Errorf("non-positive maxAge=%v should produce false", age)
		}
	}
}

func TestIsPrunable_ZeroTimestampSkipped(t *testing.T) {
	now := time.Date(2026, 5, 5, 9, 0, 0, 0, time.UTC)
	c := pruneFixture("a", func(cc *Case) { cc.Timestamp = time.Time{} })
	if IsPrunable(c, now, DefaultPruneMaxFailureAge) {
		t.Errorf("zero-timestamp case must be conservative (false)")
	}
}

func TestFindPrunable_SortsAndFilters(t *testing.T) {
	now := time.Date(2026, 5, 5, 9, 0, 0, 0, time.UTC)
	cases := []Case{
		pruneFixture("c"),
		pruneFixture("a"),
		pruneFixture("rescued", func(cc *Case) { cc.Outcome.Pitfalls = []string{"learned"} }),
		pruneFixture("b"),
	}
	got := FindPrunable(cases, now, DefaultPruneMaxFailureAge)
	if len(got) != 3 {
		t.Fatalf("got %d prunable, want 3 (rescued is excluded): %+v", len(got), idsOf(got))
	}
	want := []string{"a", "b", "c"}
	for i, w := range want {
		if got[i].ID != w {
			t.Errorf("position %d = %q, want %q (deterministic ID order)", i, got[i].ID, w)
		}
	}
}

func TestFindPrunable_EmptyInput(t *testing.T) {
	if got := FindPrunable(nil, time.Now(), DefaultPruneMaxFailureAge); got != nil {
		t.Errorf("nil input should return nil, got %v", got)
	}
	if got := FindPrunable([]Case{}, time.Now(), DefaultPruneMaxFailureAge); got != nil {
		t.Errorf("empty input should return nil, got %v", got)
	}
}

func idsOf(cs []Case) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}
