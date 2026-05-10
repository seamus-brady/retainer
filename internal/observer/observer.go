// Package observer is the read-only diagnostic service backing the Observer
// agent's eventual tool executor. Today it exposes Go methods callable
// directly by the TUI; when the agent framework lands, the same methods
// become the implementation of the Observer's tool calls.
//
// In Springdrift, the Observer is a Transient-restart specialist agent
// with 18 tools (cycle forensics, pattern detection, CBR curation, fact
// tracing). See _impl_docs/ref/springdrift/src/agents/observer.gleam.
//
// What this package is NOT: it is not an actor. It does not have a goroutine,
// inbox, or react loop. It is the SUBSTRATE that the future Observer agent
// will use, not the agent itself. Don't dissolve the eventual-actor shape
// into "Observer is just methods on Librarian."
package observer

import (
	"time"

	"github.com/seamus-brady/retainer/internal/dag"
	"github.com/seamus-brady/retainer/internal/librarian"
)

// Observer is the diagnostic service. Holds references to the librarian
// and DAG actors; methods are read-only and synchronous.
type Observer struct {
	lib *librarian.Librarian
	d   *dag.DAG
}

// New constructs an Observer. Either dependency may be nil — methods that
// need it return zero values.
func New(lib *librarian.Librarian, d *dag.DAG) *Observer {
	return &Observer{lib: lib, d: d}
}

// CycleInspection merges DAG telemetry with narrative data for one
// cycle. Phase 2C: now includes the rich narrative fields (Intent,
// Outcome, DelegationChain, Metrics) so inspect_cycle output
// reflects the full curator-derived shape, not just a flat summary
// + status. Found is true if either subsystem knew about the
// cycle ID.
type CycleInspection struct {
	CycleID string

	// From DAG (zero values if cycle not in DAG)
	Type         dag.NodeType
	Status       dag.Status
	StartedAt    time.Time
	CompletedAt  time.Time
	Duration     time.Duration
	ErrorMessage string

	// From narrative (zero values if no narrative entry exists)
	NarrativeStatus librarian.NarrativeStatus
	Summary         string
	Keywords        []string
	NarrativeFound  bool

	// Rich narrative fields (Phase 2C). Zero/empty for entries
	// produced before the rich shape landed.
	Intent          librarian.Intent
	Outcome         librarian.Outcome
	DelegationChain []librarian.DelegationStep
	Topics          []string
	Entities        librarian.Entities
	Metrics         librarian.Metrics

	// Found is true when at least one subsystem returned data.
	Found bool
}

// RecentCycles returns up to limit most-recent narrative entries (newest
// last). Equivalent to the eventual `list_recent_cycles` Observer tool.
func (o *Observer) RecentCycles(limit int) []librarian.NarrativeEntry {
	if o.lib == nil {
		return nil
	}
	return o.lib.RecentNarrative(limit)
}

// InspectCycle returns merged DAG + narrative info for one cycle ID.
// Equivalent to the eventual `inspect_cycle` Observer tool.
func (o *Observer) InspectCycle(cycleID string) CycleInspection {
	out := CycleInspection{CycleID: cycleID}

	if o.d != nil {
		if n := o.d.Get(dag.CycleID(cycleID)); n != nil {
			out.Type = n.Type
			out.Status = n.Status
			out.StartedAt = n.StartedAt
			out.CompletedAt = n.CompletedAt
			out.ErrorMessage = n.ErrorMessage
			if !n.CompletedAt.IsZero() && !n.StartedAt.IsZero() {
				out.Duration = n.CompletedAt.Sub(n.StartedAt)
			}
			out.Found = true
		}
	}

	if o.lib != nil {
		// Search recent narrative for this cycle. We pass a generous limit
		// to find older cycles within the index window. Exact match on
		// cycle ID, so the cost is one slice scan over (at most) 60 days
		// of narrative entries — cheap.
		for _, e := range o.lib.RecentNarrative(500) {
			if e.CycleID == cycleID {
				out.Summary = e.Summary
				out.NarrativeStatus = e.Status
				out.Keywords = append([]string{}, e.Keywords...)
				out.NarrativeFound = true
				out.Found = true
				// Rich narrative fields (Phase 2C). Zero values
				// pass through unchanged for legacy entries that
				// pre-date the rich schema.
				out.Intent = e.Intent
				out.Outcome = e.Outcome
				if len(e.DelegationChain) > 0 {
					out.DelegationChain = append([]librarian.DelegationStep{}, e.DelegationChain...)
				}
				if len(e.Topics) > 0 {
					out.Topics = append([]string{}, e.Topics...)
				}
				out.Entities = e.Entities
				out.Metrics = e.Metrics
				break
			}
		}
	}

	return out
}

// GetFact wraps librarian.GetFact. Returns nil if unknown or librarian
// not configured. Equivalent (in service form) to the eventual
// `memory_read` / `memory_trace_fact` Observer tools.
func (o *Observer) GetFact(key string) *librarian.Fact {
	if o.lib == nil {
		return nil
	}
	return o.lib.GetFact(key)
}
