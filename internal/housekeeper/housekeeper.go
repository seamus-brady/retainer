// Package housekeeper periodically prunes the librarian's in-memory
// SQLite hot index per `project_per_store_windows`. JSONL on disk is
// NEVER touched — the immutable-archive rule
// (`project_archive_immutable`) means we only manage the runtime
// working set.
//
// V1 scope:
//   - Narrative index: drop rows older than NarrativeWindowDays.
//   - CBR dedup: pairs above similarity threshold get the loser
//     marked SupersededBy the dominant. JSONL preserves both
//     records; only the in-memory CaseBase + retrieval result
//     change.
//   - CBR prune: failure cases past max age with no recorded
//     pitfall and no operator validation get suppressed (Redacted
//     via SuppressCase). Same JSONL-preserved semantics as dedup.
//
// Deferred (each tied to its prerequisite):
//   - Session-scoped fact pruning on session end (needs explicit
//     session lifecycle; right now sessions are implicit per process)
//   - Ephemeral-scoped fact pruning at end-of-cycle (needs cog
//     to expose a "cycle complete" hook beyond the cycle-log event)
//
// Mirrors Springdrift's `_impl_docs/ref/springdrift/src/narrative/
// housekeeper.gleam` shape: a periodic actor, single concern (index
// hygiene), no JSONL deletions ever.
package housekeeper

import (
	"context"
	"log/slog"
	"time"

	"github.com/seamus-brady/retainer/internal/actorstate"
	"github.com/seamus-brady/retainer/internal/cbr"
)

const (
	// defaultTickInterval is how often the housekeeper sweeps. One
	// hour matches Springdrift's `housekeeping_interval_ms` default
	// closely enough for V1; can be lowered for tests via Config.
	defaultTickInterval = time.Hour

	// defaultNarrativeWindowDays is the rolling window for narrative
	// index entries. Matches the librarian's
	// defaultNarrativeWindowDays so the housekeeper preserves the
	// same window the librarian's startup replay uses.
	defaultNarrativeWindowDays = 60
)

// IndexPruner is the slice of *librarian.Librarian the housekeeper
// uses. Letting tests substitute a fake keeps them independent of
// the librarian's SQLite plumbing.
//
// CBR-related methods are optional from the housekeeper's
// perspective: a Config without CBR sweeps enabled never calls them,
// so test fakes can return zero values when CBR is out of scope.
// Keeping them on the same interface (vs. a second optional one)
// matches the existing librarian's full surface and avoids type-
// assertion gymnastics.
type IndexPruner interface {
	PruneNarrativeIndex(cutoff time.Time) (int64, error)
	AllCases() []cbr.Case
	SupersedeCase(id, byID string) (cbr.Case, error)
	SuppressCase(id string) (cbr.Case, error)
}

// Config wires the housekeeper's collaborators. Librarian is required;
// everything else has a sensible default.
type Config struct {
	// Librarian is the index whose narrative table gets pruned.
	Librarian IndexPruner
	// TickInterval is how often the housekeeper sweeps. Zero
	// defaults to one hour.
	TickInterval time.Duration
	// NarrativeWindowDays is the rolling window beyond which
	// narrative entries drop out of the index. Zero defaults to 60.
	NarrativeWindowDays int
	// NowFn returns the current time. Defaults to time.Now; tests
	// inject a deterministic clock.
	NowFn func() time.Time
	// Logger receives diagnostic logs. Defaults to slog.Default().
	Logger *slog.Logger
	// StateFile is the path to the JSON state file (typically
	// `<workspace>/data/.housekeeper-state.json`). Tracks last sweep
	// time + cumulative pruned count for operator visibility. When
	// empty, state isn't persisted (test config).
	StateFile string

	// CBRSweepEnabled gates the CBR dedup + prune sweeps. False (the
	// zero value) skips them entirely so a misconfiguration can't
	// flag false-positives across the corpus.
	CBRSweepEnabled bool
	// CBRDedupThreshold is the similarity score above which a pair
	// is treated as duplicates. Zero defaults to
	// cbr.DefaultDedupThreshold (0.85). Tunable per workspace when
	// the corpus characteristics need it.
	CBRDedupThreshold float64
	// CBRPruneMaxFailureAge is the age threshold past which a
	// failure-without-lesson case becomes prunable. Zero defaults to
	// cbr.DefaultPruneMaxFailureAge (30 days).
	CBRPruneMaxFailureAge time.Duration
}

// State is the housekeeper's durable cadence state. Persisted to
// `data/.housekeeper-state.json` per `project_system_actors_self_tick`.
// Records sweep history rather than driving sweep decisions — the
// housekeeper's prune is cutoff-driven, not cursor-driven, so state is
// for operator visibility, not correctness.
type State struct {
	Version              int       `json:"version"`
	LastSweepAt          time.Time `json:"last_sweep_at,omitempty"`
	LastSweepRemoved     int64     `json:"last_sweep_removed"`
	CumulativeRemoved    int64     `json:"cumulative_removed"`
	ConsecutiveFailures  int       `json:"consecutive_failures"`
	LastFailureAt        time.Time `json:"last_failure_at,omitempty"`
	LastFailureMessage   string    `json:"last_failure_message,omitempty"`
	// CBR sweep counters. Cumulative across the workspace's
	// lifetime; LastCBRSweep* hold per-tick deltas. Distinct from
	// the narrative counters above — narrative sweeps prune SQL rows
	// with no audit trail, while CBR sweeps mark JSONL records that
	// remain on disk forever.
	LastCBRSupersededCount int64 `json:"last_cbr_superseded_count,omitempty"`
	LastCBRSuppressedCount int64 `json:"last_cbr_suppressed_count,omitempty"`
	CumulativeSuperseded   int64 `json:"cumulative_superseded,omitempty"`
	CumulativeSuppressed   int64 `json:"cumulative_suppressed,omitempty"`
}

// Housekeeper is the running actor. Constructed with New, started by
// Run under a Permanent supervisor spec so periodic sweeps survive
// any individual sweep failure.
type Housekeeper struct {
	cfg   Config
	state State
}

// New constructs a Housekeeper from a Config, applying defaults.
// Returns an error when Librarian is nil.
func New(cfg Config) (*Housekeeper, error) {
	if cfg.Librarian == nil {
		return nil, errLibrarianRequired
	}
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = defaultTickInterval
	}
	if cfg.NarrativeWindowDays <= 0 {
		cfg.NarrativeWindowDays = defaultNarrativeWindowDays
	}
	if cfg.CBRDedupThreshold <= 0 {
		cfg.CBRDedupThreshold = cbr.DefaultDedupThreshold
	}
	if cfg.CBRPruneMaxFailureAge <= 0 {
		cfg.CBRPruneMaxFailureAge = cbr.DefaultPruneMaxFailureAge
	}
	if cfg.NowFn == nil {
		cfg.NowFn = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	h := &Housekeeper{cfg: cfg}
	// Restore state from disk when configured. Fresh-start (no file)
	// is fine — we just begin with zeroed counters.
	if cfg.StateFile != "" {
		if err := actorstate.Read(cfg.StateFile, &h.state); err != nil {
			// Restoration failure is non-fatal — log and continue with
			// zero state. The housekeeper is idempotent (cutoff-driven),
			// so a lost state file just means we lose ops history, not
			// correctness.
			cfg.Logger.Warn("housekeeper: state restore failed; starting fresh",
				"path", cfg.StateFile, "err", err)
			h.state = State{}
		}
	}
	if h.state.Version == 0 {
		h.state.Version = 1
	}
	return h, nil
}

// Run is the actor loop. Block until ctx is cancelled. Wraps with
// actor.Run under actor.Permanent — panics in a sweep restart cleanly
// with the next tick. Sweeps run on the configured TickInterval.
//
// On startup, performs one immediate sweep so the index is current
// even if the agent has just resumed from a long-running JSONL
// archive (replay loads the full window; an immediate sweep is a
// no-op there but cheap to verify).
func (h *Housekeeper) Run(ctx context.Context) error {
	h.cfg.Logger.Info("housekeeper started",
		"tick_interval", h.cfg.TickInterval,
		"narrative_window_days", h.cfg.NarrativeWindowDays,
	)
	defer h.cfg.Logger.Info("housekeeper stopped")

	// Immediate sweep at startup so the operator sees one happen
	// before the first tick interval elapses.
	h.sweep()

	ticker := time.NewTicker(h.cfg.TickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			h.sweep()
		}
	}
}

// sweep runs one prune pass against the librarian. Errors are logged
// but never bubble up — a transient SQL failure shouldn't crash the
// housekeeper goroutine. The next tick will try again. Exposed as
// a method for direct test invocation without spinning up the loop.
//
// On success, updates the durable state file (last_sweep_at,
// cumulative_removed, last_sweep_removed) so an operator can see when
// the last sweep ran and how much it cleaned. State writes are atomic
// (temp + rename); a failure to write state is logged but doesn't
// roll back the prune.
//
// CBR sweeps run after the narrative sweep when CBRSweepEnabled is
// true. They never bubble errors either: each per-case mark records
// individually so a single librarian failure can't take out the rest
// of the sweep.
func (h *Housekeeper) sweep() {
	now := h.cfg.NowFn()
	cutoff := now.Add(-time.Duration(h.cfg.NarrativeWindowDays) * 24 * time.Hour)
	removed, err := h.cfg.Librarian.PruneNarrativeIndex(cutoff)
	if err != nil {
		h.state.ConsecutiveFailures++
		h.state.LastFailureAt = now
		h.state.LastFailureMessage = err.Error()
		h.persistState()
		h.cfg.Logger.Warn("housekeeper: narrative prune failed",
			"cutoff", cutoff,
			"err", err,
			"consecutive_failures", h.state.ConsecutiveFailures,
		)
		return
	}
	h.state.LastSweepAt = now
	h.state.LastSweepRemoved = removed
	h.state.CumulativeRemoved += removed
	h.state.ConsecutiveFailures = 0
	h.state.LastFailureAt = time.Time{}
	h.state.LastFailureMessage = ""
	if removed > 0 {
		h.cfg.Logger.Info("housekeeper: narrative index pruned",
			"removed", removed,
			"cutoff", cutoff,
			"cumulative_removed", h.state.CumulativeRemoved,
		)
	}

	if h.cfg.CBRSweepEnabled {
		h.cbrSweep(now)
	}

	h.persistState()
}

// cbrSweep marks duplicates and prunable failures via the librarian's
// curation API. Pure-by-rule (the cbr package's Similarity / Find*
// functions) so the policy is unit-testable without a librarian; the
// I/O lives here.
//
// Both passes preserve the JSONL archive — SupersedeCase /
// SuppressCase append new records, never delete. The librarian's
// CaseBase.Retrieve excludes both flags so retrieval result honours
// the marks immediately.
func (h *Housekeeper) cbrSweep(now time.Time) {
	all := h.cfg.Librarian.AllCases()
	if len(all) == 0 {
		h.state.LastCBRSupersededCount = 0
		h.state.LastCBRSuppressedCount = 0
		return
	}

	// Dedup pass: pairs above threshold get the loser marked.
	pairs := cbr.FindDuplicates(all, h.cfg.CBRDedupThreshold)
	var superseded int64
	for _, p := range pairs {
		if _, err := h.cfg.Librarian.SupersedeCase(p.Loser, p.Dominant); err != nil {
			h.cfg.Logger.Warn("housekeeper: supersede failed",
				"loser", p.Loser, "dominant", p.Dominant, "err", err,
			)
			continue
		}
		superseded++
	}
	h.state.LastCBRSupersededCount = superseded
	h.state.CumulativeSuperseded += superseded
	if superseded > 0 {
		h.cfg.Logger.Info("housekeeper: cbr dedup ran",
			"superseded", superseded,
			"threshold", h.cfg.CBRDedupThreshold,
			"cumulative_superseded", h.state.CumulativeSuperseded,
		)
	}

	// Prune pass: failure-without-lesson cases past max age get
	// suppressed. Re-fetch after dedup so newly-superseded cases
	// don't slip into the prune candidate set.
	pruned := cbr.FindPrunable(h.cfg.Librarian.AllCases(), now, h.cfg.CBRPruneMaxFailureAge)
	var suppressed int64
	for _, c := range pruned {
		if _, err := h.cfg.Librarian.SuppressCase(c.ID); err != nil {
			h.cfg.Logger.Warn("housekeeper: prune-suppress failed",
				"case_id", c.ID, "err", err,
			)
			continue
		}
		suppressed++
	}
	h.state.LastCBRSuppressedCount = suppressed
	h.state.CumulativeSuppressed += suppressed
	if suppressed > 0 {
		h.cfg.Logger.Info("housekeeper: cbr prune ran",
			"suppressed", suppressed,
			"max_age", h.cfg.CBRPruneMaxFailureAge,
			"cumulative_suppressed", h.state.CumulativeSuppressed,
		)
	}
}

// persistState writes the durable state file. No-op when StateFile is
// empty (test config). State write failures are logged but never
// surface — they're operational visibility, not correctness.
func (h *Housekeeper) persistState() {
	if h.cfg.StateFile == "" {
		return
	}
	if err := actorstate.Write(h.cfg.StateFile, h.state); err != nil {
		h.cfg.Logger.Warn("housekeeper: state persist failed",
			"path", h.cfg.StateFile,
			"err", err,
		)
	}
}

// errLibrarianRequired is package-level so callers can errors.Is on
// it for clean New-time validation.
var errLibrarianRequired = housekeeperError("housekeeper: Librarian is required")

type housekeeperError string

func (e housekeeperError) Error() string { return string(e) }
