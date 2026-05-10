// Package dailyfile is the rotating file writer used by both the slog system
// log and the cycle-log telemetry stream. Each Write checks today's date; if
// it differs from the open file's date, the writer rotates to a new file
// named YYYY-MM-DD.<suffix>. Atomic at the Write boundary — slog/JSONL
// records that fit in one Write don't span files.
package dailyfile

import (
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DateLayout is the YYYY-MM-DD format used for filenames.
const DateLayout = "2006-01-02"

// Writer rotates a file under dir based on the date returned by now().
type Writer struct {
	dir    string
	suffix string
	now    func() time.Time
	mu     sync.Mutex
	file   *os.File
	date   string
}

// NewWriter creates a daily-rotating writer under dir. Files are named
// YYYY-MM-DD<suffix> (suffix should include the leading dot, e.g. ".jsonl").
// The directory must already exist.
func NewWriter(dir, suffix string, now func() time.Time) *Writer {
	return &Writer{dir: dir, suffix: suffix, now: now}
}

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	today := w.now().Format(DateLayout)
	if w.file == nil || today != w.date {
		if w.file != nil {
			_ = w.file.Close()
		}
		path := filepath.Join(w.dir, today+w.suffix)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return 0, err
		}
		w.file = f
		w.date = today
	}
	return w.file.Write(p)
}

// Close flushes and closes the open file. Safe to call multiple times.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}
