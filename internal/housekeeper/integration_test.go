package housekeeper

import (
	"context"
	"testing"
	"time"

	"github.com/seamus-brady/retainer/internal/cbr"
	"github.com/seamus-brady/retainer/internal/librarian"
)

// TestSweep_RealLibrarian_DedupAndRetrieve drives the housekeeper
// against a real librarian (SQLite + JSONL) to verify the cross-
// package wiring. Asserts:
//
//  1. Two near-duplicate cases get one marked SupersededBy after a
//     CBR-enabled sweep.
//  2. Retrieval after the sweep skips the superseded case.
//  3. The JSONL on disk preserves the superseded record (immutable
//     archive — supersede appends a new line, never deletes).
//
// This is the end-to-end shape that Go unit tests with fakePruner
// can't cover: librarian → casebase → JSONL store → housekeeper
// supersede call → librarian mutate → JSONL append → casebase
// re-retain → retrieve filter.
func TestSweep_RealLibrarian_DedupAndRetrieve(t *testing.T) {
	dir := t.TempDir()
	lib, err := librarian.New(librarian.Options{
		DataDir: dir,
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatalf("librarian: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go lib.Run(ctx)

	// Two near-duplicate cases in the same Strategy category. Higher
	// confidence on case A so dedup picks B as the loser.
	caseA := buildCase("case-a-aaaa", 0.9)
	caseB := buildCase("case-b-bbbb", 0.5)
	lib.RecordCase(caseA)
	lib.RecordCase(caseB)
	// Wait for the librarian inbox to drain so both records are
	// indexed before the sweep reads them.
	for i := 0; i < 50; i++ {
		if lib.CaseCount() == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if lib.CaseCount() != 2 {
		t.Fatalf("setup: expected 2 cases, got %d", lib.CaseCount())
	}

	hk, err := New(Config{
		Librarian:       lib,
		Logger:          discardLogger(),
		NowFn:           func() time.Time { return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) },
		CBRSweepEnabled: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	hk.sweep()

	// State counter: exactly one supersede.
	if hk.state.LastCBRSupersededCount != 1 {
		t.Errorf("LastCBRSupersededCount = %d, want 1", hk.state.LastCBRSupersededCount)
	}

	// Retrieve should now skip case B (superseded). Pass an Intent
	// that matches both — without the skip, both would surface; with
	// the skip, only case A remains.
	results := lib.RetrieveCases(ctx, cbr.Query{
		Intent:     "look up the weather forecast",
		MaxResults: 5,
	})
	for _, s := range results {
		if s.Case.ID == caseB.ID {
			t.Errorf("superseded case B leaked into retrieval: %+v", s.Case)
		}
	}
	if len(results) == 0 {
		t.Errorf("expected case A in retrieval; got nothing")
	}

	// Active count should reflect the supersede.
	if got := lib.CaseCount(); got != 1 {
		t.Errorf("CaseCount after dedup = %d, want 1", got)
	}

	// Superseded record on disk: the JSONL trail must include the
	// supersede mark, not just have the original record. Read it back
	// via the librarian's GetCase (which returns redacted/superseded
	// records too).
	got, ok := lib.GetCase(caseB.ID)
	if !ok {
		t.Fatalf("GetCase(%q) returned not-found", caseB.ID)
	}
	if got.SupersededBy != caseA.ID {
		t.Errorf("SupersededBy = %q, want %q", got.SupersededBy, caseA.ID)
	}
}

// TestSweep_RealLibrarian_PruneOldFailureSuppressesIt asserts the
// prune pass marks an old-failure-no-pitfall case as redacted, and
// retrieval respects the redaction.
func TestSweep_RealLibrarian_PruneOldFailureSuppressesIt(t *testing.T) {
	dir := t.TempDir()
	lib, err := librarian.New(librarian.Options{
		DataDir: dir,
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatalf("librarian: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go lib.Run(ctx)

	old := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC) // 75 days before the now
	failure := cbr.Case{
		ID:        "old-failure-aaaa",
		Timestamp: old,
		Category:  cbr.CategoryPitfall,
		Problem: cbr.Problem{
			IntentClass: cbr.IntentExploration,
			Intent:      "old failed exploration",
			Domain:      "auth",
		},
		Solution: cbr.Solution{Approach: "tried something that didn't work"},
		Outcome:  cbr.Outcome{Status: cbr.StatusFailure, Confidence: 0.3},
	}
	lib.RecordCase(failure)
	for i := 0; i < 50; i++ {
		if lib.CaseCount() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	hk, _ := New(Config{
		Librarian:       lib,
		Logger:          discardLogger(),
		NowFn:           func() time.Time { return time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) },
		CBRSweepEnabled: true,
	})
	hk.sweep()

	if hk.state.LastCBRSuppressedCount != 1 {
		t.Errorf("LastCBRSuppressedCount = %d, want 1", hk.state.LastCBRSuppressedCount)
	}

	got, ok := lib.GetCase(failure.ID)
	if !ok {
		t.Fatalf("GetCase missing")
	}
	if !got.Redacted {
		t.Errorf("expected case to be redacted after prune sweep")
	}
}

// buildCase builds a fully-populated near-duplicate case at the given
// confidence. The Problem fields match across calls (only ID +
// Confidence change) so any two of these score above the dedup
// threshold.
func buildCase(id string, confidence float64) cbr.Case {
	return cbr.Case{
		ID:        id,
		Timestamp: time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC),
		Category:  cbr.CategoryStrategy,
		Problem: cbr.Problem{
			IntentClass: cbr.IntentExploration,
			Intent:      "look up the weather forecast",
			Domain:      "weather",
			Keywords:    []string{"weather", "forecast"},
			Entities:    []string{"Dublin"},
		},
		Solution: cbr.Solution{Approach: "delegate to the researcher with brave_search"},
		Outcome:  cbr.Outcome{Status: cbr.StatusSuccess, Confidence: confidence},
	}
}
