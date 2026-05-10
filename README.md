# Retainer

A persistent personal AI agent that runs on your laptop. Memory lives on disk in a folder you own, every cycle is appended to a JSONL audit log, and the persona is an editable markdown file rendered into the system prompt each cycle. The default LLM is Anthropic's Claude (bring your own API key); a mock provider ships for local development and integration tests.

Retainer is a Go port of selected ideas from **[Springdrift](https://github.com/seamus-brady/springdrift)** — a Gleam reference implementation of a long-running cognitive-loop agent by the same author. It is intentionally smaller and not feature-parity: many of Springdrift's subsystems are noted in the source as deferred or out of scope. Treat this repository as a working demonstration of the actor-model approach Springdrift proposes, not a canonical implementation. See [docs/springdrift.md](docs/springdrift.md) for what was ported and what was left.

---

## Contents

- [Quickstart](#quickstart)
- [Capabilities](#capabilities)
- [Workspace layout](#workspace-layout)
- [Configuration](#configuration)
- [Running without Docker](#running-without-docker)
- [Architecture](#architecture)
- [Documentation](#documentation)
- [Scope](#scope)
- [License](#license)

---

## Quickstart

Requirements:
- [Docker](https://www.docker.com/products/docker-desktop/)
- An Anthropic API key from <https://console.anthropic.com/>

```sh
git clone https://github.com/seamus-brady/retainer.git
cd retainer
echo "ANTHROPIC_API_KEY=sk-ant-..." > .env
scripts/docker-run -d
```

Open <http://localhost:7878/> and start chatting. Stop with `docker stop retainer`.

The bind-mounted workspace at `~/retainer/` persists across runs. Without `ANTHROPIC_API_KEY`, the binary still boots — provider falls back to the mock (echoes input), useful for verifying the harness without spending tokens.

If you don't want Docker, see **[Running without Docker](#running-without-docker)**.

---

## Capabilities

The cog (central conversation actor) exposes the following tools to the model. Specialist agents (observer, scheduler) extend this surface via `agent_<name>` delegate tools.

- **Persistent key/value memory** — `memory_write`, `memory_read`, `memory_clear_key`, `memory_query_facts`. Backed by JSONL on disk, indexed in SQLite. Read-time half-life decay handles staleness.
- **Web search and fetch** — `web_search` (DuckDuckGo HTML scrape), `fetch_url`. No API keys, no third-party SaaS.
- **Cycle introspection** — observer's `inspect_cycle`, `recall_recent`, `get_fact`, `deep_search`, `find_connections`. Walks the JSONL cycle log.
- **CBR retrieval and curation** — observer's `recall_cases`, `case_curate` (suppress / unsuppress / boost / annotate / correct).
- **Scheduled cycles** — scheduler's cron-expression tools. Each fire is a normal logged cycle.
- **Skill discovery** — `read_skill` reads markdown `SKILL.md` files from `<workspace>/config/skills/<id>/`. Seeded skills ship in `internal/skills/defaults/`.
- **Self-extending tool budget** — `request_more_turns` raises the per-cycle react-loop ceiling up to a hard cap of 20.
- **Editable persona** — `<workspace>/config/identity/persona.md` is rendered into the system prompt every cycle by the curator.

---

## Workspace layout

`retainer init <path>` (or first Docker run) seeds the workspace. The binaries operate against this directory; nothing in the source tree is written to at runtime.

```
~/retainer/
├── config/
│   ├── config.toml              # provider, models, gates, memory windows
│   ├── policy.json              # deterministic policy rules
│   ├── identity/
│   │   ├── persona.md           # editable agent personality
│   │   └── session_preamble.md  # per-cycle preamble template
│   └── skills/<id>/SKILL.md     # discoverable skills (seeded)
└── data/
    ├── cycle-log/*.jsonl        # one event per line per day — durable audit
    ├── narrative/*.jsonl        # one-line summary per cycle
    ├── facts/*.jsonl            # operator-saved facts
    ├── identity.json            # stable agent UUID
    └── models/                  # ONNX embedding models (downloaded on first run)
```

The cog Unix socket (`data/cog.sock` natively, `/tmp/retainer-cog.sock` inside Docker — virtiofs cannot host sockets) is the IPC surface `retainer-webui` connects to. See [docs/configuration.md](docs/configuration.md#docker-socket) for details.

---

## Configuration

Edit `~/retainer/config/config.toml`:

```toml
provider        = "anthropic"
task_model      = "claude-haiku-4-5-20251001"   # Default — cheap and fast
# task_model    = "claude-opus-4-7"             # Slower, smarter
reasoning_model = "claude-opus-4-7"             # Reserved for the model split
max_tokens      = 2048

[agent]
name = "Nemo"

[cog]
# max_tool_turns       = 10    # React-loop ceiling per cycle (hard cap 20)
# max_context_messages = 200   # 0 = unbounded; smaller = tighter token budget
```

Every field can be overridden by an env var with the convention `RETAINER_<SECTION>_<KEY>` (or `RETAINER_<KEY>` for top-level fields). Precedence: env > TOML > default.

```sh
RETAINER_AGENT_NAME=Echo scripts/docker-run -d
RETAINER_PROVIDER=mock retainer       # Skip Anthropic entirely
```

A `.env` in the working directory or in `<workspace>/.env` is loaded automatically (workspace `.env` layered last; shell exports take precedence).

Mistral is wired as a non-default alternative — see the comments in `internal/example/config.example.toml`.

The full configuration reference, including environment variables and the override convention, is in [docs/configuration.md](docs/configuration.md).

---

## Running without Docker

```sh
go install github.com/seamus-brady/retainer/cmd/retainer@latest
go install github.com/seamus-brady/retainer/cmd/retainer-webui@latest

retainer init ~/retainer        # First time only
retainer                          # TUI (terminal UI)
```

For the browser UI, the simplest path is the launcher script in this repo, which starts the cog, polls for the Unix socket, then execs `retainer-webui`:

```sh
scripts/retainer-with-webui --workspace ~/retainer
```

Or run them in two terminals manually:

```sh
retainer serve --workspace ~/retainer
retainer-webui --workspace ~/retainer
```

Then open <http://localhost:7878/>.

Requires Go 1.26 or later (pinned in `go.mod`). macOS and Linux are tested; Windows is not.

---

## Architecture

Retainer is a multi-actor Go program. Every load-bearing component is a **named actor**: its own goroutine, tagged-message inbox, and react/run loop, supervised under `internal/actor` (one-for-one restart). The "agents are named actors" rule is enforced — system-prompt assembly, memory, scheduling, and policy each have a named actor rather than methods on a god object.

Load-bearing packages:

- `internal/cog/` — the central conversation actor. Each cycle: `Idle → EvaluatingPolicy → Thinking → UsingTools → Idle`.
- `internal/agent/` — substrate for specialist sub-agents (Spec, Task/Outcome, bounded react loop). The cog reaches specialists via `agent_<name>` delegate tools.
- `internal/agents/observer/`, `internal/agents/scheduler/` — the registered specialists.
- `internal/librarian/` — single-owner memory store. JSONL on disk is durable; SQLite (modernc, in-memory) is a rebuildable index replayed from JSONL at startup.
- `internal/curator/` — assembles the system prompt each cycle. Merges static identity (persona, preamble) with memory-derived slots queried from the librarian.
- `internal/policy/` — D'-style safety gates on input / output / tool / post-exec. Layer 1 deterministic; canary probes detect prompt-injection on input.
- `internal/captures/` — tracks promises the agent makes to the user.
- `internal/cbr/` — case-based reasoning, 6-signal retrieval over librarian cases.
- `internal/remembrancer/` — system-layer deep archive over `cycle-log/` and `narrative/`. Backs observer's `deep_search` and `find_connections` tools.
- `internal/metalearning/` — daily background audits (mining and consolidation).
- `internal/scheduler/` — `robfig/cron`-backed background firings.
- `internal/actor/` — supervisor + restart strategies (Permanent / Transient / Temporary).

Most files open with a paragraph explaining what the package does and why, often citing the Springdrift module they descend from. Tests live alongside the code they test. About 30K lines of Go.

For a deeper walkthrough of the lifecycle, message flow, and per-package responsibilities, see [docs/architecture.md](docs/architecture.md).

---

## Documentation

Technical docs live under [docs/](docs/):

- [docs/architecture.md](docs/architecture.md) — actor topology, lifecycle, per-cycle data flow.
- [docs/memory.md](docs/memory.md) — JSONL + SQLite storage model, narrative / facts / cases stores, decay.
- [docs/policy.md](docs/policy.md) — gate placement, canary probes, deferred layers.
- [docs/configuration.md](docs/configuration.md) — full `config.toml` reference, environment overrides, workspace resolution.
- [docs/scripts.md](docs/scripts.md) — every script in `scripts/`: what it does, when to use it.
- [docs/springdrift.md](docs/springdrift.md) — relationship to Springdrift: what was ported, what was deferred, what was dropped.

---

## Scope

What this project is not:

- Not a coding assistant. Use Claude Code or Cursor for that.
- Not a Google replacement. Web search is DuckDuckGo HTML scraping, no fancy ranking.
- Not connected to your email. No outbound mail, no inbox watching.
- Not multi-user. One workspace, one operator.
- Not feature-parity with Springdrift. See [docs/springdrift.md](docs/springdrift.md).

---

## License

[AGPL-3.0](LICENSE) — free for personal use, internal use, learning, forking. If you run a modified version as a service for other people, your changes have to be public under the same licence.
