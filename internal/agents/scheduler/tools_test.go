package scheduler

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/seamus-brady/retainer/internal/policy"
	schedsvc "github.com/seamus-brady/retainer/internal/scheduler"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// noSubmit is a Submitter that drops everything — the agent-tool
// tests exercise the agent's API, not actual cycle dispatch.
type noSubmit struct{}

func (noSubmit) SubmitWithSource(_ context.Context, _ string, _ policy.Source) {}

func newSvc(t *testing.T, now time.Time) *schedsvc.Service {
	t.Helper()
	s, err := schedsvc.New(schedsvc.Config{
		DataDir:   t.TempDir(),
		Submitter: noSubmit{},
		NowFn:     func() time.Time { return now },
		Logger:    discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestScheduleJob_RecurringHappyPath(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	s := newSvc(t, now)
	h := ScheduleJob{Svc: s}
	out, err := h.Execute(context.Background(),
		[]byte(`{"name":"morning standup","prompt":"summarise overnight cycles","cron":"0 9 * * *"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "morning standup") {
		t.Errorf("output missing name: %q", out)
	}
	if !strings.Contains(out, "0 9 * * *") {
		t.Errorf("output missing cron: %q", out)
	}
}

func TestScheduleJob_OneShotHappyPath(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	s := newSvc(t, now)
	h := ScheduleJob{Svc: s}
	fireAt := now.Add(2 * time.Hour).Format(time.RFC3339)
	body := `{"name":"buy milk reminder","prompt":"remind me to buy milk","fire_at":"` + fireAt + `"}`
	out, err := h.Execute(context.Background(), []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "buy milk reminder") {
		t.Errorf("output missing name: %q", out)
	}
}

func TestScheduleJob_RejectsBothCronAndFireAt(t *testing.T) {
	s := newSvc(t, time.Now())
	h := ScheduleJob{Svc: s}
	body := `{"name":"x","prompt":"x","cron":"* * * * *","fire_at":"2030-01-01T00:00:00Z"}`
	if _, err := h.Execute(context.Background(), []byte(body)); err == nil {
		t.Error("expected error when both cron and fire_at are set")
	}
}

func TestScheduleJob_RejectsNeither(t *testing.T) {
	s := newSvc(t, time.Now())
	h := ScheduleJob{Svc: s}
	body := `{"name":"x","prompt":"x"}`
	if _, err := h.Execute(context.Background(), []byte(body)); err == nil {
		t.Error("expected error when neither cron nor fire_at is set")
	}
}

func TestScheduleJob_RejectsBadCron(t *testing.T) {
	s := newSvc(t, time.Now())
	h := ScheduleJob{Svc: s}
	body := `{"name":"x","prompt":"x","cron":"not a cron"}`
	if _, err := h.Execute(context.Background(), []byte(body)); err == nil {
		t.Error("expected error for malformed cron")
	}
}

func TestScheduleJob_RejectsBadFireAt(t *testing.T) {
	s := newSvc(t, time.Now())
	h := ScheduleJob{Svc: s}
	body := `{"name":"x","prompt":"x","fire_at":"yesterday"}`
	if _, err := h.Execute(context.Background(), []byte(body)); err == nil {
		t.Error("expected error for non-RFC3339 fire_at")
	}
}

func TestListJobs_EmptyMessage(t *testing.T) {
	s := newSvc(t, time.Now())
	h := ListJobs{Svc: s}
	out, err := h.Execute(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no active jobs") {
		t.Errorf("expected empty-list message; got %q", out)
	}
}

func TestListJobs_RendersScheduledJobs(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	s := newSvc(t, now)
	if _, err := s.ScheduleRecurring("daily", "0 9 * * *", "morning", "9am"); err != nil {
		t.Fatal(err)
	}
	out, err := (ListJobs{Svc: s}).Execute(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"daily", "0 9 * * *", "morning"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q in:\n%s", want, out)
		}
	}
}

func TestInspectJob_RejectsMissingID(t *testing.T) {
	s := newSvc(t, time.Now())
	h := InspectJob{Svc: s}
	if _, err := h.Execute(context.Background(), []byte(`{}`)); err == nil {
		t.Error("expected error when job_id missing")
	}
}

func TestInspectJob_NotFound(t *testing.T) {
	s := newSvc(t, time.Now())
	h := InspectJob{Svc: s}
	if _, err := h.Execute(context.Background(), []byte(`{"job_id":"nope"}`)); err == nil {
		t.Error("expected error for unknown id")
	}
}

func TestInspectJob_RendersFullRecord(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	s := newSvc(t, now)
	job, _ := s.ScheduleOneShot("once", now.Add(time.Hour), "ping", "details")
	out, err := (InspectJob{Svc: s}).Execute(context.Background(),
		[]byte(`{"job_id":"`+job.ID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"once", "one_shot", "ping", "active:       true"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestCancelJob_HappyPath(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	s := newSvc(t, now)
	job, _ := s.ScheduleOneShot("doomed", now.Add(time.Hour), "p", "")
	out, err := (CancelJob{Svc: s}).Execute(context.Background(),
		[]byte(`{"job_id":"`+job.ID+`","reason":"changed mind"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, job.ID) {
		t.Errorf("output should mention job id: %q", out)
	}
	got, _ := s.Inspect(job.ID)
	if got.Active {
		t.Error("expected Active=false after cancel")
	}
}

func TestCancelJob_RejectsEmptyID(t *testing.T) {
	s := newSvc(t, time.Now())
	h := CancelJob{Svc: s}
	if _, err := h.Execute(context.Background(), []byte(`{"job_id":""}`)); err == nil {
		t.Error("empty job_id should error")
	}
}

func TestBuildTools_RegistersFour(t *testing.T) {
	s := newSvc(t, time.Now())
	r := BuildTools(s)
	want := map[string]bool{
		"schedule_job": true,
		"list_jobs":    true,
		"inspect_job":  true,
		"cancel_job":   true,
	}
	for _, n := range r.Names() {
		delete(want, n)
	}
	if len(want) != 0 {
		t.Errorf("missing tools: %+v", want)
	}
}
