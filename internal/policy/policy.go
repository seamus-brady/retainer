// Package policy is Retainer's port of Springdrift's D' / dprime safety
// scoring layer. The Engine evaluates inputs, outputs, tool calls, and
// post-execution outputs against a layered policy:
//
//   - Layer 1 (deterministic): regex prefilter from a JSON ruleset.
//   - Canary probes (input gate only): fresh-sentinel hijack + leakage
//     checks via two parallel LLM calls. Detects prompt-injection that
//     regex misses.
//
// Out of scope (deferred / dropped):
//
//   - Layer 2 (LLM scorer with structured output) — deferred.
//   - Layer 3 (meta observer) — deferred.
//   - Normative calculus / FlourishingVerdict / Stoic axioms — out entirely.
//
// Cog wires call sites in slice 6; this package is the engine, callable
// from the cog or future agents.
package policy

import (
	"context"
)

// Verdict is the gate's decision. Allow passes through, Escalate surfaces
// to the operator, Block hard-rejects.
type Verdict int

const (
	Allow Verdict = iota
	Escalate
	Block
)

func (v Verdict) String() string {
	switch v {
	case Allow:
		return "allow"
	case Escalate:
		return "escalate"
	case Block:
		return "block"
	}
	return "unknown"
}

// GateKind identifies which gate is being evaluated. Reserved values
// (Tool, PostExec) compile but go unused until tools land.
type GateKind int

const (
	GateInput GateKind = iota
	GateTool
	GateOutput
	GatePostExec
)

func (g GateKind) String() string {
	switch g {
	case GateInput:
		return "input"
	case GateTool:
		return "tool"
	case GateOutput:
		return "output"
	case GatePostExec:
		return "post_exec"
	}
	return "unknown"
}

// Source describes where the input originated. Interactive inputs (operator
// in TUI) get Block-to-Escalate demotion on the input gate so adversarial
// content typed for testing isn't hard-blocked. Autonomous inputs (scheduler,
// external mail/webhook) preserve the hard reject.
type Source int

const (
	SourceInteractive Source = iota
	SourceAutonomous
)

// Trigger is one rule or probe that fired during evaluation. Source values:
// "deterministic", "canary.hijack", "canary.leakage".
type Trigger struct {
	Source     string
	RuleName   string  // empty for canary
	Domain     string  // optional categorisation
	Importance float64 // 0 for canary
	Magnitude  float64 // 0 for canary
}

// Result is one evaluation's full outcome. Trail is human-readable, suitable
// for cycle-log and TUI display.
type Result struct {
	Verdict      Verdict
	Score        float64
	Triggered    []Trigger
	Trail        string
	Inconclusive bool // canary couldn't decide (LLM error)
}

// Config builds an Engine.
type Config struct {
	Rules       *RuleSet
	Canary      CanaryConfig             // zero value disables canary probes
	LLMScorer   LLMScorerConfig          // zero value disables L2 input disambiguation
	Fabrication FabricationScorerConfig  // zero value disables output-side fabrication detection
}

// Engine is the policy evaluator. Layers in priority order during input
// evaluation:
//   1. L1 deterministic regex prefilter (always)
//   2. L2 LLM scorer (when L1 has candidates AND configured) — disambiguates L1
//   3. Canary probes (when configured) — independent attack-detection signal
type Engine struct {
	rules       *RuleSet
	canary      *canaryProbes
	llmScorer   *llmScorer
	fabrication *fabricationScorer
}

// New constructs an Engine. If cfg.Canary.Provider is nil, canary probes
// are disabled. If cfg.LLMScorer.Provider is nil, L2 disambiguation is
// disabled and L1's verdict stands.
func New(cfg Config) *Engine {
	if cfg.Rules == nil {
		panic("policy: Rules is required")
	}
	e := &Engine{rules: cfg.Rules}
	if cfg.Canary.Provider != nil {
		e.canary = newCanaryProbes(cfg.Canary)
	}
	if cfg.LLMScorer.Provider != nil {
		e.llmScorer = newLLMScorer(cfg.LLMScorer)
	}
	if cfg.Fabrication.Provider != nil {
		e.fabrication = newFabricationScorer(cfg.Fabrication)
	}
	return e
}

// EvaluateOutputFabrication runs the LLM-scored fabrication
// gate. Returns Allow when no scorer is configured (zero-value
// FabricationScorerConfig at engine construction) — the
// deterministic EvaluateOutput is unaffected and runs
// independently. The cog calls this AFTER EvaluateOutput so the
// deterministic rules (credential leakage, etc) still apply
// regardless of the scorer's availability.
//
// toolLog is the cycle's tool name+output pairs. Empty toolLog
// means no tools fired this cycle — fabrication detection
// short-circuits to Allow because there's nothing to verify
// against (conversational reply with no tools = no claims to
// ground).
//
// Always returns a Result, never an error — fail-open if the
// scorer is unreachable. The Inconclusive flag on the Result
// signals a scorer-side issue the operator can investigate.
func (e *Engine) EvaluateOutputFabrication(ctx context.Context, output string, toolLog []ToolEvent) Result {
	if e.fabrication == nil {
		return Result{Verdict: Allow, Trail: "fabrication scorer not configured"}
	}
	return e.fabrication.Score(ctx, output, toolLog)
}

// HasFabricationScorer reports whether the engine has a
// fabrication scorer wired. Cog uses this to decide whether
// to bother collecting the cycle's tool log for the call.
func (e *Engine) HasFabricationScorer() bool {
	return e.fabrication != nil
}

// IsHighRiskTool reports whether the named tool's INPUTS
// should be fabrication-scored before dispatch. Returns false
// when the scorer isn't configured (no work to do anyway).
// Used by both the cog and the agent substrate to short-
// circuit the scorer call for benign tools.
func (e *Engine) IsHighRiskTool(toolName string) bool {
	if e.fabrication == nil {
		return false
	}
	return e.fabrication.IsHighRiskTool(toolName)
}

// EvaluateToolFabrication scores a TOOL INPUT against the
// cycle's accumulated tool log. Catches the case where a
// model composes a tool argument (email body, document
// content) containing claims not grounded in any prior
// tool's output — the substantive Mistral failure mode the
// existing output gate doesn't reach.
//
// Returns Allow with a "no scorer configured" trail when
// fabrication isn't wired (caller should treat that as
// "proceed"). Always returns a Result; never errors.
func (e *Engine) EvaluateToolFabrication(ctx context.Context, toolName, input string, toolLog []ToolEvent) Result {
	if e.fabrication == nil {
		return Result{Verdict: Allow, Trail: "fabrication scorer not configured"}
	}
	return e.fabrication.ScoreToolInput(ctx, toolName, input, toolLog)
}

// EvaluateInput runs L1, then L2 (when configured AND L1 had candidates),
// then canary probes (when configured). L2 has final say on L1's call —
// it can downgrade Block to Allow when the matched pattern is in a benign
// context. Canary is an independent signal: a hijack/leakage detection
// blocks regardless of what L2 said. Interactive sources get
// Block→Escalate demotion applied at the end.
func (e *Engine) EvaluateInput(ctx context.Context, text string, source Source) Result {
	det := evaluateDeterministic(text, e.rules.Input, e.rules.InputThreshold)

	final := det

	if e.llmScorer != nil && len(det.Triggered) > 0 {
		l2 := e.llmScorer.score(ctx, GateInput, text, det.Triggered)
		final = mergeL1L2(det, l2)
	}

	if e.canary != nil {
		canaryRes := e.canary.run(ctx, text)
		final = mergeWithCanary(final, canaryRes)
	}

	return demoteIfInteractive(final, source)
}

// EvaluateOutput runs L1 deterministic on the assistant's reply. Interactive
// cycles use deterministic-only per the arch notes (operator is the quality
// gate). No canary probes on outputs — they're built for input-injection
// detection.
func (e *Engine) EvaluateOutput(text string) Result {
	return evaluateDeterministic(text, e.rules.Output, e.rules.OutputThreshold)
}

// EvaluateTool scores a tool invocation. Path/domain allowlists short-circuit
// to Allow when the tool is one of the trusted file-read or fetch operations.
// LLM scorer (L2) for tool gate is deferred — slice 5 is deterministic-only
// here too.
func (e *Engine) EvaluateTool(toolName, args string) Result {
	if isAllowlisted(args, e.rules.PathAllowlist) {
		return Result{Verdict: Allow, Trail: "tool gate: path allowlisted"}
	}
	if isAllowlisted(args, e.rules.DomainAllowlist) {
		return Result{Verdict: Allow, Trail: "tool gate: domain allowlisted"}
	}
	payload := toolName + " " + args
	return evaluateDeterministic(payload, e.rules.Tool, e.rules.ToolThreshold)
}

// EvaluatePostExec scores tool output before it re-enters the conversation.
// Catches tool results that themselves contain prompt injection.
func (e *Engine) EvaluatePostExec(toolName, output string) Result {
	return evaluateDeterministic(output, e.rules.PostExec, e.rules.PostExecThreshold)
}

// EvaluateCommsOutbound runs the comms-specific deterministic rules
// against an outbound email body. Distinct from EvaluateOutput so
// rules tightly coupled to external email (credential exposure,
// internal URLs, system jargon recipients shouldn't see) don't apply
// to TUI replies, where some of the same patterns are legitimate
// (the operator can ask "what's a bearer token?" without tripping a
// block).
//
// The agent's send_email tool calls this before the HTTP send. A
// Block decision short-circuits the tool with an `IsError` result;
// Escalate surfaces to the operator as a policy_decision event but
// the send proceeds (gates wired into the cog handle the
// short-circuit + cycle-log emission for higher-level surfaces; this
// helper just returns the Result).
func (e *Engine) EvaluateCommsOutbound(text string) Result {
	return evaluateDeterministic(text, e.rules.CommsOutbound, e.rules.CommsOutboundThreshold)
}

// EvaluateCommsInbound runs the input-gate rules against an inbound
// email body — same threat model (prompt injection, role override,
// system-prompt extraction). The poller calls this before either
// emitting a Notice or triggering an autonomous Submit, so attacker-
// controlled mail content can't slip past the gates the cog
// otherwise applies to operator input.
//
// Always uses SourceAutonomous semantics — interactive demotion
// (Block→Escalate) doesn't apply here; mail is never operator-
// originated even when the operator is who they appear to be.
func (e *Engine) EvaluateCommsInbound(text string) Result {
	// Reuses the input-policies slice rather than introducing yet
	// another rule set. Same prompt-injection threat model; if the
	// operator wants different sensitivity for mail bodies they can
	// shift InputThreshold.
	return evaluateDeterministic(text, e.rules.Input, e.rules.InputThreshold)
}

func demoteIfInteractive(r Result, source Source) Result {
	if source != SourceInteractive {
		return r
	}
	if r.Verdict != Block {
		return r
	}
	r.Verdict = Escalate
	r.Trail = "[interactive demotion: block→escalate] " + r.Trail
	return r
}

// mergeL1L2: L2 disambiguates L1's verdict. L2 wins on the verdict (it can
// downgrade Block→Allow); L1's triggers + trail are preserved alongside
// L2's reasoning.
func mergeL1L2(l1, l2 Result) Result {
	score := l1.Score
	if l2.Score > score {
		score = l2.Score
	}
	return Result{
		Verdict:      l2.Verdict,
		Score:        score,
		Triggered:    append([]Trigger{}, l1.Triggered...),
		Trail:        l1.Trail + "; " + l2.Trail,
		Inconclusive: l2.Inconclusive,
	}
}

// mergeWithCanary: canary's Block overrides current verdict regardless of
// what L1+L2 said. Allows pure additivity — canary can only escalate, not
// downgrade.
func mergeWithCanary(current, canary Result) Result {
	merged := current
	merged.Triggered = append(merged.Triggered, canary.Triggered...)
	if canary.Trail != "" {
		if merged.Trail != "" {
			merged.Trail += "; "
		}
		merged.Trail += canary.Trail
	}
	if canary.Verdict == Block {
		merged.Verdict = Block
		if canary.Score > merged.Score {
			merged.Score = canary.Score
		}
	}
	if canary.Inconclusive {
		merged.Inconclusive = true
	}
	return merged
}

func isAllowlisted(value string, list []string) bool {
	for _, entry := range list {
		if entry != "" && value == entry {
			return true
		}
	}
	return false
}
