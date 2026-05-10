package dailyfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriter_RotatesOnDateChange(t *testing.T) {
	dir := t.TempDir()
	fakeNow := time.Date(2026, 4, 28, 23, 59, 0, 0, time.UTC)
	w := NewWriter(dir, ".jsonl", func() time.Time { return fakeNow })

	if _, err := w.Write([]byte("day1\n")); err != nil {
		t.Fatal(err)
	}
	fakeNow = time.Date(2026, 4, 29, 0, 1, 0, 0, time.UTC)
	if _, err := w.Write([]byte("day2\n")); err != nil {
		t.Fatal(err)
	}
	w.Close()

	d1, err := os.ReadFile(filepath.Join(dir, "2026-04-28.jsonl"))
	if err != nil {
		t.Fatalf("day1 file: %v", err)
	}
	d2, err := os.ReadFile(filepath.Join(dir, "2026-04-29.jsonl"))
	if err != nil {
		t.Fatalf("day2 file: %v", err)
	}
	if !strings.Contains(string(d1), "day1") {
		t.Fatalf("day1 wrong content: %s", d1)
	}
	if !strings.Contains(string(d2), "day2") {
		t.Fatalf("day2 wrong content: %s", d2)
	}
}

func TestWriter_HonorsSuffix(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	w := NewWriter(dir, ".log", func() time.Time { return now })
	if _, err := w.Write([]byte("entry\n")); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if _, err := os.Stat(filepath.Join(dir, "2026-04-29.log")); err != nil {
		t.Fatalf("expected file with .log suffix: %v", err)
	}
}

func TestWriter_CloseIdempotent(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	w := NewWriter(dir, ".jsonl", func() time.Time { return now })
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}
