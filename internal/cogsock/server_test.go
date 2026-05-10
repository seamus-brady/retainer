package cogsock

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seamus-brady/retainer/internal/cog"
	"github.com/seamus-brady/retainer/internal/cyclelog"
	"github.com/seamus-brady/retainer/internal/policy"
)

// fakeSubmitter implements Submitter without the real cog. The
// server tests exercise wire framing + connection lifecycle —
// real cog logic is covered in internal/cog/cog_test.go.
type fakeSubmitter struct {
	mu             sync.Mutex
	submits        []string
	replyFn        func(text string) cog.Reply
	replyDelay     time.Duration
	activitiesHub  *cog.Cog // we use a real Cog just for its hub plumbing
	staticActivity chan cog.Activity
}

func newFakeSubmitter() *fakeSubmitter {
	return &fakeSubmitter{
		replyFn: func(text string) cog.Reply {
			return cog.Reply{Kind: cog.ReplyKindText, Text: "ack: " + text}
		},
		staticActivity: make(chan cog.Activity, 16),
	}
}

func (f *fakeSubmitter) SubmitWithSource(ctx context.Context, text string, _ policy.Source) <-chan cog.Reply {
	ch := make(chan cog.Reply, 1)
	f.mu.Lock()
	f.submits = append(f.submits, text)
	delay := f.replyDelay
	rfn := f.replyFn
	f.mu.Unlock()
	go func() {
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				ch <- cog.Reply{Kind: cog.ReplyKindError, Err: ctx.Err()}
				return
			}
		}
		ch <- rfn(text)
	}()
	return ch
}

func (f *fakeSubmitter) Subscribe(buffer int) (<-chan cog.Activity, func()) {
	// Return a fresh channel; emitting events is up to the test
	// via PushActivity.
	ch := make(chan cog.Activity, buffer)
	f.mu.Lock()
	defer f.mu.Unlock()
	// Replace the test-controlled channel so callers can push.
	f.staticActivity = ch
	return ch, func() {}
}

func (f *fakeSubmitter) PushActivity(a cog.Activity) {
	f.mu.Lock()
	ch := f.staticActivity
	f.mu.Unlock()
	select {
	case ch <- a:
	default:
	}
}

// SubscribeTraces returns a no-op subscription. Trace forwarding
// is tested separately in TestServer_TraceForwardedAsTraceEnvelope
// via a dedicated fake; the unit-test fakeSubmitter is the
// activity-side double.
func (f *fakeSubmitter) SubscribeTraces(buffer int) (<-chan cog.Trace, func()) {
	ch := make(chan cog.Trace, buffer)
	return ch, func() { close(ch) }
}

func (f *fakeSubmitter) Provider() string { return "fake" }
func (f *fakeSubmitter) Model() string    { return "fake-model" }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// shortSocketPath returns a path under /tmp with a short random
// suffix. macOS limits Unix socket paths to 104 bytes; the
// default t.TempDir() is too long for some test names.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "wkck-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "cog.sock")
}

func startServer(t *testing.T, sub Submitter, opts ...func(*Config)) (*Server, string, func()) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets not supported on Windows")
	}
	path := shortSocketPath(t)
	cfg := Config{
		SocketPath: path,
		Cog:        sub,
		AgentName:  "retainer",
		InstanceID: "abc12345",
		Logger:     discardLogger(),
	}
	for _, o := range opts {
		o(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	// Wait for socket to appear.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(path); err != nil {
		cancel()
		<-done
		t.Fatalf("socket never appeared: %v", err)
	}
	teardown := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("server didn't stop")
		}
	}
	return srv, path, teardown
}

func dialAndRead(t *testing.T, path string) (net.Conn, *bufio.Scanner) {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return conn, sc
}

func nextMsg(t *testing.T, sc *bufio.Scanner) ServerMsg {
	t.Helper()
	if !sc.Scan() {
		t.Fatalf("scan failed: %v", sc.Err())
	}
	var m ServerMsg
	if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
		t.Fatalf("decode %q: %v", sc.Text(), err)
	}
	return m
}

func writeMsg(t *testing.T, conn net.Conn, m ClientMsg) {
	t.Helper()
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	if _, err := conn.Write(body); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// ---- Lifecycle ----

func TestServer_BindCreatesSocket(t *testing.T) {
	sub := newFakeSubmitter()
	_, path, teardown := startServer(t, sub)
	defer teardown()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %o, want 0600", info.Mode().Perm())
	}
}

func TestServer_ShutdownRemovesSocket(t *testing.T) {
	sub := newFakeSubmitter()
	_, path, teardown := startServer(t, sub)
	teardown()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("socket should be removed; stat err = %v", err)
	}
}

func TestServer_StaleSocketIsReplaced(t *testing.T) {
	path := shortSocketPath(t)
	// Drop a stale file.
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := newFakeSubmitter()
	srv, err := New(Config{
		SocketPath: path, Cog: sub, AgentName: "x", InstanceID: "y",
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	defer func() { cancel(); <-done }()
	// Socket should be a proper listener now.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c, err := net.Dial("unix", path)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("never able to dial after stale-socket replacement")
}

// ---- Ready handshake ----

func TestServer_ReadyOnConnect(t *testing.T) {
	sub := newFakeSubmitter()
	_, path, teardown := startServer(t, sub)
	defer teardown()
	conn, sc := dialAndRead(t, path)
	defer conn.Close()
	got := nextMsg(t, sc)
	if got.Type != MsgTypeReady {
		t.Errorf("first msg = %s, want ready", got.Type)
	}
	if got.AgentName != "retainer" || got.InstanceID != "abc12345" {
		t.Errorf("ready fields drift: %+v", got)
	}
}

// ---- Submit / reply ----

func TestServer_SubmitDeliversReply(t *testing.T) {
	sub := newFakeSubmitter()
	_, path, teardown := startServer(t, sub)
	defer teardown()
	conn, sc := dialAndRead(t, path)
	defer conn.Close()
	_ = nextMsg(t, sc) // ready

	writeMsg(t, conn, ClientMsg{Type: MsgTypeSubmit, Input: "hello"})
	got := nextMsg(t, sc)
	if got.Type != MsgTypeReply {
		t.Errorf("type = %s, want reply", got.Type)
	}
	if got.Body != "ack: hello" {
		t.Errorf("body = %q", got.Body)
	}
	if got.ReplyKind != "text" {
		t.Errorf("reply_kind = %q, want text", got.ReplyKind)
	}
}

func TestServer_SubmitErrorReplyPrefersTextOverRawErr(t *testing.T) {
	// Pin the error-leak fix: when the cog emits a Reply with both
	// Text (sanitised summary) and Err (raw provider error), the
	// server forwards Text — never the raw err.Error() string —
	// so the operator's webui never sees provider-internal noise
	// like "mistral: status 429: {...}".
	sub := newFakeSubmitter()
	sub.replyFn = func(text string) cog.Reply {
		return cog.Reply{
			Kind: cog.ReplyKindError,
			Text: "I hit a rate limit and couldn't recover after retries. Try again in a minute.",
			Err:  errors.New(`mistral: status 429: {"object":"error","message":"Rate limit exceeded"}`),
		}
	}
	_, path, teardown := startServer(t, sub)
	defer teardown()
	conn, sc := dialAndRead(t, path)
	defer conn.Close()
	_ = nextMsg(t, sc) // ready

	writeMsg(t, conn, ClientMsg{Type: MsgTypeSubmit, Input: "anything"})
	got := nextMsg(t, sc)
	if got.ReplyKind != "error" {
		t.Errorf("reply_kind = %q, want error", got.ReplyKind)
	}
	if got.Body == "" {
		t.Fatal("Body empty; should carry sanitised summary")
	}
	for _, leak := range []string{"mistral", "status 429", `{"object":`, "rate_limited"} {
		if strings.Contains(got.Body, leak) {
			t.Errorf("Body leaks provider-internal token %q: %s", leak, got.Body)
		}
	}
	if !strings.Contains(strings.ToLower(got.Body), "rate limit") {
		t.Errorf("Body should mention rate limit (sanitised); got %q", got.Body)
	}
}

func TestServer_SubmitErrorReplyFallsBackToFormatWhenTextEmpty(t *testing.T) {
	// Defensive — if a Reply arrives with only Err and no Text
	// (legacy / direct-construction path), the server runs it
	// through FormatErrorForUser rather than dumping err.Error().
	sub := newFakeSubmitter()
	sub.replyFn = func(text string) cog.Reply {
		return cog.Reply{
			Kind: cog.ReplyKindError,
			// Text deliberately empty.
			Err: errors.New(`mistral: status 429: rate limited`),
		}
	}
	_, path, teardown := startServer(t, sub)
	defer teardown()
	conn, sc := dialAndRead(t, path)
	defer conn.Close()
	_ = nextMsg(t, sc) // ready

	writeMsg(t, conn, ClientMsg{Type: MsgTypeSubmit, Input: "x"})
	got := nextMsg(t, sc)
	if strings.Contains(got.Body, "mistral") || strings.Contains(got.Body, "status 429") {
		t.Errorf("fallback should sanitise; got %q", got.Body)
	}
	if !strings.Contains(strings.ToLower(got.Body), "rate limit") {
		t.Errorf("fallback should classify rate limit; got %q", got.Body)
	}
}

func TestServer_SubmitEmptyInputErrors(t *testing.T) {
	sub := newFakeSubmitter()
	_, path, teardown := startServer(t, sub)
	defer teardown()
	conn, sc := dialAndRead(t, path)
	defer conn.Close()
	_ = nextMsg(t, sc) // ready

	writeMsg(t, conn, ClientMsg{Type: MsgTypeSubmit, Input: ""})
	got := nextMsg(t, sc)
	if got.Type != MsgTypeError {
		t.Errorf("type = %s, want error", got.Type)
	}
	if got.Code != ErrCodeEmptyInput {
		t.Errorf("code = %s, want %s", got.Code, ErrCodeEmptyInput)
	}
}

func TestServer_MalformedLineErrors(t *testing.T) {
	sub := newFakeSubmitter()
	_, path, teardown := startServer(t, sub)
	defer teardown()
	conn, sc := dialAndRead(t, path)
	defer conn.Close()
	_ = nextMsg(t, sc) // ready

	if _, err := conn.Write([]byte("{not valid json\n")); err != nil {
		t.Fatal(err)
	}
	got := nextMsg(t, sc)
	if got.Type != MsgTypeError {
		t.Errorf("type = %s, want error", got.Type)
	}
	if got.Code != ErrCodeMalformedLine {
		t.Errorf("code = %s, want %s", got.Code, ErrCodeMalformedLine)
	}
}

func TestServer_UnknownTypeErrors(t *testing.T) {
	sub := newFakeSubmitter()
	_, path, teardown := startServer(t, sub)
	defer teardown()
	conn, sc := dialAndRead(t, path)
	defer conn.Close()
	_ = nextMsg(t, sc) // ready

	writeMsg(t, conn, ClientMsg{Type: "telephone"})
	got := nextMsg(t, sc)
	if got.Type != MsgTypeError {
		t.Errorf("type = %s, want error", got.Type)
	}
	if got.Code != ErrCodeUnknownType {
		t.Errorf("code = %s, want %s", got.Code, ErrCodeUnknownType)
	}
}

// ---- Ping / pong ----

func TestServer_PingPong(t *testing.T) {
	sub := newFakeSubmitter()
	_, path, teardown := startServer(t, sub)
	defer teardown()
	conn, sc := dialAndRead(t, path)
	defer conn.Close()
	_ = nextMsg(t, sc) // ready

	writeMsg(t, conn, ClientMsg{Type: MsgTypePing})
	got := nextMsg(t, sc)
	if got.Type != MsgTypePong {
		t.Errorf("type = %s, want pong", got.Type)
	}
}

// ---- Multiple concurrent clients ----

func TestServer_MultipleConcurrentClientsEachGetReady(t *testing.T) {
	sub := newFakeSubmitter()
	_, path, teardown := startServer(t, sub)
	defer teardown()

	const n = 5
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			conn, sc := dialAndRead(t, path)
			defer conn.Close()
			got := nextMsg(t, sc)
			if got.Type != MsgTypeReady {
				t.Errorf("missing ready for client; got %v", got.Type)
			}
		}()
	}
	wg.Wait()
}

// ---- Connection survives errors ----

func TestServer_ConnectionSurvivesProtocolError(t *testing.T) {
	sub := newFakeSubmitter()
	_, path, teardown := startServer(t, sub)
	defer teardown()
	conn, sc := dialAndRead(t, path)
	defer conn.Close()
	_ = nextMsg(t, sc) // ready

	writeMsg(t, conn, ClientMsg{Type: "garbage"})
	_ = nextMsg(t, sc) // error

	// Now a valid submit should still work.
	writeMsg(t, conn, ClientMsg{Type: MsgTypeSubmit, Input: "after-error"})
	got := nextMsg(t, sc)
	if got.Type != MsgTypeReply || got.Body != "ack: after-error" {
		t.Errorf("expected reply after error envelope; got %+v", got)
	}
}

// ---- Cycle event subscription (opt-in) ----

func TestServer_CycleEventsNotForwardedWithoutSubscription(t *testing.T) {
	// One connection, no subscribe — events pushed into the tap
	// must NOT arrive on the wire.
	sub := newFakeSubmitter()
	tap := make(chan cyclelog.Event, 4)
	_, path, teardown := startServer(t, sub, func(c *Config) {
		c.CycleLogTap = tap
	})
	defer teardown()
	defer close(tap)

	conn, sc := dialAndRead(t, path)
	defer conn.Close()
	_ = nextMsg(t, sc) // ready

	tap <- cyclelog.Event{Type: cyclelog.EventCycleStart, CycleID: "x"}

	conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err == nil && n > 0 {
		t.Errorf("unexpected bytes without subscription: %s", buf[:n])
	}
}

func TestServer_CycleEventsForwardedAfterSubscription(t *testing.T) {
	// Separate connection in a separate test so the read-deadline
	// state from the negative case doesn't bleed in. Push events
	// in a tight loop so the first one to land after subscriber
	// registration arrives without depending on a fixed sleep.
	sub := newFakeSubmitter()
	tap := make(chan cyclelog.Event, 4)
	_, path, teardown := startServer(t, sub, func(c *Config) {
		c.CycleLogTap = tap
	})
	defer teardown()
	defer close(tap)

	conn, sc := dialAndRead(t, path)
	defer conn.Close()
	_ = nextMsg(t, sc) // ready

	writeMsg(t, conn, ClientMsg{Type: MsgTypeSubscribeCycleLog})

	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				select {
				case tap <- cyclelog.Event{Type: cyclelog.EventCycleStart, CycleID: "after-sub"}:
				default:
				}
			}
		}
	}()
	defer close(stop)

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := nextMsg(t, sc)
	if got.Type != MsgTypeCycleEvent {
		t.Fatalf("type = %s, want cycle_event", got.Type)
	}
	if got.Event["cycle_id"] != "after-sub" {
		t.Errorf("cycle_id = %v, want after-sub", got.Event["cycle_id"])
	}
}

// ---- Config validation ----

func TestNew_RequiresSocketPath(t *testing.T) {
	if _, err := New(Config{Cog: newFakeSubmitter()}); err == nil {
		t.Error("missing SocketPath should fail")
	}
}

func TestNew_RequiresCog(t *testing.T) {
	if _, err := New(Config{SocketPath: "/tmp/x"}); err == nil {
		t.Error("missing Cog should fail")
	}
}

func TestNew_DefaultsBuffer(t *testing.T) {
	srv, err := New(Config{SocketPath: "/tmp/x", Cog: newFakeSubmitter()})
	if err != nil {
		t.Fatal(err)
	}
	if srv.cfg.PerSubscriberBuffer != defaultPerSubscriberBuffer {
		t.Errorf("buffer = %d, want %d", srv.cfg.PerSubscriberBuffer, defaultPerSubscriberBuffer)
	}
}
