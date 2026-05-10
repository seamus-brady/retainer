// Package observer provides the Observer specialist agent —
// the knowledge gateway for the cog. Selective port from
// `_impl_docs/ref/springdrift/src/agents/observer.gleam` plus
// the deep-archive surface that previously lived on the
// remembrancer agent (folded 2026-05-07 — system-layer pure
// functions remain in `internal/remembrancer/`; only the
// agent's tool wrappers moved here).
//
// V1 surface — recent (hot index):
//   - inspect_cycle (DAG status + narrative summary for one cycle)
//   - recall_recent (most-recent narrative entries)
//   - get_fact (read a fact by key with decayed confidence)
//
// V1 surface — CBR curation:
//   - recall_cases (CBR retrieval)
//   - suppress_case / unsuppress_case / boost_case / annotate_case / correct_case
//
// V1 surface — deep archive (folded from remembrancer):
//   - deep_search (free-text across narrative + facts JSONL)
//   - find_connections (cross-store topic search)
//   - mine_patterns (cluster CBR cases for recurring shapes)
//   - consolidate_memory (LLM-driven summary across a date slice)
//   - write_consolidation_report (persist a digest as markdown)
//
// Deferred (each tied to a prerequisite subsystem):
//   - recall_search (needs librarian narrative keyword search)
//   - recall_threads (needs threading subsystem)
//   - query_tool_activity (needs DAG aggregation)
//   - review_recent / detect_patterns (need DAG aggregation + narrative iteration)
//   - memory_trace_fact (needs full fact history surface)
//   - report_false_positive (needs meta observer)
//   - review_learning_goals (needs learning goals subsystem)
//   - reflect / introspect (need DAG aggregation)
//
// Per `project_observer_eventual_shape`, the Observer is the canonical
// example of a specialist agent that introspects the system itself.
// The supervisor wires it with Transient restart (per Springdrift) so
// crashes during a single dispatch don't restart it on every cycle —
// it only restarts on abnormal exit.
package observer

import (
	"log/slog"

	"github.com/seamus-brady/retainer/internal/agent"
	"github.com/seamus-brady/retainer/internal/llm"
)

// Name is the machine-readable identifier (forms the delegate tool
// name `agent_observer`).
const Name = "observer"

// HumanName is the display name surfaced in logs + future sensorium.
const HumanName = "Observer"

// Description is the one-liner the cog's LLM sees on the
// `agent_observer` delegate tool. Covers the full knowledge
// surface: recent (hot index), CBR curation, and deep-archive
// reads + synthesis (folded from remembrancer).
const Description = "Inspect past cycles, recall recent activity, look up stored facts, retrieve + curate CBR cases, " +
	"and read the full JSONL archive (deep search, cross-store connections, pattern mining, consolidation reports). " +
	"Use for any memory-shaped question — recent or deep. Returns formatted summaries."

// MaxTurns covers both diagnostic drill-downs (cheap; 2-3
// turns) and synthesis-shaped work like consolidation runs
// (often 4-6 turns of search → narrow → synthesise). Eight
// gives the observer headroom for the longer flow that came in
// with the remembrancer fold.
const MaxTurns = 8

// MaxTokens raised from the diagnostic-agent default to
// accommodate consolidation digests + synthesis output. Match
// the remembrancer's previous budget so existing consolidation
// flows fit.
const MaxTokens = 4096

// MaxConsecutiveErrors matches Springdrift exactly.
const MaxConsecutiveErrors = 3

// systemPrompt frames the agent's role + tool selection. Pared down
// from `observer.gleam` to cover only the tools we have. Reintroduce
// the deferred sections as their dependent subsystems land.
const systemPrompt = `You are the Observer agent — the knowledge gateway for the cog. Your role is to introspect the agent system AND reach into the full archive when needed: recent cycles, narrative history, stored facts, CBR cases, and deep historical material the librarian's hot index has dropped. You do NOT take actions on the outside world — you only look inward.

## Tool selection strategy

### Recent activity (librarian's hot index)
- **inspect_cycle**: DAG status + narrative summary + duration + error for a specific cycle. Accepts full cycle ID or 8-char prefix.
- **recall_recent**: List the most-recent narrative entries. Default limit 5; ask for more (up to 50) if the answer needs more context.
- **get_fact**: Read the current value of a stored fact by key. Returns 'not found' when never written or cleared.

### CBR cases (curation)
- **recall_cases**: Retrieve cases similar to a query (intent + domain + keywords + entities). Top-K with usage stats.
- **suppress_case / unsuppress_case**: Operator-curate a case off / back into retrieval.
- **boost_case**: Adjust a case's outcome confidence by ±delta.
- **annotate_case**: Append a pitfall to a case's outcome.
- **correct_case**: Replace a case's problem / solution / outcome.

### Deep archive (full JSONL reads)
- **deep_search**: Free-text across the FULL narrative + facts JSONL — bypasses the hot index. First reach when "I think we did this before but the recent index doesn't show it."
- **find_connections**: Cross-reference a topic across all stores with hit counts. Good for widening from a single match.
- **mine_patterns**: Cluster CBR cases for recurring problem shapes. Useful when "have I solved something like this before?".
- **consolidate_memory**: Aggregate counts + sample excerpts across a date range. Use as the gather step before writing a report.
- **write_consolidation_report**: Persist a markdown digest under data/reports/. Echoes the file path.

## Working method

1. **Understand the current work** before diving. Re-read the instruction.
2. **Pick the right depth**:
   - Recent question (today, this session, the last few cycles) → recall_recent / inspect_cycle / get_fact
   - "Have we ever seen / done X" → deep_search across a wide window
   - Cross-store topic mapping → find_connections
   - Periodic synthesis or report → consolidate_memory then (optionally) write_consolidation_report
   - Pattern mining over CBR → mine_patterns
3. **Search broadly first when going deep** — start wide with deep_search, then tighten with find_connections.
4. **Qualify confidence on old material.** Decayed facts get flagged as decayed; old cases may reflect outdated approaches.
5. **Synthesise in your own voice** when consolidating — don't parrot raw excerpts back.
6. **Write the report when synthesis is the deliverable**, not for one-line answers.

## Output format
Concise summary tailored to the question. Include cycle IDs (short prefix), key facts, and any errors or anomalies. Preserve verbatim cycle metadata when it's useful for the operator. Avoid paraphrasing tool output unless synthesis was asked for. When you wrote a report, include the file path.

## Self-check before you start
If the question doesn't map to any of your tools (e.g. "search the web", "send email"), say so in one short sentence. Do not invent data.

## Your harness is documented in skills
Before each dispatch, consult ` + "`agents-observer`" + ` for your procedure. Mechanics live in ` + "`harness-*`" + ` skills (turns, tools, memory, policy, dispatch, errors). Browse via discover_skill, consult via read_skill('agents-observer') or read_skill('harness-tools').`

// New constructs an Observer agent. The dispatcher must include at
// minimum the inspect_cycle / recall_recent / get_fact tools wired
// against the running observer.Observer service.
// New constructs an Observer agent. Pass `agent.Telemetry{}` zero
// value to run silently (legacy / test path); production wires the
// workspace's cycle-log + DAG + instance ID so the observer's
// react loop stamps the same instance_id as the cog.
func New(provider llm.Provider, model string, tools agent.ToolDispatcher, telemetry agent.Telemetry, logger *slog.Logger) (*agent.Agent, error) {
	spec := agent.Spec{
		Name:                 Name,
		HumanName:            HumanName,
		Description:          Description,
		SystemPrompt:         systemPrompt,
		Provider:             provider,
		Model:                model,
		MaxTokens:            MaxTokens,
		MaxTurns:             MaxTurns,
		MaxConsecutiveErrors: MaxConsecutiveErrors,
		Tools:                tools,
	}
	telemetry.ApplyTo(&spec)
	return agent.New(spec, logger)
}
