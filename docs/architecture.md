# Architecture

Retainer is a multi-actor Go program. Every load-bearing component runs as a named actor with its own goroutine, tagged-message inbox, and react/run loop, supervised under `internal/actor`. State is mutated only inside an actor's run loop; cross-actor communication is by message, never shared mutable state.

## Process layout

Two binaries:

- `cmd/retainer/` — the cog binary. Subcommands: `init`, `send`, `serve`, and (default) the Bubble Tea TUI. All subcommands construct the same actor tree via `bootstrap.go::bootstrap()`; what differs is what runs on top.
- `cmd/retainer-webui/` — the browser frontend. Connects to the cog binary's Unix socket (`internal/cogsock`) and surfaces a JSON/SSE API on top of it.

The TUI and the webui are interchangeable frontends over the same cog. The cog has no awareness of which is connected; both submit `UserInput` messages and receive `Reply`s.

## Bootstrap

`cmd/retainer/bootstrap.go::bootstrap()` returns a `world` struct containing the supervisor, all actors, and a cleanup closure. The order of construction matters because actors take dependencies on each other's inboxes:

1. Resolve workspace paths (`internal/paths`).
2. Load `.env` files (working dir, then `<workspace>/.env`).
3. Load and validate `config.toml` (`internal/config`).
4. Set up structured logging (`internal/logging` — JSON slog with daily rotation).
5. Acquire the workspace lock file (`internal/lockfile` — refuses second instance per workspace).
6. Initialise the embedder (`internal/embed` — Hugot/ONNX, models cached under `data/models/`).
7. Construct the librarian, archivist, curator, observer, scheduler, captures, metalearning, and finally the cog.
8. Wire each actor's tool registry (`cmd/retainer/main.go::buildCogTools`, `buildObserverTools`).
9. Register every actor with `actor.Supervisor` under a one-for-one strategy.

`world.cleanup` is the inverse. The supervisor terminates when its context is cancelled; actors that crash are restarted per their declared strategy.

## Restart strategies (`internal/actor`)

- **Permanent** — always restart on exit (clean or crash). Cog, librarian, curator, scheduler.
- **Transient** — restart only on crash; clean exit terminates. Specialist agents.
- **Temporary** — never restart. Used for one-shot work spawned under the supervisor.

`MaxRestartIntensity` caps restarts per time window; exceeding the cap fails the supervisor.

## The cog cycle

The cog (`internal/cog`) drives one cycle per inbound `UserInput`. The status FSM:

```
Idle
  └─→ EvaluatingPolicy(input)        # canary + deterministic gates on user text
        └─→ Thinking                  # LLM call(s)
              └─→ UsingTools          # react loop, bounded by max_tool_turns
                    ├─→ Thinking      # subsequent turns
                    └─→ Idle          # final assistant text + post-exec gate
```

Worker goroutines (LLM calls, canary probes) post results back through the cog's inbox. The cog goroutine never blocks on I/O — it dispatches a worker, parks the in-flight cycle state, and returns to `Idle` reception. When the worker delivers a result message, the cog resumes the parked cycle.

### Tool turn limits

- `defaultMaxToolTurns = 10` per cycle.
- `hardMaxToolTurns = 20` is the absolute ceiling, even after `request_more_turns` is invoked.
- Operator overrides via `[cog].max_tool_turns`.

### Watchdog

A per-status timeout (`gate.timeout_ms`, default 60s) catches stuck workers. On timeout the cog emits a `ReplyKindError` and returns to `Idle`; the supervisor does not restart the cog because the run loop itself is healthy.

### Activity stream

Every status transition, tool dispatch, and turn boundary publishes an `Activity` event on a lossy buffered channel. Subscribers (TUI, webui SSE) render live status without blocking the cog.

## Specialist agents (`internal/agent`, `internal/agents/`)

Each specialist is a supervised actor with the same shape as the cog: tagged inbox, bounded react loop, own tool registry. The cog reaches them only through `agent_<name>` delegate tools — there is no direct method call from cog → agent. The delegate tool name shape (`agent_observer`, `agent_scheduler`) is taken from Springdrift verbatim so prompts remain portable.

Currently registered:

- **Observer** — knowledge gateway. Hot-index reads (`recall_recent`, `inspect_cycle`, `get_fact`), CBR curation (`recall_cases`, `case_curate`), deep archive (`deep_search`, `find_connections`).
- **Scheduler** — cron-expression management. Each scheduled fire submits a fresh `UserInput` with `policy.SourceAutonomous`.

Adding a specialist: place its package under `internal/agents/<name>/`, build a `tools.DelegateToAgent` registration in `buildCogTools`, register the agent under the supervisor.

## Per-cycle data flow

For one user turn:

1. **Frontend** submits `UserInput{Text, Source, Reply}` to the cog inbox.
2. **Cog** transitions `Idle → EvaluatingPolicy`. Calls `policy.Engine.EvaluateInput` synchronously off the run loop via a worker.
3. On `Allow`: cog asks the **curator** for the system prompt (one IPC round-trip; curator queries the librarian for memory slots).
4. **Cog** assembles the message list (system + history + user turn, capped at `max_context_messages`) and dispatches an LLM call.
5. **LLM response** lands in the cog inbox.
   - If text-only: post-exec gate, archive cycle, send `Reply`.
   - If tool calls: dispatch each through the tool registry, accumulate `tool_result` blocks, loop back to **4** for the next turn.
6. **Cycle complete**: cog emits `archivist.CycleComplete`. The archivist writes the narrative summary; the librarian indexes it.
7. **CycleLog**: every step above appends one or more JSON events to `data/cycle-log/<date>.jsonl`. This file is the durable audit and the input to `deep_search` / `find_connections`.

## Persistence boundaries

- **JSONL is durable.** Every fact, narrative entry, CBR case, cycle event, and capture is appended to a daily JSONL file under `data/`.
- **SQLite is rebuildable.** Indexes for narrative + facts + cases live in an in-memory modernc SQLite, populated on startup by replaying the last `narrative_window_days` of JSONL.
- **No deletes.** Tombstones (`FactOperationClear`) supersede; the JSONL stays immutable. Operators delete by editing files, not by API.

See [memory.md](memory.md) for the storage details.

## Gates and policy

`internal/policy` runs the input gate (deterministic + canary), the output gate (deterministic + optional fabrication scorer), the tool gate, and the post-exec gate. The cog calls into the engine; agents skip the gate for low-risk tools. See [policy.md](policy.md).

## Cycle log as source of truth

`data/cycle-log/<date>.jsonl` is the only place where a complete record of "what happened this cycle" exists. Every other store (narrative, facts, cases) is derived from it. When debugging a misbehaving cycle, read the JSONL — the SQLite index can lie, the cycle log cannot.
