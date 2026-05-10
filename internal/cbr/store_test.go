package cbr

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := NewStore(t.TempDir(), logger)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func TestStore_AppendAndLoadAll(t *testing.T) {
	s := newTestStore(t)

	c1 := Case{
		ID:                "c-1",
		Timestamp:         time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC),
		SchemaVersion:     SchemaVersion,
		SourceNarrativeID: "cycle-1",
		Problem:           Problem{Intent: "debug", Domain: "auth"},
		Solution:          Solution{Approach: "look at logs"},
		Outcome:           Outcome{Status: StatusSuccess, Confidence: 0.9},
	}
	c2 := Case{
		ID:                "c-2",
		Timestamp:         time.Date(2026, 5, 2, 13, 0, 0, 0, time.UTC),
		SchemaVersion:     SchemaVersion,
		SourceNarrativeID: "cycle-2",
		Problem:           Problem{Intent: "research", Domain: "papers"},
		Solution:          Solution{Approach: "search web"},
		Outcome:           Outcome{Status: StatusFailure, Confidence: 0.4, Pitfalls: []string{"misread title"}},
	}
	if err := s.Append(c1); err != nil {
		t.Fatalf("Append c1: %v", err)
	}
	if err := s.Append(c2); err != nil {
		t.Fatalf("Append c2: %v", err)
	}

	loaded, err := s.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("loaded %d, want 2", len(loaded))
	}
	if loaded[0].ID != "c-1" || loaded[1].ID != "c-2" {
		t.Errorf("order wrong: %q %q", loaded[0].ID, loaded[1].ID)
	}
	if !reflect.DeepEqual(loaded[1].Outcome.Pitfalls, []string{"misread title"}) {
		t.Errorf("pitfalls didn't round-trip: %+v", loaded[1].Outcome)
	}
}

func TestStore_AppendStampsZeroTimestamp(t *testing.T) {
	s := newTestStore(t)
	c := Case{ID: "c-1", SchemaVersion: SchemaVersion}
	if err := s.Append(c); err != nil {
		t.Fatalf("Append: %v", err)
	}
	loaded, _ := s.LoadAll()
	if len(loaded) != 1 {
		t.Fatalf("expected 1 case loaded")
	}
	if loaded[0].Timestamp.IsZero() {
		t.Error("Append should stamp zero timestamps before writing")
	}
}

func TestStore_AppendDefaultsSchemaVersion(t *testing.T) {
	s := newTestStore(t)
	c := Case{ID: "c-1"} // SchemaVersion=0
	if err := s.Append(c); err != nil {
		t.Fatalf("Append: %v", err)
	}
	loaded, _ := s.LoadAll()
	if loaded[0].SchemaVersion != SchemaVersion {
		t.Errorf("default SchemaVersion = %d, want %d", loaded[0].SchemaVersion, SchemaVersion)
	}
}

func TestStore_LoadAllOnMissingFileReturnsEmpty(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dataDir := t.TempDir()
	// Don't construct via NewStore — that creates the file. Build a
	// Store manually so LoadAll hits a missing file.
	s := &Store{
		dir:    filepath.Join(dataDir, SubDir),
		path:   filepath.Join(dataDir, SubDir, "cases.jsonl"),
		logger: logger,
	}
	cases, err := s.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll on missing file: %v", err)
	}
	if cases != nil {
		t.Errorf("missing file should return nil; got %v", cases)
	}
}

func TestStore_LoadAllSkipsMalformedLines(t *testing.T) {
	s := newTestStore(t)
	good := Case{ID: "c-1", SchemaVersion: SchemaVersion, Timestamp: time.Now()}
	if err := s.Append(good); err != nil {
		t.Fatal(err)
	}
	// Append a malformed line directly.
	if err := appendRaw(s.Path(), "{not valid json"); err != nil {
		t.Fatal(err)
	}
	another := Case{ID: "c-2", SchemaVersion: SchemaVersion, Timestamp: time.Now()}
	if err := s.Append(another); err != nil {
		t.Fatal(err)
	}

	loaded, err := s.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 {
		t.Errorf("malformed line should be skipped; loaded %d", len(loaded))
	}
}

// appendRaw writes a literal line + newline. Used to inject corrupt
// JSONL into a store for the resilience test.
func appendRaw(path, line string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write([]byte(line + "\n"))
	return err
}
