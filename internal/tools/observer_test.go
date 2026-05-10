package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/seamus-brady/retainer/internal/dag"
	"github.com/seamus-brady/retainer/internal/librarian"
	"github.com/seamus-brady/retainer/internal/observer"
)

// fakeObserver implements ObserverService for handler tests so we
// don't need a real librarian + DAG instance per test.
type fakeObserver struct {
	recent     []librarian.NarrativeEntry
	insp       observer.CycleInspection
	fact       *librarian.Fact
	lastInsp   string
	lastRecall int
	lastFact   string
}

func (f *fakeObserver) RecentCycles(limit int) []librarian.NarrativeEntry {
	f.lastRecall = limit
	return f.recent
}

func (f *fakeObserver) InspectCycle(cycleID string) observer.CycleInspection {
	f.lastInsp = cycleID
	return f.insp
}

func (f *fakeObserver) GetFact(key string) *librarian.Fact {
	f.lastFact = key
	return f.fact
}

// ---- inspect_cycle ----

func TestObserverInspectCycle_HappyPath(t *testing.T) {
	obs := &fakeObserver{
		insp: observer.CycleInspection{
			CycleID:     "abc-123",
			Type:        dag.NodeCognitive,
			Status:      dag.StatusComplete,
			StartedAt:   time.Date(2026, 4, 30, 14, 0, 0, 0, time.UTC),
			Duration:    1500 * time.Millisecond,
			Summary:     "User: hi\nReply: hello",
			NarrativeFound: true,
			Found:       true,
		},
	}
	h := ObserverInspectCycle{Observer: obs}
	out, err := h.Execute(context.Background(), []byte(`{"cycle_id":"abc-123"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cycle:    abc-123", "type:     cognitive", "status:   complete", "duration: 1.5s", "User: hi"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
}

func TestObserverInspectCycle_PrefixResolution(t *testing.T) {
	// Direct lookup misses → handler scans recent cycles for a prefix
	// match → re-inspects on the resolved full ID.
	cyc := librarian.NarrativeEntry{CycleID: "abc-12345-full", Status: librarian.NarrativeStatusComplete}
	obs := &fakeObserver{
		recent: []librarian.NarrativeEntry{cyc},
		// First InspectCycle returns Found: false; the prefix-resolution
		// path inspects again with the full ID. We can't easily mock
		// "different return per call" without a counter, so use a
		// sentinel value that proves the second lookup ran.
	}
	h := ObserverInspectCycle{Observer: obs}
	_, _ = h.Execute(context.Background(), []byte(`{"cycle_id":"abc-12345"}`))
	if obs.lastInsp != "abc-12345-full" {
		t.Errorf("prefix didn't resolve to full ID; lastInsp=%q", obs.lastInsp)
	}
}

func TestObserverInspectCycle_NotFound(t *testing.T) {
	obs := &fakeObserver{} // empty recent + Found:false insp
	h := ObserverInspectCycle{Observer: obs}
	out, err := h.Execute(context.Background(), []byte(`{"cycle_id":"missing"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "cycle not found: missing") {
		t.Errorf("got %q", out)
	}
}

func TestObserverInspectCycle_RejectsEmptyID(t *testing.T) {
	h := ObserverInspectCycle{Observer: &fakeObserver{}}
	_, err := h.Execute(context.Background(), []byte(`{"cycle_id":"  "}`))
	if err == nil || !strings.Contains(err.Error(), "cycle_id must not be empty") {
		t.Fatalf("err = %v", err)
	}
}

func TestObserverInspectCycle_EmptyInputErrors(t *testing.T) {
	h := ObserverInspectCycle{Observer: &fakeObserver{}}
	_, err := h.Execute(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "empty input") {
		t.Fatalf("err = %v", err)
	}
}

func TestObserverInspectCycle_MalformedInputErrors(t *testing.T) {
	h := ObserverInspectCycle{Observer: &fakeObserver{}}
	_, err := h.Execute(context.Background(), []byte(`{not json`))
	if err == nil || !strings.Contains(err.Error(), "decode input") {
		t.Fatalf("err = %v", err)
	}
}

func TestObserverInspectCycle_OmitsZeroFields(t *testing.T) {
	// A cycle with only summary populated (e.g. narrative-only entry
	// not in DAG) should still render legibly.
	obs := &fakeObserver{
		insp: observer.CycleInspection{CycleID: "x", Summary: "just text", Found: true},
	}
	h := ObserverInspectCycle{Observer: obs}
	out, _ := h.Execute(context.Background(), []byte(`{"cycle_id":"x"}`))
	if !strings.Contains(out, "cycle:    x") {
		t.Errorf("missing cycle id: %q", out)
	}
	if strings.Contains(out, "duration:") {
		t.Errorf("zero duration should be omitted: %q", out)
	}
	if strings.Contains(out, "started:") {
		t.Errorf("zero StartedAt should be omitted: %q", out)
	}
}

func TestObserverInspectCycle_RendersRichFields(t *testing.T) {
	conf := 0.85
	obs := &fakeObserver{
		insp: observer.CycleInspection{
			CycleID: "rich-1",
			Type:    dag.NodeCognitive,
			Status:  dag.StatusComplete,
			Found:   true,
			Intent: librarian.Intent{
				Classification: librarian.IntentComparison,
				Description:    "compare two metrics",
				Domain:         "weather",
			},
			Outcome: librarian.Outcome{
				Status:     librarian.OutcomeSuccess,
				Confidence: conf,
				Assessment: "looked good",
			},
			DelegationChain: []librarian.DelegationStep{
				{Agent: "researcher", AgentCycleID: "agent-cycle-id-12345", Instruction: "look it up", OutcomeText: "found it", ToolsUsed: []string{"brave_search"}},
			},
			Topics: []string{"weather", "comparison"},
			Entities: librarian.Entities{
				Locations:     []string{"Dublin"},
				Organisations: []string{"Met Eireann"},
			},
			Metrics: librarian.Metrics{
				TotalDurationMs:  1234,
				InputTokens:      100,
				OutputTokens:     50,
				ToolCalls:        2,
				AgentDelegations: 1,
				ModelUsed:        "claude-opus-4-7",
			},
			NarrativeFound: true,
		},
	}
	h := ObserverInspectCycle{Observer: obs}
	out, err := h.Execute(context.Background(), []byte(`{"cycle_id":"rich-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	wants := []string{
		"intent:   comparison (weather)",
		"desc:   compare two metrics",
		"outcome:  success",
		"confidence 0.85",
		"assess: looked good",
		"topics:   weather, comparison",
		"locs:   Dublin",
		"orgs:   Met Eireann",
		"delegations:",
		"- researcher",
		"instr:  look it up",
		"out:    found it",
		"tools:  brave_search",
		"metrics: 1234ms tokens=100/50 tools=2 delegations=1 model=claude-opus-4-7",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in output:\n%s", w, out)
		}
	}
}

func TestObserverInspectCycle_LegacyEntryOmitsRichLines(t *testing.T) {
	// A legacy entry has zero Intent/Outcome/Metrics — the rich
	// blocks must not render at all.
	obs := &fakeObserver{
		insp: observer.CycleInspection{
			CycleID:        "legacy-1",
			Status:         dag.StatusComplete,
			Summary:        "old shape",
			NarrativeFound: true,
			Found:          true,
		},
	}
	h := ObserverInspectCycle{Observer: obs}
	out, _ := h.Execute(context.Background(), []byte(`{"cycle_id":"legacy-1"}`))
	for _, banned := range []string{"intent:", "outcome:", "delegations:", "metrics:"} {
		if strings.Contains(out, banned) {
			t.Errorf("legacy entry should not render %q:\n%s", banned, out)
		}
	}
}

// ---- recall_recent ----

func TestObserverRecallRecent_DefaultLimit(t *testing.T) {
	obs := &fakeObserver{
		recent: []librarian.NarrativeEntry{
			{Timestamp: time.Date(2026, 4, 30, 9, 0, 0, 0, time.UTC), Status: librarian.NarrativeStatusComplete, CycleID: "first-cycle-id-long", Summary: "first"},
			{Timestamp: time.Date(2026, 4, 30, 9, 5, 0, 0, time.UTC), Status: librarian.NarrativeStatusComplete, CycleID: "second-cycle-id-long", Summary: "second"},
		},
	}
	h := ObserverRecallRecent{Observer: obs}
	out, err := h.Execute(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if obs.lastRecall != observerRecallDefaultLimit {
		t.Errorf("limit = %d, want %d (default)", obs.lastRecall, observerRecallDefaultLimit)
	}
	if !strings.Contains(out, "first") || !strings.Contains(out, "second") {
		t.Errorf("missing entries: %q", out)
	}
	if !strings.Contains(out, "first-c") {
		t.Errorf("expected short cycle id: %q", out)
	}
}

func TestObserverRecallRecent_NilInput(t *testing.T) {
	// recall_recent has all-optional params; nil input should default
	// to the default limit.
	obs := &fakeObserver{}
	h := ObserverRecallRecent{Observer: obs}
	if _, err := h.Execute(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if obs.lastRecall != observerRecallDefaultLimit {
		t.Errorf("limit = %d", obs.lastRecall)
	}
}

func TestObserverRecallRecent_RespectsLimit(t *testing.T) {
	obs := &fakeObserver{}
	h := ObserverRecallRecent{Observer: obs}
	_, _ = h.Execute(context.Background(), []byte(`{"limit":3}`))
	if obs.lastRecall != 3 {
		t.Errorf("limit = %d", obs.lastRecall)
	}
}

func TestObserverRecallRecent_CapsAtMax(t *testing.T) {
	obs := &fakeObserver{}
	h := ObserverRecallRecent{Observer: obs}
	_, _ = h.Execute(context.Background(), []byte(`{"limit":1000}`))
	if obs.lastRecall != observerRecallMaxLimit {
		t.Errorf("limit = %d, want %d (capped)", obs.lastRecall, observerRecallMaxLimit)
	}
}

func TestObserverRecallRecent_NegativeLimitFallsBackToDefault(t *testing.T) {
	obs := &fakeObserver{}
	h := ObserverRecallRecent{Observer: obs}
	_, _ = h.Execute(context.Background(), []byte(`{"limit":-5}`))
	if obs.lastRecall != observerRecallDefaultLimit {
		t.Errorf("limit = %d", obs.lastRecall)
	}
}

func TestObserverRecallRecent_EmptyStore(t *testing.T) {
	obs := &fakeObserver{}
	h := ObserverRecallRecent{Observer: obs}
	out, err := h.Execute(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no cycles recorded yet") {
		t.Errorf("got %q", out)
	}
}

func TestObserverRecallRecent_PrefersRichOutcomeStatus(t *testing.T) {
	// When Outcome.Status is set, recall_recent should show that
	// rather than the legacy NarrativeStatus — and the intent
	// classification should appear as a tag.
	obs := &fakeObserver{
		recent: []librarian.NarrativeEntry{
			{
				Timestamp: time.Date(2026, 4, 30, 9, 0, 0, 0, time.UTC),
				Status:    librarian.NarrativeStatusComplete,
				CycleID:   "rich-cycle-id-long",
				Summary:   "did a thing",
				Intent:    librarian.Intent{Classification: librarian.IntentDataQuery},
				Outcome:   librarian.Outcome{Status: librarian.OutcomePartial},
			},
		},
	}
	h := ObserverRecallRecent{Observer: obs}
	out, err := h.Execute(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[partial]") {
		t.Errorf("expected rich Outcome.Status in output: %q", out)
	}
	if strings.Contains(out, "[complete]") {
		t.Errorf("legacy status should not appear when rich Outcome present: %q", out)
	}
	if !strings.Contains(out, "<data_query>") {
		t.Errorf("expected intent classification tag: %q", out)
	}
}

func TestObserverRecallRecent_LegacyEntryFallsBackToNarrativeStatus(t *testing.T) {
	obs := &fakeObserver{
		recent: []librarian.NarrativeEntry{
			{
				Timestamp: time.Date(2026, 4, 30, 9, 0, 0, 0, time.UTC),
				Status:    librarian.NarrativeStatusComplete,
				CycleID:   "legacy-cycle-id-long",
				Summary:   "old shape",
			},
		},
	}
	h := ObserverRecallRecent{Observer: obs}
	out, _ := h.Execute(context.Background(), []byte(`{}`))
	if !strings.Contains(out, "[complete]") {
		t.Errorf("expected legacy NarrativeStatus when no Outcome: %q", out)
	}
	if strings.Contains(out, "<") {
		t.Errorf("no intent tag should appear for legacy entries: %q", out)
	}
}

func TestObserverRecallRecent_TruncatesLongSummary(t *testing.T) {
	obs := &fakeObserver{
		recent: []librarian.NarrativeEntry{
			{Timestamp: time.Now(), Summary: strings.Repeat("a", 300), Status: librarian.NarrativeStatusComplete, CycleID: "x"},
		},
	}
	h := ObserverRecallRecent{Observer: obs}
	out, _ := h.Execute(context.Background(), []byte(`{}`))
	if !strings.Contains(out, "...") {
		t.Errorf("expected truncation marker: %q", out)
	}
}

func TestObserverRecallRecent_MalformedInputErrors(t *testing.T) {
	h := ObserverRecallRecent{Observer: &fakeObserver{}}
	_, err := h.Execute(context.Background(), []byte(`{not json`))
	if err == nil || !strings.Contains(err.Error(), "decode input") {
		t.Fatalf("err = %v", err)
	}
}

// ---- get_fact ----

func TestObserverGetFact_Found(t *testing.T) {
	obs := &fakeObserver{
		fact: &librarian.Fact{
			Key: "user_name", Value: "Seamus",
			Scope: librarian.FactScopePersistent, Confidence: 0.95,
			Timestamp: time.Date(2026, 4, 30, 14, 0, 0, 0, time.UTC),
		},
	}
	h := ObserverGetFact{Observer: obs}
	out, err := h.Execute(context.Background(), []byte(`{"key":"user_name"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "key: user_name") {
		t.Errorf("missing key: %q", out)
	}
	if !strings.Contains(out, "value: Seamus") {
		t.Errorf("missing value: %q", out)
	}
}

func TestObserverGetFact_NotFound(t *testing.T) {
	h := ObserverGetFact{Observer: &fakeObserver{}}
	out, err := h.Execute(context.Background(), []byte(`{"key":"missing"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No fact found for key") {
		t.Errorf("got %q", out)
	}
}

func TestObserverGetFact_RejectsEmptyKey(t *testing.T) {
	h := ObserverGetFact{Observer: &fakeObserver{}}
	_, err := h.Execute(context.Background(), []byte(`{"key":"   "}`))
	if err == nil || !strings.Contains(err.Error(), "key must not be empty") {
		t.Fatalf("err = %v", err)
	}
}

func TestObserverGetFact_EmptyInputErrors(t *testing.T) {
	h := ObserverGetFact{Observer: &fakeObserver{}}
	_, err := h.Execute(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "empty input") {
		t.Fatalf("err = %v", err)
	}
}

func TestObserverGetFact_MalformedInputErrors(t *testing.T) {
	h := ObserverGetFact{Observer: &fakeObserver{}}
	_, err := h.Execute(context.Background(), []byte(`{not json`))
	if err == nil || !strings.Contains(err.Error(), "decode input") {
		t.Fatalf("err = %v", err)
	}
}

// ---- Tool metadata ----

func TestObserverTools_Metadata(t *testing.T) {
	for _, h := range []Handler{
		ObserverInspectCycle{},
		ObserverRecallRecent{},
		ObserverGetFact{},
	} {
		got := h.Tool()
		if got.Name == "" {
			t.Errorf("%T: empty Name", h)
		}
		if got.Description == "" {
			t.Errorf("%T: empty Description", h)
		}
	}
}

// ---- helpers ----

func TestShortCycleID_TruncatesLongID(t *testing.T) {
	if got := shortCycleID("01234567-abcd-ef01-2345-6789abcdef01"); got != "01234567" {
		t.Errorf("got %q", got)
	}
}

func TestShortCycleID_PassesShortIDThrough(t *testing.T) {
	if got := shortCycleID("abc"); got != "abc" {
		t.Errorf("got %q", got)
	}
}

func TestTruncateInline_ShortPassesThrough(t *testing.T) {
	if got := truncateInline("hello", 10); got != "hello" {
		t.Errorf("got %q", got)
	}
}

func TestTruncateInline_LongTruncates(t *testing.T) {
	if got := truncateInline("hello world", 5); got != "hello..." {
		t.Errorf("got %q", got)
	}
}
