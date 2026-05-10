package librarian

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/seamus-brady/retainer/internal/dailyfile"
)

const FactsSubdir = "facts"

// FactScope determines lifetime visibility. Matches Springdrift's
// facts/types.gleam exactly:
//
//	Persistent — workspace-scoped, survives session restarts
//	Session    — current session only (not reloaded on restart)
//	Ephemeral  — very short-lived, cleared at end of cycle
type FactScope string

const (
	FactScopePersistent FactScope = "persistent"
	FactScopeSession    FactScope = "session"
	FactScopeEphemeral  FactScope = "ephemeral"
)

// FactOperation discriminates what a fact record represents. Matches
// Springdrift's FactOp:
//
//	Write      — a new assertion or update (default)
//	Clear      — explicitly removed; reads return nil for cleared keys
//	Superseded — replaced by a newer fact (the chain pointer lives on
//	             the new record)
//
// Cleared is a tombstone — JSONL stays immutable, the index returns nil.
type FactOperation string

const (
	FactOperationWrite      FactOperation = "write"
	FactOperationClear      FactOperation = "clear"
	FactOperationSuperseded FactOperation = "superseded"
)

// Fact is a key-value entry with provenance and confidence. Stored
// confidence never mutates; decay is applied at read time via half-life.
// Supersession is via newer entries with the same key (most recent wins
// at lookup); a Clear-operation entry tombstones the key.
type Fact struct {
	Key           string        `json:"key"`
	Value         string        `json:"value"`
	Scope         FactScope     `json:"scope"`
	Operation     FactOperation `json:"operation,omitempty"`
	Confidence    float64       `json:"confidence"`
	Timestamp     time.Time     `json:"timestamp"`
	SourceCycleID string        `json:"source_cycle_id,omitempty"`
	HalfLifeDays  float64       `json:"half_life_days,omitempty"`
}

// DecayedConfidence returns the confidence at `now`, applying the
// half-life formula. Stored Confidence is unchanged. Pure function.
func (f Fact) DecayedConfidence(now time.Time) float64 {
	if f.HalfLifeDays == 0 {
		return f.Confidence
	}
	age := now.Sub(f.Timestamp).Hours() / 24
	if age < 0 {
		age = 0
	}
	return f.Confidence * math.Pow(0.5, age/f.HalfLifeDays)
}

// factsStore writes JSONL durably and indexes via SQLite. No window —
// facts are current state; the most recent entry per key remains valid
// regardless of age.
type factsStore struct {
	dir    string
	writer *dailyfile.Writer
	db     *sql.DB
	logger *slog.Logger
}

func newFactsStore(dataDir string, db *sql.DB, logger *slog.Logger) (*factsStore, error) {
	dir := filepath.Join(dataDir, FactsSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("librarian: mkdir facts: %w", err)
	}
	return &factsStore{
		dir:    dir,
		writer: dailyfile.NewWriter(dir, "-facts.jsonl", time.Now),
		db:     db,
		logger: logger,
	}, nil
}

func (s *factsStore) close() error { return s.writer.Close() }

func (s *factsStore) record(f Fact) error {
	if f.Key == "" {
		return errors.New("librarian: fact missing key")
	}
	if f.Timestamp.IsZero() {
		f.Timestamp = time.Now()
	}
	if f.Confidence < 0 {
		f.Confidence = 0
	}
	if f.Confidence > 1 {
		f.Confidence = 1
	}
	if f.Scope == "" {
		f.Scope = FactScopePersistent
	}
	if f.Operation == "" {
		f.Operation = FactOperationWrite
	}

	body, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("librarian: marshal fact: %w", err)
	}
	body = append(body, '\n')
	if _, err := s.writer.Write(body); err != nil {
		return fmt.Errorf("librarian: write fact jsonl: %w", err)
	}
	if err := s.indexInsert(f); err != nil {
		s.logger.Warn("librarian: fact index insert failed (replay will heal)",
			"key", f.Key, "err", err)
	}
	return nil
}

func (s *factsStore) indexInsert(f Fact) error {
	_, err := s.db.Exec(`
		INSERT INTO facts
		    (key, value, scope, operation, confidence, timestamp, source_cycle_id, half_life_days)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`,
		f.Key, f.Value, string(f.Scope), string(f.Operation), f.Confidence,
		f.Timestamp.UnixNano(), f.SourceCycleID, f.HalfLifeDays,
	)
	return err
}

// persistentCount returns the number of distinct keys whose most-recent
// non-superseded entry has scope=Persistent and operation=Write. The
// "most-recent per key" rule matters because a Persistent key can be
// superseded by a Session-scoped write or cleared later — those keys
// shouldn't count as persistent any more.
func (s *factsStore) persistentCount() int {
	row := s.db.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT key, scope, operation,
			       ROW_NUMBER() OVER (PARTITION BY key ORDER BY timestamp DESC) AS rn
			FROM facts
		) WHERE rn = 1 AND scope = ? AND operation = ?
	`, string(FactScopePersistent), string(FactOperationWrite))
	var n int
	if err := row.Scan(&n); err != nil {
		s.logger.Warn("librarian: persistent count failed", "err", err)
		return 0
	}
	return n
}

// recentPersistent returns up to limit most-recent persistent-scoped facts
// (one per key, most-recent wins; cleared keys excluded), confidence
// decayed to now. Newest first.
func (s *factsStore) recentPersistent(limit int, now time.Time) []Fact {
	if limit <= 0 {
		return nil
	}
	rows, err := s.db.Query(`
		SELECT key, value, scope, operation, confidence, timestamp, source_cycle_id, half_life_days
		FROM (
			SELECT key, value, scope, operation, confidence, timestamp, source_cycle_id, half_life_days,
			       ROW_NUMBER() OVER (PARTITION BY key ORDER BY timestamp DESC) AS rn
			FROM facts
		)
		WHERE rn = 1 AND scope = ? AND operation = ?
		ORDER BY timestamp DESC
		LIMIT ?
	`, string(FactScopePersistent), string(FactOperationWrite), limit)
	if err != nil {
		s.logger.Warn("librarian: recentPersistent query failed", "err", err)
		return nil
	}
	defer rows.Close()
	var out []Fact
	for rows.Next() {
		f, err := scanFactRow(rows)
		if err != nil {
			s.logger.Warn("librarian: recentPersistent scan failed", "err", err)
			continue
		}
		f.Confidence = f.DecayedConfidence(now)
		out = append(out, f)
	}
	return out
}

// get returns the most recent fact for key with confidence decayed to
// now, or nil when the key was never recorded OR its most recent entry
// is a Clear tombstone (logically absent).
func (s *factsStore) get(key string, now time.Time) *Fact {
	row := s.db.QueryRow(`
		SELECT key, value, scope, operation, confidence, timestamp, source_cycle_id, half_life_days
		FROM facts
		WHERE key = ?
		ORDER BY timestamp DESC
		LIMIT 1
	`, key)

	f, err := scanFact(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		s.logger.Warn("librarian: get fact failed", "key", key, "err", err)
		return nil
	}
	if f.Operation == FactOperationClear {
		// Tombstone — logically absent. JSONL still has the trail for
		// memory_trace_fact (when that lands).
		return nil
	}
	out := f
	out.Confidence = f.DecayedConfidence(now)
	return &out
}

// search returns most-recent-per-key facts whose key or value contains
// keyword (case-insensitive). Cleared keys are excluded. Confidence
// decayed to now. Newest first, capped at limit.
func (s *factsStore) search(keyword string, limit int, now time.Time) []Fact {
	if keyword == "" || limit <= 0 {
		return nil
	}
	pattern := "%" + strings.ToLower(keyword) + "%"
	rows, err := s.db.Query(`
		SELECT key, value, scope, operation, confidence, timestamp, source_cycle_id, half_life_days
		FROM (
			SELECT key, value, scope, operation, confidence, timestamp, source_cycle_id, half_life_days,
			       ROW_NUMBER() OVER (PARTITION BY key ORDER BY timestamp DESC) AS rn
			FROM facts
		)
		WHERE rn = 1
		  AND operation = ?
		  AND (LOWER(key) LIKE ? OR LOWER(value) LIKE ?)
		ORDER BY timestamp DESC
		LIMIT ?
	`, string(FactOperationWrite), pattern, pattern, limit)
	if err != nil {
		s.logger.Warn("librarian: search facts failed", "err", err)
		return nil
	}
	defer rows.Close()
	var out []Fact
	for rows.Next() {
		f, err := scanFactRow(rows)
		if err != nil {
			s.logger.Warn("librarian: search scan failed", "err", err)
			continue
		}
		f.Confidence = f.DecayedConfidence(now)
		out = append(out, f)
	}
	return out
}

func scanFact(row *sql.Row) (Fact, error) {
	var f Fact
	var ts int64
	var sourceCycle sql.NullString
	var halfLife sql.NullFloat64
	var scope, operation string
	if err := row.Scan(&f.Key, &f.Value, &scope, &operation, &f.Confidence, &ts, &sourceCycle, &halfLife); err != nil {
		return Fact{}, err
	}
	f.Scope = FactScope(scope)
	f.Operation = FactOperation(operation)
	f.Timestamp = time.Unix(0, ts)
	if sourceCycle.Valid {
		f.SourceCycleID = sourceCycle.String
	}
	if halfLife.Valid {
		f.HalfLifeDays = halfLife.Float64
	}
	return f, nil
}

// scanFactRow handles the *sql.Rows variant used by recentPersistent and
// search — same column shape as scanFact, different scanner type.
func scanFactRow(rows *sql.Rows) (Fact, error) {
	var f Fact
	var ts int64
	var sourceCycle sql.NullString
	var halfLife sql.NullFloat64
	var scope, operation string
	if err := rows.Scan(&f.Key, &f.Value, &scope, &operation, &f.Confidence, &ts, &sourceCycle, &halfLife); err != nil {
		return Fact{}, err
	}
	f.Scope = FactScope(scope)
	f.Operation = FactOperation(operation)
	f.Timestamp = time.Unix(0, ts)
	if sourceCycle.Valid {
		f.SourceCycleID = sourceCycle.String
	}
	if halfLife.Valid {
		f.HalfLifeDays = halfLife.Float64
	}
	return f, nil
}

var factsFilePattern = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})-facts\.jsonl$`)

// replay reads ALL JSONL files in chronological order and bulk-INSERTs into
// SQLite. No window cutoff — facts are current state; the most recent entry
// per key remains valid until superseded, regardless of age. Most-recent-
// wins semantics fall out of querying with ORDER BY timestamp DESC LIMIT 1.
func (s *factsStore) replay(now time.Time) error {
	dirEntries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}

	type fileEntry struct {
		name string
		date time.Time
	}
	var files []fileEntry
	for _, e := range dirEntries {
		if e.IsDir() {
			continue
		}
		match := factsFilePattern.FindStringSubmatch(e.Name())
		if match == nil {
			continue
		}
		t, err := time.Parse(dailyfile.DateLayout, match[1])
		if err != nil {
			continue
		}
		files = append(files, fileEntry{name: e.Name(), date: t})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].date.Before(files[j].date) })

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("librarian: begin facts replay tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO facts
		    (key, value, scope, operation, confidence, timestamp, source_cycle_id, half_life_days)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("librarian: prepare facts replay: %w", err)
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
			var fact Fact
			if err := json.Unmarshal([]byte(line), &fact); err != nil {
				continue
			}
			// Records written before the Operation field landed have
			// no operation; treat them as Write so their semantics
			// don't change.
			if fact.Operation == "" {
				fact.Operation = FactOperationWrite
			}
			if _, err := stmt.Exec(
				fact.Key, fact.Value, string(fact.Scope), string(fact.Operation), fact.Confidence,
				fact.Timestamp.UnixNano(), fact.SourceCycleID, fact.HalfLifeDays,
			); err != nil {
				s.logger.Warn("librarian: facts replay insert failed",
					"key", fact.Key, "err", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("librarian: commit facts replay tx: %w", err)
	}
	return nil
}
