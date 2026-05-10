package remembrancer

import (
	"testing"
	"time"

	"github.com/seamus-brady/retainer/internal/cbr"
	"github.com/seamus-brady/retainer/internal/librarian"
)

// ---- Search ----

func TestSearch_EmptyKeywordsReturnsInput(t *testing.T) {
	in := []librarian.NarrativeEntry{{Summary: "anything"}}
	out := Search(in, nil)
	if len(out) != 1 {
		t.Errorf("nil keywords should pass through; got %d", len(out))
	}
	out = Search(in, []string{""})
	if len(out) != 1 {
		t.Errorf("empty keyword should pass through; got %d", len(out))
	}
}

func TestSearch_MatchesSummary(t *testing.T) {
	in := []librarian.NarrativeEntry{
		{Summary: "investigated the auth flow"},
		{Summary: "wrote a summary"},
	}
	got := Search(in, []string{"auth"})
	if len(got) != 1 || got[0].Summary != "investigated the auth flow" {
		t.Errorf("got %+v", got)
	}
}

func TestSearch_CaseInsensitive(t *testing.T) {
	in := []librarian.NarrativeEntry{{Summary: "AUTH issue"}}
	got := Search(in, []string{"auth"})
	if len(got) != 1 {
		t.Errorf("case-insensitive search failed; got %+v", got)
	}
}

func TestSearch_MatchesKeywords(t *testing.T) {
	in := []librarian.NarrativeEntry{
		{Summary: "some work", Keywords: []string{"oauth", "session"}},
	}
	got := Search(in, []string{"oauth"})
	if len(got) != 1 {
		t.Errorf("keyword match failed; got %+v", got)
	}
}

func TestSearch_MatchesDomain(t *testing.T) {
	in := []librarian.NarrativeEntry{{Summary: "x", Domain: "auth"}}
	got := Search(in, []string{"auth"})
	if len(got) != 1 {
		t.Errorf("domain match failed; got %+v", got)
	}
}

// ---- FindConnections ----

func TestFindConnections_EmptyTopicReturnsZero(t *testing.T) {
	r := FindConnections(nil, nil, nil, "")
	if r.Counts.Narrative != 0 || r.Counts.Facts != 0 || r.Counts.Cases != 0 {
		t.Errorf("empty topic should return zero counts; got %+v", r)
	}
}

func TestFindConnections_CountsAndSamples(t *testing.T) {
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	narrative := []librarian.NarrativeEntry{
		{CycleID: "n1", Timestamp: now, Summary: "auth flow"},
		{CycleID: "n2", Timestamp: now.Add(time.Hour), Summary: "different topic"},
		{CycleID: "n3", Timestamp: now.Add(2 * time.Hour), Summary: "auth refactor"},
		{CycleID: "n4", Timestamp: now.Add(3 * time.Hour), Summary: "auth doc"},
		{CycleID: "n5", Timestamp: now.Add(4 * time.Hour), Summary: "auth review"},
	}
	facts := []librarian.Fact{
		{Key: "auth.token", Value: "xyz", Timestamp: now},
		{Key: "user.name", Value: "Seamus", Timestamp: now},
	}
	cases := []cbr.Case{
		{ID: "c1", Problem: cbr.Problem{Domain: "auth", Intent: "debug login"}},
		{ID: "c2", Problem: cbr.Problem{Domain: "research"}},
	}
	r := FindConnections(narrative, facts, cases, "auth")
	if r.Counts.Narrative != 4 {
		t.Errorf("narrative count = %d, want 4", r.Counts.Narrative)
	}
	if r.Counts.Facts != 1 {
		t.Errorf("facts count = %d, want 1", r.Counts.Facts)
	}
	if r.Counts.Cases != 1 {
		t.Errorf("cases count = %d, want 1", r.Counts.Cases)
	}
	// Samples capped at 3.
	if len(r.NarrativeSamples) > connectionSampleSize {
		t.Errorf("narrative samples not capped; got %d", len(r.NarrativeSamples))
	}
	// Most-recent ordering — first sample should be n5 (latest).
	if r.NarrativeSamples[0].CycleID != "n5" {
		t.Errorf("first sample = %q, want n5 (most-recent)", r.NarrativeSamples[0].CycleID)
	}
}

// MinePatterns coverage moved to internal/cbr/cluster_test.go
// (Phase 5). The cog tool path now calls cbr.FindClusters; the
// remembrancer package no longer hosts the clustering code.

// ---- Consolidate ----

func TestConsolidate_NoTopicCountsEverything(t *testing.T) {
	s := Consolidate(
		[]librarian.NarrativeEntry{{Summary: "a"}, {Summary: "b"}},
		[]librarian.Fact{{Key: "k", Value: "v"}},
		[]cbr.Case{{ID: "c1"}},
		"",
	)
	if s.NarrativeEntries != 2 || s.Facts != 1 || s.Cases != 1 {
		t.Errorf("got %+v", s)
	}
}

func TestConsolidate_TopicFiltersNarrativeAndCases(t *testing.T) {
	s := Consolidate(
		[]librarian.NarrativeEntry{{Summary: "auth issue"}, {Summary: "unrelated"}},
		[]librarian.Fact{{Key: "k", Value: "v"}},
		[]cbr.Case{
			{ID: "c1", Problem: cbr.Problem{Domain: "auth"}},
			{ID: "c2", Problem: cbr.Problem{Domain: "research"}},
		},
		"auth",
	)
	if s.NarrativeEntries != 1 {
		t.Errorf("narrative filter failed; got %d", s.NarrativeEntries)
	}
	if s.Cases != 1 {
		t.Errorf("case filter failed; got %d", s.Cases)
	}
	// Facts are unfiltered (current shape — keys aren't topic-scoped today).
	if s.Facts != 1 {
		t.Errorf("facts shouldn't be topic-filtered; got %d", s.Facts)
	}
}

func TestConsolidate_SamplesCappedAndMostRecent(t *testing.T) {
	now := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	var n []librarian.NarrativeEntry
	for i := 0; i < 10; i++ {
		n = append(n, librarian.NarrativeEntry{CycleID: itoa(i), Timestamp: now.Add(time.Duration(i) * time.Hour), Summary: "entry"})
	}
	s := Consolidate(n, nil, nil, "")
	if len(s.NarrativeSamples) != consolidateSampleSize {
		t.Errorf("sample count = %d, want %d", len(s.NarrativeSamples), consolidateSampleSize)
	}
	// Last sample should be the most-recent input (CycleID "9").
	last := s.NarrativeSamples[len(s.NarrativeSamples)-1].CycleID
	if last != "9" {
		t.Errorf("last sample = %q, want 9 (most-recent)", last)
	}
}

// ---- helper ----

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
