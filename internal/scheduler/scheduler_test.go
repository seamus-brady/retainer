package scheduler

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seamus-brady/retainer/internal/policy"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeSubmitter records every SubmitWithSource call so tests can
// assert what the scheduler dispatched.
type fakeSubmitter struct {
	mu    sync.Mutex
	calls []fakeSubmit
}

type fakeSubmit struct {
	text   string
	source policy.Source
}

func (f *fakeSubmitter) SubmitWithSource(_ context.Context, text string, source policy.Source) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeSubmit{text: text, source: source})
}

func (f *fakeSubmitter) snapshot() []fakeSubmit {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeSubmit, len(f.calls))
	copy(out, f.calls)
	return out
}

// fixedClock returns a NowFn that always reports `t`.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func TestNew_RequiresDataDir(t *testing.T) {
	if _, err := New(Config{Submitter: &fakeSubmitter{}}); err == nil {
		t.Error("expected error when DataDir empty")
	}
}

func TestScheduleOneShot_RejectsPastFireAt(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	s, err := New(Config{
		DataDir:   t.TempDir(),
		Submitter: &fakeSubmitter{},
		NowFn:     fixedClock(now),
		Logger:    discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ScheduleOneShot("past", now.Add(-time.Hour), "p", ""); err == nil {
		t.Error("expected error for fire_at in the past")
	}
}

func TestScheduleRecurring_RejectsBadCron(t *testing.T) {
	s, err := New(Config{
		DataDir:   t.TempDir(),
		Submitter: &fakeSubmitter{},
		Logger:    discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ScheduleRecurring("bad", "not a cron", "p", ""); err == nil {
		t.Error("expected error for malformed cron")
	}
}

func TestScheduleAndList_Roundtrips(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	s, err := New(Config{
		DataDir:   t.TempDir(),
		Submitter: &fakeSubmitter{},
		NowFn:     fixedClock(now),
		Logger:    discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ScheduleRecurring("daily", "0 9 * * *", "morning standup", "9am every day"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ScheduleOneShot("once", now.Add(2*time.Hour), "buy milk", ""); err != nil {
		t.Fatal(err)
	}
	jobs := s.List()
	if len(jobs) != 2 {
		t.Fatalf("got %d active jobs, want 2", len(jobs))
	}
}

func TestList_OrdersByNextFireAscending(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	s, err := New(Config{
		DataDir:   t.TempDir(),
		Submitter: &fakeSubmitter{},
		NowFn:     fixedClock(now),
		Logger:    discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Far first, near second — List should reorder near→far.
	_, _ = s.ScheduleOneShot("far", now.Add(24*time.Hour), "p", "")
	_, _ = s.ScheduleOneShot("near", now.Add(time.Hour), "p", "")
	jobs := s.List()
	if len(jobs) != 2 || jobs[0].Name != "near" || jobs[1].Name != "far" {
		t.Errorf("expected near before far, got %+v", jobs)
	}
}

func TestCancel_DeactivatesJob(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	s, err := New(Config{
		DataDir:   t.TempDir(),
		Submitter: &fakeSubmitter{},
		NowFn:     fixedClock(now),
		Logger:    discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	job, _ := s.ScheduleOneShot("doomed", now.Add(time.Hour), "p", "")
	if err := s.Cancel(job.ID, "operator changed mind"); err != nil {
		t.Fatal(err)
	}
	if jobs := s.List(); len(jobs) != 0 {
		t.Errorf("cancelled job still in active list: %+v", jobs)
	}
	got, ok := s.Inspect(job.ID)
	if !ok {
		t.Fatal("Inspect should still find a cancelled job")
	}
	if got.Active {
		t.Error("expected Active=false after cancellation")
	}
}

func TestCancel_RejectsUnknownAndAlreadyCancelled(t *testing.T) {
	s, err := New(Config{
		DataDir:   t.TempDir(),
		Submitter: &fakeSubmitter{},
		Logger:    discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel("nope", ""); err == nil {
		t.Error("unknown id should error")
	}
	job, _ := s.ScheduleOneShot("once", time.Now().Add(time.Hour), "p", "")
	if err := s.Cancel(job.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(job.ID, ""); err == nil {
		t.Error("double-cancel should error")
	}
}

func TestTick_FiresDueOneShotJob(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	sub := &fakeSubmitter{}
	s, err := New(Config{
		DataDir:   t.TempDir(),
		Submitter: sub,
		NowFn:     fixedClock(now),
		Logger:    discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Schedule for 1 second in the future, then advance the clock
	// past the fire time + tick.
	job, _ := s.ScheduleOneShot("fire", now.Add(time.Second), "ping", "")
	s.cfg.NowFn = fixedClock(now.Add(time.Minute))
	s.tick(context.Background())
	// Submit is async (goroutine inside fire) — give it time.
	if !waitFor(func() bool { return len(sub.snapshot()) >= 1 }, time.Second) {
		t.Fatal("submit never fired")
	}
	calls := sub.snapshot()
	// The submitted text is the framed prompt — header that tells
	// the model "this is an independent autonomous fire", then the
	// original prompt verbatim. Assert both pieces are present
	// rather than re-typing the exact framing string (which would
	// re-test the helper, not the wiring).
	if !strings.Contains(calls[0].text, "Scheduled job") {
		t.Errorf("submit text missing scheduler framing header: %q", calls[0].text)
	}
	if !strings.Contains(calls[0].text, "ping") {
		t.Errorf("submit text missing original prompt: %q", calls[0].text)
	}
	if calls[0].source != policy.SourceAutonomous {
		t.Errorf("submit source = %d, want autonomous", calls[0].source)
	}
	got, _ := s.Inspect(job.ID)
	if got.Active {
		t.Error("one-shot should be inactive after firing")
	}
	if got.FiredCount != 1 {
		t.Errorf("FiredCount = %d, want 1", got.FiredCount)
	}
}

func TestTick_RecurringJobFiresMultipleTimes(t *testing.T) {
	now := time.Date(2026, 5, 8, 8, 59, 0, 0, time.UTC)
	sub := &fakeSubmitter{}
	clock := &mutableClock{t: now}
	s, err := New(Config{
		DataDir:   t.TempDir(),
		Submitter: sub,
		NowFn:     clock.now,
		Logger:    discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Every minute.
	if _, err := s.ScheduleRecurring("ping", "*/1 * * * *", "ping", ""); err != nil {
		t.Fatal(err)
	}
	// Advance two minutes; expect two fires.
	clock.t = now.Add(2*time.Minute + 30*time.Second)
	s.tick(context.Background())
	if !waitFor(func() bool { return len(sub.snapshot()) >= 1 }, time.Second) {
		t.Fatal("first fire never landed")
	}
	clock.t = now.Add(5 * time.Minute)
	s.tick(context.Background())
	if !waitFor(func() bool { return len(sub.snapshot()) >= 2 }, time.Second) {
		t.Fatalf("second fire never landed; got %d", len(sub.snapshot()))
	}
}

func TestPersist_SurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	s1, err := New(Config{
		DataDir:   dir,
		Submitter: &fakeSubmitter{},
		NowFn:     fixedClock(now),
		Logger:    discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	job, _ := s1.ScheduleRecurring("survives", "0 9 * * *", "morning", "")
	s2, err := New(Config{
		DataDir:   dir,
		Submitter: &fakeSubmitter{},
		NowFn:     fixedClock(now),
		Logger:    discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s2.Inspect(job.ID)
	if !ok {
		t.Fatal("job lost across restart")
	}
	if got.Cron != "0 9 * * *" || got.Name != "survives" {
		t.Errorf("restored job mismatch: %+v", got)
	}
}

func TestSetSubmitter_BindsLateAndSubmitsAfter(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	s, err := New(Config{
		DataDir: t.TempDir(),
		NowFn:   fixedClock(now),
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// No submitter yet — fire should be a logged no-op, not a panic.
	_, _ = s.ScheduleOneShot("orphan", now.Add(time.Second), "ping", "")
	s.cfg.NowFn = fixedClock(now.Add(time.Minute))
	s.tick(context.Background())
	// Now bind + verify a fresh job fires.
	sub := &fakeSubmitter{}
	s.SetSubmitter(sub)
	_, _ = s.ScheduleOneShot("real", now.Add(2*time.Minute), "ping2", "")
	s.cfg.NowFn = fixedClock(now.Add(3 * time.Minute))
	s.tick(context.Background())
	if !waitFor(func() bool { return len(sub.snapshot()) >= 1 }, time.Second) {
		t.Fatal("post-bind submit never fired")
	}
}

// mutableClock is a tiny test helper for moving time forward
// across successive checks without rebuilding the service.
type mutableClock struct {
	t time.Time
}

func (m *mutableClock) now() time.Time { return m.t }

// waitFor polls cond until it returns true or timeout elapses.
// Used to bridge the async fire goroutine in tick → snapshot
// assertions.
func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// Unused-warning silencer for atomic if a future test relies on
// concurrent fire counting; keep here so we don't grow + drop the
// import on each test addition.
var _ = atomic.AddInt32
