// Package actorstate is the shared utility for system actors that need to
// persist their cadence state to disk so it survives process restarts.
//
// The pattern (per `project_system_actors_self_tick` memory):
//
//   - Each system actor that does periodic work has a state file at
//     `<workspace>/data/.<actor>-state.json` (gitignored).
//   - Read on startup: actor restores cursor / last-success / failure
//     counts from disk before resuming work.
//   - Atomic write: temp-file + rename so a crash mid-write leaves the
//     previous state.json intact.
//   - Write order: state.json updates AFTER successful action, never
//     before. A crash between action and state-write means one redundant
//     action on restart (harmless). State-write before action would mean
//     a crash could orphan the action.
//
// Today's consumers: housekeeper (last_sweep_at, narrative_entries_pruned).
// Planned consumers: archivist (last_cycle_archived), backup
// (last_success / last_failure / consecutive_failures), remembrancer
// (last_consolidation_cursor), forecaster (last_tick_at).
package actorstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Read reads and decodes a state file into dst (a pointer to the actor's
// state struct). Returns nil error and leaves dst at its zero value when
// the file doesn't exist — that's the fresh-start case (no prior state),
// not a failure. Any other I/O or decode error is returned.
func Read(path string, dst any) error {
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("actorstate: read %s: %w", path, err)
	}
	if len(body) == 0 {
		// Empty file — treat as fresh-start to avoid a confusing decode
		// error. Atomic-write should never produce empty files, but
		// disks fill up.
		return nil
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("actorstate: decode %s: %w", path, err)
	}
	return nil
}

// Write atomically writes the JSON-encoded src to path. Uses temp-file
// + rename so a crash mid-write leaves the previous state.json intact;
// the rename is atomic on POSIX filesystems for paths in the same
// directory.
//
// Creates parent directories as needed (0o755). The state file itself
// gets 0o644 — operator-readable for debugging.
func Write(path string, src any) error {
	body, err := json.MarshalIndent(src, "", "  ")
	if err != nil {
		return fmt.Errorf("actorstate: encode: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("actorstate: mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("actorstate: temp file: %w", err)
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup if anything below fails.
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("actorstate: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("actorstate: close temp: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return fmt.Errorf("actorstate: chmod temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("actorstate: rename %s → %s: %w", tmpPath, path, err)
	}
	return nil
}
