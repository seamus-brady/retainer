package transcript

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seamus-brady/retainer/internal/cyclelog"
)

// writeJSONL is a small helper that writes one cycle-log
// fixture file at <dir>/<date>.jsonl with the supplied events.
func writeJSONL(t *testing.T, dir, date string, events []cyclelog.Event) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, date+".jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, ev := range events {
		body, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(append(body, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadDir_NewestFirst(t *testing.T) {
	dir := t.TempDir()
	for _, date := range []string{"2026-05-07", "2026-05-09", "2026-05-08"} {
		writeJSONL(t, dir, date, []cyclelog.Event{{Type: cyclelog.EventCycleStart, CycleID: "x", Text: "hi"}})
	}
	got, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	want := []string{"2026-05-09", "2026-05-08", "2026-05-07"}
	if len(got) != len(want) {
		t.Fatalf("len = %d; want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLoadDir_FreshWorkspaceReturnsEmpty(t *testing.T) {
	got, err := LoadDir(filepath.Join(t.TempDir(), "nonexistent"))
	if err != nil {
		t.Errorf("expected nil err on missing dir, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %v", got)
	}
}

func TestLoadDir_IgnoresNonDateFiles(t *testing.T) {
	dir := t.TempDir()
	writeJSONL(t, dir, "2026-05-09", nil)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "garbage.jsonl"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _ := LoadDir(dir)
	if len(got) != 1 || got[0] != "2026-05-09" {
		t.Errorf("expected 2026-05-09 only, got %v", got)
	}
}

func TestLoadDay_PairsStartAndComplete(t *testing.T) {
	dir := t.TempDir()
	t1 := time.Date(2026, 5, 9, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Second)
	writeJSONL(t, dir, "2026-05-09", []cyclelog.Event{
		{Type: cyclelog.EventCycleStart, CycleID: "c1", Timestamp: t1, Text: "hello", NodeType: "cog"},
		{Type: cyclelog.EventCycleComplete, CycleID: "c1", Timestamp: t2, Text: "hi there", Status: cyclelog.StatusComplete},
	})
	turns, err := LoadDay(dir, "2026-05-09")
	if err != nil {
		t.Fatalf("LoadDay: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(turns))
	}
	if turns[0].Role != RoleUser || turns[0].Text != "hello" {
		t.Errorf("user turn wrong: %+v", turns[0])
	}
	if turns[1].Role != RoleAssistant || turns[1].Text != "hi there" {
		t.Errorf("assistant turn wrong: %+v", turns[1])
	}
	if turns[0].CycleID != turns[1].CycleID {
		t.Errorf("cycle ids should match across paired turns")
	}
}

func TestLoadDay_PreservesChronologicalOrder(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 5, 9, 10, 0, 0, 0, time.UTC)
	writeJSONL(t, dir, "2026-05-09", []cyclelog.Event{
		{Type: cyclelog.EventCycleStart, CycleID: "c1", Timestamp: base, Text: "first"},
		{Type: cyclelog.EventCycleComplete, CycleID: "c1", Timestamp: base.Add(time.Second), Text: "first reply", Status: cyclelog.StatusComplete},
		{Type: cyclelog.EventCycleStart, CycleID: "c2", Timestamp: base.Add(2 * time.Second), Text: "second"},
		{Type: cyclelog.EventCycleComplete, CycleID: "c2", Timestamp: base.Add(3 * time.Second), Text: "second reply", Status: cyclelog.StatusComplete},
	})
	turns, _ := LoadDay(dir, "2026-05-09")
	if len(turns) != 4 {
		t.Fatalf("turns = %d, want 4", len(turns))
	}
	want := []string{"first", "first reply", "second", "second reply"}
	for i := range want {
		if turns[i].Text != want[i] {
			t.Errorf("[%d] %q, want %q", i, turns[i].Text, want[i])
		}
	}
}

func TestLoadDay_AbandonedCycleSurfacesPlaceholder(t *testing.T) {
	dir := t.TempDir()
	writeJSONL(t, dir, "2026-05-09", []cyclelog.Event{
		{Type: cyclelog.EventCycleStart, CycleID: "c1", Timestamp: time.Now(), Text: "starting"},
		// no matching cycle_complete — cog crashed mid-cycle
	})
	turns, _ := LoadDay(dir, "2026-05-09")
	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2 (user + abandoned-placeholder)", len(turns))
	}
	if turns[1].Role != RoleError {
		t.Errorf("expected error role for abandoned cycle, got %v", turns[1].Role)
	}
	if !strings.Contains(turns[1].Text, "did not complete") {
		t.Errorf("placeholder text missing: %q", turns[1].Text)
	}
}

func TestLoadDay_ErrorCycleSurfacesAsError(t *testing.T) {
	dir := t.TempDir()
	writeJSONL(t, dir, "2026-05-09", []cyclelog.Event{
		{Type: cyclelog.EventCycleStart, CycleID: "c1", Timestamp: time.Now(), Text: "tricky question"},
		{Type: cyclelog.EventCycleComplete, CycleID: "c1", Timestamp: time.Now(), Status: cyclelog.StatusError, Error: "rate limit hit"},
	})
	turns, _ := LoadDay(dir, "2026-05-09")
	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(turns))
	}
	if turns[1].Role != RoleError {
		t.Errorf("expected error role, got %v", turns[1].Role)
	}
}

func TestLoadDay_SkipsAgentSubCycles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeJSONL(t, dir, "2026-05-09", []cyclelog.Event{
		{Type: cyclelog.EventCycleStart, CycleID: "c1", Timestamp: now, Text: "what's the weather", NodeType: "cog"},
		{Type: cyclelog.EventAgentCycleStart, CycleID: "agent-1", Timestamp: now, NodeType: "agent"},
		{Type: cyclelog.EventAgentCycleComplete, CycleID: "agent-1", Timestamp: now},
		{Type: cyclelog.EventCycleComplete, CycleID: "c1", Timestamp: now, Text: "warm and sunny", Status: cyclelog.StatusComplete},
	})
	turns, _ := LoadDay(dir, "2026-05-09")
	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2 (agent sub-cycles excluded)", len(turns))
	}
}

func TestLoadDay_LegacyEntriesWithoutTextSkip(t *testing.T) {
	dir := t.TempDir()
	writeJSONL(t, dir, "2026-05-09", []cyclelog.Event{
		// Pre-text-field cycle log
		{Type: cyclelog.EventCycleStart, CycleID: "c1", Timestamp: time.Now()},
		{Type: cyclelog.EventCycleComplete, CycleID: "c1", Timestamp: time.Now(), Status: cyclelog.StatusComplete},
	})
	turns, _ := LoadDay(dir, "2026-05-09")
	if len(turns) != 0 {
		t.Errorf("expected legacy text-less entries to be skipped, got %d turns", len(turns))
	}
}

func TestLoadDay_RejectsMalformedDate(t *testing.T) {
	_, err := LoadDay(t.TempDir(), "not-a-date")
	if err == nil || !strings.Contains(err.Error(), "invalid date") {
		t.Errorf("expected invalid-date error, got %v", err)
	}
}

func TestLoadDay_MissingFileReturnsEmpty(t *testing.T) {
	turns, err := LoadDay(t.TempDir(), "2099-01-01")
	if err != nil {
		t.Errorf("expected nil err on missing day, got %v", err)
	}
	if len(turns) != 0 {
		t.Errorf("expected empty turns on missing day, got %v", turns)
	}
}

func TestExportMarkdown_RendersDayHeader(t *testing.T) {
	turns := []Turn{
		{Role: RoleUser, Text: "hello", Timestamp: time.Date(2026, 5, 9, 10, 30, 0, 0, time.UTC)},
		{Role: RoleAssistant, Text: "hi back", Timestamp: time.Date(2026, 5, 9, 10, 30, 1, 0, time.UTC)},
	}
	got := string(ExportMarkdown("2026-05-09", "Nemo", turns))
	for _, want := range []string{"# Nemo — 2026-05-09", "operator", "Nemo", "hello", "hi back"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestExportMarkdown_EmptyDayPlaceholder(t *testing.T) {
	got := string(ExportMarkdown("2099-01-01", "Nemo", nil))
	if !strings.Contains(got, "No cycles") {
		t.Errorf("expected placeholder message, got: %s", got)
	}
}

func TestSafeDate_AcceptsValid(t *testing.T) {
	_, canonical, err := SafeDate("2026-05-09")
	if err != nil {
		t.Fatalf("SafeDate: %v", err)
	}
	if canonical != "2026-05-09" {
		t.Errorf("canonical = %q", canonical)
	}
}

func TestSafeDate_RejectsTraversal(t *testing.T) {
	for _, bad := range []string{"../etc/passwd", "2026-05-09/../../foo", "not-a-date", ""} {
		if _, _, err := SafeDate(bad); err == nil {
			t.Errorf("SafeDate(%q) should error", bad)
		}
	}
}
