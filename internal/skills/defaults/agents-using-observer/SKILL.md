---
name: agents-using-observer
description: When the Retainer cog should delegate to the observer, how to brief it, what to expect back, and how to verify the result. Observer is the knowledge gateway — recent (hot index) and deep (full archive). Read before dispatching to agent_observer.
agents: cognitive
---

## Using the observer

The observer is the **knowledge gateway**. It looks inward — at
cycles, narrative, facts, CBR cases — AND reaches into the full
JSONL archive when needed. You reach it via `agent_observer`.

The observer routes recent vs deep internally. You don't need to
pre-classify the question.

### When to delegate

**Recent (hot index):**
- "What did I do earlier?" / "what was that about?" — recall
  beyond your conversation window
- "Why did cycle X fail?" / "what was the result of cycle Y?" —
  cycle drill-down
- A curated summary of recent activity (3+ cycles), not one fact
  lookup

**Deep (full archive):**
- "Have we ever seen X?" / "find every time we discussed Y" —
  recall older than the librarian's 60-day hot index
- "Consolidate the last month's work into a report"
- "What patterns do we see across the case archive?" — pattern
  mining for recurring shapes
- Cross-store topic mapping ("everything about auth across
  narrative + facts + cases")

**CBR curation:**
- "Suppress this case" / "boost that one" / "annotate with this
  pitfall" — operator-driven curation
- "Recall cases similar to this query" — CBR retrieval

### When NOT to delegate

- A single fact lookup ("what's the value of key K?") — call
  `memory_read` directly. The observer's `get_fact` is the same
  data; the round-trip via delegation is wasted.
- Questions about something already in your conversation context.
- Anything requiring action on the world — the observer reads,
  never writes (its only writes are CBR curation events and
  consolidation reports, which are knowledge-shaped).

### Briefing the observer

Be specific about the question. The observer has many tools and
you're effectively scoping it when you brief:

**Recent — cycle drill-down:**
> "Inspect cycle abc12345. Report status, duration, and any
> error."

**Recent — activity recall:**
> "Show the last 5 narrative entries with summaries."

**Recent — fact lookup:**
> "What's the current value of the fact `user.preference.theme`?"

**Deep — historical search:**
> "Search the full narrative archive for any cycle that
> discussed the auth-flow timeout. Date range: open-ended."

**Deep — consolidation:**
> "Consolidate work from 2026-04-01 to 2026-05-01 around the
> auth flow. Write a report if the digest is worth keeping."

**Deep — pattern mining:**
> "Mine the case archive for recurring problem shapes. Min
> cluster size 3."

**CBR curation:**
> "Suppress case abc123 — operator says it's a bad pattern."

A vague brief like "what's been going on" makes the observer
guess which tool to call. Pass relevant context — operator's
prior question, the cycle ID you're curious about, the date
range, whatever scopes the work.

### What to expect back

A direct text reply. Usually:
- The answer at the top
- Cycle IDs (8-char prefix) and timestamps for anything sourced
- Caveats on what couldn't be determined
- Confidence qualifiers on old material (decayed facts flagged
  as decayed; old cases noted as potentially outdated approaches)
- Report file path when synthesis was the deliverable

The observer won't speculate beyond the data — if the narrative
or archive doesn't say, neither does it.

### Verifying the result

- **"no matching cycle" / "no fact found" / "no patterns above
  threshold"** are real answers, not failures. Treat them as
  authoritative.
- **Cycle IDs in the observer's reply** are real — you can pass
  them to a follow-up `inspect_cycle` brief if you want more
  detail on one.
- **The observer doesn't fabricate.** If it says X, the data
  said X.
- **Synthesis output (consolidation, mining)** is the
  observer's voice — re-reading raw excerpts isn't required,
  but flag back to the user if the synthesis seems thin.

### What the observer refuses / can't do

- Won't take actions on the outside world.
- Won't write to memory keys (consolidation reports + CBR
  curation events are the only writes; both are
  knowledge-shaped).
- Can't predict future cycles or recommend what the cog should
  do.
- Doesn't replicate the cog's memory tools — for fact-key
  lookups you should use `memory_read` directly.

### Cost awareness

The observer's react loop is `max_turns=8` (was 6 before the
remembrancer fold). Recent queries usually finish in 2-3 turns;
synthesis-shaped work (consolidation, mining) can use 4-6.

Still — one or two of your direct memory tools is usually
faster than a delegation. Reserve the observer for queries
that genuinely need cycle inspection, curated recall, or
deep-archive work.
