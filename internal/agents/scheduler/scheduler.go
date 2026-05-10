// Package scheduler provides the Scheduler specialist agent —
// the operator-facing interface to Retainer's autonomous-cycle
// scheduler. The actor that actually runs cron + dispatches to
// the cog lives in `internal/scheduler/`; this agent's tools are
// thin wrappers over its public API.
//
// V1 surface:
//   - schedule_job (recurring via cron OR one-shot via fire_at)
//   - list_jobs (active jobs sorted by next-fire ascending)
//   - inspect_job (single record with last/next fire times)
//   - cancel_job (deactivates an active job)
//
// Same shape as the researcher / observer / taskmaster: a
// react-loop agent with a focused tool set, no inter-agent
// chatter, restart strategy supplied by bootstrap.
package scheduler

import (
	"log/slog"

	"github.com/seamus-brady/retainer/internal/agent"
	"github.com/seamus-brady/retainer/internal/llm"
)

// Name is the machine-readable identifier (forms the delegate
// tool name `agent_scheduler`).
const Name = "scheduler"

// HumanName is the display name surfaced in logs and the
// sensorium.
const HumanName = "Scheduler"

// Description is the one-liner the cog's LLM sees on the
// `agent_scheduler` delegate tool.
const Description = "Schedule autonomous cycles (recurring via cron, or one-shot at a wall-clock time). " +
	"Use to add, list, inspect, or cancel scheduled prompts. The cog runs scheduled prompts as " +
	"non-interactive inputs; outputs flow into the cycle log + narrative like any other reply."

// MaxTurns is intentionally short. Scheduling operations are
// usually one-step (schedule X, list everything, cancel ID).
// Six gives the agent room to chain a list-then-cancel flow in
// one dispatch.
const MaxTurns = 6

// MaxTokens caps the per-call response budget. Mirrors
// taskmaster's setting; agent replies are concise.
const MaxTokens = 2048

// MaxConsecutiveErrors short-circuits on repeated dispatch
// failures — same as the other specialists.
const MaxConsecutiveErrors = 3

const systemPrompt = `You are the Scheduler agent. Your role is to manage autonomous cycles — prompts the cog runs at scheduled times — via the four tools below. You do not run the prompts yourself; the scheduler service runs them when their time arrives.

## When you are dispatched

The operator (via the cog) wants to add, list, inspect, or cancel a scheduled job. Pick the right tool, call it with the right arguments, and reply with what changed (or what's currently scheduled). Do not invent jobs the operator did not ask for.

## Your tools

- ` + "`schedule_job`" + ` — register a new job. Two modes:
  - Recurring: pass ` + "`cron`" + ` (a five-field expression like ` + "`0 9 * * MON`" + ` for "9am every Monday"). Omit ` + "`fire_at`" + `.
  - One-shot: pass ` + "`fire_at`" + ` (an RFC3339 / ISO 8601 timestamp in UTC). Omit ` + "`cron`" + `.

  Always pass ` + "`name`" + ` (short label) and ` + "`prompt`" + ` (the text the cog will run). ` + "`description`" + ` is optional context.

- ` + "`list_jobs`" + ` — return active jobs sorted by next-fire ascending. No arguments.

- ` + "`inspect_job`" + ` — return the full record for one job by ID. Use the ID returned by ` + "`list_jobs`" + ` or ` + "`schedule_job`" + `.

- ` + "`cancel_job`" + ` — deactivate an active job by ID. Optional ` + "`reason`" + ` is recorded for audit.

## Cron format

Five-field cron: minute, hour, day-of-month, month, day-of-week. Examples:

- ` + "`0 9 * * MON`" + ` — 9am every Monday
- ` + "`*/15 * * * *`" + ` — every 15 minutes
- ` + "`0 0 1 * *`" + ` — midnight on the first of each month
- ` + "`30 18 * * FRI`" + ` — 6:30pm every Friday

Day-of-week names: SUN, MON, TUE, WED, THU, FRI, SAT (case-insensitive).

## Output format

Concise plain-text reply tailored to what was asked. Include job IDs (full UUID for cancellations; short prefix is fine in lists), the cron expression / fire time, and the prompt the cycle will run. For ` + "`list_jobs`" + ` with no jobs, say so plainly.

## Self-check

If the operator's request is not a scheduling operation (e.g. "search the web", "check my memory"), say so plainly in one short sentence. Do not invent a tool you don't have.`

// New constructs a Scheduler agent. The dispatcher must include
// the four scheduler tools wired against a running
// `*scheduler.Service`. Pass `agent.Telemetry{}` zero value to
// run silently (legacy / test path); production wires the
// workspace's cycle-log + DAG + instance ID.
func New(provider llm.Provider, model string, tools agent.ToolDispatcher, telemetry agent.Telemetry, logger *slog.Logger) (*agent.Agent, error) {
	spec := agent.Spec{
		Name:                 Name,
		HumanName:            HumanName,
		Description:          Description,
		SystemPrompt:         systemPrompt,
		Provider:             provider,
		Model:                model,
		MaxTokens:            MaxTokens,
		MaxTurns:             MaxTurns,
		MaxConsecutiveErrors: MaxConsecutiveErrors,
		Tools:                tools,
	}
	telemetry.ApplyTo(&spec)
	return agent.New(spec, logger)
}
