package librarian

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/seamus-brady/retainer/internal/dailyfile"
)

const NarrativeSubdir = "narrative"

// NarrativeStatus categorises how a cycle ended.
type NarrativeStatus string

const (
	NarrativeStatusComplete  NarrativeStatus = "complete"
	NarrativeStatusBlocked   NarrativeStatus = "blocked"
	NarrativeStatusError     NarrativeStatus = "error"
	NarrativeStatusAbandoned NarrativeStatus = "abandoned"
)

// NarrativeEntry is one cycle's record. Append-only on disk;
// never mutated. Schema ports SD's `narrative/types.gleam:
// NarrativeEntry`: structured Intent + Outcome + DelegationChain
// + Entities + Sources + Metrics + Observations alongside the
// legacy top-level Status/Domain/Keywords fields. Old records
// produced before the rich shape landed decode cleanly — new
// fields default to zero values, omitempty keeps them out of
// the wire.
//
// Why both rich AND legacy fields:
//   - The SQLite index reads top-level Status/Domain/Keywords;
//     migrating it to read from nested fields is a separate
//     slice covering category-organised retrieval.
//   - Backward decoding of pre-migration entries needs the
//     legacy top-level fields to keep their meaning.
//   - New writes populate BOTH: the structured forms are
//     authoritative; the top-level fields mirror them so the
//     index keeps working without changes.
type NarrativeEntry struct {
	// SchemaVersion records the layout the entry was written
	// with. Old entries lack this and decode as 0 — readers
	// treat 0 as "legacy, has only the top-level fields".
	SchemaVersion int `json:"schema_version,omitempty"`

	CycleID       string    `json:"cycle_id"`
	ParentCycleID string    `json:"parent_cycle_id,omitempty"`
	Timestamp     time.Time `json:"timestamp"`

	// EntryType discriminates Narrative / Amendment / Summary /
	// Observation. Today only Narrative is emitted; the others
	// are reserved.
	EntryType EntryType `json:"type,omitempty"`

	Summary string `json:"summary"`

	// Intent is the structured intent. Curator-derived; the
	// description is NOT the verbatim user text.
	Intent Intent `json:"intent,omitempty"`

	// Outcome carries status + confidence + assessment. Status
	// here is the authoritative one; the legacy top-level
	// `Status` mirrors `Outcome.Status` on new writes.
	Outcome Outcome `json:"outcome,omitempty"`

	// DelegationChain is one entry per agent dispatch in this
	// cycle. Populated from the agent.CompletionRecord stream
	// the cog accumulates.
	DelegationChain []DelegationStep `json:"delegation_chain,omitempty"`

	// Decisions captures reasoning anchors the curator chose to
	// record. Optional.
	Decisions []Decision `json:"decisions,omitempty"`

	// Topics are higher-level subject tags (separate from
	// Keywords which are token-level).
	Topics []string `json:"topics,omitempty"`

	// Entities groups named things mentioned. Drives thread
	// assignment when threading lands.
	Entities Entities `json:"entities,omitempty"`

	// Sources lists external/internal references consulted.
	Sources []Source `json:"sources,omitempty"`

	// Thread is the derived grouping. Optional; populated by
	// the threading subsystem (deferred).
	Thread *Thread `json:"thread,omitempty"`

	// Metrics is the per-cycle resource accounting.
	Metrics Metrics `json:"metrics,omitempty"`

	// Observations records anomalies / noteworthy events.
	Observations []Observation `json:"observations,omitempty"`

	// Redacted marks an entry as suppressed (PII, operator-
	// removed). Not deleted on disk; readers exclude.
	Redacted bool `json:"redacted,omitempty"`

	// --- Legacy top-level fields ---
	// Kept for backward compatibility with pre-migration
	// records and for the SQLite index's existing columns.
	// New writes mirror them from the structured fields.

	Status   NarrativeStatus `json:"status"`
	Keywords []string        `json:"keywords,omitempty"`
	Domain   string          `json:"domain,omitempty"`
	Location string          `json:"location,omitempty"`
}

// MirrorLegacyFields populates the legacy top-level fields from
// the structured equivalents when they're set. Called by the
// store before writing so the SQLite index keeps getting the
// values it expects without touching every producer call site.
// Idempotent — safe to call multiple times.
func (e *NarrativeEntry) MirrorLegacyFields() {
	if e.Outcome.Status != "" && e.Status == "" {
		switch e.Outcome.Status {
		case OutcomeSuccess:
			e.Status = NarrativeStatusComplete
		case OutcomeFailure:
			e.Status = NarrativeStatusError
		case OutcomePartial:
			// Partial maps to Complete for the legacy index —
			// the cycle delivered something, just not cleanly.
			// The structured Outcome carries the nuance.
			e.Status = NarrativeStatusComplete
		}
	}
	if e.Intent.Domain != "" && e.Domain == "" {
		e.Domain = e.Intent.Domain
	}
	if e.SchemaVersion == 0 {
		e.SchemaVersion = NarrativeSchemaVersion
	}
}

// narrativeStore writes JSONL durably and indexes via SQLite for queries.
// windowDays bounds what gets imported into the index at startup; older
// JSONL files remain on disk untouched for the remembrancer.
type narrativeStore struct {
	dir        string
	writer     *dailyfile.Writer
	db         *sql.DB
	logger     *slog.Logger
	windowDays int
}

func newNarrativeStore(dataDir string, db *sql.DB, windowDays int, logger *slog.Logger) (*narrativeStore, error) {
	dir := filepath.Join(dataDir, NarrativeSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("librarian: mkdir narrative: %w", err)
	}
	return &narrativeStore{
		dir:        dir,
		writer:     dailyfile.NewWriter(dir, ".jsonl", time.Now),
		db:         db,
		logger:     logger,
		windowDays: windowDays,
	}, nil
}

func (s *narrativeStore) close() error {
	return s.writer.Close()
}

// record writes JSONL durably first, then INSERTs into the SQLite index.
// SQLite failure is logged but not returned — replay heals on restart.
func (s *narrativeStore) record(entry NarrativeEntry) error {
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now()
	}
	// Mirror structured fields onto the legacy top-level fields
	// so the SQLite index keeps populating correctly without
	// requiring every producer call site to know the new shape.
	entry.MirrorLegacyFields()
	body, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("librarian: marshal narrative: %w", err)
	}
	body = append(body, '\n')
	if _, err := s.writer.Write(body); err != nil {
		return fmt.Errorf("librarian: write narrative jsonl: %w", err)
	}
	if err := s.indexInsert(entry); err != nil {
		s.logger.Warn("librarian: narrative index insert failed (replay will heal)",
			"cycle_id", entry.CycleID, "err", err)
	}
	return nil
}

func (s *narrativeStore) indexInsert(entry NarrativeEntry) error {
	keywords, _ := json.Marshal(entry.Keywords)
	body, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("librarian: marshal narrative body: %w", err)
	}
	_, err = s.db.Exec(`
		INSERT OR REPLACE INTO narrative
		    (cycle_id, parent_cycle_id, timestamp, status, summary, keywords, domain, location, body)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		entry.CycleID, entry.ParentCycleID, entry.Timestamp.UnixNano(),
		string(entry.Status), entry.Summary, string(keywords), entry.Domain, entry.Location, body,
	)
	return err
}

// recentN returns up to limit most-recent entries, newest LAST (matches the
// public Librarian API contract used by the cog and tests). SQL returns
// newest-first; we reverse the slice.
func (s *narrativeStore) recentN(limit int) []NarrativeEntry {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`
		SELECT cycle_id, parent_cycle_id, timestamp, status, summary, keywords, domain, location, body
		FROM narrative
		ORDER BY timestamp DESC
		LIMIT ?
	`, limit)
	if err != nil {
		s.logger.Warn("librarian: narrative recent query failed", "err", err)
		return nil
	}
	defer rows.Close()

	var out []NarrativeEntry
	for rows.Next() {
		e, err := scanNarrative(rows)
		if err != nil {
			s.logger.Warn("librarian: narrative row scan failed", "err", err)
			continue
		}
		out = append(out, e)
	}
	// Reverse to newest-last.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// pruneIndexBefore deletes index rows whose timestamp is older than
// cutoff. JSONL on disk is unaffected — the immutable archive rule
// (project_archive_immutable memory) means we never delete records,
// only the runtime hot index. Returns the number of rows removed.
//
// Idempotent: running it twice with the same cutoff has the same
// effect as running once. Safe for the housekeeper to call on every
// tick.
func (s *narrativeStore) pruneIndexBefore(cutoff time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM narrative WHERE timestamp < ?`, cutoff.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("librarian: prune narrative index: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func scanNarrative(rows *sql.Rows) (NarrativeEntry, error) {
	var e NarrativeEntry
	var ts int64
	var parent, keywords, domain, location sql.NullString
	var status string
	var body []byte
	if err := rows.Scan(&e.CycleID, &parent, &ts, &status, &e.Summary, &keywords, &domain, &location, &body); err != nil {
		return NarrativeEntry{}, err
	}
	// Prefer the JSON body — it carries the rich schema (Intent,
	// Outcome, DelegationChain, Metrics, etc.). Old rows written
	// before the body column landed have body=nil; fall through to
	// the legacy column scan in that case so pre-migration data
	// keeps decoding.
	if len(body) > 0 {
		if err := json.Unmarshal(body, &e); err == nil {
			return e, nil
		}
		// Body decode failed — fall through and reconstruct from
		// the legacy columns rather than returning an error.
	}
	e.Timestamp = time.Unix(0, ts)
	e.Status = NarrativeStatus(status)
	if parent.Valid {
		e.ParentCycleID = parent.String
	}
	if keywords.Valid && keywords.String != "" {
		_ = json.Unmarshal([]byte(keywords.String), &e.Keywords)
	}
	if domain.Valid {
		e.Domain = domain.String
	}
	if location.Valid {
		e.Location = location.String
	}
	return e, nil
}

var narrativeFilePattern = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})\.jsonl$`)

// replay reads the last maxDays of JSONL files and bulk-INSERTs into
// SQLite. Truncated / malformed lines are dropped. Files outside the
// window are ignored. Uses a single transaction for speed.
func (s *narrativeStore) replay(now time.Time) error {
	dirEntries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	cutoff := now.AddDate(0, 0, -s.windowDays)

	type fileEntry struct {
		name string
		date time.Time
	}
	var files []fileEntry
	for _, e := range dirEntries {
		if e.IsDir() {
			continue
		}
		match := narrativeFilePattern.FindStringSubmatch(e.Name())
		if match == nil {
			continue
		}
		t, err := time.Parse(dailyfile.DateLayout, match[1])
		if err != nil {
			continue
		}
		if t.Before(cutoff) {
			continue
		}
		files = append(files, fileEntry{name: e.Name(), date: t})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].date.Before(files[j].date) })

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("librarian: begin replay tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT OR REPLACE INTO narrative
		    (cycle_id, parent_cycle_id, timestamp, status, summary, keywords, domain, location, body)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("librarian: prepare narrative replay: %w", err)
	}
	defer stmt.Close()

	for _, f := range files {
		path := filepath.Join(s.dir, f.name)
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("librarian: read %s: %w", path, err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var entry NarrativeEntry
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				continue
			}
			// Mirror legacy fields on rows written before the
			// rich-shape migration so the index columns stay
			// consistent across replay.
			entry.MirrorLegacyFields()
			keywords, _ := json.Marshal(entry.Keywords)
			// Re-marshal the entry into the body column so
			// reads can reconstruct the rich shape — the
			// raw `line` would also work, but re-marshalling
			// after MirrorLegacyFields keeps body and columns
			// in lock-step.
			body, _ := json.Marshal(entry)
			if _, err := stmt.Exec(
				entry.CycleID, entry.ParentCycleID, entry.Timestamp.UnixNano(),
				string(entry.Status), entry.Summary, string(keywords), entry.Domain, entry.Location, body,
			); err != nil {
				s.logger.Warn("librarian: narrative replay insert failed",
					"cycle_id", entry.CycleID, "err", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("librarian: commit replay tx: %w", err)
	}
	return nil
}
