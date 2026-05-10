# Configuration

Every Retainer process reads three sources, in precedence:

```
environment variables   (highest)
<workspace>/config/config.toml
built-in defaults       (lowest)
```

Plus two `.env` files, layered before env vars are read:

1. `.env` in the current working directory (loaded first).
2. `<workspace>/.env` (loaded second; **does not** overwrite anything already exported by the shell or by the cwd `.env`).

## Workspace resolution

In order:

1. `--workspace <path>` CLI flag.
2. `$RETAINER_WORKSPACE` env var.
3. `$HOME/retainer` (default).

`retainer init <path>` seeds a workspace. It is idempotent: existing files are preserved unless `--force` is passed. `init` also drops a `data/`-only `.gitignore` so an operator pointing `--workspace` at a git repo doesn't accidentally commit cycle logs.

## Environment override convention

Every TOML field has an env equivalent: `RETAINER_<SECTION>_<KEY>` for nested fields, `RETAINER_<KEY>` for top-level fields.

```sh
RETAINER_PROVIDER=anthropic
RETAINER_AGENT_NAME=Echo
RETAINER_COG_MAX_TOOL_TURNS=15
RETAINER_POLICY_FABRICATION_ENABLED=false
```

String-slice fields accept comma-separated values; whitespace around each element is trimmed; empty elements are dropped:

```sh
RETAINER_COMMS_ALLOWED_RECIPIENTS=alice@example.com,bob@example.com
```

Slices of non-string elements stay TOML-only.

## Reference

The authoritative reference with comments is `internal/example/config.example.toml` — it's the file `retainer init` writes to `<workspace>/config/config.toml`. Sections:

### Top level — provider and models

```toml
provider        = "anthropic"                    # anthropic | mistral | mock
task_model      = "claude-haiku-4-5-20251001"    # cog + specialists
reasoning_model = "claude-opus-4-7"              # reserved for the model split
max_tokens      = 2048                           # output tokens per LLM call
```

`mistral` is wired but explicitly second-class: empirical testing (2026-05-09) showed it fabricates tool outcomes on multi-turn agentic work. The harness has the safety nets to make it usable; expect more operator vigilance.

`mock` is the deterministic offline provider. Used by integration tests (`scripts/integration-tests/lib.sh` forces `RETAINER_PROVIDER=mock`).

### `[agent]`

```toml
[agent]
name = "Nemo"
```

Persona / character text lives in `<workspace>/config/identity/persona.md` and `session_preamble.md`, **not** in `config.toml`. The curator re-renders both per cycle, substituting slots like `{{agent_name}}`, `{{date}}`, `{{time}}`, `{{workspace}}`.

### `[gate]` — cog watchdog

```toml
[gate]
timeout_ms      = 60000   # per non-Idle status transition
input_queue_cap = 10      # buffered UserInputs while cog is busy
```

### `[cog]` — react loop and history

```toml
[cog]
# max_tool_turns       = 10      # hard ceiling 20, even with request_more_turns
# max_context_messages = 200     # 0 = unbounded; smaller = tighter token budget
```

`max_context_messages` drops the **oldest** messages while preserving role-alternation and tool-call/tool-result pair invariants. Setting `0` matches Springdrift's default (unbounded) but is unsafe for multi-day sessions.

### `[policy]` — refusal text and L2 / canary tuning

```toml
[policy]
input_refusal              = "I can't help with that."
output_refusal             = "I started to answer but stopped — could you rephrase?"
llm_scorer_timeout_ms      = 8000
canary_failure_limit       = 3
```

### `[policy.fabrication]` — output-side fabrication scorer

Default-on. See [policy.md](policy.md#output-side-fabrication-scorer).

```toml
[policy.fabrication]
# enabled        = true
# model          = ""        # defaults to task_model
# timeout_ms     = 6000
# min_confidence = 0.7
```

### `[memory]` — librarian hot-index windows

```toml
[memory]
narrative_window_days = 60
```

Bounds the SQLite index loaded at startup, **not** the JSONL on disk. Older narrative stays on disk and is reachable via observer's `deep_search`.

### `[logging]`

```toml
[logging]
verbose        = false
retention_days = 30
```

`retention_days` controls log file pruning under `data/logs/`, not memory stores.

### `[retry]` — LLM transport retries

Reserved for the retry layer; commented out today.

## Environment variables

| Variable | Purpose |
| --- | --- |
| `RETAINER_WORKSPACE` | Override workspace dir. Default `$HOME/retainer`. |
| `RETAINER_CONFIG` | Path to a specific config file (alternative to `--config`). |
| `RETAINER_PROVIDER` | Force provider. `anthropic` / `mistral` / `mock`. |
| `RETAINER_MODEL` | Override `task_model`. |
| `RETAINER_COG_SOCKET` | Override the cog Unix socket path. Used in Docker (virtiofs cannot host sockets). |
| `ANTHROPIC_API_KEY` | Required for `provider = "anthropic"`. |
| `MISTRAL_API_KEY` | Required for `provider = "mistral"`. |

Env-var-side aliases for nested TOML fields follow the `RETAINER_<SECTION>_<KEY>` convention; they exist for every field shown above.

## Docker socket

Docker on macOS uses virtiofs for bind mounts, and virtiofs cannot host Unix-socket binds. `scripts/docker-entrypoint.sh` sets `RETAINER_COG_SOCKET=/tmp/retainer-cog.sock` (container-local, not in the bind-mounted workspace). Both `retainer` and `retainer-webui` honour the env var. Native macOS / Linux runs leave the socket in `<workspace>/data/cog.sock` where it's also a useful debugging surface.

## What `retainer init` writes

```
<workspace>/
├── .gitignore                   # data/ — generated state, not committed
├── config/
│   ├── config.toml              # from internal/example/config.example.toml
│   ├── policy.json              # from internal/policy/defaults.json
│   ├── identity/
│   │   ├── persona.md           # from internal/identity/defaults/
│   │   └── session_preamble.md
│   └── skills/<id>/SKILL.md     # from internal/skills/defaults/
└── data/
    └── (created on first run)
```

`init --force` overwrites; without it, existing files are preserved and the binary refuses rather than clobber operator edits.
