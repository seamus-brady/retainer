// Package agenttokens tracks per-agent token usage with durable
// JSON state files under `<workspace>/data/.agent-<name>-tokens.json`.
//
// Why a separate package: tokens are the load-bearing cost signal
// for the cog's routing decisions. They need to (a) survive
// restart, (b) accumulate over the agent's lifetime, (c) be
// surfaceable to the curator's sensorium block per cycle, and (d)
// be reportable to the webui activity stream in near-real-time.
// Centralising the storage shape keeps every consumer reading the
// same numbers.
//
// Concurrency: a Tracker guards its in-memory map with a mutex.
// Add() merges a delta and writes the state file atomically (via
// internal/actorstate). Reads are lock-free snapshots.
//
// What's recorded per agent:
//   - lifetime input + output tokens (cumulative since first
//     dispatch ever)
//   - today's input + output (resets at workspace local midnight)
//   - last_dispatch input + output (just the most recent task)
//   - last_seen timestamp
//
// Not recorded (deferred): per-cycle history, per-tool
// attribution, time-series rollups. Those land if + when the
// full Observer rebuild needs them.
package agenttokens

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/seamus-brady/retainer/internal/actorstate"
)

// Stats is the persisted shape per agent. JSON-tagged for
// stability — operators can `cat` the state file and read it.
type Stats struct {
	// SchemaVersion lets us evolve the file shape without
	// breaking older workspaces. Bump deliberately.
	SchemaVersion int `json:"schema_version"`

	// LifetimeInput + LifetimeOutput are cumulative since the
	// first dispatch ever (or since the file was last manually
	// reset).
	LifetimeInput  int `json:"lifetime_input"`
	LifetimeOutput int `json:"lifetime_output"`

	// TodayInput + TodayOutput cover dispatches whose
	// `last_seen` lies within the same workspace-local day as
	// `now`. Reset implicitly at midnight by the date check on
	// every Add().
	TodayInput  int `json:"today_input"`
	TodayOutput int `json:"today_output"`

	// TodayDate is the YYYY-MM-DD on which TodayInput/Output
	// began counting. When Add() sees a new day, the today
	// counters reset before adding.
	TodayDate string `json:"today_date,omitempty"`

	// LastDispatchInput + LastDispatchOutput are the tokens
	// from the most recent dispatch. Useful for "what was the
	// last task's cost" without a sliding-window data
	// structure.
	LastDispatchInput  int `json:"last_dispatch_input"`
	LastDispatchOutput int `json:"last_dispatch_output"`

	// DispatchCount is the number of completed dispatches the
	// counters have absorbed.
	DispatchCount int `json:"dispatch_count"`

	// LastSeen is the timestamp of the most recent Add().
	LastSeen time.Time `json:"last_seen,omitempty"`
}

// SchemaVersion pins the on-disk shape. A real bump means
// reading older files needs a migration.
const SchemaVersion = 1

// Tracker is the in-memory + on-disk per-agent token ledger for
// one workspace. Construct via NewTracker; one per running cog.
type Tracker struct {
	dataDir string

	mu     sync.RWMutex
	byName map[string]Stats

	// nowFn is overridable for tests; production uses time.Now.
	nowFn func() time.Time
}

// NewTracker constructs a Tracker rooted at the workspace's data
// dir. State files are at `<dataDir>/.agent-<name>-tokens.json`.
// Pre-existing files are NOT loaded eagerly — Stats() loads on
// first read per agent, and Add() reads-then-writes per call.
// This keeps boot fast and avoids holding stale data when the
// file is mutated between cog runs.
func NewTracker(dataDir string) *Tracker {
	return &Tracker{
		dataDir: dataDir,
		byName:  make(map[string]Stats),
		nowFn:   time.Now,
	}
}

// Add merges one dispatch's tokens into the named agent's
// counters and persists. Thread-safe; the in-memory + disk write
// happen under one lock.
//
// Returns the post-update Stats so callers can immediately
// publish (e.g. to an activity envelope) without a follow-up
// Stats() call.
func (t *Tracker) Add(agentName string, inputTokens, outputTokens int) (Stats, error) {
	if agentName == "" {
		return Stats{}, fmt.Errorf("agenttokens: agent name required")
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	cur, _ := t.loadLocked(agentName)

	now := t.nowFn()
	today := now.Format("2006-01-02")

	if cur.TodayDate != today {
		cur.TodayDate = today
		cur.TodayInput = 0
		cur.TodayOutput = 0
	}
	cur.SchemaVersion = SchemaVersion
	cur.LifetimeInput += inputTokens
	cur.LifetimeOutput += outputTokens
	cur.TodayInput += inputTokens
	cur.TodayOutput += outputTokens
	cur.LastDispatchInput = inputTokens
	cur.LastDispatchOutput = outputTokens
	cur.DispatchCount++
	cur.LastSeen = now

	t.byName[agentName] = cur
	if err := actorstate.Write(t.path(agentName), cur); err != nil {
		return cur, fmt.Errorf("agenttokens: write %s: %w", agentName, err)
	}
	return cur, nil
}

// Record satisfies the agent.TokenSink interface — same as Add
// but discards the returned Stats. Used by the agent substrate
// which only needs the side effect (persist + accumulate), not
// the post-update snapshot.
func (t *Tracker) Record(agentName string, inputTokens, outputTokens int) error {
	_, err := t.Add(agentName, inputTokens, outputTokens)
	return err
}

// Stats returns the current Stats for an agent. Reads are
// lock-free after the first lookup per agent in the process'
// lifetime — first read pulls from disk, subsequent reads from
// memory.
func (t *Tracker) Stats(agentName string) Stats {
	t.mu.RLock()
	cur, ok := t.byName[agentName]
	t.mu.RUnlock()
	if ok {
		return cur
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	cur, _ = t.loadLocked(agentName)
	return cur
}

// loadLocked reads the agent's state file, applies the
// today-rollover check, and caches in-memory. Caller holds the
// write lock. Returns the loaded stats and ok=true if the file
// existed and parsed cleanly.
func (t *Tracker) loadLocked(agentName string) (Stats, bool) {
	if cur, ok := t.byName[agentName]; ok {
		return cur, true
	}
	var s Stats
	err := actorstate.Read(t.path(agentName), &s)
	if err != nil {
		// Either the file doesn't exist (fresh agent) or it's
		// malformed. Either way, start fresh — preserving the
		// malformed file would just confuse operators.
		s = Stats{}
	}
	if s.SchemaVersion != SchemaVersion {
		// Future-proofing: today there's only v1. A real bump
		// would migrate here.
		s.SchemaVersion = SchemaVersion
	}
	t.byName[agentName] = s
	return s, err == nil
}

// path returns the absolute on-disk path for one agent's state.
// Filename is `.agent-<name>-tokens.json` with the leading dot
// matching the existing convention for actor state files
// (housekeeper, archivist).
func (t *Tracker) path(agentName string) string {
	return filepath.Join(t.dataDir, ".agent-"+sanitizeName(agentName)+"-tokens.json")
}

// sanitizeName reduces a name to filesystem-safe characters.
// Agents conventionally use bare lowercase identifiers
// ("researcher", "observer"); this is belt-and-braces against a
// future agent with hyphens or unusual characters.
func sanitizeName(name string) string {
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+('a'-'A'))
		case c >= '0' && c <= '9':
			out = append(out, c)
		case c == '-' || c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}
