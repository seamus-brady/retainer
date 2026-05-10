// Package lockfile is a tiny advisory-lock helper around fcntl
// flock. Used by the cog to enforce single-instance per workspace
// (only one `retainer` process talking to a given workspace's
// data dir).
//
// Wire shape: a regular file at the configured path. Body is the
// PID of the holder, written for diagnostics ("which process holds
// it?"). The lock itself is the kernel's flock entry on the file
// descriptor — releasing the FD or exiting the process drops the
// lock automatically, so a crashed cog leaves a lock-claimable
// file behind even if the file body still names the dead PID.
//
// Why advisory + flock rather than a PID file alone: a stale PID
// file from a crashed boot is indistinguishable from a live one
// without spelunking /proc. flock cuts that knot — the OS knows
// whether the original holder still has the FD open.
//
// Concurrency rules:
//   - Acquire is non-blocking (LOCK_NB). Returns ErrLocked if
//     another process already holds the lock.
//   - Release closes the FD and removes the file. Idempotent.
//   - The Lock object is not safe for concurrent Release calls;
//     the holder is one goroutine (typically the cog's main
//     boot/shutdown thread).
package lockfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
)

// ErrLocked is returned by Acquire when another process holds the
// lock. Callers print a friendly "another instance is running"
// message; we don't try to interpret the holder PID's liveness
// because flock has already done that for us.
var ErrLocked = errors.New("lockfile: another process holds the lock")

// Lock is one acquired lock. Hold the value for the lifetime you
// want the lock; call Release on shutdown.
type Lock struct {
	path string
	file *os.File
	once sync.Once
}

// Acquire opens or creates path, takes an exclusive non-blocking
// flock, and writes the supplied pid to the body for diagnostics.
// Returns ErrLocked when the lock is held elsewhere; any other
// error is filesystem-related (permission denied, parent dir
// missing).
//
// The caller owns the returned *Lock and must Release it on
// graceful shutdown. On crash, the kernel drops the flock entry
// when the process exits, so a fresh Acquire from a new boot
// succeeds without manual cleanup.
func Acquire(path string, pid int) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("lockfile: ensure parent dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lockfile: open %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lockfile: flock %s: %w", path, err)
	}
	// Truncate + write the holder's PID. Best-effort: a write
	// failure here doesn't unlock — the flock is the source of
	// truth, the body is just diagnostics.
	if err := f.Truncate(0); err == nil {
		_, _ = f.Seek(0, 0)
		_, _ = f.WriteString(strconv.Itoa(pid))
	}
	return &Lock{path: path, file: f}, nil
}

// Release drops the flock and removes the file. Idempotent — a
// second call is a no-op.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	var firstErr error
	l.once.Do(func() {
		if l.file != nil {
			// Best-effort unlock + close — the close alone drops
			// the flock since the kernel ties the lock to the FD.
			_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
			if err := l.file.Close(); err != nil {
				firstErr = fmt.Errorf("lockfile: close: %w", err)
			}
		}
		// Best-effort remove. If a different process has already
		// claimed the path (rare race during shutdown), letting
		// the remove fail is the right thing — we don't want to
		// nuke their lockfile.
		if err := os.Remove(l.path); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = fmt.Errorf("lockfile: remove: %w", err)
		}
	})
	return firstErr
}

// Path returns the filesystem path the lock occupies. Useful for
// log lines and for tests asserting cleanup.
func (l *Lock) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// HolderPID reads the lockfile body and returns the PID stored
// there, or 0 if the file is missing / unreadable / malformed.
// Callers use this for diagnostic log lines ("locked by pid 1234")
// — never for liveness decisions, which flock already answered.
func HolderPID(path string) int {
	body, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(string(body))
	if err != nil {
		return 0
	}
	return pid
}
