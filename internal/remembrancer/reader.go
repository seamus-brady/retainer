// Package remembrancer reads the workspace's memory archives
// directly off disk, bypassing the librarian's SQLite hot index.
// Used by the cog's deep-archive tools to reach beyond the rolling
// window into months / years of accumulated memory.
//
// All readers are pure: they take a data directory and return the
// records they find, with malformed-line skipping so a partial-write
// at process crash doesn't take out the whole read. No actor, no
// goroutines, no locks — caller serialises if it needs to.
//
// See `doc/roadmap/shipped/remembrancer.md` for the architecture
// rationale and V1 scope.
package remembrancer

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/seamus-brady/retainer/internal/cbr"
	"github.com/seamus-brady/retainer/internal/librarian"
)

// scannerBufferMax is the per-line cap for the JSONL readers.
// Cases carry 384-dim float32 embeddings → ~3KB serialised; bumping
// to 4MB gives plenty of headroom for narrative entries that grow
// future fields.
const scannerBufferMax = 4 * 1024 * 1024

// dailyFilePattern matches the date-rotated JSONL filenames the
// narrative + facts stores produce. Capture group 1 is the
// YYYY-MM-DD date, used for in-window filtering before we open
// the file.
var dailyFilePattern = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})(?:-facts)?\.jsonl$`)

// ReadNarrative walks every narrative JSONL file under
// `<dataDir>/narrative/` whose date falls in [start, end] and
// returns the entries in chronological order (oldest first).
//
// `start` / `end` zero-valued means "no bound on that side". Both
// zero returns every entry on disk. Inclusive on both ends — an
// entry at exactly `end` is included.
//
// Missing dir → empty result, no error (fresh workspace). Malformed
// lines are skipped with a debug log. Returns an error only on
// filesystem errors that suggest the dir itself is unreadable.
func ReadNarrative(dataDir string, start, end time.Time, logger *slog.Logger) ([]librarian.NarrativeEntry, error) {
	if logger == nil {
		logger = slog.Default()
	}
	dir := filepath.Join(dataDir, librarian.NarrativeSubdir)

	files, err := relevantDailyFiles(dir, start, end)
	if err != nil {
		return nil, err
	}

	var out []librarian.NarrativeEntry
	for _, path := range files {
		entries, err := decodeNarrativeFile(path, logger)
		if err != nil {
			return nil, err
		}
		// In-file filter — same-day files can hold entries above /
		// below the per-day window when start/end are mid-day.
		for _, e := range entries {
			if !inRange(e.Timestamp, start, end) {
				continue
			}
			out = append(out, e)
		}
	}
	// Daily files come back in alphabetical (= chronological) order;
	// in-file order is append-only so already chronological. No
	// further sort needed.
	return out, nil
}

// ReadFacts walks every fact JSONL file under `<dataDir>/facts/`
// in [start, end] and returns ALL records — every write, supersede,
// and clear, in chronological order. Callers that want "current
// value per key" run their own most-recent-wins reduction.
//
// Same conventions as ReadNarrative: missing dir → empty, malformed
// lines skipped.
func ReadFacts(dataDir string, start, end time.Time, logger *slog.Logger) ([]librarian.Fact, error) {
	if logger == nil {
		logger = slog.Default()
	}
	dir := filepath.Join(dataDir, librarian.FactsSubdir)

	files, err := relevantDailyFiles(dir, start, end)
	if err != nil {
		return nil, err
	}

	var out []librarian.Fact
	for _, path := range files {
		facts, err := decodeFactsFile(path, logger)
		if err != nil {
			return nil, err
		}
		for _, f := range facts {
			if !inRange(f.Timestamp, start, end) {
				continue
			}
			out = append(out, f)
		}
	}
	return out, nil
}

// ReadCases reads every case from `<dataDir>/cases/cases.jsonl`. The
// case store is a single file (not date-rotated), so this is a
// simpler reader than ReadNarrative / ReadFacts.
//
// Returns the LATEST record per case ID — curation tools (suppress,
// boost) append new records that supersede older ones, and replay
// semantics in `librarian.casesStore` are last-record-wins. We
// mirror that here so deep-archive reads agree with what the
// CaseBase serves.
func ReadCases(dataDir string, logger *slog.Logger) ([]cbr.Case, error) {
	if logger == nil {
		logger = slog.Default()
	}
	path := filepath.Join(dataDir, cbr.SubDir, "cases.jsonl")

	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("remembrancer: open cases: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), scannerBufferMax)

	// Map ID → case so later writes override earlier (last wins).
	bucket := make(map[string]cbr.Case)
	order := make([]string, 0, 32)

	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var c cbr.Case
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			logger.Debug("remembrancer: skipping malformed case line", "line", lineNo, "err", err)
			continue
		}
		if _, ok := bucket[c.ID]; !ok {
			order = append(order, c.ID)
		}
		bucket[c.ID] = c
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("remembrancer: scan cases: %w", err)
	}

	out := make([]cbr.Case, 0, len(order))
	for _, id := range order {
		out = append(out, bucket[id])
	}
	return out, nil
}

// relevantDailyFiles returns the sorted absolute paths of JSONL files
// in `dir` whose embedded date falls in [start, end]. Files whose
// names don't match the date pattern are skipped silently — operator
// drops + tooling artifacts (`.tmp`, etc) shouldn't break a read.
func relevantDailyFiles(dir string, start, end time.Time) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("remembrancer: read %s: %w", dir, err)
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := dailyFilePattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		if !dateInWindow(m[1], start, end) {
			continue
		}
		paths = append(paths, filepath.Join(dir, e.Name()))
	}
	// Alphabetical sort = chronological by the YYYY-MM-DD prefix.
	for i := 0; i < len(paths); i++ {
		for j := i + 1; j < len(paths); j++ {
			if paths[i] > paths[j] {
				paths[i], paths[j] = paths[j], paths[i]
			}
		}
	}
	return paths, nil
}

// dateInWindow reports whether the YYYY-MM-DD-named file's date
// could intersect [start, end]. The file holds a full day's
// records, so we compare the calendar date against truncated bounds:
// a file is relevant if its date is on or after start's date AND on
// or before end's date.
func dateInWindow(dateStr string, start, end time.Time) bool {
	fileDate, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		return false
	}
	if !start.IsZero() {
		startDay := start.Truncate(24 * time.Hour)
		if fileDate.Before(startDay.AddDate(0, 0, -1)) {
			// File ends before start - 1 day = file's last possible
			// timestamp (end of file's UTC day) is before start.
			return false
		}
		// Use day-equal comparison — file contains [day, day+1)
		// timestamps; if fileDate < startDay's date, all entries
		// predate start.
		_, fM, fD := fileDate.Date()
		_, sM, sD := start.Date()
		fY := fileDate.Year()
		sY := start.Year()
		if fY < sY || (fY == sY && fM < sM) || (fY == sY && fM == sM && fD < sD) {
			return false
		}
	}
	if !end.IsZero() {
		fY := fileDate.Year()
		_, fM, fD := fileDate.Date()
		eY := end.Year()
		_, eM, eD := end.Date()
		if fY > eY || (fY == eY && fM > eM) || (fY == eY && fM == eM && fD > eD) {
			return false
		}
	}
	return true
}

// inRange reports whether ts ∈ [start, end] (inclusive both ends).
// Zero start / end means "no bound on that side".
func inRange(ts time.Time, start, end time.Time) bool {
	if !start.IsZero() && ts.Before(start) {
		return false
	}
	if !end.IsZero() && ts.After(end) {
		return false
	}
	return true
}

// decodeNarrativeFile reads one JSONL file and returns the entries.
// Lines that fail to decode are skipped with a debug log; an
// underlying I/O error returns up.
func decodeNarrativeFile(path string, logger *slog.Logger) ([]librarian.NarrativeEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("remembrancer: open %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), scannerBufferMax)

	var out []librarian.NarrativeEntry
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var e librarian.NarrativeEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			logger.Debug("remembrancer: skipping malformed narrative line",
				"path", path, "line", lineNo, "err", err)
			continue
		}
		out = append(out, e)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("remembrancer: scan %s: %w", path, err)
	}
	return out, nil
}

// decodeFactsFile mirrors decodeNarrativeFile for the Fact type.
func decodeFactsFile(path string, logger *slog.Logger) ([]librarian.Fact, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("remembrancer: open %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), scannerBufferMax)

	var out []librarian.Fact
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var fact librarian.Fact
		if err := json.Unmarshal([]byte(line), &fact); err != nil {
			logger.Debug("remembrancer: skipping malformed fact line",
				"path", path, "line", lineNo, "err", err)
			continue
		}
		out = append(out, fact)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("remembrancer: scan %s: %w", path, err)
	}
	return out, nil
}
