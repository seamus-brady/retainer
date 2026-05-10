package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/seamus-brady/retainer/internal/cogsock"
)

// cogClient holds a long-lived connection to the cog's Unix
// socket. Reconnects with exponential backoff on disconnect; pings
// every 30s for liveness. Server-sent envelopes are fanned out to
// every subscribed HTTP client (one subscriber per browser tab).
type cogClient struct {
	socketPath string
	logger     *slog.Logger

	// dialBackoff bounds the reconnect interval. Reset on
	// successful dial; doubles on each failure to dialBackoffMax.
	dialBackoffMin time.Duration
	dialBackoffMax time.Duration
	pingInterval   time.Duration

	mu        sync.Mutex
	conn      net.Conn
	connected bool
	ready     cogsock.ServerMsg

	// subs is the in-process fan-out: every connected SSE stream
	// adds a channel here. Drop-on-full per subscriber.
	subs []chan<- cogsock.ServerMsg

	wg     sync.WaitGroup
	cancel context.CancelFunc
}

// envelopes shaped this way so tests can construct them.
const (
	defaultDialBackoffMin = 250 * time.Millisecond
	defaultDialBackoffMax = 30 * time.Second
	defaultPingInterval   = 30 * time.Second
)

// newCogClient wires a client around socketPath. Defaults are
// suitable for production; tests can override the unexported
// fields after construction.
func newCogClient(socketPath string, logger *slog.Logger) *cogClient {
	return &cogClient{
		socketPath:     socketPath,
		logger:         logger,
		dialBackoffMin: defaultDialBackoffMin,
		dialBackoffMax: defaultDialBackoffMax,
		pingInterval:   defaultPingInterval,
	}
}

// Start dials the cog and launches the read + ping loops. Returns
// when the first dial completes (success or failure). Failure is
// logged but not returned — the reconnect loop will keep trying.
func (c *cogClient) Start(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel
	if err := c.dial(); err != nil {
		// First dial failure is logged; reconnect loop handles it.
		c.logger.Warn("cogclient: initial dial failed", "err", err)
	}
	c.wg.Add(2)
	go func() { defer c.wg.Done(); c.readLoop(runCtx) }()
	go func() { defer c.wg.Done(); c.pingLoop(runCtx) }()
	return nil
}

// Stop closes the connection and waits for read/ping goroutines to
// exit. Idempotent.
func (c *cogClient) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
	c.mu.Lock()
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.mu.Unlock()
	c.wg.Wait()
}

// IsConnected reports whether the client currently holds a live
// socket. Used by /api/health.
func (c *cogClient) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// Ready returns the most recent `ready` envelope from the cog.
// Empty until the first connect succeeds.
func (c *cogClient) Ready() cogsock.ServerMsg {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ready
}

// Send sends one client envelope to the cog. Returns an error if
// the connection isn't live or the write fails.
func (c *cogClient) Send(m cogsock.ClientMsg) error {
	c.mu.Lock()
	conn := c.conn
	connected := c.connected
	c.mu.Unlock()
	if !connected || conn == nil {
		return errors.New("cogclient: not connected")
	}
	body, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("cogclient: marshal: %w", err)
	}
	body = append(body, '\n')
	if _, err := conn.Write(body); err != nil {
		return fmt.Errorf("cogclient: write: %w", err)
	}
	return nil
}

// Subscribe registers a channel to receive every server envelope.
// Returns a cancel func that removes the subscriber and closes the
// channel. Drop-on-full per subscriber so a slow SSE consumer
// can't block the read loop.
func (c *cogClient) Subscribe(buffer int) (<-chan cogsock.ServerMsg, func()) {
	if buffer <= 0 {
		buffer = 32
	}
	ch := make(chan cogsock.ServerMsg, buffer)
	c.mu.Lock()
	c.subs = append(c.subs, ch)
	c.mu.Unlock()
	cancel := func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		for i, s := range c.subs {
			if s == ch {
				c.subs[i] = c.subs[len(c.subs)-1]
				c.subs = c.subs[:len(c.subs)-1]
				close(ch)
				return
			}
		}
	}
	return ch, cancel
}

// dial opens a fresh connection and stores it under the mutex.
// Sends `subscribe_cycle_log` on every reconnect so cycle events
// flow even after a cog restart.
func (c *cogClient) dial() error {
	conn, err := net.Dial("unix", c.socketPath)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.conn = conn
	c.connected = true
	c.mu.Unlock()
	return nil
}

// readLoop drains the cog's stdout. On every server envelope, fans
// out to every subscriber. On disconnect, reconnects with backoff.
func (c *cogClient) readLoop(ctx context.Context) {
	backoff := c.dialBackoffMin
	for {
		if ctx.Err() != nil {
			return
		}
		c.mu.Lock()
		conn := c.conn
		connected := c.connected
		c.mu.Unlock()
		if !connected || conn == nil {
			// Reconnect.
			if err := c.dial(); err != nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				if backoff < c.dialBackoffMax {
					backoff *= 2
					if backoff > c.dialBackoffMax {
						backoff = c.dialBackoffMax
					}
				}
				continue
			}
			backoff = c.dialBackoffMin
			continue
		}

		scanner := bufio.NewScanner(conn)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			var msg cogsock.ServerMsg
			if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
				c.logger.Warn("cogclient: malformed line", "err", err, "line", scanner.Text())
				continue
			}
			if msg.Type == cogsock.MsgTypeReady {
				c.mu.Lock()
				c.ready = msg
				c.mu.Unlock()
			}
			c.fanOut(msg)
		}
		// Read returned (EOF or error) — mark disconnected and
		// loop into reconnect.
		c.mu.Lock()
		_ = c.conn.Close()
		c.conn = nil
		c.connected = false
		c.mu.Unlock()
		c.fanOut(cogsock.ServerMsg{
			Type:    cogsock.MsgTypeError,
			Code:    "disconnected",
			Message: "cog connection lost; reconnecting",
		})
	}
}

// fanOut sends an envelope to every subscriber, drop-on-full.
func (c *cogClient) fanOut(m cogsock.ServerMsg) {
	c.mu.Lock()
	subs := append([]chan<- cogsock.ServerMsg(nil), c.subs...)
	c.mu.Unlock()
	for _, sub := range subs {
		select {
		case sub <- m:
		default:
		}
	}
}

// pingLoop sends a ping every pingInterval. The server's pong
// reply is just another envelope to the read loop — we don't
// track it explicitly here. If the connection is dead, Send
// returns an error and the read loop detects the disconnect on
// its own scanner.
func (c *cogClient) pingLoop(ctx context.Context) {
	t := time.NewTicker(c.pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = c.Send(cogsock.ClientMsg{Type: cogsock.MsgTypePing})
		}
	}
}
