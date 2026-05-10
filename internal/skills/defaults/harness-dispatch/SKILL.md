---
name: harness-dispatch
description: How dispatching to specialist agents works in Retainer's current shape — the agent_<name> tool shape, what the reply envelope looks like, how errors propagate. Read with `delegation-strategy` (which covers when to delegate).
agents: cognitive
---

## Dispatching to specialists

You delegate work to specialists via `agent_<name>` tools. Each
specialist surfaces as one tool: `agent_researcher`,
`agent_observer`. From your perspective, the dispatch is
synchronous — you call the tool, you get a reply.

Behind the scenes the agent has its own react loop, system
prompt, tools, and (sometimes) its own subprocess. You don't see
any of that — just the final text.

### The tool shape

```json
{
  "name": "agent_<name>",
  "input": {
    "instruction": "<what to do>",
    "context": "<optional supplementary text>"
  }
}
```

- **`instruction`** is required. This is the brief — be specific.
- **`context`** is optional. Use it for background the agent
  needs but that isn't the work itself ("the user's prior
  question was X").

The instruction becomes the user message inside the agent's
react loop. The context, when present, is prepended:
`"Context: ${ctx}\n\nInstruction: ${instr}"`.

### The reply envelope

You get back the agent's final text. Successful dispatches are a
plain `tool_result` with `IsError: false`.

The reply does NOT contain:
- The agent's intermediate tool calls (those are visible in the
  cycle log via `inspect_cycle` if you really need them)
- The agent's stop reason / token counts (those land in
  cycle-log events, not your turn data)
- Multiple replies — one dispatch, one final text

### Error shapes

Three failure modes:

1. **Agent-level failure** — the agent ran but reported
   `success: false` (e.g. researcher hit max_turns without a
   final reply). You see `IsError: true` with the agent's
   reason. *Don't retry blindly* — the brief was probably wrong
   for what the agent could do.

2. **Process-level failure** (subprocess specialists only) —
   the subprocess crashed, exited without sending a terminal
   response, or the binary couldn't be spawned. You see
   `IsError: true` with a "subprocess <name>: …" message. This
   is infrastructure-level; report to the user, don't paper
   over.

3. **Dispatch refusal by policy** — your `agent_<name>` call
   was blocked by the tool gate. Same shape as any other tool
   refusal (see `harness-policy`).

### Parent/child cycle linkage

Every dispatch creates a `NodeAgent` DAG entry parented to your
current cog cycle, plus a chain of `agent_cycle_start` /
`llm_request` / `llm_response` / `tool_call` / `tool_result` /
`agent_cycle_complete` events in the cycle log.

You can inspect this chain after the fact via the observer's
`inspect_cycle` if you need to understand what the agent did.

### Subprocess vs in-process — you don't care

When the operator has the subprocess binary on PATH, the
researcher runs as a child process. Otherwise it runs inside the
cog. The wire shape your tool_use sees is identical either way.

### Practical advice

- **Brief like the agent has no context other than what you
  send**. Don't assume it sees the conversation. It doesn't.
- **One dispatch per concept**. Don't fire two parallel
  `agent_researcher` calls hoping for redundant verification —
  let the agent decide internally.
- **Verify the reply** before forwarding to the user. The
  agent's answer is its claim, not its work. (See the
  per-agent `agents-using-*` skills.)
