// Package scheduler is Retainer's autonomous-cycle scheduler.
// Jobs fire on cron expressions (recurring) or wall-clock times
// (one-shot), submitting their configured prompt back to the cog
// as a SourceAutonomous input — same channel as a TUI message,
// just with a different source tag for the policy gate.
//
// The package mirrors the taskmaster shape: a JSONL-backed
// service actor (`Service`) plus a specialist agent
// (`internal/agents/scheduler/`) that operators talk to. The
// agent's tools are thin wrappers over the service's API; the
// actor owns the cron tick + dispatch.
//
// Persistence: append-only `data/scheduler/jobs.jsonl`. State is
// derived by replaying `Op` events, same pattern as
// `internal/taskwarrior/storage`. Per `project_archive_immutable`
// the log is never edited; cancellations are new lines.
//
// Cron: `robfig/cron/v3` per the decisions-locked-in memory.
// Five-field expressions (no seconds), mirroring the
// most-common cron grammar.
package scheduler

import (
	"time"
)

// JobKind discriminates one-shot vs recurring jobs.
type JobKind string

const (
	// JobOneShot fires exactly once at FireAt, then auto-cancels.
	JobOneShot JobKind = "one_shot"
	// JobRecurring fires on Cron expressions; runs until the
	// operator cancels it.
	JobRecurring JobKind = "recurring"
)

// Job is one scheduled prompt the cog should run autonomously.
// Wire format = JSON; pinned field names so a future version of
// the package can read older logs cleanly.
type Job struct {
	// ID is the stable UUID. Returned to the operator so
	// cancellations target the right entry.
	ID string `json:"id"`
	// Name is a short operator-readable identifier ("weekly
	// review", "morning standup"). Not unique; ID is.
	Name string `json:"name"`
	// Kind selects scheduling semantics — JobOneShot or
	// JobRecurring.
	Kind JobKind `json:"kind"`
	// Cron is the five-field expression for recurring jobs.
	// Empty for one-shot.
	Cron string `json:"cron,omitempty"`
	// FireAt is the wall-clock time for one-shot jobs.
	// Zero for recurring.
	FireAt time.Time `json:"fire_at,omitempty"`
	// Prompt is the text the cog runs when the job fires.
	// Submitted as if the operator had typed it, but with
	// SourceAutonomous so the policy gate treats it as a
	// non-interactive cycle.
	Prompt string `json:"prompt"`
	// Description is operator-supplied context the agent can
	// surface back when listing jobs. Optional.
	Description string `json:"description,omitempty"`
	// CreatedAt stamps when the job was scheduled. Audit.
	CreatedAt time.Time `json:"created_at"`
	// LastFiredAt stamps the most recent fire. Zero for
	// jobs that have never fired.
	LastFiredAt time.Time `json:"last_fired_at,omitempty"`
	// FiredCount tracks total fires for operator visibility.
	FiredCount int64 `json:"fired_count,omitempty"`
	// Active is false when the job has been cancelled or has
	// completed (one-shot after firing). Replay flips this on
	// the cancellation / completion event.
	Active bool `json:"active"`
}

// OpKind discriminates the events that get appended to the
// JSONL log. Each operation produces one line.
type OpKind string

const (
	OpCreated   OpKind = "created"
	OpFired     OpKind = "fired"
	OpCancelled OpKind = "cancelled"
	OpCompleted OpKind = "completed" // one-shot reached its fire
)

// Op is one append-only event in the scheduler log. Replay
// derives current Job state from the sequence of Ops per ID.
type Op struct {
	// Kind selects which fields below are populated.
	Kind OpKind `json:"kind"`
	// Timestamp is the event time. Always populated.
	Timestamp time.Time `json:"timestamp"`
	// JobID is the target. For OpCreated this names the new
	// job; for the others it references an existing one.
	JobID string `json:"job_id"`

	// Job carries the full record on OpCreated; nil otherwise.
	Job *Job `json:"job,omitempty"`

	// CancelReason is operator-supplied free text on
	// OpCancelled. Optional.
	CancelReason string `json:"cancel_reason,omitempty"`
}
