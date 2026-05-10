package lockfile

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

func TestAcquire_FreshPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.lock")
	l, err := Acquire(path, 4242)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("lockfile not created: %v", err)
	}
	if got := HolderPID(path); got != 4242 {
		t.Errorf("HolderPID = %d, want 4242", got)
	}
}

func TestAcquire_NestedDirCreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "deep", "data")
	path := filepath.Join(dir, "cog.lock")
	l, err := Acquire(path, 1)
	if err != nil {
		t.Fatalf("Acquire should mkdir parents: %v", err)
	}
	defer l.Release()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("lockfile not created in nested dir: %v", err)
	}
}

func TestAcquire_DoubleAcquireSameProcessSucceeds(t *testing.T) {
	// Note: flock semantics on POSIX make repeated locks from the
	// SAME process an upgrade, not a contention. Cross-process
	// contention is what we actually care about — covered by the
	// subprocess test below.
	dir := t.TempDir()
	path := filepath.Join(dir, "x.lock")
	l1, err := Acquire(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer l1.Release()
	// Open a fresh FD from the same process. flock LOCK_NB
	// returns success because POSIX advisory locks aren't
	// per-FD-strict in the same process.
	l2, err := Acquire(path, 2)
	if err != nil && !errors.Is(err, ErrLocked) {
		t.Fatalf("unexpected error from same-process re-Acquire: %v", err)
	}
	if l2 != nil {
		_ = l2.Release()
	}
}

// TestAcquire_CrossProcessContention forks a child process that
// holds the lock and pings on stdout. Parent attempts Acquire and
// must get ErrLocked.
func TestAcquire_CrossProcessContention(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("flock semantics differ on Windows; skip")
	}
	if os.Getenv("WK_LOCK_HELPER") == "1" {
		// Helper mode: acquire the lock, write "ready", block
		// until stdin closes.
		path := os.Getenv("WK_LOCK_PATH")
		if path == "" {
			os.Exit(2)
		}
		l, err := Acquire(path, os.Getpid())
		if err != nil {
			os.Exit(3)
		}
		defer l.Release()
		_, _ = os.Stdout.WriteString("ready\n")
		_ = os.Stdout.Sync()
		// Block on stdin close.
		buf := make([]byte, 1)
		_, _ = os.Stdin.Read(buf)
		os.Exit(0)
		return
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "x.lock")

	cmd := exec.Command(os.Args[0], "-test.run=TestAcquire_CrossProcessContention")
	cmd.Env = append(os.Environ(),
		"WK_LOCK_HELPER=1",
		"WK_LOCK_PATH="+path,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})

	// Wait for "ready" — child has acquired.
	ready := make([]byte, 6)
	if _, err := stdout.Read(ready); err != nil {
		t.Fatalf("waiting for child ready: %v", err)
	}
	if string(ready) != "ready\n" {
		t.Fatalf("child did not signal ready; got %q", ready)
	}

	// Parent attempts Acquire — must fail with ErrLocked.
	l, err := Acquire(path, os.Getpid())
	if err == nil {
		_ = l.Release()
		t.Fatal("Acquire should have failed; child holds the lock")
	}
	if !errors.Is(err, ErrLocked) {
		t.Errorf("err = %v, want ErrLocked", err)
	}
}

func TestRelease_AllowsReacquire(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.lock")
	l, err := Acquire(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Errorf("Release: %v", err)
	}
	// Re-acquire should succeed since the lock is free + file
	// removed.
	l2, err := Acquire(path, 2)
	if err != nil {
		t.Fatalf("re-Acquire after Release: %v", err)
	}
	defer l2.Release()
}

func TestRelease_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.lock")
	l, err := Acquire(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	// Second Release is a no-op.
	if err := l.Release(); err != nil {
		t.Errorf("second Release should be no-op; got %v", err)
	}
}

func TestRelease_NilSafe(t *testing.T) {
	var l *Lock
	if err := l.Release(); err != nil {
		t.Errorf("nil Release should be no-op; got %v", err)
	}
}

func TestPath_NilEmpty(t *testing.T) {
	var l *Lock
	if got := l.Path(); got != "" {
		t.Errorf("nil Path = %q, want empty", got)
	}
}

func TestHolderPID_MissingFileReturnsZero(t *testing.T) {
	if got := HolderPID(filepath.Join(t.TempDir(), "missing")); got != 0 {
		t.Errorf("HolderPID on missing = %d, want 0", got)
	}
}

func TestHolderPID_MalformedReturnsZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lock")
	if err := os.WriteFile(path, []byte("not-a-pid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := HolderPID(path); got != 0 {
		t.Errorf("HolderPID on malformed = %d, want 0", got)
	}
}

func TestHolderPID_ReadsValidPID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lock")
	if err := os.WriteFile(path, []byte(strconv.Itoa(9876)), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := HolderPID(path); got != 9876 {
		t.Errorf("HolderPID = %d, want 9876", got)
	}
}

func TestAcquire_FileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode semantics differ on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "x.lock")
	l, err := Acquire(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	mode := info.Mode().Perm()
	if mode != 0o600 {
		t.Errorf("file mode = %o, want 0600 (lockfile may leak diagnostics on shared hosts)", mode)
	}
}
