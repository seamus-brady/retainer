package agent

import "time"

// CompletionRecord is the cog's view of one completed agent
// dispatch: the agent it called, the brief, the agent's reply,
// AND — load-bearing — the agent's internal tool list and token
// usage. The archivist's curator uses this to ground claim-vs-
// tool checks past the cog level: when the cog delegates to the
// researcher, the curator can see what tools the researcher
// itself fired, not just "agent_researcher returned ok".
//
// Shape ports SD's `narrative/types.gleam:DelegationStep` minus
// the fields that depend on subsystems we haven't built
// (sources_accessed needs an artifacts subsystem; data_points
// needs structured Entities). Everything that has data today is
// here.
type CompletionRecord struct {
	// AgentName is the machine-readable specialist name
	// ("researcher", "observer", "remembrancer").
	AgentName string
	// AgentCycleID is the agent's own cycle ID (TaskID), so the
	// curator can drill into the agent's own cycle log entries.
	AgentCycleID string
	// Instruction is the brief the cog sent. Truncated to a
	// reasonable size to keep prompt context lean.
	Instruction string
	// OutcomeText is the agent's final reply text. The cog
	// passes this back as a tool_result; the curator sees the
	// same text but joined with the structured fields here.
	OutcomeText string
	// Success reports whether the agent reported success
	// (Outcome.IsSuccess()). False on agent-level failures,
	// max_turns exhaustion, or transport errors.
	Success bool
	// ErrorMessage carries the failure reason when Success is
	// false. Empty on success.
	ErrorMessage string
	// ToolsUsed lists the tools the agent dispatched during
	// its react loop, in dispatch order, duplicates allowed.
	// This is the load-bearing field — without it the curator
	// can't ground claim-vs-tool checks past the cog level.
	ToolsUsed []string
	// InputTokens / OutputTokens are summed across the agent's
	// react-loop LLM calls. Drives the curator's cost picture
	// + future telemetry.
	InputTokens  int
	OutputTokens int
	// Duration is wall-clock time from agent.Submit to the
	// agent's Outcome reply landing back at the dispatch site.
	Duration time.Duration
}
