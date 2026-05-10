package agenttokens

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTracker(t *testing.T) (*Tracker, string) {
	t.Helper()
	dir := t.TempDir()
	return NewTracker(dir), dir
}

func TestAdd_FirstWriteCreatesFile(t *testing.T) {
	tr, dir := newTracker(t)
	stats, err := tr.Add("researcher", 100, 50)
	if err != nil {
		t.Fatal(err)
	}
	if stats.LifetimeInput != 100 || stats.LifetimeOutput != 50 {
		t.Errorf("lifetime = %d/%d, want 100/50", stats.LifetimeInput, stats.LifetimeOutput)
	}
	if stats.TodayInput != 100 || stats.TodayOutput != 50 {
		t.Errorf("today = %d/%d, want 100/50", stats.TodayInput, stats.TodayOutput)
	}
	if stats.DispatchCount != 1 {
		t.Errorf("dispatch_count = %d, want 1", stats.DispatchCount)
	}
	path := filepath.Join(dir, ".agent-researcher-tokens.json")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("state file not created: %v", err)
	}
}

func TestAdd_AccumulatesAcrossCalls(t *testing.T) {
	tr, _ := newTracker(t)
	_, _ = tr.Add("researcher", 100, 50)
	stats, _ := tr.Add("researcher", 30, 20)
	if stats.LifetimeInput != 130 || stats.LifetimeOutput != 70 {
		t.Errorf("lifetime = %d/%d, want 130/70", stats.LifetimeInput, stats.LifetimeOutput)
	}
	if stats.LastDispatchInput != 30 || stats.LastDispatchOutput != 20 {
		t.Errorf("last_dispatch = %d/%d, want 30/20", stats.LastDispatchInput, stats.LastDispatchOutput)
	}
	if stats.DispatchCount != 2 {
		t.Errorf("dispatch_count = %d, want 2", stats.DispatchCount)
	}
}

func TestAdd_PerAgentIsolated(t *testing.T) {
	tr, _ := newTracker(t)
	_, _ = tr.Add("researcher", 100, 50)
	_, _ = tr.Add("observer", 200, 80)
	r := tr.Stats("researcher")
	o := tr.Stats("observer")
	if r.LifetimeInput != 100 || o.LifetimeInput != 200 {
		t.Errorf("agents not isolated: r=%d o=%d", r.LifetimeInput, o.LifetimeInput)
	}
}

func TestAdd_TodayResetsOnDayChange(t *testing.T) {
	tr, _ := newTracker(t)
	// Pin the clock to a specific day for the first add.
	first := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	tr.nowFn = func() time.Time { return first }
	_, _ = tr.Add("researcher", 100, 50)

	// Advance the clock to the next day.
	next := first.Add(24 * time.Hour)
	tr.nowFn = func() time.Time { return next }
	stats, _ := tr.Add("researcher", 30, 20)

	if stats.TodayInput != 30 || stats.TodayOutput != 20 {
		t.Errorf("today should have reset; got %d/%d, want 30/20", stats.TodayInput, stats.TodayOutput)
	}
	if stats.LifetimeInput != 130 || stats.LifetimeOutput != 70 {
		t.Errorf("lifetime should NOT have reset; got %d/%d, want 130/70", stats.LifetimeInput, stats.LifetimeOutput)
	}
}

func TestAdd_RejectsEmptyName(t *testing.T) {
	tr, _ := newTracker(t)
	if _, err := tr.Add("", 1, 1); err == nil {
		t.Error("empty agent name should fail")
	}
}

func TestStats_FreshAgentReturnsZero(t *testing.T) {
	tr, _ := newTracker(t)
	stats := tr.Stats("never-touched")
	if stats.LifetimeInput != 0 || stats.DispatchCount != 0 {
		t.Errorf("fresh agent should be zero, got %+v", stats)
	}
}

func TestStats_SurvivesProcessRestart(t *testing.T) {
	dir := t.TempDir()
	tr1 := NewTracker(dir)
	_, _ = tr1.Add("researcher", 100, 50)

	// Simulate a fresh process with a new tracker on the same dir.
	tr2 := NewTracker(dir)
	stats := tr2.Stats("researcher")
	if stats.LifetimeInput != 100 || stats.LifetimeOutput != 50 {
		t.Errorf("after restart, lifetime = %d/%d, want 100/50", stats.LifetimeInput, stats.LifetimeOutput)
	}
	if stats.DispatchCount != 1 {
		t.Errorf("after restart, dispatch_count = %d, want 1", stats.DispatchCount)
	}
}

func TestStats_AfterRestart_AddContinuesAccumulation(t *testing.T) {
	dir := t.TempDir()
	tr1 := NewTracker(dir)
	_, _ = tr1.Add("researcher", 100, 50)

	tr2 := NewTracker(dir)
	stats, _ := tr2.Add("researcher", 30, 20)
	if stats.LifetimeInput != 130 || stats.LifetimeOutput != 70 {
		t.Errorf("post-restart Add didn't accumulate; got %d/%d, want 130/70", stats.LifetimeInput, stats.LifetimeOutput)
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"researcher":     "researcher",
		"observer":       "observer",
		"my-agent":       "my-agent",
		"my_agent":       "my_agent",
		"Path/With/Slash": "path_with_slash",
		"123":            "123",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPath_UsesSanitizedName(t *testing.T) {
	tr := NewTracker("/data")
	if got := tr.path("Researcher"); got != "/data/.agent-researcher-tokens.json" {
		t.Errorf("path = %q", got)
	}
}

func TestStateFile_HasReadableJSONShape(t *testing.T) {
	// Operators should be able to `cat` the file and read it.
	// Pin the JSON keys.
	tr, dir := newTracker(t)
	_, _ = tr.Add("researcher", 100, 50)
	body, err := os.ReadFile(filepath.Join(dir, ".agent-researcher-tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"schema_version", "lifetime_input", "lifetime_output", "today_input", "today_output", "today_date", "dispatch_count", "last_seen"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing JSON key %q in state file", k)
		}
	}
}

func TestAdd_ConcurrentSafety(t *testing.T) {
	// Race-detector smoke test: many goroutines add to the
	// same agent. The mutex serialises; final lifetime must
	// equal sum of contributions.
	tr, _ := newTracker(t)
	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _ = tr.Add("researcher", 1, 1)
		}()
	}
	wg.Wait()
	stats := tr.Stats("researcher")
	if stats.LifetimeInput != n || stats.LifetimeOutput != n {
		t.Errorf("under concurrency, lifetime = %d/%d, want %d/%d", stats.LifetimeInput, stats.LifetimeOutput, n, n)
	}
	if stats.DispatchCount != n {
		t.Errorf("dispatch_count = %d, want %d", stats.DispatchCount, n)
	}
}
