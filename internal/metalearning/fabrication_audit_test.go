package metalearning

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeAuditCycleLog seeds <dataDir>/cycle-log/<date>.jsonl with
// raw JSONL lines. Test helpers can hand-write the events they
// need — keeps the audit tests independent of the cog's exact
// emission shape (which evolves more often than this audit's
// expectations).
func writeAuditCycleLog(t *testing.T, dataDir, date string, lines []string) {
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

// captureSink is a test FactSink that records every RecordFact
// call. Concurrency-safe even though the audit is single-
// threaded — keeps the helper reusable if a future test runs
// the audit in parallel.
type captureSink struct {
	mu      sync.Mutex
	records []FactRecord
}

func (c *captureSink) RecordFact(r FactRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
}

func (c *captureSink) latest() (FactRecord, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.records) == 0 {
		return FactRecord{}, false
	}
	return c.records[len(c.records)-1], true
}

func auditDeps(t *testing.T, now time.Time) (string, *captureSink, Deps) {
	t.Helper()
	dataDir := t.TempDir()
	sink := &captureSink{}
	return dataDir, sink, Deps{
		DataDir:  dataDir,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		NowFn:    func() time.Time { return now },
		FactSink: sink,
	}
}

// decodeAuditValue decodes the latest fact's JSON body into the
// fact-value struct so tests can assert against named fields
// instead of grepping the raw string.
func decodeAuditValue(t *testing.T, sink *captureSink) fabricationAuditFactValue {
	t.Helper()
	rec, ok := sink.latest()
	if !ok {
		t.Fatal("no fact recorded")
	}
	if rec.Key != FabricationAuditFactKey {
		t.Fatalf("fact key = %q, want %q", rec.Key, FabricationAuditFactKey)
	}
	var v fabricationAuditFactValue
	if err := json.Unmarshal([]byte(rec.Value), &v); err != nil {
		t.Fatalf("decode fact value %q: %v", rec.Value, err)
	}
	return v
}

func TestFabricationAudit_FlagsURLsWithoutFetchTools(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	dataDir, sink, deps := auditDeps(t, now)
	writeAuditCycleLog(t, dataDir, "2026-05-09", []string{
		`{"type":"cycle_start","cycle_id":"c1","timestamp":"2026-05-09T11:00:00Z","node_type":"cognitive"}`,
		`{"type":"cycle_complete","cycle_id":"c1","timestamp":"2026-05-09T11:00:01Z","status":"complete","text":"Check https://example.com/foo for the price."}`,
	})
	if err := FabricationAudit(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	v := decodeAuditValue(t, sink)
	if v.Count != 1 {
		t.Errorf("count = %d, want 1", v.Count)
	}
	if v.Examined != 1 {
		t.Errorf("examined = %d, want 1", v.Examined)
	}
	if len(v.SuspectCycleIDs) != 1 || v.SuspectCycleIDs[0] != "c1" {
		t.Errorf("suspect_cycle_ids = %v, want [c1]", v.SuspectCycleIDs)
	}
	_ = dataDir
}

func TestFabricationAudit_PassesWhenURLsHaveFetchSupport(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	dataDir, sink, deps := auditDeps(t, now)
	writeAuditCycleLog(t, dataDir, "2026-05-09", []string{
		`{"type":"cycle_start","cycle_id":"c1","timestamp":"2026-05-09T11:00:00Z","node_type":"cognitive"}`,
		`{"type":"tool_call","cycle_id":"c1","timestamp":"2026-05-09T11:00:01Z","tool_name":"brave_web_search"}`,
		`{"type":"tool_result","cycle_id":"c1","timestamp":"2026-05-09T11:00:02Z","tool_name":"brave_web_search","success":true}`,
		`{"type":"cycle_complete","cycle_id":"c1","timestamp":"2026-05-09T11:00:03Z","status":"complete","text":"Found at https://example.com/foo."}`,
	})
	if err := FabricationAudit(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	v := decodeAuditValue(t, sink)
	if v.Count != 0 {
		t.Errorf("expected zero suspects (URL grounded by fetch tool), got count=%d", v.Count)
	}
	if v.Examined != 1 {
		t.Errorf("examined = %d, want 1", v.Examined)
	}
	_ = dataDir
}

func TestFabricationAudit_FlagsActionClaimsWithoutTools(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	dataDir, sink, deps := auditDeps(t, now)
	writeAuditCycleLog(t, dataDir, "2026-05-09", []string{
		`{"type":"cycle_start","cycle_id":"c1","timestamp":"2026-05-09T11:00:00Z","node_type":"cognitive"}`,
		`{"type":"cycle_complete","cycle_id":"c1","timestamp":"2026-05-09T11:00:01Z","status":"complete","text":"The email has been sent and the links have been verified."}`,
	})
	if err := FabricationAudit(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	v := decodeAuditValue(t, sink)
	if v.Count != 1 {
		t.Errorf("count = %d, want 1", v.Count)
	}
	_ = dataDir
}

func TestFabricationAudit_PassesActionClaimsWithTools(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	dataDir, sink, deps := auditDeps(t, now)
	writeAuditCycleLog(t, dataDir, "2026-05-09", []string{
		`{"type":"cycle_start","cycle_id":"c1","timestamp":"2026-05-09T11:00:00Z","node_type":"cognitive"}`,
		`{"type":"tool_call","cycle_id":"c1","timestamp":"2026-05-09T11:00:01Z","tool_name":"agent_comms"}`,
		`{"type":"tool_result","cycle_id":"c1","timestamp":"2026-05-09T11:00:02Z","tool_name":"agent_comms","success":true}`,
		`{"type":"cycle_complete","cycle_id":"c1","timestamp":"2026-05-09T11:00:03Z","status":"complete","text":"The email has been sent."}`,
	})
	if err := FabricationAudit(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	v := decodeAuditValue(t, sink)
	if v.Count != 0 {
		t.Errorf("action claim WITH tool call should not flag; got count=%d", v.Count)
	}
	_ = dataDir
}

func TestFabricationAudit_IgnoresAgentSubCycles(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	dataDir, sink, deps := auditDeps(t, now)
	writeAuditCycleLog(t, dataDir, "2026-05-09", []string{
		// Agent cycle that would otherwise flag — should NOT
		// because the audit scopes to cog cycles only.
		`{"type":"cycle_start","cycle_id":"a1","timestamp":"2026-05-09T11:00:00Z","node_type":"agent"}`,
		`{"type":"cycle_complete","cycle_id":"a1","timestamp":"2026-05-09T11:00:01Z","status":"complete","text":"The email has been sent."}`,
	})
	if err := FabricationAudit(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	v := decodeAuditValue(t, sink)
	if v.Count != 0 {
		t.Errorf("agent sub-cycle should not be audited; got count=%d", v.Count)
	}
	if v.Examined != 0 {
		t.Errorf("agent sub-cycle should not be examined; got examined=%d", v.Examined)
	}
	_ = dataDir
}

func TestFabricationAudit_RespectsWindow(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	dataDir, sink, deps := auditDeps(t, now)
	// Eight days back — outside the 7-day window.
	writeAuditCycleLog(t, dataDir, "2026-05-01", []string{
		`{"type":"cycle_start","cycle_id":"old","timestamp":"2026-05-01T11:00:00Z","node_type":"cognitive"}`,
		`{"type":"cycle_complete","cycle_id":"old","timestamp":"2026-05-01T11:00:01Z","status":"complete","text":"Visit https://x.com — email has been sent."}`,
	})
	if err := FabricationAudit(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	v := decodeAuditValue(t, sink)
	if v.Count != 0 || v.Examined != 0 {
		t.Errorf("cycle outside window should not flag or be examined; got count=%d examined=%d", v.Count, v.Examined)
	}
	_ = dataDir
}

func TestFabricationAudit_FreshWorkspaceWritesZeroFact(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	dataDir, sink, deps := auditDeps(t, now)
	if err := FabricationAudit(context.Background(), deps); err != nil {
		t.Fatalf("audit should succeed on fresh workspace; got %v", err)
	}
	// Even on a fresh workspace the audit writes a fact — the
	// sensorium needs to see "0/0" to render `<integrity
	// suspect_replies_7d="0" replies_examined_7d="0"/>`. Quiet
	// is information.
	v := decodeAuditValue(t, sink)
	if v.Count != 0 || v.Examined != 0 {
		t.Errorf("fresh workspace expected count=0 examined=0; got count=%d examined=%d", v.Count, v.Examined)
	}
	_ = dataDir
}

func TestFabricationAudit_OverwritesPreviousFact(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	dataDir, sink, deps := auditDeps(t, now)
	writeAuditCycleLog(t, dataDir, "2026-05-09", []string{
		`{"type":"cycle_start","cycle_id":"c1","timestamp":"2026-05-09T11:00:00Z","node_type":"cognitive"}`,
		`{"type":"cycle_complete","cycle_id":"c1","timestamp":"2026-05-09T11:00:01Z","status":"complete","text":"Visit https://example.com/x."}`,
	})
	if err := FabricationAudit(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	// Run a second time — same scan, same result. The librarian
	// most-recent-wins makes both entries safe; the curator
	// reads the latest. Both writes go to the sink so we can
	// confirm the worker idempotently re-emits.
	if err := FabricationAudit(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	if got := len(sink.records); got != 2 {
		t.Errorf("expected 2 fact writes (once per run); got %d", got)
	}
	_ = dataDir
}

func TestFabricationAudit_NoSinkSkipsPersist(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	dataDir, _, deps := auditDeps(t, now)
	deps.FactSink = nil
	writeAuditCycleLog(t, dataDir, "2026-05-09", []string{
		`{"type":"cycle_start","cycle_id":"c1","timestamp":"2026-05-09T11:00:00Z","node_type":"cognitive"}`,
		`{"type":"cycle_complete","cycle_id":"c1","timestamp":"2026-05-09T11:00:01Z","status":"complete","text":"https://x.com — email has been sent."}`,
	})
	// Should not panic, should not return an error — workers
	// tolerate missing FactSink so test fakes can exercise the
	// scan logic in isolation.
	if err := FabricationAudit(context.Background(), deps); err != nil {
		t.Fatalf("audit should succeed without FactSink; got %v", err)
	}
}

func TestExtractURLs(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"see https://example.com/foo for details", []string{"https://example.com/foo"}},
		{"see (https://x.com) and https://y.com.", []string{"https://x.com", "https://y.com"}},
		{"no urls here", nil},
		{"http://insecure.example.org/page", []string{"http://insecure.example.org/page"}},
	}
	for _, tc := range cases {
		got := extractURLs(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("extractURLs(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i, w := range tc.want {
			if got[i] != w {
				t.Errorf("extractURLs(%q)[%d] = %q, want %q", tc.in, i, got[i], w)
			}
		}
	}
}
