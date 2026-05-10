---
name: harness-tools
description: How tools work in Retainer's current shape — call shape, error semantics, IsError vs error envelope, what to retry vs give up on. Read when a tool returns something unexpected or when planning to use multiple tools.
agents: cognitive, researcher, observer
---

## Tools

Tools are functions you can call by emitting a `tool_use` block
in your LLM response. Each call goes through:

```
your tool_use → policy gate → handler.Execute → tool_result back to you
```

### Call shape

Every tool has:
- A name (`memory_read`, `agent_researcher`, `inspect_cycle`, etc.)
- An input schema (you provide JSON matching the schema)
- An output: text the next turn sees as `tool_result` content

You don't control the dispatch — you describe what you want, the
runtime executes it, and the result lands as a content block in
your next message.

### Two failure shapes — distinguish them

#### Tool error (`IsError: true`)

The tool ran and decided your input was wrong, or the policy
gate refused the call. The result content explains. Examples:

- `memory_write` with a missing key → tool error: "key required"
- A policy refusal on a tool gate → tool error: "policy rejected"
- A subprocess agent reporting `status: error` → tool error
  bubbling the agent's reason

What to do: **read the error, fix your call, try again** — if the
error is recoverable. Don't loop the same call expecting a
different result.

#### Transport / runtime error

The tool itself failed to dispatch (provider down, network
partition, decode error before reaching the handler). You see
this as a tool_result with an error message that doesn't look
like input validation.

What to do: **the retry middleware already retried**. If you're
seeing it, the retry budget is exhausted. Treat as terminal for
this turn; report to the user / orchestrator.

### Don't confuse tool error with policy refusal

A policy refusal IS a tool error from your perspective — the
result has `IsError: true` and the content is the refusal text.
You can't tell from the error envelope whether the policy
rejected you or the tool's own validation did.

That's fine. Both mean: don't retry the same call.

### Multiple tool calls in one turn

You can emit multiple `tool_use` blocks in the same response.
The cog dispatches them in parallel (per
`project_loop_closing`). Their results land as separate
`tool_result` blocks in your next message in dispatch order.

Be intentional with parallel calls — three fact-lookups is fine,
three searches that depend on each other isn't.

### When to give up on a tool

- Three consecutive errors on the same tool with the same input
  shape → stop. Either your input shape is wrong or the tool is
  down.
- An IsError reply where the message says "policy rejected" or
  "permission denied" → stop. Trying again won't change the
  policy.

### Practical advice

- **Read tool descriptions**. They're in your tool list every
  turn. Don't guess at input shapes.
- **Don't paraphrase tool output to the operator** unless asked
  — pass the relevant content through. Paraphrase loses signal.
- **One tool per concept**: `memory_read` for one key,
  `memory_query_facts` for keyword search. Don't `memory_read`
  in a loop when the search tool exists.
