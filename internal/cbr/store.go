package cbr

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SubDir is the workspace data subdirectory cases live in.
// `<workspace>/data/cases/`.
const SubDir = "cases"

// Store is the append-only JSONL persistence layer for cases. The
// librarian owns one of these and serialises writes through its
// inbox. Reads (replay) happen at startup before the librarian's
// goroutine starts.
//
// Single file shape: cases.jsonl (not date-rotated). Cases are a
// small population — the workspace might accumulate hundreds over
// months — so date rotation would just spread them across many files
// for no replay benefit. The narrative store rotates by date because
// recent-N queries care about today; the case store rebuilds a full
// in-memory index regardless.
type Store struct {
	dir    string
	path   string
	logger *slog.Logger
}

// NewStore opens (or creates) the cases JSONL file under
// `<dataDir>/<SubDir>/cases.jsonl`. Returns an error if the
// destination dir can't be created or the writer can't open the file.
func NewStore(dataDir string, logger *slog.Logger) (*Store, error) {
	if logger == nil {
		logger = slog.Default()
	}
	dir := filepath.Join(dataDir, SubDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cbr: mkdir cases: %w", err)
	}
	// We deliberately use a fixed filename rather than a date-rotated
	// one. dailyfile.NewWriter with a constant `now` would still
	// rotate on midnight; we hand-roll the path instead and use
	// dailyfile only for its atomic-write helper later if needed.
	path := filepath.Join(dir, "cases.jsonl")
	// Open file for append; create if missing.
	w, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("cbr: open cases.jsonl: %w", err)
	}
	_ = w.Close()
	return &Store{
		dir:    dir,
		path:   path,
		logger: logger,
	}, nil
}

// Append writes one case to the JSONL file. Sets Timestamp to now
// when caller leaves it zero (matches the actor-side timestamp rule
// from feedback_actor_timestamps).
func (s *Store) Append(c Case) error {
	if c.Timestamp.IsZero() {
		c.Timestamp = time.Now()
	}
	if c.SchemaVersion == 0 {
		c.SchemaVersion = SchemaVersion
	}
	body, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("cbr: marshal case: %w", err)
	}
	body = append(body, '\n')
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("cbr: open cases.jsonl: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(body); err != nil {
		return fmt.Errorf("cbr: write case: %w", err)
	}
	return nil
}

// LoadAll reads every case in the JSONL file. Truncated / malformed
// lines are skipped with a warning so a partial write at process-
// crash doesn't take out the whole replay. Returns an empty slice
// when the file doesn't exist (fresh workspace).
//
// Used by the librarian at startup to rebuild the in-memory CaseBase.
// Subsequent updates land via Append + CaseBase.Retain in the same
// actor goroutine.
func (s *Store) LoadAll() ([]Case, error) {
	f, err := os.Open(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cbr: open cases.jsonl for replay: %w", err)
	}
	defer f.Close()

	var out []Case
	scanner := bufio.NewScanner(f)
	// Cases can carry 384-dim float32 vectors when present; default
	// scanner buffer (~64KB) is too small for those. Bump to 4MB.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var c Case
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			s.logger.Warn("cbr: skipped malformed line during replay",
				"path", s.path, "line", lineNo, "err", err)
			continue
		}
		out = append(out, c)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("cbr: scan cases.jsonl: %w", err)
	}
	return out, nil
}

// Path returns the cases.jsonl path. Used by tests + the remembrancer
// (when it lands) to read the full archive directly.
func (s *Store) Path() string { return s.path }

