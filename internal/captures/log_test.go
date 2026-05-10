package captures

import (
	"path/filepath"
	"testing"
	"time"
)

func openStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir, func() time.Time { return now() })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

func TestOpen_RequiresDataDir(t *testing.T) {
	if _, err := Open("", nil); err == nil {
		t.Errorf("expected error on empty dataDir")
	}
}

func TestOpen_CreatesSubdir(t *testing.T) {
	s, dir := openStore(t)
	if s.dir != filepath.Join(dir, Subdir) {
		t.Errorf("dir = %q", s.dir)
	}
}

func TestAppend_RequiresID(t *testing.T) {
	s, _ := openStore(t)
	c := Capture{Text: "no id"}
	if err := s.Append(c); err == nil {
		t.Error("expected error appending without ID")
	}
}

func TestAppend_DefaultsApplied(t *testing.T) {
	s, _ := openStore(t)
	c := Capture{ID: "abc", SourceCycleID: "c1", Text: "I'll do X"}
	if err := s.Append(c); err != nil {
		t.Fatal(err)
	}
	pending, err := s.SnapshotPending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending, got %d", len(pending))
	}
	got := pending[0]
	if got.Status != StatusPending {
		t.Errorf("default status = %q, want %q", got.Status, StatusPending)
	}
	if got.SchemaVersion != SchemaVersion {
		t.Errorf("default schema = %d, want %d", got.SchemaVersion, SchemaVersion)
	}
	if got.Timestamp.IsZero() {
		t.Error("Timestamp should default to now()")
	}
}

func TestSnapshotPending_StatusSupersession(t *testing.T) {
	// Append a Pending entry, then a same-id Expired entry. The
	// snapshot should drop the capture (status no longer Pending).
	s, _ := openStore(t)
	c := Capture{
		ID: "abc", SourceCycleID: "c1", Text: "deferred",
		CreatedAt: now().Add(-10 * 24 * time.Hour),
	}
	if err := s.Append(c); err != nil {
		t.Fatal(err)
	}
	expired := c
	expired.Status = StatusExpired
	expired.Timestamp = now()
	expired.Reason = "auto-expired"
	if err := s.Append(expired); err != nil {
		t.Fatal(err)
	}
	pending, err := s.SnapshotPending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("expected 0 pending after supersession, got %d", len(pending))
	}
}

func TestSnapshotPending_OrderedOldestFirst(t *testing.T) {
	s, _ := openStore(t)
	older := Capture{ID: "old", SourceCycleID: "c1", Text: "older",
		CreatedAt: now().Add(-3 * 24 * time.Hour)}
	newer := Capture{ID: "new", SourceCycleID: "c2", Text: "newer",
		CreatedAt: now().Add(-1 * 24 * time.Hour)}
	if err := s.Append(newer); err != nil { // append in reverse order
		t.Fatal(err)
	}
	if err := s.Append(older); err != nil {
		t.Fatal(err)
	}
	pending, err := s.SnapshotPending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("expected 2 pending, got %d", len(pending))
	}
	if pending[0].ID != "old" || pending[1].ID != "new" {
		t.Errorf("expected oldest-first; got %s then %s", pending[0].ID, pending[1].ID)
	}
}

func TestCountPending_MatchesSnapshot(t *testing.T) {
	s, _ := openStore(t)
	for i := 0; i < 3; i++ {
		c := Capture{ID: string(rune('a' + i)), SourceCycleID: "c1", Text: "x",
			CreatedAt: now()}
		if err := s.Append(c); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.CountPending(); got != 3 {
		t.Errorf("CountPending = %d, want 3", got)
	}
}

func TestCountPending_FreshWorkspace(t *testing.T) {
	s, _ := openStore(t)
	if got := s.CountPending(); got != 0 {
		t.Errorf("fresh workspace count = %d, want 0", got)
	}
}

func TestLookupByID_FindsLatest(t *testing.T) {
	s, _ := openStore(t)
	original := Capture{ID: "abc", SourceCycleID: "c1", Text: "promise",
		CreatedAt: now().Add(-5 * 24 * time.Hour)}
	if err := s.Append(original); err != nil {
		t.Fatal(err)
	}
	updated := original
	updated.Status = StatusExpired
	updated.Timestamp = now()
	updated.Reason = "expired"
	if err := s.Append(updated); err != nil {
		t.Fatal(err)
	}
	got, err := s.LookupByID("abc")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected to find capture")
	}
	if got.Status != StatusExpired {
		t.Errorf("expected latest status, got %q", got.Status)
	}
	if got.Reason != "expired" {
		t.Errorf("expected reason=expired, got %q", got.Reason)
	}
}

func TestLookupByID_Missing(t *testing.T) {
	s, _ := openStore(t)
	got, err := s.LookupByID("nope")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("expected nil for missing id, got %+v", got)
	}
}

func TestSweepExpired_AgesOlderEntries(t *testing.T) {
	s, _ := openStore(t)
	old := Capture{ID: "old", SourceCycleID: "c1", Text: "stale promise",
		CreatedAt: now().Add(-10 * 24 * time.Hour)}
	fresh := Capture{ID: "fresh", SourceCycleID: "c2", Text: "new promise",
		CreatedAt: now().Add(-1 * 24 * time.Hour)}
	if err := s.Append(old); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(fresh); err != nil {
		t.Fatal(err)
	}
	expiredCount, err := s.SweepExpired(now(), 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if expiredCount != 1 {
		t.Errorf("expected to expire 1, got %d", expiredCount)
	}
	pending, _ := s.SnapshotPending()
	if len(pending) != 1 || pending[0].ID != "fresh" {
		t.Errorf("expected only 'fresh' pending; got %v", pending)
	}
}

func TestSweepExpired_Idempotent(t *testing.T) {
	s, _ := openStore(t)
	old := Capture{ID: "old", SourceCycleID: "c1", Text: "stale",
		CreatedAt: now().Add(-10 * 24 * time.Hour)}
	if err := s.Append(old); err != nil {
		t.Fatal(err)
	}
	first, err := s.SweepExpired(now(), 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if first != 1 {
		t.Errorf("first sweep should expire 1; got %d", first)
	}
	second, err := s.SweepExpired(now().Add(time.Hour), 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if second != 0 {
		t.Errorf("second sweep should be no-op; expired %d", second)
	}
}

func TestSweepExpired_EmptyStore(t *testing.T) {
	s, _ := openStore(t)
	got, err := s.SweepExpired(now(), 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("empty store sweep = %d, want 0", got)
	}
}
