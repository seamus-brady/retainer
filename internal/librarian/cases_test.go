package librarian

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/seamus-brady/retainer/internal/cbr"
	"github.com/seamus-brady/retainer/internal/cyclelog"
	"github.com/seamus-brady/retainer/internal/embed"
)

// runLibrarianWithOptions boots a Librarian with arbitrary Options
// under the actor loop and returns it. Cancellation is registered
// via t.Cleanup so tests don't need to track the context manually.
//
// Distinct from the package's existing runLibrarian helper which only
// accepts a dir path. This one carries Options for the cases-store
// embedder.
func runLibrarianWithOptions(t *testing.T, opts Options) *Librarian {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.DataDir == "" {
		opts.DataDir = t.TempDir()
	}
	l, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go l.Run(ctx)
	return l
}

func makeCase(id, intent, domain string) cbr.Case {
	return cbr.Case{
		ID:                id,
		Timestamp:         time.Now(),
		SchemaVersion:     cbr.SchemaVersion,
		SourceNarrativeID: "cyc-" + id,
		Problem: cbr.Problem{
			Intent:   intent,
			Domain:   domain,
			Keywords: []string{intent, domain},
		},
		Solution: cbr.Solution{
			Approach: "approach for " + intent,
		},
		Outcome: cbr.Outcome{
			Status:     cbr.StatusSuccess,
			Confidence: 0.9,
		},
	}
}

func TestLibrarian_RecordAndCount(t *testing.T) {
	l := runLibrarianWithOptions(t, Options{})

	if got := l.CaseCount(); got != 0 {
		t.Errorf("empty CaseCount = %d, want 0", got)
	}
	l.RecordCase(makeCase("c1", "debug", "auth"))
	l.RecordCase(makeCase("c2", "research", "papers"))

	// RecordCase is fire-and-forget; let the inbox drain.
	syncBarrier(l)

	if got := l.CaseCount(); got != 2 {
		t.Errorf("CaseCount = %d, want 2", got)
	}
}

func TestLibrarian_RetrieveDomainBoost(t *testing.T) {
	l := runLibrarianWithOptions(t, Options{})

	l.RecordCase(makeCase("c1", "debug login", "auth"))
	l.RecordCase(makeCase("c2", "research paper", "papers"))
	syncBarrier(l)

	got := l.RetrieveCases(context.Background(), cbr.Query{
		Intent: "debug login",
		Domain: "auth",
	})
	if len(got) == 0 {
		t.Fatal("expected results")
	}
	if got[0].Case.ID != "c1" {
		t.Errorf("auth-domain match should rank first; got %q", got[0].Case.ID)
	}
}

func TestLibrarian_RetrieveWithMockEmbedder(t *testing.T) {
	l := runLibrarianWithOptions(t, Options{
		Embedder: embed.NewMock(8),
	})

	c := makeCase("c1", "debug", "auth")
	c.Embedding = []float32{1, 0, 0, 0, 0, 0, 0, 0}
	c.EmbedderID = "mock/seed-fnv/v1"
	l.RecordCase(c)
	syncBarrier(l)

	got := l.RetrieveCases(context.Background(), cbr.Query{
		Intent: "debug",
		Domain: "auth",
	})
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	if got[0].Score == 0 {
		t.Error("score zero — embedder not contributing?")
	}
}

func TestLibrarian_GetCaseFound(t *testing.T) {
	l := runLibrarianWithOptions(t, Options{})

	c := makeCase("c1", "debug", "auth")
	l.RecordCase(c)
	syncBarrier(l)

	got, ok := l.GetCase("c1")
	if !ok {
		t.Fatal("c1 not found after Record")
	}
	if got.Problem.Intent != "debug" {
		t.Errorf("intent = %q, want debug", got.Problem.Intent)
	}
}

func TestLibrarian_GetCaseMissing(t *testing.T) {
	l := runLibrarianWithOptions(t, Options{})

	if _, ok := l.GetCase("nope"); ok {
		t.Error("missing case shouldn't return ok=true")
	}
}

func TestLibrarian_SuppressCaseExcludesFromRetrieval(t *testing.T) {
	l := runLibrarianWithOptions(t, Options{})

	l.RecordCase(makeCase("c1", "debug", "auth"))
	l.RecordCase(makeCase("c2", "debug", "auth"))
	syncBarrier(l)

	if _, err := l.SuppressCase("c1"); err != nil {
		t.Fatalf("Suppress: %v", err)
	}

	got := l.RetrieveCases(context.Background(), cbr.Query{Domain: "auth"})
	for _, s := range got {
		if s.Case.ID == "c1" {
			t.Errorf("suppressed case c1 still in retrieval: %+v", got)
		}
	}
	// c1 still readable via GetCase (curation tools see it).
	if _, ok := l.GetCase("c1"); !ok {
		t.Error("suppressed case should still be Get-able")
	}
}

func TestLibrarian_UnsuppressRestoresRetrieval(t *testing.T) {
	l := runLibrarianWithOptions(t, Options{})
	l.RecordCase(makeCase("c1", "debug", "auth"))
	syncBarrier(l)

	_, err := l.SuppressCase("c1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = l.UnsuppressCase("c1")
	if err != nil {
		t.Fatal(err)
	}
	got := l.RetrieveCases(context.Background(), cbr.Query{Domain: "auth"})
	if len(got) != 1 {
		t.Errorf("after Unsuppress, got %d results", len(got))
	}
}

func TestLibrarian_BoostCaseClampsToUnitRange(t *testing.T) {
	l := runLibrarianWithOptions(t, Options{})
	c := makeCase("c1", "debug", "auth")
	c.Outcome.Confidence = 0.9
	l.RecordCase(c)
	syncBarrier(l)

	updated, err := l.BoostCase("c1", 0.5) // 0.9 + 0.5 = 1.4 → clamp to 1.0
	if err != nil {
		t.Fatalf("Boost: %v", err)
	}
	if updated.Outcome.Confidence != 1.0 {
		t.Errorf("boost clamp; got %g, want 1.0", updated.Outcome.Confidence)
	}

	// Negative delta clamps to 0.
	updated, err = l.BoostCase("c1", -2.0)
	if err != nil {
		t.Fatalf("Boost negative: %v", err)
	}
	if updated.Outcome.Confidence != 0 {
		t.Errorf("negative boost clamp; got %g, want 0", updated.Outcome.Confidence)
	}
}

func TestLibrarian_AnnotateCaseAppendsPitfall(t *testing.T) {
	l := runLibrarianWithOptions(t, Options{})

	l.RecordCase(makeCase("c1", "debug", "auth"))
	syncBarrier(l)

	updated, err := l.AnnotateCase("c1", "watch out for stale tokens")
	if err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	if len(updated.Outcome.Pitfalls) != 1 {
		t.Errorf("pitfalls = %v", updated.Outcome.Pitfalls)
	}
}

func TestLibrarian_AnnotateRejectsEmpty(t *testing.T) {
	l := runLibrarianWithOptions(t, Options{})
	l.RecordCase(makeCase("c1", "debug", "auth"))
	syncBarrier(l)

	if _, err := l.AnnotateCase("c1", ""); err == nil {
		t.Error("empty pitfall should error")
	}
}

func TestLibrarian_CorrectCaseUpdatesContent(t *testing.T) {
	l := runLibrarianWithOptions(t, Options{})
	l.RecordCase(makeCase("c1", "debug", "auth"))
	syncBarrier(l)

	updated, err := l.CorrectCase("c1", cbr.Case{
		Problem:  cbr.Problem{Intent: "diagnose", Domain: "auth-system"},
		Solution: cbr.Solution{Approach: "trace request"},
		Outcome:  cbr.Outcome{Status: cbr.StatusSuccess, Confidence: 0.7},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Problem.Intent != "diagnose" {
		t.Errorf("intent didn't update: %q", updated.Problem.Intent)
	}
	if updated.ID != "c1" {
		t.Errorf("ID should not change on correct: %q", updated.ID)
	}
}

func TestLibrarian_RecordCaseRetrievalUpdatesUsageStats(t *testing.T) {
	l := runLibrarianWithOptions(t, Options{})
	l.RecordCase(makeCase("c1", "debug", "auth"))
	l.RecordCase(makeCase("c2", "debug", "auth"))
	syncBarrier(l)

	// Cycle that retrieved c1 + c2 succeeded.
	l.RecordCaseRetrieval([]string{"c1", "c2"}, true)

	got1, _ := l.GetCase("c1")
	got2, _ := l.GetCase("c2")
	if got1.UsageStats == nil || got1.UsageStats.RetrievalCount != 1 || got1.UsageStats.RetrievalSuccessCount != 1 {
		t.Errorf("c1 stats wrong: %+v", got1.UsageStats)
	}
	if got2.UsageStats == nil || got2.UsageStats.RetrievalCount != 1 || got2.UsageStats.RetrievalSuccessCount != 1 {
		t.Errorf("c2 stats wrong: %+v", got2.UsageStats)
	}

	// Another retrieval, this time the cycle failed.
	l.RecordCaseRetrieval([]string{"c1"}, false)
	got1, _ = l.GetCase("c1")
	if got1.UsageStats.RetrievalCount != 2 || got1.UsageStats.RetrievalSuccessCount != 1 {
		t.Errorf("c1 after failed retrieval: %+v", got1.UsageStats)
	}
}

func TestLibrarian_ReplayLatestRecordWins(t *testing.T) {
	dataDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// First Librarian: records, suppresses, then we cancel it so a
	// fresh instance can replay the same JSONL.
	l1, err := New(Options{DataDir: dataDir, Logger: logger})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	go l1.Run(ctx1)
	l1.RecordCase(makeCase("c1", "debug", "auth"))
	syncBarrier(l1)
	if _, err := l1.SuppressCase("c1"); err != nil {
		t.Fatal(err)
	}
	cancel1()

	// New Librarian on the same dir: replay should land c1 in the
	// suppressed state (last record wins).
	l2 := runLibrarianWithOptions(t, Options{DataDir: dataDir})

	got, ok := l2.GetCase("c1")
	if !ok {
		t.Fatal("c1 missing after replay")
	}
	if !got.Redacted {
		t.Error("replay didn't preserve suppressed state")
	}
	got2 := l2.RetrieveCases(context.Background(), cbr.Query{Domain: "auth"})
	for _, s := range got2 {
		if s.Case.ID == "c1" {
			t.Errorf("replayed suppressed case in retrieval: %+v", got2)
		}
	}
}

// syncBarrier issues a synchronous query so the test thread waits
// until the librarian has drained its inbox up to that point.
// RecordCase is fire-and-forget; without this, subsequent assertions
// race against the goroutine.
func syncBarrier(l *Librarian) {
	_ = l.CaseCount()
}

// ---- Per-cycle retrieval registry ----

func TestLibrarian_RetrieveRegistersAgainstCycleID(t *testing.T) {
	l := runLibrarianWithOptions(t, Options{})
	l.RecordCase(makeCase("c1", "debug", "auth"))
	l.RecordCase(makeCase("c2", "research", "papers"))
	syncBarrier(l)

	ctx := cyclelog.WithCycleID(context.Background(), "cycle-X")
	scored := l.RetrieveCases(ctx, cbr.Query{Domain: "auth"})
	if len(scored) == 0 {
		t.Fatal("expected at least one result")
	}

	got := l.DrainRetrievedCaseIDs("cycle-X")
	if len(got) == 0 {
		t.Fatalf("DrainRetrievedCaseIDs returned nothing; got %v", got)
	}
	if got[0] != "c1" {
		t.Errorf("expected first ID c1; got %v", got)
	}
}

func TestLibrarian_RetrieveWithoutCycleIDDoesNotRegister(t *testing.T) {
	l := runLibrarianWithOptions(t, Options{})
	l.RecordCase(makeCase("c1", "debug", "auth"))
	syncBarrier(l)

	// No cycleID in context — registration should skip.
	_ = l.RetrieveCases(context.Background(), cbr.Query{Domain: "auth"})
	if got := l.DrainRetrievedCaseIDs(""); len(got) != 0 {
		t.Errorf("empty cycleID drain should be empty; got %v", got)
	}
}

func TestLibrarian_DrainRetrievedClearsEntry(t *testing.T) {
	l := runLibrarianWithOptions(t, Options{})
	l.RecordCase(makeCase("c1", "debug", "auth"))
	syncBarrier(l)

	ctx := cyclelog.WithCycleID(context.Background(), "cycle-X")
	_ = l.RetrieveCases(ctx, cbr.Query{Domain: "auth"})

	first := l.DrainRetrievedCaseIDs("cycle-X")
	second := l.DrainRetrievedCaseIDs("cycle-X")
	if len(first) == 0 {
		t.Fatal("first drain empty")
	}
	if len(second) != 0 {
		t.Errorf("second drain should be empty (entry cleared); got %v", second)
	}
}

func TestLibrarian_DrainDedupesAcrossMultipleRetrievals(t *testing.T) {
	l := runLibrarianWithOptions(t, Options{})
	l.RecordCase(makeCase("c1", "debug", "auth"))
	syncBarrier(l)

	ctx := cyclelog.WithCycleID(context.Background(), "cycle-X")
	for i := 0; i < 3; i++ {
		_ = l.RetrieveCases(ctx, cbr.Query{Domain: "auth"})
	}
	got := l.DrainRetrievedCaseIDs("cycle-X")
	if len(got) != 1 {
		t.Errorf("repeated retrievals of same case should dedupe; got %v", got)
	}
}

func TestLibrarian_DrainSegmentsByCycleID(t *testing.T) {
	l := runLibrarianWithOptions(t, Options{})
	l.RecordCase(makeCase("c1", "debug", "auth"))
	l.RecordCase(makeCase("c2", "debug", "auth"))
	syncBarrier(l)

	ctxA := cyclelog.WithCycleID(context.Background(), "cycle-A")
	ctxB := cyclelog.WithCycleID(context.Background(), "cycle-B")
	_ = l.RetrieveCases(ctxA, cbr.Query{Domain: "auth", MaxResults: 1})
	_ = l.RetrieveCases(ctxB, cbr.Query{Domain: "auth", MaxResults: 1})

	gotA := l.DrainRetrievedCaseIDs("cycle-A")
	gotB := l.DrainRetrievedCaseIDs("cycle-B")
	if len(gotA) == 0 || len(gotB) == 0 {
		t.Fatalf("each cycle should have one entry; A=%v B=%v", gotA, gotB)
	}
	// Drain on A shouldn't have touched B.
	if l.DrainRetrievedCaseIDs("cycle-B") != nil {
		t.Error("cycle-B should already be drained")
	}
}
