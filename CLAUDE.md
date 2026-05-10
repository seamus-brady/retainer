# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Common commands

All scripts are bash, run from the repo root, and chdir there themselves so you can invoke from anywhere.

- `scripts/build` — `go build -o bin/retainer ./cmd/retainer` (the TUI / `serve` / `send` / `init` binary).
- `scripts/run [args...]` — `go run ./cmd/retainer "$@"`.
- `scripts/test` — `go test ./...` (unit tests; CI runs with `-race`).
- `scripts/vet` — `go vet ./...`.
- `scripts/run-example` — boots against `internal/example/config.example.toml` in `$TMPDIR/retainer-example`.
- `scripts/integration-tests/run-all.sh` — full end-to-end suite. Builds both binaries once into temp paths, runs ~12 bash scenarios (`basic`, `memory`, `cbr`, `multi-turn`, `webui`, `captures`, …) each driving the cog through `retainer send` against the **mock provider** and asserting on JSONL artifacts. Set `WK_TEST_VERBOSE=1` to surface stderr. Per-scenario: `bash scripts/integration-tests/<name>.sh`.
- Single Go test: `go test ./internal/<pkg>/ -run TestName -v` (or `go test -race -run ^TestX$ ./internal/cog/`).
- Docker: `scripts/docker-run [-d] [--rebuild] [--workspace PATH]` — wraps `docker build` + `docker run`, bind-mounts `~/retainer` as the workspace, picks up `.env` for `ANTHROPIC_API_KEY`.
- Webui locally without Docker: `scripts/retainer-with-webui --workspace ~/retainer` — starts `retainer serve` in the background, waits for the cog Unix socket, then execs `retainer-webui`.

Go toolchain: **1.26+** (pinned by `go.mod`; CI uses `go-version-file: go.mod`). The Dockerfile builds with `CGO_ENABLED=0` so the resulting binaries drop into Alpine clean.

## Provider selection

- `ANTHROPIC_API_KEY` set → real Anthropic provider (default model `claude-haiku-4-5-20251001`; switch to `claude-opus-4-7` via `task_model` in `config.toml`).
- Unset, or `RETAINER_PROVIDER=mock` → mock provider that echoes input. **Integration tests force this** (`scripts/integration-tests/lib.sh` unsets the API keys and exports `RETAINER_PROVIDER=mock`). Bootstrap can also load a scripted mock (deterministic tool-use sequences) via the `mockScriptPath` argument.
- Config overrides via env: `RETAINER_<SECTION>_<KEY>` (e.g. `RETAINER_AGENT_NAME=Echo`, `RETAINER_WORKSPACE=...`).

## Workspace layout (runtime)

`retainer init <path>` seeds a workspace. Both binaries operate against this directory; nothing in the repo is written to at runtime.

```
<workspace>/
├── config/
│   ├── config.toml          # `[agent]`, `[cog]`, provider, models, limits
│   ├── policy.json          # deterministic policy rules (seeded from internal/policy/defaults.json)
│   ├── identity/persona.md  # editable persona — curator renders it into the system prompt
│   └── skills/<id>/SKILL.md # discoverable skills (seeded from internal/skills)
└── data/
    ├── cog.sock             # cogsock IPC (NOT used in Docker; see below)
    ├── cycle-log/*.jsonl    # one JSON event per line per day — durable audit
    ├── narrative/*.jsonl    # one-line summary per cycle
    ├── facts/*.jsonl        # operator-saved key/value facts
    ├── identity.json        # stable agent UUID
    └── models/              # ONNX embedding models downloaded on first run
```

**Docker socket gotcha**: macOS virtiofs bind mounts can't host Unix sockets. `scripts/docker-entrypoint.sh` pins `RETAINER_COG_SOCKET=/tmp/retainer-cog.sock` (container-local, not in the bind-mounted workspace). Both `retainer` and `retainer-webui` honour `RETAINER_COG_SOCKET`.

## Architecture

Retainer is a multi-actor Go program. Every load-bearing component is a **named actor**: its own goroutine + tagged-message inbox + react/run loop, supervised under `internal/actor` (one-for-one restart). The rule "agents are named actors" is enforced — system-prompt assembly, memory, scheduling, etc. are not methods tucked into a god object.

### Lifecycle

1. `cmd/retainer/main.go` dispatches subcommands: `init` / `send` / `serve` / (default) TUI.
2. `cmd/retainer/bootstrap.go::bootstrap()` constructs a `world`: resolves paths, loads config, sets up `slog` JSON logging, wires every subsystem, and registers them with `actor.Supervisor`. Both the TUI (`runTUI`) and headless `serve` use the same bootstrap; the difference is what runs on top (Bubble Tea program vs signal-wait).
3. `serve` mode binds the cog Unix socket (`internal/cogsock`) so `retainer-webui` (a separate binary in `cmd/retainer-webui`) can drive the cog over IPC.

### The cog (`internal/cog`)

The central conversation actor. Each cycle: `Idle → EvaluatingPolicy → Thinking → UsingTools (react loop) → Idle`. State is mutated **only** inside its run loop — workers (LLM calls, canary probes) post results back via the inbox. The cog never blocks on I/O. Tool turns are bounded by `defaultMaxToolTurns=10` per cycle, hard-capped at `hardMaxToolTurns=20` even with the in-cycle `request_more_turns` tool.

### Specialist agents (`internal/agent`, `internal/agents/`)

Each specialist (currently `observer`, `scheduler`) is a supervised actor with its own bounded react loop and tool registry. The cog reaches them through `agent_<name>` delegate tools, never by calling methods directly. `cmd/retainer/main.go::buildCogTools` and `buildObserverTools` show how the registries are wired.

### Memory (`internal/librarian`)

JSONL on disk is the **durable** record; SQLite (modernc, in-memory) is a **rebuildable** index that's replayed from JSONL on startup. Every Record* op writes JSONL first, then INSERTs into SQLite — if the index write fails, replay heals it on the next boot. The librarian goroutine is the **single owner** of all index state; everything else queries through its inbox. Stores: `narrative` (per-cycle one-liners), `facts` (key/value with scope + half-life decay), `cases` (CBR retrieval).

### Curator (`internal/curator`)

Assembles the system prompt every cycle: merges static identity (persona, preamble templates) with memory-derived slots (recent narrative, persistent facts, recalled CBR cases) and per-cycle context (cycle ID, input source, queue depth). It **never owns indexes** — it only queries the librarian.

### Policy (`internal/policy`)

D'-style safety gates on input / output / tool / post-exec. Layer 1 is deterministic (regex from `policy.json`); the input gate also fires **canary probes** — two parallel LLM calls that detect prompt-injection regex misses. Layer 2 (LLM scorer) and Layer 3 (meta observer) are deferred. The cog calls into the engine; agents skip the gate for non-high-risk tools.

### Other load-bearing pieces

- `internal/agent` — substrate for specialists (Spec, Task/Outcome, react loop).
- `internal/captures` — tracks promises the agent makes to the user.
- `internal/cbr` — case-based reasoning (6-signal retrieval over `librarian` cases).
- `internal/cyclelog` — append-only JSONL events; the audit trail every other subsystem reads.
- `internal/dag` — DAG over cycles for cross-cycle linkage.
- `internal/embed` — ONNX embedding via `knights-analytics/hugot`; models cached under `data/models/`.
- `internal/metalearning` — daily background audits (folded mining/consolidation that used to live in remembrancer).
- `internal/remembrancer` — system-layer deep archive (Search, ReadCases, ReadNarrative). Observer's `deep_search` / `find_connections` tools wrap this.
- `internal/scheduler` — `robfig/cron`-backed background firings; each cycle is logged.
- `internal/skills` — discoverable `SKILL.md` files; `read_skill` is the agent-facing surface.
- `internal/tools` — tool implementations (memory, web, delegate, read_skill, request_more_turns, observer/cbr surfaces).

### Reference lineage

This Go codebase is a smaller port of the author's Gleam reference [Springdrift](https://github.com/seamus-brady/springdrift). Many package doc-comments cite the Springdrift module they descend from and call out what's deferred — read those when changing semantics. `_impl_docs/` (gitignored) holds the reference; don't rely on it being present.

## Conventions

- **Per-file lead comment.** Most files open with a paragraph explaining what the package/file does and why. Match that style on new files.
- **Tests live alongside code** (`foo.go` + `foo_test.go`). No separate `tests/` tree.
- **Actor discipline.** If you're adding a long-running subsystem, build it as an actor with an inbox under `actor.Supervisor`, not as a goroutine called from `bootstrap`. Mutate state only inside the run loop; post results back via the inbox.
- **Single owner of index state.** Don't add a parallel cache or query path to memory — go through the librarian.
- **JSONL first, SQLite second.** Durability lives on disk; the index is throwaway. Don't invert that.
