// Package logging configures Retainer's slog-based system logger.
//
// Files land at <data-dir>/logs/YYYY-MM-DD.jsonl via internal/dailyfile.
// Verbose mode tees the same records to stderr in slog's human-readable
// text format.
//
// Retention: at startup, files older than RetentionDays are deleted. Only
// files matching the YYYY-MM-DD.jsonl pattern are considered.
package logging

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/seamus-brady/retainer/internal/dailyfile"
)

type Options struct {
	DataDir       string
	Verbose       bool
	RetentionDays int
	Level         slog.Level
}

// Setup configures the logger. Returns the logger, a close function that
// flushes the rotating file, and any setup error.
func Setup(opts Options) (*slog.Logger, func() error, error) {
	if opts.DataDir == "" {
		return nil, nil, errors.New("logging: DataDir is required")
	}
	logsDir := filepath.Join(opts.DataDir, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("logging: mkdir logs: %w", err)
	}
	if opts.RetentionDays > 0 {
		if err := pruneOld(logsDir, opts.RetentionDays, time.Now()); err != nil {
			return nil, nil, fmt.Errorf("logging: retention sweep: %w", err)
		}
	}

	rw := dailyfile.NewWriter(logsDir, ".jsonl", time.Now)
	handlerOpts := &slog.HandlerOptions{Level: opts.Level}

	var handler slog.Handler = slog.NewJSONHandler(rw, handlerOpts)
	if opts.Verbose {
		text := slog.NewTextHandler(os.Stderr, handlerOpts)
		handler = &multiHandler{handlers: []slog.Handler{handler, text}}
	}

	return slog.New(handler), rw.Close, nil
}

// multiHandler fans out a record to multiple slog.Handlers. Used for verbose
// mode: JSON to disk, text to stderr.
type multiHandler struct {
	handlers []slog.Handler
}

func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	for _, h := range m.handlers {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if err := h.Handle(ctx, r.Clone()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		next[i] = h.WithAttrs(attrs)
	}
	return &multiHandler{handlers: next}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		next[i] = h.WithGroup(name)
	}
	return &multiHandler{handlers: next}
}

var datedFilePattern = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})\.jsonl$`)

// pruneOld removes dated log files older than retentionDays. Non-matching
// filenames are ignored.
func pruneOld(dir string, retentionDays int, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	cutoff := now.AddDate(0, 0, -retentionDays)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		match := datedFilePattern.FindStringSubmatch(e.Name())
		if match == nil {
			continue
		}
		t, err := time.Parse(dailyfile.DateLayout, match[1])
		if err != nil {
			continue
		}
		if t.Before(cutoff) {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
