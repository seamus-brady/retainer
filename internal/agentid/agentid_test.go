package agentid

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---- LoadOrCreate ----

func TestLoadOrCreate_FreshDirCreatesFile(t *testing.T) {
	dir := t.TempDir()
	id, err := LoadOrCreate(dir, discardLog())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if id.UUID == "" {
		t.Errorf("UUID empty after create")
	}
	if id.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", id.SchemaVersion, SchemaVersion)
	}
	if id.CreatedAt.IsZero() {
		t.Error("CreatedAt zero")
	}
	if id.LastSeenAt.IsZero() {
		t.Error("LastSeenAt zero")
	}
	// File written.
	if _, err := os.Stat(filepath.Join(dir, Filename)); err != nil {
		t.Errorf("identity.json not created: %v", err)
	}
}

func TestLoadOrCreate_ReusesExistingUUID(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreate(dir, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreate(dir, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if first.UUID != second.UUID {
		t.Errorf("UUIDs differ across loads: %q vs %q", first.UUID, second.UUID)
	}
	if !second.LastSeenAt.After(first.LastSeenAt) && !second.LastSeenAt.Equal(first.LastSeenAt) {
		t.Errorf("LastSeenAt should advance or stay (within clock resolution); first=%v second=%v", first.LastSeenAt, second.LastSeenAt)
	}
}

func TestLoadOrCreate_RegeneratesOnMalformedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, Filename)
	if err := os.WriteFile(path, []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := LoadOrCreate(dir, discardLog())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if id.UUID == "" {
		t.Error("expected fresh UUID after malformed file")
	}
	// File should now be valid JSON.
	body, _ := os.ReadFile(path)
	var roundTrip Identity
	if err := json.Unmarshal(body, &roundTrip); err != nil {
		t.Errorf("rewritten file not valid JSON: %v", err)
	}
}

func TestLoadOrCreate_RegeneratesOnSchemaVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, Filename)
	body := []byte(`{"schema_version": 999, "uuid": "old-uuid", "created_at": "2020-01-01T00:00:00Z", "last_seen_at": "2020-01-01T00:00:00Z"}`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := LoadOrCreate(dir, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if id.UUID == "old-uuid" {
		t.Error("expected new UUID after schema version mismatch")
	}
	if id.SchemaVersion != SchemaVersion {
		t.Errorf("expected SchemaVersion = %d", SchemaVersion)
	}
}

func TestLoadOrCreate_HandlesEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, Filename)
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := LoadOrCreate(dir, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if id.UUID == "" {
		t.Error("empty file should have triggered fresh create")
	}
}

func TestLoadOrCreate_CreatesDataDirIfMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "data")
	id, err := LoadOrCreate(dir, discardLog())
	if err != nil {
		t.Fatalf("LoadOrCreate should mkdir nested dirs: %v", err)
	}
	if id.UUID == "" {
		t.Error("UUID empty")
	}
}

// ---- Identity helpers ----

func TestInstanceID_Length(t *testing.T) {
	id := Identity{UUID: "f47ac10b-58cc-4372-a567-0e02b2c3d479"}
	if got := id.InstanceID(); len(got) != 8 {
		t.Errorf("InstanceID len = %d, want 8 (got %q)", len(got), got)
	}
	if id.InstanceID() != "f47ac10b" {
		t.Errorf("InstanceID = %q, want first 8 chars f47ac10b", id.InstanceID())
	}
}

func TestInstanceID_ShortUUIDPasses(t *testing.T) {
	id := Identity{UUID: "abc"}
	if got := id.InstanceID(); got != "abc" {
		t.Errorf("short UUID should pass through; got %q", got)
	}
}

func TestInstanceID_ZeroValueEmpty(t *testing.T) {
	if got := (Identity{}).InstanceID(); got != "" {
		t.Errorf("zero-value InstanceID should be empty; got %q", got)
	}
}

func TestIsZero(t *testing.T) {
	if !(Identity{}).IsZero() {
		t.Error("zero-value Identity should report IsZero")
	}
	if (Identity{UUID: "x"}).IsZero() {
		t.Error("non-empty UUID should not report IsZero")
	}
}

// ---- File shape ----

func TestLoadOrCreate_FileMatchesDocumentedShape(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrCreate(dir, discardLog()); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, Filename))
	if err != nil {
		t.Fatal(err)
	}
	// Spec doc pins these JSON keys; if they shift the doc + file
	// drift apart silently. Keep this test as the canonical pin.
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"schema_version", "uuid", "created_at", "last_seen_at"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing key %q in identity.json", key)
		}
	}
}
