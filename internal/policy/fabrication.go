package policy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/seamus-brady/retainer/internal/llm"
)

// defaultFabricationTimeout caps the fabrication-scorer LLM
// call. Shorter than the input L2 scorer because the cog runs
// this on the output gate path AFTER the LLM has already
// produced a reply — the operator is waiting and another
// long round-trip degrades the chat-feel.
const defaultFabricationTimeout = 6 * time.Second

// defaultFabricationMinConfidence is the threshold at which a
// "suspicious" verdict gates the output. Below this, the
// scorer's signal lands in the cycle log as telemetry but
// doesn't change the assistant's reply. Tuned conservatively
// (0.7) so false positives don't spam the operator with
// verification footers on every reply.
const defaultFabricationMinConfidence = 0.7

// FabricationScorerConfig wires the scorer. Provider non-nil
// activates it; otherwise the cog's output gate runs
// deterministic-only (the V1-shipped behaviour).
type FabricationScorerConfig struct {
	Provider llm.Provider
	Model    string
	// Timeout caps the per-output LLM call. Zero picks
	// defaultFabricationTimeout.
	Timeout time.Duration
	// MinConfidence is the threshold for escalating a
	// "suspicious" verdict to a policy-gate Escalate. Below
	// the threshold the verdict is logged but not gated. Zero
	// picks defaultFabricationMinConfidence.
	MinConfidence float64
	// HighRiskTools is the set of tool names whose INPUTS get
	// fabrication-scored before dispatch (in addition to the
	// existing OUTPUT gate that scores the assistant's reply).
	// These are tools that produce externally-visible side
	// effects with content — fabricated email bodies, drafts,
	// exports — where flagging in the post-hoc reply is too
	// late. Empty / nil picks defaultHighRiskTools. Set to a
	// non-nil empty slice to disable input-side scoring.
	HighRiskTools []string
}

// defaultHighRiskTools is the conservative starting set: the
// Retainer tools that externalise content (send mail, write
// files the operator will read, generate PDF). Adding a new
// content-emitting tool? Add it here.
var defaultHighRiskTools = []string{
	"send_email",
	"save_to_library",
	"export_pdf",
	"create_draft",
	"update_draft",
	"promote_draft",
}

type fabricationScorer struct {
	provider      llm.Provider
	model         string
	timeout       time.Duration
	minConfidence float64
	highRisk      map[string]struct{}
}

func newFabricationScorer(cfg FabricationScorerConfig) *fabricationScorer {
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultFabricationTimeout
	}
	if cfg.MinConfidence == 0 {
		cfg.MinConfidence = defaultFabricationMinConfidence
	}
	highRisk := cfg.HighRiskTools
	if highRisk == nil {
		highRisk = defaultHighRiskTools
	}
	hrSet := make(map[string]struct{}, len(highRisk))
	for _, name := range highRisk {
		hrSet[name] = struct{}{}
	}
	return &fabricationScorer{
		provider:      cfg.Provider,
		model:         cfg.Model,
		timeout:       cfg.Timeout,
		minConfidence: cfg.MinConfidence,
		highRisk:      hrSet,
	}
}

// IsHighRiskTool reports whether the named tool's INPUTS should be
// fabrication-scored before dispatch. The agent (and cog) calls
// this to skip the LLM round-trip on benign tools like
// brave_web_search.
func (s *fabricationScorer) IsHighRiskTool(name string) bool {
	if s == nil {
		return false
	}
	_, ok := s.highRisk[name]
	return ok
}

// ToolEvent is the bare minimum the fabrication scorer needs:
// a tool name and the textual output the cycle produced from
// it. The cog populates this from its react-loop's tool-result
// blocks. We intentionally don't carry the JSON input — the
// scorer is checking the OUTPUT against the EVIDENCE, and the
// evidence is what tools returned, not what was asked for.
type ToolEvent struct {
	Name   string
	Output string
}

// fabricationVerdict is the structured output the LLM is forced
// to produce. Uses the same json-schema shape as l2Verdict but
// with a fabrication-specific field set.
type fabricationVerdict struct {
	Verdict       string   `json:"verdict"`
	Reasoning     string   `json:"reasoning"`
	Confidence    float64  `json:"confidence"`
	FlaggedClaims []string `json:"flagged_claims,omitempty"`
}

var fabricationSchema = llm.Schema{
	Name: "fabrication_verdict",
	Description: "Detect specific technical claims in the assistant's reply that aren't supported by the cycle's tool log. " +
		"Allow grounded claims; escalate confidently-stated unsupported claims.",
	Properties: map[string]llm.Property{
		"verdict": {
			Type:        "string",
			Description: "allow if all specific identifier-naming claims are grounded in the tool log; escalate if 1+ confident claims look unsupported.",
			Enum:        []string{"allow", "escalate"},
		},
		"reasoning": {
			Type:        "string",
			Description: "One short paragraph explaining the verdict. Cite which tool outputs you considered.",
		},
		"confidence": {
			Type:        "number",
			Description: "Confidence in the verdict, between 0.0 and 1.0. High confidence + escalate verdict triggers the policy gate.",
		},
		"flagged_claims": {
			Type:        "array",
			Description: "When the verdict is escalate, the specific claim strings from the reply that look unsupported. Each entry is a short quote + one-clause reason.",
			Items: &llm.Property{
				Type: "string",
			},
		},
	},
	Required: []string{"verdict", "reasoning", "confidence"},
}

// Score runs the fabrication check. Returns a Result whose
// Verdict is Allow (grounded), Escalate (suspicious — gating
// applied), or Allow with Inconclusive=true (scorer errored —
// fail-open so cog progress isn't blocked by the safety
// layer). The cog logs the Result via cycle-log either way.
//
// Always returns a Result, never an error — operator-friendly:
// the safety layer should never crash the cycle.
func (s *fabricationScorer) Score(ctx context.Context, output string, toolLog []ToolEvent) Result {
	// Empty output / no tool log → nothing to verify against.
	// Allow with a trail that explains why the scorer didn't
	// run substantively.
	if strings.TrimSpace(output) == "" {
		return Result{Verdict: Allow, Trail: "fabrication: empty output"}
	}
	if len(toolLog) == 0 {
		// No tools fired in this cycle → no claims to ground.
		// Conversational replies / acknowledgements pass.
		return Result{Verdict: Allow, Trail: "fabrication: no tool log to verify against"}
	}

	scoreCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	system := buildFabricationSystemPrompt()
	user := buildFabricationUserPrompt(output, toolLog)

	var v fabricationVerdict
	_, err := s.provider.ChatStructured(scoreCtx, llm.Request{
		Model:    s.model,
		System:   system,
		Messages: []llm.Message{llm.UserText(user)},
	}, fabricationSchema, &v)
	if err != nil {
		// Fail-open: if the scorer is unreachable / times out,
		// we don't gate. The cycle log records the failure so
		// the operator can investigate scorer reliability.
		return Result{
			Verdict:      Allow,
			Inconclusive: true,
			Trail:        fmt.Sprintf("fabrication scorer error: %v; allowing", err),
		}
	}

	if v.Confidence < 0 || v.Confidence > 1 {
		return Result{
			Verdict:      Allow,
			Inconclusive: true,
			Trail:        fmt.Sprintf("fabrication: confidence %.2f out of [0,1]; allowing", v.Confidence),
		}
	}

	switch v.Verdict {
	case "allow":
		return Result{
			Verdict: Allow,
			Score:   v.Confidence,
			Trail:   fmt.Sprintf("fabrication: allow confidence=%.2f reasoning=%q", v.Confidence, truncateForTrail(v.Reasoning)),
		}
	case "escalate":
		// Below threshold → record the signal but don't gate.
		// Above threshold → escalate so the cog appends a
		// verification footer. We keep it Allow on the wire when
		// below threshold so the trail still appears in cycle log
		// while the operator's response stays clean.
		if v.Confidence < s.minConfidence {
			return Result{
				Verdict: Allow,
				Score:   v.Confidence,
				Trail: fmt.Sprintf("fabrication: subthreshold escalate (%.2f < %.2f); reasoning=%q",
					v.Confidence, s.minConfidence, truncateForTrail(v.Reasoning)),
			}
		}
		return Result{
			Verdict: Escalate,
			Score:   v.Confidence,
			Trail: fmt.Sprintf("fabrication: escalate confidence=%.2f reasoning=%q flagged=%s",
				v.Confidence, truncateForTrail(v.Reasoning), formatFlagged(v.FlaggedClaims)),
			Triggered: claimsToTriggers(v.FlaggedClaims),
		}
	}

	return Result{
		Verdict:      Allow,
		Inconclusive: true,
		Trail:        fmt.Sprintf("fabrication: unknown verdict %q; allowing", v.Verdict),
	}
}

// ScoreToolInput is the tool-INPUT-side counterpart to Score.
// Used by the agent substrate (and cog dispatch path) to verify
// that a tool's arguments don't contain claims unsupported by
// the cycle's accumulated tool log — the Mistral failure mode
// where a model composes an email body containing fabricated
// URLs that no tool retrieved.
//
// Behavioural shape mirrors Score: empty input or empty log →
// short-circuit Allow with a trail explaining why. Network /
// timeout failures → fail-open Allow with Inconclusive=true.
// LLM verdict drives Verdict + Score + flagged claims.
//
// Differs from Score in two ways:
//
//  1. The user prompt names the tool being dispatched and frames
//     the input as "the model wants to call <tool> with these
//     args; check whether the args contain claims not in the
//     tool log".
//  2. Verdict semantics: at threshold OR above, returns Block
//     rather than Escalate. Tool dispatch happens once; we can't
//     "append a verification footer" to an email that's about to
//     be sent. Either let it through or refuse the dispatch so
//     the LLM gets a chance to verify.
func (s *fabricationScorer) ScoreToolInput(ctx context.Context, toolName, input string, toolLog []ToolEvent) Result {
	if strings.TrimSpace(input) == "" {
		return Result{Verdict: Allow, Trail: "fabrication: empty tool input"}
	}
	if len(toolLog) == 0 {
		// No prior tools → no claims to ground. Allow but record
		// the reasoning so the cycle log explains the bypass.
		return Result{Verdict: Allow, Trail: "fabrication: no tool log to verify against (tool input)"}
	}

	scoreCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	system := buildFabricationToolInputSystemPrompt()
	user := buildFabricationToolInputUserPrompt(toolName, input, toolLog)

	var v fabricationVerdict
	_, err := s.provider.ChatStructured(scoreCtx, llm.Request{
		Model:    s.model,
		System:   system,
		Messages: []llm.Message{llm.UserText(user)},
	}, fabricationSchema, &v)
	if err != nil {
		return Result{
			Verdict:      Allow,
			Inconclusive: true,
			Trail:        fmt.Sprintf("fabrication tool-input scorer error: %v; allowing", err),
		}
	}

	if v.Confidence < 0 || v.Confidence > 1 {
		return Result{
			Verdict:      Allow,
			Inconclusive: true,
			Trail:        fmt.Sprintf("fabrication: tool-input confidence %.2f out of [0,1]; allowing", v.Confidence),
		}
	}

	switch v.Verdict {
	case "allow":
		return Result{
			Verdict: Allow,
			Score:   v.Confidence,
			Trail:   fmt.Sprintf("fabrication: tool-input allow tool=%q confidence=%.2f reasoning=%q", toolName, v.Confidence, truncateForTrail(v.Reasoning)),
		}
	case "escalate":
		// Below threshold → log + allow (don't block on weak
		// signal; tool dispatch is one-shot, false positives
		// hurt operator flow).
		if v.Confidence < s.minConfidence {
			return Result{
				Verdict: Allow,
				Score:   v.Confidence,
				Trail:   fmt.Sprintf("fabrication: tool-input subthreshold (%.2f < %.2f) tool=%q reasoning=%q", v.Confidence, s.minConfidence, toolName, truncateForTrail(v.Reasoning)),
			}
		}
		// Above threshold → BLOCK. The agent should refuse the
		// dispatch with an IsError result the LLM can react to;
		// can't recall a sent email after the fact.
		return Result{
			Verdict: Block,
			Score:   v.Confidence,
			Trail:   fmt.Sprintf("fabrication: tool-input block tool=%q confidence=%.2f reasoning=%q flagged=%s", toolName, v.Confidence, truncateForTrail(v.Reasoning), formatFlagged(v.FlaggedClaims)),
			Triggered: claimsToTriggers(v.FlaggedClaims),
		}
	}

	return Result{
		Verdict:      Allow,
		Inconclusive: true,
		Trail:        fmt.Sprintf("fabrication: unknown tool-input verdict %q tool=%q; allowing", v.Verdict, toolName),
	}
}

// FlaggedClaims extracts the per-claim strings from a Result
// produced by the fabrication scorer. Returns nil for results
// from any other source. Used by the cog to format a
// verification footer the operator sees in chat.
func FlaggedClaims(r Result) []string {
	out := make([]string, 0, len(r.Triggered))
	for _, t := range r.Triggered {
		if t.Source == "fabrication" {
			out = append(out, t.RuleName)
		}
	}
	return out
}

func claimsToTriggers(claims []string) []Trigger {
	out := make([]Trigger, 0, len(claims))
	for _, c := range claims {
		out = append(out, Trigger{
			Source:   "fabrication",
			RuleName: c,
			Domain:   "fabrication",
		})
	}
	return out
}

func formatFlagged(claims []string) string {
	if len(claims) == 0 {
		return "[]"
	}
	out := make([]string, 0, len(claims))
	for _, c := range claims {
		out = append(out, fmt.Sprintf("%q", truncateForTrail(c)))
	}
	return "[" + strings.Join(out, "; ") + "]"
}

// truncateForTrail keeps trail strings bounded so the cycle-log
// JSONL doesn't grow huge per policy_decision event.
func truncateForTrail(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func buildFabricationSystemPrompt() string {
	return `You are a fabrication-detection classifier. The assistant produced a reply after running tools. Your job is to identify any specific technical claims in the reply that aren't supported by the tool log.

A "specific technical claim" names something concrete:
- An identifier (e.g. "the FooHandler", "result.Location", "MyStruct.SomeField")
- A file path or line number (e.g. "internal/foo/bar.go:42")
- A function signature or method
- A value, count, or fact attributed to a tool result

For each such claim:
- Was it produced by something in the tool log? (A read returned the file; a search hit the identifier; a memory lookup retrieved the value.)
- If yes → grounded.
- If no → flag it.

General statements ("the system seems healthy", "this looks straightforward") are not specific claims and are not flagged.

Verdict semantics:
- "allow" — every specific claim in the reply is grounded in the tool log.
- "escalate" — one or more confident, specific claims are NOT supported by the tool log. Include each in flagged_claims with a brief reason.

Be strict on confident-but-ungrounded code references — those are exactly the failure mode this gate exists to catch. The 2026-05-09 motivating incident: an analysis confidently stated "the bug is in result.Location == nil" when no tool had read or referenced any code containing that field. That's a clear escalate.

Be fair on hedged speculation: "the symptoms suggest a nil-deref somewhere in the response handling" is general, not specific.

If you flag a claim, be sure it's a real claim from the reply, quoted accurately. False positives waste operator time.`
}

func buildFabricationUserPrompt(output string, toolLog []ToolEvent) string {
	var b strings.Builder
	b.WriteString("Tool log from this cycle (in order):\n\n")
	for i, ev := range toolLog {
		fmt.Fprintf(&b, "--- tool[%d]: %s ---\n%s\n\n", i+1, ev.Name, truncateForToolLog(ev.Output))
	}
	b.WriteString("Assistant reply to verify:\n\n")
	b.WriteString(output)
	b.WriteString("\n\nReturn your verdict.")
	return b.String()
}

func buildFabricationToolInputSystemPrompt() string {
	return `You are a fabrication-detection classifier. The assistant is about to dispatch a tool with the given input arguments. Your job is to identify any specific factual claims in the input that AREN'T supported by the tool log of this cycle's prior tools.

The motivating failure mode: an LLM composes an email body, document draft, or library save containing URLs, prices, dates, or identifiers it MADE UP — content that no prior brave_web_search / library_get / memory_query returned. The result is fabricated content getting externalised to the operator's inbox or document store.

Examples of specific claims to check:
- URLs ("https://example.com/page") — must appear in a search/fetch result
- Prices, dates, numbers attributed to a source — must trace to a tool output
- Names of organisations, products, people presented as factual — must be grounded
- Quoted text presented as from an external source — must match a retrieved excerpt

Verdict semantics:
- "allow" — every specific factual claim in the tool input is supported by the tool log, OR the input is general enough that no grounding is required (a plain confirmation message, a request for clarification, etc.).
- "escalate" — one or more confident factual claims (especially URLs, prices, identifiers) are NOT supported by the tool log. The agent should run a tool to verify before retrying.

Be strict on URLs and prices — those are the exact failure mode this gate exists to catch. The 2026-05-09 motivating incident: comms agent composed an email with WiMo, MHZ Outdoor, and Radioworld URLs/prices that no brave_web_search call had returned in the cycle.

Be fair on conversational text. An email opening "Hi Seamus, here's what I found..." is fine on its own — the question is whether what follows is grounded.

If you flag a claim, quote it accurately. Operator-facing false positives that block legitimate sends are corrosive to trust.`
}

func buildFabricationToolInputUserPrompt(toolName, input string, toolLog []ToolEvent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The agent is about to dispatch tool %q with the input below. Verify that any specific factual claims in the input are grounded in the cycle's tool log.\n\n", toolName)
	b.WriteString("Tool log from this cycle (in order):\n\n")
	for i, ev := range toolLog {
		fmt.Fprintf(&b, "--- tool[%d]: %s ---\n%s\n\n", i+1, ev.Name, truncateForToolLog(ev.Output))
	}
	b.WriteString("Tool input to verify (the args being passed):\n\n")
	b.WriteString(input)
	b.WriteString("\n\nReturn your verdict.")
	return b.String()
}

// truncateForToolLog caps each tool output before sending to the
// scorer. Tool outputs can be long (full file reads, large search
// results); the scorer doesn't need every line, just enough
// context to recognise grounded references. 4 KB / event is a
// pragmatic cap — tunable later if false positives correlate
// with truncated evidence.
func truncateForToolLog(s string) string {
	const max = 4096
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n…[truncated for fabrication-scorer evidence pass]"
}
