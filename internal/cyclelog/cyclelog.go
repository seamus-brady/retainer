// Package cyclelog records per-cycle telemetry as JSONL events. Each event
// is one line with type + cycle_id + timestamp + type-specific fields. The
// cog emits cycle_start at every UserInput, cycle_complete at every
// DeliverReply (or watchdog/error abandon), llm_request before dispatching
// the worker, llm_response on completion.
//
// The Writer is safe for concurrent Emit calls — typically the cog calls
// it inside its run loop, but future agents would also write through the
// same writer.
package cyclelog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

type EventType string

const (
	EventCycleStart       EventType = "cycle_start"
	EventCycleComplete    EventType = "cycle_complete"
	EventLLMRequest       EventType = "llm_request"
	EventLLMResponse      EventType = "llm_response"
	EventWatchdogFire     EventType = "watchdog_fire"
	EventPolicyDecision   EventType = "policy_decision"
	EventCuratorAssembled EventType = "curator_assembled"

	// EventToolTurnsExtended fires when the cog raises the
	// in-flight cycle's effective tool-turn cap via the
	// `request_more_turns` tool. CycleID is the parent cycle;
	// MaxTokens carries the cap-after value; Error carries the
	// (truncated) reason text. Audit-only — operators can grep
	// these to see when the cog needed budget headroom.
	EventToolTurnsExtended EventType = "tool_turns_extended"

	// LLMCurator (archivist's two-phase pipeline) per-call events.
	// One curator_reflection then one curator_curation per case-
	// worthy cycle when the LLMCurator is configured. Both carry
	// model + tokens + duration + success/error so cost telemetry
	// + reliability are observable separately from the cog's own
	// llm_request/llm_response stream. ParentID on both = the
	// cog cycle id that triggered the curation.
	EventCuratorReflection EventType = "curator_reflection"
	EventCuratorCuration   EventType = "curator_curation"

	// Agent react-loop events — emitted from inside agent.Agent and
	// the subprocess specialists. Each agent dispatch produces one
	// agent_cycle_start + one agent_cycle_complete bracketing zero
	// or more (llm_request + llm_response) and (tool_call +
	// tool_result) pairs. ParentID on each event = the agent's
	// own cycle id (TaskID); the agent_cycle_start carries
	// parent_id = the cog's cycle that triggered the dispatch.
	EventAgentCycleStart    EventType = "agent_cycle_start"
	EventAgentCycleComplete EventType = "agent_cycle_complete"
	EventToolCall           EventType = "tool_call"
	EventToolResult         EventType = "tool_result"
)

type CycleStatus string

const (
	StatusRunning  CycleStatus = "running"
	StatusComplete CycleStatus = "complete"
	StatusError    CycleStatus = "error"
	StatusAbandon  CycleStatus = "abandoned"
	StatusBlocked  CycleStatus = "blocked"
)

// Event is one record in the cycle log. Optional fields are omitted when
// unset; the type field discriminates which optional fields are meaningful.
type Event struct {
	Type      EventType `json:"type"`
	CycleID   string    `json:"cycle_id"`
	Timestamp time.Time `json:"timestamp"`

	// InstanceID is the 8-char prefix of the workspace's stable
	// agent UUID (see internal/agentid). Stamped on every event by
	// the cog and the agent emitter so external readers can group
	// events by Retainer instance — useful for multi-workspace
	// log aggregation. Optional; empty when identity load failed
	// at boot or when emitted by a test fixture.
	InstanceID string `json:"instance_id,omitempty"`

	// cycle_start / agent_cycle_start
	ParentID string `json:"parent_id,omitempty"`
	NodeType string `json:"node_type,omitempty"`

	// tool_call / tool_result
	ToolName    string `json:"tool_name,omitempty"`
	ToolInputLen int   `json:"tool_input_len,omitempty"`
	Success     bool   `json:"success,omitempty"`

	// llm_request
	Model        string `json:"model,omitempty"`
	MessageCount int    `json:"message_count,omitempty"`
	MaxTokens    int    `json:"max_tokens,omitempty"`

	// llm_response + curator_reflection + curator_curation
	StopReason   string `json:"stop_reason,omitempty"`
	InputTokens  int    `json:"input_tokens,omitempty"`
	OutputTokens int    `json:"output_tokens,omitempty"`
	// DurationMs is wall-clock time of the LLM call in milliseconds.
	// Populated on curator_reflection / curator_curation today;
	// reserved for llm_response when the cog gains the same
	// surface (a separate slice). No `omitempty` — 0ms is a
	// meaningful telemetry value (sub-millisecond mock or cached
	// call), distinct from "field not applicable to this event".
	DurationMs int64 `json:"duration_ms"`

	// cycle_complete / cycle_error
	Status CycleStatus `json:"status,omitempty"`
	Error  string      `json:"error,omitempty"`

	// policy_decision
	Gate         string  `json:"gate,omitempty"`
	Verdict      string  `json:"verdict,omitempty"`
	PolicyScore  float64 `json:"policy_score,omitempty"`
	PolicyTrail  string  `json:"policy_trail,omitempty"`
	Inconclusive bool    `json:"inconclusive,omitempty"`

	// curator_assembled
	PromptChars      int `json:"prompt_chars,omitempty"`
	NarrativeEntries int `json:"narrative_entries,omitempty"`
	FactSampleCount  int `json:"fact_sample_count,omitempty"`
	FactCount        int `json:"fact_count,omitempty"`
	// RecalledCaseIDs lists the (8-char prefix) IDs of cases the
	// sensorium <recalled_cases> block included this cycle. Empty
	// when retrieval was skipped (no input / nil librarian) or
	// returned no usable categorised results. Surfaced for audit
	// + integration assertions: "did this case actually reach the
	// agent?" without parsing the prompt body.
	RecalledCaseIDs []string `json:"recalled_case_ids,omitempty"`

	// Text carries the verbatim cycle text — operator's input
	// on cycle_start, assistant's reply on cycle_complete.
	// Populated by the cog so a per-day transcript view can be
	// reconstructed from the cycle log alone (no need to
	// cross-reference narrative + history). Optional; tests
	// and tools that don't surface chat history can ignore.
	Text string `json:"text,omitempty"`
}

// Writer serialises Events to an underlying writer (typically a
// dailyfile.Writer). Safe for concurrent calls.
type Writer struct {
	out io.Writer
	mu  sync.Mutex
}

func NewWriter(out io.Writer) *Writer {
	return &Writer{out: out}
}

// Emit writes one event. Returns the marshalling or write error.
func (w *Writer) Emit(ev Event) error {
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now()
	}
	body, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("cyclelog: marshal: %w", err)
	}
	body = append(body, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.out.Write(body); err != nil {
		return fmt.Errorf("cyclelog: write: %w", err)
	}
	return nil
}

// nopWriter is a Writer that drops events. Useful in tests where cog wiring
// should be transparent.
type nopWriter struct{}

func (nopWriter) Emit(Event) error { return nil }

// Sink is the minimal interface the cog needs. *Writer satisfies it; tests
// can pass NopSink() to drop events.
type Sink interface {
	Emit(Event) error
}

func NopSink() Sink { return nopWriter{} }

// Emitter wraps a Sink and auto-stamps InstanceID on every event,
// so callers don't repeat the InstanceID on every emission. Used by
// the cog and the agent substrate — both get an Emitter pointing at
// the same underlying Sink, both stamp the same InstanceID.
//
// Emitter is itself a Sink (implements Emit), so swapping a raw
// *Writer for a wrapped Emitter doesn't ripple type changes through
// callers.
type Emitter struct {
	sink       Sink
	instanceID string
}

// NewEmitter constructs a wrapper that stamps InstanceID on every
// forwarded event. Pass an empty instanceID to disable stamping
// (telemetry degrades to pre-identity behaviour).
func NewEmitter(sink Sink, instanceID string) *Emitter {
	return &Emitter{sink: sink, instanceID: instanceID}
}

// Emit stamps InstanceID (when non-empty AND the event doesn't
// already carry one) then forwards to the underlying sink. Honors
// caller-supplied InstanceID — useful for cross-process tests where
// a different instance writes through the same emitter chain.
func (e *Emitter) Emit(ev Event) error {
	if e.sink == nil {
		return nil
	}
	if ev.InstanceID == "" {
		ev.InstanceID = e.instanceID
	}
	return e.sink.Emit(ev)
}

// InstanceID returns the prefix the emitter stamps. Mostly for
// debugging / log-line context.
func (e *Emitter) InstanceID() string { return e.instanceID }

// cycleIDKey is the unexported context key for the in-flight cog cycle
// ID. The opaque struct{} type guarantees no other package can collide.
type cycleIDKey struct{}

// WithCycleID returns ctx with cycleID attached. The cog wraps the
// dispatch context with this so tool handlers (memory_write etc.) can
// stamp facts with the cycle that wrote them — Springdrift's
// FactsContext.cycle_id pattern, expressed via context.Context to avoid
// changing every Handler signature.
func WithCycleID(ctx context.Context, cycleID string) context.Context {
	return context.WithValue(ctx, cycleIDKey{}, cycleID)
}

// CycleIDFromContext returns the cycle ID injected by WithCycleID, or
// "" when none. Tools that don't need provenance (Brave, Jina) can
// ignore it.
func CycleIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(cycleIDKey{}).(string); ok {
		return v
	}
	return ""
}
