---
name: harness-errors
description: Recovery patterns in Retainer's current shape for tool errors, agent failures, and cycle abandonment — when to retry, when to narrow, when to escalate. Read after seeing an unexpected error or when planning a multi-step task that might fail partway.
agents: cognitive, researcher, observer
---

## Errors and recovery

### Three error layers

- **Tool layer** — a single tool dispatch errored. Local; affects
  the next turn only.
- **Agent layer** — an agent dispatch failed (subprocess crash,
  max_turns, max_consecutive_errors). One dispatch, terminal.
- **Cycle layer** — the whole cycle abandoned (watchdog,
  unrecoverable error). The operator gets an error reply.

Each has a different recovery shape.

### Tool-level recovery

When a tool returns IsError:

1. **Read the message.** It usually says what's wrong.
2. **Decide between fix-and-retry vs give up:**
   - Input-validation error ("key required", "invalid range") →
     fix the input, retry.
   - Policy refusal → don't retry; see `harness-policy`.
   - Transport / runtime error → already retried by middleware;
     stop, report.
3. **Don't loop the same call.** Three consecutive errors on the
   same tool with the same input means stop.

### Narrowing

If the work is failing because the brief is too broad (researcher
returns "couldn't find a definitive answer", or tool errors that
suggest scope mismatch):

- **Narrow the question.** Instead of "find everything about X",
  try "find the official spec for X published after date Y".
- **Sequence the work.** Step 1 dispatch finds candidates; step
  2 picks the best one and verifies; step 3 reports.

Narrowing is often more useful than retrying.

### Escalating

If you can't make progress:

- **As an agent**: include a partial result with an honest gap
  ("found X, couldn't verify Y, would need a tool I don't
  have"). Let the cog decide.
- **As the cog**: tell the operator what you tried, what failed,
  and what you'd need to continue. Don't synthesise a
  plausible-sounding answer to hide the failure.

### When to abandon a cycle

The cog abandons cycles automatically on:
- Watchdog timeout
- Unrecoverable internal error
- Policy hard-block on input

You don't trigger abandonment yourself. If you're tempted to
"just give up" mid-loop, instead emit a final reply that says
what happened.

### Recognising patterns

| Signal | Likely cause | Recovery |
|---|---|---|
| Repeated IsError on memory tools | Wrong key shape, or fact doesn't exist | Try `memory_query_facts` with a keyword first |
| Subprocess agent: "exited without terminal response" | Spawn worked, agent crashed mid-run | Report; don't retry — likely a real bug |
| `max_turns exhausted` from agent | Brief too open-ended | Re-dispatch with a narrower instruction |
| Three consecutive llm_request errors | Provider down or rate-limited | Stop; the retry middleware already retried |
| Tool refusal with "policy" in message | Policy gate engaged | Don't retry the same call |

### Practical advice

- **Errors are signal, not noise**. Read them; they're more
  useful than your guesses.
- **Don't claim success when you've failed**. The narrative
  records the truth either way; mismatched replies erode trust.
- **One try per concept; iterate, don't repeat**. If your first
  approach didn't work, the second one should be different,
  not the same with more emphasis.
