package metalearning

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seamus-brady/retainer/internal/cbr"
)

func TestDailyMining_NoCasesNoOp(t *testing.T) {
	dir := t.TempDir()
	deps := Deps{DataDir: dir, Logger: discardLogger(), NowFn: time.Now}
	if err := DailyMining(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	// Patterns dir should not have been created — nothing to mine.
	if _, err := os.Stat(filepath.Join(dir, "patterns")); !os.IsNotExist(err) {
		t.Error("patterns dir should not exist when no cases")
	}
}

func TestDailyMining_AppendsClustersToPatternsLog(t *testing.T) {
	dir := t.TempDir()
	// Seed enough cases to form one cluster (min_cases=3, threshold).
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	seedCases(t, dir, []cbr.Case{
		makeAuthCase("c1", now.Add(-3*24*time.Hour)),
		makeAuthCase("c2", now.Add(-2*24*time.Hour)),
		makeAuthCase("c3", now.Add(-1*24*time.Hour)),
	})

	deps := Deps{DataDir: dir, Logger: discardLogger(), NowFn: fixedClock(now)}
	if err := DailyMining(context.Background(), deps); err != nil {
		t.Fatal(err)
	}

	patternsPath := filepath.Join(dir, "patterns", "patterns.jsonl")
	patterns := readPatterns(t, patternsPath)
	if len(patterns) == 0 {
		t.Fatalf("expected at least one pattern, got 0")
	}
	for _, p := range patterns {
		if !p.MinedAt.Equal(now) {
			t.Errorf("MinedAt = %v, want %v", p.MinedAt, now)
		}
		if p.Size < dailyMiningMinCases {
			t.Errorf("cluster size = %d below min %d", p.Size, dailyMiningMinCases)
		}
		if len(p.CaseIDs) != p.Size {
			t.Errorf("Size %d != len(CaseIDs) %d", p.Size, len(p.CaseIDs))
		}
	}
}

func TestDailyMining_AppendsAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	seedCases(t, dir, []cbr.Case{
		makeAuthCase("c1", now),
		makeAuthCase("c2", now),
		makeAuthCase("c3", now),
	})
	deps := Deps{DataDir: dir, Logger: discardLogger(), NowFn: fixedClock(now)}
	if err := DailyMining(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	first := readPatterns(t, filepath.Join(dir, "patterns", "patterns.jsonl"))

	// Re-run with the same data — the patterns file should grow,
	// not be rewritten. Append-only is the immutable-archive
	// invariant.
	if err := DailyMining(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	second := readPatterns(t, filepath.Join(dir, "patterns", "patterns.jsonl"))
	if len(second) != 2*len(first) {
		t.Errorf("expected append: first=%d second=%d", len(first), len(second))
	}
}

func TestDailyMining_MapClusterFields(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	c := cbr.Cluster{
		ID:             "auth-3",
		Size:           3,
		CommonDomain:   "auth",
		CommonKeywords: []string{"oauth", "token"},
		CaseIDs:        []string{"c1", "c2", "c3"},
	}
	got := mapCluster(c, now)
	if got.Domain != "auth" {
		t.Errorf("Domain = %q", got.Domain)
	}
	if got.Size != 3 {
		t.Errorf("Size = %d", got.Size)
	}
	if !got.MinedAt.Equal(now) {
		t.Errorf("MinedAt = %v", got.MinedAt)
	}
	if len(got.CaseIDs) != 3 {
		t.Errorf("CaseIDs len = %d", len(got.CaseIDs))
	}
}

// makeAuthCase produces a CBR case structured to cluster — same
// domain + heavy keyword overlap so cbr.FindClusters' similarity
// score crosses the threshold.
func makeAuthCase(id string, ts time.Time) cbr.Case {
	return cbr.Case{
		ID:        id,
		Timestamp: ts,
		Problem: cbr.Problem{
			Intent:   "debug auth flow",
			Domain:   "auth",
			Keywords: []string{"oauth", "token", "session"},
			Entities: []string{"login"},
		},
		Solution: cbr.Solution{Approach: "trace OAuth"},
		Outcome:  cbr.Outcome{Status: cbr.StatusSuccess, Confidence: 0.85},
		Category: cbr.CategoryTroubleshooting,
	}
}

// seedCases writes the given cases to data/cases/cases.jsonl. The
// remembrancer.ReadCases helper expects one record per line.
func seedCases(t *testing.T, dataDir string, cases []cbr.Case) {
	t.Helper()
	dir := filepath.Join(dataDir, "cases")
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
		line, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}

// readPatterns slurps the patterns.jsonl file into a slice. Used by
// tests asserting the worker's output shape.
func readPatterns(t *testing.T, path string) []MinedPattern {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open patterns: %v", err)
	}
	defer f.Close()
	var out []MinedPattern
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		var p MinedPattern
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			t.Fatalf("decode pattern: %v", err)
		}
		out = append(out, p)
	}
	if err := s.Err(); err != nil {
		t.Fatalf("scan patterns: %v", err)
	}
	return out
}
