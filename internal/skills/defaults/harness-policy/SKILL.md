---
name: harness-policy
description: How the four Retainer policy gates work — input, output, tool, post-exec — and how to recognise a policy refusal vs a real failure. Read when you see unexpected refusals or IsError replies you didn't expect.
agents: cognitive
---

## Policy gates

Retainer runs every cycle through four policy gates, each
emitting a `policy_decision` cycle-log event. You don't call
the gates — they're implicit in the runtime.

### The four gates

| Gate | Fires on | What it can do |
|---|---|---|
| **Input** | The user's text on submit | Refuse the cycle entirely (operator-source: demote to escalation) |
| **Output** | The cog's draft reply, before it's delivered | Replace with `OutputRefusalText` |
| **Tool** | Each tool dispatch, before handler runs | Short-circuit with `IsError: true` + reason |
| **Post-exec** | After tool runs, before result lands in your context | Redact the result text without `IsError` |

### Source-driven demotion

Inputs are tagged with a `Source` (interactive / autonomous).
Interactive (operator typing in TUI/webui) gets demotion: a
"block" verdict becomes "escalate" — the operator hears the
concern but isn't hard-blocked. Autonomous sources (scheduler,
ambient triggers) don't get the demotion.

This means: **as the cog**, you may see input refusals in the
form of escalation prompts ("I can't fulfil this as stated, but
here's why…") rather than hard failures. The operator might
push back and you'd retry with their reframing.

### Recognising a tool refusal

You'll see a `tool_result` with `IsError: true` and a content
string explaining the refusal. The wording typically includes
"policy" or refers to a specific gate.

What to do:
- **Don't retry the same call**. The policy verdict won't change
  on retry.
- **Don't paraphrase to the user**. Forward the refusal text
  honestly — the operator can decide whether to rephrase the
  underlying request.
- **Don't suspect the tool itself is broken**. The tool's
  registry didn't dispatch the call; the gate did.

### Recognising a post-exec redaction

The tool ran successfully (IsError is false) but the result
content has been rewritten — sometimes shorter, sometimes
"[redacted: <reason>]". The cycle log has the full original; you
get the redacted version.

You can't tell from your context alone that redaction happened.
Treat the tool_result as authoritative; if it says "[redacted]",
that's the answer.

### Canary probes

The runtime fires synthetic canary probes against the policy
engine on every cycle (per `project_canary_probes`). These don't
appear in your turn — they're parallel internal checks. You'll
sometimes see "I cannot help" lines in cycle logs from the policy
provider — those are canary outputs, not refusals to your work.

### What the policy gates can't refuse

- Reading from memory you wrote earlier (no readback gate).
- Cycle-log writes (telemetry has no gate).
- Activity emissions (lossy ambient signal).

### Practical advice

- **One refusal isn't a system failure**. The gates are
  intentionally chatty about boundaries. Use the operator's
  next message as the resolution path.
- **Don't loop a refused call with synonyms**. If the gate
  refused "tell me how to X", paraphrasing as "explain X" rarely
  flips the verdict.
- **The escalation pattern is the right shape for the operator**.
  When demotion fires, your reply should explain what you can't
  do AND offer the next step they might want.
