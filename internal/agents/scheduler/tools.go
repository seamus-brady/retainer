package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/seamus-brady/retainer/internal/llm"
	schedsvc "github.com/seamus-brady/retainer/internal/scheduler"
	"github.com/seamus-brady/retainer/internal/tools"
)

// BuildTools returns a fresh tools.Registry pre-populated with
// the agent's four scheduler tools wired against the supplied
// service. Bootstrap calls this once and passes the registry
// into the agent's New.
func BuildTools(svc *schedsvc.Service) *tools.Registry {
	r := tools.NewRegistry()
	r.MustRegister(ScheduleJob{Svc: svc})
	r.MustRegister(ListJobs{Svc: svc})
	r.MustRegister(InspectJob{Svc: svc})
	r.MustRegister(CancelJob{Svc: svc})
	return r
}

// ---------------------------------------------------------------------------
// schedule_job
// ---------------------------------------------------------------------------

type scheduleJobInput struct {
	Name        string `json:"name"`
	Prompt      string `json:"prompt"`
	Cron        string `json:"cron,omitempty"`
	FireAt      string `json:"fire_at,omitempty"`
	Description string `json:"description,omitempty"`
}

// ScheduleJob registers a recurring or one-shot job. Recurring
// uses a cron expression; one-shot uses fire_at (RFC3339).
// Exactly one of cron / fire_at must be supplied.
type ScheduleJob struct{ Svc *schedsvc.Service }

func (ScheduleJob) Tool() llm.Tool {
	return llm.Tool{
		Name: "schedule_job",
		Description: "Register a new scheduled job. Exactly one of `cron` (recurring) or `fire_at` " +
			"(one-shot, RFC3339 timestamp) must be supplied. The job's prompt runs as a non-interactive " +
			"cycle when its time arrives. Returns the assigned job ID.",
		InputSchema: llm.Schema{
			Name: "schedule_job",
			Properties: map[string]llm.Property{
				"name": {
					Type:        "string",
					Description: "Short operator-readable label (e.g. \"morning standup\").",
				},
				"prompt": {
					Type:        "string",
					Description: "The text the cog will run when the job fires.",
				},
				"cron": {
					Type: "string",
					Description: "Five-field cron expression for recurring jobs. " +
						"Format: minute hour day-of-month month day-of-week. " +
						"Examples: \"0 9 * * MON\" (9am Mondays), \"*/15 * * * *\" (every 15min).",
				},
				"fire_at": {
					Type: "string",
					Description: "RFC3339 / ISO 8601 timestamp for one-shot jobs (e.g. " +
						"\"2026-05-09T18:30:00Z\"). Must be in the future.",
				},
				"description": {
					Type:        "string",
					Description: "Optional context the operator wants stored alongside the job.",
				},
			},
			Required: []string{"name", "prompt"},
		},
	}
}

func (h ScheduleJob) Execute(_ context.Context, input []byte) (string, error) {
	if h.Svc == nil {
		return "", fmt.Errorf("schedule_job: scheduler service not configured")
	}
	if len(input) == 0 {
		return "", fmt.Errorf("schedule_job: empty input")
	}
	var in scheduleJobInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("schedule_job: decode input: %w", err)
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Prompt = strings.TrimSpace(in.Prompt)
	in.Cron = strings.TrimSpace(in.Cron)
	in.FireAt = strings.TrimSpace(in.FireAt)

	hasCron := in.Cron != ""
	hasFireAt := in.FireAt != ""
	if hasCron == hasFireAt {
		return "", fmt.Errorf("schedule_job: supply exactly one of `cron` or `fire_at`")
	}

	if hasCron {
		j, err := h.Svc.ScheduleRecurring(in.Name, in.Cron, in.Prompt, in.Description)
		if err != nil {
			return "", fmt.Errorf("schedule_job: %w", err)
		}
		return fmt.Sprintf("scheduled recurring job %q (id=%s, cron=%q)",
			j.Name, j.ID, j.Cron), nil
	}

	t, err := time.Parse(time.RFC3339, in.FireAt)
	if err != nil {
		return "", fmt.Errorf("schedule_job: fire_at %q is not RFC3339: %w", in.FireAt, err)
	}
	j, err := h.Svc.ScheduleOneShot(in.Name, t, in.Prompt, in.Description)
	if err != nil {
		return "", fmt.Errorf("schedule_job: %w", err)
	}
	return fmt.Sprintf("scheduled one-shot job %q (id=%s, fire_at=%s)",
		j.Name, j.ID, j.FireAt.Format(time.RFC3339)), nil
}

// ---------------------------------------------------------------------------
// list_jobs
// ---------------------------------------------------------------------------

// ListJobs returns active jobs sorted by next-fire ascending.
type ListJobs struct{ Svc *schedsvc.Service }

func (ListJobs) Tool() llm.Tool {
	return llm.Tool{
		Name: "list_jobs",
		Description: "List active scheduled jobs sorted by next-fire ascending. Returns job IDs, names, " +
			"cron / fire-time, and the prompts each will run.",
		InputSchema: llm.Schema{
			Name:       "list_jobs",
			Properties: map[string]llm.Property{},
			Required:   []string{},
		},
	}
}

func (h ListJobs) Execute(_ context.Context, _ []byte) (string, error) {
	if h.Svc == nil {
		return "", fmt.Errorf("list_jobs: scheduler service not configured")
	}
	jobs := h.Svc.List()
	if len(jobs) == 0 {
		return "no active jobs", nil
	}
	now := time.Now()
	var b strings.Builder
	for i, j := range jobs {
		fmt.Fprintf(&b, "[%d] %s (id=%s)\n", i+1, j.Name, j.ID)
		if j.Kind == schedsvc.JobRecurring {
			fmt.Fprintf(&b, "    cron: %s\n", j.Cron)
		} else {
			fmt.Fprintf(&b, "    fire_at: %s\n", j.FireAt.Format(time.RFC3339))
		}
		fmt.Fprintf(&b, "    next: %s\n", h.Svc.NextFire(j, now).Format(time.RFC3339))
		fmt.Fprintf(&b, "    prompt: %s\n", truncateInline(j.Prompt, 200))
		if j.Description != "" {
			fmt.Fprintf(&b, "    description: %s\n", truncateInline(j.Description, 200))
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// ---------------------------------------------------------------------------
// inspect_job
// ---------------------------------------------------------------------------

type inspectJobInput struct {
	JobID string `json:"job_id"`
}

// InspectJob returns the full record for one job, including
// cancelled / completed history.
type InspectJob struct{ Svc *schedsvc.Service }

func (InspectJob) Tool() llm.Tool {
	return llm.Tool{
		Name: "inspect_job",
		Description: "Return the full record for one scheduled job by ID. Includes last-fired / next-fire " +
			"times, fired count, active state, and any cancellation reason.",
		InputSchema: llm.Schema{
			Name: "inspect_job",
			Properties: map[string]llm.Property{
				"job_id": {Type: "string", Description: "The job's UUID, returned by schedule_job or list_jobs."},
			},
			Required: []string{"job_id"},
		},
	}
}

func (h InspectJob) Execute(_ context.Context, input []byte) (string, error) {
	if h.Svc == nil {
		return "", fmt.Errorf("inspect_job: scheduler service not configured")
	}
	if len(input) == 0 {
		return "", fmt.Errorf("inspect_job: empty input")
	}
	var in inspectJobInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("inspect_job: decode input: %w", err)
	}
	id := strings.TrimSpace(in.JobID)
	if id == "" {
		return "", fmt.Errorf("inspect_job: job_id must not be empty")
	}
	j, ok := h.Svc.Inspect(id)
	if !ok {
		return "", fmt.Errorf("inspect_job: job %q not found", id)
	}
	now := time.Now()
	var b strings.Builder
	fmt.Fprintf(&b, "id:           %s\n", j.ID)
	fmt.Fprintf(&b, "name:         %s\n", j.Name)
	fmt.Fprintf(&b, "kind:         %s\n", j.Kind)
	if j.Kind == schedsvc.JobRecurring {
		fmt.Fprintf(&b, "cron:         %s\n", j.Cron)
	} else {
		fmt.Fprintf(&b, "fire_at:      %s\n", j.FireAt.Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "active:       %t\n", j.Active)
	fmt.Fprintf(&b, "created_at:   %s\n", j.CreatedAt.Format(time.RFC3339))
	if !j.LastFiredAt.IsZero() {
		fmt.Fprintf(&b, "last_fired:   %s\n", j.LastFiredAt.Format(time.RFC3339))
	}
	if j.Active {
		next := h.Svc.NextFire(j, now)
		if !next.IsZero() {
			fmt.Fprintf(&b, "next_fire:    %s\n", next.Format(time.RFC3339))
		}
	}
	fmt.Fprintf(&b, "fired_count:  %d\n", j.FiredCount)
	fmt.Fprintf(&b, "prompt:       %s\n", truncateInline(j.Prompt, 400))
	if j.Description != "" {
		fmt.Fprintf(&b, "description:  %s\n", truncateInline(j.Description, 400))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// ---------------------------------------------------------------------------
// cancel_job
// ---------------------------------------------------------------------------

type cancelJobInput struct {
	JobID  string `json:"job_id"`
	Reason string `json:"reason,omitempty"`
}

// CancelJob deactivates an active job. Idempotent in spirit: a
// second cancellation against the same ID returns an error so
// the agent surfaces "already cancelled" plainly instead of a
// silent success.
type CancelJob struct{ Svc *schedsvc.Service }

func (CancelJob) Tool() llm.Tool {
	return llm.Tool{
		Name: "cancel_job",
		Description: "Cancel an active scheduled job by ID. Optional reason is recorded for audit. " +
			"The on-disk JSONL log is preserved (immutable archive); the cancellation is a new record.",
		InputSchema: llm.Schema{
			Name: "cancel_job",
			Properties: map[string]llm.Property{
				"job_id": {Type: "string", Description: "The job's UUID."},
				"reason": {Type: "string", Description: "Optional free-text reason for cancellation."},
			},
			Required: []string{"job_id"},
		},
	}
}

func (h CancelJob) Execute(_ context.Context, input []byte) (string, error) {
	if h.Svc == nil {
		return "", fmt.Errorf("cancel_job: scheduler service not configured")
	}
	if len(input) == 0 {
		return "", fmt.Errorf("cancel_job: empty input")
	}
	var in cancelJobInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("cancel_job: decode input: %w", err)
	}
	id := strings.TrimSpace(in.JobID)
	if id == "" {
		return "", fmt.Errorf("cancel_job: job_id must not be empty")
	}
	if err := h.Svc.Cancel(id, strings.TrimSpace(in.Reason)); err != nil {
		return "", fmt.Errorf("cancel_job: %w", err)
	}
	return fmt.Sprintf("cancelled job %s", id), nil
}

// truncateInline caps a single-line value at n characters with
// `…` truncation. Local helper so the agent doesn't pull in the
// tools-package's truncate.
func truncateInline(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
