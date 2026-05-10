package metalearning

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seamus-brady/retainer/internal/cbr"
	"github.com/seamus-brady/retainer/internal/librarian"
)

func TestWeeklyConsolidation_EmptyWeekWritesNoReport(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	deps := Deps{DataDir: dir, Logger: discardLogger(), NowFn: fixedClock(now)}
	if err := WeeklyConsolidation(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	// No reports dir + no consolidation log entries.
	reportsDir := filepath.Join(dir, "knowledge", "consolidation")
	if entries, _ := os.ReadDir(reportsDir); len(entries) > 0 {
		t.Errorf("empty week should write no report; got %d files", len(entries))
	}
}

func TestWeeklyConsolidation_WritesReportWithCounts(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	weekAgo := now.Add(-3 * 24 * time.Hour)

	// Seed a narrative entry inside the window.
	seedNarrative(t, dir, weekAgo, []librarian.NarrativeEntry{
		{
			CycleID:   "cyc-1",
			Timestamp: weekAgo,
			Summary:   "investigated the auth login flow",
			Intent:    librarian.Intent{Domain: "auth"},
		},
	})
	// Seed a case inside the window.
	seedCases(t, dir, []cbr.Case{
		{
			ID:        "case-1",
			Timestamp: weekAgo,
			Problem:   cbr.Problem{Intent: "debug auth", Domain: "auth"},
			Outcome:   cbr.Outcome{Status: cbr.StatusSuccess, Confidence: 0.85},
		},
	})

	deps := Deps{DataDir: dir, Logger: discardLogger(), NowFn: fixedClock(now)}
	if err := WeeklyConsolidation(context.Background(), deps); err != nil {
		t.Fatal(err)
	}

	// Report file landed under data/knowledge/consolidation/.
	reportsDir := filepath.Join(dir, "knowledge", "consolidation")
	entries, err := os.ReadDir(reportsDir)
	if err != nil {
		t.Fatalf("read reports dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 report, got %d", len(entries))
	}

	bodyBytes, err := os.ReadFile(filepath.Join(reportsDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	body := string(bodyBytes)
	for _, want := range []string{
		"# Weekly review",
		"## Counts",
		"narrative entries",
		"facts",
		"cases",
		"## Top topics",
		"auth:",
		"investigated the auth login flow",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q in:\n%s", want, body)
		}
	}
}

func TestCasesInWindow_FiltersByTimestamp(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	start := now.Add(-7 * 24 * time.Hour)
	in := []cbr.Case{
		{ID: "before", Timestamp: now.Add(-30 * 24 * time.Hour)},
		{ID: "inside", Timestamp: now.Add(-3 * 24 * time.Hour)},
		{ID: "after", Timestamp: now.Add(24 * time.Hour)},
		{ID: "no-ts"},
	}
	out := casesInWindow(in, start, now)
	if len(out) != 1 || out[0].ID != "inside" {
		t.Errorf("got %+v, want only 'inside'", out)
	}
}

func TestTopTopics_OrdersByCountDesc(t *testing.T) {
	es := []librarian.NarrativeEntry{
		{Intent: librarian.Intent{Domain: "auth"}},
		{Intent: librarian.Intent{Domain: "auth"}},
		{Intent: librarian.Intent{Domain: "research"}},
		{Intent: librarian.Intent{Domain: ""}}, // skipped
	}
	got := topTopics(es, 5)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].label != "auth" || got[0].count != 2 {
		t.Errorf("top = %+v", got[0])
	}
	if got[1].label != "research" || got[1].count != 1 {
		t.Errorf("second = %+v", got[1])
	}
}

func TestTopSampleEntries_ReturnsMostRecent(t *testing.T) {
	now := time.Now()
	es := []librarian.NarrativeEntry{
		{Timestamp: now.Add(-3 * time.Hour), Summary: "old"},
		{Timestamp: now.Add(-2 * time.Hour), Summary: ""}, // skipped
		{Timestamp: now.Add(-time.Hour), Summary: "newer"},
		{Timestamp: now, Summary: "newest"},
	}
	got := topSampleEntries(es, 2)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Summary != "newest" || got[1].Summary != "newer" {
		t.Errorf("got = %+v", got)
	}
}

func TestOneLine_CollapsesAndTruncates(t *testing.T) {
	got := oneLine("multi\n\nline\n  text", 100)
	if got != "multi line text" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("x", 50)
	got = oneLine(long, 10)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected truncation suffix; got %q", got)
	}
}

// seedNarrative writes one or more entries to data/narrative/<date>.jsonl.
// remembrancer.ReadNarrative scans the per-date files in the window.
func seedNarrative(t *testing.T, dataDir string, date time.Time, entries []librarian.NarrativeEntry) {
	t.Helper()
	dir := filepath.Join(dataDir, "narrative")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, date.Format("2006-01-02")+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}
