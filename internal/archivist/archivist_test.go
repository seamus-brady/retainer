package archivist

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/seamus-brady/retainer/internal/agent"
	"github.com/seamus-brady/retainer/internal/cbr"
	"github.com/seamus-brady/retainer/internal/librarian"
)

// fakeCurator returns a canned CurationResult or an error. Lets
// tests pin specific curation outcomes without spinning up an
// LLM.
type fakeCurator struct {
	result CurationResult
	err    error
}

func (f *fakeCurator) Curate(_ context.Context, _ CurationInput) (CurationResult, error) {
	if f.err != nil {
		return CurationResult{}, f.err
	}
	return f.result, nil
}

// fakeLib captures whatever the archivist sends to the librarian so
// tests can assert on the resulting writes — narrative entries,
// cases, and usage-stat updates.
type fakeLib struct {
	mu             sync.Mutex
	entries        []librarian.NarrativeEntry
	cases          []cbr.Case
	retrievalCalls []retrievalCall
}

type retrievalCall struct {
	IDs       []string
	Succeeded bool
}

func (f *fakeLib) RecordNarrative(e librarian.NarrativeEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, e)
}

func (f *fakeLib) RecordCase(c cbr.Case) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cases = append(f.cases, c)
}

func (f *fakeLib) RecordCaseRetrieval(ids []string, succeeded bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retrievalCalls = append(f.retrievalCalls, retrievalCall{IDs: append([]string(nil), ids...), Succeeded: succeeded})
}

func (f *fakeLib) snapshot() []librarian.NarrativeEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]librarian.NarrativeEntry, len(f.entries))
	copy(out, f.entries)
	return out
}

func (f *fakeLib) caseSnapshot() []cbr.Case {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]cbr.Case, len(f.cases))
	copy(out, f.cases)
	return out
}

func (f *fakeLib) retrievalSnapshot() []retrievalCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]retrievalCall, len(f.retrievalCalls))
	copy(out, f.retrievalCalls)
	return out
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNew_RequiresLibrarian(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Errorf("expected error when Librarian is nil")
	}
}

func TestRun_RecordsCycleComplete(t *testing.T) {
	lib := &fakeLib{}
	a, err := New(Config{Librarian: lib, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	want := CycleComplete{
		CycleID:   "cyc-1",
		Status:    librarian.NarrativeStatusComplete,
		Summary:   "User: hi\nReply: hello",
		Timestamp: time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC),
	}
	a.Record(want)

	// Allow the archivist to drain the inbox.
	deadline := time.Now().Add(time.Second)
	for {
		if got := lib.snapshot(); len(got) == 1 {
			if got[0].CycleID != want.CycleID {
				t.Errorf("CycleID = %q, want %q", got[0].CycleID, want.CycleID)
			}
			if got[0].Status != want.Status {
				t.Errorf("Status = %v, want %v", got[0].Status, want.Status)
			}
			if got[0].Summary != want.Summary {
				t.Errorf("Summary = %q, want %q", got[0].Summary, want.Summary)
			}
			if !got[0].Timestamp.Equal(want.Timestamp) {
				t.Errorf("Timestamp = %v, want %v", got[0].Timestamp, want.Timestamp)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("archivist didn't record within 1s; got %d entries", len(lib.snapshot()))
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRecord_FillsZeroTimestamp(t *testing.T) {
	lib := &fakeLib{}
	a, err := New(Config{Librarian: lib, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	a.Record(CycleComplete{
		CycleID: "cyc-1",
		Status:  librarian.NarrativeStatusComplete,
		Summary: "no timestamp set",
		// Timestamp left as zero
	})

	deadline := time.Now().Add(time.Second)
	for {
		if got := lib.snapshot(); len(got) == 1 {
			if got[0].Timestamp.IsZero() {
				t.Errorf("zero timestamp should be filled in by archivist")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no entries written within 1s")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRecord_EmptyCycleIDIsDropped(t *testing.T) {
	lib := &fakeLib{}
	a, err := New(Config{Librarian: lib, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	a.Record(CycleComplete{
		Status:  librarian.NarrativeStatusComplete,
		Summary: "no cycle id",
	})

	// Wait briefly to confirm nothing was written.
	time.Sleep(50 * time.Millisecond)
	if got := lib.snapshot(); len(got) != 0 {
		t.Errorf("empty CycleID should be dropped; got %d entries", len(got))
	}
}

func TestRun_StopsOnContextCancel(t *testing.T) {
	lib := &fakeLib{}
	a, err := New(Config{Librarian: lib, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("Run should return ctx.Err on cancel, got %v", err)
		}
	case <-time.After(time.Second):
		t.Errorf("Run didn't return after context cancel")
	}
}

func TestRecord_InboxFull_DropsWithoutBlocking(t *testing.T) {
	// Don't start the actor — inbox fills, Record must not block.
	lib := &fakeLib{}
	a, err := New(Config{Librarian: lib, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	// Pump enough messages to fill the inbox, then more. None of these
	// calls should block (cog stays unblocked even when archivist is
	// stalled).
	deadline := time.Now().Add(time.Second)
	for i := 0; i < inboxBufferSize*2; i++ {
		if time.Now().After(deadline) {
			t.Fatalf("Record blocked at iteration %d", i)
		}
		a.Record(CycleComplete{
			CycleID: "cyc-x",
			Status:  librarian.NarrativeStatusComplete,
			Summary: "burst",
		})
	}
}

// ---- Case derivation ----

func TestRun_DerivesCaseOnCompletedCycle(t *testing.T) {
	lib := &fakeLib{}
	a, err := New(Config{Librarian: lib, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	a.Record(CycleComplete{
		CycleID:    "cyc-success",
		Status:     librarian.NarrativeStatusComplete,
		Summary:    "delivered cleanly",
		UserInput:  "Investigate auth timeout in the production service",
		ReplyText:  "Traced the request through the OAuth flow.",
		AgentsUsed: []string{"researcher"},
		ToolsUsed:  []string{"grep"},
	})

	cases := waitForCases(t, lib, 1)
	if len(cases) != 1 {
		t.Fatalf("got %d cases, want 1", len(cases))
	}
	c := cases[0]
	if c.SourceNarrativeID != "cyc-success" {
		t.Errorf("source narrative ID not stamped: %q", c.SourceNarrativeID)
	}
	if c.Outcome.Status != cbr.StatusSuccess {
		t.Errorf("status = %q, want success", c.Outcome.Status)
	}
	// Intent verb "investigate" should produce a Troubleshooting case.
	if c.Category != cbr.CategoryTroubleshooting {
		t.Errorf("category = %q, want troubleshooting", c.Category)
	}
}

func TestRun_FailureCycleProducesTroubleshootingCase(t *testing.T) {
	// Failure WITHOUT pitfalls listed = Troubleshooting per
	// SD's category-assignment rules (we know it didn't work
	// but haven't extracted pitfall lessons). Failure WITH
	// pitfalls = Pitfall — covered by
	// TestRun_CuratorOutputDrivesCase.
	lib := &fakeLib{}
	a, err := New(Config{Librarian: lib, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	a.Record(CycleComplete{
		CycleID:   "cyc-fail",
		Status:    librarian.NarrativeStatusError,
		Summary:   "tool dispatcher errored mid-flight",
		UserInput: "summarise the latest paper",
		ReplyText: "I started, but the tool failed.",
	})

	cases := waitForCases(t, lib, 1)
	if cases[0].Outcome.Status != cbr.StatusFailure {
		t.Errorf("failed cycle should produce failure status; got %q", cases[0].Outcome.Status)
	}
	if cases[0].Category != cbr.CategoryTroubleshooting {
		t.Errorf("failure without pitfalls should yield Troubleshooting; got %q", cases[0].Category)
	}
}

func TestRun_AbandonedCycleSkipsCaseDerivation(t *testing.T) {
	lib := &fakeLib{}
	a, err := New(Config{Librarian: lib, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	a.Record(CycleComplete{
		CycleID:   "cyc-abandon",
		Status:    librarian.NarrativeStatusAbandoned,
		Summary:   "watchdog timed out",
		UserInput: "do the thing",
		ReplyText: "",
	})

	// Wait briefly for the narrative entry — but no case should appear.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(lib.snapshot()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := lib.caseSnapshot(); len(got) != 0 {
		t.Errorf("abandoned cycle should not derive a case; got %+v", got)
	}
}

func TestRun_EmptyUserInputSkipsCaseDerivation(t *testing.T) {
	lib := &fakeLib{}
	a, err := New(Config{Librarian: lib, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	a.Record(CycleComplete{
		CycleID:   "cyc-no-input",
		Status:    librarian.NarrativeStatusComplete,
		Summary:   "ok",
		UserInput: "",
	})
	time.Sleep(100 * time.Millisecond)
	if got := lib.caseSnapshot(); len(got) != 0 {
		t.Errorf("empty user input should not derive a case; got %+v", got)
	}
}

func TestRun_RetrievedCaseIDsTriggerUsageStatsUpdate(t *testing.T) {
	lib := &fakeLib{}
	a, err := New(Config{Librarian: lib, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	a.Record(CycleComplete{
		CycleID:          "cyc-1",
		Status:           librarian.NarrativeStatusComplete,
		Summary:          "ok",
		UserInput:        "hello",
		ReplyText:        "hi",
		RetrievedCaseIDs: []string{"case-a", "case-b"},
	})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if calls := lib.retrievalSnapshot(); len(calls) > 0 {
			if calls[0].IDs[0] != "case-a" || calls[0].IDs[1] != "case-b" {
				t.Errorf("retrieval IDs wrong: %+v", calls)
			}
			if !calls[0].Succeeded {
				t.Errorf("succeeded should be true for complete cycle")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("RecordCaseRetrieval was never called")
}

// embedderEcho records every Embed call and returns a fixed vector.
// Used to verify the archivist threads embedder errors and IDs.
type embedderEcho struct {
	mu       sync.Mutex
	calls    []string
	failNext bool
}

func (e *embedderEcho) Embed(_ context.Context, text string) ([]float32, error) {
	e.mu.Lock()
	e.calls = append(e.calls, text)
	failed := e.failNext
	e.failNext = false
	e.mu.Unlock()
	if failed {
		return nil, errFakeEmbedder
	}
	return []float32{1, 2, 3, 4}, nil
}
func (e *embedderEcho) Dimensions() int { return 4 }
func (e *embedderEcho) ID() string      { return "fake/embed/v1" }
func (e *embedderEcho) Close() error    { return nil }

var errFakeEmbedder = fakeEmbedderError("forced")

type fakeEmbedderError string

func (e fakeEmbedderError) Error() string { return string(e) }

func TestRun_StampsEmbedderIDOnDerivedCase(t *testing.T) {
	lib := &fakeLib{}
	em := &embedderEcho{}
	a, err := New(Config{Librarian: lib, Embedder: em, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	a.Record(CycleComplete{
		CycleID:   "cyc-emb",
		Status:    librarian.NarrativeStatusComplete,
		Summary:   "ok",
		UserInput: "investigate auth",
		ReplyText: "traced it",
	})
	cases := waitForCases(t, lib, 1)
	if cases[0].EmbedderID != "fake/embed/v1" {
		t.Errorf("embedder ID = %q, want fake/embed/v1", cases[0].EmbedderID)
	}
	if len(cases[0].Embedding) != 4 {
		t.Errorf("embedding len = %d, want 4", len(cases[0].Embedding))
	}
}

func TestRun_EmbedderErrorStillStoresCaseWithoutVector(t *testing.T) {
	lib := &fakeLib{}
	em := &embedderEcho{failNext: true}
	a, err := New(Config{Librarian: lib, Embedder: em, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	a.Record(CycleComplete{
		CycleID:   "cyc-err",
		Status:    librarian.NarrativeStatusComplete,
		Summary:   "ok",
		UserInput: "investigate auth",
		ReplyText: "traced it",
	})
	cases := waitForCases(t, lib, 1)
	if len(cases[0].Embedding) != 0 {
		t.Errorf("expected no vector after embed error; got %v", cases[0].Embedding)
	}
	if cases[0].EmbedderID != "" {
		t.Errorf("embedder ID should be empty after error; got %q", cases[0].EmbedderID)
	}
}

// waitForCases polls fakeLib until n cases land or the deadline expires.
func waitForCases(t *testing.T, lib *fakeLib, n int) []cbr.Case {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		got := lib.caseSnapshot()
		if len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waited for %d cases; got %d", n, len(lib.caseSnapshot()))
	return nil
}

// ---- Curator integration ----

func TestRun_CuratorOutputDrivesCase(t *testing.T) {
	// Curator returns a structured failure with pitfalls — case
	// should land with status=failure, category=Pitfall,
	// pitfalls populated, intent_class set from curator.
	lib := &fakeLib{}
	cur := &fakeCurator{
		result: CurationResult{
			IntentClassification: cbr.IntentDataQuery,
			IntentDescription:    "summarise the paper",
			Domain:               "research",
			Status:               cbr.StatusFailure,
			Confidence:           0.9,
			Assessment:           "the assistant misunderstood the question",
			Pitfalls:             []string{"clarify scope before answering"},
		},
	}
	a, err := New(Config{Librarian: lib, Curator: cur, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	a.Record(CycleComplete{
		CycleID:   "cyc-curator-fail",
		Status:    librarian.NarrativeStatusComplete,
		UserInput: "summarise the paper",
		ReplyText: "I don't know.",
	})

	cases := waitForCases(t, lib, 1)
	c := cases[0]
	if c.Outcome.Status != cbr.StatusFailure {
		t.Errorf("curator failure should drive status; got %q", c.Outcome.Status)
	}
	if c.Category != cbr.CategoryPitfall {
		t.Errorf("failure with pitfalls should classify as Pitfall; got %q", c.Category)
	}
	if c.Outcome.Confidence != 0.9 {
		t.Errorf("confidence should match curator; got %g", c.Outcome.Confidence)
	}
	if c.Problem.IntentClass != cbr.IntentDataQuery {
		t.Errorf("intent_class should track curator; got %q", c.Problem.IntentClass)
	}
	if c.Problem.Intent != "summarise the paper" {
		t.Errorf("intent description should be curator's, not user text verbatim; got %q", c.Problem.Intent)
	}
	if c.Problem.Domain != "research" {
		t.Errorf("domain should track curator; got %q", c.Problem.Domain)
	}
	if len(c.Outcome.Pitfalls) != 1 {
		t.Errorf("pitfalls should be populated; got %v", c.Outcome.Pitfalls)
	}
}

func TestRun_CuratorPartialDrivesDomainKnowledge(t *testing.T) {
	lib := &fakeLib{}
	cur := &fakeCurator{
		result: CurationResult{
			IntentClassification: cbr.IntentExploration,
			IntentDescription:    "do A and B",
			Status:               cbr.StatusPartial,
			Confidence:           0.6,
			Assessment:           "missed the second half",
		},
	}
	a, err := New(Config{Librarian: lib, Curator: cur, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	a.Record(CycleComplete{
		CycleID:   "cyc-partial",
		Status:    librarian.NarrativeStatusComplete,
		UserInput: "do A and B",
		ReplyText: "did A.",
	})

	cases := waitForCases(t, lib, 1)
	c := cases[0]
	if c.Outcome.Status != cbr.StatusPartial {
		t.Errorf("partial should track curator; got %q", c.Outcome.Status)
	}
	if c.Category != cbr.CategoryDomainKnowledge {
		t.Errorf("partial should classify as DomainKnowledge; got %q", c.Category)
	}
}

func TestRun_CuratorConversationGetsNoCategory(t *testing.T) {
	// Load-bearing fix: Conversation cycles produce cases for
	// audit but should NOT surface as patterns in CBR retrieval.
	lib := &fakeLib{}
	cur := &fakeCurator{
		result: CurationResult{
			IntentClassification: cbr.IntentConversation,
			IntentDescription:    "operator greeted me",
			Status:               cbr.StatusSuccess,
			Confidence:           0.95,
			Assessment:           "Conversational acknowledgement handled appropriately.",
		},
	}
	a, err := New(Config{Librarian: lib, Curator: cur, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	a.Record(CycleComplete{
		CycleID:   "cyc-greet",
		Status:    librarian.NarrativeStatusComplete,
		UserInput: "hi there",
		ReplyText: "Hello.",
	})

	cases := waitForCases(t, lib, 1)
	c := cases[0]
	if c.Outcome.Status != cbr.StatusSuccess {
		t.Errorf("conversational success should track curator; got %q", c.Outcome.Status)
	}
	if c.Category != "" {
		t.Errorf("Conversation classification should produce empty category; got %q", c.Category)
	}
	if c.Problem.IntentClass != cbr.IntentConversation {
		t.Errorf("intent_class should be Conversation; got %q", c.Problem.IntentClass)
	}
	if c.Problem.Intent == "hi there" {
		t.Errorf("intent should be curator's description, NOT verbatim user text; got %q", c.Problem.Intent)
	}
}

func TestRun_CuratorErrorFallsBackToHeuristic(t *testing.T) {
	// When the curator errors, archivist falls back to
	// HeuristicCurator. Case still gets stored.
	lib := &fakeLib{}
	cur := &fakeCurator{err: errFakeCurator}
	a, err := New(Config{Librarian: lib, Curator: cur, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	a.Record(CycleComplete{
		CycleID:   "cyc-curator-err",
		Status:    librarian.NarrativeStatusComplete,
		UserInput: "hi",
		ReplyText: "Hello.",
	})

	cases := waitForCases(t, lib, 1)
	c := cases[0]
	// HeuristicCurator should classify "hi" as Conversation —
	// short ack-style input. Category should be empty.
	if c.Problem.IntentClass != cbr.IntentConversation {
		t.Errorf("heuristic fallback should classify 'hi' as Conversation; got %q", c.Problem.IntentClass)
	}
	if c.Category != "" {
		t.Errorf("Conversation should produce empty category even via heuristic fallback; got %q", c.Category)
	}
}

func TestRun_NarrativeEntryGetsRichFields(t *testing.T) {
	// Phase 2B: the curator's structured output drives BOTH the
	// case AND the narrative entry. The entry's Intent / Outcome
	// / DelegationChain / Metrics should now reflect curator
	// + cog data, not just a flat summary.
	lib := &fakeLib{}
	cur := &fakeCurator{
		result: CurationResult{
			IntentClassification: cbr.IntentExploration,
			IntentDescription:    "investigate the auth flow",
			Domain:               "auth",
			Keywords:             []string{"auth", "investigate"},
			Status:               cbr.StatusSuccess,
			Confidence:           0.9,
			Assessment:           "found the timeout cause",
			Approach:             "delegated to researcher",
			Entities:             []string{"login service"},
		},
	}
	a, err := New(Config{Librarian: lib, Curator: cur, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	a.Record(CycleComplete{
		CycleID:   "cyc-rich",
		Status:    librarian.NarrativeStatusComplete,
		Summary:   "investigated auth",
		UserInput: "investigate the auth flow",
		ReplyText: "Found the timeout cause.",
		AgentCompletions: []agent.CompletionRecord{
			{
				AgentName:    "researcher",
				AgentCycleID: "agent-cyc-1",
				Success:      true,
				ToolsUsed:    []string{"brave_web_search", "jina_reader"},
				InputTokens:  500,
				OutputTokens: 200,
			},
		},
		ToolCalls: []ToolCallRecord{
			{Name: "agent_researcher", Success: true},
		},
	})

	// Wait for one case + one narrative entry.
	cases := waitForCases(t, lib, 1)
	if len(cases) != 1 {
		t.Fatalf("expected 1 case, got %d", len(cases))
	}

	// Find the narrative entry for this cycle.
	entries := lib.snapshot()
	if len(entries) != 1 {
		t.Fatalf("expected 1 narrative entry, got %d", len(entries))
	}
	e := entries[0]

	// Rich fields populated.
	if e.Intent.Classification != librarian.IntentExploration {
		t.Errorf("Intent.Classification = %q, want exploration", e.Intent.Classification)
	}
	if e.Intent.Description != "investigate the auth flow" {
		t.Errorf("Intent.Description = %q", e.Intent.Description)
	}
	if e.Intent.Domain != "auth" {
		t.Errorf("Intent.Domain = %q", e.Intent.Domain)
	}
	if e.Outcome.Status != librarian.OutcomeSuccess {
		t.Errorf("Outcome.Status = %q, want success", e.Outcome.Status)
	}
	if e.Outcome.Confidence != 0.9 {
		t.Errorf("Outcome.Confidence = %g", e.Outcome.Confidence)
	}
	if len(e.DelegationChain) != 1 {
		t.Fatalf("DelegationChain = %d steps, want 1", len(e.DelegationChain))
	}
	step := e.DelegationChain[0]
	if step.Agent != "researcher" {
		t.Errorf("step.Agent = %q", step.Agent)
	}
	if len(step.ToolsUsed) != 2 {
		t.Errorf("step.ToolsUsed = %v, want [brave_web_search jina_reader]", step.ToolsUsed)
	}
	if step.InputTokens != 500 || step.OutputTokens != 200 {
		t.Errorf("step tokens = %d/%d", step.InputTokens, step.OutputTokens)
	}

	// Legacy fields mirrored (the SQLite index reads these).
	if e.Status != librarian.NarrativeStatusComplete {
		t.Errorf("legacy Status not mirrored: %q", e.Status)
	}
	if e.Domain != "auth" {
		t.Errorf("legacy Domain not mirrored: %q", e.Domain)
	}
	if e.SchemaVersion != librarian.NarrativeSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", e.SchemaVersion, librarian.NarrativeSchemaVersion)
	}
}

func TestRun_SkippedCycleStillEmitsBasicNarrative(t *testing.T) {
	// Empty UserInput + abandoned cycles aren't case-worthy
	// but should still produce a narrative entry — the
	// librarian's window expects complete cycle coverage.
	lib := &fakeLib{}
	a, err := New(Config{Librarian: lib, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	a.Record(CycleComplete{
		CycleID: "cyc-empty",
		Status:  librarian.NarrativeStatusAbandoned,
		Summary: "abandoned mid-flight",
	})

	// Drain — narrative should land.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(lib.snapshot()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	entries := lib.snapshot()
	if len(entries) != 1 {
		t.Fatalf("expected 1 narrative entry; got %d", len(entries))
	}
	if len(lib.caseSnapshot()) != 0 {
		t.Errorf("abandoned cycle should not produce a case; got %d", len(lib.caseSnapshot()))
	}
	// The basic-narrative path doesn't run the curator, so
	// rich fields stay empty. Status comes from the cog directly.
	if entries[0].Status != librarian.NarrativeStatusAbandoned {
		t.Errorf("Status = %q", entries[0].Status)
	}
}

func TestNew_DefaultsToHeuristicCurator(t *testing.T) {
	// No Curator configured — archivist constructs HeuristicCurator.
	lib := &fakeLib{}
	a, err := New(Config{Librarian: lib, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	a.Record(CycleComplete{
		CycleID:   "cyc-default",
		Status:    librarian.NarrativeStatusComplete,
		UserInput: "explain http",
		ReplyText: "HTTP is...",
	})

	cases := waitForCases(t, lib, 1)
	c := cases[0]
	// HeuristicCurator classifies "explain http" (short, no
	// ack prefix, no question mark, no tools) as Conversation
	// per the heuristic rule. Verify the case still got
	// produced and structured fields are present.
	if c.Problem.IntentClass == "" {
		t.Errorf("heuristic default should populate intent_class; got empty")
	}
	if c.Problem.Intent == "" {
		t.Errorf("heuristic default should populate intent description; got empty")
	}
}

var errFakeCurator = fakeCuratorError("forced")

type fakeCuratorError string

func (e fakeCuratorError) Error() string { return string(e) }

// fakeCuratorCapturing captures the CurationInput so tests can
// assert on what the archivist passed in. fakeCurator (used
// elsewhere) ignores its input.
type fakeCuratorCapturing struct {
	mu     sync.Mutex
	input  *CurationInput
	result CurationResult
}

func (f *fakeCuratorCapturing) Curate(_ context.Context, in CurationInput) (CurationResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := in
	f.input = &cp
	return f.result, nil
}

func (f *fakeCuratorCapturing) lastInput() *CurationInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.input
}
