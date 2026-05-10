// Package cog is the cognitive loop actor — a goroutine + tagged-message
// inbox + status machine + watchdog. Replaces the previous synchronous
// agent.Run with the actor shape required by Springdrift's cognitive-loop.md.
//
// State is mutated only inside the run loop. Worker goroutines (LLM calls,
// canary policy probes) post results back via the inbox; the cog never
// blocks on I/O. The cog is run under actor.Permanent — panics in cog code
// are recovered by the restart loop.
//
// Status flow per cycle:
//   Idle → EvaluatingPolicy(input) → Thinking → (output policy inline) → Idle
//
// If Policy is nil, EvaluatingPolicy is skipped: Idle → Thinking → Idle.
package cog

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/seamus-brady/retainer/internal/actor"
	"github.com/seamus-brady/retainer/internal/agent"
	"github.com/seamus-brady/retainer/internal/ambient"
	"github.com/seamus-brady/retainer/internal/archivist"
	"github.com/seamus-brady/retainer/internal/cyclelog"
	"github.com/seamus-brady/retainer/internal/dag"
	"github.com/seamus-brady/retainer/internal/librarian"
	"github.com/seamus-brady/retainer/internal/llm"
	"github.com/seamus-brady/retainer/internal/policy"
)

const (
	defaultInputQueueCap = 10
	defaultGateTimeout   = 60 * time.Second
	inboxBufferSize      = 64
	// defaultMaxToolTurns bounds the cog's react loop within a
	// single cycle when no operator override is configured. 10 sits
	// above the realistic envelope for a multi-step request that
	// reads a skill + recalls + delegates + summarises (≈5 turns)
	// without leaving the cog room to recover. The previous default
	// (5) was too tight for routine consultation-then-act flows.
	defaultMaxToolTurns = 10
	// hardMaxToolTurns is the absolute ceiling — even an in-cycle
	// `request_more_turns` call cannot exceed this. Twenty is enough
	// for genuinely long diagnostic / stress sequences while
	// remaining a hard backstop against runaway tool loops.
	hardMaxToolTurns     = 20
	activitiesBufferSize = 16

	// defaultAmbientBufferCap caps the lossy ambient-signal buffer.
	// When producers push faster than the cog cycles (rare today, more
	// likely once forecaster/observer publish autonomously), the oldest
	// signals drop. Sized small because each cycle drains in full —
	// signals don't survive a cycle by design.
	defaultAmbientBufferCap = 32

	// toolGateRefusal is the tool_result content the cog injects when
	// the policy tool gate blocks a call. The model sees IsError=true
	// + this string and can recover or refuse.
	toolGateRefusal = "tool blocked by policy"

	// postExecRefusal replaces a tool's actual output when the
	// post-execution gate blocks it. The model sees a clean (non-error)
	// tool_result with this content — the actual output is never
	// surfaced.
	postExecRefusal = "tool output redacted by policy"
)

type Status int

const (
	StatusIdle Status = iota
	StatusEvaluatingPolicy
	StatusThinking
	StatusUsingTools
	// StatusRetrying is emitted by the LLM retry layer during a
	// backoff sleep. The cog goroutine itself is blocked inside
	// the provider call — without this surfacing, the operator
	// sees ~50s of silence and thinks the agent died. Retry
	// metadata (attempt, max, delay, reason) carries the
	// "what's happening" detail.
	StatusRetrying
)

func (s Status) String() string {
	switch s {
	case StatusIdle:
		return "idle"
	case StatusEvaluatingPolicy:
		return "evaluating_policy"
	case StatusThinking:
		return "thinking"
	case StatusUsingTools:
		return "using_tools"
	case StatusRetrying:
		return "retrying"
	}
	return "unknown"
}

// ToolDispatcher is the seam between the cog's react loop and a tool
// registry. List() advertises the tools to the provider; Dispatch executes
// one by name with the model's raw JSON args. A non-nil error is treated as
// a tool-level failure — the cog still loops, but the result block has
// IsError=true so the model can recover.
type ToolDispatcher interface {
	List() []llm.Tool
	Dispatch(ctx context.Context, name string, input []byte) (string, error)
}

// NarrativeArchivist is the slice of *archivist.Archivist the cog uses.
// Receives end-of-cycle CycleComplete messages. Letting the cog depend
// on this interface lets tests substitute a fake without coupling to
// the archivist implementation.
type NarrativeArchivist interface {
	Record(msg archivist.CycleComplete)
}

// Activity is an ambient signal of what the cog is currently doing.
// Pushed on every status transition + on tool dispatch + on each
// react-loop turn boundary. Lossy by design: when no subscriber is
// reading or the buffer is full, updates are dropped. The next event
// supersedes the previous one anyway, so a stale display catches up
// within milliseconds. Used by the TUI to render richer status than
// "thinking..." (e.g. "using tool: brave_web_search").
type Activity struct {
	// Status is the cog's current FSM state.
	Status Status
	// CycleID is the in-flight cycle, or "" when idle.
	CycleID string
	// Turn is the current react-loop turn (1-based) when in
	// StatusThinking/StatusUsingTools, or 0 otherwise.
	Turn int
	// MaxTurns is the configured react-loop ceiling, surfaced so the
	// TUI can render "turn 2/5".
	MaxTurns int
	// ToolNames is populated when Status == StatusUsingTools — the
	// names of the tools currently dispatching, in declaration order
	// from the LLM response.
	ToolNames []string
	// InputTokens / OutputTokens are running totals across the
	// in-flight cycle's LLM calls. Updated on every llm_response.
	// Subscribers (TUI, webui SSE) can render the current cost
	// without waiting for cycle_complete to land in the cycle log.
	InputTokens  int
	OutputTokens int

	// Retry metadata — populated only on Status == StatusRetrying.
	// Emitted by the LLM retry layer's OnBackoff callback so the UI
	// can render "rate limited, retry 2/5, 15s" instead of going
	// silent during backoff.
	RetryAttempt     int
	RetryMaxAttempts int
	RetryDelayMs     int64
	// RetryReason is one of "rate_limited" / "overloaded" /
	// "transient" — coarse classification matching the retry
	// layer's delay schedule.
	RetryReason string
}

// Message is the cog inbox sum type. Only the types defined in this package
// satisfy it.
type Message interface{ isCogMsg() }

// UserInput is a turn from any input source. Reply receives the cycle's
// outcome (TUI uses it; scheduler/comms-future fire-and-forget set it nil
// or use a routing wrapper).
//
// Source tags the input for the policy gate: SourceInteractive (TUI)
// gets Block→Escalate demotion so adversarial-prompt testing doesn't
// hard-block; SourceAutonomous (scheduler / comms / future webhook
// receivers) preserves the hard reject. New sources land cleanly by
// adding values to policy.Source.
type UserInput struct {
	Text   string
	Source policy.Source
	Reply  chan<- Reply
}

// noticeMsg carries an ambient.Signal from a Notice() caller into the
// cog goroutine where it can be appended to pendingAmbient under the
// run-loop's single-writer invariant.
type noticeMsg struct {
	signal ambient.Signal
}

// ReplyKind categorises a Reply for the TUI:
//   Text    — normal assistant response. Render as agent speech.
//   Refusal — agent declined (policy / safety). Still agent speech;
//             distinguished only for telemetry / styling.
//   Error   — transport, internal, or watchdog failure. Render as error.
type ReplyKind int

const (
	ReplyKindText ReplyKind = iota
	ReplyKindRefusal
	ReplyKindError
)

func (k ReplyKind) String() string {
	switch k {
	case ReplyKindText:
		return "text"
	case ReplyKindRefusal:
		return "refusal"
	case ReplyKindError:
		return "error"
	}
	return "unknown"
}

// Reply is delivered to the source of a UserInput when the cycle completes
// or is abandoned. Text is populated for ReplyKindText and ReplyKindRefusal,
// AND for ReplyKindError when the cog has a sanitised user-facing summary
// of the failure (the typical case — see internal/cog/errortext.go).
// Err is populated for ReplyKindError; UI consumers should prefer Text and
// fall back to a generic message rather than rendering Err.Error() — raw
// provider error strings ("mistral: status 429: ...") are not appropriate
// for the operator's chat surface. Err is preserved for slog and tests.
type Reply struct {
	Kind ReplyKind
	Text string
	Err  error
}

type inputPolicyComplete struct {
	reply   chan<- Reply
	cycleID string
	text    string // the original user text, needed to dispatch the LLM call after Allow
	result  policy.Result
}

type thinkComplete struct {
	reply      chan<- Reply
	cycleID    string
	content    []llm.ContentBlock
	stopReason string
	usage      llm.Usage
	err        error
}

type toolsComplete struct {
	reply   chan<- Reply
	cycleID string
	results []llm.ContentBlock // ToolResultBlock per ToolUseBlock from the response
	// records pairs each tool call with whether it succeeded.
	// Used by the archivist's curator as ground truth — without
	// it, the curator can only judge from prose and produces
	// rubbish cases (see memory-and-logging-audit). Built in the
	// dispatch goroutine, threaded through here so the cog's
	// run loop accumulates onto pendingToolCalls.
	records []ToolCallRecord
	// events carries (tool_name, output) pairs for the
	// fabrication scorer. Empty when the engine has no scorer
	// wired — building it has a per-tool string-copy cost we
	// only pay when the scorer needs the input.
	events []policy.ToolEvent
	err    error
}

// ToolCallRecord is one (tool_name, success) pair from a single
// dispatch — appended in dispatch order to the cog's
// pendingToolCalls and passed via CycleComplete to the archivist.
type ToolCallRecord struct {
	Name    string
	Success bool
}

// agentCompletionMsg carries one agent dispatch's CompletionRecord
// from a tool's OnDone callback into the cog's run loop. Buffered
// in the inbox so the call stays non-blocking on the dispatcher
// goroutine. The handler discards records whose parentCycleID
// doesn't match the current cycle — a stale dispatch from a
// timed-out previous cycle shouldn't pollute the next cycle's
// archivist payload.
type agentCompletionMsg struct {
	parentCycleID string
	record        agent.CompletionRecord
}

func (agentCompletionMsg) isCogMsg() {}

// requestMoreTurnsMsg carries an in-cycle request from the
// `request_more_turns` tool to extend the current cycle's
// effective tool-turn cap. Linearised through the cog's inbox so
// the state mutation happens on the cog goroutine — same pattern
// as agentCompletionMsg.
//
// The reply channel returns the new effective cap on success, or
// an error when the request would exceed hardMaxToolTurns or when
// no cycle is currently in flight.
type requestMoreTurnsMsg struct {
	additional int
	reason     string
	parentCycleID string
	reply      chan<- requestMoreTurnsResult
}

func (requestMoreTurnsMsg) isCogMsg() {}

// requestMoreTurnsResult is the synchronous reply to a
// requestMoreTurnsMsg. NewCap is the effective cap after the
// adjustment (current cap when refused, raised cap when granted).
type requestMoreTurnsResult struct {
	NewCap int
	Err    error
}

type watchdogFire struct{}

func (UserInput) isCogMsg()           {}
func (inputPolicyComplete) isCogMsg() {}
func (thinkComplete) isCogMsg()       {}
func (toolsComplete) isCogMsg()       {}
func (watchdogFire) isCogMsg()        {}
func (noticeMsg) isCogMsg()           {}

const (
	defaultInputRefusalText  = "I can't help with that."
	defaultOutputRefusalText = "I started to answer but stopped — could you rephrase?"
)

// CycleSnapshot is the per-cycle context the cog hands to SystemPromptFn.
// The assembler (curator) reads it to populate sensorium / slot data
// without having to re-derive cog-internal state.
//
// Snapshot fields are computed at the LLM-dispatch boundary: ambient
// signals are drained from the buffer at that moment; queue depth and
// message count reflect state at dispatch time, not at submit time.
type CycleSnapshot struct {
	// CycleID is the in-flight cycle's UUID (the SystemPromptFn caller
	// uses this for cycle-attribution / audit fields in the prompt).
	CycleID string
	// InputSource identifies what woke the cog up (interactive vs
	// autonomous). The curator surfaces this in <situation>.
	InputSource policy.Source
	// QueueDepth is the count of UserInputs buffered behind the
	// in-flight one at dispatch time.
	QueueDepth int
	// MessageCount is the conversation history length at dispatch time
	// (count of messages the LLM is being given this turn).
	MessageCount int
	// Ambient is the drained ambient-signals buffer for this cycle.
	// Empty when nothing's been Notice()'d since the previous cycle.
	// The signals don't survive past this cycle — once handed to the
	// curator they're consumed.
	Ambient []ambient.Signal
	// UserText is the operator's input text for this cycle (or the
	// scheduler-injected prompt for autonomous cycles). Carried so the
	// curator can derive a CBR retrieval query from it without re-
	// touching cog state. Empty on cycles with no triggering text
	// (rare — most paths capture pendingUserText at submit time).
	UserText string
}

type Config struct {
	Provider  llm.Provider
	Model     string
	MaxTokens int
	// SystemPromptFn returns the cache-aware system prompt for one cycle.
	// Called fresh per cycle so identity / time-sensitive / memory-derived
	// slots update. The CycleSnapshot carries cycle-attribution + ambient
	// signals + queue/message counts the assembler needs.
	// If nil, the cog sends an empty system prompt (deterministic
	// behaviour for tests).
	//
	// The returned llm.SystemPrompt has Stable + Dynamic halves; the
	// Anthropic adapter places a cache_control marker between them.
	// Other providers concatenate.
	SystemPromptFn func(ctx context.Context, snap CycleSnapshot) llm.SystemPrompt
	Logger         *slog.Logger
	// Policy is the safety/policy engine. If nil, gates are skipped and the
	// cog goes Idle → Thinking → Idle directly.
	Policy *policy.Engine
	// CycleLog receives per-cycle telemetry. If nil, NopSink is used.
	CycleLog cyclelog.Sink
	// InstanceID is the workspace's stable agent-identity prefix
	// (8 chars of the agent UUID). Stamped on every event the cog
	// emits so external readers can correlate cycle-log events
	// across processes + restarts. Empty disables stamping (drops
	// to pre-identity behaviour).
	InstanceID string
	// DAG records cycle parent/child relationships. May be nil to skip.
	DAG *dag.DAG
	// Librarian records narrative entries on cycle completion when
	// Archivist is nil. When Archivist is set, narrative writes route
	// through the archivist instead and Librarian becomes optional
	// (the archivist owns the librarian reference). Kept here as a
	// fallback for tests that don't construct an archivist.
	Librarian *librarian.Librarian
	// Archivist owns the post-cycle path (narrative writes, future
	// CBR-case derivation). When set, the cog sends `CycleComplete`
	// fire-and-forget at end-of-cycle and the archivist writes
	// narrative asynchronously. When nil, the cog falls back to
	// direct Librarian.RecordNarrative for backward compatibility
	// with existing tests.
	Archivist NarrativeArchivist
	// InputQueueCap caps buffered UserInputs while busy. Zero uses
	// defaultInputQueueCap.
	InputQueueCap int
	// GateTimeout arms the watchdog on every non-Idle status transition.
	// Zero uses defaultGateTimeout.
	GateTimeout time.Duration
	// InputRefusalText is what the agent says when input policy blocks /
	// escalates. Empty uses defaultInputRefusalText.
	InputRefusalText string
	// OutputRefusalText is what the agent says when output policy blocks.
	// Empty uses defaultOutputRefusalText.
	OutputRefusalText string
	// Tools is an optional dispatcher giving the model access to tools. If
	// nil, no tools are advertised and the cog runs single-turn.
	Tools ToolDispatcher
	// MaxToolTurns bounds the react loop within a single cycle. Zero uses
	// defaultMaxToolTurns.
	MaxToolTurns int
	// MaxContextMessages caps the running conversation history fed to the
	// LLM each cycle. When state.history grows past this many messages
	// the oldest are dropped. Zero / negative = no cap (conversation
	// grows unbounded until process restart). SD-faithful — the
	// reference deployment defaults to None / unbounded but exposes
	// the knob (config example shows 50). Tunable when token costs
	// or rate-limit pressure ask for it.
	MaxContextMessages int
	// AmbientBufferCap caps the in-memory ambient-signal buffer. Producers
	// (forecaster, observer, etc.) push via cog.Notice; the cog drains the
	// buffer at the start of each LLM dispatch and hands the signals to
	// SystemPromptFn. When the buffer is full new signals drop the oldest.
	// Zero uses defaultAmbientBufferCap.
	AmbientBufferCap int
}

type state struct {
	status          Status
	history         llm.MessageHistory
	pendingHistory  llm.MessageHistory
	pendingReply    chan<- Reply
	pendingCycleID  string
	pendingUserText string        // captured at UserInput so failure paths can record it
	pendingSource   policy.Source // input source for the in-flight cycle (drives the gate, surfaces in <situation>)
	pendingTurns    int           // number of LLM dispatches issued in the current cycle
	// pendingMaxToolTurns is the effective tool-turn cap for the
	// in-flight cycle. Initialised from cfg.MaxToolTurns at cycle
	// start; can be raised mid-cycle via the request_more_turns
	// tool up to hardMaxToolTurns. Reset in discardPending.
	pendingMaxToolTurns int
	pendingInTok        int           // running input tokens for the current cycle's LLM calls
	pendingOutTok       int           // running output tokens for the current cycle's LLM calls
	// pendingTools is the deduplicated list of tools dispatched
	// during the current cycle. Accumulated in continueWithTools;
	// drained at end-of-cycle into the archivist's CycleComplete so
	// derived CBR cases carry which tools were used.
	pendingTools            []string
	pendingToolCalls        []ToolCallRecord
	// pendingToolEvents accumulates the (name, output) pairs the
	// fabrication scorer needs to verify the cycle's final reply
	// against. Populated only when the policy engine has a
	// fabrication scorer wired (otherwise empty — saves the
	// per-tool string-copy cost on hot-path cycles where the
	// scorer is disabled). Drained after the output gate runs.
	pendingToolEvents       []policy.ToolEvent
	pendingAgentCompletions []agent.CompletionRecord
	queue                   []UserInput
	watchdogGen  uint64
	// pendingAmbient is the lossy buffer of ambient signals waiting to
	// be drained into the next cycle's <ambient> sensorium block. Bounded
	// by AmbientBufferCap; oldest signals drop when full. Drained by
	// dispatchLLM at the moment the LLM call is built.
	pendingAmbient []ambient.Signal
}

type Cog struct {
	cfg      Config
	inbox    chan Message
	state    state
	watchdog *actor.Watchdog

	// hub fans Activity events out to N subscribers (TUI in-process
	// + zero-or-more socket clients). The publish path is non-
	// blocking per subscriber so a stuck consumer can't wedge the
	// cog goroutine.
	hub *activityHub

	// activities is the back-compat channel returned by Activities().
	// Subscribed eagerly in New() so events emitted before the first
	// reader call buffer up. New consumers should use Subscribe().
	activities <-chan Activity

	// traces fans out Trace events — the cog's reply text for
	// AUTONOMOUS cycles (scheduler fires, comms-poller submits).
	// Operator-facing surfaces (TUI, webui) subscribe so the
	// operator sees that the cog handled an autonomous cycle even
	// though they didn't initiate it. Buffered + drop-on-full per
	// subscriber, same shape as the activity hub.
	traceHub *traceHub

	// currentCycleID is an atomic mirror of state.pendingCycleID so
	// goroutines outside the cog run loop (notably the LLM retry
	// layer's OnBackoff callback) can stamp their Activity events
	// with the in-flight cycle. Run loop is the sole writer; readers
	// are goroutine-safe via atomic.Pointer.
	currentCycleID atomic.Pointer[string]
}

// Trace is one autonomous-cycle reply surfaced to subscribers so
// operator-facing surfaces can render scheduler / comms traffic in
// the chat log. Distinct from the operator-driven Reply path —
// those go via the submitter's reply channel.
type Trace struct {
	CycleID string
	// Source is "scheduler" / "comms" / future receiver names.
	// Derived from the policy.Source on the autonomous input plus
	// any framing the submitter applied. The cog itself just sees
	// SourceAutonomous, so for V1 every Trace gets Source=
	// "autonomous"; the submitting subsystem could extend this
	// later by passing source metadata through the input.
	Source string
	// Body is the cog's final reply for the autonomous cycle.
	Body string
}

func New(cfg Config) *Cog {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.InputQueueCap == 0 {
		cfg.InputQueueCap = defaultInputQueueCap
	}
	if cfg.GateTimeout == 0 {
		cfg.GateTimeout = defaultGateTimeout
	}
	if cfg.CycleLog == nil {
		cfg.CycleLog = cyclelog.NopSink()
	}
	if cfg.InputRefusalText == "" {
		cfg.InputRefusalText = defaultInputRefusalText
	}
	if cfg.OutputRefusalText == "" {
		cfg.OutputRefusalText = defaultOutputRefusalText
	}
	if cfg.MaxToolTurns == 0 {
		cfg.MaxToolTurns = defaultMaxToolTurns
	}
	if cfg.AmbientBufferCap == 0 {
		cfg.AmbientBufferCap = defaultAmbientBufferCap
	}
	hub := newActivityHub()
	// Eagerly subscribe the back-compat channel so events emitted
	// between New() and the first Activities() call buffer up
	// instead of dropping. Matches the pre-hub behaviour callers
	// depend on (TUI subscribes on its first Update tick, which
	// can lose the race against the cog's first cycle otherwise).
	defaultCh, _ := hub.subscribe(activitiesBufferSize)
	return &Cog{
		cfg:        cfg,
		inbox:      make(chan Message, inboxBufferSize),
		watchdog:   actor.New(),
		hub:        hub,
		activities: defaultCh,
		traceHub:   newTraceHub(),
	}
}

// SubscribeTraces returns a channel that receives one Trace per
// completed AUTONOMOUS cycle (scheduler fires, comms-poller
// submits). Buffered + drop-on-full per subscriber so a slow
// reader can't wedge the cog. Operator-facing surfaces (TUI,
// webui SSE) subscribe so the operator sees scheduler / comms
// activity in the chat log even when they didn't initiate it.
func (c *Cog) SubscribeTraces(buffer int) (<-chan Trace, func()) {
	return c.traceHub.subscribe(buffer)
}

func (c *Cog) Provider() string { return c.cfg.Provider.Name() }
func (c *Cog) Model() string    { return c.cfg.Model }

// Activities returns the default Activity channel for in-process
// consumers (typically the TUI). The subscription is eager —
// allocated in New() — so events emitted before the first call
// buffer up rather than drop. Lossy on buffer overflow (status
// is ambient, next event supersedes).
//
// New consumers (socket clients, monitors) should use Subscribe()
// instead so each gets its own buffered channel and explicit
// cancel — Activities() is the back-compat path for the original
// single-subscriber TUI.
func (c *Cog) Activities() <-chan Activity { return c.activities }

// Subscribe registers a new Activity subscriber and returns a
// receive-only channel + a cancel func. Each subscriber gets its
// own buffer; slow consumers drop events independently of others.
// Cancel removes the subscription and closes the channel.
//
// Used by the cog socket listener (`internal/cogsock`) — every
// connected webui tab gets its own subscription so the TUI's
// activity feed doesn't compete with browser SSE streams.
func (c *Cog) Subscribe(buffer int) (<-chan Activity, func()) {
	return c.hub.subscribe(buffer)
}

// NotifyRetry surfaces an in-progress LLM retry backoff to
// subscribers. Safe to call from any goroutine — invoked by the
// retry layer's OnBackoff callback during a backoff sleep so the
// TUI / webui can render "rate limited, retry 2/5, 15s" instead
// of going silent for tens of seconds.
//
// CycleID is read from the atomic mirror of pendingCycleID so the
// event correlates with whatever cycle the operator submitted.
// Empty when no cycle is in flight (which shouldn't happen — the
// retry only fires inside a provider call — but is harmless).
//
// Status is StatusRetrying. The next normal Activity emission from
// the run loop (when the retry sleep completes and the LLM call
// resumes) will override the retrying state.
func (c *Cog) NotifyRetry(attempt, maxAttempts int, delay time.Duration, reason string) {
	cid := ""
	if p := c.currentCycleID.Load(); p != nil {
		cid = *p
	}
	c.hub.publish(Activity{
		Status:           StatusRetrying,
		CycleID:          cid,
		RetryAttempt:     attempt,
		RetryMaxAttempts: maxAttempts,
		RetryDelayMs:     delay.Milliseconds(),
		RetryReason:      reason,
	})
}

// emitActivity is a non-blocking publish. Callers must already hold
// the run-loop's single-writer invariant — only the cog goroutine
// emits. The hub fans out to every subscriber with per-subscriber
// drop-on-full semantics.
func (c *Cog) emitActivity(toolNames []string) {
	c.hub.publish(Activity{
		Status:       c.state.status,
		CycleID:      c.state.pendingCycleID,
		Turn:         c.state.pendingTurns,
		MaxTurns:     c.cfg.MaxToolTurns,
		ToolNames:    toolNames,
		InputTokens:  c.state.pendingInTok,
		OutputTokens: c.state.pendingOutTok,
	})
}

// Submit sends a UserInput to the cog as an interactive (TUI) operator
// turn and returns a buffered reply channel. The policy gate applies its
// Block→Escalate demotion to interactive sources (operator can be
// testing adversarial content without getting hard-blocked).
//
// If ctx is cancelled before the inbox accepts, the channel receives an
// error reply.
//
// Non-interactive callers (scheduler, comms wrapper, future webhook
// receivers) use SubmitWithSource so the gate sees the right Source
// value.
func (c *Cog) Submit(ctx context.Context, text string) <-chan Reply {
	return c.SubmitWithSource(ctx, text, policy.SourceInteractive)
}

// SubmitWithSource is the source-typed variant of Submit. The Source
// value drives policy-gate behaviour (interactive demotion vs autonomous
// hard-reject) and surfaces in the <situation> sensorium attribute. New
// sources land cleanly by adding values to policy.Source.
func (c *Cog) SubmitWithSource(ctx context.Context, text string, source policy.Source) <-chan Reply {
	reply := make(chan Reply, 1)
	select {
	case c.inbox <- UserInput{Text: text, Source: source, Reply: reply}:
	case <-ctx.Done():
		reply <- Reply{Err: ctx.Err()}
	}
	return reply
}

// Notice publishes an ambient signal that will surface in the next
// cycle's <ambient> sensorium block but does NOT trigger a cycle on its
// own. Distinct from Submit: Submit wakes the cog up to run; Notice
// shapes the next cycle's perception whenever it happens.
//
// Non-blocking by design — the buffer is lossy; if no in-flight cycle
// is consuming and the buffer is full, the oldest signal drops. The
// next cycle's perception catches up. Producers that need an audit
// trail beyond the LLM's view should write to cyclelog directly.
//
// Safe to call from any goroutine.
func (c *Cog) Notice(s ambient.Signal) {
	if s.Timestamp.IsZero() {
		s.Timestamp = time.Now()
	}
	select {
	case c.inbox <- noticeMsg{signal: s}:
	default:
		// Inbox full — drop the signal. Cog is heavily loaded;
		// dropping ambient awareness is the right thing here.
	}
}

// Run is the actor loop. Block until ctx is cancelled. Wrap with actor.Run
// under actor.Permanent so panics restart the loop without losing the
// process.
func (c *Cog) Run(ctx context.Context) error {
	c.cfg.Logger.Info("cog started", "provider", c.Provider(), "model", c.Model())
	defer c.cfg.Logger.Info("cog stopped")

	for {
		select {
		case <-ctx.Done():
			c.disarmWatchdog()
			return ctx.Err()
		case msg := <-c.inbox:
			c.handle(ctx, msg)
		}
	}
}

func (c *Cog) handle(ctx context.Context, msg Message) {
	switch m := msg.(type) {
	case UserInput:
		c.onUserInput(ctx, m)
	case inputPolicyComplete:
		c.onInputPolicyComplete(ctx, m)
	case thinkComplete:
		c.onThinkComplete(ctx, m)
	case toolsComplete:
		c.onToolsComplete(ctx, m)
	case watchdogFire:
		c.onWatchdog()
	case noticeMsg:
		c.onNotice(m)
	case agentCompletionMsg:
		c.onAgentCompletion(m)
	case requestMoreTurnsMsg:
		c.onRequestMoreTurns(m)
	}
	c.drainQueue(ctx)
}

// onNotice appends an ambient signal to the buffer, dropping the oldest
// when the buffer is full. Single-writer (cog goroutine only) so the
// slice mutation is safe. Producers must use cog.Notice rather than
// touching the slice directly.
func (c *Cog) onNotice(m noticeMsg) {
	if c.cfg.AmbientBufferCap <= 0 {
		return
	}
	if len(c.state.pendingAmbient) >= c.cfg.AmbientBufferCap {
		// Drop the oldest. Lossy by design — fresher signals are
		// more relevant for the next cycle's perception.
		c.state.pendingAmbient = c.state.pendingAmbient[1:]
	}
	c.state.pendingAmbient = append(c.state.pendingAmbient, m.signal)
}

// RecordAgentCompletion is the entry point tools (DelegateToAgent /
// SubprocessDelegate) call via their OnDone callback after each
// completed agent dispatch. Posts an inbox message; the cog
// goroutine accumulates into pendingAgentCompletions for the
// current cycle.
//
// Non-blocking — a full inbox drops the record with a warning.
// Same fire-and-forget shape as Notice. Stale records (whose
// parentCycleID doesn't match the current cycle) are discarded
// in onAgentCompletion.
func (c *Cog) RecordAgentCompletion(parentCycleID string, rec agent.CompletionRecord) {
	select {
	case c.inbox <- agentCompletionMsg{parentCycleID: parentCycleID, record: rec}:
	default:
		c.cfg.Logger.Warn("cog: agent-completion inbox full; dropping",
			"agent", rec.AgentName, "parent_cycle_id", parentCycleID,
		)
	}
}

// onAgentCompletion handles one agent-dispatch completion record.
// Stale records (parentCycleID != current pending cycle) are
// dropped — a dispatch that lands after the cog has moved on
// shouldn't pollute the next cycle's archivist payload.
func (c *Cog) onAgentCompletion(m agentCompletionMsg) {
	if m.parentCycleID == "" || m.parentCycleID != c.state.pendingCycleID {
		c.cfg.Logger.Debug("cog: dropping stale agent completion",
			"agent", m.record.AgentName,
			"msg_parent", m.parentCycleID,
			"current", c.state.pendingCycleID,
		)
		return
	}
	c.state.pendingAgentCompletions = append(c.state.pendingAgentCompletions, m.record)
}

// RequestMoreTurns is the cog-side entry point for the
// `request_more_turns` tool. Synchronous — blocks on the cog
// inbox so the calling tool gets the new effective cap (or an
// error) before its Execute returns to the LLM.
//
// `additional` must be > 0; the request is clamped at
// hardMaxToolTurns. `reason` is required (audit trail) and
// short-truncated for logging.
//
// `parentCycleID` is the cycle from whose tool dispatch this
// request originates — stale calls (parent != current pending)
// are rejected so a delayed extension request from a previous
// cycle can't bump the budget of the next one.
func (c *Cog) RequestMoreTurns(parentCycleID string, additional int, reason string) (int, error) {
	if additional <= 0 {
		return 0, fmt.Errorf("cog: request_more_turns: additional must be > 0, got %d", additional)
	}
	if reason == "" {
		return 0, fmt.Errorf("cog: request_more_turns: reason is required")
	}
	reply := make(chan requestMoreTurnsResult, 1)
	c.inbox <- requestMoreTurnsMsg{
		additional:    additional,
		reason:        reason,
		parentCycleID: parentCycleID,
		reply:         reply,
	}
	r := <-reply
	return r.NewCap, r.Err
}

// onRequestMoreTurns mutates the in-flight cycle's effective
// tool-turn cap. Refuses when:
//   - no cycle is in flight (pendingCycleID empty),
//   - the request's parentCycleID doesn't match the current cycle
//     (stale request from a previous cycle),
//   - the current cap is already at hardMaxToolTurns.
//
// Otherwise raises the cap by `additional`, clamped to
// hardMaxToolTurns. Logs at info level so operators can see the
// extensions; cycle-log carries the same data via the
// EventToolTurnsExtended type for structured audit.
func (c *Cog) onRequestMoreTurns(m requestMoreTurnsMsg) {
	if c.state.pendingCycleID == "" {
		m.reply <- requestMoreTurnsResult{
			NewCap: 0,
			Err:    fmt.Errorf("cog: request_more_turns: no cycle in flight"),
		}
		return
	}
	if m.parentCycleID != "" && m.parentCycleID != c.state.pendingCycleID {
		m.reply <- requestMoreTurnsResult{
			NewCap: c.state.pendingMaxToolTurns,
			Err: fmt.Errorf("cog: request_more_turns: stale parent cycle %q (current %q)",
				m.parentCycleID, c.state.pendingCycleID),
		}
		return
	}
	current := c.state.pendingMaxToolTurns
	if current <= 0 {
		current = c.cfg.MaxToolTurns
	}
	if current >= hardMaxToolTurns {
		m.reply <- requestMoreTurnsResult{
			NewCap: current,
			Err: fmt.Errorf("cog: request_more_turns: already at hard cap %d",
				hardMaxToolTurns),
		}
		return
	}
	target := current + m.additional
	if target > hardMaxToolTurns {
		target = hardMaxToolTurns
	}
	c.state.pendingMaxToolTurns = target
	c.cfg.Logger.Info("cog: tool-turn cap extended",
		"cycle_id", c.state.pendingCycleID,
		"from", current,
		"to", target,
		"requested_additional", m.additional,
		"reason", reason200(m.reason),
	)
	c.emit(cyclelog.Event{
		Type:    cyclelog.EventToolTurnsExtended,
		CycleID: c.state.pendingCycleID,
		// MaxTokens reuse: cap-after value. The event's existing
		// MaxTokens slot is the closest typed field; we add a
		// dedicated field too so consumers can disambiguate.
		MaxTokens: target,
		// Reason carried via the existing Error field — overloaded
		// here for brevity rather than adding a new field.
		Error: reason200(m.reason),
	})
	m.reply <- requestMoreTurnsResult{NewCap: target, Err: nil}
}

// reason200 truncates a free-text reason to 200 chars for log
// output. Avoids drowning operator logs in long tool-call args.
func reason200(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// drainAmbient returns and clears the pending ambient buffer. Called by
// dispatchLLM at the moment the LLM call is built so the snapshot is
// fresh. Returns nil when empty so SystemPromptFn callers don't have
// to special-case zero-length slices.
func (c *Cog) drainAmbient() []ambient.Signal {
	if len(c.state.pendingAmbient) == 0 {
		return nil
	}
	out := c.state.pendingAmbient
	c.state.pendingAmbient = nil
	return out
}

func (c *Cog) onUserInput(ctx context.Context, m UserInput) {
	if c.state.status != StatusIdle {
		if len(c.state.queue) >= c.cfg.InputQueueCap {
			err := fmt.Errorf("cog: input queue full (cap=%d)", c.cfg.InputQueueCap)
			m.Reply <- Reply{
				Kind: ReplyKindError,
				Text: "I'm busy with another cycle and the queue is full. Try again in a moment.",
				Err:  err,
			}
			return
		}
		c.state.queue = append(c.state.queue, m)
		c.cfg.Logger.Debug("cog queued user input", "queue_len", len(c.state.queue))
		return
	}

	// Bound the running history when the operator has set
	// [cog].max_context_messages. SD-faithful: reference
	// deployment exposes the same knob; default unbounded.
	// Truncation drops oldest messages, walks forward to a
	// user-role start, and re-runs the orphan-pair sanitiser so
	// the resulting slice still satisfies the alternation +
	// tool-pair invariants.
	base := c.state.history
	if c.cfg.MaxContextMessages > 0 {
		base = base.Truncate(c.cfg.MaxContextMessages)
	}
	staged, err := base.Add(llm.UserText(m.Text))
	if err != nil {
		m.Reply <- Reply{
			Kind: ReplyKindError,
			Text: FormatErrorForUser(err),
			Err:  err,
		}
		return
	}
	cycleID := uuid.NewString()
	c.state.pendingHistory = staged
	c.state.pendingReply = m.Reply
	c.state.pendingCycleID = cycleID
	c.currentCycleID.Store(&cycleID)
	c.state.pendingUserText = m.Text
	c.state.pendingSource = m.Source
	c.state.pendingMaxToolTurns = c.cfg.MaxToolTurns

	if c.cfg.DAG != nil {
		c.cfg.DAG.StartCycle(dag.CycleID(cycleID), "", dag.NodeCognitive)
	}
	c.emit(cyclelog.Event{
		Type:     cyclelog.EventCycleStart,
		CycleID:  cycleID,
		NodeType: string(dag.NodeCognitive),
		// Operator-visible input text — surfaced so a per-day
		// transcript view can render the chat without
		// cross-referencing narrative + history.
		Text: m.Text,
	})

	// No policy configured → skip straight to the LLM dispatch.
	if c.cfg.Policy == nil {
		c.transition(StatusThinking)
		c.dispatchLLM(ctx, m.Reply, cycleID, staged)
		return
	}

	// Policy configured: spawn input-policy worker.
	c.transition(StatusEvaluatingPolicy)
	c.cfg.Logger.Info("cog evaluating input policy", "cycle_id", cycleID, "source", m.Source)

	engine := c.cfg.Policy
	text := m.Text
	source := m.Source
	replyCh := m.Reply

	actor.Spawn(ctx,
		func(ctx context.Context) (policy.Result, error) {
			return engine.EvaluateInput(ctx, text, source), nil
		},
		func(res policy.Result, err error) {
			if err != nil {
				// Panic recovered — fail closed for safety.
				res = policy.Result{
					Verdict: policy.Block,
					Trail:   fmt.Sprintf("input policy: panic recovered: %v", err),
				}
			}
			c.inbox <- inputPolicyComplete{reply: replyCh, cycleID: cycleID, text: text, result: res}
		},
	)
}

func (c *Cog) onInputPolicyComplete(ctx context.Context, m inputPolicyComplete) {
	if c.state.pendingReply != m.reply {
		c.cfg.Logger.Debug("cog dropped stale inputPolicyComplete", "cycle_id", m.cycleID)
		return
	}

	c.emit(cyclelog.Event{
		Type:         cyclelog.EventPolicyDecision,
		CycleID:      m.cycleID,
		Gate:         policy.GateInput.String(),
		Verdict:      m.result.Verdict.String(),
		PolicyScore:  m.result.Score,
		PolicyTrail:  m.result.Trail,
		Inconclusive: m.result.Inconclusive,
	})

	if m.result.Verdict == policy.Block || m.result.Verdict == policy.Escalate {
		c.abandonCycle(
			m.reply, m.cycleID,
			ReplyKindRefusal, c.cfg.InputRefusalText,
			fmt.Errorf("input policy %s: %s", m.result.Verdict, m.result.Trail),
			dag.StatusError, cyclelog.StatusBlocked,
		)
		return
	}

	// Allow: dispatch the LLM call.
	c.transition(StatusThinking)
	c.dispatchLLM(ctx, m.reply, m.cycleID, c.state.pendingHistory)
}

// dispatchLLM emits llm_request, spawns the worker, and returns. Caller is
// responsible for setting state to StatusThinking before calling.
func (c *Cog) dispatchLLM(ctx context.Context, replyCh chan<- Reply, cycleID string, staged llm.MessageHistory) {
	c.state.pendingTurns++
	// Re-emit the Thinking activity now that we know the turn count —
	// the bare transition emission earlier had turn=0. Subscribers
	// see the latest emission, so the turn-aware one wins.
	c.emitActivity(nil)
	c.cfg.Logger.Info("cog dispatching to llm", "cycle_id", cycleID, "history_len", staged.Len(), "turn", c.state.pendingTurns)
	c.emit(cyclelog.Event{
		Type:         cyclelog.EventLLMRequest,
		CycleID:      cycleID,
		Model:        c.cfg.Model,
		MessageCount: staged.Len(),
		MaxTokens:    c.cfg.MaxTokens,
	})

	provider := c.cfg.Provider
	model := c.cfg.Model
	var system llm.SystemPrompt
	if c.cfg.SystemPromptFn != nil {
		// Drain ambient signals only on the first dispatch of a cycle —
		// later turns within the same cycle (post-tool follow-up)
		// shouldn't see fresh ambient because the agent is mid-thought.
		// Subsequent turns get an empty Ambient slice; the curator
		// renders the same <ambient> block (or omits it) accordingly.
		var drainedAmbient []ambient.Signal
		if c.state.pendingTurns == 1 {
			drainedAmbient = c.drainAmbient()
		}
		system = c.cfg.SystemPromptFn(ctx, CycleSnapshot{
			CycleID:      cycleID,
			InputSource:  c.state.pendingSource,
			QueueDepth:   len(c.state.queue),
			MessageCount: staged.Len(),
			Ambient:      drainedAmbient,
			UserText:     c.state.pendingUserText,
		})
	}
	maxTokens := c.cfg.MaxTokens
	msgs := staged.Messages()
	var tools []llm.Tool
	if c.cfg.Tools != nil {
		tools = c.cfg.Tools.List()
	}

	type workResult struct {
		content    []llm.ContentBlock
		stopReason string
		usage      llm.Usage
	}

	actor.Spawn(ctx,
		func(ctx context.Context) (workResult, error) {
			resp, err := provider.Chat(ctx, llm.Request{
				Model:        model,
				SystemPrompt: system,
				Messages:     msgs,
				MaxTokens:    maxTokens,
				Tools:        tools,
			})
			if err != nil {
				return workResult{}, err
			}
			return workResult{content: resp.Content, stopReason: resp.StopReason, usage: resp.Usage}, nil
		},
		func(r workResult, err error) {
			c.inbox <- thinkComplete{
				reply:      replyCh,
				cycleID:    cycleID,
				content:    r.content,
				stopReason: r.stopReason,
				usage:      r.usage,
				err:        err,
			}
		},
	)
}

func (c *Cog) onThinkComplete(ctx context.Context, m thinkComplete) {
	if c.state.pendingReply != m.reply {
		c.cfg.Logger.Debug("cog dropped stale thinkComplete", "cycle_id", m.cycleID)
		return
	}

	c.state.pendingInTok += m.usage.InputTokens
	c.state.pendingOutTok += m.usage.OutputTokens

	c.emit(cyclelog.Event{
		Type:         cyclelog.EventLLMResponse,
		CycleID:      m.cycleID,
		StopReason:   m.stopReason,
		InputTokens:  m.usage.InputTokens,
		OutputTokens: m.usage.OutputTokens,
		Error:        errMsg(m.err),
	})

	if m.err != nil {
		c.abandonCycle(
			m.reply, m.cycleID,
			ReplyKindError, "",
			m.err,
			dag.StatusError, cyclelog.StatusError,
		)
		return
	}

	toolUses := collectToolUses(m.content)
	if len(toolUses) > 0 {
		c.continueWithTools(ctx, m, toolUses)
		return
	}

	// No tool calls — final text reply path.
	finalText := textFromContent(m.content)

	// Output policy (inline — no LLM call for deterministic-only output gate).
	if c.cfg.Policy != nil {
		out := c.cfg.Policy.EvaluateOutput(finalText)
		c.emit(cyclelog.Event{
			Type:        cyclelog.EventPolicyDecision,
			CycleID:     m.cycleID,
			Gate:        policy.GateOutput.String(),
			Verdict:     out.Verdict.String(),
			PolicyScore: out.Score,
			PolicyTrail: out.Trail,
		})
		if out.Verdict == policy.Block || out.Verdict == policy.Escalate {
			c.abandonCycle(
				m.reply, m.cycleID,
				ReplyKindRefusal, c.cfg.OutputRefusalText,
				fmt.Errorf("output policy %s: %s", out.Verdict, out.Trail),
				dag.StatusError, cyclelog.StatusBlocked,
			)
			return
		}

		// Fabrication-detection (Layer 3 of the fabrication-
		// prevention policy). Always logs the score; gates only
		// Output-fabrication gate: emits a policy_decision event
		// to cycle log so the verdict is auditable + visible in
		// the admin tab, but does NOT mutate the chat reply.
		// Earlier versions appended a "⚠ Verification:" footer to
		// every flagged output — operators reported this as
		// noisy and operator-burdening (the footer reads as "I
		// might have lied, please double-check"). SD's pattern
		// is to keep the chat surface clean and run a separate
		// daily fabrication-audit worker that catches patterns
		// off-cog (see internal/metalearning/fabrication_audit.go).
		// Tool-input fabrication scoring (PR #99) still BLOCKS
		// at dispatch for irreversible tools (send_email, etc.)
		// — that's the prevention layer; this is just the audit
		// telemetry stream.
		if c.cfg.Policy.HasFabricationScorer() {
			fab := c.cfg.Policy.EvaluateOutputFabrication(ctx, finalText, c.state.pendingToolEvents)
			c.emit(cyclelog.Event{
				Type:        cyclelog.EventPolicyDecision,
				CycleID:     m.cycleID,
				Gate:        "output_fabrication",
				Verdict:     fab.Verdict.String(),
				PolicyScore: fab.Score,
				PolicyTrail: fab.Trail,
			})
		}
	}

	final, err := c.state.pendingHistory.Add(llm.Message{Role: llm.RoleAssistant, Content: m.content})
	if err != nil {
		c.abandonCycle(
			m.reply, m.cycleID,
			ReplyKindError, "",
			err,
			dag.StatusError, cyclelog.StatusError,
		)
		return
	}
	c.state.history = final
	userText := c.state.pendingUserText
	wasAutonomous := c.state.pendingSource == policy.SourceAutonomous
	c.discardPending()

	c.cfg.Logger.Info("cog delivered reply",
		"cycle_id", m.cycleID,
		"history_len", c.state.history.Len(),
		"autonomous", wasAutonomous,
	)
	m.reply <- Reply{Kind: ReplyKindText, Text: finalText}
	if wasAutonomous {
		// Surface the autonomous reply to operator-facing
		// subscribers (TUI / webui SSE). They render it in the
		// chat log with a distinct muted style so the operator
		// can see scheduler / comms activity passing through.
		// The reply already went to m.reply for the submitter
		// (scheduler / poller adapter) to drain; the trace is
		// the operator-visible mirror.
		c.traceHub.publish(Trace{
			CycleID: m.cycleID,
			Source:  "autonomous",
			Body:    finalText,
		})
	}
	c.completeCycleWithText(m.cycleID, dag.StatusComplete, cyclelog.StatusComplete, nil, finalText)
	c.recordNarrative(m.cycleID, librarian.NarrativeStatusComplete, userText, finalText, nil)
	c.transition(StatusIdle)
}

// continueWithTools is the react-loop branch: stage the assistant message
// (with its tool_use blocks) into pendingHistory, spawn the tool dispatch
// worker, and transition to UsingTools so the watchdog re-arms for the
// tool-execution window.
func (c *Cog) continueWithTools(ctx context.Context, m thinkComplete, toolUses []llm.ToolUseBlock) {
	if c.cfg.Tools == nil {
		// Model emitted tool_use without a registry — protocol violation;
		// the request shouldn't have advertised tools. Surface as error.
		c.abandonCycle(
			m.reply, m.cycleID,
			ReplyKindError, "",
			fmt.Errorf("cog: response contained tool_use but no ToolDispatcher configured"),
			dag.StatusError, cyclelog.StatusError,
		)
		return
	}
	// Per-cycle effective cap. Initialised from cfg.MaxToolTurns at
	// cycle start; can be raised mid-cycle via the
	// request_more_turns tool up to hardMaxToolTurns. Read here
	// (not cfg) so an in-flight extension takes effect on the next
	// dispatch decision.
	effectiveCap := c.state.pendingMaxToolTurns
	if effectiveCap <= 0 {
		// Defensive: a path that didn't seed the cycle's cap falls
		// back to the configured default rather than abort
		// immediately.
		effectiveCap = c.cfg.MaxToolTurns
	}
	if c.state.pendingTurns >= effectiveCap {
		c.abandonCycle(
			m.reply, m.cycleID,
			ReplyKindError, "",
			fmt.Errorf("cog: tool loop exceeded max_tool_turns=%d", effectiveCap),
			dag.StatusError, cyclelog.StatusError,
		)
		return
	}

	staged, err := c.state.pendingHistory.Add(llm.Message{Role: llm.RoleAssistant, Content: m.content})
	if err != nil {
		c.abandonCycle(
			m.reply, m.cycleID,
			ReplyKindError, "",
			err,
			dag.StatusError, cyclelog.StatusError,
		)
		return
	}
	c.state.pendingHistory = staged

	c.cfg.Logger.Info("cog dispatching tools",
		"cycle_id", m.cycleID,
		"tool_count", len(toolUses),
		"turn", c.state.pendingTurns,
	)
	c.transition(StatusUsingTools)
	// transition() already emitted the bare status change; re-emit
	// once with the tool names so the TUI can show "using tool: X".
	c.emitActivity(toolNamesOf(toolUses))

	// Accumulate tool names per cycle so the archivist can record
	// which tools shaped this cycle's outcome on the derived CBR
	// case. Append + dedupe once at end-of-cycle to keep the per-turn
	// path cheap.
	for _, u := range toolUses {
		if u.Name != "" {
			c.state.pendingTools = append(c.state.pendingTools, u.Name)
		}
	}

	dispatcher := c.cfg.Tools
	policyEngine := c.cfg.Policy
	cycleSink := c.cfg.CycleLog
	cycleID := m.cycleID
	replyCh := m.reply
	uses := toolUses

	actor.Spawn(ctx,
		func(ctx context.Context) ([]llm.ContentBlock, error) {
			// Inject the in-flight cog cycle ID so tool handlers
			// (memory_write etc.) can stamp facts with provenance.
			// Tools that don't need it ignore the context value.
			ctx = cyclelog.WithCycleID(ctx, cycleID)
			return dispatchToolsParallel(ctx, dispatcher, policyEngine, cycleSink, cycleID, c.cfg.InstanceID, uses), nil
		},
		func(results []llm.ContentBlock, werr error) {
			msg := toolsComplete{
				reply:   replyCh,
				cycleID: cycleID,
				results: results,
				records: buildToolCallRecords(uses, results),
				err:     werr,
			}
			// Build the fabrication-scorer evidence only when
			// the engine actually wants it — copies tool output
			// strings, which are unbounded in size in principle.
			if policyEngine != nil && policyEngine.HasFabricationScorer() {
				msg.events = buildToolEvents(uses, results)
			}
			c.inbox <- msg
		},
	)
}

// dispatchToolsParallel runs every tool_use through the
// gate→dispatch→post-gate pipeline concurrently. Each tool call lives
// in its own goroutine; results land in a fixed-size slice indexed by
// the tool's position in `uses`, so the final order matches input
// order — important because the model sees `tool_result` blocks in a
// stable sequence and humans reading the cycle log can pair each
// dispatch with its source `tool_use`.
//
// SD parity (`agent/cognitive.gleam` runs tool dispatches as
// independent OTP processes and waits for all before re-thinking).
// The Anthropic / Mistral APIs both accept a tool_result block per
// tool_use_id regardless of order, but the cog still emits in
// declaration order to keep telemetry deterministic.
//
// All collaborators are concurrency-safe by contract:
//   - dispatcher.Dispatch — tools.Registry handles their own
//     synchronisation; agent delegates serialise per-agent.
//   - policyEngine.EvaluateTool / EvaluatePostExec — pure functions
//     over the in-memory rule set.
//   - cycleSink.Emit — cyclelog.Writer is documented mutex-protected.
//
// Single-tool case skips goroutine spawn overhead — common path
// stays trivial.
func dispatchToolsParallel(
	ctx context.Context,
	dispatcher ToolDispatcher,
	policyEngine *policy.Engine,
	cycleSink cyclelog.Sink,
	cycleID, instanceID string,
	uses []llm.ToolUseBlock,
) []llm.ContentBlock {
	if len(uses) == 0 {
		return nil
	}
	if len(uses) == 1 {
		return []llm.ContentBlock{
			runOneTool(ctx, dispatcher, policyEngine, cycleSink, cycleID, instanceID, uses[0]),
		}
	}

	results := make([]llm.ContentBlock, len(uses))
	var wg sync.WaitGroup
	wg.Add(len(uses))
	for i := range uses {
		go func(idx int, u llm.ToolUseBlock) {
			defer wg.Done()
			results[idx] = runOneTool(ctx, dispatcher, policyEngine, cycleSink, cycleID, instanceID, u)
		}(i, uses[i])
	}
	wg.Wait()
	return results
}

// runOneTool executes the gate→dispatch→post-gate chain for a single
// tool_use. Returns the tool_result block to surface to the model.
//
// Behaviour mirrors the previous serial loop:
//   - Tool gate Block / Escalate → IsError tool_result (Dispatch skipped)
//   - Dispatch error → IsError tool_result with the error string
//   - PostExec gate Block / Escalate → redacted-content tool_result
//     (NOT IsError; the call succeeded, the output is policy)
//   - Otherwise → tool_result with the dispatcher's output
func runOneTool(
	ctx context.Context,
	dispatcher ToolDispatcher,
	policyEngine *policy.Engine,
	cycleSink cyclelog.Sink,
	cycleID, instanceID string,
	u llm.ToolUseBlock,
) llm.ContentBlock {
	if policyEngine != nil {
		tg := policyEngine.EvaluateTool(u.Name, string(u.Input))
		emitPolicyDecision(cycleSink, cycleID, instanceID, policy.GateTool, tg)
		if tg.Verdict == policy.Block || tg.Verdict == policy.Escalate {
			return llm.ToolResultBlock{
				ToolUseID: u.ID,
				Content:   toolGateRefusal,
				IsError:   true,
			}
		}
	}
	out, derr := dispatcher.Dispatch(ctx, u.Name, u.Input)
	if derr != nil {
		return llm.ToolResultBlock{
			ToolUseID: u.ID,
			Content:   derr.Error(),
			IsError:   true,
		}
	}
	if policyEngine != nil {
		pe := policyEngine.EvaluatePostExec(u.Name, out)
		emitPolicyDecision(cycleSink, cycleID, instanceID, policy.GatePostExec, pe)
		if pe.Verdict == policy.Block || pe.Verdict == policy.Escalate {
			return llm.ToolResultBlock{
				ToolUseID: u.ID,
				Content:   postExecRefusal,
			}
		}
	}
	return llm.ToolResultBlock{
		ToolUseID: u.ID,
		Content:   out,
	}
}

// emitPolicyDecision is a small helper for the tool/postexec gates so
// the dispatch worker can record decisions without duplicating the
// cyclelog event shape. Mirrors the input/output gate emit blocks in
// onInputPolicyComplete / onThinkComplete. The instanceID parameter
// stamps `instance_id` on the event so policy decisions in the
// dispatch goroutine carry the same correlation tag as everything
// else from this cog instance.
func emitPolicyDecision(sink cyclelog.Sink, cycleID, instanceID string, gate policy.GateKind, r policy.Result) {
	if sink == nil {
		return
	}
	_ = sink.Emit(cyclelog.Event{
		Type:         cyclelog.EventPolicyDecision,
		CycleID:      cycleID,
		InstanceID:   instanceID,
		Gate:         gate.String(),
		Verdict:      r.Verdict.String(),
		PolicyScore:  r.Score,
		PolicyTrail:  r.Trail,
		Inconclusive: r.Inconclusive,
	})
}

func (c *Cog) onToolsComplete(ctx context.Context, m toolsComplete) {
	if c.state.pendingReply != m.reply {
		c.cfg.Logger.Debug("cog dropped stale toolsComplete", "cycle_id", m.cycleID)
		return
	}
	if m.err != nil {
		c.abandonCycle(
			m.reply, m.cycleID,
			ReplyKindError, "",
			m.err,
			dag.StatusError, cyclelog.StatusError,
		)
		return
	}

	if len(m.records) > 0 {
		c.state.pendingToolCalls = append(c.state.pendingToolCalls, m.records...)
	}
	if len(m.events) > 0 {
		c.state.pendingToolEvents = append(c.state.pendingToolEvents, m.events...)
	}

	staged, err := c.state.pendingHistory.Add(llm.Message{Role: llm.RoleUser, Content: m.results})
	if err != nil {
		c.abandonCycle(
			m.reply, m.cycleID,
			ReplyKindError, "",
			err,
			dag.StatusError, cyclelog.StatusError,
		)
		return
	}
	c.state.pendingHistory = staged

	c.transition(StatusThinking)
	c.dispatchLLM(ctx, m.reply, m.cycleID, c.state.pendingHistory)
}

func collectToolUses(blocks []llm.ContentBlock) []llm.ToolUseBlock {
	var out []llm.ToolUseBlock
	for _, b := range blocks {
		if tu, ok := b.(llm.ToolUseBlock); ok {
			out = append(out, tu)
		}
	}
	return out
}

// copyAgentCompletions returns a defensive copy of the records
// to ship to the archivist. The cog's slice gets reset on cycle
// boundary; if the archivist is still iterating we don't want
// the underlying array reused. Cheap allocation — typically 0-2
// entries per cycle.
func copyAgentCompletions(in []agent.CompletionRecord) []agent.CompletionRecord {
	if len(in) == 0 {
		return nil
	}
	out := make([]agent.CompletionRecord, len(in))
	copy(out, in)
	return out
}

// toolCallsForArchivist converts the cog's internal tool-call
// records to the archivist's matching shape. Same data, different
// package — the archivist depends on cog (not the other way
// around) so we re-pack here at the boundary rather than expose
// the cog type from archivist.
func toolCallsForArchivist(in []ToolCallRecord) []archivist.ToolCallRecord {
	if len(in) == 0 {
		return nil
	}
	out := make([]archivist.ToolCallRecord, len(in))
	for i, r := range in {
		out[i] = archivist.ToolCallRecord{Name: r.Name, Success: r.Success}
	}
	return out
}

// buildToolEvents pairs each ToolUseBlock with the textual
// output from its ToolResultBlock. Used to feed the fabrication
// scorer, which checks whether the assistant's prose claims
// are grounded in what tools actually returned. Only built
// when the policy engine has a fabrication scorer (allocation
// + string-copy cost otherwise paid for nothing).
//
// IsError tool results still get included — the scorer can
// reason about "the tool failed" too. Tool result content
// blocks come back stringified by the dispatcher; we use them
// as-is rather than re-serialising.
func buildToolEvents(uses []llm.ToolUseBlock, results []llm.ContentBlock) []policy.ToolEvent {
	if len(uses) == 0 {
		return nil
	}
	byID := make(map[string]string, len(results))
	for _, r := range results {
		if rb, ok := r.(llm.ToolResultBlock); ok {
			byID[rb.ToolUseID] = rb.Content
		}
	}
	out := make([]policy.ToolEvent, 0, len(uses))
	for _, u := range uses {
		out = append(out, policy.ToolEvent{
			Name:   u.Name,
			Output: byID[u.ID], // empty when no matching result
		})
	}
	return out
}

// buildToolCallRecords pairs each ToolUseBlock with the
// success/failure flag from its corresponding ToolResultBlock.
// Match by ToolUseID. Order: dispatch order from `uses`. Used to
// build the ground-truth tool log the archivist's curator
// grounds outcome assessments in.
func buildToolCallRecords(uses []llm.ToolUseBlock, results []llm.ContentBlock) []ToolCallRecord {
	if len(uses) == 0 {
		return nil
	}
	// Index results by ToolUseID for the lookup.
	byID := make(map[string]bool, len(results))
	for _, r := range results {
		if rb, ok := r.(llm.ToolResultBlock); ok {
			byID[rb.ToolUseID] = !rb.IsError
		}
	}
	out := make([]ToolCallRecord, 0, len(uses))
	for _, u := range uses {
		success, ok := byID[u.ID]
		if !ok {
			// Result missing for this use — treat as failure
			// (better honesty than assuming success).
			success = false
		}
		out = append(out, ToolCallRecord{Name: u.Name, Success: success})
	}
	return out
}

// toolNamesOf extracts the tool names from a slice of ToolUseBlocks in
// declaration order, for surfacing on Activity events.
func toolNamesOf(uses []llm.ToolUseBlock) []string {
	names := make([]string, len(uses))
	for i, u := range uses {
		names[i] = u.Name
	}
	return names
}

func textFromContent(blocks []llm.ContentBlock) string {
	var out string
	for _, b := range blocks {
		if t, ok := b.(llm.TextBlock); ok {
			out += t.Text
		}
	}
	return out
}


func (c *Cog) discardPending() {
	c.state.pendingHistory = llm.MessageHistory{}
	c.state.pendingReply = nil
	c.state.pendingCycleID = ""
	empty := ""
	c.currentCycleID.Store(&empty)
	c.state.pendingUserText = ""
	c.state.pendingSource = policy.SourceInteractive
	c.state.pendingTurns = 0
	c.state.pendingMaxToolTurns = c.cfg.MaxToolTurns
	c.state.pendingTools = nil
	c.state.pendingToolCalls = nil
	c.state.pendingToolEvents = nil
	c.state.pendingAgentCompletions = nil
	c.state.pendingInTok = 0
	c.state.pendingOutTok = 0
}

func (c *Cog) onWatchdog() {
	if c.state.status == StatusIdle {
		return
	}
	cycleID := c.state.pendingCycleID
	c.emit(cyclelog.Event{
		Type:    cyclelog.EventWatchdogFire,
		CycleID: cycleID,
	})
	c.abandonCycle(
		c.state.pendingReply, cycleID,
		ReplyKindError, "",
		fmt.Errorf("cog: watchdog timeout in %s", c.state.status),
		dag.StatusAbandoned, cyclelog.StatusAbandon,
	)
}

// abandonCycle is the centralised failure-path exit. Every error / refusal /
// watchdog termination flows through here: log, emit cycle_complete, mark
// DAG, send Reply, clean up state, return to Idle.
//
// kind selects user-facing presentation:
//   - ReplyKindRefusal: send userVisible text as agent speech (graceful decline)
//   - ReplyKindError:   send internalErr as Err (red error in TUI)
// internalErr is logged and recorded on the DAG / cycle log regardless of
// the user-facing presentation.
func (c *Cog) abandonCycle(
	replyTo chan<- Reply,
	cycleID string,
	kind ReplyKind,
	userVisible string,
	internalErr error,
	dagStatus dag.Status,
	logStatus cyclelog.CycleStatus,
) {
	c.cfg.Logger.Warn("cog cycle abandoned",
		"cycle_id", cycleID,
		"kind", kind.String(),
		"err", internalErr,
	)
	c.completeCycle(cycleID, dagStatus, logStatus, internalErr)
	c.recordNarrative(cycleID, narrativeStatusFromCycleLog(logStatus), c.state.pendingUserText, "", internalErr)
	if replyTo != nil {
		switch kind {
		case ReplyKindRefusal:
			replyTo <- Reply{Kind: kind, Text: userVisible}
		case ReplyKindError:
			// Surface a sanitised user-facing summary alongside the
			// raw err. UI consumers (TUI / webui) prefer Text; raw
			// Err is preserved on the wire for slog and tests but
			// must not be rendered as chat — provider-internal text
			// like "mistral: status 429: {...}" is operator-hostile.
			text := userVisible
			if text == "" {
				text = FormatErrorForUser(internalErr)
			}
			replyTo <- Reply{Kind: kind, Text: text, Err: internalErr}
		}
	}
	c.discardPending()
	c.transition(StatusIdle)
}

// recordNarrative writes one cycle's outcome through the archivist
// when configured (preferred path: archivist owns post-cycle work and
// derives CBR cases), falling back to direct
// `Librarian.RecordNarrative` for test configs that don't construct
// an archivist. Fire-and-forget either way.
//
// Skipped entirely when cycleID is empty (no cycle to attribute to)
// or when neither archivist nor librarian is configured.
//
// The CycleComplete carries enough content for the archivist to
// derive a CBR case (UserInput + ReplyText + ToolsUsed) and update
// usage stats on cases the cycle retrieved (RetrievedCaseIDs drained
// from the librarian's per-cycle registry). Drain runs on success
// AND abandon paths so the registry self-cleans either way.
func (c *Cog) recordNarrative(cycleID string, status librarian.NarrativeStatus, userText, replyText string, err error) {
	if cycleID == "" {
		return
	}
	summary := buildNarrativeSummary(status, userText, replyText, err)
	now := time.Now()
	if c.cfg.Archivist != nil {
		c.cfg.Archivist.Record(archivist.CycleComplete{
			CycleID:          cycleID,
			Status:           status,
			Summary:          summary,
			UserInput:        userText,
			ReplyText:        replyText,
			ToolsUsed:        dedupeStrings(c.state.pendingTools),
			ToolCalls:        toolCallsForArchivist(c.state.pendingToolCalls),
			AgentCompletions: copyAgentCompletions(c.state.pendingAgentCompletions),
			RetrievedCaseIDs: c.drainRetrievedCaseIDs(cycleID),
			Timestamp:        now,
		})
		return
	}
	if c.cfg.Librarian != nil {
		// Drain anyway so the librarian's registry doesn't leak —
		// the test path that uses Librarian directly without an
		// archivist still fires retrievals.
		_ = c.drainRetrievedCaseIDs(cycleID)
		c.cfg.Librarian.RecordNarrative(librarian.NarrativeEntry{
			CycleID:   cycleID,
			Timestamp: now,
			Status:    status,
			Summary:   summary,
		})
	}
}

// drainRetrievedCaseIDs delegates to the librarian. Returns nil when
// no librarian is configured (test paths) so callers can safely use
// the result as a CycleComplete field.
func (c *Cog) drainRetrievedCaseIDs(cycleID string) []string {
	if c.cfg.Librarian == nil {
		return nil
	}
	return c.cfg.Librarian.DrainRetrievedCaseIDs(cycleID)
}

// dedupeStrings returns a new slice with duplicates and empty entries
// removed, preserving first-occurrence order. Used to clean up the
// per-cycle tool-name accumulation before handing it to the archivist.
func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func buildNarrativeSummary(status librarian.NarrativeStatus, userText, replyText string, err error) string {
	user := truncate(userText, 200)
	switch status {
	case librarian.NarrativeStatusComplete:
		return fmt.Sprintf("User: %s\nReply: %s", user, truncate(replyText, 200))
	case librarian.NarrativeStatusBlocked:
		return fmt.Sprintf("User: %s\nBlocked: %s", user, errString(err))
	case librarian.NarrativeStatusAbandoned:
		return fmt.Sprintf("User: %s\nAbandoned: %s", user, errString(err))
	case librarian.NarrativeStatusError:
		return fmt.Sprintf("User: %s\nError: %s", user, errString(err))
	}
	return fmt.Sprintf("User: %s", user)
}

func narrativeStatusFromCycleLog(s cyclelog.CycleStatus) librarian.NarrativeStatus {
	switch s {
	case cyclelog.StatusComplete:
		return librarian.NarrativeStatusComplete
	case cyclelog.StatusBlocked:
		return librarian.NarrativeStatusBlocked
	case cyclelog.StatusAbandon:
		return librarian.NarrativeStatusAbandoned
	}
	return librarian.NarrativeStatusError
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (c *Cog) completeCycle(id string, dagStatus dag.Status, logStatus cyclelog.CycleStatus, err error) {
	c.completeCycleWithText(id, dagStatus, logStatus, err, "")
}

// completeCycleWithText is the success-path variant that
// includes the assistant's reply text in the cycle_complete
// event. Surfaced so a per-day transcript view can reconstruct
// the chat from the cycle log alone (no need to cross-
// reference narrative + history). Abandon paths use the
// text-less completeCycle since there's no operator-visible
// text to surface.
func (c *Cog) completeCycleWithText(id string, dagStatus dag.Status, logStatus cyclelog.CycleStatus, err error, text string) {
	if id == "" {
		return
	}
	msg := errMsg(err)
	if c.cfg.DAG != nil {
		c.cfg.DAG.CompleteCycle(dag.CycleID(id), dagStatus, msg)
	}
	c.emit(cyclelog.Event{
		Type:    cyclelog.EventCycleComplete,
		CycleID: id,
		Status:  logStatus,
		Error:   msg,
		Text:    text,
	})
}

func (c *Cog) emit(ev cyclelog.Event) {
	if ev.InstanceID == "" {
		ev.InstanceID = c.cfg.InstanceID
	}
	if err := c.cfg.CycleLog.Emit(ev); err != nil {
		c.cfg.Logger.Warn("cyclelog emit failed", "type", string(ev.Type), "err", err)
	}
}

func errMsg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// transition updates status and (re-)arms the watchdog on every non-Idle
// transition. Per arch notes: each non-Idle status gets a fresh timeout
// window.
func (c *Cog) transition(next Status) {
	prev := c.state.status
	c.state.status = next
	if prev == next {
		return
	}
	if next == StatusIdle {
		c.disarmWatchdog()
		c.emitActivity(nil)
		return
	}
	c.armWatchdog()
	c.emitActivity(nil)
}

func (c *Cog) armWatchdog() {
	c.state.watchdogGen = c.watchdog.Arm(c.cfg.GateTimeout, func() {
		select {
		case c.inbox <- watchdogFire{}:
		default:
		}
	})
}

func (c *Cog) disarmWatchdog() {
	if c.state.watchdogGen != 0 {
		c.watchdog.Disarm(c.state.watchdogGen)
		c.state.watchdogGen = 0
	}
}

func (c *Cog) drainQueue(ctx context.Context) {
	if c.state.status != StatusIdle || len(c.state.queue) == 0 {
		return
	}
	next := c.state.queue[0]
	c.state.queue = c.state.queue[1:]
	c.onUserInput(ctx, next)
}
