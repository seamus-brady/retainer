package curator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seamus-brady/retainer/internal/cbr"
	"github.com/seamus-brady/retainer/internal/cyclelog"
	"github.com/seamus-brady/retainer/internal/identity"
	"github.com/seamus-brady/retainer/internal/librarian"
	"github.com/seamus-brady/retainer/internal/skills"
)

type recordingSink struct {
	mu     sync.Mutex
	events []cyclelog.Event
	err    error
}

func (r *recordingSink) Emit(ev cyclelog.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return r.err
}

func (r *recordingSink) snapshot() []cyclelog.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]cyclelog.Event, len(r.events))
	copy(out, r.events)
	return out
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func fixedNow() time.Time {
	return time.Date(2026, 4, 30, 9, 30, 0, 0, time.UTC)
}

// fakeLib implements LibrarianQuery for tests so we don't need a real
// SQLite instance and so we can assert what the curator queries for.
type fakeLib struct {
	count          int
	persistent     []librarian.Fact
	narrative      []librarian.NarrativeEntry
	cases          int
	scored         []cbr.Scored
	facts          map[string]*librarian.Fact
	countCalls     int
	factsCalls     int
	narrativeCalls int
	caseCountCalls int
	retrieveCalls  int
	getFactCalls   int
	lastFactsLimit int
	lastNarrLimit  int
	lastQuery      cbr.Query
	lastFactKey    string
}

func (f *fakeLib) PersistentFactCount() int {
	f.countCalls++
	return f.count
}

func (f *fakeLib) RecentPersistentFacts(limit int) []librarian.Fact {
	f.factsCalls++
	f.lastFactsLimit = limit
	return f.persistent
}

func (f *fakeLib) RecentNarrative(limit int) []librarian.NarrativeEntry {
	f.narrativeCalls++
	f.lastNarrLimit = limit
	return f.narrative
}

func (f *fakeLib) CaseCount() int {
	f.caseCountCalls++
	return f.cases
}

func (f *fakeLib) RetrieveCases(_ context.Context, q cbr.Query) []cbr.Scored {
	f.retrieveCalls++
	f.lastQuery = q
	return f.scored
}

func (f *fakeLib) GetFact(key string) *librarian.Fact {
	f.getFactCalls++
	f.lastFactKey = key
	if f.facts == nil {
		return nil
	}
	return f.facts[key]
}

func basicIdentity() *identity.Identity {
	return &identity.Identity{
		Persona:  "I am {{agent_name}}.",
		Preamble: "Date: {{date}}\nFacts: {{persistent_fact_count}} [OMIT IF EMPTY]\nFresh: {{recent_fact_sample}} [OMIT IF EMPTY]\nLog:\n{{recent_narrative}} [OMIT IF EMPTY]",
	}
}

// ---- New ----

func TestNew_RequiresIdentity(t *testing.T) {
	_, err := New(Config{})
	if err == nil || !strings.Contains(err.Error(), "Identity is required") {
		t.Fatalf("err = %v, want Identity-required", err)
	}
}

func TestNew_DefaultsAreApplied(t *testing.T) {
	c, err := New(Config{Identity: basicIdentity()})
	if err != nil {
		t.Fatal(err)
	}
	if c.cfg.NowFn == nil {
		t.Error("NowFn should default")
	}
	if c.cfg.Logger == nil {
		t.Error("Logger should default")
	}
}

// ---- recalled_cases sensorium block (Phase 3) ----

func TestBuildSensoriumBlock_QueriesLibrarianWhenUserInputSet(t *testing.T) {
	lib := &fakeLib{
		scored: []cbr.Scored{
			{
				Score: 0.9,
				Case: cbr.Case{
					ID:       "abcd1234-5678",
					Category: cbr.CategoryStrategy,
					Problem: cbr.Problem{
						IntentClass: cbr.IntentDataQuery,
						Intent:      "look up the weather",
						Domain:      "weather",
					},
					Solution: cbr.Solution{Approach: "delegate to researcher"},
					Outcome:  cbr.Outcome{Assessment: "worked"},
				},
			},
		},
	}
	c, _ := New(Config{
		Identity:  basicIdentity(),
		Librarian: lib,
		AgentName: "Nemo",
		NowFn:     fixedNow,
		Logger:    discardLogger(),
	})

	cyc := CycleContext{CycleID: "cyc-recall", UserInput: "what's the weather like in Dublin?"}
	got := c.buildSensoriumBlock(context.Background(), cyc, &assemblyStats{})

	if lib.retrieveCalls != 1 {
		t.Fatalf("RetrieveCases calls = %d, want 1", lib.retrieveCalls)
	}
	if lib.lastQuery.Intent != "what's the weather like in Dublin?" {
		t.Errorf("Query.Intent = %q", lib.lastQuery.Intent)
	}
	if lib.lastQuery.MaxResults != recalledCasesQueryMaxResults {
		t.Errorf("Query.MaxResults = %d, want %d", lib.lastQuery.MaxResults, recalledCasesQueryMaxResults)
	}
	if !strings.Contains(got, "<recalled_cases>") {
		t.Errorf("recalled_cases block missing from sensorium:\n%s", got)
	}
	if !strings.Contains(got, "abcd1234") {
		t.Errorf("expected case id prefix in output:\n%s", got)
	}
}

func TestBuildSensoriumBlock_SkipsRetrievalOnEmptyUserInput(t *testing.T) {
	lib := &fakeLib{}
	c, _ := New(Config{
		Identity:  basicIdentity(),
		Librarian: lib,
		AgentName: "Nemo",
		NowFn:     fixedNow,
		Logger:    discardLogger(),
	})

	for _, in := range []string{"", "   ", "\t\n"} {
		got := c.buildSensoriumBlock(context.Background(), CycleContext{UserInput: in}, &assemblyStats{})
		if strings.Contains(got, "<recalled_cases>") {
			t.Errorf("recalled_cases must drop out for input %q:\n%s", in, got)
		}
	}
	if lib.retrieveCalls != 0 {
		t.Errorf("RetrieveCases should not be called for whitespace input; got %d", lib.retrieveCalls)
	}
}

func TestBuildSensoriumBlock_SkipsRetrievalWhenLibrarianNil(t *testing.T) {
	c, _ := New(Config{
		Identity:  basicIdentity(),
		Librarian: nil,
		AgentName: "Nemo",
		NowFn:     fixedNow,
		Logger:    discardLogger(),
	})

	got := c.buildSensoriumBlock(context.Background(), CycleContext{UserInput: "hello"}, &assemblyStats{})
	if strings.Contains(got, "<recalled_cases>") {
		t.Errorf("recalled_cases must drop out when librarian is nil:\n%s", got)
	}
}

func TestBuildPrompt_EmitsRecalledCaseIDsOnAssembledEvent(t *testing.T) {
	// Verify the curator_assembled cycle-log event carries the
	// rendered case IDs (8-char prefix). This is the audit hook
	// integration tests use to assert "case X reached the agent."
	lib := &fakeLib{
		scored: []cbr.Scored{
			{Score: 0.95, Case: cbr.Case{ID: "abcd1234-aaaa-bbbb", Category: cbr.CategoryStrategy}},
			{Score: 0.85, Case: cbr.Case{ID: "efgh5678-aaaa-bbbb", Category: cbr.CategoryTroubleshooting}},
		},
	}
	sink := &recordingSink{}
	c, _ := New(Config{
		Identity:  basicIdentity(),
		Librarian: lib,
		AgentName: "Nemo",
		NowFn:     fixedNow,
		Logger:    discardLogger(),
		CycleLog:  sink,
		IDFn:      func() string { return "assembly-id" },
	})

	_ = c.buildPrompt(context.Background(), CycleContext{
		CycleID:   "cog-cyc-7",
		UserInput: "what's the weather?",
	})

	events := sink.snapshot()
	var assembled *cyclelog.Event
	for i := range events {
		if events[i].Type == cyclelog.EventCuratorAssembled {
			assembled = &events[i]
			break
		}
	}
	if assembled == nil {
		t.Fatal("expected curator_assembled event; got none")
	}
	if len(assembled.RecalledCaseIDs) != 2 {
		t.Fatalf("RecalledCaseIDs = %v, want 2", assembled.RecalledCaseIDs)
	}
	want := map[string]bool{"abcd1234": false, "efgh5678": false}
	for _, id := range assembled.RecalledCaseIDs {
		if _, ok := want[id]; ok {
			want[id] = true
			continue
		}
		t.Errorf("unexpected RecalledCaseID %q", id)
	}
	for id, seen := range want {
		if !seen {
			t.Errorf("missing %q in RecalledCaseIDs", id)
		}
	}
}

// ---- BuildSystemPrompt — actor path ----

func TestBuildSystemPrompt_ThroughActor(t *testing.T) {
	lib := &fakeLib{
		count:     2,
		narrative: []librarian.NarrativeEntry{{Timestamp: fixedNow(), Summary: "hello world"}},
	}
	c, _ := New(Config{
		Identity:  basicIdentity(),
		Librarian: lib,
		AgentName: "Nemo",
		NowFn:     fixedNow,
		Logger:    discardLogger(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	got := c.BuildSystemPromptString(context.Background(), CycleContext{CycleID: "cyc-1"})
	if !strings.Contains(got, "I am Nemo.") {
		t.Errorf("persona missing: %q", got)
	}
	if !strings.Contains(got, "Date: 2026-04-30") {
		t.Errorf("date slot missing: %q", got)
	}
	if !strings.Contains(got, "Facts: 2") {
		t.Errorf("fact count missing: %q", got)
	}
	if !strings.Contains(got, "hello world") {
		t.Errorf("recent narrative missing: %q", got)
	}
}

// ---- assembleSlots: pure path, fast ----

func TestAssembleSlots_NilLibrarianLeavesMemorySlotsEmpty(t *testing.T) {
	c, _ := New(Config{
		Identity:  basicIdentity(),
		Librarian: nil,
		AgentName: "Nemo",
		NowFn:     fixedNow,
		Logger:    discardLogger(),
	})
	slots, _ := c.assembleSlots(CycleContext{})
	for _, key := range []string{"persistent_fact_count", "recent_fact_sample", "recent_narrative", "last_session_summary"} {
		if slots[key] != "" {
			t.Errorf("slot %q = %q, want empty", key, slots[key])
		}
	}
}

func TestAssembleSlots_QueriesLibrarianWithCorrectLimits(t *testing.T) {
	lib := &fakeLib{}
	c, _ := New(Config{Identity: basicIdentity(), Librarian: lib, NowFn: fixedNow, Logger: discardLogger()})
	_, _ = c.assembleSlots(CycleContext{})
	if lib.lastFactsLimit != recentFactsForSlots {
		t.Errorf("facts limit = %d, want %d", lib.lastFactsLimit, recentFactsForSlots)
	}
	if lib.lastNarrLimit != recentNarrativeForSlots {
		t.Errorf("narrative limit = %d, want %d", lib.lastNarrLimit, recentNarrativeForSlots)
	}
	if lib.countCalls != 1 {
		t.Errorf("count calls = %d, want 1", lib.countCalls)
	}
}

func TestAssembleSlots_PopulatesAllMemorySlots(t *testing.T) {
	lib := &fakeLib{
		count: 4,
		persistent: []librarian.Fact{
			{Key: "user_name", Value: "Seamus", Confidence: 0.95},
			{Key: "tz", Value: "Europe/Dublin", Confidence: 1.0},
		},
		narrative: []librarian.NarrativeEntry{
			{Timestamp: fixedNow().Add(-time.Hour), Summary: "older"},
			{Timestamp: fixedNow(), Summary: "newest"},
		},
	}
	c, _ := New(Config{Identity: basicIdentity(), Librarian: lib, NowFn: fixedNow, Logger: discardLogger()})
	slots, _ := c.assembleSlots(CycleContext{})
	if slots["persistent_fact_count"] != "4" {
		t.Errorf("count slot = %q", slots["persistent_fact_count"])
	}
	if !strings.Contains(slots["recent_fact_sample"], "user_name = Seamus") {
		t.Errorf("recent_fact_sample = %q", slots["recent_fact_sample"])
	}
	if !strings.Contains(slots["recent_fact_sample"], "0.95") {
		t.Errorf("confidence missing: %q", slots["recent_fact_sample"])
	}
	if !strings.Contains(slots["recent_narrative"], "older") || !strings.Contains(slots["recent_narrative"], "newest") {
		t.Errorf("recent_narrative = %q", slots["recent_narrative"])
	}
	if slots["last_session_summary"] != "newest" {
		t.Errorf("last_session_summary = %q, want 'newest'", slots["last_session_summary"])
	}
}

func TestAssembleSlots_ZeroCountStaysEmptyForOmitIf(t *testing.T) {
	// PersistentFactCount returning 0 must produce an empty slot (not
	// the literal string "0") so OMIT IF EMPTY drops the line.
	lib := &fakeLib{count: 0}
	c, _ := New(Config{Identity: basicIdentity(), Librarian: lib, NowFn: fixedNow, Logger: discardLogger()})
	slots, _ := c.assembleSlots(CycleContext{})
	if slots["persistent_fact_count"] != "" {
		t.Errorf("zero count = %q, want empty", slots["persistent_fact_count"])
	}
}

func TestAssembleSlots_PassesCycleContext(t *testing.T) {
	c, _ := New(Config{Identity: basicIdentity(), NowFn: fixedNow, Logger: discardLogger()})
	slots, _ := c.assembleSlots(CycleContext{
		CycleID:      "abc",
		InputSource:  "scheduler",
		QueueDepth:   3,
		MessageCount: 12,
	})
	if slots["cycle_id"] != "abc" {
		t.Errorf("cycle_id = %q", slots["cycle_id"])
	}
	if slots["input_source"] != "scheduler" {
		t.Errorf("input_source = %q", slots["input_source"])
	}
	if slots["queue_depth"] != "3" {
		t.Errorf("queue_depth = %q", slots["queue_depth"])
	}
	if slots["message_count"] != "12" {
		t.Errorf("message_count = %q", slots["message_count"])
	}
}

func TestAssembleSlots_DefaultInputSourceUser(t *testing.T) {
	c, _ := New(Config{Identity: basicIdentity(), NowFn: fixedNow, Logger: discardLogger()})
	slots, _ := c.assembleSlots(CycleContext{})
	if slots["input_source"] != "user" {
		t.Errorf("default input_source = %q, want 'user'", slots["input_source"])
	}
}

func TestAssembleSlots_AgentVersionPasses(t *testing.T) {
	c, _ := New(Config{
		Identity:     basicIdentity(),
		AgentName:    "Nemo",
		AgentVersion: "0.1.2",
		NowFn:        fixedNow,
		Logger:       discardLogger(),
	})
	slots, _ := c.assembleSlots(CycleContext{})
	if slots["agent_version"] != "0.1.2" {
		t.Errorf("agent_version = %q", slots["agent_version"])
	}
}

// ---- curator_assembled emission ----

// stubID returns an injectable IDFn for deterministic test assertions.
func stubID(id string) func() string {
	return func() string { return id }
}

func TestEmitAssembled_RecordsLibrarianStats(t *testing.T) {
	sink := &recordingSink{}
	lib := &fakeLib{
		count: 7,
		persistent: []librarian.Fact{
			{Key: "a", Value: "1", Confidence: 1.0},
			{Key: "b", Value: "2", Confidence: 1.0},
		},
		narrative: []librarian.NarrativeEntry{
			{Timestamp: fixedNow(), Summary: "one"},
			{Timestamp: fixedNow(), Summary: "two"},
			{Timestamp: fixedNow(), Summary: "three"},
		},
	}
	c, _ := New(Config{
		Identity:  basicIdentity(),
		Librarian: lib,
		AgentName: "Nemo",
		NowFn:     fixedNow,
		IDFn:      stubID("assembly-1"),
		Logger:    discardLogger(),
		CycleLog:  sink,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	prompt := c.BuildSystemPromptString(context.Background(), CycleContext{CycleID: "cog-cyc-1"})

	events := sink.snapshot()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Type != cyclelog.EventCuratorAssembled {
		t.Errorf("type = %q, want curator_assembled", ev.Type)
	}
	if ev.CycleID != "assembly-1" {
		t.Errorf("cycle_id = %q, want curator's own work-unit id", ev.CycleID)
	}
	if ev.ParentID != "cog-cyc-1" {
		t.Errorf("parent_id = %q, want cog cycle id", ev.ParentID)
	}
	if ev.PromptChars != len(prompt) {
		t.Errorf("prompt_chars = %d, want %d", ev.PromptChars, len(prompt))
	}
	if ev.NarrativeEntries != 3 {
		t.Errorf("narrative_entries = %d, want 3", ev.NarrativeEntries)
	}
	if ev.FactSampleCount != 2 {
		t.Errorf("fact_sample_count = %d, want 2", ev.FactSampleCount)
	}
	if ev.FactCount != 7 {
		t.Errorf("fact_count = %d, want 7", ev.FactCount)
	}
}

func TestEmitAssembled_FreshAssemblyIDPerCall(t *testing.T) {
	// Each BuildSystemPrompt call must produce a distinct assembly id —
	// when the same cog cycle triggers two assemblies (rare but
	// possible), they need to be distinguishable in the cycle log.
	sink := &recordingSink{}
	calls := 0
	c, _ := New(Config{
		Identity: basicIdentity(),
		NowFn:    fixedNow,
		IDFn: func() string {
			calls++
			return fmt.Sprintf("a-%d", calls)
		},
		Logger:   discardLogger(),
		CycleLog: sink,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	c.BuildSystemPromptString(context.Background(), CycleContext{CycleID: "cog-1"})
	c.BuildSystemPromptString(context.Background(), CycleContext{CycleID: "cog-1"})

	events := sink.snapshot()
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if events[0].CycleID == events[1].CycleID {
		t.Errorf("assembly ids should differ: %q == %q", events[0].CycleID, events[1].CycleID)
	}
	if events[0].ParentID != "cog-1" || events[1].ParentID != "cog-1" {
		t.Errorf("both events should share parent_id 'cog-1', got %q / %q", events[0].ParentID, events[1].ParentID)
	}
}

func TestNew_DefaultsIDFn(t *testing.T) {
	// IDFn defaults to uuid.NewString — verify a non-empty distinct id
	// is produced so the production path doesn't silently emit empty
	// cycle_id fields.
	c, _ := New(Config{Identity: basicIdentity()})
	id1 := c.cfg.IDFn()
	id2 := c.cfg.IDFn()
	if id1 == "" || id2 == "" {
		t.Fatalf("IDFn produced empty: %q / %q", id1, id2)
	}
	if id1 == id2 {
		t.Errorf("IDFn should produce distinct ids: %q == %q", id1, id2)
	}
}

func TestEmitAssembled_SkippedWithoutCycleID(t *testing.T) {
	// No cycle to attribute = nothing to log. Avoids unattributable
	// audit events from startup-time prompt builds (if any caller does
	// that).
	sink := &recordingSink{}
	c, _ := New(Config{
		Identity: basicIdentity(),
		NowFn:    fixedNow,
		Logger:   discardLogger(),
		CycleLog: sink,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	c.BuildSystemPromptString(context.Background(), CycleContext{}) // no CycleID
	if got := sink.snapshot(); len(got) != 0 {
		t.Errorf("events = %d, want 0", len(got))
	}
}

func TestEmitAssembled_SkippedWithoutSink(t *testing.T) {
	// Curator without a CycleLog must not crash on emit — it just
	// silently doesn't audit. (No way to assert no-emit directly; this
	// test exercises the nil-guard path.)
	c, _ := New(Config{
		Identity: basicIdentity(),
		NowFn:    fixedNow,
		Logger:   discardLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	_ = c.BuildSystemPromptString(context.Background(), CycleContext{CycleID: "cyc-1"})
}

func TestEmitAssembled_SinkErrorIsLoggedNotPanicked(t *testing.T) {
	// A failing sink must not poison the cycle. The curator logs and
	// moves on — auditability matters but it can't break the
	// cog → curator round-trip.
	sink := &recordingSink{err: errors.New("disk full")}
	c, _ := New(Config{
		Identity: basicIdentity(),
		NowFn:    fixedNow,
		Logger:   discardLogger(),
		CycleLog: sink,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	got := c.BuildSystemPromptString(context.Background(), CycleContext{CycleID: "cyc-1"})
	if got == "" {
		t.Errorf("prompt should still be returned even on sink error")
	}
}

func TestEmitAssembled_ZeroLibrarianStatsWhenNilLibrarian(t *testing.T) {
	sink := &recordingSink{}
	c, _ := New(Config{
		Identity: basicIdentity(),
		NowFn:    fixedNow,
		Logger:   discardLogger(),
		CycleLog: sink,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	c.BuildSystemPromptString(context.Background(), CycleContext{CycleID: "cyc-1"})
	events := sink.snapshot()
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	ev := events[0]
	if ev.NarrativeEntries != 0 || ev.FactCount != 0 || ev.FactSampleCount != 0 {
		t.Errorf("expected zero memory stats, got %+v", ev)
	}
	if ev.PromptChars == 0 {
		t.Error("prompt_chars should be non-zero (persona renders even without librarian)")
	}
}

// ---- Skills wiring ----

// writeSkillFile creates a SKILL.md at <dir>/<id>/SKILL.md for tests
// that need a real on-disk path the bootstrap reader can hit.
func writeSkillFile(t *testing.T, dir, id, body string) string {
	t.Helper()
	skillDir := filepath.Join(dir, id)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildPrompt_AppendsAvailableSkills(t *testing.T) {
	id := &identity.Identity{Persona: "I am {{agent_name}}.", Preamble: "Date: {{date}}"}
	c, _ := New(Config{
		Identity:  id,
		AgentName: "Nemo",
		NowFn:     fixedNow,
		Logger:    discardLogger(),
		Skills: []skills.SkillMeta{
			{ID: "delegation-strategy", Name: "Delegation Strategy",
				Description: "When/how to delegate to specialists.",
				Path:        "/skills/delegation-strategy/SKILL.md", Agents: []string{"cognitive"}},
		},
	})
	got := c.buildPrompt(context.Background(), CycleContext{CycleID: "x"}).Concat()
	if !strings.Contains(got, "<available_skills>") {
		t.Errorf("missing <available_skills>: %q", got)
	}
	if !strings.Contains(got, "Delegation Strategy") {
		t.Errorf("missing skill name: %q", got)
	}
	if !strings.Contains(got, "/skills/delegation-strategy/SKILL.md") {
		t.Errorf("missing skill location: %q", got)
	}
}

func TestBuildPrompt_NoSkillsBlockWhenNoneScoped(t *testing.T) {
	id := &identity.Identity{Persona: "x", Preamble: ""}
	c, _ := New(Config{
		Identity: id, NowFn: fixedNow, Logger: discardLogger(),
		Skills: []skills.SkillMeta{
			// Scoped only for researcher; cog's filter drops it.
			{ID: "x", Agents: []string{"researcher"}},
		},
	})
	got := c.buildPrompt(context.Background(), CycleContext{CycleID: "y"}).Concat()
	if strings.Contains(got, "<available_skills>") {
		t.Errorf("should not emit empty available_skills: %q", got)
	}
}

func TestBuildPrompt_NoSkillsBlockWhenSkillsConfigEmpty(t *testing.T) {
	id := &identity.Identity{Persona: "x", Preamble: ""}
	c, _ := New(Config{Identity: id, NowFn: fixedNow, Logger: discardLogger()})
	got := c.buildPrompt(context.Background(), CycleContext{CycleID: "y"}).Concat()
	if strings.Contains(got, "<available_skills>") {
		t.Errorf("no skills configured → no block: %q", got)
	}
}

func TestBuildPrompt_BootstrapInlinesOnFreshSession(t *testing.T) {
	dir := t.TempDir()
	path := writeSkillFile(t, dir, "delegation-strategy",
		"---\nname: delegation-strategy\ndescription: x\n---\n\nDelegate carefully.")

	id := &identity.Identity{Persona: "x", Preamble: ""}
	c, _ := New(Config{
		Identity: id, NowFn: fixedNow, Logger: discardLogger(),
		// No librarian → narrative_entries == 0 → fresh session.
		Skills: []skills.SkillMeta{
			{ID: "delegation-strategy", Name: "Delegation Strategy",
				Description: "x", Path: path, Agents: []string{"cognitive"}},
		},
		BootstrapSkillIDs: []string{"delegation-strategy"},
	})
	got := c.buildPrompt(context.Background(), CycleContext{CycleID: "z"}).Concat()
	if !strings.Contains(got, "<bootstrap_skills>") {
		t.Errorf("missing bootstrap_skills on fresh session: %q", got)
	}
	if !strings.Contains(got, "Delegate carefully.") {
		t.Errorf("missing inlined body: %q", got)
	}
	if !strings.Contains(got, `id="delegation-strategy"`) {
		t.Errorf("missing skill id attr: %q", got)
	}
}

func TestBuildPrompt_BootstrapSkippedWhenNarrativePresent(t *testing.T) {
	// Mock librarian returns a non-empty narrative — assemblySlots
	// reports stats.narrativeEntries > 0 → no bootstrap.
	dir := t.TempDir()
	path := writeSkillFile(t, dir, "x", "---\nname: x\ndescription: y\n---\nbody")

	lib := &fakeLib{narrative: []librarian.NarrativeEntry{
		{Timestamp: fixedNow(), Summary: "prior cycle"},
	}}
	c, _ := New(Config{
		Identity:  &identity.Identity{Persona: "x", Preamble: ""},
		Librarian: lib,
		NowFn:     fixedNow, Logger: discardLogger(),
		Skills:            []skills.SkillMeta{{ID: "x", Name: "X", Description: "y", Path: path}},
		BootstrapSkillIDs: []string{"x"},
	})
	got := c.buildPrompt(context.Background(), CycleContext{CycleID: "z"}).Concat()
	if strings.Contains(got, "<bootstrap_skills>") {
		t.Errorf("bootstrap should skip when narrative present: %q", got)
	}
}

func TestBuildPrompt_BootstrapSkippedWhenIDsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := writeSkillFile(t, dir, "x", "---\nname: x\ndescription: y\n---\nbody")
	c, _ := New(Config{
		Identity: &identity.Identity{Persona: "x", Preamble: ""},
		NowFn:    fixedNow, Logger: discardLogger(),
		Skills:            []skills.SkillMeta{{ID: "x", Path: path}},
		BootstrapSkillIDs: nil, // operator opt-out
	})
	got := c.buildPrompt(context.Background(), CycleContext{CycleID: "z"}).Concat()
	if strings.Contains(got, "<bootstrap_skills>") {
		t.Errorf("empty BootstrapSkillIDs should opt out: %q", got)
	}
}

func TestBuildPrompt_BootstrapSkipsUnknownIDs(t *testing.T) {
	dir := t.TempDir()
	path := writeSkillFile(t, dir, "real", "---\nname: real\ndescription: y\n---\nbody")

	c, _ := New(Config{
		Identity: &identity.Identity{Persona: "x", Preamble: ""},
		NowFn:    fixedNow, Logger: discardLogger(),
		Skills:            []skills.SkillMeta{{ID: "real", Name: "Real", Description: "y", Path: path}},
		BootstrapSkillIDs: []string{"ghost", "real"}, // ghost doesn't exist
	})
	got := c.buildPrompt(context.Background(), CycleContext{CycleID: "z"}).Concat()
	if !strings.Contains(got, `id="real"`) {
		t.Errorf("real skill should be inlined: %q", got)
	}
	if strings.Contains(got, `id="ghost"`) {
		t.Errorf("ghost id should not appear: %q", got)
	}
}

func TestBuildPrompt_BootstrapSkipsUnreadableBody(t *testing.T) {
	c, _ := New(Config{
		Identity: &identity.Identity{Persona: "x", Preamble: ""},
		NowFn:    fixedNow, Logger: discardLogger(),
		Skills: []skills.SkillMeta{
			// Path doesn't exist — bootstrap skips silently.
			{ID: "missing", Path: "/nonexistent/SKILL.md"},
		},
		BootstrapSkillIDs: []string{"missing"},
	})
	got := c.buildPrompt(context.Background(), CycleContext{CycleID: "z"}).Concat()
	if strings.Contains(got, "<bootstrap_skills>") {
		t.Errorf("unreadable body should produce no block: %q", got)
	}
}

// ---- Helpers ----

func TestFormatFacts_Empty(t *testing.T) {
	if got := formatFacts(nil); got != "" {
		t.Errorf("empty = %q", got)
	}
}

func TestFormatFacts_OrderPreserved(t *testing.T) {
	got := formatFacts([]librarian.Fact{
		{Key: "a", Value: "1", Confidence: 0.5},
		{Key: "b", Value: "2", Confidence: 0.7},
	})
	if !strings.HasPrefix(got, "- a = 1 (confidence: 0.50)") {
		t.Errorf("first line wrong: %q", got)
	}
	if !strings.Contains(got, "- b = 2 (confidence: 0.70)") {
		t.Errorf("second line missing: %q", got)
	}
}

func TestFormatNarrative_Empty(t *testing.T) {
	if got := formatNarrative(nil); got != "" {
		t.Errorf("empty = %q", got)
	}
}

func TestFormatNarrative_TruncatesLongSummary(t *testing.T) {
	long := strings.Repeat("x", recentEntrySummaryChars+50)
	got := formatNarrative([]librarian.NarrativeEntry{
		{Timestamp: fixedNow(), Summary: long},
	})
	if !strings.HasSuffix(got, "...") {
		t.Errorf("missing truncation marker: %q", got)
	}
	// Length of the rendered line: "- HH:MM: " (9) + 200 chars + "..." (3)
	if len(got) > recentEntrySummaryChars+15 {
		t.Errorf("output too long: %d chars", len(got))
	}
}

func TestFormatNarrative_RendersAllEntries(t *testing.T) {
	got := formatNarrative([]librarian.NarrativeEntry{
		{Timestamp: fixedNow().Add(-time.Hour), Summary: "first"},
		{Timestamp: fixedNow(), Summary: "second"},
	})
	if !strings.Contains(got, "first") || !strings.Contains(got, "second") {
		t.Errorf("missing entries: %q", got)
	}
}

func TestMostRecentSummary_LastEntry(t *testing.T) {
	got := mostRecentSummary([]librarian.NarrativeEntry{
		{Summary: "old"},
		{Summary: "recent  "},
	})
	if got != "recent" { // trimmed
		t.Errorf("got %q, want 'recent'", got)
	}
}

func TestMostRecentSummary_Empty(t *testing.T) {
	if got := mostRecentSummary(nil); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestTruncate_ShortPassesThrough(t *testing.T) {
	if got := truncate("hi", 10); got != "hi" {
		t.Errorf("got %q", got)
	}
}

func TestTruncate_LongAddsMarker(t *testing.T) {
	if got := truncate("0123456789abcdef", 5); got != "01234..." {
		t.Errorf("got %q", got)
	}
}

func TestIntToStr_Zero(t *testing.T) {
	if got := intToStr(0); got != "" {
		t.Errorf("zero → %q, want empty", got)
	}
}

func TestIntToStr_Positive(t *testing.T) {
	if got := intToStr(42); got != "42" {
		t.Errorf("got %q", got)
	}
}

func TestInputSource_Defaults(t *testing.T) {
	if got := inputSource(""); got != "user" {
		t.Errorf("empty → %q, want user", got)
	}
	if got := inputSource("scheduler"); got != "scheduler" {
		t.Errorf("scheduler → %q", got)
	}
}

// ---- End-to-end with real librarian ----

func TestBuildSystemPrompt_AgainstRealLibrarian(t *testing.T) {
	dir := t.TempDir()
	lib, err := librarian.New(librarian.Options{DataDir: dir, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	libCtx, libCancel := context.WithCancel(context.Background())
	defer libCancel()
	go lib.Run(libCtx)

	lib.RecordFact(librarian.Fact{
		Key: "user_name", Value: "Seamus",
		Scope: librarian.FactScopePersistent, Confidence: 1.0,
	})
	lib.RecordNarrative(librarian.NarrativeEntry{
		CycleID: "cyc-1", Timestamp: fixedNow(),
		Status: librarian.NarrativeStatusComplete, Summary: "had a chat",
	})

	c, err := New(Config{
		Identity:  basicIdentity(),
		Librarian: lib,
		AgentName: "Nemo",
		NowFn:     fixedNow,
		Logger:    discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	curatorCtx, curatorCancel := context.WithCancel(context.Background())
	defer curatorCancel()
	go c.Run(curatorCtx)

	got := c.BuildSystemPromptString(context.Background(), CycleContext{CycleID: "cyc-2"})
	if !strings.Contains(got, "Facts: 1") {
		t.Errorf("fact count missing: %q", got)
	}
	if !strings.Contains(got, "user_name = Seamus") {
		t.Errorf("recent fact missing: %q", got)
	}
	if !strings.Contains(got, "had a chat") {
		t.Errorf("narrative missing: %q", got)
	}
}

func TestBuildSystemPrompt_OmitsEmptyMemorySlots(t *testing.T) {
	// Fresh workspace — no facts, no narrative. The OMIT-IF rules in
	// the preamble must drop the memory lines so the prompt doesn't
	// leak template syntax or empty bullet points.
	dir := t.TempDir()
	lib, err := librarian.New(librarian.Options{DataDir: dir, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	libCtx, libCancel := context.WithCancel(context.Background())
	defer libCancel()
	go lib.Run(libCtx)

	c, _ := New(Config{
		Identity:  basicIdentity(),
		Librarian: lib,
		AgentName: "Nemo",
		NowFn:     fixedNow,
		Logger:    discardLogger(),
	})
	curatorCtx, curatorCancel := context.WithCancel(context.Background())
	defer curatorCancel()
	go c.Run(curatorCtx)

	got := c.BuildSystemPromptString(context.Background(), CycleContext{})
	if strings.Contains(got, "Facts:") {
		t.Errorf("Facts line should be omitted on empty store: %q", got)
	}
	if strings.Contains(got, "Fresh:") {
		t.Errorf("Fresh line should be omitted on empty store: %q", got)
	}
	if strings.Contains(got, "{{") {
		t.Errorf("unrendered slot leaked: %q", got)
	}
}

// TestBuildSystemPrompt_StablePrefixIsByteIdenticalAcrossCycles —
// the cache-aware contract: BuildSystemPrompt's Stable half must be
// byte-equal across cycles in a session even when the Dynamic half
// differs. This is what makes Anthropic's cache_control marker actually
// hit: the prefix before the marker is identical, so the cached tokens
// match. Different timestamps / queue depth / message count etc. land
// in Dynamic; Stable stays put.
func TestBuildSystemPrompt_StablePrefixIsByteIdenticalAcrossCycles(t *testing.T) {
	id := basicIdentity()
	c, err := New(Config{
		Identity:  id,
		Librarian: &fakeLib{}, // empty memory; sufficient for prefix test
		AgentName: "Nemo",
		NowFn:     fixedNow,
		IDFn:      stubID("a"),
		Logger:    discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}

	p1 := c.buildPrompt(context.Background(), CycleContext{
		CycleID:      "cyc-1",
		InputSource:  "user",
		QueueDepth:   0,
		MessageCount: 1,
	})
	p2 := c.buildPrompt(context.Background(), CycleContext{
		CycleID:      "cyc-2",   // different
		InputSource:  "user",
		QueueDepth:   3,         // different
		MessageCount: 17,        // different
	})

	if p1.Stable != p2.Stable {
		t.Errorf("Stable must be byte-identical across cycles\np1.Stable=%q\np2.Stable=%q", p1.Stable, p2.Stable)
	}

	// Sanity: Dynamic should differ (proves the test setup is not
	// trivially passing because both halves are equal).
	// In a minimal-substrate setup it might be empty in both cases;
	// the assertion is "if Dynamic varies, Stable still doesn't"
	// so this branch is only meaningful when Dynamic can change.
	// Currently the preamble doesn't reference the cycle-context
	// values that differ, so this is informational, not a hard pin.
	t.Logf("Stable len=%d Dynamic1=%q Dynamic2=%q", len(p1.Stable), p1.Dynamic, p2.Dynamic)
}

func TestBuildSystemPrompt_ReturnsStructured(t *testing.T) {
	c, err := New(Config{
		Identity:  basicIdentity(),
		Librarian: &fakeLib{},
		AgentName: "Nemo",
		NowFn:     fixedNow,
		IDFn:      stubID("a"),
		Logger:    discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	prompt := c.BuildSystemPrompt(context.Background(), CycleContext{CycleID: "cyc-1"})
	if prompt.Stable == "" {
		t.Error("Stable should be non-empty (persona renders into it)")
	}
	// Concat round-trip
	concat := prompt.Concat()
	if !strings.Contains(concat, "I am Nemo.") {
		t.Errorf("Concat missing persona text: %q", concat)
	}
}
