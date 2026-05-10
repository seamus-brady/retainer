package cogsock

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/seamus-brady/retainer/internal/cog"
	"github.com/seamus-brady/retainer/internal/cyclelog"
	"github.com/seamus-brady/retainer/internal/policy"
)

// Submitter is the slice of *cog.Cog the server uses for inputs.
// Defining the interface here keeps the cogsock package
// substitutable in tests (a fake submitter exercises the
// connection logic without spinning up a real cog).
type Submitter interface {
	SubmitWithSource(ctx context.Context, text string, source policy.Source) <-chan cog.Reply
	Subscribe(buffer int) (<-chan cog.Activity, func())
	SubscribeTraces(buffer int) (<-chan cog.Trace, func())
	Provider() string
	Model() string
}

// Config wires the server into the cog. All fields except
// CycleLog are required.
type Config struct {
	// SocketPath is the absolute path to the Unix socket. The
	// server creates and removes it; parent dir must exist.
	SocketPath string

	// Cog is the cog the server forwards inputs to + subscribes
	// for activity.
	Cog Submitter

	// AgentName + InstanceID are echoed in the `ready` envelope
	// so clients confirm they connected to the right workspace.
	AgentName  string
	InstanceID string

	// PerSubscriberBuffer caps the per-connection Activity buffer
	// before drop-on-full kicks in. Zero defaults to 32.
	PerSubscriberBuffer int

	// CycleLogTap, when non-nil, is a channel the server reads
	// for cyclelog.Event events to forward to clients that opt in
	// via subscribe_cycle_log. Optional; leave nil to disable
	// cycle-event forwarding entirely.
	CycleLogTap <-chan cyclelog.Event

	// Logger receives server-side diagnostics.
	Logger *slog.Logger
}

const defaultPerSubscriberBuffer = 32

// Server is the running listener. Construct with New, run with
// Run (typically under actor supervision).
type Server struct {
	cfg      Config
	listener net.Listener

	// cycleEventBus fans out CycleLogTap to all subscribed
	// clients. Lazy-allocated when the first client subscribes.
	cycleMu      sync.Mutex
	cycleSubs    []chan<- cyclelog.Event
	cycleStarted bool
}

// New constructs a Server. Returns an error if config is invalid.
// Does NOT yet bind the socket — Run does that, so a supervised
// restart can rebind cleanly.
func New(cfg Config) (*Server, error) {
	if cfg.SocketPath == "" {
		return nil, errors.New("cogsock: SocketPath is required")
	}
	if cfg.Cog == nil {
		return nil, errors.New("cogsock: Cog is required")
	}
	if cfg.PerSubscriberBuffer == 0 {
		cfg.PerSubscriberBuffer = defaultPerSubscriberBuffer
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Server{cfg: cfg}, nil
}

// Run binds the socket, accepts clients, and serves them until
// ctx is cancelled. Returns nil on clean shutdown, non-nil on
// listen failure.
func (s *Server) Run(ctx context.Context) error {
	if err := s.bind(); err != nil {
		return err
	}
	defer s.cleanup()

	if s.cfg.CycleLogTap != nil {
		go s.fanCycleEvents(ctx)
	}

	// Accept loop runs in the foreground. ctx-cancel triggers
	// listener.Close which unblocks Accept.
	go func() {
		<-ctx.Done()
		_ = s.listener.Close()
	}()

	s.cfg.Logger.Info("cogsock: listening", "path", s.cfg.SocketPath)
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			if ctx.Err() != nil {
				return nil
			}
			s.cfg.Logger.Warn("cogsock: accept failed", "err", err)
			continue
		}
		go s.handle(ctx, conn)
	}
}

// bind creates the socket. If the path already exists and isn't a
// live listener, removes the stale file. Sets mode 0600.
func (s *Server) bind() error {
	if err := os.MkdirAll(filepath.Dir(s.cfg.SocketPath), 0o755); err != nil {
		return fmt.Errorf("cogsock: mkdir parent: %w", err)
	}
	// Stale-socket handling: if the file exists, try to dial it.
	// Live listener → another cog is running (lockfile would have
	// caught this earlier; defensive). Refused → stale, remove.
	if _, err := os.Stat(s.cfg.SocketPath); err == nil {
		if c, dialErr := net.DialTimeout("unix", s.cfg.SocketPath, 200*time.Millisecond); dialErr == nil {
			_ = c.Close()
			return fmt.Errorf("cogsock: socket %s appears live (another cog running?)", s.cfg.SocketPath)
		}
		if err := os.Remove(s.cfg.SocketPath); err != nil {
			return fmt.Errorf("cogsock: remove stale socket: %w", err)
		}
	}
	l, err := net.Listen("unix", s.cfg.SocketPath)
	if err != nil {
		return fmt.Errorf("cogsock: listen: %w", err)
	}
	if err := os.Chmod(s.cfg.SocketPath, 0o600); err != nil {
		_ = l.Close()
		return fmt.Errorf("cogsock: chmod 0600: %w", err)
	}
	s.listener = l
	return nil
}

// cleanup closes the listener and removes the socket file.
// Errors log; don't fail shutdown over them.
func (s *Server) cleanup() {
	if s.listener != nil {
		_ = s.listener.Close()
	}
	if err := os.Remove(s.cfg.SocketPath); err != nil && !os.IsNotExist(err) {
		s.cfg.Logger.Warn("cogsock: cleanup remove failed", "path", s.cfg.SocketPath, "err", err)
	}
}

// handle runs one client connection. Reader loop on conn,
// activity fan-out to the writer, mutex-serialised writes.
func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	logger := s.cfg.Logger.With("remote", conn.RemoteAddr().String())
	logger.Debug("cogsock: client connected")

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var writeMu sync.Mutex
	write := func(m ServerMsg) error {
		if m.Timestamp == "" {
			m.Timestamp = time.Now().UTC().Format(time.RFC3339)
		}
		body, err := json.Marshal(m)
		if err != nil {
			return fmt.Errorf("cogsock: marshal: %w", err)
		}
		body = append(body, '\n')
		writeMu.Lock()
		defer writeMu.Unlock()
		_, err = conn.Write(body)
		return err
	}

	// ready handshake — first envelope on every connection.
	if err := write(ServerMsg{
		Type:       MsgTypeReady,
		AgentName:  s.cfg.AgentName,
		InstanceID: s.cfg.InstanceID,
	}); err != nil {
		logger.Warn("cogsock: ready write failed", "err", err)
		return
	}

	// Activity fan-out — each conn gets its own subscription so
	// slow clients don't starve fast ones.
	actCh, actCancel := s.cfg.Cog.Subscribe(s.cfg.PerSubscriberBuffer)
	defer actCancel()

	go func() {
		for {
			select {
			case <-connCtx.Done():
				return
			case a, ok := <-actCh:
				if !ok {
					return
				}
				if err := write(ServerMsg{
					Type:             MsgTypeActivity,
					Status:           a.Status.String(),
					CycleID:          a.CycleID,
					Turn:             a.Turn,
					MaxTurns:         a.MaxTurns,
					Tools:            a.ToolNames,
					InputTokens:      a.InputTokens,
					OutputTokens:     a.OutputTokens,
					RetryAttempt:     a.RetryAttempt,
					RetryMaxAttempts: a.RetryMaxAttempts,
					RetryDelayMs:     a.RetryDelayMs,
					RetryReason:      a.RetryReason,
				}); err != nil {
					return
				}
			}
		}
	}()

	// Trace fan-out — autonomous-cycle replies (scheduler fires,
	// comms-poller submits). The operator didn't initiate these
	// cycles but should see them in the chat log; the webui
	// renders trace events with a distinct muted style.
	traceCh, traceCancel := s.cfg.Cog.SubscribeTraces(s.cfg.PerSubscriberBuffer)
	defer traceCancel()

	go func() {
		for {
			select {
			case <-connCtx.Done():
				return
			case t, ok := <-traceCh:
				if !ok {
					return
				}
				if err := write(ServerMsg{
					Type:        MsgTypeTrace,
					CycleID:     t.CycleID,
					Body:        t.Body,
					TraceSource: t.Source,
				}); err != nil {
					return
				}
			}
		}
	}()

	// Cycle-event subscription is opt-in per connection.
	var cycleCh chan cyclelog.Event
	var cycleCancel func()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var msg ClientMsg
		if err := json.Unmarshal(line, &msg); err != nil {
			_ = write(ServerMsg{
				Type:    MsgTypeError,
				Code:    ErrCodeMalformedLine,
				Message: err.Error(),
			})
			continue
		}
		if err := msg.Validate(); err != nil {
			code := ErrCodeMalformedLine
			if errors.Is(err, ErrUnknownMessageType) {
				code = ErrCodeUnknownType
			} else if msg.Type == MsgTypeSubmit {
				code = ErrCodeEmptyInput
			}
			_ = write(ServerMsg{
				Type:    MsgTypeError,
				Code:    code,
				Message: err.Error(),
			})
			continue
		}

		switch msg.Type {
		case MsgTypePing:
			_ = write(ServerMsg{Type: MsgTypePong})

		case MsgTypeSubmit:
			s.dispatchSubmit(connCtx, msg.Input, write)

		case MsgTypeSubscribeCycleLog:
			if cycleCh == nil {
				cycleCh, cycleCancel = s.subscribeCycleEvents(s.cfg.PerSubscriberBuffer)
				go s.pumpCycleEvents(connCtx, cycleCh, write)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		logger.Debug("cogsock: scanner err", "err", err)
	}
	if cycleCancel != nil {
		cycleCancel()
	}
	logger.Debug("cogsock: client disconnected")
}

// dispatchSubmit forwards one user input to the cog and writes
// back the Reply on the same connection.
func (s *Server) dispatchSubmit(ctx context.Context, input string, write func(ServerMsg) error) {
	replyCh := s.cfg.Cog.SubmitWithSource(ctx, input, policy.SourceInteractive)
	select {
	case <-ctx.Done():
		_ = write(ServerMsg{
			Type:    MsgTypeError,
			Code:    ErrCodeContextCancelled,
			Message: ctx.Err().Error(),
		})
	case r, ok := <-replyCh:
		if !ok {
			_ = write(ServerMsg{
				Type:    MsgTypeError,
				Code:    ErrCodeSubmitFailed,
				Message: "reply channel closed",
			})
			return
		}
		out := ServerMsg{
			Type:      MsgTypeReply,
			ReplyKind: r.Kind.String(),
			Body:      r.Text,
		}
		if r.Err != nil {
			// Prefer the cog's sanitised user-facing summary over
			// the raw provider error string. The cog populates
			// Reply.Text via FormatErrorForUser for every
			// ReplyKindError today; this is the defensive
			// fallback for any path that constructs a Reply
			// directly without going through abandonCycle.
			out.ReplyKind = cog.ReplyKindError.String()
			if r.Text != "" {
				out.Body = r.Text
			} else {
				out.Body = cog.FormatErrorForUser(r.Err)
			}
		}
		_ = write(out)
	}
}

// subscribeCycleEvents adds a per-conn channel to the cycle-event
// bus and returns it plus a cancel func. Lazy-starts the global
// fan-out goroutine on first subscriber.
func (s *Server) subscribeCycleEvents(buffer int) (chan cyclelog.Event, func()) {
	ch := make(chan cyclelog.Event, buffer)
	s.cycleMu.Lock()
	s.cycleSubs = append(s.cycleSubs, ch)
	s.cycleMu.Unlock()
	cancel := func() {
		s.cycleMu.Lock()
		defer s.cycleMu.Unlock()
		for i, sub := range s.cycleSubs {
			if sub == ch {
				s.cycleSubs[i] = s.cycleSubs[len(s.cycleSubs)-1]
				s.cycleSubs = s.cycleSubs[:len(s.cycleSubs)-1]
				close(ch)
				return
			}
		}
	}
	return ch, cancel
}

// fanCycleEvents reads CycleLogTap and broadcasts to every
// per-conn subscriber. Drop-on-full per subscriber. Started once
// from Run when CycleLogTap is non-nil.
func (s *Server) fanCycleEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-s.cfg.CycleLogTap:
			if !ok {
				return
			}
			s.cycleMu.Lock()
			subs := append([]chan<- cyclelog.Event(nil), s.cycleSubs...)
			s.cycleMu.Unlock()
			for _, sub := range subs {
				select {
				case sub <- ev:
				default:
				}
			}
		}
	}
}

// pumpCycleEvents drains one connection's cycle-event channel
// and writes ServerMsg{Type: cycle_event} on the wire.
func (s *Server) pumpCycleEvents(ctx context.Context, ch <-chan cyclelog.Event, write func(ServerMsg) error) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			body, err := cycleEventToMap(ev)
			if err != nil {
				continue
			}
			if err := write(ServerMsg{Type: MsgTypeCycleEvent, Event: body}); err != nil {
				return
			}
		}
	}
}

// cycleEventToMap round-trips a cyclelog.Event through JSON so
// the wire shape matches the on-disk JSONL byte for byte.
func cycleEventToMap(ev cyclelog.Event) (map[string]any, error) {
	body, err := json.Marshal(ev)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// fakeReadDeadline is documented for review-traceability — we do
// not impose a per-conn read deadline today. Long-lived
// connections (chat sessions) idle indefinitely; ping/pong is
// the client's liveness probe. If a future client wants
// idle-timeout enforcement, it lands here as a Config field.
var _ = io.EOF
