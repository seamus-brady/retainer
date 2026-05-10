package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/seamus-brady/retainer/internal/cogsock"
)

// fakeServer is a tiny stand-in for the cog's Unix socket. Each
// dial gets a goroutine that writes a `ready` envelope plus
// whatever else the test queues up. Used to exercise cogclient's
// reconnect, fan-out, and ping handling without depending on a
// real cog.
type fakeServer struct {
	listener net.Listener
	path     string

	mu       sync.Mutex
	conns    []net.Conn
	received []cogsock.ClientMsg
	scripts  []func(conn net.Conn) // optional: per-connection script
}

func startFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets not supported on Windows")
	}
	dir, err := os.MkdirTemp("/tmp", "wkcc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "cog.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeServer{listener: l, path: path}
	go fs.acceptLoop()
	return fs
}

func (f *fakeServer) acceptLoop() {
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns = append(f.conns, conn)
		var script func(net.Conn)
		if len(f.scripts) > 0 {
			script = f.scripts[0]
			f.scripts = f.scripts[1:]
		}
		f.mu.Unlock()
		go f.handle(conn, script)
	}
}

func (f *fakeServer) handle(conn net.Conn, script func(net.Conn)) {
	// Always write ready.
	body, _ := json.Marshal(cogsock.ServerMsg{
		Type: cogsock.MsgTypeReady, AgentName: "fake", InstanceID: "deadbeef",
	})
	body = append(body, '\n')
	_, _ = conn.Write(body)

	if script != nil {
		script(conn)
	}

	// Read client messages until disconnect.
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		var m cogsock.ClientMsg
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		f.mu.Lock()
		f.received = append(f.received, m)
		f.mu.Unlock()
		if m.Type == cogsock.MsgTypePing {
			pong, _ := json.Marshal(cogsock.ServerMsg{Type: cogsock.MsgTypePong})
			pong = append(pong, '\n')
			_, _ = conn.Write(pong)
		}
		if m.Type == cogsock.MsgTypeSubmit {
			reply, _ := json.Marshal(cogsock.ServerMsg{
				Type: cogsock.MsgTypeReply, Body: "ack: " + m.Input, ReplyKind: "text",
			})
			reply = append(reply, '\n')
			_, _ = conn.Write(reply)
		}
	}
}

func (f *fakeServer) Stop() {
	_ = f.listener.Close()
	f.mu.Lock()
	for _, c := range f.conns {
		_ = c.Close()
	}
	f.mu.Unlock()
}

func (f *fakeServer) Received() []cogsock.ClientMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]cogsock.ClientMsg(nil), f.received...)
}

// killActiveConnection closes whichever connection is currently
// open — used to test reconnect.
func (f *fakeServer) killActiveConnection() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.conns) > 0 {
		_ = f.conns[len(f.conns)-1].Close()
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestClient(path string) *cogClient {
	c := newCogClient(path, discardLogger())
	// Tighter timing for tests.
	c.dialBackoffMin = 20 * time.Millisecond
	c.dialBackoffMax = 100 * time.Millisecond
	c.pingInterval = 50 * time.Millisecond
	return c
}

func waitForReady(t *testing.T, c *cogClient) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.IsConnected() && c.Ready().Type == cogsock.MsgTypeReady {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("client never became ready")
}

func TestCogClient_ConnectsAndReceivesReady(t *testing.T) {
	fs := startFakeServer(t)
	defer fs.Stop()
	c := newTestClient(fs.path)
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Stop()
	waitForReady(t, c)
	if c.Ready().AgentName != "fake" {
		t.Errorf("AgentName = %q, want fake", c.Ready().AgentName)
	}
	if c.Ready().InstanceID != "deadbeef" {
		t.Errorf("InstanceID = %q, want deadbeef", c.Ready().InstanceID)
	}
}

func TestCogClient_SubmitSendsThroughTheWire(t *testing.T) {
	fs := startFakeServer(t)
	defer fs.Stop()
	c := newTestClient(fs.path)
	_ = c.Start(context.Background())
	defer c.Stop()
	waitForReady(t, c)

	if err := c.Send(cogsock.ClientMsg{Type: cogsock.MsgTypeSubmit, Input: "ping"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		got := fs.Received()
		for _, m := range got {
			if m.Type == cogsock.MsgTypeSubmit && m.Input == "ping" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server never received submit")
}

func TestCogClient_FanOutDeliversReply(t *testing.T) {
	fs := startFakeServer(t)
	defer fs.Stop()
	c := newTestClient(fs.path)
	_ = c.Start(context.Background())
	defer c.Stop()
	waitForReady(t, c)

	ch, cancel := c.Subscribe(8)
	defer cancel()
	if err := c.Send(cogsock.ClientMsg{Type: cogsock.MsgTypeSubmit, Input: "hi"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		select {
		case m := <-ch:
			if m.Type == cogsock.MsgTypeReply && m.Body == "ack: hi" {
				return
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("subscriber never received reply")
}

func TestCogClient_PingTriggersPong(t *testing.T) {
	fs := startFakeServer(t)
	defer fs.Stop()
	c := newTestClient(fs.path)
	_ = c.Start(context.Background())
	defer c.Stop()
	waitForReady(t, c)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got := fs.Received()
		for _, m := range got {
			if m.Type == cogsock.MsgTypePing {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("client never sent ping")
}

func TestCogClient_ReconnectsAfterDisconnect(t *testing.T) {
	fs := startFakeServer(t)
	defer fs.Stop()
	c := newTestClient(fs.path)
	_ = c.Start(context.Background())
	defer c.Stop()
	waitForReady(t, c)

	// Kill the active connection — read loop should detect EOF
	// and reconnect.
	fs.killActiveConnection()
	time.Sleep(100 * time.Millisecond) // let disconnect register

	// Connection should come back.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.IsConnected() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("client did not reconnect")
}

func TestCogClient_SendFailsWhenDisconnected(t *testing.T) {
	c := newTestClient("/tmp/never-exists/cog.sock")
	_ = c.Start(context.Background())
	defer c.Stop()
	// Briefly let the dial fail; client should NOT be connected.
	time.Sleep(100 * time.Millisecond)
	if c.IsConnected() {
		t.Fatal("expected disconnected client")
	}
	if err := c.Send(cogsock.ClientMsg{Type: cogsock.MsgTypeSubmit, Input: "x"}); err == nil {
		t.Error("Send should fail when disconnected")
	}
}

func TestCogClient_SubscribeCancelClosesChannel(t *testing.T) {
	fs := startFakeServer(t)
	defer fs.Stop()
	c := newTestClient(fs.path)
	_ = c.Start(context.Background())
	defer c.Stop()
	waitForReady(t, c)

	ch, cancel := c.Subscribe(2)
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel should be closed after cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("channel not closed after cancel")
	}
}
