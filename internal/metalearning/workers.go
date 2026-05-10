package metalearning

import (
	"context"
	"log/slog"
	"time"
)

// Worker is one background job the pool tracks. Name uniquely
// identifies it (used as the state key). Interval is the minimum
// gap between runs. Run is the function that does the work.
//
// V1 only ships the DirectTool kind — Run is a Go function that
// reads the workspace and writes durable artifacts directly. SD
// also has an AgentDelegation kind that dispatches to an agent;
// that ports as a follow-up if the V1 workers prove insufficient.
type Worker struct {
	Name     string
	Interval time.Duration
	Run      RunFn
}

// RunFn is the worker function signature. Returns an error so the
// pool can record failures + decide whether the next tick should
// retry. ctx is honoured for cancellation — workers should pass it
// through any long-running file or network calls.
type RunFn func(ctx context.Context, deps Deps) error

// Deps is the slice of state every worker receives. The pool builds
// this fresh per invocation so workers stay stateless — all per-
// worker state lives in the pool's State map.
type Deps struct {
	// DataDir is the workspace data directory. Workers read/write
	// the same JSONL hierarchy the cog uses (narrative, facts,
	// cases) and write artifacts under data/reports/ +
	// data/patterns/.
	DataDir string
	// Logger receives per-worker logs. The pool pre-attaches the
	// worker name, so workers can log freely without re-tagging.
	Logger *slog.Logger
	// NowFn returns the current time. Defaults to time.Now in
	// production; tests inject a deterministic clock.
	NowFn func() time.Time
	// FactSink lets workers persist a single summary fact the
	// sensorium can later perceive. Today only the fabrication-
	// audit worker uses it: each run writes one
	// `integrity_suspect_replies_7d` fact; the curator's
	// `<integrity>` block reads that fact and renders an
	// attribute every cycle so the agent sees its own integrity
	// number. Optional — workers MUST tolerate nil and skip the
	// persistence step (test fakes leave it nil; production
	// wires *librarian.Librarian via the bootstrap adapter).
	FactSink FactSink
	// CapturesStore is the captures package's persistence handle.
	// The captures worker uses it to append new commitments and
	// run the expiry sweep. Optional — workers MUST tolerate nil
	// and skip persistence (test fakes leave it nil; production
	// wires *captures.Store).
	CapturesStore CapturesStore
}

// FactSink is the narrow seam workers use to persist a single
// summary fact. Defined locally so the metalearning package
// doesn't depend on the librarian package — bootstrap wires an
// adapter that calls librarian.Librarian.RecordFact with the
// SD-default scope/operation/confidence.
type FactSink interface {
	RecordFact(FactRecord)
}

// FactRecord is the metalearning-shaped slice of librarian.Fact
// that workers care about. Bootstrap's adapter fills in the
// rest (scope=Persistent, operation=Write, confidence=1.0,
// timestamp assigned by the librarian actor).
type FactRecord struct {
	// Key is the fact key. Sensorium consumers look up facts by
	// key, so this is the contract between producer and consumer
	// (e.g. "integrity_suspect_replies_7d").
	Key string
	// Value is the JSON-encoded fact body. Curator consumers
	// parse this; producers MUST emit a stable schema.
	Value string
	// SourceCycleID names the cog cycle the worker runs under.
	// Workers run off-cog so this is typically empty — present
	// for parity with librarian.Fact's wire shape and for future
	// on-cog audit workers.
	SourceCycleID string
	// HalfLifeDays optionally tells the fact decay curve to
	// shrink confidence over time. Zero means no decay (the
	// audit's summary stays at full confidence until the next
	// run overwrites it).
	HalfLifeDays float64
}

// State is the durable record persisted at
// data/.metalearning-state.json. One entry per registered worker —
// last_run_at gates `due` checks, the rest is operator-visible
// audit + diagnostics.
type State struct {
	Version int                    `json:"version"`
	Workers map[string]WorkerState `json:"workers"`
}

// WorkerState tracks one worker's lifecycle. LastRunAt drives the
// due check; the rest is for the operator's eye.
type WorkerState struct {
	LastRunAt           time.Time `json:"last_run_at,omitempty"`
	CumulativeRuns      int64     `json:"cumulative_runs"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	LastFailureAt       time.Time `json:"last_failure_at,omitempty"`
	LastFailureMessage  string    `json:"last_failure_message,omitempty"`
}

// DefaultWorkers returns the V1 worker registry: one daily mining
// job and one weekly consolidation job. Bootstrap calls this and
// passes the slice into Pool's Config.
//
// Adding a new worker means appending here and writing its Run
// function (see consolidation.go / mining.go for templates).
// Operator-configurable intervals can come later via Config; for
// V1 the cadences are baked in.
func DefaultWorkers() []Worker {
	return []Worker{
		{
			Name:     "daily_mining",
			Interval: 24 * time.Hour,
			Run:      DailyMining,
		},
		{
			Name:     "weekly_consolidation",
			Interval: 7 * 24 * time.Hour,
			Run:      WeeklyConsolidation,
		},
		{
			Name:     "fabrication_audit",
			Interval: 24 * time.Hour,
			Run:      FabricationAudit,
		},
		{
			Name:     "captures",
			Interval: CapturesWorkerInterval,
			Run:      CapturesWorker,
		},
	}
}
