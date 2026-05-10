// Package agent is the substrate for specialist agents — researcher,
// observer, comms, etc. — as named, supervised actors. Each agent has
// its own goroutine, tagged inbox, react loop, and tool registry.
//
// Selective port from `_impl_docs/ref/springdrift/src/agent/`:
//
//	Load-bearing here:
//	  - Spec (name, model, max_turns, max_consecutive_errors, tools)
//	  - Task / Outcome public API
//	  - Agent actor with serial task processing
//	  - Bounded react loop with tool dispatch
//	  - Activity push for ambient telemetry
//
//	Deferred (per `_impl_docs/.../agent/framework.gleam` complexity):
//	  - Worker-per-task concurrency (V1 processes one task at a time;
//	    the cog can dispatch to multiple agents in parallel, just not
//	    multiple tasks to the same agent)
//	  - Truncation-guard nudges + admission ships
//	  - Inter-turn delays / redact-secrets / depth caps
//	  - Structured findings extraction (ResearcherFindings etc.) —
//	    V1 returns the assistant's final text; structured shapes can
//	    be extracted later by inspecting the agent's tool calls
//	  - Agent teams / dispatch strategies (ParallelMerge, Pipeline,
//	    DebateAndConsensus, LeadWithSpecialists)
//
// Per the project's "agents are named actors" rule, every specialist
// agent ports as a struct + Spec + Run loop, never as a method tucked
// into the cog. This package owns the substrate; specs live alongside
// their tools (e.g. internal/agents/researcher).
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/seamus-brady/retainer/internal/cyclelog"
	"github.com/seamus-brady/retainer/internal/dag"
	"github.com/seamus-brady/retainer/internal/llm"
)

const (
	defaultMaxTurns             = 5
	defaultMaxConsecutiveErrors = 3
	defaultMaxTokens            = 2048
	inboxBufferSize             = 32
	activitiesBufferSize        = 16
)

// AgentFabricationGate is the interface the agent calls to fabrication-
// score a tool input before dispatch. Mirrors the policy engine's
// EvaluateToolFabrication shape but stays narrow so the agent package
// doesn't import policy directly (would create a cycle through the
// shared cyclelog types). Returns the verdict, a confidence score,
// and a trail string for cycle-log telemetry. shouldGate reports
// whether this tool name is in the high-risk set — agents skip
// scoring entirely for low-risk tools (no LLM round-trip cost).
type AgentFabricationGate interface {
	ShouldGate(toolName string) bool
	ScoreToolInput(ctx context.Context, toolName, input string, toolLog []FabricationToolEvent) FabricationGateResult
}

// FabricationToolEvent is one (toolName, output) pair the agent
// accumulates as tools complete. Passed back to the gate at the next
// high-risk dispatch as the "ground truth" the new input is checked
// against.
type FabricationToolEvent struct {
	Name   string
	Output string
}

// FabricationGateResult mirrors the relevant subset of policy.Result.
// Block / Escalate verdicts cause the agent to refuse the dispatch
// with an IsError result the LLM can react to; Allow lets the
// dispatch proceed.
type FabricationGateResult struct {
	Verdict       string // "allow" | "escalate" | "block"
	Score         float64
	Trail         string
	FlaggedClaims []string
}

// ToolDispatcher is the seam between the agent's react loop and a tool
// registry — same shape as cog.ToolDispatcher (Go's structural typing
// means *tools.Registry satisfies both with no import gymnastics).
// Defined here so the agent package doesn't import the cog.
type ToolDispatcher interface {
	List() []llm.Tool
	Dispatch(ctx context.Context, name string, input []byte) (string, error)
}

// Status is the agent's FSM state, mirrored to subscribers via the
// Activities channel.
type Status int

const (
	StatusIdle Status = iota
	StatusThinking
	StatusUsingTools
)

func (s Status) String() string {
	switch s {
	case StatusIdle:
		return "idle"
	case StatusThinking:
		return "thinking"
	case StatusUsingTools:
		return "using_tools"
	}
	return "unknown"
}

// Spec describes how to run one agent. Pure data — agents are
// constructed from a Spec at supervisor wiring time.
type Spec struct {
	// Name is the machine-readable identifier (e.g. "researcher"). Used
	// as the agent_<name> tool name when the cog delegates.
	Name string
	// HumanName is the display name (e.g. "Researcher").
	HumanName string
	// Description is the one-liner the LLM sees on the delegate tool —
	// helps the cog decide when to call this agent.
	Description string
	// SystemPrompt frames the agent's role for every dispatch. Static
	// for now; a future curator hook can enrich it per-task.
	SystemPrompt string
	// Provider is the LLM the agent talks to. Distinct from the cog's
	// provider so different agents can use different models.
	Provider llm.Provider
	// Model overrides the provider's default for this agent. Empty
	// keeps the provider default.
	Model string
	// MaxTokens caps the per-call response budget. Zero defaults to
	// defaultMaxTokens.
	MaxTokens int
	// MaxTurns bounds the react loop within a single task. Zero
	// defaults to defaultMaxTurns.
	MaxTurns int
	// MaxConsecutiveErrors short-circuits the react loop if the LLM
	// or tool dispatcher errors this many times in a row. Zero
	// defaults to defaultMaxConsecutiveErrors.
	MaxConsecutiveErrors int
	// Tools is the dispatcher the agent's LLM can call. Required —
	// an agent with no tools is just a one-shot LLM call, which the
	// cog can do directly.
	Tools ToolDispatcher

	// FabricationGate, when non-nil, runs a per-tool fabrication
	// check before dispatching certain "externalising" tools
	// (send_email, save_to_library, export_pdf, create_draft, etc.)
	// against the cycle's accumulated tool log. Catches the case
	// where a model composes a tool input (email body, document
	// content) containing claims not grounded in any prior tool's
	// output — precisely the Mistral failure mode that motivates
	// this hook.
	//
	// Optional. nil disables agent-side fabrication scoring; the
	// agent runs as before. The set of tools considered
	// "high-risk" lives in the gate's own config — the agent
	// just calls .ScoreToolInput; the policy engine decides if
	// the named tool is in scope.
	FabricationGate AgentFabricationGate

	// CycleLog receives per-task telemetry events. Optional — when
	// nil the agent runs silently (legacy behaviour). When set,
	// emits agent_cycle_start / llm_request / llm_response /
	// tool_call / tool_result / agent_cycle_complete with parent_id
	// chained to the cog's cycle that triggered the dispatch.
	CycleLog cyclelog.Sink

	// DAG records cycle parent/child relationships. Optional — when
	// nil, no NodeAgent entry is created for the agent's task. When
	// set, every dispatch creates a NodeAgent parented to the cog's
	// NodeCognitive (Task.ParentCycleID).
	DAG DAGRecorder

	// InstanceID is the workspace's stable identity prefix (8 chars
	// of the agent UUID). Stamped on every cycle-log event the
	// agent emits. Empty disables stamping (telemetry degrades to
	// pre-identity behaviour).
	InstanceID string

	// TokenSink, when non-nil, receives the agent's per-dispatch
	// token usage so it can be aggregated + persisted (typically
	// via internal/agenttokens). Called once per completed task,
	// after the Outcome is final, with InputTokens + OutputTokens
	// summed across the react loop. Failures log but don't affect
	// the dispatch outcome.
	TokenSink TokenSink
}

// TokenSink absorbs per-dispatch token usage. The production
// implementation is *agenttokens.Tracker (via Record), but tests
// can pass any value satisfying the interface. Defining it here
// keeps the agent package independent of the concrete tracker.
type TokenSink interface {
	Record(agentName string, inputTokens, outputTokens int) error
}

// DAGRecorder is the slice of *dag.DAG the agent uses. Defining the
// interface here keeps the agent package independent of the dag
// concrete type — *dag.DAG satisfies it via Go's structural
// typing.
type DAGRecorder interface {
	StartCycle(id, parentID dag.CycleID, nodeType dag.NodeType)
	CompleteCycle(id dag.CycleID, status dag.Status, errMsg string)
}

// Telemetry bundles the optional cycle-log + DAG + instance-id
// fields specialist agents share. Specialist constructors
// (researcher.New, observer.New) accept one of these so callers
// thread telemetry once instead of three separate args. Zero value
// is valid: telemetry-less agent runs silently (the legacy
// behaviour).
type Telemetry struct {
	CycleLog   cyclelog.Sink
	DAG        DAGRecorder
	InstanceID string
	TokenSink  TokenSink
	// FabricationGate is the per-tool fabrication scorer the agent
	// consults before dispatching "externalising" tools. Optional —
	// nil disables agent-side scoring (the gate runs only on
	// agents whose telemetry carries it). Bootstrap installs the
	// adapter when the policy engine has a fabrication scorer
	// configured AND the agent dispatches at least one high-risk
	// tool (comms, writer). Researcher / observer don't get the
	// gate — they don't externalise content.
	FabricationGate AgentFabricationGate
}

// ApplyTo copies the telemetry fields onto a Spec. Specialist
// constructors call this after assembling their static spec so
// telemetry plugs in without restating field names.
func (t Telemetry) ApplyTo(s *Spec) {
	s.CycleLog = t.CycleLog
	s.DAG = t.DAG
	s.InstanceID = t.InstanceID
	s.TokenSink = t.TokenSink
	s.FabricationGate = t.FabricationGate
}

// Task is one unit of work dispatched to an agent.
type Task struct {
	// TaskID uniquely identifies this dispatch. The agent uses it as
	// its cycle_id so cycle-log + DAG events line up.
	TaskID string
	// Instruction is the user-facing description of the work
	// (typically generated by the cog's LLM as the delegate tool's
	// arg). Becomes the user message in the agent's react loop.
	Instruction string
	// ParentCycleID is the cog cycle that triggered this dispatch.
	// Surfaces on the agent's cycle-log events as parent_id, mirroring
	// the curator_assembled pattern.
	ParentCycleID string
	// Reply receives the Outcome when the agent finishes. Buffered (1).
	Reply chan<- Outcome
}

// Outcome is the agent's reply for one Task. Always data — failures
// are encoded as Err, never panics or process exits.
type Outcome struct {
	TaskID       string
	AgentName    string
	AgentCycleID string

	// Result is the agent's final assistant text on success, empty
	// on failure.
	Result string
	// Err is non-nil when the agent failed (max turns, max errors,
	// LLM down, etc.). Result is empty when Err != nil.
	Err error

	// ToolsUsed is the list of tool names invoked during the react
	// loop, in dispatch order (duplicates allowed).
	ToolsUsed []string
	// InputTokens / OutputTokens summed across every LLM call in the
	// react loop.
	InputTokens  int
	OutputTokens int
	// Duration is wall-clock time spent in the react loop.
	Duration time.Duration
}

// IsSuccess reports whether the agent returned a final text reply.
func (o Outcome) IsSuccess() bool { return o.Err == nil }

// Activity is the agent's ambient progress signal — same shape as
// cog.Activity but agent-scoped. Lossy push channel.
type Activity struct {
	AgentName string
	Status    Status
	TaskID    string
	Turn      int
	MaxTurns  int
	ToolNames []string

	// InputTokens / OutputTokens are running totals across the
	// in-flight task's react loop, updated on every LLM response.
	// Subscribers (TUI, webui SSE) can render the current cost
	// without joining against the cycle log. Zero on transitions
	// that don't follow an LLM call (e.g. status=idle on entry).
	InputTokens  int
	OutputTokens int
}

// Agent is the running actor wrapping one Spec.
type Agent struct {
	spec       Spec
	inbox      chan Task
	activities chan Activity
	logger     *slog.Logger
	state      state
}

type state struct {
	status         Status
	currentTaskID  string
	currentTurn    int
	currentInTok   int
	currentOutTok  int
}

// New constructs an Agent from a Spec, applying defaults. Returns an
// error if Spec.Tools is nil (an agent without tools has nothing to
// react about).
func New(spec Spec, logger *slog.Logger) (*Agent, error) {
	if spec.Name == "" {
		return nil, fmt.Errorf("agent: Spec.Name is required")
	}
	if spec.Provider == nil {
		return nil, fmt.Errorf("agent: Spec.Provider is required")
	}
	if spec.Tools == nil {
		return nil, fmt.Errorf("agent: Spec.Tools is required (use the cog directly for tool-less LLM calls)")
	}
	if spec.MaxTokens == 0 {
		spec.MaxTokens = defaultMaxTokens
	}
	if spec.MaxTurns == 0 {
		spec.MaxTurns = defaultMaxTurns
	}
	if spec.MaxConsecutiveErrors == 0 {
		spec.MaxConsecutiveErrors = defaultMaxConsecutiveErrors
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Agent{
		spec:       spec,
		inbox:      make(chan Task, inboxBufferSize),
		activities: make(chan Activity, activitiesBufferSize),
		logger:     logger.With("agent", spec.Name),
	}, nil
}

// Name returns the agent's machine-readable identifier.
func (a *Agent) Name() string { return a.spec.Name }

// HumanName returns the agent's display name.
func (a *Agent) HumanName() string { return a.spec.HumanName }

// Description returns the agent's one-line description (for delegate
// tool surfacing).
func (a *Agent) Description() string { return a.spec.Description }

// Activities returns the receive-only channel for ambient activity
// updates. Lossy: emissions drop when no subscriber reads. Same
// pattern as cog.Activities.
func (a *Agent) Activities() <-chan Activity { return a.activities }

// Submit enqueues a task. Non-blocking when there's buffer; falls
// back to ctx-aware send when full. The task's Reply channel must be
// pre-allocated (buffered 1) by the caller so completion never blocks.
func (a *Agent) Submit(ctx context.Context, t Task) error {
	if t.Reply == nil {
		return fmt.Errorf("agent: Task.Reply channel is required")
	}
	if t.TaskID == "" {
		t.TaskID = uuid.NewString()
	}
	select {
	case a.inbox <- t:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run is the agent's actor loop. Block until ctx is cancelled. Wrap
// with actor.Run under actor.Permanent (or whichever Spec.restart
// strategy you wire at supervisor level) so panics restart cleanly.
func (a *Agent) Run(ctx context.Context) error {
	a.logger.Info("agent started", "human_name", a.spec.HumanName, "tools", toolListSnapshot(a.spec.Tools))
	defer a.logger.Info("agent stopped")

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case task := <-a.inbox:
			a.handleTask(ctx, task)
		}
	}
}

// handleTask runs the react loop for one task, sends the Outcome on
// the task's Reply channel, and resets state for the next task. V1
// processes tasks serially — multiple concurrent tasks queue.
//
// Telemetry: when CycleLog/DAG/InstanceID are configured on the
// Spec, brackets the react loop with agent_cycle_start +
// agent_cycle_complete events and a NodeAgent DAG node parented to
// task.ParentCycleID. Telemetry failures don't fail the dispatch —
// a cycle-log error logs but the Outcome flows back regardless.
func (a *Agent) handleTask(ctx context.Context, task Task) {
	a.state.currentTaskID = task.TaskID
	a.state.currentTurn = 0
	a.state.currentInTok = 0
	a.state.currentOutTok = 0
	defer func() {
		a.state.currentTaskID = ""
		a.state.currentTurn = 0
		a.state.currentInTok = 0
		a.state.currentOutTok = 0
		a.transition(StatusIdle, nil)
	}()

	a.startCycleTelemetry(task)

	start := time.Now()
	out := a.react(ctx, task)
	out.TaskID = task.TaskID
	out.AgentName = a.spec.Name
	out.AgentCycleID = task.TaskID
	out.Duration = time.Since(start)

	a.completeCycleTelemetry(task, out)

	// Persist per-agent token totals for the workspace. Best
	// effort: a sink failure logs but doesn't fail the dispatch.
	// Records every dispatch — including failed ones and
	// zero-token mock dispatches — so dispatch_count reflects
	// real usage and a pathological agent that errors fast
	// still surfaces its real frequency.
	if a.spec.TokenSink != nil {
		if err := a.spec.TokenSink.Record(a.spec.Name, out.InputTokens, out.OutputTokens); err != nil {
			a.logger.Warn("agent: token sink record failed", "task_id", task.TaskID, "err", err)
		}
	}

	// Reply is buffered 1; the send always succeeds. Defensive select
	// so a misbehaving caller (closed channel) doesn't panic the actor.
	defer func() {
		if r := recover(); r != nil {
			a.logger.Warn("agent reply send panicked", "task_id", task.TaskID, "recover", r)
		}
	}()
	task.Reply <- out
}

// startCycleTelemetry emits agent_cycle_start + StartCycle on the
// DAG. No-ops cleanly when CycleLog / DAG aren't configured.
func (a *Agent) startCycleTelemetry(task Task) {
	if a.spec.DAG != nil {
		a.spec.DAG.StartCycle(dag.CycleID(task.TaskID), dag.CycleID(task.ParentCycleID), dag.NodeAgent)
	}
	a.emitEvent(cyclelog.Event{
		Type:     cyclelog.EventAgentCycleStart,
		CycleID:  task.TaskID,
		ParentID: task.ParentCycleID,
		NodeType: string(dag.NodeAgent),
	})
}

// completeCycleTelemetry emits agent_cycle_complete + CompleteCycle.
func (a *Agent) completeCycleTelemetry(task Task, out Outcome) {
	status := cyclelog.StatusComplete
	dagStatus := dag.StatusComplete
	errMsg := ""
	if out.Err != nil {
		status = cyclelog.StatusError
		dagStatus = dag.StatusError
		errMsg = out.Err.Error()
	}
	if a.spec.DAG != nil {
		a.spec.DAG.CompleteCycle(dag.CycleID(task.TaskID), dagStatus, errMsg)
	}
	a.emitEvent(cyclelog.Event{
		Type:         cyclelog.EventAgentCycleComplete,
		CycleID:      task.TaskID,
		ParentID:     task.ParentCycleID,
		Status:       status,
		Error:        errMsg,
		InputTokens:  out.InputTokens,
		OutputTokens: out.OutputTokens,
	})
}

// emitEvent writes one event to the configured CycleLog, stamping
// the agent's InstanceID. Best-effort: errors log but don't fail
// the dispatch.
func (a *Agent) emitEvent(ev cyclelog.Event) {
	if a.spec.CycleLog == nil {
		return
	}
	if ev.InstanceID == "" {
		ev.InstanceID = a.spec.InstanceID
	}
	if err := a.spec.CycleLog.Emit(ev); err != nil {
		a.logger.Warn("agent: cyclelog emit failed", "type", string(ev.Type), "err", err)
	}
}

// react runs the bounded LLM-tool-LLM loop, returning a partial Outcome
// (handleTask fills in TaskID/AgentName/Duration after).
func (a *Agent) react(ctx context.Context, task Task) Outcome {
	history := llm.MessageHistory{}
	staged, err := history.Add(llm.UserText(task.Instruction))
	if err != nil {
		return Outcome{Err: fmt.Errorf("agent: build instruction history: %w", err)}
	}
	history = staged

	out := Outcome{}
	consecutiveErrors := 0
	// toolEventsCopy accumulates each successfully-dispatched tool's
	// (name, output) so the fabrication gate can verify subsequent
	// tool inputs against earlier outputs. Empty for low-risk-only
	// flows; only externalising tools (send_email etc.) consult it.
	var toolEventsCopy []FabricationToolEvent

	for turn := 1; turn <= a.spec.MaxTurns; turn++ {
		a.state.currentTurn = turn
		a.transition(StatusThinking, nil)

		// Emit llm_request — parent_id chains to the agent's own
		// task ID so the audit trail walks: cog cycle → agent
		// cycle (parent_id=cog) → llm_request (parent_id=agent).
		a.emitEvent(cyclelog.Event{
			Type:         cyclelog.EventLLMRequest,
			CycleID:      task.TaskID,
			ParentID:     task.TaskID,
			Model:        a.spec.Model,
			MessageCount: history.Len(),
			MaxTokens:    a.spec.MaxTokens,
		})

		resp, err := a.spec.Provider.Chat(ctx, llm.Request{
			Model:     a.spec.Model,
			System:    a.spec.SystemPrompt,
			Messages:  history.Messages(),
			MaxTokens: a.spec.MaxTokens,
			Tools:     a.spec.Tools.List(),
		})
		if err != nil {
			consecutiveErrors++
			a.logger.Warn("agent llm error", "task_id", task.TaskID, "turn", turn, "err", err, "consecutive", consecutiveErrors)
			a.emitEvent(cyclelog.Event{
				Type:     cyclelog.EventLLMResponse,
				CycleID:  task.TaskID,
				ParentID: task.TaskID,
				Error:    err.Error(),
			})
			if consecutiveErrors >= a.spec.MaxConsecutiveErrors {
				out.Err = fmt.Errorf("agent: %d consecutive LLM errors (last: %w)", consecutiveErrors, err)
				return out
			}
			continue
		}
		consecutiveErrors = 0
		out.InputTokens += resp.Usage.InputTokens
		out.OutputTokens += resp.Usage.OutputTokens
		// Mirror running totals into agent state so the next
		// transition() emission picks them up — webui shows
		// live cost without waiting for agent_cycle_complete.
		a.state.currentInTok = out.InputTokens
		a.state.currentOutTok = out.OutputTokens

		a.emitEvent(cyclelog.Event{
			Type:         cyclelog.EventLLMResponse,
			CycleID:      task.TaskID,
			ParentID:     task.TaskID,
			StopReason:   resp.StopReason,
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
		})

		toolUses := collectToolUses(resp.Content)
		if len(toolUses) == 0 {
			// Final text reply.
			out.Result = textFromContent(resp.Content)
			return out
		}

		// Tool-use turn: stage the assistant message, dispatch tools,
		// stage the tool results, loop.
		assistantMsg := llm.Message{Role: llm.RoleAssistant, Content: resp.Content}
		history, err = history.Add(assistantMsg)
		if err != nil {
			out.Err = fmt.Errorf("agent: stage assistant turn: %w", err)
			return out
		}

		a.transition(StatusUsingTools, toolNamesOf(toolUses))
		results := make([]llm.ContentBlock, 0, len(toolUses))
		for _, u := range toolUses {
			out.ToolsUsed = append(out.ToolsUsed, u.Name)
			a.emitEvent(cyclelog.Event{
				Type:         cyclelog.EventToolCall,
				CycleID:      task.TaskID,
				ParentID:     task.TaskID,
				ToolName:     u.Name,
				ToolInputLen: len(u.Input),
			})

			// Fabrication gate — runs BEFORE dispatch on
			// "externalising" tools (send_email, save_to_library,
			// etc.). Scores the tool input against the cycle's
			// accumulated tool log. Block/Escalate → refuse with an
			// IsError result the LLM can self-correct from. Skipped
			// when the gate isn't configured or the tool is
			// low-risk; both checks live inside the gate.
			if a.spec.FabricationGate != nil && a.spec.FabricationGate.ShouldGate(u.Name) {
				gateRes := a.spec.FabricationGate.ScoreToolInput(ctx, u.Name, string(u.Input), toolEventsCopy)
				a.emitEvent(cyclelog.Event{
					Type:        cyclelog.EventPolicyDecision,
					CycleID:     task.TaskID,
					ParentID:    task.TaskID,
					ToolName:    u.Name,
					Gate:        "tool_fabrication",
					Verdict:     gateRes.Verdict,
					PolicyScore: gateRes.Score,
					PolicyTrail: gateRes.Trail,
				})
				if gateRes.Verdict == "block" || gateRes.Verdict == "escalate" {
					refusal := fmt.Sprintf(
						"refused: tool input contains claims not supported by the cycle's tool log. "+
							"Run a tool to verify (e.g. brave_web_search for URLs) before retrying. "+
							"Flagged: %s",
						strings.Join(gateRes.FlaggedClaims, "; "),
					)
					a.emitEvent(cyclelog.Event{
						Type:     cyclelog.EventToolResult,
						CycleID:  task.TaskID,
						ParentID: task.TaskID,
						ToolName: u.Name,
						Success:  false,
						Error:    "fabrication gate refused dispatch",
					})
					results = append(results, llm.ToolResultBlock{
						ToolUseID: u.ID,
						Content:   refusal,
						IsError:   true,
					})
					continue
				}
			}

			res, derr := a.spec.Tools.Dispatch(ctx, u.Name, u.Input)
			if derr != nil {
				a.emitEvent(cyclelog.Event{
					Type:     cyclelog.EventToolResult,
					CycleID:  task.TaskID,
					ParentID: task.TaskID,
					ToolName: u.Name,
					Success:  false,
					Error:    derr.Error(),
				})
				results = append(results, llm.ToolResultBlock{
					ToolUseID: u.ID,
					Content:   derr.Error(),
					IsError:   true,
				})
				continue
			}
			a.emitEvent(cyclelog.Event{
				Type:     cyclelog.EventToolResult,
				CycleID:  task.TaskID,
				ParentID: task.TaskID,
				ToolName: u.Name,
				Success:  true,
			})
			toolEventsCopy = append(toolEventsCopy, FabricationToolEvent{Name: u.Name, Output: res})
			results = append(results, llm.ToolResultBlock{ToolUseID: u.ID, Content: res})
		}
		history, err = history.Add(llm.Message{Role: llm.RoleUser, Content: results})
		if err != nil {
			out.Err = fmt.Errorf("agent: stage tool results: %w", err)
			return out
		}
	}

	out.Err = fmt.Errorf("agent: max_turns=%d exhausted without final reply", a.spec.MaxTurns)
	return out
}

// transition updates the agent's status and emits an activity. Like
// cog.transition but simpler — no watchdog, no policy gates.
//
// Activity carries the in-flight task's running token totals so
// subscribers (TUI, webui SSE) can show the live cost without
// joining against the cycle log. Totals reset to zero between
// tasks (handleTask sets them before each react()).
func (a *Agent) transition(next Status, toolNames []string) {
	a.state.status = next
	select {
	case a.activities <- Activity{
		AgentName:    a.spec.Name,
		Status:       next,
		TaskID:       a.state.currentTaskID,
		Turn:         a.state.currentTurn,
		MaxTurns:     a.spec.MaxTurns,
		ToolNames:    toolNames,
		InputTokens:  a.state.currentInTok,
		OutputTokens: a.state.currentOutTok,
	}:
	default:
		// Subscriber not keeping up — drop. Status is ambient.
	}
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

func toolNamesOf(uses []llm.ToolUseBlock) []string {
	names := make([]string, len(uses))
	for i, u := range uses {
		names[i] = u.Name
	}
	return names
}

func textFromContent(blocks []llm.ContentBlock) string {
	var b strings.Builder
	for _, c := range blocks {
		if t, ok := c.(llm.TextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// toolListSnapshot extracts tool names for startup logging.
func toolListSnapshot(d ToolDispatcher) []string {
	tools := d.List()
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	return names
}
