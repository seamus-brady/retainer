---
name: self-diagnostic
description: An 8-step procedure for verifying that Retainer's tools and specialist delegates are wired correctly end-to-end. Run when the operator asks for a self-check, a health check, or "is everything working?". Do not run unprompted.
agents: cognitive
---

## Self-diagnostic — procedural

When the operator asks for a self-check, run this procedure in
order. Each step touches a real surface and reports what
happened. The diagnostic IS the runtime — there is no separate
"test mode." If a step fails, that failure is a real signal
about the running system, not a test artifact.

The point of this skill is to drill the habit of trusting and
using the surface I actually have. Skip nothing. Don't
substitute reasoning for tool calls.

### Step 1 — memory_write

Write a single ephemeral fact:

- key: `self_diagnostic_marker`
- value: `<current ISO timestamp from the sensorium clock>`
- scope: `ephemeral`
- confidence: `1.0`
- description: `self-diagnostic write, will be cleared`

Record whether the write succeeded.

### Step 2 — memory_read

Read back the marker key. The returned value should match what
I wrote in step 1. Record whether it matched.

### Step 3 — memory_clear_key

Clear `self_diagnostic_marker` so the diagnostic doesn't leave
clutter in the operator's fact store. Record whether the clear
succeeded.

### Step 4 — agent_observer

Dispatch with instruction: "Recall the most recent narrative
entry. One-line summary." The observer should reply with a
short summary of the most-recent cycle (the one that drove
this diagnostic, or the previous one).

If the observer reply is empty or generic ("no entries"), that
is still a successful dispatch — the chain works; the index is
just thin. Record success on dispatch, note any anomaly in the
content.

### Step 5 — agent_researcher (skip-if-absent)

If `agent_researcher` appears in my tool list, dispatch with
instruction: "Search for `retainer` and return one source."
The researcher should reply with a result (URL + brief
summary).

If `agent_researcher` is NOT in my tool list, this means no
search-API keys are configured in the workspace. Record as
"skipped — researcher not configured" and continue. This is a
valid workspace state, not a failure.

### Step 6 — agent_taskmaster

Dispatch with instruction: "List my pending tasks." The
taskmaster should reply with the Taskwarrior `list` table
verbatim (or `no pending tasks` for an empty store). Either
outcome is a successful dispatch.

### Step 7 — agent_scheduler

Dispatch with instruction: "List my scheduled jobs." The
scheduler should reply with active jobs sorted by next-fire
ascending (or `no active jobs` for an empty workspace). Either
outcome is a successful dispatch.

### Step 8 — read_skill

Call `read_skill` with the location of THIS skill (its path
appears in `<available_skills>` under `<name>self-diagnostic
</name>`). The tool should return the body of this file.
Record whether the read succeeded — proves the skill substrate
itself is functional from inside a live cycle.

### Final report

Reply with a structured summary, one line per step:

```
Self-diagnostic results
1. memory_write       — ✅ ok | ❌ <error>
2. memory_read        — ✅ ok (matched) | ⚠ ok (mismatch: got X, wanted Y) | ❌ <error>
3. memory_clear_key   — ✅ ok | ❌ <error>
4. agent_observer     — ✅ dispatched | ❌ <error>
5. agent_researcher   — ✅ dispatched | ⏭ skipped (not configured) | ❌ <error>
6. agent_taskmaster   — ✅ dispatched | ❌ <error>
7. agent_scheduler    — ✅ dispatched | ❌ <error>
8. read_skill         — ✅ ok | ❌ <error>

Overall: <healthy | degraded — see failures above>
```

Use the literal characters ✅ ⚠ ⏭ ❌ — the operator parses
this output by eye, and the symbols make pass/fail scannable.

### When NOT to run

- Unprompted. The diagnostic costs ~7 LLM calls (one per
  dispatch + one for the final reply); never run it without an
  operator ask.
- Mid-task. If the operator is in the middle of asking for
  something else, finish that first.
- Repeatedly within a session. Once per session is plenty;
  results don't change minute-to-minute.

### Failure modes I might see

- **Step 4 / 5 / 6 / 7 returns "agent not available."** The
  delegate tool is missing from my tool list. That's a wiring
  break — surface it plainly; the operator needs to know.
- **Step 8 returns an error about path validation.** `read_skill`
  is restrictive about which paths it accepts. If the skill's
  `<location>` from the system prompt fails this check,
  something is wrong with how the skill was discovered.
- **Repeated LLM errors during a dispatch.** Adapter-level
  decode bug or rate-limit hit. The error message will name the
  cause; report it verbatim.
