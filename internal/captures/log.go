package captures

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/seamus-brady/retainer/internal/dailyfile"
)

// Store wraps the on-disk capture JSONL collection. Construct
// with Open; lifecycle is bound to the workspace data dir.
//
// On-disk layout: `<dataDir>/captures/YYYY-MM-DD-captures.jsonl`,
// one JSONL line per record (Pending or status-supersession).
// Append-only — older days never edited.
//
// Thread-safety: dailyfile.Writer is goroutine-safe for Write;
// reads scan the directory at call time so concurrent writers
// don't corrupt the read view (each line is atomic up to its
// length under PIPE_BUF, which the librarian convention assumes).
type Store struct {
	dir    string
	writer *dailyfile.Writer
	nowFn  func() time.Time
}

// Open creates the captures dir if needed and returns a handle.
// Cheap to call repeatedly; the dailyfile.Writer is lazy.
//
// The `now` clock is shared between the dailyfile writer (which
// uses it to pick the daily filename) and Append's Timestamp
// default. Tests inject a deterministic clock so persistence
// order matches CreatedAt order; production passes nil and gets
// time.Now.
func Open(dataDir string, now func() time.Time) (*Store, error) {
	if dataDir == "" {
		return nil, errors.New("captures: dataDir is required")
	}
	dir := filepath.Join(dataDir, Subdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("captures: mkdir %s: %w", dir, err)
	}
	if now == nil {
		now = time.Now
	}
	return &Store{
		dir:    dir,
		writer: dailyfile.NewWriter(dir, FilenameSuffix, now),
		nowFn:  now,
	}, nil
}

// Close releases the underlying file handle. Safe to call once.
func (s *Store) Close() error {
	if s == nil || s.writer == nil {
		return nil
	}
	return s.writer.Close()
}

// Append writes a Capture to today's JSONL. Idempotent on the
// content-addressed ID — callers that re-detect the same phrase
// can call Append twice; later readers replay-and-dedupe by ID.
// (We don't pre-check before write because the scanner takes the
// dedupe responsibility; the JSONL stays append-only.)
func (s *Store) Append(c Capture) error {
	if s == nil || s.writer == nil {
		return errors.New("captures: store not open")
	}
	if c.ID == "" {
		return errors.New("captures: capture has no ID")
	}
	if c.Timestamp.IsZero() {
		c.Timestamp = s.nowFn()
	}
	if c.SchemaVersion == 0 {
		c.SchemaVersion = SchemaVersion
	}
	if c.Status == "" {
		c.Status = StatusPending
	}
	body, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("captures: marshal: %w", err)
	}
	if _, err := s.writer.Write(append(body, '\n')); err != nil {
		return fmt.Errorf("captures: write: %w", err)
	}
	return nil
}

// SnapshotPending replays all on-disk JSONL into a most-recent-
// per-ID map and returns the entries whose latest status is
// Pending. Order: oldest CreatedAt first — sensorium consumers
// usually want "what's been outstanding longest" rendered first.
//
// Pure read — never mutates files. Safe to call from the curator
// at the start of every cycle.
func (s *Store) SnapshotPending() ([]Capture, error) {
	if s == nil {
		return nil, nil
	}
	all, err := s.replayAll()
	if err != nil {
		return nil, err
	}
	pending := make([]Capture, 0, len(all))
	for _, c := range all {
		if c.Status == StatusPending {
			pending = append(pending, c)
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		return pending[i].CreatedAt.Before(pending[j].CreatedAt)
	})
	return pending, nil
}

// CountPending returns the same number SnapshotPending(...) would
// return without materialising the full list. Convenience for the
// sensorium's count-only render. Errors are absorbed and treated
// as zero — the sensorium is a read-only ambient signal and a
// missing file is normal on a fresh workspace.
func (s *Store) CountPending() int {
	pending, err := s.SnapshotPending()
	if err != nil {
		return 0
	}
	return len(pending)
}

// LookupByID returns the most-recent record with the given id, or
// nil if no record exists. Used by callers (future
// dismiss_capture / expiry sweep) to fetch the original Pending
// entry's metadata before writing a status-supersession line.
func (s *Store) LookupByID(id string) (*Capture, error) {
	all, err := s.replayAll()
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].ID == id {
			c := all[i]
			return &c, nil
		}
	}
	return nil, nil
}

// replayAll reads every JSONL file under <dir>/, returning the
// most-recent-per-ID record (by Timestamp). Files are read in
// alphabetical (== chronological) order; within a file lines are
// processed top-to-bottom; within an ID, the latest wins.
func (s *Store) replayAll() ([]Capture, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("captures: read dir: %w", err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, FilenameSuffix) {
			continue
		}
		files = append(files, name)
	}
	sort.Strings(files)

	latest := map[string]Capture{}
	for _, fname := range files {
		path := filepath.Join(s.dir, fname)
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var c Capture
			if err := json.Unmarshal(line, &c); err != nil {
				continue
			}
			if c.ID == "" {
				continue
			}
			prev, seen := latest[c.ID]
			if !seen || !c.Timestamp.Before(prev.Timestamp) {
				latest[c.ID] = c
			}
		}
		f.Close()
	}
	out := make([]Capture, 0, len(latest))
	for _, c := range latest {
		out = append(out, c)
	}
	return out, nil
}

// SweepExpired walks the on-disk Pending entries and writes a
// supersession record (Status=Expired) for each one whose
// CreatedAt is older than `now - olderThan`. Returns the count
// of newly-expired captures. Called by the expiry worker on a
// daily cadence.
//
// Idempotent: a Pending entry that's been Expired won't be
// re-expired on the next sweep (its latest record is no longer
// Pending). The original Pending JSONL line is never touched.
func (s *Store) SweepExpired(now time.Time, olderThan time.Duration) (int, error) {
	if s == nil {
		return 0, nil
	}
	pending, err := s.SnapshotPending()
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-olderThan)
	expired := 0
	for _, c := range pending {
		if c.CreatedAt.After(cutoff) {
			continue
		}
		exp := c
		exp.Status = StatusExpired
		exp.Timestamp = now
		exp.Reason = fmt.Sprintf("auto-expired after %s", olderThan)
		if err := s.Append(exp); err != nil {
			return expired, err
		}
		expired++
	}
	return expired, nil
}
