package remembrancer

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/seamus-brady/retainer/internal/cbr"
	"github.com/seamus-brady/retainer/internal/librarian"
)

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// seedNarrativeFile writes the entries to a date-keyed JSONL.
// Used to set up test fixtures without spinning up a real librarian.
func seedNarrativeFile(t *testing.T, dataDir, date string, entries []librarian.NarrativeEntry) {
	t.Helper()
	dir := filepath.Join(dataDir, librarian.NarrativeSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, date+".jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, e := range entries {
		body, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		f.Write(append(body, '\n'))
	}
}

func seedFactsFile(t *testing.T, dataDir, date string, facts []librarian.Fact) {
	t.Helper()
	dir := filepath.Join(dataDir, librarian.FactsSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, date+"-facts.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, fact := range facts {
		body, err := json.Marshal(fact)
		if err != nil {
			t.Fatal(err)
		}
		f.Write(append(body, '\n'))
	}
}

func seedCasesFile(t *testing.T, dataDir string, cases []cbr.Case) {
	t.Helper()
	dir := filepath.Join(dataDir, cbr.SubDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "cases.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, c := range cases {
		body, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		f.Write(append(body, '\n'))
	}
}

// ---- ReadNarrative ----

func TestReadNarrative_MissingDirReturnsEmpty(t *testing.T) {
	out, err := ReadNarrative(t.TempDir(), time.Time{}, time.Time{}, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Errorf("missing dir → expected empty; got %d", len(out))
	}
}

func TestReadNarrative_ReturnsAllAcrossDays(t *testing.T) {
	dir := t.TempDir()
	day1 := []librarian.NarrativeEntry{
		{CycleID: "c1", Timestamp: time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC), Status: "complete", Summary: "first"},
	}
	day2 := []librarian.NarrativeEntry{
		{CycleID: "c2", Timestamp: time.Date(2026, 4, 2, 11, 0, 0, 0, time.UTC), Status: "complete", Summary: "second"},
	}
	day3 := []librarian.NarrativeEntry{
		{CycleID: "c3", Timestamp: time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC), Status: "complete", Summary: "third"},
	}
	seedNarrativeFile(t, dir, "2026-04-01", day1)
	seedNarrativeFile(t, dir, "2026-04-02", day2)
	seedNarrativeFile(t, dir, "2026-04-03", day3)

	out, err := ReadNarrative(dir, time.Time{}, time.Time{}, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("got %d entries; want 3", len(out))
	}
	for i, want := range []string{"first", "second", "third"} {
		if out[i].Summary != want {
			t.Errorf("entry %d summary = %q, want %q", i, out[i].Summary, want)
		}
	}
}

func TestReadNarrative_FiltersByDateRange(t *testing.T) {
	dir := t.TempDir()
	for _, day := range []string{"2026-04-01", "2026-04-02", "2026-04-03", "2026-04-04"} {
		t0, _ := time.Parse("2006-01-02", day)
		seedNarrativeFile(t, dir, day, []librarian.NarrativeEntry{
			{CycleID: day, Timestamp: t0.Add(12 * time.Hour), Status: "complete", Summary: "entry-" + day},
		})
	}

	start := time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 3, 23, 59, 59, 0, time.UTC)
	out, err := ReadNarrative(dir, start, end, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d, want 2 (4-02 + 4-03)", len(out))
	}
	for _, e := range out {
		if e.CycleID == "2026-04-01" || e.CycleID == "2026-04-04" {
			t.Errorf("entry outside window: %s", e.CycleID)
		}
	}
}

func TestReadNarrative_SkipsMalformedLines(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, librarian.NarrativeSubdir)
	os.MkdirAll(subdir, 0o755)
	path := filepath.Join(subdir, "2026-04-01.jsonl")
	body := `{"cycle_id":"good","timestamp":"2026-04-01T10:00:00Z","status":"complete","summary":"ok"}
{not valid json
{"cycle_id":"good2","timestamp":"2026-04-01T11:00:00Z","status":"complete","summary":"ok2"}
`
	os.WriteFile(path, []byte(body), 0o644)

	out, err := ReadNarrative(dir, time.Time{}, time.Time{}, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Errorf("got %d, want 2 (malformed line skipped)", len(out))
	}
}

func TestReadNarrative_IgnoresNonMatchingFilenames(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, librarian.NarrativeSubdir)
	os.MkdirAll(subdir, 0o755)
	// Drop a stray file that doesn't match the date pattern.
	os.WriteFile(filepath.Join(subdir, "notes.txt"), []byte("operator notes"), 0o644)
	os.WriteFile(filepath.Join(subdir, ".DS_Store"), []byte{}, 0o644)
	seedNarrativeFile(t, dir, "2026-04-01", []librarian.NarrativeEntry{
		{CycleID: "c1", Timestamp: time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC), Status: "complete", Summary: "ok"},
	})
	out, err := ReadNarrative(dir, time.Time{}, time.Time{}, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Errorf("non-matching files should be skipped; got %d entries", len(out))
	}
}

// ---- ReadFacts ----

func TestReadFacts_RoundsTrip(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	seedFactsFile(t, dir, "2026-04-01", []librarian.Fact{
		{Key: "user.name", Value: "Seamus", Scope: librarian.FactScopePersistent, Operation: librarian.FactOperationWrite, Confidence: 0.9, Timestamp: t0},
	})
	out, err := ReadFacts(dir, time.Time{}, time.Time{}, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Key != "user.name" || out[0].Value != "Seamus" {
		t.Errorf("got %+v", out)
	}
}

func TestReadFacts_MissingDirReturnsEmpty(t *testing.T) {
	out, err := ReadFacts(t.TempDir(), time.Time{}, time.Time{}, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Errorf("missing dir → expected empty; got %d", len(out))
	}
}

// ---- ReadCases ----

func TestReadCases_RoundsTrip(t *testing.T) {
	dir := t.TempDir()
	seedCasesFile(t, dir, []cbr.Case{
		{ID: "c1", Timestamp: time.Now(), SchemaVersion: cbr.SchemaVersion, Problem: cbr.Problem{Intent: "debug"}, Outcome: cbr.Outcome{Status: cbr.StatusSuccess, Confidence: 0.9}},
	})
	out, err := ReadCases(dir, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].ID != "c1" {
		t.Errorf("got %+v", out)
	}
}

func TestReadCases_LastRecordPerIDWins(t *testing.T) {
	dir := t.TempDir()
	seedCasesFile(t, dir, []cbr.Case{
		{ID: "c1", Timestamp: time.Now(), SchemaVersion: cbr.SchemaVersion, Outcome: cbr.Outcome{Confidence: 0.5}},
		{ID: "c1", Timestamp: time.Now(), SchemaVersion: cbr.SchemaVersion, Outcome: cbr.Outcome{Confidence: 0.9}}, // supersede
	})
	out, err := ReadCases(dir, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("expected dedup to 1 case; got %d", len(out))
	}
	if out[0].Outcome.Confidence != 0.9 {
		t.Errorf("last-wins violated: confidence = %g, want 0.9", out[0].Outcome.Confidence)
	}
}

func TestReadCases_MissingFileReturnsEmpty(t *testing.T) {
	out, err := ReadCases(t.TempDir(), discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Errorf("missing file → expected empty; got %d", len(out))
	}
}

// ---- dateInWindow boundary ----

func TestDateInWindow_BoundaryConditions(t *testing.T) {
	day1 := time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 4, 2, 23, 59, 0, 0, time.UTC)

	if !dateInWindow("2026-04-02", day1, day2) {
		t.Error("file on the same day as bounds should be included")
	}
	if !dateInWindow("2026-04-02", time.Time{}, day2) {
		t.Error("zero start should include")
	}
	if !dateInWindow("2026-04-02", day1, time.Time{}) {
		t.Error("zero end should include")
	}
	if dateInWindow("2026-04-01", day1, day2) {
		t.Error("file before start should be excluded")
	}
	if dateInWindow("2026-04-03", day1, day2) {
		t.Error("file after end should be excluded")
	}
}
