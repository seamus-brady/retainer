---
name: delegation-strategy
description: Decision framework for when and how to delegate work to Retainer's specialist agents. Covers the two specialists in this reference cut (observer, scheduler) plus the cog's direct tools.
agents: cognitive
---

## Agent Delegation Strategy

### When to Delegate

Delegate when the task matches a specialist's scope. Retainer ships two specialists in this reference cut:

| Task Type | Agent | When NOT to delegate |
|---|---|---|
| Any memory-shaped question — recent OR deep. Cycle inspection, recall, fact lookup, CBR retrieval/curation, deep-archive search, cross-store connections, pattern mining | observer | Quick status check the user asked about explicitly — answer directly. Memory tools you can call yourself (memory_read / memory_query_facts) for fact-key-shaped lookups |
| Autonomous-cycle scheduling — cron-recurring or one-shot prompts the cog runs on its own | scheduler | A one-off thing the operator wants run NOW — that's a regular cycle, not a scheduled one |

The observer routes recent vs deep internally based on the question — you don't need to pre-classify.

Web search and URL fetch live on the cog directly via `web_search` and `fetch_url` — no separate researcher specialist. Just call them.

Do NOT delegate for:
- Short conversational replies (answer directly)
- Tasks you can complete with a single tool call (memory_*, web_search, fetch_url are on you, not specialists)
- Follow-up questions about something already in your context window
- Anything you can verify from current narrative without dispatching a sub-cycle

### Tools you have directly (no delegation needed)

- **Memory**: `memory_write`, `memory_read`, `memory_clear_key`, `memory_query_facts`
- **Specialists**: `agent_observer`, `agent_scheduler`
- **Web**: `web_search`, `fetch_url` (on the cog directly — no separate researcher specialist in this cut)
- **Skills**: `read_skill` (consult any skill listed in `<available_skills>`)
- **Self-management**: `request_more_turns` (extend the cog's tool-turn budget mid-cycle when a deep delegation chain warrants)

The memory tools are synchronous and cheap. Use them directly when the answer is "what value did I store under this key?" — not via the observer's `get_fact`.

### Specialist scopes — one-line summary, then drill in

**Observer** (`agent_observer`) — knowledge gateway. Recent activity (cycle inspection, recall, facts) AND deep archive (full JSONL search, cross-store connections, pattern mining). Also handles CBR retrieval + curation. Never acts on the world. Read `agents-using-observer` before dispatching.

**Scheduler** (`agent_scheduler`) — autonomous-cycle scheduling. Recurring (cron) and one-shot (RFC3339) prompts the cog will run with `SourceAutonomous` when their time arrives. Read `agents-using-scheduler` before dispatching.

The per-agent `agents-using-*` skills cover briefing, expected reply shape, verification, and cost. This skill is the master TOC; the details live there so they can evolve per-agent without rewriting the routing decision tree.

### Verifying delegation results

When a specialist returns a summary:

1. **Read it before forwarding to the user.** The agent's reply text is its claim, not its work — verify the substance is what was asked for.
2. **If a delegate fails**, that's information. Report what happened to the user; don't paper over it with a plausible-sounding response.
3. **The DAG records the delegation as a child cycle of the current cog cycle.** A future inspect tool will let you walk that tree to verify what happened.

### Single agent only — no teams

Retainer dispatches to one specialist at a time. If a task genuinely needs multiple perspectives, do them sequentially yourself, or escalate to the operator.

### Cost awareness

Every delegation is its own cycle of LLM calls (the specialist's react loop runs up to its `max_turns`). The observer is `max_turns=8`; the scheduler is `max_turns=6`. A simple question that costs you one LLM call costs the observer up to 8 if you delegate. Don't delegate "what's 2+2."

### Plan before delegating

If the work has more than 2-3 logical steps, write down what you're going to ask the specialist to do *before* dispatching. The instruction you pass via `agent_<name>` is the brief — vague briefs produce vague summaries.
