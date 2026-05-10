// Package metalearning is the dream-cycle harness — a self-ticking
// pool of background workers that perform reflective batch work
// (pattern mining, consolidation reports, audits) without going
// through the cog or the operator-facing scheduler.
//
// Mirrors Springdrift's `meta_learning/` worker pool. SD's pool
// dispatches to the remembrancer agent for synthesis-shaped jobs
// (`AgentDelegation`) and runs Go-only audits directly
// (`DirectTool`). Retainer V1 ships only the DirectTool kind —
// the AgentDelegation variant is deferred per
// `doc/specs/dream-cycle-metalearning.md`.
//
// The pool is the architectural counterpart to the housekeeper:
// the housekeeper handles HOT-INDEX HYGIENE (drop old SQLite rows,
// dedupe cases, suppress prunable failures); the metalearning pool
// handles REFLECTION (consolidate, mine patterns, write reports).
// Both self-tick, both persist `last_run_at` so cadence survives
// restart, both are independent of the cog so reflection never
// blocks user-facing work.
//
// V1 workers (see workers.go for the registry):
//
//   - daily_mining: cluster CBR cases via internal/cbr.FindClusters,
//     append cluster summaries to data/patterns/patterns.jsonl.
//   - weekly_consolidation: gather narrative + facts + cases for the
//     last 7 days, write a heuristic markdown digest to
//     data/reports/YYYY-MM-DD-weekly.md. LLM-driven synthesis is a
//     follow-up.
//
// Persistence lives at data/.metalearning-state.json — one entry per
// worker, atomic-write via internal/actorstate.
package metalearning

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/seamus-brady/retainer/internal/actorstate"
)

// defaultTickInterval bounds how often the pool wakes up to check
// whether any worker is due. Worker intervals are independent (one
// can be daily, another weekly); the tick is the polling cadence.
// 10 minutes keeps the pool responsive for short-interval workers
// without burning cycles on long-interval ones — a worker due in 1
// hour fires at most 10 minutes late, which is fine for batch work.
const defaultTickInterval = 10 * time.Minute

// Config wires the pool's collaborators. Workers + DataDir are
// required; the rest have sensible defaults.
type Config struct {
	// Workers is the registry of jobs the pool runs. Order does not
	// matter — every tick checks all workers; the ones whose
	// interval has elapsed since their last_run_at are run. Required.
	Workers []Worker
	// DataDir is the workspace data directory (for state file +
	// for workers' own JSONL writes — they receive it via Deps).
	// Required.
	DataDir string
	// TickInterval is how often the pool checks for due workers.
	// Zero defaults to defaultTickInterval (10 minutes).
	TickInterval time.Duration
	// NowFn returns the current time. Defaults to time.Now; tests
	// inject a deterministic clock.
	NowFn func() time.Time
	// Logger receives diagnostic logs. Defaults to slog.Default().
	Logger *slog.Logger
	// FactSink is forwarded to every worker's Deps. Optional. The
	// fabrication-audit worker uses it to persist its summary fact;
	// other workers MAY use it for the same pattern when they need
	// the agent to perceive their output through the sensorium.
	FactSink FactSink
	// CapturesStore is forwarded to every worker's Deps. Optional.
	// Currently only the captures worker uses it.
	CapturesStore CapturesStore
}

// Pool is the running actor. Construct with New, supervise via Run
// under actor.Permanent so a single worker failure never wedges the
// pool.
type Pool struct {
	cfg   Config
	state State
}

// New constructs a Pool from a Config, applying defaults. Returns
// an error when required fields are missing or workers fail
// validation.
func New(cfg Config) (*Pool, error) {
	if len(cfg.Workers) == 0 {
		return nil, fmt.Errorf("metalearning: at least one worker is required")
	}
	if cfg.DataDir == "" {
		return nil, fmt.Errorf("metalearning: DataDir is required")
	}
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = defaultTickInterval
	}
	if cfg.NowFn == nil {
		cfg.NowFn = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	seen := make(map[string]bool, len(cfg.Workers))
	for i, w := range cfg.Workers {
		if w.Name == "" {
			return nil, fmt.Errorf("metalearning: worker[%d] has empty Name", i)
		}
		if w.Interval <= 0 {
			return nil, fmt.Errorf("metalearning: worker %q: Interval must be positive", w.Name)
		}
		if w.Run == nil {
			return nil, fmt.Errorf("metalearning: worker %q: Run must not be nil", w.Name)
		}
		if seen[w.Name] {
			return nil, fmt.Errorf("metalearning: duplicate worker name %q", w.Name)
		}
		seen[w.Name] = true
	}

	p := &Pool{cfg: cfg}
	// Restore state from disk. Fresh-start (no file) is fine — every
	// worker's last_run_at is zero, so the first tick runs everything
	// once (catch-up) and then waits the configured interval.
	if err := actorstate.Read(p.stateFile(), &p.state); err != nil {
		cfg.Logger.Warn("metalearning: state restore failed; starting fresh",
			"path", p.stateFile(), "err", err)
		p.state = State{}
	}
	if p.state.Workers == nil {
		p.state.Workers = make(map[string]WorkerState)
	}
	if p.state.Version == 0 {
		p.state.Version = 1
	}
	return p, nil
}

// Run is the actor loop. Block until ctx is cancelled. Wraps with
// actor.Run under actor.Permanent — a single worker error increments
// that worker's failure counter and continues; only a panic in the
// loop itself triggers restart, and Run is a thin select.
//
// On startup, performs one immediate check so workers due at boot
// fire without waiting a full tick interval.
func (p *Pool) Run(ctx context.Context) error {
	p.cfg.Logger.Info("metalearning started",
		"tick_interval", p.cfg.TickInterval,
		"workers", len(p.cfg.Workers),
	)
	defer p.cfg.Logger.Info("metalearning stopped")

	p.checkAll(ctx)

	ticker := time.NewTicker(p.cfg.TickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			p.checkAll(ctx)
		}
	}
}

// checkAll runs one pass: for each worker, if its interval has
// elapsed since the last run, fire it. Errors are caught + logged
// per-worker so one bad worker can't take out the pass.
func (p *Pool) checkAll(ctx context.Context) {
	now := p.cfg.NowFn()
	for _, w := range p.cfg.Workers {
		if ctx.Err() != nil {
			return
		}
		if !p.due(w, now) {
			continue
		}
		p.runWorker(ctx, w, now)
	}
}

// due reports whether worker w should fire at time now. A worker
// with no recorded last_run_at runs immediately (catch-up); after
// that, it runs every Interval.
func (p *Pool) due(w Worker, now time.Time) bool {
	st := p.state.Workers[w.Name]
	if st.LastRunAt.IsZero() {
		return true
	}
	return now.Sub(st.LastRunAt) >= w.Interval
}

// runWorker invokes one worker's Run function and persists state.
// Errors are caught (not bubbled) — a worker failure increments its
// failure counter and the next tick will retry. The pool itself
// stays up.
func (p *Pool) runWorker(ctx context.Context, w Worker, now time.Time) {
	deps := Deps{
		DataDir:       p.cfg.DataDir,
		Logger:        p.cfg.Logger.With("worker", w.Name),
		NowFn:         p.cfg.NowFn,
		FactSink:      p.cfg.FactSink,
		CapturesStore: p.cfg.CapturesStore,
	}
	start := time.Now()
	err := safeRun(ctx, w, deps)
	durMs := time.Since(start).Milliseconds()

	st := p.state.Workers[w.Name]
	st.LastRunAt = now
	if err != nil {
		st.LastFailureAt = now
		st.LastFailureMessage = err.Error()
		st.ConsecutiveFailures++
		p.cfg.Logger.Warn("metalearning: worker failed",
			"worker", w.Name,
			"err", err,
			"duration_ms", durMs,
			"consecutive_failures", st.ConsecutiveFailures,
		)
	} else {
		st.LastFailureAt = time.Time{}
		st.LastFailureMessage = ""
		st.ConsecutiveFailures = 0
		st.CumulativeRuns++
		p.cfg.Logger.Info("metalearning: worker ran",
			"worker", w.Name,
			"duration_ms", durMs,
			"cumulative_runs", st.CumulativeRuns,
		)
	}
	p.state.Workers[w.Name] = st
	p.persistState()
}

// safeRun wraps a worker call in a panic recovery so one worker's
// crash never propagates into the pool loop. Mirrors actor.Permanent
// behaviour at the worker grain: a crash here counts as an error,
// not a pool restart.
func safeRun(ctx context.Context, w Worker, deps Deps) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("worker panicked: %v", r)
		}
	}()
	return w.Run(ctx, deps)
}

// stateFile returns the durable state path. One file for the whole
// pool — worker entries inside.
func (p *Pool) stateFile() string {
	return p.cfg.DataDir + "/.metalearning-state.json"
}

// persistState writes the durable state file. Atomic via
// actorstate.Write. Failure is logged + ignored so a transient
// disk-full event doesn't block worker progress.
func (p *Pool) persistState() {
	if err := actorstate.Write(p.stateFile(), p.state); err != nil {
		p.cfg.Logger.Warn("metalearning: state persist failed",
			"path", p.stateFile(),
			"err", err,
		)
	}
}
