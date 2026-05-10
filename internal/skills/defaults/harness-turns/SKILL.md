---
name: harness-turns
description: How turns and cycle budgets work in Retainer's current shape — what max_turns means, what the watchdog does, what max_consecutive_errors triggers. Read when planning a multi-step task or when a cycle ended unexpectedly.
agents: cognitive, researcher, observer
---

## Turns + cycle budgets

A "turn" is one round of (LLM call → optional tool dispatches →
back to LLM). Both the cog and every agent run a bounded react
loop with a maximum number of turns.

### Budgets in this workspace

| Actor | max_turns | max_consecutive_errors | watchdog |
|---|---|---|---|
| Cog | 10 default, hard cap 20 (extendable mid-cycle via `request_more_turns`) | n/a (errors handled per-tool) | per-cycle deadline |
| Researcher | 8 | 3 | 90s default |
| Observer | 6 | 3 | 90s default |

The numbers above match Springdrift's defaults except where noted
in `internal/agent/agent.go`.

### What "max_turns exhausted" means

Your loop ran the configured number of LLM calls without
producing a final text reply (every turn ended with another tool
call). The agent returns an error: "max_turns exhausted without
final reply".

What to do:
- **As an agent**: don't deepen on the next dispatch. If the work
  truly needs more steps, return a partial answer with a note
  ("partial result; would need more turns to verify X").
- **As the cog**: the agent's error is informational — pass it to
  the user or retry with a narrower brief.

### What max_consecutive_errors does

Three LLM-call errors in a row (transport, decode, etc.) ends the
react loop with a hard error. This protects against a wedged
provider hammering the budget.

If you see this: there's likely a real upstream problem (provider
down, key revoked, network partition). The retry middleware
already handles transient errors; reaching the consecutive-errors
ceiling means it's not transient.

### What the watchdog does

The watchdog kills the cycle if it runs longer than its deadline.
This bounds the worst-case latency the operator sees.

You don't see the watchdog firing from inside the LLM loop —
your context terminates and the cog emits a `watchdog_fire`
cycle-log event. From the operator's perspective, the cycle
returns an error.

### Reading turn state

In your reply, you can see the turn from your activity stream
(if you read it) or implicitly from how many tool-result blocks
have accumulated in the conversation. Don't rely on counting
manually — focus on the work.

### Extending the cog's budget mid-cycle

The cog has a tool — `request_more_turns(additional, reason)`
— that extends its own tool-turn budget for the in-flight cycle
up to a hard ceiling of 20. Use it when:

- you're walking a diagnostic across multiple agents and would
  hit the default cap before finishing
- a deep delegation chain (cog → agent → agent's-internal-work
  → summarise) genuinely needs more rounds than 10 allows
- a self-check loop ("verify with a tool, then act") is bumping
  the cap on what's normally a short flow

Don't reach for it on routine work. Routine work fits in 10
turns. Each call to `request_more_turns` lands in the cycle
log so an operator can see when (and why) the cog needed
headroom — too many bumps is itself a signal that something's
wrong with the workflow shape.

### Practical advice

- **Plan in the first turn**: when the task is non-trivial, your
  first turn should think about what needs to happen, not jump
  to the first tool call. A wasted first turn often saves three.
- **Stop when the answer is good enough**: max_turns is a cap,
  not a target. The cleanest replies often come at turn 2 or 3.
- **Don't retry blindly when a tool errors**: read the error
  message; it's there for a reason. See `harness-errors`.
