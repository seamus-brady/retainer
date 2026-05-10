package remembrancer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- WriteRun ----

func TestWriteRun_AppendsToDailyJSONL(t *testing.T) {
	dir := t.TempDir()
	run := Run{
		Period:     "ad-hoc",
		StartDate:  "2026-04-01",
		EndDate:    "2026-04-07",
		ReportPath: "knowledge/consolidation/2026-04-07-weekly.md",
		Stats:      RunStats{NarrativeEntries: 10, Facts: 3, Cases: 4},
	}
	written, err := WriteRun(dir, run)
	if err != nil {
		t.Fatalf("WriteRun: %v", err)
	}
	if written.ID == "" {
		t.Error("ID should be auto-assigned")
	}
	if written.Timestamp.IsZero() {
		t.Error("Timestamp should be auto-assigned")
	}

	// The day file should exist and contain one record.
	dayKey := written.Timestamp.UTC().Format("2006-01-02")
	path := filepath.Join(dir, ConsolidationSubdir, dayKey+".jsonl")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) != 1 {
		t.Errorf("expected 1 line, got %d", len(lines))
	}
	var got Run
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != written.ID {
		t.Errorf("ID mismatch: %s != %s", got.ID, written.ID)
	}
}

func TestWriteRun_AppendsMultipleRunsSameDay(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for i := 0; i < 3; i++ {
		_, err := WriteRun(dir, Run{
			Timestamp:  now,
			Period:     "ad-hoc",
			StartDate:  "2026-04-01",
			EndDate:    "2026-04-01",
			ReportPath: "p" + itoa(i),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	dayKey := now.UTC().Format("2006-01-02")
	body, _ := os.ReadFile(filepath.Join(dir, ConsolidationSubdir, dayKey+".jsonl"))
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) != 3 {
		t.Errorf("got %d lines, want 3", len(lines))
	}
}

func TestWriteRun_RejectsInvalidPeriod(t *testing.T) {
	if _, err := WriteRun(t.TempDir(), Run{
		Period:     "yearly", // not in validPeriods
		StartDate:  "2026-04-01",
		EndDate:    "2026-04-07",
		ReportPath: "x",
	}); err == nil {
		t.Error("invalid period should error")
	}
}

func TestWriteRun_RejectsBadDateFormat(t *testing.T) {
	if _, err := WriteRun(t.TempDir(), Run{
		Period:     "weekly",
		StartDate:  "April 1 2026",
		EndDate:    "2026-04-07",
		ReportPath: "x",
	}); err == nil {
		t.Error("bad start_date should error")
	}
}

func TestWriteRun_RejectsEmptyReportPath(t *testing.T) {
	if _, err := WriteRun(t.TempDir(), Run{
		Period:    "weekly",
		StartDate: "2026-04-01",
		EndDate:   "2026-04-07",
	}); err == nil {
		t.Error("empty report_path should error")
	}
}

// ---- WriteReport ----

func TestWriteReport_HappyPath(t *testing.T) {
	dir := t.TempDir()
	rel, err := WriteReport(dir, "2026-04-07", "weekly-cbr", "# Weekly CBR\n\nbody")
	if err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(KnowledgeSubdir, ReportsSubdir, "2026-04-07-weekly-cbr.md")
	if rel != expected {
		t.Errorf("rel = %q, want %q", rel, expected)
	}
	abs := filepath.Join(dir, rel)
	body, err := os.ReadFile(abs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "Weekly CBR") {
		t.Errorf("body wrong: %q", body)
	}
}

func TestWriteReport_RejectsEmptyBody(t *testing.T) {
	if _, err := WriteReport(t.TempDir(), "2026-04-07", "x", ""); err == nil {
		t.Error("empty body should error")
	}
}

func TestWriteReport_RejectsBadSlug(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{
		"",
		"With Spaces",
		"UpperCase",
		"path/traversal",
		"../escape",
		"trailing-",
		"-leading",
		"double--hyphen",
	} {
		if _, err := WriteReport(dir, "2026-04-07", bad, "body"); err == nil {
			t.Errorf("slug %q should be rejected", bad)
		}
	}
}

func TestWriteReport_RejectsBadDate(t *testing.T) {
	if _, err := WriteReport(t.TempDir(), "April 7", "weekly", "body"); err == nil {
		t.Error("bad date format should error")
	}
}

func TestWriteReport_RefusesToClobberExistingFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteReport(dir, "2026-04-07", "x", "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteReport(dir, "2026-04-07", "x", "second"); err == nil {
		t.Error("second write should refuse to clobber")
	}
}

// ---- SlugifyTitle ----

func TestSlugifyTitle_Cases(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Weekly CBR Review", "weekly-cbr-review"},
		{"Auth refactor!", "auth-refactor"},
		{"  spaces  everywhere  ", "spaces-everywhere"},
		{"all-good-already", "all-good-already"},
		{"!!!", ""},
		{"unicode café", "unicode-caf"}, // strips non-ASCII (not strictly required but tested for stability)
	}
	for _, c := range cases {
		got := SlugifyTitle(c.in)
		if got != c.want {
			t.Errorf("Slugify(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
