# Memory

The librarian (`internal/librarian`) is the single owner of memory state. All reads and writes go through its inbox; there is no parallel cache, no per-actor memory store. Persistence is split into two layers with strict roles.

## Two-layer model

```
JSONL on disk                  SQLite (in-memory, modernc)
─────────────                  ──────────────────────────
durable record                 rebuildable index
append-only                    queries: recency, key, scope
one file per day               loaded at startup from JSONL
data/{store}/<date>.jsonl      lives in-process
never deleted                  thrown away on shutdown
```

Every `Record*` operation in the librarian:

1. Appends one JSON line to the day's JSONL file.
2. INSERTs the same row into SQLite.

If the SQLite write fails, the librarian logs and continues. On the next startup the replay heals the index from JSONL. **Disk is the truth, SQLite is the cache.** Operators can delete the entire `data/{store}/` SQLite-backing-only state without losing data — the JSONL repopulates it.

## Stores

### Narrative (`data/narrative/<date>.jsonl`)

One entry per cog cycle. Each entry is a one-line summary plus metadata:

- `cycle_id`
- `summary` — model-generated, written by the archivist on `CycleComplete`
- `timestamp`, `domain`, `status`

The SQLite hot index loads the last `narrative_window_days` (default 60). Older entries stay on disk and are accessible only through the deep-archive path (observer's `deep_search`).

### Facts (`data/facts/<date>.jsonl`)

Key/value entries with provenance and confidence. Schema (`internal/librarian/facts.go`):

```
Key           string
Value         string
Scope         FactScope     // persistent | session | ephemeral
Operation     FactOperation // write | clear | superseded
Confidence    float64       // stored value never mutates
Timestamp     time.Time
SourceCycleID string
HalfLifeDays  float64
```

**Decay is read-time only.** `Fact.DecayedConfidence(now)` applies a half-life formula: `Confidence * 0.5 ^ (age_days / HalfLifeDays)`. The stored confidence is immutable. Half-life of 0 means no decay.

**Tombstones, not deletes.** A `Clear` operation appends a tombstone record. Reads of a tombstoned key return nil. The JSONL stays immutable, which is what makes the index rebuildable.

**Supersession.** Writing a fact with the same key writes a new entry; the most recent record wins at lookup. Older entries get an implicit `Superseded` status when queried.

**Scopes:**
- `Persistent` — survives session restarts. Loaded into the index on every boot.
- `Session` — current run only. Index loads but does not preserve across restarts in practice (entries dated outside the session are filtered).
- `Ephemeral` — cleared at end of cycle.

### Cases — CBR (`data/cases/<date>.jsonl`)

Case-based reasoning records: `(situation, action, outcome)` triples plus retrieval metadata. The CBR retrieval path (`internal/cbr`) uses a 6-signal score: keyword overlap, embedding similarity, recency, success rate, suppression flag, operator boost.

Curation surface (observer's `case_curate` tool): suppress, unsuppress, boost, annotate, correct. Each curation action appends a new record; the index reflects the latest state.

### Captures (`data/captures/<date>.jsonl`)

Promise tracking. When the agent says "I'll remember that" or "I'll follow up tomorrow", the captures actor (`internal/captures`) records the commitment. The metalearning pool reviews open captures and surfaces stale ones.

## Cycle log

`data/cycle-log/<date>.jsonl` is **not** a librarian store — it is the master event stream. Every actor writes events here through `internal/cyclelog`. The file is consulted by:

- The observer's deep-archive tools (`deep_search`, `find_connections`).
- The metalearning audits (daily mining and consolidation).
- Operators debugging cycles.

Every other store is derivable from the cycle log. If a librarian JSONL is corrupted, reconstruction from the cycle log is feasible (though not currently automated).

## Embeddings

`internal/embed` wraps Hugot / ONNX. Models live under `data/models/` and download lazily on first use. The CBR retrieval path uses these for the embedding-similarity signal; nothing else queries them directly.

## Decay (`internal/decay`)

A pure-function port of Springdrift's `dprime/decay.gleam`. Currently used for fact half-life only; the formula is general enough to apply to other stores when needed.

## Daily file rotation

`internal/dailyfile` provides the `<store>/<YYYY-MM-DD>.jsonl` write path with mid-day file rotation. All librarian writes go through it. There is no log-rotation tool; files accumulate forever by design — they are the durable record. Operators wanting a smaller workspace prune by hand.

## What is *not* in the librarian

- **System prompt slots.** The curator queries the librarian for "recent narrative", "most-recent persistent facts", "recalled cases" — but the curator, not the librarian, owns assembly. The librarian never renders.
- **Conversation history.** The cog holds the in-cycle message list. After `CycleComplete`, the messages are summarised into narrative; raw history is not preserved as a queryable store.
- **Logs.** `slog` JSON logs live under `data/logs/`. They are operator-facing diagnostics, not memory.
