// Package archivist owns the post-cycle path between a cog cycle
// completing and that cycle becoming durable memory. Two outputs:
//
//   - Narrative entry → `librarian.RecordNarrative`
//   - CBR case (when content + outcome are present) → derived via
//     `cbr.DeriveProblem / DeriveSolution / DeriveOutcome / NewCase`,
//     embedded via the configured embedder, and recorded via
//     `librarian.RecordCase`.
//
// Architectural rationale (see `subprocess-specialist-pattern.md` and
// `feedback_agents_are_actors`): the archivist is *brain function* —
// memory formation. It runs in-process under the supervisor as an
// actor with an inbox. The cog sends `CycleComplete` fire-and-forget
// at end-of-cycle; the archivist does the rest asynchronously so the
// cog can move on to the next cycle.
//
// Mirrors Springdrift's `narrative/archivist.gleam` shape (single
// responsibility: post-cycle memory formation; supervised actor; never
// blocks the cog). The case-derivation path adds a heuristic
// extraction step today (intent / keywords from user input); a
// future LLM-driven enricher will land alongside without touching
// the cog.
package archivist

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/seamus-brady/retainer/internal/agent"
	"github.com/seamus-brady/retainer/internal/cbr"
	"github.com/seamus-brady/retainer/internal/embed"
	"github.com/seamus-brady/retainer/internal/librarian"
)

// inboxBufferSize bounds the cog's fire-and-forget channel so a sudden
// burst of cycle completions doesn't block the cog if the archivist is
// briefly slow. 64 is generous — at typical cycle rates (seconds per
// cycle), the archivist drains nearly instantly.
const inboxBufferSize = 64

// CycleComplete is the message the cog sends at end-of-cycle. The
// archivist turns this into a NarrativeEntry plus (when content +
// outcome are present) a CbrCase.
type CycleComplete struct {
	// CycleID identifies the completed cycle. Empty causes the
	// archivist to drop the message silently — no cycle-attribution
	// means no case-attribution either.
	CycleID string
	// Status is the narrative status (complete / blocked / error /
	// abandoned). Drives both the narrative entry's status field and
	// the case outcome's status (success vs failure).
	Status librarian.NarrativeStatus
	// Summary is a short prose description of what happened. Goes
	// straight into the narrative entry's Summary; also feeds the
	// case's outcome.assessment.
	Summary string
	// UserInput is the original user / scheduler input text. Drives
	// case problem derivation (intent + keywords). Empty means no
	// case is derived (no problem text → no useful case).
	UserInput string
	// ReplyText is the cycle's final reply text (or the abandonment
	// reason for non-success cycles). Drives the case's
	// solution.approach.
	ReplyText string
	// AgentsUsed lists specialist agents the cycle delegated to.
	// Stored on the case for retrieval; deduped by the cbr
	// derivation helpers.
	AgentsUsed []string
	// ToolsUsed lists tools the cog dispatched within this cycle.
	// Stored on the case for retrieval.
	ToolsUsed []string
	// RetrievedCaseIDs lists case IDs the agent saw via
	// `recall_cases` during this cycle. Drives the usage-stats
	// feedback loop: each case's RetrievalCount bumps; cycles that
	// succeeded also bump RetrievalSuccessCount. Empty when the
	// agent didn't recall any cases.
	RetrievedCaseIDs []string
	// ToolCalls is the (name, success) record for every tool the
	// cog dispatched in this cycle. Load-bearing: the Curator
	// grounds its outcome assessment in this list — without it
	// the curator can only judge from prose, which is what the
	// prior heuristic+Judge did and what produced the rubbish
	// cases the audit flagged.
	ToolCalls []ToolCallRecord
	// AgentCompletions is the per-dispatch record for every
	// `agent_<name>` call in this cycle. Carries the agent's
	// internal tool list + token usage, so the curator can
	// ground claim-vs-tool checks past the cog level. Without
	// this the curator only sees "agent_researcher: ok" and
	// can't tell whether the agent actually fired its own
	// tools or fabricated the result.
	AgentCompletions []agent.CompletionRecord
	// Timestamp is when the cycle completed. Defaults to time.Now()
	// at archivist receive-time when zero.
	Timestamp time.Time
}

// Librarian is the slice of *librarian.Librarian the archivist needs.
// Letting the archivist depend on this interface keeps tests
// independent of the librarian's SQLite-and-JSONL implementation.
type Librarian interface {
	RecordNarrative(entry librarian.NarrativeEntry)
	RecordCase(c cbr.Case)
	RecordCaseRetrieval(caseIDs []string, cycleSucceeded bool)
}

// Config wires the archivist's collaborators. Librarian is required;
// Embedder + Judge are optional — when nil, the archivist falls back
// gracefully (cases without vectors, heuristic outcome only).
type Config struct {
	// Librarian receives narrative + case writes. Required.
	Librarian Librarian
	// Embedder produces the case's problem-text embedding. Runs
	// inside the archivist goroutine (NOT the librarian goroutine)
	// so embedding latency doesn't serialise behind other librarian
	// reads. Nil disables embedding writes — case still gets stored
	// but with no vector.
	Embedder embed.Embedder
	// Curator turns the cycle's raw input/reply/tool-log into a
	// structured CurationResult populating intent classification,
	// domain, status, pitfalls, etc. Replaces the prior heuristic-
	// derive + Judge pipeline (see
	// `doc/specs/memory-and-logging-audit.md`). Nil defaults to
	// HeuristicCurator — the LLM-free fallback. Production wires
	// LLMCurator with the task model + provider.
	Curator Curator
	// Logger receives diagnostic logs. Defaults to slog.Default().
	Logger *slog.Logger
}

// Archivist is the running actor. Construct with New, supervise via
// Run, send `CycleComplete` messages via Record (fire-and-forget).
type Archivist struct {
	cfg    Config
	inbox  chan CycleComplete
	logger *slog.Logger
}

// New constructs an Archivist from a Config, applying defaults.
// Returns an error when Librarian is nil — without it the archivist
// has no destination.
func New(cfg Config) (*Archivist, error) {
	if cfg.Librarian == nil {
		return nil, errors.New("archivist: Librarian required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Curator == nil {
		// Default: heuristic-only. Operator opts in to the LLM
		// curator by passing &archivist.LLMCurator{...} in
		// bootstrap.go.
		cfg.Curator = HeuristicCurator{}
	}
	return &Archivist{
		cfg:    cfg,
		inbox:  make(chan CycleComplete, inboxBufferSize),
		logger: cfg.Logger,
	}, nil
}

// Run is the actor loop. Block until ctx is cancelled. Wrap with
// actor.Run under actor.Permanent so the post-cycle path keeps running
// across any individual record failure.
func (a *Archivist) Run(ctx context.Context) error {
	a.logger.Info("archivist started")
	defer a.logger.Info("archivist stopped")
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg := <-a.inbox:
			a.handle(msg)
		}
	}
}

// Record is the cog's entry point. Fire-and-forget — never blocks. If
// the inbox is full (cog is producing CycleCompletes faster than the
// archivist drains), the message is dropped with a warning so the cog
// stays unblocked. Same fire-and-forget semantics as Springdrift's
// `spawn_unlinked` archivist: failures shouldn't affect the user.
func (a *Archivist) Record(msg CycleComplete) {
	if msg.CycleID == "" {
		// Defensive: nothing to attribute to. Skip silently.
		return
	}
	select {
	case a.inbox <- msg:
	default:
		a.logger.Warn("archivist: inbox full; dropping CycleComplete",
			"cycle_id", msg.CycleID,
			"status", msg.Status,
		)
	}
}

// handle processes one CycleComplete. Three side effects, in order:
//
//  1. Curator runs ONCE per case-worthy cycle, producing
//     structured fields the archivist composes into BOTH a
//     rich NarrativeEntry AND a CbrCase from the same
//     CurationResult. This matches SD's
//     `narrative/archivist.gleam` shape: one curation call,
//     two structured records, kept consistent by construction.
//  2. Narrative entry recorded — rich form when curator ran;
//     legacy minimal form for skipped cycles (empty input,
//     abandoned).
//  3. Case recorded when the cycle was case-worthy.
//  4. Usage-stats feedback for any cases the cycle retrieved.
//
// The narrative is recorded for EVERY cycle (case-worthy or not)
// so the librarian's window has complete cycle coverage. Only
// the case is gated by shouldDeriveCase.
func (a *Archivist) handle(msg CycleComplete) {
	ts := msg.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}

	if !shouldDeriveCase(msg) {
		// Skipped cycle (empty input, abandoned). Write a
		// minimal narrative entry so the cycle still appears
		// in recall_recent / inspect_cycle output, but no
		// curator call + no case. Same shape the archivist
		// emitted before Phase 2B.
		a.cfg.Librarian.RecordNarrative(librarian.NarrativeEntry{
			CycleID:   msg.CycleID,
			Timestamp: ts,
			Status:    msg.Status,
			Summary:   msg.Summary,
		})
		if len(msg.RetrievedCaseIDs) > 0 {
			a.cfg.Librarian.RecordCaseRetrieval(msg.RetrievedCaseIDs, msg.Status == librarian.NarrativeStatusComplete)
		}
		return
	}

	// Case-worthy cycle: one curation call drives both records.
	in := CurationInput{
		UserInput:        msg.UserInput,
		ReplyText:        msg.ReplyText,
		AgentsUsed:       msg.AgentsUsed,
		ToolsUsed:        msg.ToolsUsed,
		ToolCalls:        msg.ToolCalls,
		AgentCompletions: msg.AgentCompletions,
		CycleStatus:      string(msg.Status),
		ParentCycleID:    msg.CycleID,
	}
	result, err := a.cfg.Curator.Curate(context.Background(), in)
	if err != nil {
		a.logger.Warn("archivist: curator failed; falling back to heuristic",
			"cycle_id", msg.CycleID, "err", err,
		)
		// HeuristicCurator never errors — guaranteed result.
		result, _ = HeuristicCurator{}.Curate(context.Background(), in)
	}

	// Compose + record narrative entry from the curation result.
	entry := buildNarrativeEntry(msg, in, result, ts)
	a.cfg.Librarian.RecordNarrative(entry)

	// Compose + record CbrCase from the same curation result.
	c := a.composeCase(msg, in, result, ts)
	a.cfg.Librarian.RecordCase(c)

	// Usage-stats feedback closes the retrieval feedback loop.
	if len(msg.RetrievedCaseIDs) > 0 {
		a.cfg.Librarian.RecordCaseRetrieval(msg.RetrievedCaseIDs, msg.Status == librarian.NarrativeStatusComplete)
	}
}

// shouldDeriveCase is the gate: only derive a case when the cycle has
// content the agent can learn from. Empty user input means the
// archivist has no problem-text to derive a Case.Problem from;
// abandoned cycles never reached an outcome.
func shouldDeriveCase(msg CycleComplete) bool {
	if msg.UserInput == "" {
		return false
	}
	if msg.Status == librarian.NarrativeStatusAbandoned {
		return false
	}
	return true
}

// composeCase builds a CbrCase from the curation result + cycle
// context. Mirrors what deriveCase did before Phase 2B, minus
// the Curate call (now done once at the handle level so the
// narrative entry shares the same result).
func (a *Archivist) composeCase(msg CycleComplete, in CurationInput, result CurationResult, now time.Time) cbr.Case {
	problem := cbr.Problem{
		UserInput:       truncate(msg.UserInput, 600),
		Intent:          result.IntentDescription,
		IntentClass:     result.IntentClassification,
		Domain:          result.Domain,
		Entities:        result.Entities,
		Keywords:        result.Keywords,
		QueryComplexity: result.QueryComplexity,
	}
	solution := cbr.Solution{
		Approach:   result.Approach,
		AgentsUsed: dedupe(msg.AgentsUsed),
		ToolsUsed:  dedupe(msg.ToolsUsed),
		Steps:      result.Steps,
	}
	outcome := cbr.Outcome{
		Status:     result.Status,
		Confidence: result.Confidence,
		Assessment: result.Assessment,
		Pitfalls:   result.Pitfalls,
	}

	var (
		vec        []float32
		embedderID string
	)
	if a.cfg.Embedder != nil {
		text := embeddingText(problem)
		v, embedErr := a.cfg.Embedder.Embed(context.Background(), text)
		if embedErr != nil {
			a.logger.Warn("archivist: embedding failed; case stored without vector",
				"cycle_id", msg.CycleID, "err", embedErr)
		} else {
			vec = v
			embedderID = a.cfg.Embedder.ID()
		}
	}
	return cbr.NewCase(msg.CycleID, problem, solution, outcome, vec, embedderID, now)
}

// buildNarrativeEntry composes a rich NarrativeEntry from the
// same CurationResult that drives the case. Maps controlled-
// vocabulary classification, structured outcome, agent
// completions → DelegationChain, and per-cycle metrics. The
// entry's legacy top-level fields (Status, Summary, Domain,
// Keywords) are mirrored by NarrativeEntry.MirrorLegacyFields()
// at write time so the SQLite index keeps populating without
// changes.
//
// SD parity: ports `narrative/types.gleam:NarrativeEntry`
// minus subsystem-deferred fields (Thread populated by
// threading; Decisions populated by curator; Sources populated
// when the curator extracts them — left empty for now since
// the curator schema doesn't yet output Decisions / Sources
// directly. Those land in a follow-up that extends the
// curation schema.)
func buildNarrativeEntry(msg CycleComplete, in CurationInput, result CurationResult, ts time.Time) librarian.NarrativeEntry {
	entry := librarian.NarrativeEntry{
		CycleID:       msg.CycleID,
		ParentCycleID: "", // root cycle for cog cycles; agent cycles set their own parent in the agent's narrative
		Timestamp:     ts,
		EntryType:     librarian.EntryTypeNarrative,
		Summary:       msg.Summary,
		Intent: librarian.Intent{
			Classification: librarian.IntentClassification(result.IntentClassification),
			Description:    result.IntentDescription,
			Domain:         result.Domain,
		},
		Outcome: librarian.Outcome{
			Status:     librarian.OutcomeStatus(result.Status),
			Confidence: result.Confidence,
			Assessment: result.Assessment,
		},
		DelegationChain: delegationChainFromCompletions(msg.AgentCompletions),
		Topics:          result.Keywords, // No separate topics today; reuse keywords as a proxy
		Keywords:        result.Keywords,
		Entities: librarian.Entities{
			// Curator's Entities are a flat string list today.
			// Routing them to Locations is a coarse default —
			// proper named-entity recognition lands when the
			// curator schema gains structured Entities.
			Locations: result.Entities,
		},
		Metrics: librarian.Metrics{
			InputTokens:      tokensFromCompletions(msg.AgentCompletions, true),
			OutputTokens:     tokensFromCompletions(msg.AgentCompletions, false),
			ToolCalls:        len(msg.ToolCalls),
			AgentDelegations: len(msg.AgentCompletions),
		},
	}
	// Populate the legacy top-level fields from the structured
	// equivalents so the SQLite index keeps working without
	// changes. The librarian also defensively mirrors on
	// write — calling it here means tests + readers see a
	// complete entry without depending on the store path.
	entry.MirrorLegacyFields()
	return entry
}

// delegationChainFromCompletions converts agent completion
// records into NarrativeEntry's DelegationStep shape.
func delegationChainFromCompletions(comps []agent.CompletionRecord) []librarian.DelegationStep {
	if len(comps) == 0 {
		return nil
	}
	out := make([]librarian.DelegationStep, 0, len(comps))
	for _, c := range comps {
		step := librarian.DelegationStep{
			Agent:        c.AgentName,
			AgentCycleID: c.AgentCycleID,
			Instruction:  c.Instruction,
			OutcomeText:  c.OutcomeText,
			ToolsUsed:    c.ToolsUsed,
			InputTokens:  c.InputTokens,
			OutputTokens: c.OutputTokens,
			DurationMs:   c.Duration.Milliseconds(),
		}
		if c.Success {
			step.Contribution = "succeeded"
		} else if c.ErrorMessage != "" {
			step.Contribution = "failed: " + truncate(c.ErrorMessage, 200)
		} else {
			step.Contribution = "failed"
		}
		out = append(out, step)
	}
	return out
}

// tokensFromCompletions sums input or output tokens across
// agent completions. The cog's own LLM tokens land via
// metrics later (Phase 6 telemetry); this is just the agent-
// dispatched chunk for now.
func tokensFromCompletions(comps []agent.CompletionRecord, input bool) int {
	total := 0
	for _, c := range comps {
		if input {
			total += c.InputTokens
		} else {
			total += c.OutputTokens
		}
	}
	return total
}

// embeddingText is what we hand to the embedder for each case. SD
// uses problem text (intent + domain + keywords joined with spaces)
// — we mirror that. Long enough to capture the semantic content,
// short enough to stay under MiniLM's 256-token context window for
// pathological inputs.
func embeddingText(p cbr.Problem) string {
	parts := []string{p.Intent}
	if p.Domain != "" {
		parts = append(parts, p.Domain)
	}
	parts = append(parts, p.Keywords...)
	return joinNonEmpty(parts, " ")
}

// joinNonEmpty filters empty strings before joining so we don't get
// leading / trailing / doubled separators when fields are absent.
func joinNonEmpty(in []string, sep string) string {
	out := ""
	for _, s := range in {
		if s == "" {
			continue
		}
		if out == "" {
			out = s
			continue
		}
		out += sep + s
	}
	return out
}
