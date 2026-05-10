---
name: memory-management
description: When and how to use the memory stores in Retainer. Adapted to current shape (narrative + facts via the librarian; CBR + remembrancer V1 shipped; threading + artifacts deferred).
agents: cognitive, observer
---

## Memory Management

### Two stores today

| Store | What goes there | How to access |
|---|---|---|
| **Narrative** | One entry per cycle: cycle_id, timestamp, status, summary (user input + reply or error). Append-only on disk. The librarian indexes the last 60 days into SQLite for fast recall. | `recall_recent` (observer agent), `agent_observer` for `inspect_cycle` |
| **Facts** | Key/value entries with scope (persistent / session / ephemeral) and confidence. Most-recent-per-key wins on lookup. JSONL on disk; index has no time window — facts are current state. | `memory_write`, `memory_read`, `memory_clear_key`, `memory_query_facts` (cog), `get_fact` (observer agent) |

### Which tool for which question

| Question | Use this | Why |
|---|---|---|
| "What did I just do?" / "What was that about?" | Cog: nothing — already in your conversation context | Don't call tools for stuff in the live window |
| "What did I do today / earlier / last session?" | Delegate to `agent_observer` with `recall_recent` | Pulls narrative entries from the librarian |
| "What's the status of cycle X?" / "Why did that fail?" | Delegate to `agent_observer` with `inspect_cycle` | DAG status + narrative summary + error for one cycle |
| "Did I store a value under key K?" | Cog: `memory_read` | Direct lookup, no delegation |
| "Search facts containing the word 'mustang'" | Cog: `memory_query_facts` | Direct keyword search, no delegation |
| Save a value to remember later | Cog: `memory_write` | Direct write |
| Forget a stored value | Cog: `memory_clear_key` | Direct tombstone (history preserved on disk; index returns nil afterwards) |

Rule of thumb: cog tools are synchronous and cheap. The observer adds value when the user wants formatted cycle inspection or a curated recall, not when they want a single key/value lookup.

### Facts: when to write

Write a fact when:
- The user tells you something that should survive cycles ("my name is Seamus", "the Cavan price is X")
- You discover a reusable configuration ("Mistral Small 4 handles X better than Y")
- A computation produced a value the next cycle will want

Use scopes correctly:
- **Persistent** — survives across sessions; the default for "remember this for me"
- **Session** — current run only; cleared by clearing the key
- **Ephemeral** — short-lived; would be cleared at end-of-cycle once that hook lands (today behaves like session)

Don't write facts for:
- Things already in your conversation history (the window has them)
- Long content (no artifacts subsystem yet — wait for that to land)
- Self-narration about your own performance

Confidence matters. If you're not sure, write 0.5–0.7 rather than 1.0. Confidence decays at read time via half-life so older uncertain facts naturally weaken.

### Reading facts honestly

`memory_read` returns the most-recent non-cleared entry with confidence decayed to now. If a fact comes back with low confidence:

- Treat it as a hint, not gospel
- Verify it before acting on it (re-read narrative, ask the user, or just say the value out loud and check the operator's reaction)

If `memory_read` returns "No fact found" — say so. Don't fabricate a remembered value.

### Remembrancer — deep archive (V1 agent shipped)

For history older than the librarian's hot index, delegate to
the **Observer** specialist via `agent_observer`. It owns the
deep-archive read tools and reads JSONL files directly (the
immutable archive), not the librarian's index.

| Tool the Observer has | Use for |
|---|---|
| `deep_search` | Free-text search across the full narrative + facts archive. Use when `recall_recent` / `memory_query_facts` come up empty and the answer might be older than the index window. Also reads weekly consolidation digests under `data/knowledge/consolidation/` and the mined-cluster log at `data/patterns/patterns.jsonl`. |
| `find_connections` | Surface relationships between cycles / facts that share themes or terms. Use to triangulate "what else have I done in this area?". |

Pattern mining and weekly consolidation no longer happen on
demand. The **metalearning pool** (`internal/metalearning/`) runs
those as background workers on a tick: daily mining clusters CBR
cases into `data/patterns/patterns.jsonl`; weekly consolidation
writes a markdown digest to `data/knowledge/consolidation/`. The
Observer reads those durable artifacts via `deep_search` — it
does not regenerate them. When the user asks "what patterns are
emerging?" or "what was learned this week?", brief the Observer
to read the corresponding artifact.

The Observer is a delegated agent (matching SD's shape). Don't
try to call its tools directly from the cog — they're not on your
registry. Brief it via `agent_observer` with a specific question.
See `agents-using-observer` for briefing patterns.

When the user asks for something archival ("what did I work on
three months ago?", "find every time we discussed auth", "show me
last week's review"), this is the right delegate.

### Still deferred

These belong in this skill but the subsystems aren't shipped yet — DON'T pretend you have them:

- **Threads** (overlap-scoring narrative grouping). No `recall_threads`.
- **Artifacts** (50KB-truncated content store). No `store_result` / `retrieve_result`.
- **Memory trace** (full key history). No `memory_trace_fact`.

If the user asks for any of these, say what's not available. Do NOT pretend by approximating with the tools you have.
