package archivist

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/seamus-brady/retainer/internal/agent"
	"github.com/seamus-brady/retainer/internal/cbr"
	"github.com/seamus-brady/retainer/internal/cyclelog"
	"github.com/seamus-brady/retainer/internal/llm"
)

// Curator turns a completed cycle into a structured CurationResult
// the archivist can use to populate the Case.Problem / Solution /
// Outcome fields. Replaces the prior heuristic-derive + Judge
// pipeline, which produced verbatim user-text intents and binary
// success/failure outcomes (see
// `doc/specs/memory-and-logging-audit.md`).
//
// Two implementations:
//   - LLMCurator runs the two-phase Reflector → Curator pipeline
//     ported from SD's `narrative/archivist.gleam`. Phase 1 is a
//     plain-text reflection that walks through the load-bearing
//     "do prose claims and the tool log agree" question. Phase 2
//     is a structured-output call grounded in the reflection +
//     the cycle's tool log.
//   - HeuristicCurator is the LLM-free fallback (mock provider, no
//     API key, LLM errors). Produces a sensible structured shape
//     so cases stay shaped correctly even when the curator can't
//     run an LLM call.
//
// Concurrency: implementations must be safe for concurrent calls —
// the archivist runs one Curate per inbox message but tests spin
// many.
type Curator interface {
	Curate(ctx context.Context, in CurationInput) (CurationResult, error)
}

// CurationInput is the cycle context the curator works from. The
// load-bearing fields are ToolCalls + AgentCompletions — these
// are the ground truth the curation phase grounds its outcome
// assessment in. Without them, the curator can only judge from
// prose, which is what the prior Judge did and what produced
// the rubbish cases.
type CurationInput struct {
	UserInput  string
	ReplyText  string
	AgentsUsed []string
	ToolsUsed  []string
	// ToolCalls is the (name, success) record for every tool the
	// cog dispatched in this cycle. Ordered by dispatch order.
	// Empty when no tools fired (conversational cycle, mock echo).
	ToolCalls []ToolCallRecord
	// AgentCompletions is the per-dispatch record for every
	// `agent_<name>` call in this cycle. Carries the agent's
	// internal tool list + token usage so the curator can ground
	// claim-vs-tool checks past the cog level — essential for
	// catching fabricated agent results.
	AgentCompletions []agent.CompletionRecord
	// CycleStatus is the cog's own status assessment for the
	// cycle ("complete" / "error" / "abandoned" / "blocked").
	// LLMCurator uses it as a hint in the reflection prompt;
	// HeuristicCurator uses it directly as a tie-breaker for
	// success/failure when no tool log is available.
	CycleStatus string
	// ParentCycleID is the cog cycle id that triggered the
	// curation. Used by LLMCurator's telemetry events
	// (curator_reflection / curator_curation) so they can be
	// joined to the originating cycle in the cycle-log.
	// HeuristicCurator ignores it. Empty in tests / call paths
	// that don't need cycle-log attribution.
	ParentCycleID string
}

// ToolCallRecord is one (name, success) pair from the cycle's
// tool log. Pumped through CycleComplete from the cog.
type ToolCallRecord struct {
	Name    string
	Success bool
}

// CurationResult is the structured output of the curation phase.
// Fields map directly onto cbr.Problem / cbr.Solution /
// cbr.Outcome — the archivist composes these into a Case.
//
// Fields the curator may leave empty: Domain (curator couldn't
// determine), Pitfalls (success path; nothing to warn about),
// Steps (no procedural shape).
type CurationResult struct {
	IntentClassification cbr.IntentClassification
	IntentDescription    string
	Domain               string
	Entities             []string
	Keywords             []string
	QueryComplexity      string
	Approach             string
	Steps                []string
	Status               cbr.Status
	Confidence           float64
	Assessment           string
	Pitfalls             []string
}

// ---------------------------------------------------------------------------
// LLMCurator — two-phase Reflector → Curator pipeline
// ---------------------------------------------------------------------------

// LLMCurator runs reflection + structured curation against an
// llm.Provider. Cost: two LLM calls per case derivation. The
// archivist's actor loop runs Curate fire-and-forget so the cog
// never blocks on it.
type LLMCurator struct {
	Provider llm.Provider
	Model    string
	// MaxTokens caps the reflection + curation calls. Zero defaults
	// to 1024 (reflection ~200-400 tokens, curation ~400-800 with
	// the structured output overhead).
	MaxTokens int
	Logger    *slog.Logger
	// CycleLog receives curator_reflection + curator_curation
	// events when set. Phase 6 telemetry: each LLM call emits
	// model + tokens + duration + success so the operator can see
	// cost + reliability separately from the cog's own
	// llm_request/llm_response stream. Nil sink means tests +
	// pre-Phase-6 wiring don't need to know about it; emission
	// is silent then.
	CycleLog cyclelog.Sink
	// IDFn produces a fresh event id per emission. Defaults to
	// uuid.NewString. Overridable for deterministic test output.
	IDFn func() string
	// NowFn stamps event timestamps + measures duration. Defaults
	// to time.Now. Overridable for deterministic test output.
	NowFn func() time.Time
}

const defaultLLMCuratorMaxTokens = 1024

// Curate runs the two-phase pipeline. Errors from Phase 1
// (reflection) fall through to Phase 2 with an empty reflection —
// the curator can still produce a record from raw input, just
// without the reflection's grounding. Errors from Phase 2 are
// returned to the caller; the archivist falls back to the
// heuristic curator on this path.
func (c *LLMCurator) Curate(ctx context.Context, in CurationInput) (CurationResult, error) {
	if c.Provider == nil {
		return CurationResult{}, fmt.Errorf("llmcurator: Provider required")
	}
	if c.Model == "" {
		return CurationResult{}, fmt.Errorf("llmcurator: Model required")
	}
	maxTokens := c.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultLLMCuratorMaxTokens
	}

	// Phase 1 — plain-text reflection. Soft fail.
	reflection, refErr := c.reflect(ctx, in, maxTokens)
	if refErr != nil {
		c.logger().Warn("llmcurator: reflection failed; curating without reflection",
			"err", refErr,
		)
	}

	// Phase 2 — structured curation grounded in reflection + tool log.
	return c.curate(ctx, in, reflection, maxTokens)
}

const reflectionSystemPrompt = `You are the Reflector for an AI agent. Your job is to honestly assess what just happened in a cycle. Write in plain text — no JSON, no special formatting.

Work through these questions in order:

1. What did the user ask for or say?
2. What does the TOOLS FIRED list show actually happened? Which tools were called, and which succeeded?
3. What does the assistant's REPLY claim was done?
4. Do the reply's claims and the tool log agree? If the reply says "I searched X" or "I found Y", does the corresponding tool appear in TOOLS FIRED? Note any divergence plainly — this is the most important part of the reflection.
5. What worked well?
6. What failed or was unexpected?
7. What kind of cycle was this — a research query, a conversational ack, a system command, an exploration?
8. What should be remembered for future similar cycles?

Be brief and concrete. Two short paragraphs is usually enough.`

func (c *LLMCurator) reflect(ctx context.Context, in CurationInput, maxTokens int) (string, error) {
	user := buildReflectionUserPrompt(in)
	start := c.now()
	resp, err := c.Provider.Chat(ctx, llm.Request{
		Model:     c.Model,
		System:    reflectionSystemPrompt,
		MaxTokens: maxTokens,
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: user}}},
		},
	})
	durMs := c.now().Sub(start).Milliseconds()
	c.emitCallEvent(cyclelog.EventCuratorReflection, in.ParentCycleID, maxTokens, resp.Usage, resp.StopReason, err, durMs)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Text()), nil
}

func buildReflectionUserPrompt(in CurationInput) string {
	var b strings.Builder
	b.WriteString("USER INPUT:\n")
	b.WriteString(truncate(in.UserInput, 2000))
	b.WriteString("\n\nREPLY:\n")
	b.WriteString(truncate(in.ReplyText, 2000))
	b.WriteString("\n\n")
	b.WriteString(formatToolCalls(in.ToolCalls))
	if len(in.AgentCompletions) > 0 {
		b.WriteString("\n\n")
		b.WriteString(formatAgentCompletions(in.AgentCompletions))
	} else if len(in.AgentsUsed) > 0 {
		// Backstop when AgentCompletions isn't populated (legacy
		// path, tests). Just list names.
		b.WriteString("\n\nAGENTS DISPATCHED: ")
		b.WriteString(strings.Join(in.AgentsUsed, ", "))
	}
	return b.String()
}

// formatAgentCompletions renders the per-dispatch agent records
// for the reflector + curator prompts. The load-bearing detail
// is the "internal tools" list — when the cycle delegates to
// `agent_researcher`, the curator sees what tools the
// researcher itself fired, not just "agent_researcher: ok".
//
// Format mirrors SD's `format_agent_completions` (one bullet per
// dispatch) but adds the success flag explicitly so the LLM
// doesn't have to infer success from result text alone.
func formatAgentCompletions(comps []agent.CompletionRecord) string {
	if len(comps) == 0 {
		return "AGENT COMPLETIONS: (none)"
	}
	var b strings.Builder
	b.WriteString("AGENT COMPLETIONS:\n")
	for _, c := range comps {
		mark := "ok"
		if !c.Success {
			mark = "FAILED"
		}
		fmt.Fprintf(&b, "  - %s (%s)\n", c.AgentName, mark)
		if strings.TrimSpace(c.Instruction) != "" {
			fmt.Fprintf(&b, "      brief: %s\n", c.Instruction)
		}
		if len(c.ToolsUsed) > 0 {
			fmt.Fprintf(&b, "      internal tools: %s\n", strings.Join(c.ToolsUsed, ", "))
		} else {
			fmt.Fprintf(&b, "      internal tools: (none — agent made no tool calls)\n")
		}
		if c.ErrorMessage != "" {
			fmt.Fprintf(&b, "      error: %s\n", truncate(c.ErrorMessage, 200))
		}
		if c.InputTokens > 0 || c.OutputTokens > 0 {
			fmt.Fprintf(&b, "      tokens: %d in / %d out\n", c.InputTokens, c.OutputTokens)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatToolCalls(calls []ToolCallRecord) string {
	if len(calls) == 0 {
		return "TOOLS FIRED: (none — this cycle made no tool calls)"
	}
	var b strings.Builder
	b.WriteString("TOOLS FIRED:\n")
	for _, c := range calls {
		mark := "ok"
		if !c.Success {
			mark = "FAILED"
		}
		fmt.Fprintf(&b, "  - %s (%s)\n", c.Name, mark)
	}
	return strings.TrimRight(b.String(), "\n")
}

const curationSystemPrompt = `You are the Curator. You're given a reflection (plain-text analysis of a cycle), the cycle's user input + reply, and the tool-call log. Your job is to produce a structured first-person record of what happened.

You have two inputs you can check against each other: the REFLECTION (which is itself prose) and the TOOLS FIRED list (which is the ground truth of what actually ran). **Do not trust the reflection blindly.** If the reflection describes work the tool log does not support, the reflection is the one that's wrong — the tool log is authoritative. Anchor on tools, not on the reflection's self-report.

RULES:
- Use the controlled vocabulary for intent_classification — exactly one of: data_report, data_query, comparison, trend_analysis, monitoring_check, exploration, clarification, system_command, conversation
- Status: success / partial / failure
  - success: the task was attempted and the tool log supports the claimed work
  - partial: some work was done, but claims exceed what the tools show, OR expected tools didn't fire, OR the answer is incomplete
  - failure: the task was not accomplished, or the reply fabricated work that did not happen
- Pleasantries, greetings, acks, banter, and conversational follow-ups are intent_classification="conversation". Even if the reply is short ("Hello", "OK"), if it appropriately matches the input, status is "success" — not "failure" just because no work was done. A successful greeting is success.
- intent_description: one short phrase (5-10 words) describing what the user was trying to do. NOT the verbatim user text.
- domain: a single keyword for the subject area (e.g. "auth", "research", "scheduler", "conversation"). Empty when no clear domain.
- assessment: one or two sentences explaining the status grounded in the tool log.
- pitfalls: only populate when status is failure or partial AND there are concrete reusable lessons. Empty for clean successes and pleasantries.`

const curationToolName = "record_curation"

var curationSchema = llm.Schema{
	Name:        curationToolName,
	Description: "Record the structured curation of a completed cycle.",
	Properties: map[string]llm.Property{
		"intent_classification": {
			Type:        "string",
			Description: "Controlled vocabulary classification of the cycle's intent.",
			Enum: []string{
				"data_report", "data_query", "comparison", "trend_analysis",
				"monitoring_check", "exploration", "clarification",
				"system_command", "conversation",
			},
		},
		"intent_description": {
			Type:        "string",
			Description: "5-10 word description of what the user was trying to do. NOT the verbatim user text.",
		},
		"domain": {
			Type:        "string",
			Description: "Single keyword for the subject area (auth, research, scheduler, conversation, etc.). Empty when no clear domain.",
		},
		"query_complexity": {
			Type:        "string",
			Description: "How complex this query was.",
			Enum:        []string{"simple", "moderate", "complex"},
		},
		"entities": {
			Type:        "array",
			Description: "Named things mentioned (services, files, repos, people).",
			Items:       &llm.Property{Type: "string"},
		},
		"keywords": {
			Type:        "array",
			Description: "Salient non-entity terms.",
			Items:       &llm.Property{Type: "string"},
		},
		"approach": {
			Type:        "string",
			Description: "One-sentence description of what was done in response.",
		},
		"steps": {
			Type:        "array",
			Description: "Ordered list of what happened (when there's procedural shape).",
			Items:       &llm.Property{Type: "string"},
		},
		"status": {
			Type:        "string",
			Description: "How the cycle ended.",
			Enum:        []string{"success", "partial", "failure"},
		},
		"confidence": {
			Type:        "number",
			Description: "Curator's confidence in the status, 0.0-1.0.",
		},
		"assessment": {
			Type:        "string",
			Description: "One or two sentences explaining the status, grounded in the tool log.",
		},
		"pitfalls": {
			Type:        "array",
			Description: "Concrete reusable lessons. Only populate for partial/failure with real lessons.",
			Items:       &llm.Property{Type: "string"},
		},
	},
	Required: []string{"intent_classification", "status", "confidence"},
}

// curationPayload is the decode target for the Curator's
// structured output. Field-tagged to JSON-keys matching the
// schema's Property names.
type curationPayload struct {
	IntentClassification string   `json:"intent_classification"`
	IntentDescription    string   `json:"intent_description"`
	Domain               string   `json:"domain"`
	QueryComplexity      string   `json:"query_complexity"`
	Entities             []string `json:"entities"`
	Keywords             []string `json:"keywords"`
	Approach             string   `json:"approach"`
	Steps                []string `json:"steps"`
	Status               string   `json:"status"`
	Confidence           float64  `json:"confidence"`
	Assessment           string   `json:"assessment"`
	Pitfalls             []string `json:"pitfalls"`
}

func (c *LLMCurator) curate(ctx context.Context, in CurationInput, reflection string, maxTokens int) (CurationResult, error) {
	user := buildCurationUserPrompt(in, reflection)
	var payload curationPayload
	start := c.now()
	usage, err := c.Provider.ChatStructured(ctx, llm.Request{
		Model:     c.Model,
		System:    curationSystemPrompt,
		MaxTokens: maxTokens,
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: user}}},
		},
	}, curationSchema, &payload)
	durMs := c.now().Sub(start).Milliseconds()
	// ChatStructured doesn't surface a stop_reason; pass "" so the
	// event keeps the field empty rather than defaulting to a stale
	// reflection value.
	c.emitCallEvent(cyclelog.EventCuratorCuration, in.ParentCycleID, maxTokens, usage, "", err, durMs)
	if err != nil {
		return CurationResult{}, fmt.Errorf("llmcurator: curation: %w", err)
	}
	return resultFromPayload(payload), nil
}

func buildCurationUserPrompt(in CurationInput, reflection string) string {
	var b strings.Builder
	if reflection != "" {
		b.WriteString("REFLECTION (Phase 1 — verify against tool log before trusting):\n")
		b.WriteString(reflection)
		b.WriteString("\n\n---\n\n")
	}
	b.WriteString("USER INPUT:\n")
	b.WriteString(truncate(in.UserInput, 2000))
	b.WriteString("\n\nREPLY:\n")
	b.WriteString(truncate(in.ReplyText, 2000))
	b.WriteString("\n\n")
	b.WriteString(formatToolCalls(in.ToolCalls))
	if len(in.AgentCompletions) > 0 {
		b.WriteString("\n\n")
		b.WriteString(formatAgentCompletions(in.AgentCompletions))
	} else if len(in.AgentsUsed) > 0 {
		b.WriteString("\n\nAGENTS DISPATCHED: ")
		b.WriteString(strings.Join(in.AgentsUsed, ", "))
	}
	b.WriteString("\n\nCall the record_curation tool with your structured assessment.")
	return b.String()
}

func resultFromPayload(p curationPayload) CurationResult {
	conf := p.Confidence
	if conf < 0 {
		conf = 0
	}
	if conf > 1 {
		conf = 1
	}
	return CurationResult{
		IntentClassification: cbr.IntentClassification(p.IntentClassification),
		IntentDescription:    p.IntentDescription,
		Domain:               p.Domain,
		Entities:             p.Entities,
		Keywords:             p.Keywords,
		QueryComplexity:      p.QueryComplexity,
		Approach:             p.Approach,
		Steps:                p.Steps,
		Status:               cbr.Status(p.Status),
		Confidence:           conf,
		Assessment:           p.Assessment,
		Pitfalls:             p.Pitfalls,
	}
}

func (c *LLMCurator) logger() *slog.Logger {
	if c.Logger == nil {
		return slog.Default()
	}
	return c.Logger
}

// now returns the current time via NowFn (defaults to time.Now).
// Used for event timestamps + duration measurement so tests can
// inject a deterministic clock.
func (c *LLMCurator) now() time.Time {
	if c.NowFn == nil {
		return time.Now()
	}
	return c.NowFn()
}

// id produces a fresh event ID. Defaults to uuid.NewString.
func (c *LLMCurator) id() string {
	if c.IDFn == nil {
		return uuid.NewString()
	}
	return c.IDFn()
}

// emitCallEvent writes one cycle-log event for an LLMCurator LLM
// call (reflection or curation). No-op when CycleLog is unset
// (tests, pre-Phase-6 wiring). Errors from the sink are logged at
// warn level but never bubble — telemetry must not break curation.
//
// usage may be a zero-value llm.Usage when err != nil; the helper
// reads input/output tokens which default to 0. stopReason is the
// provider's reported stop signal (reflection only — ChatStructured
// doesn't surface it; pass "" there).
func (c *LLMCurator) emitCallEvent(
	evType cyclelog.EventType,
	parentCycleID string,
	maxTokens int,
	usage llm.Usage,
	stopReason string,
	callErr error,
	durMs int64,
) {
	if c.CycleLog == nil {
		return
	}
	ev := cyclelog.Event{
		Type:         evType,
		CycleID:      c.id(),
		ParentID:     parentCycleID,
		Timestamp:    c.now(),
		Model:        c.Model,
		MaxTokens:    maxTokens,
		MessageCount: 1, // both phases send a single user message
		StopReason:   stopReason,
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		DurationMs:   durMs,
		Success:      callErr == nil,
		Error:        errString(callErr),
	}
	if emitErr := c.CycleLog.Emit(ev); emitErr != nil {
		c.logger().Warn("llmcurator: cyclelog emit failed",
			"type", string(evType),
			"err", emitErr,
		)
	}
}

// errString returns err.Error() or "" for nil. Local helper so the
// archivist doesn't depend on cog/agent's errMsg.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ---------------------------------------------------------------------------
// HeuristicCurator — LLM-free fallback
// ---------------------------------------------------------------------------

// HeuristicCurator produces a sensible structured shape without
// any LLM calls. Used:
//   - When the workspace has no LLM provider (`mock` provider).
//   - When the LLMCurator returns an error (transport failure,
//     schema mismatch, etc.) — the archivist falls back here so
//     case derivation never breaks the post-cycle path.
//
// The heuristic's classification logic is deliberately simple —
// it tags conversational acks as `Conversation` (the load-bearing
// case for getting category=nil) and falls through to
// `Exploration` for everything else. Domain stays empty (the
// Curator's job, not a heuristic's). Pitfalls populated only when
// the cycle status is failure AND we can spot a concrete error
// pattern in the reply.
//
// This is NOT a replacement for the LLMCurator — it's the floor.
// LLM-curated cases are richer; heuristic-curated cases are
// "stored, audit-grade, but won't produce great patterns".
type HeuristicCurator struct{}

// Curate produces a CurationResult heuristically. Never returns
// an error — the archivist relies on this curator as a guaranteed
// fallback.
func (HeuristicCurator) Curate(_ context.Context, in CurationInput) (CurationResult, error) {
	classification := classifyHeuristic(in)
	status := statusHeuristic(in, classification)
	return CurationResult{
		IntentClassification: classification,
		IntentDescription:    intentDescriptionHeuristic(in.UserInput, classification),
		Domain:               "",
		Keywords:             headKeywords(in.UserInput, 6),
		QueryComplexity:      complexityHeuristic(in.UserInput),
		Approach:             firstSentence(strings.TrimSpace(in.ReplyText), 200),
		Status:               status,
		Confidence:           confidenceHeuristic(status),
		Assessment:           assessmentHeuristic(status, classification, in),
	}, nil
}

// classifyHeuristic detects conversational cycles by exact
// match or strict prefix against a small ack list. Everything
// else is Exploration — the safe non-Conversation default.
//
// Errs on the side of Exploration: a misclassified Exploration
// still gets a category in CBR; a misclassified Conversation
// gets dropped from CBR retrieval entirely. When in doubt,
// preserve the case as a learnable pattern. The LLMCurator does
// proper classification when it's available; this is the floor
// for when it isn't.
func classifyHeuristic(in CurationInput) cbr.IntentClassification {
	text := strings.ToLower(strings.TrimSpace(in.UserInput))
	if text == "" {
		return cbr.IntentConversation
	}
	for _, ack := range conversationalAcks {
		if text == ack ||
			strings.HasPrefix(text, ack+" ") ||
			strings.HasPrefix(text, ack+",") ||
			strings.HasPrefix(text, ack+".") {
			return cbr.IntentConversation
		}
	}
	return cbr.IntentExploration
}

// conversationalAcks are short user-input prefixes that almost
// certainly mean "this is conversational, not a request for work".
// Lowercased; comparison is exact-match or whitespace/comma-prefix.
var conversationalAcks = []string{
	"hi", "hello", "hey", "yo", "ok", "okay", "thanks", "ta",
	"please", "yes", "yep", "yeah", "no", "nope", "sure",
	"cool", "great", "nice", "bye", "goodbye", "cheers",
}

// statusHeuristic decides success/partial/failure when the
// curator can't ask an LLM. Order:
//
//  1. Cog status "abandoned" / "blocked" → failure (cog gave up).
//  2. Cog status "error" → failure.
//  3. Empty reply → failure.
//  4. Conversation classification + non-empty reply → success.
//  5. Any tool failed → partial.
//  6. Default → success.
//
// The cog's status is a strong signal — if the cycle terminated
// abnormally per the cog itself, treat it as failure regardless
// of the reply text.
func statusHeuristic(in CurationInput, c cbr.IntentClassification) cbr.Status {
	switch in.CycleStatus {
	case "error", "abandoned", "blocked":
		return cbr.StatusFailure
	}
	if strings.TrimSpace(in.ReplyText) == "" {
		return cbr.StatusFailure
	}
	if c == cbr.IntentConversation {
		return cbr.StatusSuccess
	}
	for _, t := range in.ToolCalls {
		if !t.Success {
			return cbr.StatusPartial
		}
	}
	return cbr.StatusSuccess
}

// confidenceHeuristic — heuristic confidence per status, kept low
// to match the floor-quality nature of the heuristic curator.
// LLM-curated confidence will be higher when the LLM is available.
func confidenceHeuristic(s cbr.Status) float64 {
	switch s {
	case cbr.StatusSuccess:
		return 0.7
	case cbr.StatusPartial:
		return 0.5
	case cbr.StatusFailure:
		return 0.5
	}
	return 0.5
}

// intentDescriptionHeuristic produces a short human-readable
// description of the cycle's intent without an LLM call. Avoids
// returning the verbatim user text — that's what produced the
// rubbish cases the audit flagged.
func intentDescriptionHeuristic(userInput string, c cbr.IntentClassification) string {
	switch c {
	case cbr.IntentConversation:
		return "operator engaged conversationally"
	case cbr.IntentExploration:
		s := strings.TrimSpace(userInput)
		if len(s) > 60 {
			s = s[:60] + "..."
		}
		return "exploration: " + s
	}
	return string(c)
}

// complexityHeuristic — coarse three-bucket split by length +
// tool-call count.
func complexityHeuristic(userInput string) string {
	n := len(strings.TrimSpace(userInput))
	switch {
	case n < 30:
		return "simple"
	case n < 200:
		return "moderate"
	default:
		return "complex"
	}
}

// assessmentHeuristic builds a short prose assessment for the
// outcome. Matches the structure SD's Curator produces — one or
// two sentences anchored on the tool log.
func assessmentHeuristic(s cbr.Status, c cbr.IntentClassification, in CurationInput) string {
	if c == cbr.IntentConversation && s == cbr.StatusSuccess {
		return "Conversational acknowledgement handled appropriately."
	}
	if s == cbr.StatusSuccess {
		if len(in.ToolCalls) > 0 {
			names := make([]string, 0, len(in.ToolCalls))
			for _, t := range in.ToolCalls {
				names = append(names, t.Name)
			}
			return fmt.Sprintf("Cycle completed with %d tool call(s): %s.", len(in.ToolCalls), strings.Join(dedupe(names), ", "))
		}
		return "Cycle completed without tool calls."
	}
	if s == cbr.StatusPartial {
		var failed []string
		for _, t := range in.ToolCalls {
			if !t.Success {
				failed = append(failed, t.Name)
			}
		}
		if len(failed) > 0 {
			return fmt.Sprintf("Cycle completed but tool(s) failed: %s.", strings.Join(dedupe(failed), ", "))
		}
		return "Cycle completed with reduced confidence."
	}
	if strings.TrimSpace(in.ReplyText) == "" {
		return "Cycle ended without producing a reply."
	}
	return "Cycle status: failure."
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// truncate caps a string at n bytes (UTF-8 safe — slices on byte
// boundaries; multi-byte chars at the boundary are preserved by
// the trailing ellipsis).
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// firstSentence returns the first sentence of s (up to first . or
// max chars), trimmed. Used by the heuristic curator for the
// approach summary.
func firstSentence(s string, max int) string {
	s = strings.TrimSpace(s)
	if dot := strings.Index(s, "."); dot >= 0 && dot < max {
		return strings.TrimSpace(s[:dot])
	}
	if len(s) > max {
		return strings.TrimSpace(s[:max])
	}
	return s
}

// headKeywords picks short keywords from text — first n distinct
// words ≥ 4 chars. Heuristic-only; LLMCurator overrides with a
// proper extracted set.
func headKeywords(text string, n int) []string {
	words := strings.Fields(strings.ToLower(text))
	seen := map[string]bool{}
	out := make([]string, 0, n)
	for _, w := range words {
		w = strings.Trim(w, ".,!?;:\"'()[]")
		if len(w) < 4 || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
		if len(out) >= n {
			break
		}
	}
	return out
}

// dedupe returns input with duplicates removed, preserving first-
// seen order.
func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// _ time.Time — review-traceability anchor: the curator's
// CurationInput could carry a Timestamp later for time-sensitive
// classifications (e.g. "trending news now vs trending news 2y
// ago"). Today we work from in-memory cycle data only.
var _ = time.Now
