package librarian

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func runLibrarian(t *testing.T, dir string) *Librarian {
	t.Helper()
	l, err := New(Options{DataDir: dir, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go l.Run(ctx)
	return l
}

// ---- narrative ----

func TestNarrative_RecordAndRecent(t *testing.T) {
	dir := t.TempDir()
	l := runLibrarian(t, dir)

	for i := 0; i < 5; i++ {
		l.RecordNarrative(NarrativeEntry{
			CycleID: cycleIDFromInt(i),
			Status:  NarrativeStatusComplete,
			Summary: "summary " + cycleIDFromInt(i),
		})
	}
	// Allow async writes to finish.
	time.Sleep(20 * time.Millisecond)

	got := l.RecentNarrative(3)
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3", len(got))
	}
	// Newest last; with cycle IDs 0..4, most recent 3 should be 2, 3, 4.
	for i, want := range []string{cycleIDFromInt(2), cycleIDFromInt(3), cycleIDFromInt(4)} {
		if got[i].CycleID != want {
			t.Errorf("entry[%d] = %q, want %q", i, got[i].CycleID, want)
		}
	}
}

func TestNarrative_PersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()

	l1 := runLibrarian(t, dir)
	l1.RecordNarrative(NarrativeEntry{CycleID: "c1", Status: NarrativeStatusComplete, Summary: "first"})
	time.Sleep(20 * time.Millisecond)

	// Build a second librarian against the same dir; it should replay the
	// JSONL written by the first.
	l2 := runLibrarian(t, dir)
	got := l2.RecentNarrative(10)
	found := false
	for _, e := range got {
		if e.CycleID == "c1" && e.Summary == "first" {
			found = true
		}
	}
	if !found {
		t.Fatalf("entry c1 not replayed; got %+v", got)
	}
}

func TestNarrative_TruncationRecoverySkipsBadLines(t *testing.T) {
	dir := t.TempDir()
	narrDir := filepath.Join(dir, NarrativeSubdir)
	if err := os.MkdirAll(narrDir, 0o755); err != nil {
		t.Fatal(err)
	}
	today := time.Now().Format("2006-01-02")
	body := `{"cycle_id":"c1","status":"complete","summary":"good","timestamp":"2026-04-30T00:00:00Z"}
{ this line is malformed
{"cycle_id":"c2","status":"complete","summary":"also good","timestamp":"2026-04-30T00:00:01Z"}
`
	if err := os.WriteFile(filepath.Join(narrDir, today+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	l := runLibrarian(t, dir)
	got := l.RecentNarrative(10)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2 (malformed line skipped); got %+v", len(got), got)
	}
}

func TestNarrative_RichFieldsRoundTripThroughIndex(t *testing.T) {
	// Phase 2C: rich fields (Intent, Outcome, DelegationChain,
	// Topics, Entities, Metrics) must survive the SQLite index
	// round-trip — without the body column they were silently
	// dropped.
	dir := t.TempDir()
	l := runLibrarian(t, dir)

	l.RecordNarrative(NarrativeEntry{
		CycleID:   "rich-1",
		Timestamp: time.Now(),
		Summary:   "ran a comparison",
		Intent: Intent{
			Classification: IntentComparison,
			Description:    "compare two metrics",
			Domain:         "weather",
		},
		Outcome: Outcome{
			Status:     OutcomeSuccess,
			Confidence: 0.9,
			Assessment: "looks right",
		},
		DelegationChain: []DelegationStep{
			{Agent: "researcher", AgentCycleID: "agent-cyc-1", Instruction: "look it up", OutcomeText: "found", ToolsUsed: []string{"brave_search"}},
		},
		Topics:   []string{"weather", "comparison"},
		Entities: Entities{Locations: []string{"Dublin"}},
		Metrics: Metrics{
			TotalDurationMs: 1234,
			InputTokens:     100,
			ToolCalls:       2,
			ModelUsed:       "claude-opus-4-7",
		},
	})
	time.Sleep(30 * time.Millisecond)

	got := l.RecentNarrative(10)
	var found *NarrativeEntry
	for i := range got {
		if got[i].CycleID == "rich-1" {
			found = &got[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("rich-1 not in recent: %+v", got)
	}
	if found.Intent.Classification != IntentComparison {
		t.Errorf("Intent.Classification = %q", found.Intent.Classification)
	}
	if found.Intent.Domain != "weather" {
		t.Errorf("Intent.Domain = %q", found.Intent.Domain)
	}
	if found.Outcome.Status != OutcomeSuccess || found.Outcome.Confidence != 0.9 {
		t.Errorf("Outcome = %+v", found.Outcome)
	}
	if len(found.DelegationChain) != 1 || found.DelegationChain[0].Agent != "researcher" {
		t.Errorf("DelegationChain = %+v", found.DelegationChain)
	}
	if len(found.Topics) != 2 {
		t.Errorf("Topics = %v", found.Topics)
	}
	if len(found.Entities.Locations) != 1 || found.Entities.Locations[0] != "Dublin" {
		t.Errorf("Entities.Locations = %v", found.Entities.Locations)
	}
	if found.Metrics.TotalDurationMs != 1234 || found.Metrics.ModelUsed != "claude-opus-4-7" {
		t.Errorf("Metrics = %+v", found.Metrics)
	}
}

func TestNarrative_RichFieldsSurviveReplay(t *testing.T) {
	// JSONL on disk is the source of truth; replay must rebuild
	// the rich body column so a fresh process sees the same shape.
	dir := t.TempDir()

	l1 := runLibrarian(t, dir)
	l1.RecordNarrative(NarrativeEntry{
		CycleID:   "rich-replay",
		Timestamp: time.Now(),
		Summary:   "test",
		Intent:    Intent{Classification: IntentDataQuery, Domain: "weather"},
		Outcome:   Outcome{Status: OutcomePartial, Confidence: 0.5},
		Metrics:   Metrics{TotalDurationMs: 99, InputTokens: 7},
	})
	time.Sleep(30 * time.Millisecond)

	l2 := runLibrarian(t, dir)
	got := l2.RecentNarrative(10)
	var e *NarrativeEntry
	for i := range got {
		if got[i].CycleID == "rich-replay" {
			e = &got[i]
			break
		}
	}
	if e == nil {
		t.Fatalf("entry not replayed: %+v", got)
	}
	if e.Intent.Classification != IntentDataQuery || e.Intent.Domain != "weather" {
		t.Errorf("Intent = %+v", e.Intent)
	}
	if e.Outcome.Status != OutcomePartial || e.Outcome.Confidence != 0.5 {
		t.Errorf("Outcome = %+v", e.Outcome)
	}
	if e.Metrics.TotalDurationMs != 99 || e.Metrics.InputTokens != 7 {
		t.Errorf("Metrics = %+v", e.Metrics)
	}
}

// ---- facts ----

func TestFacts_RecordAndGet(t *testing.T) {
	dir := t.TempDir()
	l := runLibrarian(t, dir)

	l.RecordFact(Fact{Key: "user_name", Value: "Seamus", Scope: FactScopePersistent, Confidence: 1.0})
	time.Sleep(20 * time.Millisecond)

	got := l.GetFact("user_name")
	if got == nil {
		t.Fatal("got nil; expected fact")
	}
	if got.Value != "Seamus" {
		t.Errorf("value = %q", got.Value)
	}
	if got.Confidence != 1.0 {
		t.Errorf("confidence = %f, want 1.0 (no decay configured)", got.Confidence)
	}
}

func TestFacts_PersistentCount_EmptyDB(t *testing.T) {
	l := runLibrarian(t, t.TempDir())
	if got := l.PersistentFactCount(); got != 0 {
		t.Fatalf("count = %d, want 0", got)
	}
}

func TestFacts_PersistentCount_DistinctKeys(t *testing.T) {
	l := runLibrarian(t, t.TempDir())
	l.RecordFact(Fact{Key: "a", Value: "1", Scope: FactScopePersistent, Confidence: 1.0})
	l.RecordFact(Fact{Key: "b", Value: "2", Scope: FactScopePersistent, Confidence: 1.0})
	l.RecordFact(Fact{Key: "session_only", Value: "3", Scope: FactScopeSession, Confidence: 1.0})
	if got := l.PersistentFactCount(); got != 2 {
		t.Fatalf("count = %d, want 2 (session-scoped excluded)", got)
	}
}

func TestFacts_PersistentCount_SupersededByLaterSessionWrite(t *testing.T) {
	l := runLibrarian(t, t.TempDir())
	// "mode" starts persistent, then a later session-scoped write
	// supersedes it. The most-recent-per-key rule means it's no longer
	// persistent.
	l.RecordFact(Fact{Key: "mode", Value: "deep", Scope: FactScopePersistent, Confidence: 1.0,
		Timestamp: time.Now().Add(-time.Hour)})
	l.RecordFact(Fact{Key: "mode", Value: "shallow", Scope: FactScopeSession, Confidence: 1.0,
		Timestamp: time.Now()})
	if got := l.PersistentFactCount(); got != 0 {
		t.Fatalf("count = %d, want 0 (session write supersedes)", got)
	}
}

func TestFacts_PersistentCount_LaterPersistentWriteCounts(t *testing.T) {
	l := runLibrarian(t, t.TempDir())
	// Two writes to same key, both persistent — counts as 1 distinct key.
	l.RecordFact(Fact{Key: "k", Value: "v1", Scope: FactScopePersistent, Confidence: 1.0,
		Timestamp: time.Now().Add(-time.Hour)})
	l.RecordFact(Fact{Key: "k", Value: "v2", Scope: FactScopePersistent, Confidence: 0.5,
		Timestamp: time.Now()})
	if got := l.PersistentFactCount(); got != 1 {
		t.Fatalf("count = %d, want 1", got)
	}
}

func TestFacts_RecentPersistent_OrdersByTimestampDesc(t *testing.T) {
	l := runLibrarian(t, t.TempDir())
	t0 := time.Now().Add(-3 * time.Hour)
	l.RecordFact(Fact{Key: "old", Value: "x", Scope: FactScopePersistent, Confidence: 1.0, Timestamp: t0})
	l.RecordFact(Fact{Key: "mid", Value: "y", Scope: FactScopePersistent, Confidence: 1.0, Timestamp: t0.Add(time.Hour)})
	l.RecordFact(Fact{Key: "new", Value: "z", Scope: FactScopePersistent, Confidence: 1.0, Timestamp: t0.Add(2 * time.Hour)})

	got := l.RecentPersistentFacts(2)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Key != "new" || got[1].Key != "mid" {
		t.Errorf("order = %+v, want new, mid", []string{got[0].Key, got[1].Key})
	}
}

func TestFacts_RecentPersistent_FiltersScope(t *testing.T) {
	l := runLibrarian(t, t.TempDir())
	l.RecordFact(Fact{Key: "p", Value: "v", Scope: FactScopePersistent, Confidence: 1.0})
	l.RecordFact(Fact{Key: "s", Value: "v", Scope: FactScopeSession, Confidence: 1.0})
	got := l.RecentPersistentFacts(10)
	if len(got) != 1 || got[0].Key != "p" {
		t.Fatalf("got %+v, want only persistent", got)
	}
}

func TestFacts_RecentPersistent_LimitZeroOrNegativeReturnsNil(t *testing.T) {
	l := runLibrarian(t, t.TempDir())
	l.RecordFact(Fact{Key: "k", Value: "v", Scope: FactScopePersistent, Confidence: 1.0})
	if got := l.RecentPersistentFacts(0); got != nil {
		t.Errorf("limit=0 → %+v, want nil", got)
	}
	if got := l.RecentPersistentFacts(-1); got != nil {
		t.Errorf("limit=-1 → %+v, want nil", got)
	}
}

func TestFacts_RecentPersistent_OnePerKey(t *testing.T) {
	l := runLibrarian(t, t.TempDir())
	t0 := time.Now().Add(-time.Hour)
	l.RecordFact(Fact{Key: "k", Value: "old", Scope: FactScopePersistent, Confidence: 1.0, Timestamp: t0})
	l.RecordFact(Fact{Key: "k", Value: "new", Scope: FactScopePersistent, Confidence: 1.0, Timestamp: time.Now()})
	got := l.RecentPersistentFacts(10)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (one entry per key)", len(got))
	}
	if got[0].Value != "new" {
		t.Errorf("value = %q, want 'new' (most recent wins)", got[0].Value)
	}
}

func TestFacts_ClearFact_TombstonesKey(t *testing.T) {
	l := runLibrarian(t, t.TempDir())
	l.RecordFact(Fact{Key: "k", Value: "v", Scope: FactScopePersistent, Confidence: 1.0})
	if got := l.GetFact("k"); got == nil || got.Value != "v" {
		t.Fatalf("pre-clear get = %+v", got)
	}
	l.ClearFact("k", "cyc-1")
	if got := l.GetFact("k"); got != nil {
		t.Fatalf("post-clear get = %+v, want nil tombstone", got)
	}
}

func TestFacts_ClearFact_RewriteRevivesKey(t *testing.T) {
	// Memory archive is immutable; clear is just a tombstone. A
	// later Write to the same key restores it (most-recent wins).
	// No explicit timestamps — the actor assigns them in FIFO order
	// at processing time, which is the canonical chronology.
	l := runLibrarian(t, t.TempDir())
	l.RecordFact(Fact{Key: "k", Value: "v1", Scope: FactScopePersistent, Confidence: 1.0})
	l.ClearFact("k", "cyc-1")
	l.RecordFact(Fact{Key: "k", Value: "v2", Scope: FactScopePersistent, Confidence: 1.0})
	got := l.GetFact("k")
	if got == nil || got.Value != "v2" {
		t.Fatalf("after re-write get = %+v, want v2", got)
	}
}

func TestFacts_ClearFact_ExcludedFromCount(t *testing.T) {
	l := runLibrarian(t, t.TempDir())
	l.RecordFact(Fact{Key: "a", Value: "1", Scope: FactScopePersistent, Confidence: 1.0})
	l.RecordFact(Fact{Key: "b", Value: "2", Scope: FactScopePersistent, Confidence: 1.0})
	if got := l.PersistentFactCount(); got != 2 {
		t.Fatalf("pre-clear count = %d", got)
	}
	l.ClearFact("a", "cyc-1")
	if got := l.PersistentFactCount(); got != 1 {
		t.Fatalf("post-clear count = %d, want 1", got)
	}
}

func TestFacts_ClearFact_ExcludedFromRecentPersistent(t *testing.T) {
	l := runLibrarian(t, t.TempDir())
	l.RecordFact(Fact{Key: "a", Value: "1", Scope: FactScopePersistent, Confidence: 1.0})
	l.RecordFact(Fact{Key: "b", Value: "2", Scope: FactScopePersistent, Confidence: 1.0})
	l.ClearFact("a", "cyc-1")
	got := l.RecentPersistentFacts(10)
	if len(got) != 1 || got[0].Key != "b" {
		t.Fatalf("got %+v, want only b", got)
	}
}

func TestFacts_Search_MatchesKeyOrValue(t *testing.T) {
	l := runLibrarian(t, t.TempDir())
	l.RecordFact(Fact{Key: "user_name", Value: "Seamus", Scope: FactScopePersistent, Confidence: 1.0})
	l.RecordFact(Fact{Key: "tz", Value: "Europe/Dublin", Confidence: 1.0, Scope: FactScopePersistent})
	l.RecordFact(Fact{Key: "color", Value: "blue", Confidence: 1.0, Scope: FactScopePersistent})

	// Match on value
	got := l.SearchFacts("seamus", 10)
	if len(got) != 1 || got[0].Key != "user_name" {
		t.Fatalf("value match: %+v", got)
	}
	// Match on key
	got = l.SearchFacts("USER", 10)
	if len(got) != 1 || got[0].Key != "user_name" {
		t.Fatalf("key match (case-insensitive): %+v", got)
	}
	// No match
	if got := l.SearchFacts("nope", 10); len(got) != 0 {
		t.Fatalf("no-match: %+v", got)
	}
}

func TestFacts_Search_ExcludesCleared(t *testing.T) {
	l := runLibrarian(t, t.TempDir())
	l.RecordFact(Fact{Key: "user_name", Value: "Seamus", Scope: FactScopePersistent, Confidence: 1.0})
	l.ClearFact("user_name", "cyc-1")
	if got := l.SearchFacts("seamus", 10); len(got) != 0 {
		t.Fatalf("cleared key still surfaces: %+v", got)
	}
}

func TestFacts_Search_LatestPerKey(t *testing.T) {
	l := runLibrarian(t, t.TempDir())
	t0 := time.Now().Add(-time.Hour)
	l.RecordFact(Fact{Key: "k", Value: "old-blue", Scope: FactScopePersistent, Confidence: 1.0, Timestamp: t0})
	l.RecordFact(Fact{Key: "k", Value: "new-red", Scope: FactScopePersistent, Confidence: 1.0, Timestamp: time.Now()})
	got := l.SearchFacts("blue", 10)
	if len(got) != 0 {
		t.Errorf("old value still searchable: %+v", got)
	}
	got = l.SearchFacts("red", 10)
	if len(got) != 1 || got[0].Value != "new-red" {
		t.Errorf("latest value not found: %+v", got)
	}
}

func TestFacts_Search_LimitGuards(t *testing.T) {
	l := runLibrarian(t, t.TempDir())
	l.RecordFact(Fact{Key: "k", Value: "v", Scope: FactScopePersistent, Confidence: 1.0})
	if got := l.SearchFacts("v", 0); got != nil {
		t.Errorf("limit=0 → %+v, want nil", got)
	}
	if got := l.SearchFacts("", 10); got != nil {
		t.Errorf("empty keyword → %+v, want nil", got)
	}
}

func TestFacts_Search_RespectsLimit(t *testing.T) {
	l := runLibrarian(t, t.TempDir())
	for i, key := range []string{"alpha", "alpine", "alpaca"} {
		l.RecordFact(Fact{
			Key:        key,
			Value:      "v",
			Scope:      FactScopePersistent,
			Confidence: 1.0,
			Timestamp:  time.Now().Add(time.Duration(i) * time.Second),
		})
	}
	got := l.SearchFacts("alp", 2)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
}

func TestFact_Operation_DefaultsToWrite(t *testing.T) {
	// Fact records without Operation set must be persisted as Write so
	// existing JSONL files (pre-Operation field) replay sensibly.
	l := runLibrarian(t, t.TempDir())
	l.RecordFact(Fact{Key: "k", Value: "v", Scope: FactScopePersistent, Confidence: 1.0})
	got := l.GetFact("k")
	if got == nil || got.Operation != FactOperationWrite {
		t.Fatalf("operation = %v, want write", got)
	}
}

func TestFacts_RecentPersistent_DecaysConfidence(t *testing.T) {
	l := runLibrarian(t, t.TempDir())
	l.RecordFact(Fact{
		Key: "k", Value: "v", Scope: FactScopePersistent, Confidence: 1.0,
		HalfLifeDays: 1, Timestamp: time.Now().Add(-24 * time.Hour),
	})
	got := l.RecentPersistentFacts(1)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	// One half-life elapsed → confidence halved (~0.5).
	if math.Abs(got[0].Confidence-0.5) > 0.05 {
		t.Errorf("confidence = %f, want ~0.5 after one half-life", got[0].Confidence)
	}
}

func TestFacts_GetMissingReturnsNil(t *testing.T) {
	l := runLibrarian(t, t.TempDir())
	if got := l.GetFact("never_set"); got != nil {
		t.Fatalf("got %+v, want nil", got)
	}
}

func TestFacts_NewerEntryWins(t *testing.T) {
	dir := t.TempDir()
	l := runLibrarian(t, dir)

	l.RecordFact(Fact{
		Key: "mode", Value: "old", Scope: FactScopePersistent, Confidence: 0.5,
		Timestamp: time.Now().Add(-1 * time.Hour),
	})
	l.RecordFact(Fact{
		Key: "mode", Value: "new", Scope: FactScopePersistent, Confidence: 0.9,
		Timestamp: time.Now(),
	})
	time.Sleep(20 * time.Millisecond)

	got := l.GetFact("mode")
	if got == nil {
		t.Fatal("got nil")
	}
	if got.Value != "new" {
		t.Fatalf("value = %q, want new (most recent wins)", got.Value)
	}
}

func TestFact_DecayHalfLife(t *testing.T) {
	now := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	f := Fact{
		Key:           "k",
		Confidence:    0.8,
		Timestamp:     now.AddDate(0, 0, -10),
		HalfLifeDays:  10, // exactly one half-life ago
	}
	got := f.DecayedConfidence(now)
	want := 0.4 // 0.8 * 0.5^(10/10)
	if math.Abs(got-want) > 0.001 {
		t.Errorf("got %f, want %f", got, want)
	}
}

func TestFact_DecayZeroHalfLifeMeansNoDecay(t *testing.T) {
	f := Fact{Confidence: 0.7, Timestamp: time.Now().AddDate(-10, 0, 0), HalfLifeDays: 0}
	if got := f.DecayedConfidence(time.Now()); got != 0.7 {
		t.Errorf("got %f, want 0.7 (no decay)", got)
	}
}

func TestFacts_PersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	l1 := runLibrarian(t, dir)
	l1.RecordFact(Fact{Key: "k", Value: "v", Scope: FactScopePersistent, Confidence: 1.0})
	time.Sleep(20 * time.Millisecond)

	l2 := runLibrarian(t, dir)
	got := l2.GetFact("k")
	if got == nil || got.Value != "v" {
		t.Fatalf("replay failed: %+v", got)
	}
}

// ---- shared ----

func TestNarrative_PruneIndex_RemovesOldEntries(t *testing.T) {
	l := runLibrarian(t, t.TempDir())

	// Three entries, varying ages.
	now := time.Now()
	old := NarrativeEntry{CycleID: "old-cycle", Status: NarrativeStatusComplete, Summary: "old", Timestamp: now.Add(-90 * 24 * time.Hour)}
	mid := NarrativeEntry{CycleID: "mid-cycle", Status: NarrativeStatusComplete, Summary: "mid", Timestamp: now.Add(-30 * 24 * time.Hour)}
	new := NarrativeEntry{CycleID: "new-cycle", Status: NarrativeStatusComplete, Summary: "new", Timestamp: now}
	l.RecordNarrative(old)
	l.RecordNarrative(mid)
	l.RecordNarrative(new)

	// Cutoff at 60 days ago — only "old" should drop.
	cutoff := now.Add(-60 * 24 * time.Hour)
	removed, err := l.PruneNarrativeIndex(cutoff)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}

	got := l.RecentNarrative(10)
	for _, e := range got {
		if e.CycleID == "old-cycle" {
			t.Errorf("old-cycle should have been pruned: %+v", got)
		}
	}
	// Verify mid and new survived.
	foundMid, foundNew := false, false
	for _, e := range got {
		if e.CycleID == "mid-cycle" {
			foundMid = true
		}
		if e.CycleID == "new-cycle" {
			foundNew = true
		}
	}
	if !foundMid {
		t.Error("mid-cycle should still be in the index")
	}
	if !foundNew {
		t.Error("new-cycle should still be in the index")
	}
}

func TestNarrative_PruneIndex_Idempotent(t *testing.T) {
	l := runLibrarian(t, t.TempDir())
	now := time.Now()
	l.RecordNarrative(NarrativeEntry{
		CycleID: "old", Status: NarrativeStatusComplete,
		Timestamp: now.Add(-90 * 24 * time.Hour),
	})

	cutoff := now.Add(-60 * 24 * time.Hour)
	first, _ := l.PruneNarrativeIndex(cutoff)
	if first != 1 {
		t.Errorf("first prune removed = %d, want 1", first)
	}
	second, err := l.PruneNarrativeIndex(cutoff)
	if err != nil {
		t.Fatalf("second prune err: %v", err)
	}
	if second != 0 {
		t.Errorf("second prune removed = %d, want 0 (idempotent)", second)
	}
}

func TestNarrative_PruneIndex_EmptyIndex(t *testing.T) {
	l := runLibrarian(t, t.TempDir())
	removed, err := l.PruneNarrativeIndex(time.Now())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0 on empty index", removed)
	}
}

func TestNarrative_PruneIndex_LeavesJSONLAlone(t *testing.T) {
	dir := t.TempDir()
	l := runLibrarian(t, dir)

	now := time.Now()
	l.RecordNarrative(NarrativeEntry{
		CycleID: "old", Status: NarrativeStatusComplete, Summary: "preserve me",
		Timestamp: now.Add(-90 * 24 * time.Hour),
	})

	// Wait briefly for the actor to flush JSONL (the test helper
	// doesn't expose a flush primitive). Then prune the index.
	time.Sleep(20 * time.Millisecond)

	if _, err := l.PruneNarrativeIndex(now.Add(-1 * time.Hour)); err != nil {
		t.Fatal(err)
	}

	// JSONL on disk should still contain the entry — immutable archive.
	files, err := os.ReadDir(filepath.Join(dir, "narrative"))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".jsonl") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, "narrative", f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "preserve me") {
			found = true
		}
	}
	if !found {
		t.Errorf("JSONL should still contain pruned entry — archive immutable")
	}
}

func TestNarrative_ReplaySkipsFilesOlderThanWindow(t *testing.T) {
	dir := t.TempDir()
	narrDir := filepath.Join(dir, NarrativeSubdir)
	if err := os.MkdirAll(narrDir, 0o755); err != nil {
		t.Fatal(err)
	}

	old := time.Now().AddDate(0, 0, -100).Format("2006-01-02")
	current := time.Now().Format("2006-01-02")

	writeNarr := func(filename, cycleID string) {
		entry := NarrativeEntry{CycleID: cycleID, Status: NarrativeStatusComplete, Summary: cycleID, Timestamp: time.Now()}
		body, _ := json.Marshal(entry)
		if err := os.WriteFile(filepath.Join(narrDir, filename), append(body, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeNarr(old+".jsonl", "ancient")
	writeNarr(current+".jsonl", "today")

	l, err := New(Options{DataDir: dir, Logger: discardLogger(), NarrativeWindowDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go l.Run(ctx)

	got := l.RecentNarrative(10)
	for _, e := range got {
		if e.CycleID == "ancient" {
			t.Fatalf("entry from before window cutoff was replayed: %+v", e)
		}
	}
	found := false
	for _, e := range got {
		if e.CycleID == "today" {
			found = true
		}
	}
	if !found {
		t.Fatal("today's entry not replayed")
	}
}

func TestFacts_ReplayLoadsEverythingRegardlessOfAge(t *testing.T) {
	// Facts are current state — no window. A fact set 100 days ago and
	// never updated must still be in the index.
	dir := t.TempDir()
	factsDir := filepath.Join(dir, FactsSubdir)
	if err := os.MkdirAll(factsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	old := time.Now().AddDate(0, 0, -100)
	oldFile := old.Format("2006-01-02") + "-facts.jsonl"
	entry := Fact{
		Key:        "user_name",
		Value:      "Seamus",
		Scope:      FactScopePersistent,
		Confidence: 1.0,
		Timestamp:  old,
	}
	body, _ := json.Marshal(entry)
	if err := os.WriteFile(filepath.Join(factsDir, oldFile), append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	l, err := New(Options{DataDir: dir, Logger: discardLogger(), NarrativeWindowDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go l.Run(ctx)

	got := l.GetFact("user_name")
	if got == nil {
		t.Fatal("facts replay dropped a 100-day-old fact; should have loaded it")
	}
	if got.Value != "Seamus" {
		t.Fatalf("value = %q", got.Value)
	}
}

// helper

func cycleIDFromInt(i int) string {
	return "c-" + string(rune('0'+i))
}
