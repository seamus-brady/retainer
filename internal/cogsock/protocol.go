// Package cogsock is the cog's local-IPC server: a Unix socket
// at <workspace>/data/cog.sock that accepts ndJSON clients (the
// webui today; future monitor / CLI wrappers tomorrow). The TUI
// stays in-process and bypasses the socket entirely; this package
// is for sibling processes that need the same Submit / Activities
// surface the TUI gets natively.
//
// Wire shape: one JSON object per line, both directions.
// Symmetric envelope with a `type` discriminator. Same discipline
// as `internal/ipc` — the wire is ndJSON, the protocol is a tiny
// state machine, and unknown types log + drop rather than abort
// the connection (forward-compat with future message types).
//
// Concurrency: one listener accepts; one goroutine per connected
// client. Each connection runs:
//   - a reader goroutine that decodes ClientMsg lines
//   - a writer goroutine that drains an outbound queue and an
//     Activity subscription onto the wire
// All writes go through a single mutex so the reader (when it
// needs to respond, e.g. pong) doesn't race the activity fan-out.
//
// Security: socket file is mode 0o600 (owner-only). The cog runs
// as the operator's user; siblings have to be the same user to
// connect. No additional auth — local-only by design.
package cogsock

import (
	"errors"
	"strings"
)

// Inbound message types (client → server).
const (
	// MsgTypeSubmit is "I have new user input for the cog."
	// Fields: Input.
	MsgTypeSubmit = "submit"
	// MsgTypePing requests a Pong response. Used for liveness.
	MsgTypePing = "ping"
	// MsgTypeSubscribeCycleLog opts the connection in to
	// per-cycle-log-event forwarding. Off by default to keep the
	// default chatter low — quiet UIs only see replies and
	// activities.
	MsgTypeSubscribeCycleLog = "subscribe_cycle_log"
)

// Outbound message types (server → client).
const (
	// MsgTypeReady is the server's first message: identifies the
	// cog (agent name + instance_id prefix) so the client can
	// confirm it's connected to the right workspace.
	MsgTypeReady = "ready"
	// MsgTypeReply is the cog's text response for one cycle.
	// Fields: CycleID, Body, ReplyKind ("text" / "refusal" / "error").
	MsgTypeReply = "reply"
	// MsgTypeActivity is one cog Activity event.
	MsgTypeActivity = "activity"
	// MsgTypeTrace is the cog's reply for an AUTONOMOUS cycle —
	// scheduler fires, comms-poller submits. The operator didn't
	// ask for the cycle but should see that the cog handled it,
	// so the webui renders these in the chat log with a distinct
	// muted style. Carries the same fields as MsgTypeReply
	// (CycleID, Body) plus TraceSource ("scheduler" | "comms" |
	// future receivers) so the UI can label the origin.
	MsgTypeTrace = "trace"
	// MsgTypeCycleEvent forwards a cyclelog.Event verbatim. Only
	// emitted to clients that sent subscribe_cycle_log.
	MsgTypeCycleEvent = "cycle_event"
	// MsgTypePong acknowledges a Ping.
	MsgTypePong = "pong"
	// MsgTypeError surfaces a server-side problem the client
	// should know about (decode error, cog inbox full, etc.).
	// Connection survives — error envelopes are advisory, not
	// terminal.
	MsgTypeError = "error"
)

// Error codes used in MsgTypeError envelopes. Plain strings so
// clients can switch on them without a generated enum.
const (
	ErrCodeMalformedLine    = "malformed_line"
	ErrCodeUnknownType      = "unknown_type"
	ErrCodeEmptyInput       = "empty_input"
	ErrCodeSubmitFailed     = "submit_failed"
	ErrCodeContextCancelled = "context_cancelled"
)

// ClientMsg is one inbound envelope. Optional fields stay
// `omitempty` so clients can write tight messages without
// repeating the whole shape.
type ClientMsg struct {
	Type string `json:"type"`

	// Submit-only.
	Input string `json:"input,omitempty"`
}

// ServerMsg is one outbound envelope. Discriminator is Type;
// type-specific fields are tagged by JSON convention.
type ServerMsg struct {
	Type string `json:"type"`

	// Common.
	Timestamp string `json:"timestamp,omitempty"`

	// Ready-only.
	AgentName  string `json:"agent_name,omitempty"`
	InstanceID string `json:"instance_id,omitempty"`

	// Reply-only.
	CycleID   string `json:"cycle_id,omitempty"`
	Body      string `json:"body,omitempty"`
	ReplyKind string `json:"reply_kind,omitempty"`

	// Activity-only.
	Status   string   `json:"status,omitempty"`
	TaskID   string   `json:"task_id,omitempty"`
	Turn     int      `json:"turn,omitempty"`
	MaxTurns int      `json:"max_turns,omitempty"`
	Tools    []string `json:"tools,omitempty"`
	// Activity-only — running token totals for the in-flight task
	// (cog or specialist agent). Updated on every LLM response.
	// Zero on transitions that don't follow an LLM call (idle).
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`

	// Activity-only — retry backoff metadata. Populated when
	// status == "retrying" so the UI can render the wait reason
	// and remaining time. Empty/zero for any other status.
	RetryAttempt     int    `json:"retry_attempt,omitempty"`
	RetryMaxAttempts int    `json:"retry_max_attempts,omitempty"`
	RetryDelayMs     int64  `json:"retry_delay_ms,omitempty"`
	RetryReason      string `json:"retry_reason,omitempty"`

	// Trace-only — autonomous-cycle reply surfaced to the operator's
	// chat log so scheduler / comms activity is visible.
	TraceSource string `json:"trace_source,omitempty"`

	// CycleEvent-only — embeds a cyclelog.Event directly so the
	// JSON shape on the wire matches the on-disk JSONL byte for
	// byte. Decoded as map[string]any so we don't pull cyclelog
	// into the protocol's type surface.
	Event map[string]any `json:"event,omitempty"`

	// Error-only.
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// ErrUnknownMessageType is returned by handlers when a client
// sends a Type the server doesn't know. Causes a `error`
// envelope back to the client; connection survives.
var ErrUnknownMessageType = errors.New("cogsock: unknown message type")

// Validate sanity-checks an inbound message. Used by the handler
// to surface malformed envelopes as ErrCodeMalformedLine without
// killing the connection.
func (m ClientMsg) Validate() error {
	switch m.Type {
	case MsgTypeSubmit:
		if strings.TrimSpace(m.Input) == "" {
			return errors.New("cogsock: submit requires non-empty input")
		}
	case MsgTypePing, MsgTypeSubscribeCycleLog:
		// no fields to validate
	case "":
		return errors.New("cogsock: message type is required")
	default:
		return ErrUnknownMessageType
	}
	return nil
}
