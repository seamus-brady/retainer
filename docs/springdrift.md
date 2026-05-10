# Relationship to Springdrift

[Springdrift](https://github.com/seamus-brady/springdrift) is a Gleam reference implementation of a long-running cognitive-loop agent by the same author. Retainer is a Go port of selected ideas — intentionally smaller, intentionally not feature-parity. Read Springdrift if you want the canonical treatment; read Retainer if you want a working actor-model port that fits in one repo.

## What was ported

The load-bearing shape of Springdrift survives:

- **Actors-as-named-supervised-processes.** Cog, librarian, curator, observer, scheduler, archivist, captures, metalearning are each their own actor with their own goroutine and inbox, supervised under `internal/actor` (one-for-one restart, Permanent / Transient / Temporary strategies).
- **Cog cycle FSM.** The `Idle → EvaluatingPolicy → Thinking → UsingTools → Idle` flow matches Springdrift's `cognitive-loop.md`. The cog goroutine never blocks on I/O.
- **Librarian as single owner of memory state.** All reads and writes go through its inbox; JSONL on disk is durable, SQLite is a rebuildable index. This mirrors Springdrift's "Librarian-owns-ETS" pattern (the Erlang/Gleam ETS table replaced by an in-memory modernc SQLite).
- **Curator assembles, librarian queries.** The curator never owns indexes; it queries the librarian for memory slots and merges with static identity.
- **Facts schema.** `FactScope` (Persistent / Session / Ephemeral), `FactOperation` (Write / Clear / Superseded), confidence with read-time half-life decay. The schema and decay formula are 1:1 with `dprime/decay.gleam` and `facts/types.gleam`.
- **Identity rendering.** OMIT-IF rules including the `contains_zero_count` rule (Retainer's `internal/identity` mirrors Springdrift's `identity.gleam` exactly down to where the rule fires mid-string).
- **Date formatting.** `format_relative_date` arithmetic, including the rough approximation Springdrift uses for the year/month boundary cases.
- **CBR.** 6-signal retrieval over `(situation, action, outcome)` triples, same schema, same retrieval shape (`internal/cbr/types.go`).
- **DAG over cycles.** Same node types, same edge shape (`internal/dag`).
- **Policy gate skeleton.** D'/dprime gate placement (input/tool/output/post-exec), Source-aware Block→Escalate demotion, fail-open on canary errors with the Inconclusive flag.
- **Delegate tool shape.** `agent_<name>` tool naming and parameter shape (`task` required, `context` optional) is taken from `agent/types.gleam:agent_to_tool` verbatim so prompts remain portable.
- **Tolerant config.** Unknown TOML keys are accepted (Springdrift's behaviour) so config files survive version skew.

## What was deferred

Wired in the type system or noted in package docs, but not in production use today:

- **Policy Layer 2 LLM scorer.** The config field exists; no engine call site.
- **Policy Layer 3 meta observer.** Not implemented.
- **Worker-per-task agent concurrency.** Specialists process one task at a time. The cog can dispatch to *different* specialists in parallel; it cannot give the same specialist two tasks.
- **Truncation guards / inter-turn delays / depth caps / redact-secrets.** Springdrift has these; Retainer does not.
- **Structured findings extraction** (`ResearcherFindings` shapes etc.). V1 returns the assistant's final text; structured extraction would inspect the tool calls.
- **Agent teams / dispatch strategies** (ParallelMerge, Pipeline, DebateAndConsensus, LeadWithSpecialists). Out of scope for V1.
- **Sensorium XML block in the system prompt.** Partially landed (`internal/curator/sensorium.go`) but not the full Springdrift treatment — threading, scheduler-affect, and inter-agent context injection are deferred.
- **Virtual-memory scratchpad** (Letta-style working memory slots).
- **LLM retry layer.** The config block exists (`[retry]`) and the retry status enum is wired into the activity stream; the implementation is partial.
- **Ambient signal producers.** The buffer + drain loop is in place; producers beyond the existing handful (forecaster, observer-autonomous) are deferred.

The package doc-comments name the Springdrift module each port descends from and call out specifically which functions are deferred. Search the source for `Springdrift` to find them.

## What was dropped

Out of scope for Retainer entirely:

- **Normative calculus / FlourishingVerdict / Stoic axioms.** Retainer does not run an ethics-philosophy layer. The policy engine is purely deterministic + canary.
- **The full multi-agent team architecture.** Retainer ships two specialists (observer, scheduler). Springdrift's broader cast (researcher, comms, writer, etc.) is either folded into the cog directly (web tools live on the cog, not a researcher actor) or absent.
- **Mining and consolidation as agent tools.** They moved to `internal/metalearning/` as a daily background pool that lands durable markdown reports plus a patterns log the agent reads via `deep_search`. This is a deliberate divergence: making them cog-side tools made the agent self-loop on consolidation in unhelpful ways.

## Reading both codebases together

Most Retainer files open with a paragraph stating which Springdrift module they descend from, what the load-bearing parts are, and which features are deferred. The doc-comments are the canonical map between the two codebases. Examples:

- `internal/cog/cog.go` — references Springdrift's `cognitive-loop.md`.
- `internal/agent/agent.go` — explicit list of deferred features from `_impl_docs/ref/springdrift/src/agent/framework.gleam`.
- `internal/curator/curator.go` — explicit list of deferred slot queries from `narrative/curator.gleam`.
- `internal/observer/observer.go` — references `_impl_docs/ref/springdrift/src/agents/observer.gleam`.
- `internal/policy/policy.go` — explicit list of deferred and dropped layers.

`_impl_docs/` is gitignored and not shipped in this repo. To read Springdrift directly, clone <https://github.com/seamus-brady/springdrift> and cross-reference with the package doc-comments.

## Why a Go port at all

Go is operationally simpler than Gleam for a single-binary laptop tool: static binaries, mature standard library, broad ecosystem. The actor model survives the language change cleanly because Go's goroutines + tagged-channel inboxes match Erlang/Gleam processes closely enough that the structural ports read 1:1. The places where Go is awkward (no exhaustiveness checking on tagged unions, no built-in supervision) are filled by `internal/actor` and disciplined message-type interfaces.

This repository is one way to build a persistent agent. Read the source, fork it, do better.
