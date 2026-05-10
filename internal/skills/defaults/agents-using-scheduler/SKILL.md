---
name: agents-using-scheduler
description: When the Retainer cog should delegate to the scheduler, how to brief it, what to expect back, and how to verify the result. Read before dispatching to agent_scheduler.
agents: cognitive
---

## Using the scheduler

The scheduler specialist manages autonomous-cycle scheduling.
You reach it via `agent_scheduler`. Scheduled prompts fire on
their own; the cog runs them with `SourceAutonomous` so the
policy gate's strict path applies (no operator-typing
demotions).

### When to delegate

- The operator wants something to happen on a schedule
  ("every Monday morning…", "remind me on Sunday at 6pm",
  "every 15 minutes…").
- The operator wants to see / cancel / inspect what's already
  scheduled.
- A task added via `agent_taskmaster` already auto-schedules
  a reminder when it has a `due:<RFC3339>` modification — the
  taskmaster bridges to the scheduler internally. You don't
  need to ALSO call `agent_scheduler` for that case; the
  bridge has it covered.

### When NOT to delegate

- The operator wants a one-off thing run NOW — that's a
  regular cycle, not a scheduled one.
- The operator is asking what got done after a scheduled
  cycle fired — that's `agent_observer` (cycle history), not
  the scheduler.
- The operator is adding a todo without a due time — that's
  `agent_taskmaster`. The scheduler is for autonomous fires;
  the taskmaster is for trackable items.

### How to brief

Send a clear instruction containing:
- **The cadence or fire time** in natural language ("every
  Monday at 9am", "tomorrow at 6pm", "every 15 minutes").
  The scheduler agent translates to cron / RFC3339.
- **The prompt the cog should run when the job fires.** Be
  explicit about the work — the cog gets this verbatim with
  no extra context, so vague prompts produce vague cycles.
- For cancellations, the job's name or a description; the
  scheduler agent looks up the ID.

Examples:
- "Schedule a job every Monday at 9am with the prompt
  'summarise the past week's activity and surface any open
  threads'."
- "Cancel the daily morning summary."
- "What's scheduled?"

### What to expect back

- For `schedule_job`: confirmation with the assigned ID,
  cron/fire-time, and the prompt that will run.
- For `list_jobs`: a structured list (one block per job)
  sorted by next-fire ascending. Pass it through to the
  operator verbatim — don't paraphrase.
- For `cancel_job`: a one-line confirmation with the ID.
- For `inspect_job`: full record (cron, last/next fire,
  fired count, prompt body).

### Verifying the result

If you dispatched `schedule_job` and the reply confirms a
job ID, trust it. The scheduler service persists every
operation to JSONL; jobs survive restart.

If you later want to know whether a scheduled cycle actually
fired, that's an `agent_observer` question — `recall_recent`
shows past cycles regardless of source.

### Cost note

Each `agent_scheduler` dispatch is one full agent cycle. For
common operations (one cron, one operator) it's negligible.
The scheduled cycles themselves are normal cog cycles; their
LLM cost is whatever their prompt provokes.
