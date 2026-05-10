package tools

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seamus-brady/retainer/internal/cbr"
	"github.com/seamus-brady/retainer/internal/librarian"
)

// seedTestWorkspace writes a small narrative + facts + cases fixture
// to a temp dir and returns the dir path. Used by every remembrancer
// tool test so the tools have something to read.
func seedTestWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	// Narrative across 3 days.
	narDir := filepath.Join(dir, librarian.NarrativeSubdir)
	os.MkdirAll(narDir, 0o755)
	for _, day := range []struct {
		date    string
		entries []librarian.NarrativeEntry
	}{
		{"2026-04-01", []librarian.NarrativeEntry{
			{CycleID: "cycle-1", Timestamp: time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC), Status: "complete", Summary: "investigated the auth flow", Domain: "auth"},
		}},
		{"2026-04-02", []librarian.NarrativeEntry{
			{CycleID: "cycle-2", Timestamp: time.Date(2026, 4, 2, 11, 0, 0, 0, time.UTC), Status: "complete", Summary: "wrote the architecture document"},
		}},
		{"2026-04-03", []librarian.NarrativeEntry{
			{CycleID: "cycle-3", Timestamp: time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC), Status: "complete", Summary: "fixed the auth login bug", Domain: "auth"},
		}},
	} {
		writeJSONLNarrative(t, filepath.Join(narDir, day.date+".jsonl"), day.entries)
	}

	// Facts.
	factsDir := filepath.Join(dir, librarian.FactsSubdir)
	os.MkdirAll(factsDir, 0o755)
	writeJSONLFacts(t, filepath.Join(factsDir, "2026-04-01-facts.jsonl"), []librarian.Fact{
		{Key: "user.name", Value: "Seamus", Scope: librarian.FactScopePersistent, Operation: librarian.FactOperationWrite, Confidence: 0.9, Timestamp: time.Date(2026, 4, 1, 10, 30, 0, 0, time.UTC)},
		{Key: "auth.endpoint", Value: "https://example/auth", Scope: librarian.FactScopePersistent, Operation: librarian.FactOperationWrite, Confidence: 0.9, Timestamp: time.Date(2026, 4, 1, 10, 35, 0, 0, time.UTC)},
	})

	// Cases.
	casesDir := filepath.Join(dir, cbr.SubDir)
	os.MkdirAll(casesDir, 0o755)
	writeJSONLCases(t, filepath.Join(casesDir, "cases.jsonl"), []cbr.Case{
		{ID: "case-1", Timestamp: time.Now(), SchemaVersion: cbr.SchemaVersion, Problem: cbr.Problem{Intent: "debug auth flow", Domain: "auth", Keywords: []string{"oauth", "session", "login"}}, Outcome: cbr.Outcome{Status: cbr.StatusSuccess, Confidence: 0.85}, Category: cbr.CategoryTroubleshooting},
		{ID: "case-2", Timestamp: time.Now(), SchemaVersion: cbr.SchemaVersion, Problem: cbr.Problem{Intent: "fix login redirect", Domain: "auth", Keywords: []string{"oauth", "redirect", "login"}}, Outcome: cbr.Outcome{Status: cbr.StatusSuccess, Confidence: 0.85}, Category: cbr.CategoryTroubleshooting},
		{ID: "case-3", Timestamp: time.Now(), SchemaVersion: cbr.SchemaVersion, Problem: cbr.Problem{Intent: "investigate token leak", Domain: "auth", Keywords: []string{"oauth", "token", "login"}}, Outcome: cbr.Outcome{Status: cbr.StatusSuccess, Confidence: 0.85}, Category: cbr.CategoryTroubleshooting},
		{ID: "case-4", Timestamp: time.Now(), SchemaVersion: cbr.SchemaVersion, Problem: cbr.Problem{Intent: "summarise paper", Domain: "research", Keywords: []string{"paper", "summary"}}, Outcome: cbr.Outcome{Status: cbr.StatusSuccess, Confidence: 0.85}, Category: cbr.CategoryDomainKnowledge},
	})

	return dir
}

func writeJSONLNarrative(t *testing.T, path string, entries []librarian.NarrativeEntry) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, e := range entries {
		body, _ := json.Marshal(e)
		f.Write(append(body, '\n'))
	}
}

func writeJSONLFacts(t *testing.T, path string, facts []librarian.Fact) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, fact := range facts {
		body, _ := json.Marshal(fact)
		f.Write(append(body, '\n'))
	}
}

func writeJSONLCases(t *testing.T, path string, cases []cbr.Case) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, c := range cases {
		body, _ := json.Marshal(c)
		f.Write(append(body, '\n'))
	}
}

func toolDeps(dataDir string) *RemembrancerDeps {
	return &RemembrancerDeps{
		DataDir: dataDir,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// ---- DeepSearch ----

func TestDeepSearch_FindsAcrossDays(t *testing.T) {
	dir := seedTestWorkspace(t)
	h := DeepSearch{Deps: toolDeps(dir)}
	out, err := h.Execute(context.Background(), []byte(`{"query":"auth"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"investigated the auth", "fixed the auth login"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}
}

func TestDeepSearch_RespectsDateBounds(t *testing.T) {
	dir := seedTestWorkspace(t)
	h := DeepSearch{Deps: toolDeps(dir)}
	out, err := h.Execute(context.Background(), []byte(`{"query":"auth","start_date":"2026-04-03","end_date":"2026-04-03"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "investigated") {
		t.Errorf("date bound should exclude April 1; got:\n%s", out)
	}
	if !strings.Contains(out, "fixed the auth") {
		t.Errorf("April 3 entry missing; got:\n%s", out)
	}
}

func TestDeepSearch_NoMatchesMessage(t *testing.T) {
	dir := seedTestWorkspace(t)
	h := DeepSearch{Deps: toolDeps(dir)}
	out, _ := h.Execute(context.Background(), []byte(`{"query":"nothingmatches"}`))
	if !strings.Contains(out, "no matches") {
		t.Errorf("expected no-matches message; got: %s", out)
	}
}

func TestDeepSearch_RejectsEmptyQuery(t *testing.T) {
	dir := seedTestWorkspace(t)
	h := DeepSearch{Deps: toolDeps(dir)}
	if _, err := h.Execute(context.Background(), []byte(`{"query":""}`)); err == nil {
		t.Error("empty query should error")
	}
}

func TestDeepSearch_RejectsBadDate(t *testing.T) {
	dir := seedTestWorkspace(t)
	h := DeepSearch{Deps: toolDeps(dir)}
	if _, err := h.Execute(context.Background(), []byte(`{"query":"x","start_date":"April 1"}`)); err == nil {
		t.Error("bad date format should error")
	}
}

// ---- FindConnections ----

func TestFindConnections_ReportsAllStores(t *testing.T) {
	dir := seedTestWorkspace(t)
	h := FindConnections{Deps: toolDeps(dir)}
	out, err := h.Execute(context.Background(), []byte(`{"topic":"auth"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"narrative:", "facts:", "cases:"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
	// auth-domain narrative entries: 2 (April 1 + April 3).
	if !strings.Contains(out, "narrative: 2") {
		t.Errorf("expected narrative: 2 hits in:\n%s", out)
	}
	// auth-keyed facts: 1 (auth.endpoint).
	if !strings.Contains(out, "facts: 1") {
		t.Errorf("expected facts: 1 hit in:\n%s", out)
	}
	// auth cases: 3.
	if !strings.Contains(out, "cases: 3") {
		t.Errorf("expected cases: 3 hits in:\n%s", out)
	}
}

func TestFindConnections_RejectsEmptyTopic(t *testing.T) {
	dir := seedTestWorkspace(t)
	h := FindConnections{Deps: toolDeps(dir)}
	if _, err := h.Execute(context.Background(), []byte(`{"topic":""}`)); err == nil {
		t.Error("empty topic should error")
	}
}

// MinePatterns / ConsolidateMemory / WriteConsolidationReport tools
// retired 2026-05-04. Pattern mining + weekly consolidation now run
// on the metalearning pool's tick — see internal/metalearning/ for
// the worker implementations and tests.
