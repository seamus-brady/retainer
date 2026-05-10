// Package librarian owns the in-memory query layer for narrative + facts
// stores. JSONL on disk is the durable record; SQLite is a throwaway
// in-memory index rebuilt at startup from JSONL. Mirrors Springdrift's
// Librarian-owns-ETS pattern: a single goroutine owns all index state,
// all reads and writes go through its inbox.
//
// Architecture:
//
//   On every Record*: append a JSONL line (durable) → INSERT into SQLite
//                     (rebuildable). JSONL succeeds first; if SQLite
//                     fails, we log and continue — replay heals on restart.
//
//   On startup: replay scans last MaxDays of JSONL and bulk-INSERTs into
//               SQLite via a transaction. Truncated lines are dropped.
//
//   On reads:   SQL queries against the SQLite index (newest-N narrative,
//               most-recent-per-key fact, future arbitrary queries).
//
// Stores:
//   - Narrative: per-cycle records, queried by recency and (future) status / domain
//   - Facts: key/value with scope, confidence, read-time half-life decay
//
// Deferred (per arch notes / not-load-bearing-yet):
//   - CBR cases (6-signal retrieval)
//   - Artifacts (50KB cap)
//   - Threading / overlap scoring
//   - Sensorium hookup (identity templates can grow slots that query the
//     librarian when this lands)
package librarian

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	_ "modernc.org/sqlite"

	"github.com/seamus-brady/retainer/internal/cbr"
	"github.com/seamus-brady/retainer/internal/cyclelog"
	"github.com/seamus-brady/retainer/internal/dailyfile"
	"github.com/seamus-brady/retainer/internal/embed"
)

const (
	// defaultNarrativeWindowDays bounds the SQLite hot index for narrative
	// (a rolling "recent activity" window). Older entries stay in JSONL on
	// disk and are accessible via the remembrancer's direct-file tools.
	defaultNarrativeWindowDays = 60
	inboxBufferSize            = 256
)

type Options struct {
	DataDir string       // <workspace>/data
	Logger  *slog.Logger // optional; nil → slog.Default()
	// NarrativeWindowDays caps how many days of narrative JSONL get loaded
	// into the SQLite index at startup. Older files are not deleted —
	// they remain on disk for the remembrancer's deep-archive tools.
	// Zero → defaultNarrativeWindowDays.
	NarrativeWindowDays int
	// Facts have no window concept — they're current state, not a rolling
	// window. Replay loads every fact regardless of age; staleness is
	// handled by half-life decay applied at read time.

	// Embedder, when non-nil, drives the 6th (semantic) signal in
	// CBR retrieval. When nil, CaseBase auto-renormalises the other
	// five signals — retrieval still works, just without the embedding
	// boost. Tests typically pass embed.NewMock; production wires
	// embed.NewHugot.
	Embedder embed.Embedder
}

type Librarian struct {
	logger *slog.Logger
	inbox  chan message

	db *sql.DB

	narrative *narrativeStore
	facts     *factsStore
	cases     *casesStore
}

// New creates a Librarian, opens an in-memory SQLite, applies schema, and
// replays existing JSONL into the index. Run() must be called separately
// to start the inbox loop.
func New(opts Options) (*Librarian, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.NarrativeWindowDays == 0 {
		opts.NarrativeWindowDays = defaultNarrativeWindowDays
	}

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("librarian: open sqlite: %w", err)
	}
	// Single connection — keeps the in-memory DB shared across all queries
	// and aligns with the actor's single-writer rule.
	db.SetMaxOpenConns(1)
	if err := applySchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}

	narr, err := newNarrativeStore(opts.DataDir, db, opts.NarrativeWindowDays, opts.Logger)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := narr.replay(time.Now()); err != nil {
		_ = db.Close()
		return nil, err
	}

	fa, err := newFactsStore(opts.DataDir, db, opts.Logger)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := fa.replay(time.Now()); err != nil {
		_ = db.Close()
		return nil, err
	}

	cs, err := newCasesStore(opts.DataDir, opts.Embedder, opts.Logger)
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Librarian{
		logger:    opts.Logger,
		inbox:     make(chan message, inboxBufferSize),
		db:        db,
		narrative: narr,
		facts:     fa,
		cases:     cs,
	}, nil
}

func applySchema(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE narrative (
			cycle_id        TEXT PRIMARY KEY,
			parent_cycle_id TEXT,
			timestamp       INTEGER NOT NULL,
			status          TEXT NOT NULL,
			summary         TEXT NOT NULL,
			keywords        TEXT,
			domain          TEXT,
			location        TEXT,
			body            BLOB
		)`,
		`CREATE INDEX idx_narrative_timestamp ON narrative(timestamp)`,
		`CREATE INDEX idx_narrative_status    ON narrative(status)`,
		`CREATE TABLE facts (
			key             TEXT NOT NULL,
			value           TEXT NOT NULL,
			scope           TEXT NOT NULL,
			operation       TEXT NOT NULL DEFAULT 'write',
			confidence      REAL NOT NULL,
			timestamp       INTEGER NOT NULL,
			source_cycle_id TEXT,
			half_life_days  REAL
		)`,
		`CREATE INDEX idx_facts_key_ts ON facts(key, timestamp)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("librarian: schema apply: %w", err)
		}
	}
	return nil
}

// Run is the actor loop. Block until ctx is cancelled. Wrap with actor.Run
// under actor.Permanent so panics restart the loop without losing the
// process.
func (l *Librarian) Run(ctx context.Context) error {
	defer l.narrative.close()
	defer l.facts.close()
	defer l.db.Close()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case m := <-l.inbox:
			l.handle(m)
		}
	}
}

func (l *Librarian) handle(m message) {
	switch v := m.(type) {
	case recordNarrativeMsg:
		if err := l.narrative.record(v.entry); err != nil {
			l.logger.Warn("librarian narrative write failed", "err", err)
		}
	case recentNarrativeMsg:
		v.reply <- l.narrative.recentN(v.limit)
	case recordFactMsg:
		if err := l.facts.record(v.fact); err != nil {
			l.logger.Warn("librarian fact write failed", "err", err)
		}
	case getFactMsg:
		v.reply <- l.facts.get(v.key, time.Now())
	case persistentFactCountMsg:
		v.reply <- l.facts.persistentCount()
	case recentPersistentFactsMsg:
		v.reply <- l.facts.recentPersistent(v.limit, time.Now())
	case searchFactsMsg:
		v.reply <- l.facts.search(v.keyword, v.limit, time.Now())
	case pruneNarrativeIndexMsg:
		removed, err := l.narrative.pruneIndexBefore(v.cutoff)
		v.reply <- pruneResult{removed: removed, err: err}
	case recordCaseMsg:
		if err := l.cases.recordCase(v.cbrCase); err != nil {
			l.logger.Warn("librarian case write failed", "err", err, "case_id", v.cbrCase.ID)
		}
	case retrieveCasesMsg:
		scored := l.cases.retrieve(v.ctx, v.query)
		// Register the surfaced case IDs against the in-flight cycle
		// so the archivist can bump usage stats at end-of-cycle.
		// CycleID rides in via context (cog injects it through
		// cyclelog.WithCycleID); empty means "not in a cycle" — e.g.
		// a TUI/operator-side query — and we silently skip.
		if cycleID := cyclelog.CycleIDFromContext(v.ctx); cycleID != "" && len(scored) > 0 {
			ids := make([]string, len(scored))
			for i, s := range scored {
				ids[i] = s.Case.ID
			}
			l.cases.registerRetrieval(cycleID, ids)
		}
		v.reply <- scored
	case drainRetrievedCaseIDsMsg:
		v.reply <- l.cases.drainRetrievedCaseIDs(v.cycleID)
	case getCaseMsg:
		c, ok := l.cases.getCase(v.id)
		v.reply <- getCaseResult{cbrCase: c, found: ok}
	case caseCountMsg:
		v.reply <- l.cases.caseCount()
	case allCasesMsg:
		v.reply <- l.cases.allCases()
	case mutateCaseMsg:
		c, ok := l.cases.getCase(v.id)
		if !ok {
			v.reply <- mutateCaseResult{err: fmt.Errorf("librarian: case %q not found", v.id)}
			break
		}
		updated := v.mutate(c)
		if err := l.cases.recordCase(updated); err != nil {
			v.reply <- mutateCaseResult{err: err}
			break
		}
		v.reply <- mutateCaseResult{cbrCase: updated}
	}
}

// RecordNarrative is fire-and-forget. Never blocks (inbox is buffered) — if
// the inbox fills under sustained pressure, the entry is dropped with a
// warning log.
func (l *Librarian) RecordNarrative(entry NarrativeEntry) {
	select {
	case l.inbox <- recordNarrativeMsg{entry: entry}:
	default:
		l.logger.Warn("librarian inbox full; dropped narrative entry", "cycle_id", entry.CycleID)
	}
}

// RecentNarrative returns up to limit most-recent entries, newest last.
// Synchronous — blocks on the actor inbox.
func (l *Librarian) RecentNarrative(limit int) []NarrativeEntry {
	reply := make(chan []NarrativeEntry, 1)
	l.inbox <- recentNarrativeMsg{limit: limit, reply: reply}
	return <-reply
}

// RecordFact is fire-and-forget.
func (l *Librarian) RecordFact(f Fact) {
	select {
	case l.inbox <- recordFactMsg{fact: f}:
	default:
		l.logger.Warn("librarian inbox full; dropped fact", "key", f.Key)
	}
}

// ClearFact records a tombstone for key — a log entry with
// Operation=Clear. Subsequent GetFact returns nil; the JSONL trail is
// preserved (memory archive is immutable). sourceCycleID is the cycle
// that issued the clear; empty is allowed but the audit will be
// unattributed.
//
// Timestamp is intentionally NOT set here — the actor goroutine assigns
// it inside record(). Setting time.Now() at the caller site would race
// with actor-side Write timestamps (the actor processes messages later
// in wall-clock time than the caller fires them), and a Clear could
// end up older than the Write it's meant to supersede.
func (l *Librarian) ClearFact(key, sourceCycleID string) {
	l.RecordFact(Fact{
		Key:           key,
		Operation:     FactOperationClear,
		Scope:         FactScopeSession, // Springdrift parity — Clear records use Session scope
		Confidence:    0,
		SourceCycleID: sourceCycleID,
	})
}

// SearchFacts returns up to limit facts whose key or value contains
// keyword (case-insensitive substring), most recent per key, cleared
// keys excluded. Synchronous.
func (l *Librarian) SearchFacts(keyword string, limit int) []Fact {
	reply := make(chan []Fact, 1)
	l.inbox <- searchFactsMsg{keyword: keyword, limit: limit, reply: reply}
	return <-reply
}

// PruneNarrativeIndex deletes narrative index rows whose timestamp is
// older than cutoff. JSONL files are NOT touched (archive immutable
// per project_archive_immutable). Returns the number of rows removed,
// or 0 + error on SQL failure. Synchronous — the housekeeper calls
// this on its tick interval.
//
// Idempotent: calling twice with the same cutoff is the same as
// calling once. Safe for the housekeeper to invoke without state.
func (l *Librarian) PruneNarrativeIndex(cutoff time.Time) (int64, error) {
	reply := make(chan pruneResult, 1)
	l.inbox <- pruneNarrativeIndexMsg{cutoff: cutoff, reply: reply}
	r := <-reply
	return r.removed, r.err
}

// GetFact returns the most recent fact for key with confidence decayed to
// now, or nil if the key was never recorded. Synchronous.
func (l *Librarian) GetFact(key string) *Fact {
	reply := make(chan *Fact, 1)
	l.inbox <- getFactMsg{key: key, reply: reply}
	return <-reply
}

// PersistentFactCount returns the number of distinct keys whose most-recent
// entry has scope=Persistent. Used by the Curator for the
// {{persistent_fact_count}} preamble slot. Synchronous.
func (l *Librarian) PersistentFactCount() int {
	reply := make(chan int, 1)
	l.inbox <- persistentFactCountMsg{reply: reply}
	return <-reply
}

// RecentPersistentFacts returns up to limit most-recently-written persistent
// facts (one entry per key, most recent supersedes older). Confidence is
// decayed to now. Used by the Curator for the {{recent_fact_sample}} slot.
// Synchronous.
func (l *Librarian) RecentPersistentFacts(limit int) []Fact {
	reply := make(chan []Fact, 1)
	l.inbox <- recentPersistentFactsMsg{limit: limit, reply: reply}
	return <-reply
}

// RecordCase is fire-and-forget. The case must already carry its
// embedding (when one is desired) — the librarian goroutine is the
// single writer for both JSONL and CaseBase, but it does NOT run the
// embedder itself. Callers that produce cases (the archivist) embed
// in their own goroutine before sending here.
//
// The inbox is buffered; under sustained pressure cases drop with a
// warning. Cases are re-derivable from narrative if the archivist
// path runs again, so dropping is acceptable degradation.
func (l *Librarian) RecordCase(c cbr.Case) {
	select {
	case l.inbox <- recordCaseMsg{cbrCase: c}:
	default:
		l.logger.Warn("librarian inbox full; dropped case", "case_id", c.ID)
	}
}

// RetrieveCases scores cases via the CaseBase. Synchronous — blocks
// on the actor inbox + embedding latency (one query embed call when
// embedder is configured).
//
// `ctx` is forwarded to the embedder so caller cancellation
// propagates. CaseBase.Retrieve handles embedder errors gracefully —
// a transient failure drops the embedding signal and renormalises
// weights for that one query.
func (l *Librarian) RetrieveCases(ctx context.Context, q cbr.Query) []cbr.Scored {
	reply := make(chan []cbr.Scored, 1)
	l.inbox <- retrieveCasesMsg{ctx: ctx, query: q, reply: reply}
	return <-reply
}

// GetCase returns a case by ID. Includes redacted (suppressed) cases
// — observer curation tools need to read them by ID. Returns ok=false
// when the case isn't in the in-memory CaseBase.
func (l *Librarian) GetCase(id string) (cbr.Case, bool) {
	reply := make(chan getCaseResult, 1)
	l.inbox <- getCaseMsg{id: id, reply: reply}
	r := <-reply
	return r.cbrCase, r.found
}

// CaseCount returns the active (non-redacted) case count. Used by
// the curator's sensorium <memory cases="N"/> attr.
func (l *Librarian) CaseCount() int {
	reply := make(chan int, 1)
	l.inbox <- caseCountMsg{reply: reply}
	return <-reply
}

// SuppressCase marks a case as redacted so it drops out of retrieval.
// The on-disk record stays intact (immutable archive) — the new
// record adds the Redacted flag. Returns the updated case (with new
// timestamp + Redacted=true) or an error when the case ID is unknown.
//
// Used by the observer's `suppress_case` curation tool.
func (l *Librarian) SuppressCase(id string) (cbr.Case, error) {
	return l.mutateCase(id, func(c cbr.Case) cbr.Case {
		c.Redacted = true
		c.Timestamp = time.Now()
		return c
	})
}

// UnsuppressCase clears the redacted flag on a case. Counterpart to
// SuppressCase; lets an operator restore a case they suppressed.
func (l *Librarian) UnsuppressCase(id string) (cbr.Case, error) {
	return l.mutateCase(id, func(c cbr.Case) cbr.Case {
		c.Redacted = false
		c.Timestamp = time.Now()
		return c
	})
}

// BoostCase adjusts a case's outcome confidence by delta and clamps
// to [0, 1]. Positive delta promotes a case; negative delta
// demotes. Mirrors SD's `boost_case` observer tool.
func (l *Librarian) BoostCase(id string, delta float64) (cbr.Case, error) {
	return l.mutateCase(id, func(c cbr.Case) cbr.Case {
		next := c.Outcome.Confidence + delta
		if next < 0 {
			next = 0
		}
		if next > 1 {
			next = 1
		}
		c.Outcome.Confidence = next
		c.Timestamp = time.Now()
		return c
	})
}

// AnnotateCase appends a pitfall note to the case's outcome. Used
// when an operator notices something to watch out for that the
// archivist's auto-classification missed.
func (l *Librarian) AnnotateCase(id, pitfall string) (cbr.Case, error) {
	if pitfall == "" {
		return cbr.Case{}, fmt.Errorf("librarian: empty pitfall annotation")
	}
	return l.mutateCase(id, func(c cbr.Case) cbr.Case {
		c.Outcome.Pitfalls = append(c.Outcome.Pitfalls, pitfall)
		c.Timestamp = time.Now()
		return c
	})
}

// CorrectCase replaces the case's problem / solution / outcome with
// updated values. Used by the observer's `correct_case` tool when an
// operator finds a misclassified case. Re-embedding (if the problem
// text changed) is the caller's responsibility — pass the new
// embedding on the input case.
func (l *Librarian) CorrectCase(id string, fields cbr.Case) (cbr.Case, error) {
	return l.mutateCase(id, func(c cbr.Case) cbr.Case {
		// Identity fields stay; content fields replace.
		c.Problem = fields.Problem
		c.Solution = fields.Solution
		c.Outcome = fields.Outcome
		if fields.Category != "" {
			c.Category = fields.Category
		}
		if len(fields.Embedding) > 0 {
			c.Embedding = fields.Embedding
			c.EmbedderID = fields.EmbedderID
		}
		c.Timestamp = time.Now()
		return c
	})
}

// SupersedeCase marks a case retired by dedup, linking it to the
// dominant case via SupersededBy. Records a new JSONL line via the
// existing curation pipeline (immutable archive). Effect on
// retrieval is identical to suppress (excluded), but the audit
// distinction (operator-driven vs housekeeper-driven) is preserved.
//
// Refuses to set SupersededBy to the case's own ID (would create a
// self-loop) or to an empty string (use UnsuppressCase or a future
// UnsupersedeCase if reversal is needed).
func (l *Librarian) SupersedeCase(id, byID string) (cbr.Case, error) {
	if byID == "" {
		return cbr.Case{}, fmt.Errorf("librarian: SupersedeCase requires non-empty dominant id")
	}
	if id == byID {
		return cbr.Case{}, fmt.Errorf("librarian: SupersedeCase refuses self-reference (case %q)", id)
	}
	return l.mutateCase(id, func(c cbr.Case) cbr.Case {
		c.SupersededBy = byID
		c.Timestamp = time.Now()
		return c
	})
}

// AllCases returns a snapshot of every case (including redacted +
// superseded). Synchronous; blocks on the actor inbox. Used by the
// housekeeper for dedup + prune sweeps. The slice is freshly
// allocated and safe to iterate without further synchronisation.
func (l *Librarian) AllCases() []cbr.Case {
	reply := make(chan []cbr.Case, 1)
	l.inbox <- allCasesMsg{reply: reply}
	return <-reply
}

// DrainRetrievedCaseIDs returns the deduplicated list of case IDs the
// agent saw via RetrieveCases during cycleID, then clears the entry.
// Synchronous — the cog calls this at end-of-cycle (success and
// abandon paths both) so the per-cycle map self-cleans.
//
// Returns nil when the cycle never ran a retrieval. Order is
// first-occurrence.
func (l *Librarian) DrainRetrievedCaseIDs(cycleID string) []string {
	if cycleID == "" {
		return nil
	}
	reply := make(chan []string, 1)
	l.inbox <- drainRetrievedCaseIDsMsg{cycleID: cycleID, reply: reply}
	return <-reply
}

// RecordCaseRetrieval bumps RetrievalCount on every case that was
// surfaced to the agent during a cycle and bumps
// RetrievalSuccessCount on those same cases when the cycle ended in
// success. Drives the utility-score signal in CBR retrieval.
//
// Fire-and-forget per-case — internally each one is a mutateCaseMsg.
// Best-effort: a missing case ID is logged and skipped.
func (l *Librarian) RecordCaseRetrieval(caseIDs []string, cycleSucceeded bool) {
	for _, id := range caseIDs {
		_, err := l.mutateCase(id, func(c cbr.Case) cbr.Case {
			if c.UsageStats == nil {
				c.UsageStats = &cbr.UsageStats{}
			}
			c.UsageStats.RetrievalCount++
			if cycleSucceeded {
				c.UsageStats.RetrievalSuccessCount++
			}
			c.Timestamp = time.Now()
			return c
		})
		if err != nil {
			l.logger.Warn("librarian: usage-stats update skipped", "case_id", id, "err", err)
		}
	}
}

// mutateCase is the synchronous mutation primitive. The supplied
// fn runs inside the librarian goroutine — keep it pure and fast.
func (l *Librarian) mutateCase(id string, fn func(cbr.Case) cbr.Case) (cbr.Case, error) {
	reply := make(chan mutateCaseResult, 1)
	l.inbox <- mutateCaseMsg{id: id, mutate: fn, reply: reply}
	r := <-reply
	return r.cbrCase, r.err
}

// Compile-time helpers for the dailyfile import (referenced in stores).
var _ = dailyfile.DateLayout

// message is the librarian inbox sum type.
type message interface{ isLibMsg() }

type recordNarrativeMsg struct {
	entry NarrativeEntry
}

type recentNarrativeMsg struct {
	limit int
	reply chan<- []NarrativeEntry
}

type recordFactMsg struct {
	fact Fact
}

type getFactMsg struct {
	key   string
	reply chan<- *Fact
}

type persistentFactCountMsg struct {
	reply chan<- int
}

type recentPersistentFactsMsg struct {
	limit int
	reply chan<- []Fact
}

type searchFactsMsg struct {
	keyword string
	limit   int
	reply   chan<- []Fact
}

type pruneNarrativeIndexMsg struct {
	cutoff time.Time
	reply  chan<- pruneResult
}

type pruneResult struct {
	removed int64
	err     error
}

// CBR-related messages.

type recordCaseMsg struct {
	cbrCase cbr.Case
}

type retrieveCasesMsg struct {
	ctx   context.Context
	query cbr.Query
	reply chan<- []cbr.Scored
}

type drainRetrievedCaseIDsMsg struct {
	cycleID string
	reply   chan<- []string
}

type getCaseMsg struct {
	id    string
	reply chan<- getCaseResult
}

type getCaseResult struct {
	cbrCase cbr.Case
	found   bool
}

type caseCountMsg struct {
	reply chan<- int
}

type allCasesMsg struct {
	reply chan<- []cbr.Case
}

// mutateCaseMsg is the curation entry-point. The librarian goroutine
// looks up the case, applies the caller's pure mutate fn, then
// records the result (which appends a new JSONL record + upserts the
// CaseBase). Used by SuppressCase / UnsuppressCase / BoostCase /
// AnnotateCase / CorrectCase — each is a thin wrapper that supplies
// a different mutate fn.
type mutateCaseMsg struct {
	id     string
	mutate func(cbr.Case) cbr.Case
	reply  chan<- mutateCaseResult
}

type mutateCaseResult struct {
	cbrCase cbr.Case
	err     error
}

func (recordNarrativeMsg) isLibMsg()        {}
func (recentNarrativeMsg) isLibMsg()        {}
func (recordFactMsg) isLibMsg()             {}
func (getFactMsg) isLibMsg()                {}
func (persistentFactCountMsg) isLibMsg()    {}
func (recentPersistentFactsMsg) isLibMsg()  {}
func (searchFactsMsg) isLibMsg()            {}
func (pruneNarrativeIndexMsg) isLibMsg()    {}
func (recordCaseMsg) isLibMsg()             {}
func (retrieveCasesMsg) isLibMsg()          {}
func (getCaseMsg) isLibMsg()                {}
func (caseCountMsg) isLibMsg()              {}
func (allCasesMsg) isLibMsg()               {}
func (mutateCaseMsg) isLibMsg()             {}
func (drainRetrievedCaseIDsMsg) isLibMsg()  {}
