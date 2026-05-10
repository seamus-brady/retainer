---
name: harness-memory
description: Mechanics of Retainer's current memory layer — what's narrative vs facts vs cases, when each is written vs read, retention semantics, who can see what. Read with `memory-management` (which covers the decision procedure).
agents: cognitive, researcher, observer
---

## Memory mechanics

This skill covers the *mechanics*: where data lives, who writes
it, how long it persists. For the *decision procedure* (when to
use which store), read `memory-management`.

### Three stores

| Store | What's in it | Disk | Index window |
|---|---|---|---|
| **Narrative** | One entry per cycle: cycle_id, summary, status. Append-only JSONL. | `data/narrative/<date>.jsonl` | 60d rolling (librarian) |
| **Facts** | Key/value with confidence, scope. Most-recent-per-key wins. | `data/facts/<date>.jsonl` | None — current state |
| **Cases** (CBR) | Problem/solution/outcome triples. 6-signal retrieval. | `data/cases/cases.jsonl` | None — full archive |

### Disk is immutable

Per `project_archive_immutable`: memory JSONL files are NEVER
deleted or edited. Updates land as new entries. The librarian
(SQLite hot index) prunes old entries from the index, but the
disk JSONL is the source of truth for the deep archive.

This means: a `memory_clear_key` writes a tombstone, it doesn't
delete history. The next `memory_read` on that key returns "no
fact" because the index respects the tombstone, but the
remembrancer's `deep_search` could still find the original.

### Who writes what

- **Cog** writes narrative (one entry per cycle, on
  `cycle_complete`) and facts (via `memory_write`).
- **Archivist** writes cases (derived from cycle outcomes via the
  judge — happens automatically; you don't trigger it).
- **Agents** don't write to long-term memory directly. Their
  reply is the deliverable; the cog decides what to remember.

### Who reads what

| Tool | Reads | Available to |
|---|---|---|
| `memory_read` | Facts (librarian hot index) | Cog |
| `memory_query_facts` | Facts (keyword search, hot index) | Cog |
| `recall_recent` | Narrative (recent N, hot index) | Observer |
| `inspect_cycle` | Narrative + DAG (hot index) | Observer |
| `get_fact` | Facts (hot index) | Observer |
| `recall_cases` | Cases | Observer (via `agent_observer`) |
| `case_curate` | Cases (action-discriminated mutation) | Observer (via `agent_observer`) |
| `deep_search` | All JSONL (full archive, incl. reports + patterns log) | Observer (via `agent_observer`) |
| `find_connections` | All JSONL (full archive) | Observer (via `agent_observer`) |

The librarian's hot index gives the cog + observer fast reads
over recent material. The deep-archive read surface (full JSONL,
months/years) is owned by the Observer agent — the cog reaches it
via `agent_observer`, not by calling the tools directly. See
`agents-using-observer` for routing.

Pattern mining and weekly consolidation no longer have tools.
They run on the **metalearning pool** (`internal/metalearning/`)
on a tick — daily mining writes mined-cluster JSONL to
`data/patterns/patterns.jsonl`; weekly consolidation writes a
markdown digest to `data/knowledge/consolidation/`. The Observer
reads those durable artifacts via `deep_search` rather than
regenerating on demand.

### Confidence + decay

Facts carry a confidence (0.0–1.0) at write time. Reads decay it
based on age (half-life — see `internal/decay/`). A fact written
months ago with confidence 0.9 might come back as 0.5.

Practical: write what you actually believe. 1.0 confidence is
fine for hard facts ("user's name is X"); use 0.5–0.7 for
inferences and observations.

### Scopes

- **Persistent** — survives sessions; the default for "remember
  this for me"
- **Session** — current run; cleared by cog restart
- **Ephemeral** — short-lived; today behaves like session
  (end-of-cycle clear isn't wired)

### What's NOT in memory

- Your conversation context window — that's not "memory" in the
  store sense, it's just the in-flight turn data.
- Skills — those are static workspace files, not store entries.
- The cycle log — diagnostic telemetry, not retrievable via the
  memory tools (use `inspect_cycle` or read JSONL directly).

### Practical advice

- **Don't write self-narrating facts** like "I helped the user
  with X today." That belongs in narrative (which writes itself
  per cycle).
- **Don't write huge content as a fact**. The artifacts subsystem
  for big content isn't shipped; right now long fact values waste
  retrieval bandwidth.
- **Read once, act**: if you've already done `memory_read('k')`
  this turn, don't do it again next turn. The value's in your
  context.
