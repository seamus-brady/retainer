package observer

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/seamus-brady/retainer/internal/dag"
	"github.com/seamus-brady/retainer/internal/librarian"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newLibrarian(t *testing.T) *librarian.Librarian {
	t.Helper()
	l, err := librarian.New(librarian.Options{
		DataDir: t.TempDir(),
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatalf("librarian: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go l.Run(ctx)
	return l
}

func newDAG(t *testing.T) *dag.DAG {
	t.Helper()
	d := dag.New()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go d.Run(ctx)
	return d
}

func TestRecentCycles_ReturnsFromLibrarian(t *testing.T) {
	lib := newLibrarian(t)
	o := New(lib, nil)

	for i := 0; i < 5; i++ {
		lib.RecordNarrative(librarian.NarrativeEntry{
			CycleID: "c-" + string(rune('a'+i)),
			Status:  librarian.NarrativeStatusComplete,
			Summary: "cycle " + string(rune('a'+i)),
		})
	}
	time.Sleep(20 * time.Millisecond)

	got := o.RecentCycles(3)
	if len(got) != 3 {
		t.Fatalf("got %d, want 3", len(got))
	}
}

func TestRecentCycles_NilLibrarian(t *testing.T) {
	o := New(nil, nil)
	if got := o.RecentCycles(5); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestInspectCycle_MergesDAGAndNarrative(t *testing.T) {
	lib := newLibrarian(t)
	d := newDAG(t)
	o := New(lib, d)

	d.StartCycle("c1", "", dag.NodeCognitive)
	d.CompleteCycle("c1", dag.StatusComplete, "")
	lib.RecordNarrative(librarian.NarrativeEntry{
		CycleID:  "c1",
		Status:   librarian.NarrativeStatusComplete,
		Summary:  "User: hello\nReply: hi",
		Keywords: []string{"greeting"},
	})
	time.Sleep(30 * time.Millisecond)

	insp := o.InspectCycle("c1")
	if !insp.Found {
		t.Fatal("Found should be true")
	}
	if !insp.NarrativeFound {
		t.Error("NarrativeFound should be true")
	}
	if insp.Type != dag.NodeCognitive {
		t.Errorf("Type = %q", insp.Type)
	}
	if insp.Status != dag.StatusComplete {
		t.Errorf("Status = %q", insp.Status)
	}
	if insp.Summary == "" {
		t.Error("Summary should be populated from narrative")
	}
	if len(insp.Keywords) != 1 || insp.Keywords[0] != "greeting" {
		t.Errorf("Keywords = %v", insp.Keywords)
	}
	if insp.Duration < 0 {
		t.Error("Duration should be non-negative")
	}
}

func TestInspectCycle_DAGOnly(t *testing.T) {
	d := newDAG(t)
	o := New(nil, d)

	d.StartCycle("dag-only", "", dag.NodeCognitive)
	insp := o.InspectCycle("dag-only")
	if !insp.Found {
		t.Fatal("Found should be true (DAG-only)")
	}
	if insp.NarrativeFound {
		t.Error("NarrativeFound should be false")
	}
	if insp.Status != dag.StatusInProgress {
		t.Errorf("Status = %q", insp.Status)
	}
}

func TestInspectCycle_NarrativeOnly(t *testing.T) {
	lib := newLibrarian(t)
	o := New(lib, nil)

	lib.RecordNarrative(librarian.NarrativeEntry{
		CycleID: "narr-only",
		Status:  librarian.NarrativeStatusComplete,
		Summary: "loaded from JSONL",
	})
	time.Sleep(20 * time.Millisecond)

	insp := o.InspectCycle("narr-only")
	if !insp.Found {
		t.Fatal("Found should be true (narrative-only)")
	}
	if !insp.NarrativeFound {
		t.Error("NarrativeFound should be true")
	}
	if insp.Summary != "loaded from JSONL" {
		t.Errorf("Summary = %q", insp.Summary)
	}
}

func TestInspectCycle_PopulatesRichNarrativeFields(t *testing.T) {
	lib := newLibrarian(t)
	d := newDAG(t)
	o := New(lib, d)

	d.StartCycle("rich-1", "", dag.NodeCognitive)
	d.CompleteCycle("rich-1", dag.StatusComplete, "")
	lib.RecordNarrative(librarian.NarrativeEntry{
		CycleID:   "rich-1",
		Timestamp: time.Now(),
		Summary:   "ran a comparison",
		Intent: librarian.Intent{
			Classification: librarian.IntentComparison,
			Description:    "compare two metrics",
			Domain:         "weather",
		},
		Outcome: librarian.Outcome{
			Status:     librarian.OutcomeSuccess,
			Confidence: 0.9,
			Assessment: "looks right",
		},
		DelegationChain: []librarian.DelegationStep{
			{Agent: "researcher", Instruction: "look it up", OutcomeText: "ok"},
		},
		Topics:   []string{"weather", "comparison"},
		Entities: librarian.Entities{Locations: []string{"Dublin"}},
		Metrics: librarian.Metrics{
			TotalDurationMs: 1234,
			InputTokens:     100,
			ToolCalls:       2,
		},
	})
	time.Sleep(30 * time.Millisecond)

	insp := o.InspectCycle("rich-1")
	if !insp.NarrativeFound {
		t.Fatal("NarrativeFound should be true")
	}
	if insp.Intent.Classification != librarian.IntentComparison {
		t.Errorf("Intent.Classification = %q", insp.Intent.Classification)
	}
	if insp.Intent.Domain != "weather" {
		t.Errorf("Intent.Domain = %q", insp.Intent.Domain)
	}
	if insp.Outcome.Status != librarian.OutcomeSuccess {
		t.Errorf("Outcome.Status = %q", insp.Outcome.Status)
	}
	if insp.Outcome.Confidence != 0.9 {
		t.Errorf("Outcome.Confidence = %v", insp.Outcome.Confidence)
	}
	if len(insp.DelegationChain) != 1 || insp.DelegationChain[0].Agent != "researcher" {
		t.Errorf("DelegationChain = %+v", insp.DelegationChain)
	}
	if len(insp.Topics) != 2 {
		t.Errorf("Topics = %v", insp.Topics)
	}
	if len(insp.Entities.Locations) != 1 || insp.Entities.Locations[0] != "Dublin" {
		t.Errorf("Entities.Locations = %v", insp.Entities.Locations)
	}
	if insp.Metrics.TotalDurationMs != 1234 || insp.Metrics.InputTokens != 100 || insp.Metrics.ToolCalls != 2 {
		t.Errorf("Metrics = %+v", insp.Metrics)
	}
}

func TestInspectCycle_LegacyEntryHasZeroRichFields(t *testing.T) {
	lib := newLibrarian(t)
	o := New(lib, nil)

	// Mimic a pre-rich-shape narrative entry: only the legacy
	// top-level fields populated.
	lib.RecordNarrative(librarian.NarrativeEntry{
		CycleID: "legacy-1",
		Status:  librarian.NarrativeStatusComplete,
		Summary: "old shape",
	})
	time.Sleep(20 * time.Millisecond)

	insp := o.InspectCycle("legacy-1")
	if !insp.NarrativeFound {
		t.Fatal("NarrativeFound should be true")
	}
	if !insp.Intent.IsZero() {
		t.Errorf("Intent should be zero, got %+v", insp.Intent)
	}
	if !insp.Outcome.IsZero() {
		t.Errorf("Outcome should be zero, got %+v", insp.Outcome)
	}
	if len(insp.DelegationChain) != 0 {
		t.Errorf("DelegationChain should be empty, got %+v", insp.DelegationChain)
	}
	if !insp.Metrics.IsZero() {
		t.Errorf("Metrics should be zero, got %+v", insp.Metrics)
	}
}

func TestInspectCycle_NotFound(t *testing.T) {
	lib := newLibrarian(t)
	d := newDAG(t)
	o := New(lib, d)
	insp := o.InspectCycle("nonexistent")
	if insp.Found {
		t.Error("Found should be false for unknown cycle")
	}
}

func TestGetFact_Roundtrip(t *testing.T) {
	lib := newLibrarian(t)
	o := New(lib, nil)
	lib.RecordFact(librarian.Fact{
		Key:        "user_name",
		Value:      "Seamus",
		Scope:      librarian.FactScopePersistent,
		Confidence: 1.0,
	})
	time.Sleep(20 * time.Millisecond)

	got := o.GetFact("user_name")
	if got == nil {
		t.Fatal("got nil")
	}
	if got.Value != "Seamus" {
		t.Errorf("value = %q", got.Value)
	}
}
