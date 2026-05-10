package scheduler

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// logFilename is the single append-only JSONL log of operations.
// One file matches the simplicity of the taskwarrior store —
// daily-rotation isn't worth the lookup cost for a typical
// scheduler with tens of jobs over a workspace's lifetime.
const logFilename = "jobs.jsonl"

// loadOps replays the operations log into a slice of Ops in
// append order. Returns an empty slice when the file doesn't
// exist (fresh workspace). Malformed lines are skipped with a
// note; a single corrupt line shouldn't take out replay.
func loadOps(dir string) ([]Op, error) {
	path := filepath.Join(dir, logFilename)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("scheduler: open log: %w", err)
	}
	defer f.Close()

	var ops []Op
	s := bufio.NewScanner(f)
	// Lift the line-length cap: Job.Prompt can be moderately long.
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for s.Scan() {
		line := s.Bytes()
		if len(line) == 0 {
			continue
		}
		var op Op
		if err := json.Unmarshal(line, &op); err != nil {
			// Skip malformed lines silently — the next replay
			// will re-derive state from whatever's parseable.
			continue
		}
		ops = append(ops, op)
	}
	if err := s.Err(); err != nil {
		return ops, fmt.Errorf("scheduler: scan log: %w", err)
	}
	return ops, nil
}

// appendOp writes one operation as a JSONL line. Creates the
// directory + file on first use. Each call opens + closes the
// file because operations are infrequent (manual create / cron-
// tick fire) and the simplicity beats keeping a long-lived FD.
func appendOp(dir string, op Op) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("scheduler: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, logFilename)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("scheduler: open log for append: %w", err)
	}
	defer f.Close()

	line, err := json.Marshal(op)
	if err != nil {
		return fmt.Errorf("scheduler: marshal op: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("scheduler: write op: %w", err)
	}
	return nil
}

// resolveJobs reduces an op log into the current set of jobs,
// keyed by ID. Order matters: OpCreated establishes the record;
// OpFired bumps counters; OpCancelled / OpCompleted flip Active
// off. An OpFired against an unknown ID is silently ignored
// (defensive — a corrupt log shouldn't crash).
func resolveJobs(ops []Op) map[string]*Job {
	jobs := map[string]*Job{}
	for _, op := range ops {
		switch op.Kind {
		case OpCreated:
			if op.Job == nil {
				continue
			}
			j := *op.Job
			j.Active = true
			jobs[j.ID] = &j
		case OpFired:
			j, ok := jobs[op.JobID]
			if !ok {
				continue
			}
			j.LastFiredAt = op.Timestamp
			j.FiredCount++
		case OpCompleted:
			j, ok := jobs[op.JobID]
			if !ok {
				continue
			}
			j.Active = false
			if j.LastFiredAt.IsZero() {
				j.LastFiredAt = op.Timestamp
			}
			j.FiredCount++
		case OpCancelled:
			j, ok := jobs[op.JobID]
			if !ok {
				continue
			}
			j.Active = false
		}
	}
	return jobs
}
