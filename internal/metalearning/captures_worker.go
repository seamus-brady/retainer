package metalearning

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/seamus-brady/retainer/internal/captures"
	"github.com/seamus-brady/retainer/internal/cyclelog"
)

// CapturesWorkerInterval is how often the captures worker runs.
// 10 minutes balances responsiveness (a commitment made now is
// surfaced in the next sensorium block <=10 minutes later) against
// the cost of repeatedly walking the cycle log.
//
// SD's scanner runs post-cycle (immediate) but uses an LLM call.
// The Retainer heuristic is fast enough that a worker tick is
// fine for v1.0; the LLM upgrade in v1.1 may move to post-cycle
// for the same reason SD did.
const CapturesWorkerInterval = 10 * time.Minute

// CapturesScanWindow is how far back the worker scans cycle logs
// for commitment phrases. Longer than the worker interval so a
// missed run picks up the cycles it would have scanned. The
// scanner is idempotent on (cycle_id + phrase + offset), so
// re-scanning the same cycle is cheap and safe.
const CapturesScanWindow = 30 * time.Minute

// CapturesExpirySweepEvery is how often the worker also runs the
// expiry sweep. Running the sweep on every tick (every 10 min)
// is wasteful when the bar is "older than 7 days"; once a day is
// plenty. The worker tracks last-sweep-at via the metalearning
// pool's per-worker LastRunAt is the WORKER's last run, not the
// sweep's — so we use a sub-cadence: only sweep if the last sweep
// was >24h ago. Persisted via the same state file.
const CapturesExpirySweepEvery = 24 * time.Hour

// CapturesStore is the narrow seam the worker uses to persist new
// captures and run the expiry sweep. Defined here so the
// metalearning package stays decoupled from the captures package's
// concrete types beyond what the worker actually needs.
//
// *captures.Store satisfies this interface via Go's structural
// typing.
type CapturesStore interface {
	Append(c captures.Capture) error
	LookupByID(id string) (*captures.Capture, error)
	SweepExpired(now time.Time, olderThan time.Duration) (int, error)
}

// CapturesScanner extends Deps with the seam Captures needs. The
// pool wires this through Config.CapturesStore.
//
// Optional — workers tolerate nil and skip persistence.
var capturesStoreFromConfig CapturesStore // set by Pool.Run via deps; injected at wire-time

// CapturesWorker is the metalearning worker that scans recent
// cycle logs for the commitment phrases listed in
// captures/scanner.go and persists each new match. Also runs the
// expiry sweep once per 24 hours.
//
// Operates as a closure over the deps' captures store handle so
// the worker registry's RunFn signature stays stable.
//
// Reasoning for putting this in metalearning rather than the cog:
// the metalearning pool already owns the off-cog batch-scan model.
// SD does post-cycle scanning because its scanner is LLM-driven
// (latency matters less than detection precision); our heuristic
// is precision-deficient anyway, so the cadence trade-off favours
// keeping it off the cog's hot path.
func CapturesWorker(ctx context.Context, deps Deps) error {
	if deps.DataDir == "" {
		return errors.New("captures worker: DataDir empty")
	}
	if deps.CapturesStore == nil {
		// Tests that don't wire a store still exercise the
		// scanner; production always wires *captures.Store.
		deps.Logger.Warn("captures worker: no store wired; skipping")
		return nil
	}
	now := deps.NowFn()

	// 1. Scan recent cycle log for new commitments.
	cycleDir := filepath.Join(deps.DataDir, "cycle-log")
	scanned, found, err := scanCycleLogsForCaptures(ctx, cycleDir, now.Add(-CapturesScanWindow), now, deps.CapturesStore)
	if err != nil {
		return fmt.Errorf("captures worker: scan: %w", err)
	}

	// 2. Run expiry sweep at most once per CapturesExpirySweepEvery.
	expired := 0
	if shouldSweepExpiry(deps.DataDir, now) {
		n, err := deps.CapturesStore.SweepExpired(now, captures.DefaultExpiryDays*24*time.Hour)
		if err != nil {
			deps.Logger.Warn("captures worker: expiry sweep failed",
				"err", err, "now", now)
		} else {
			expired = n
			recordSweepTime(deps.DataDir, now)
		}
	}

	deps.Logger.Info("captures worker ran",
		"cycles_scanned", scanned,
		"new_captures", found,
		"expired", expired,
	)
	return nil
}

// scanCycleLogsForCaptures replays cog-cycle reply text from the
// cycle log within the window and asks the captures scanner to
// extract commitments from each reply. New captures (deduped via
// content-addressed ID) are appended to the store.
//
// Returns (cyclesScanned, newCapturesAppended, err).
func scanCycleLogsForCaptures(
	ctx context.Context,
	cycleDir string,
	since, _ time.Time,
	store CapturesStore,
) (int, int, error) {
	entries, err := os.ReadDir(cycleDir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}

	// Newest-day-first so we don't open files entirely outside
	// the window.
	var dayFiles []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasSuffix(n, ".jsonl") {
			continue
		}
		base := strings.TrimSuffix(n, ".jsonl")
		if len(base) < 10 || base[4] != '-' || base[7] != '-' {
			continue
		}
		dayFiles = append(dayFiles, n)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dayFiles)))

	type pending struct {
		startedAt time.Time
		text      string
		nodeType  string
	}
	cycles := map[string]*pending{}

	for _, fname := range dayFiles {
		datePart := strings.TrimSuffix(fname, ".jsonl")
		fileDay, err := time.Parse("2006-01-02", datePart)
		if err == nil && fileDay.Add(24*time.Hour).Before(since) {
			break
		}
		path := filepath.Join(cycleDir, fname)
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		s := bufio.NewScanner(f)
		s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for s.Scan() {
			line := s.Bytes()
			if len(line) == 0 {
				continue
			}
			var ev cyclelog.Event
			if err := json.Unmarshal(line, &ev); err != nil {
				continue
			}
			c, ok := cycles[ev.CycleID]
			if !ok {
				c = &pending{}
				cycles[ev.CycleID] = c
			}
			switch ev.Type {
			case cyclelog.EventCycleStart:
				c.nodeType = string(ev.NodeType)
				c.startedAt = ev.Timestamp
			case cyclelog.EventCycleComplete:
				c.text = ev.Text
			}
		}
		f.Close()
	}

	scanned := 0
	appended := 0
	for cycleID, p := range cycles {
		if ctx.Err() != nil {
			return scanned, appended, ctx.Err()
		}
		// Scope to cog cycles only — agent sub-cycles
		// shouldn't generate operator-facing captures (the cog
		// already owns the user-facing reply).
		if p.nodeType != "" && p.nodeType != "cognitive" {
			continue
		}
		if p.text == "" {
			continue
		}
		if p.startedAt.Before(since) {
			continue
		}
		scanned++
		matches := captures.ScanReply(cycleID, p.text, p.startedAt)
		for _, c := range matches {
			// Dedupe — if the store already has this ID, skip.
			existing, err := store.LookupByID(c.ID)
			if err != nil {
				continue
			}
			if existing != nil {
				continue
			}
			if err := store.Append(c); err == nil {
				appended++
			}
		}
	}
	return scanned, appended, nil
}

// capturesSweepStateFile is the per-workspace marker that records
// when the expiry sweep last ran. Atomic-write via os.Rename so
// the worker can be killed mid-run without corrupting state.
const capturesSweepStateFile = ".captures-sweep-state"

// shouldSweepExpiry returns true when the marker file's mtime is
// older than CapturesExpirySweepEvery (or absent). Cheap; no
// lock contention.
func shouldSweepExpiry(dataDir string, now time.Time) bool {
	path := filepath.Join(dataDir, capturesSweepStateFile)
	info, err := os.Stat(path)
	if err != nil {
		return true // missing file = never swept = sweep now
	}
	return now.Sub(info.ModTime()) >= CapturesExpirySweepEvery
}

// recordSweepTime touches the marker file with `now`'s mtime so
// the next shouldSweepExpiry returns false until 24h passes.
// Errors are logged-and-swallowed by the caller — a failed touch
// just causes one extra sweep, which is idempotent.
func recordSweepTime(dataDir string, now time.Time) {
	path := filepath.Join(dataDir, capturesSweepStateFile)
	tmp := path + ".tmp"
	_ = os.WriteFile(tmp, []byte(now.UTC().Format(time.RFC3339)+"\n"), 0o644)
	_ = os.Rename(tmp, path)
	_ = os.Chtimes(path, now, now)
}
