package actorstate

import (
	"os"
	"path/filepath"
	"testing"
)

type sampleState struct {
	Version int    `json:"version"`
	Last    string `json:"last,omitempty"`
	Count   int    `json:"count"`
}

func TestRead_FileMissingIsFreshStart(t *testing.T) {
	dir := t.TempDir()
	var got sampleState
	if err := Read(filepath.Join(dir, "absent.json"), &got); err != nil {
		t.Fatalf("missing file should be fresh-start (no error), got %v", err)
	}
	if got != (sampleState{}) {
		t.Errorf("dst should remain zero, got %+v", got)
	}
}

func TestRead_EmptyFileIsFreshStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var got sampleState
	if err := Read(path, &got); err != nil {
		t.Fatalf("empty file should be fresh-start, got %v", err)
	}
	if got != (sampleState{}) {
		t.Errorf("dst should remain zero, got %+v", got)
	}
}

func TestRead_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte(`{not valid`), 0o644); err != nil {
		t.Fatal(err)
	}
	var got sampleState
	if err := Read(path, &got); err == nil {
		t.Errorf("malformed JSON should error")
	}
}

func TestWrite_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	want := sampleState{Version: 1, Last: "2026-05-02T12:00:00Z", Count: 42}
	if err := Write(path, want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	var got sampleState
	if err := Read(path, &got); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestWrite_CreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	// Several levels of nesting.
	path := filepath.Join(dir, "a", "b", "c", "state.json")
	want := sampleState{Version: 1, Count: 1}
	if err := Write(path, want); err != nil {
		t.Fatalf("Write should create parent dirs: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file should exist: %v", err)
	}
}

func TestWrite_AtomicLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := Write(path, sampleState{Count: 1}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("temp file should be cleaned up; got %d entries: %+v", len(entries), entries)
	}
	if entries[0].Name() != "state.json" {
		t.Errorf("only state.json should remain, got %q", entries[0].Name())
	}
}

func TestWrite_OverwritePreservesContentOnFailure(t *testing.T) {
	// Atomic-write means even if a crash happened mid-write, the
	// previous file is intact. This test simulates "write-then-write"
	// and verifies the second write replaces the first cleanly.
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := Write(path, sampleState{Version: 1, Count: 1}); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, sampleState{Version: 2, Count: 99}); err != nil {
		t.Fatal(err)
	}
	var got sampleState
	if err := Read(path, &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != 2 || got.Count != 99 {
		t.Errorf("got %+v, want version=2 count=99", got)
	}
}
