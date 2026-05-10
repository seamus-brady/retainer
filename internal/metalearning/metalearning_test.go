package metalearning

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fixedClock returns a NowFn that always reports `t`. Used to make
// "due" decisions deterministic in tests.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func TestNew_RequiresWorkers(t *testing.T) {
	_, err := New(Config{DataDir: t.TempDir()})
	if err == nil {
		t.Fatal("expected error when no workers")
	}
}

func TestNew_RequiresDataDir(t *testing.T) {
	_, err := New(Config{Workers: []Worker{{Name: "x", Interval: time.Second, Run: noopRun}}})
	if err == nil {
		t.Fatal("expected error when DataDir empty")
	}
}

func TestNew_RejectsEmptyWorkerName(t *testing.T) {
	_, err := New(Config{
		DataDir: t.TempDir(),
		Workers: []Worker{{Name: "", Interval: time.Second, Run: noopRun}},
	})
	if err == nil {
		t.Fatal("expected error on empty worker name")
	}
}

func TestNew_RejectsZeroInterval(t *testing.T) {
	_, err := New(Config{
		DataDir: t.TempDir(),
		Workers: []Worker{{Name: "x", Interval: 0, Run: noopRun}},
	})
	if err == nil {
		t.Fatal("expected error on zero interval")
	}
}

func TestNew_RejectsNilRun(t *testing.T) {
	_, err := New(Config{
		DataDir: t.TempDir(),
		Workers: []Worker{{Name: "x", Interval: time.Second}},
	})
	if err == nil {
		t.Fatal("expected error on nil Run")
	}
}

func TestNew_RejectsDuplicateNames(t *testing.T) {
	_, err := New(Config{
		DataDir: t.TempDir(),
		Workers: []Worker{
			{Name: "dup", Interval: time.Second, Run: noopRun},
			{Name: "dup", Interval: time.Second, Run: noopRun},
		},
	})
	if err == nil {
		t.Fatal("expected error on duplicate name")
	}
}

func TestCheckAll_FreshStartRunsWorker(t *testing.T) {
	dir := t.TempDir()
	var calls int32
	w := Worker{
		Name:     "first",
		Interval: time.Hour,
		Run: func(_ context.Context, _ Deps) error {
			atomic.AddInt32(&calls, 1)
			return nil
		},
	}
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	p, err := New(Config{
		Workers: []Worker{w},
		DataDir: dir,
		NowFn:   fixedClock(now),
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	p.checkAll(context.Background())
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
}

func TestCheckAll_RespectsInterval(t *testing.T) {
	dir := t.TempDir()
	var calls int32
	w := Worker{
		Name:     "spaced",
		Interval: time.Hour,
		Run: func(_ context.Context, _ Deps) error {
			atomic.AddInt32(&calls, 1)
			return nil
		},
	}
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	clock := &mutableClock{t: now}
	p, err := New(Config{
		Workers: []Worker{w},
		DataDir: dir,
		NowFn:   clock.now,
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// First check: cold start, runs.
	p.checkAll(context.Background())
	// Second check 30 minutes later: not due, should NOT run.
	clock.t = now.Add(30 * time.Minute)
	p.checkAll(context.Background())
	// Third check 90 minutes after start: due, runs.
	clock.t = now.Add(90 * time.Minute)
	p.checkAll(context.Background())

	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("calls = %d, want 2 (initial + after-interval)", got)
	}
}

func TestCheckAll_ErrorsDoNotBlockSubsequentWorkers(t *testing.T) {
	dir := t.TempDir()
	var goodCalls int32
	bad := Worker{
		Name:     "bad",
		Interval: time.Hour,
		Run: func(_ context.Context, _ Deps) error {
			return errors.New("boom")
		},
	}
	good := Worker{
		Name:     "good",
		Interval: time.Hour,
		Run: func(_ context.Context, _ Deps) error {
			atomic.AddInt32(&goodCalls, 1)
			return nil
		},
	}
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	p, err := New(Config{
		Workers: []Worker{bad, good},
		DataDir: dir,
		NowFn:   fixedClock(now),
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	p.checkAll(context.Background())
	if got := atomic.LoadInt32(&goodCalls); got != 1 {
		t.Errorf("good worker should have run despite bad worker failure; got %d", got)
	}
	if p.state.Workers["bad"].ConsecutiveFailures != 1 {
		t.Errorf("bad worker consecutive_failures = %d, want 1",
			p.state.Workers["bad"].ConsecutiveFailures)
	}
	if p.state.Workers["bad"].LastFailureMessage != "boom" {
		t.Errorf("bad worker last_failure_message = %q, want %q",
			p.state.Workers["bad"].LastFailureMessage, "boom")
	}
}

func TestCheckAll_PanicCaughtAsFailure(t *testing.T) {
	dir := t.TempDir()
	w := Worker{
		Name:     "panicky",
		Interval: time.Hour,
		Run: func(_ context.Context, _ Deps) error {
			panic("oh no")
		},
	}
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	p, err := New(Config{
		Workers: []Worker{w},
		DataDir: dir,
		NowFn:   fixedClock(now),
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Should not panic.
	p.checkAll(context.Background())
	if p.state.Workers["panicky"].ConsecutiveFailures != 1 {
		t.Errorf("panic should have been recorded as failure")
	}
}

func TestCheckAll_StatePersistsAndRestores(t *testing.T) {
	dir := t.TempDir()
	w := Worker{
		Name:     "stateful",
		Interval: time.Hour,
		Run:      noopRun,
	}
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	p1, err := New(Config{
		Workers: []Worker{w},
		DataDir: dir,
		NowFn:   fixedClock(now),
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	p1.checkAll(context.Background())

	// New pool reading the same data dir should see the recorded
	// last_run_at and skip the worker on a fresh check at the
	// same time.
	p2, err := New(Config{
		Workers: []Worker{w},
		DataDir: dir,
		NowFn:   fixedClock(now),
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if p2.state.Workers["stateful"].LastRunAt.IsZero() {
		t.Fatal("state did not restore from disk")
	}
}

func TestRun_StopsCleanlyOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	w := Worker{Name: "x", Interval: time.Hour, Run: noopRun}
	p, err := New(Config{
		Workers:      []Worker{w},
		DataDir:      dir,
		TickInterval: 50 * time.Millisecond,
		NowFn:        time.Now,
		Logger:       discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	time.Sleep(80 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

func TestStateFile_LandsAtExpectedPath(t *testing.T) {
	dir := t.TempDir()
	w := Worker{Name: "x", Interval: time.Hour, Run: noopRun}
	p, err := New(Config{
		Workers: []Worker{w},
		DataDir: dir,
		NowFn:   time.Now,
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, ".metalearning-state.json")
	if got := p.stateFile(); got != want {
		t.Errorf("state file = %q, want %q", got, want)
	}
}

func noopRun(_ context.Context, _ Deps) error { return nil }

// mutableClock is a tiny test helper for moving time forward across
// successive checks without rebuilding the pool.
type mutableClock struct {
	t time.Time
}

func (m *mutableClock) now() time.Time { return m.t }
