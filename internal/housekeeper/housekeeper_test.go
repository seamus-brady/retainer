package housekeeper

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seamus-brady/retainer/internal/cbr"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakePruner records every PruneNarrativeIndex call. Tests inspect
// the captured cutoff times to verify the housekeeper is computing
// `now - windowDays` correctly. Doubles as the CBR-side fake when
// CBRSweepEnabled is set — captures supersede + suppress calls so
// dedup/prune behaviour can be asserted without spinning up a real
// librarian.
type fakePruner struct {
	mu           sync.Mutex
	calls        []time.Time
	removed      int64
	err          error
	cases        []cbr.Case
	supersedes   []supersedeCall
	suppresses   []string
	supersedeErr error
	suppressErr  error
}

type supersedeCall struct {
	id, byID string
}

func (f *fakePruner) PruneNarrativeIndex(cutoff time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cutoff)
	return f.removed, f.err
}

func (f *fakePruner) AllCases() []cbr.Case {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]cbr.Case, len(f.cases))
	copy(out, f.cases)
	return out
}

func (f *fakePruner) SupersedeCase(id, byID string) (cbr.Case, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.supersedeErr != nil {
		return cbr.Case{}, f.supersedeErr
	}
	f.supersedes = append(f.supersedes, supersedeCall{id: id, byID: byID})
	for i, c := range f.cases {
		if c.ID == id {
			f.cases[i].SupersededBy = byID
			return f.cases[i], nil
		}
	}
	return cbr.Case{ID: id, SupersededBy: byID}, nil
}

func (f *fakePruner) SuppressCase(id string) (cbr.Case, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.suppressErr != nil {
		return cbr.Case{}, f.suppressErr
	}
	f.suppresses = append(f.suppresses, id)
	for i, c := range f.cases {
		if c.ID == id {
			f.cases[i].Redacted = true
			return f.cases[i], nil
		}
	}
	return cbr.Case{ID: id, Redacted: true}, nil
}

func (f *fakePruner) supersedeSnapshot() []supersedeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]supersedeCall, len(f.supersedes))
	copy(out, f.supersedes)
	return out
}

func (f *fakePruner) suppressSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.suppresses))
	copy(out, f.suppresses)
	return out
}

func (f *fakePruner) snapshot() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]time.Time, len(f.calls))
	copy(out, f.calls)
	return out
}

// ---- New ----

func TestNew_RequiresLibrarian(t *testing.T) {
	_, err := New(Config{})
	if err == nil {
		t.Fatal("expected error when Librarian is nil")
	}
}

func TestNew_AppliesDefaults(t *testing.T) {
	hk, err := New(Config{Librarian: &fakePruner{}})
	if err != nil {
		t.Fatal(err)
	}
	if hk.cfg.TickInterval != defaultTickInterval {
		t.Errorf("tick = %v, want %v", hk.cfg.TickInterval, defaultTickInterval)
	}
	if hk.cfg.NarrativeWindowDays != defaultNarrativeWindowDays {
		t.Errorf("window = %d, want %d", hk.cfg.NarrativeWindowDays, defaultNarrativeWindowDays)
	}
	if hk.cfg.NowFn == nil {
		t.Error("NowFn should default")
	}
	if hk.cfg.Logger == nil {
		t.Error("Logger should default")
	}
}

func TestNew_NegativeIntervalUsesDefault(t *testing.T) {
	hk, _ := New(Config{Librarian: &fakePruner{}, TickInterval: -time.Hour})
	if hk.cfg.TickInterval != defaultTickInterval {
		t.Errorf("negative tick should default; got %v", hk.cfg.TickInterval)
	}
}

func TestNew_NegativeWindowUsesDefault(t *testing.T) {
	hk, _ := New(Config{Librarian: &fakePruner{}, NarrativeWindowDays: -7})
	if hk.cfg.NarrativeWindowDays != defaultNarrativeWindowDays {
		t.Errorf("negative window should default; got %d", hk.cfg.NarrativeWindowDays)
	}
}

// ---- sweep — direct ----

func TestSweep_ComputesCutoffFromNow(t *testing.T) {
	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	pruner := &fakePruner{}
	hk, _ := New(Config{
		Librarian:           pruner,
		NarrativeWindowDays: 60,
		NowFn:               func() time.Time { return now },
		Logger:              discardLogger(),
	})
	hk.sweep()

	calls := pruner.snapshot()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	want := now.Add(-60 * 24 * time.Hour)
	if !calls[0].Equal(want) {
		t.Errorf("cutoff = %v, want %v", calls[0], want)
	}
}

func TestSweep_WindowDaysAffectsCutoff(t *testing.T) {
	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	pruner := &fakePruner{}
	hk, _ := New(Config{
		Librarian:           pruner,
		NarrativeWindowDays: 7, // tighter window
		NowFn:               func() time.Time { return now },
		Logger:              discardLogger(),
	})
	hk.sweep()

	calls := pruner.snapshot()
	want := now.Add(-7 * 24 * time.Hour)
	if !calls[0].Equal(want) {
		t.Errorf("cutoff = %v, want %v", calls[0], want)
	}
}

func TestSweep_SwallowsPrunerError(t *testing.T) {
	// A failing prune must NOT panic the housekeeper goroutine — the
	// sweep just logs and returns. Next tick will retry.
	pruner := &fakePruner{err: errors.New("sql exploded")}
	hk, _ := New(Config{Librarian: pruner, Logger: discardLogger()})
	// Must not panic.
	hk.sweep()
}

// ---- Run loop ----

func TestRun_ImmediateSweepOnStartup(t *testing.T) {
	// Even with a long tick interval, the loop should sweep once
	// at startup so the operator sees the housekeeper engage.
	pruner := &fakePruner{}
	hk, _ := New(Config{
		Librarian:    pruner,
		TickInterval: time.Hour, // long
		Logger:       discardLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- hk.Run(ctx) }()

	// Wait until the immediate sweep lands. With NowFn=time.Now and
	// no other ticks within the tight test window, exactly one call.
	deadline := time.After(2 * time.Second)
	for {
		if calls := pruner.snapshot(); len(calls) >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("startup sweep didn't fire within deadline")
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run didn't return after cancel")
	}
}

func TestRun_TickerFiresAdditionalSweeps(t *testing.T) {
	pruner := &fakePruner{}
	hk, _ := New(Config{
		Librarian:    pruner,
		TickInterval: 25 * time.Millisecond,
		Logger:       discardLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hk.Run(ctx)

	// Wait for at least 3 sweeps (1 immediate + 2 ticks).
	deadline := time.After(2 * time.Second)
	for {
		if calls := pruner.snapshot(); len(calls) >= 3 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("got only %d sweeps within deadline", len(pruner.snapshot()))
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestRun_ContinuesAfterSweepFailure(t *testing.T) {
	// First call errors, second call succeeds. The housekeeper must
	// keep running across the failure, not exit.
	calls := atomic.Int32{}
	pruner := errInjectingPruner{
		count: &calls,
		errOn: 1, // error on the first call only
	}
	hk, _ := New(Config{
		Librarian:    pruner,
		TickInterval: 20 * time.Millisecond,
		Logger:       discardLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hk.Run(ctx)

	deadline := time.After(2 * time.Second)
	for {
		if calls.Load() >= 3 {
			// At least three sweeps fired despite the first error.
			return
		}
		select {
		case <-deadline:
			t.Fatalf("only %d sweeps, expected >=3 after error recovery", calls.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// errInjectingPruner errors on the Nth call (1-indexed) and succeeds
// otherwise. Used to verify the run loop survives transient errors.
type errInjectingPruner struct {
	count *atomic.Int32
	errOn int32
}

func (e errInjectingPruner) PruneNarrativeIndex(time.Time) (int64, error) {
	n := e.count.Add(1)
	if n == e.errOn {
		return 0, errors.New("transient")
	}
	return 0, nil
}

func (errInjectingPruner) AllCases() []cbr.Case                          { return nil }
func (errInjectingPruner) SupersedeCase(string, string) (cbr.Case, error) { return cbr.Case{}, nil }
func (errInjectingPruner) SuppressCase(string) (cbr.Case, error)          { return cbr.Case{}, nil }

func TestRun_StopsCleanlyOnContextCancel(t *testing.T) {
	hk, _ := New(Config{
		Librarian:    &fakePruner{},
		TickInterval: time.Hour,
		Logger:       discardLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- hk.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run didn't return after cancel")
	}
}

// ---- error type ----

func TestErrLibrarianRequired_Message(t *testing.T) {
	if !strings.Contains(errLibrarianRequired.Error(), "Librarian is required") {
		t.Errorf("err message = %q", errLibrarianRequired.Error())
	}
}

func TestNew_ReturnsErrLibrarianRequiredSentinel(t *testing.T) {
	_, err := New(Config{})
	if !errors.Is(err, errLibrarianRequired) {
		t.Errorf("err = %v, want errLibrarianRequired sentinel", err)
	}
}

// ---- CBR sweep ----

// makeCBRCase builds a Case with the typical fields the dedup +
// prune helpers care about. Defaults to a non-prunable, non-
// duplicate "happy" case; tests override per scenario.
func makeCBRCase(id string, mods ...func(*cbr.Case)) cbr.Case {
	c := cbr.Case{
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
		Outcome:  cbr.Outcome{Status: cbr.StatusSuccess, Confidence: 0.7},
	}
	for _, m := range mods {
		m(&c)
	}
	return c
}

func TestCBRSweep_DisabledByDefault(t *testing.T) {
	pruner := &fakePruner{
		cases: []cbr.Case{
			makeCBRCase("a"),
			makeCBRCase("b"), // identical content -> would be a dedup hit if enabled
		},
	}
	hk, _ := New(Config{
		Librarian: pruner,
		NowFn:     func() time.Time { return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) },
		Logger:    discardLogger(),
	})
	hk.sweep()
	if got := pruner.supersedeSnapshot(); len(got) != 0 {
		t.Errorf("CBR sweep ran when CBRSweepEnabled=false: %+v", got)
	}
}

func TestCBRSweep_DedupSupersedes(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	pruner := &fakePruner{
		cases: []cbr.Case{
			makeCBRCase("a", func(c *cbr.Case) { c.Outcome.Confidence = 0.9 }),
			makeCBRCase("b", func(c *cbr.Case) { c.Outcome.Confidence = 0.5 }),
			makeCBRCase("c", func(c *cbr.Case) {
				c.Problem.Intent = "totally unrelated thing"
				c.Problem.IntentClass = cbr.IntentSystemCommand
				c.Problem.Domain = "infra"
				c.Problem.Keywords = []string{"shutdown"}
				c.Problem.Entities = []string{"server"}
				c.Solution.Approach = "ssh and run shutdown"
			}),
		},
	}
	hk, _ := New(Config{
		Librarian:       pruner,
		NowFn:           func() time.Time { return now },
		Logger:          discardLogger(),
		CBRSweepEnabled: true,
	})
	hk.sweep()
	got := pruner.supersedeSnapshot()
	if len(got) != 1 {
		t.Fatalf("got %d supersede call(s), want 1: %+v", len(got), got)
	}
	if got[0].id != "b" || got[0].byID != "a" {
		t.Errorf("supersede call = %+v, want id=b byID=a (b is lower-confidence)", got[0])
	}
	if hk.state.LastCBRSupersededCount != 1 {
		t.Errorf("LastCBRSupersededCount = %d, want 1", hk.state.LastCBRSupersededCount)
	}
	if hk.state.CumulativeSuperseded != 1 {
		t.Errorf("CumulativeSuperseded = %d, want 1", hk.state.CumulativeSuperseded)
	}
}

func TestCBRSweep_PruneSuppressesOldFailureWithoutLesson(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) // 75d after fixture
	pruner := &fakePruner{
		cases: []cbr.Case{
			makeCBRCase("a", func(c *cbr.Case) {
				c.Outcome.Status = cbr.StatusFailure
				c.Category = cbr.CategoryPitfall
				c.Timestamp = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
			}),
			makeCBRCase("rescued", func(c *cbr.Case) {
				c.Outcome.Status = cbr.StatusFailure
				c.Outcome.Pitfalls = []string{"learned this"}
				c.Category = cbr.CategoryPitfall
				c.Timestamp = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
				// Make it not match 'a' for dedup so it's not coincidentally
				// superseded.
				c.Problem.Intent = "completely different rescue topic"
				c.Problem.IntentClass = cbr.IntentSystemCommand
				c.Problem.Domain = "rescue"
				c.Problem.Keywords = []string{"rescued", "kept"}
				c.Problem.Entities = []string{"keeper"}
				c.Solution.Approach = "rescue path approach"
			}),
		},
	}
	hk, _ := New(Config{
		Librarian:       pruner,
		NowFn:           func() time.Time { return now },
		Logger:          discardLogger(),
		CBRSweepEnabled: true,
	})
	hk.sweep()
	got := pruner.suppressSnapshot()
	if len(got) != 1 || got[0] != "a" {
		t.Errorf("suppress calls = %v, want [a]", got)
	}
	if hk.state.LastCBRSuppressedCount != 1 {
		t.Errorf("LastCBRSuppressedCount = %d, want 1", hk.state.LastCBRSuppressedCount)
	}
}

func TestCBRSweep_DedupErrorDoesNotKillSweep(t *testing.T) {
	pruner := &fakePruner{
		cases: []cbr.Case{
			makeCBRCase("a"),
			makeCBRCase("b"),
		},
		supersedeErr: errors.New("supersede boom"),
	}
	hk, _ := New(Config{
		Librarian:       pruner,
		NowFn:           time.Now,
		Logger:          discardLogger(),
		CBRSweepEnabled: true,
	})
	hk.sweep() // must not panic
	if hk.state.LastCBRSupersededCount != 0 {
		t.Errorf("count = %d, want 0 (all calls errored)", hk.state.LastCBRSupersededCount)
	}
}

func TestCBRSweep_PruneErrorDoesNotKillSweep(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	pruner := &fakePruner{
		cases: []cbr.Case{
			makeCBRCase("a", func(c *cbr.Case) {
				c.Outcome.Status = cbr.StatusFailure
				c.Category = cbr.CategoryPitfall
				c.Timestamp = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
			}),
		},
		suppressErr: errors.New("suppress boom"),
	}
	hk, _ := New(Config{
		Librarian:       pruner,
		NowFn:           func() time.Time { return now },
		Logger:          discardLogger(),
		CBRSweepEnabled: true,
	})
	hk.sweep() // must not panic
	if hk.state.LastCBRSuppressedCount != 0 {
		t.Errorf("count = %d, want 0 (all calls errored)", hk.state.LastCBRSuppressedCount)
	}
}

func TestCBRSweep_EmptyCorpusZeroes(t *testing.T) {
	pruner := &fakePruner{}
	hk, _ := New(Config{
		Librarian:       pruner,
		NowFn:           time.Now,
		Logger:          discardLogger(),
		CBRSweepEnabled: true,
	})
	hk.sweep()
	if hk.state.LastCBRSupersededCount != 0 || hk.state.LastCBRSuppressedCount != 0 {
		t.Errorf("non-zero counters on empty corpus: superseded=%d suppressed=%d",
			hk.state.LastCBRSupersededCount, hk.state.LastCBRSuppressedCount)
	}
}

func TestNew_AppliesCBRDefaults(t *testing.T) {
	hk, err := New(Config{Librarian: &fakePruner{}})
	if err != nil {
		t.Fatal(err)
	}
	if hk.cfg.CBRDedupThreshold != cbr.DefaultDedupThreshold {
		t.Errorf("CBRDedupThreshold default = %v, want %v",
			hk.cfg.CBRDedupThreshold, cbr.DefaultDedupThreshold)
	}
	if hk.cfg.CBRPruneMaxFailureAge != cbr.DefaultPruneMaxFailureAge {
		t.Errorf("CBRPruneMaxFailureAge default = %v, want %v",
			hk.cfg.CBRPruneMaxFailureAge, cbr.DefaultPruneMaxFailureAge)
	}
}

