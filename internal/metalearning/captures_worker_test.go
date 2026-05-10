package metalearning

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seamus-brady/retainer/internal/captures"
)

// fakeCapturesStore is a test stand-in. Captures append calls and
// hands back lookups for any captured ID.
type fakeCapturesStore struct {
	mu      sync.Mutex
	stored  []captures.Capture
	swept   int
	sweepFn func(time.Time, time.Duration) (int, error)
}

func (f *fakeCapturesStore) Append(c captures.Capture) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stored = append(f.stored, c)
	return nil
}

func (f *fakeCapturesStore) LookupByID(id string) (*captures.Capture, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.stored {
		if f.stored[i].ID == id {
			c := f.stored[i]
			return &c, nil
		}
	}
	return nil, nil
}

func (f *fakeCapturesStore) SweepExpired(now time.Time, olderThan time.Duration) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sweepFn != nil {
		n, err := f.sweepFn(now, olderThan)
		f.swept += n
		return n, err
	}
	return 0, nil
}

func writeCapturesCycleLog(t *testing.T, dataDir, date string, lines []string) {
	t.Helper()
	dir := filepath.Join(dataDir, "cycle-log")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, date+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func capturesDeps(t *testing.T, now time.Time) (string, *fakeCapturesStore, Deps) {
	t.Helper()
	dataDir := t.TempDir()
	store := &fakeCapturesStore{}
	return dataDir, store, Deps{
		DataDir:       dataDir,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		NowFn:         func() time.Time { return now },
		CapturesStore: store,
	}
}

func TestCapturesWorker_DetectsCommitmentInRecentReply(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	dataDir, store, deps := capturesDeps(t, now)
	writeCapturesCycleLog(t, dataDir, "2026-05-09", []string{
		`{"type":"cycle_start","cycle_id":"c1","timestamp":"2026-05-09T11:55:00Z","node_type":"cognitive"}`,
		`{"type":"cycle_complete","cycle_id":"c1","timestamp":"2026-05-09T11:55:01Z","status":"complete","text":"On it. I'll send the report by tomorrow."}`,
	})
	if err := CapturesWorker(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	if len(store.stored) < 1 {
		t.Fatalf("expected at least one capture stored; got 0")
	}
	got := store.stored[0]
	if got.SourceCycleID != "c1" {
		t.Errorf("source_cycle_id = %q", got.SourceCycleID)
	}
	if got.Status != captures.StatusPending {
		t.Errorf("status = %q", got.Status)
	}
	if got.Source != captures.SourceAgentSelf {
		t.Errorf("source = %q", got.Source)
	}
}

func TestCapturesWorker_IgnoresAgentSubCycles(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	dataDir, store, deps := capturesDeps(t, now)
	writeCapturesCycleLog(t, dataDir, "2026-05-09", []string{
		`{"type":"cycle_start","cycle_id":"a1","timestamp":"2026-05-09T11:55:00Z","node_type":"agent"}`,
		`{"type":"cycle_complete","cycle_id":"a1","timestamp":"2026-05-09T11:55:01Z","status":"complete","text":"I'll check the inbox."}`,
	})
	if err := CapturesWorker(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	if len(store.stored) != 0 {
		t.Errorf("agent sub-cycle should not generate captures; got %d", len(store.stored))
	}
}

func TestCapturesWorker_RespectsScanWindow(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	dataDir, store, deps := capturesDeps(t, now)
	// Older than CapturesScanWindow (30 min) — outside window.
	writeCapturesCycleLog(t, dataDir, "2026-05-09", []string{
		`{"type":"cycle_start","cycle_id":"old","timestamp":"2026-05-09T11:00:00Z","node_type":"cognitive"}`,
		`{"type":"cycle_complete","cycle_id":"old","timestamp":"2026-05-09T11:00:01Z","status":"complete","text":"I'll send it tomorrow."}`,
	})
	if err := CapturesWorker(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	if len(store.stored) != 0 {
		t.Errorf("cycle outside window should not produce captures; got %d", len(store.stored))
	}
}

func TestCapturesWorker_DedupesAcrossRuns(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	dataDir, store, deps := capturesDeps(t, now)
	writeCapturesCycleLog(t, dataDir, "2026-05-09", []string{
		`{"type":"cycle_start","cycle_id":"c1","timestamp":"2026-05-09T11:55:00Z","node_type":"cognitive"}`,
		`{"type":"cycle_complete","cycle_id":"c1","timestamp":"2026-05-09T11:55:01Z","status":"complete","text":"I'll send the report by tomorrow."}`,
	})
	if err := CapturesWorker(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	first := len(store.stored)
	if first < 1 {
		t.Fatalf("first run produced 0 captures")
	}
	// Second run on the same cycle should not duplicate the same
	// (cycle_id + phrase + offset) capture.
	if err := CapturesWorker(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	if len(store.stored) != first {
		t.Errorf("dedupe failed: first run %d, second run %d", first, len(store.stored))
	}
}

func TestCapturesWorker_NoStoreSkipsCleanly(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	dataDir, _, deps := capturesDeps(t, now)
	deps.CapturesStore = nil
	writeCapturesCycleLog(t, dataDir, "2026-05-09", []string{
		`{"type":"cycle_start","cycle_id":"c1","timestamp":"2026-05-09T11:55:00Z","node_type":"cognitive"}`,
		`{"type":"cycle_complete","cycle_id":"c1","timestamp":"2026-05-09T11:55:01Z","status":"complete","text":"I'll send it."}`,
	})
	// Should not panic, should not return an error.
	if err := CapturesWorker(context.Background(), deps); err != nil {
		t.Fatalf("worker should tolerate nil store; got %v", err)
	}
}

func TestCapturesWorker_FreshWorkspaceNoOp(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	dataDir, store, deps := capturesDeps(t, now)
	if err := CapturesWorker(context.Background(), deps); err != nil {
		t.Fatalf("fresh workspace should succeed; got %v", err)
	}
	if len(store.stored) != 0 {
		t.Errorf("fresh workspace produced %d captures", len(store.stored))
	}
	// Verify cycle-log dir didn't get created as a side effect.
	if _, err := os.Stat(filepath.Join(dataDir, "cycle-log")); !os.IsNotExist(err) {
		t.Errorf("worker should not create cycle-log dir on fresh workspace")
	}
}

func TestCapturesWorker_RunsExpirySweepWhenDue(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	dataDir, store, deps := capturesDeps(t, now)
	store.sweepFn = func(_ time.Time, _ time.Duration) (int, error) { return 3, nil }
	if err := CapturesWorker(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	if store.swept != 3 {
		t.Errorf("expected sweep to record 3 expirations; got %d", store.swept)
	}
	// Sweep marker should now exist.
	if _, err := os.Stat(filepath.Join(dataDir, capturesSweepStateFile)); err != nil {
		t.Errorf("sweep state file missing after sweep: %v", err)
	}
}

func TestCapturesWorker_SkipsExpirySweepWhenRecent(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	dataDir, store, deps := capturesDeps(t, now)
	store.sweepFn = func(_ time.Time, _ time.Duration) (int, error) { return 1, nil }
	// Pre-touch the marker so the worker thinks the sweep ran an hour ago.
	marker := filepath.Join(dataDir, capturesSweepStateFile)
	if err := os.WriteFile(marker, []byte("recent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(marker, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := CapturesWorker(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	if store.swept != 0 {
		t.Errorf("recent sweep should skip; got %d expirations", store.swept)
	}
}
