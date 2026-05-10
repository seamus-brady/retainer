package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/seamus-brady/retainer/internal/agent"
	"github.com/seamus-brady/retainer/internal/cyclelog"
	"github.com/seamus-brady/retainer/internal/llm"
)

// DelegateToAgent surfaces one specialist agent as a tool the cog's
// LLM can call. Mirrors Springdrift's `agent_<name>` tool shape from
// `_impl_docs/ref/springdrift/src/agent/types.gleam:agent_to_tool` —
// V1 keeps just the load-bearing parameters (`instruction` required,
// `context` optional). Springdrift's deferred params (artifact_id,
// referenced_artifacts, task_id, prior_cycle_id, draft_slug) wait on
// the subsystems they reference (artifacts, planner, writer drafts).
//
// One handler per agent — the cog gets `agent_researcher`,
// `agent_observer`, etc., as separate tools so the LLM can choose
// among them by name + description.
type DelegateToAgent struct {
	// Agent is the specialist this tool delegates to.
	Agent *agent.Agent

	// OnDone, when non-nil, is called once per completed
	// dispatch with the cog's parent cycle ID + a structured
	// CompletionRecord built from the agent's Outcome. The
	// archivist consumes these records via CycleComplete →
	// CurationInput so the curator can ground its outcome
	// assessment in the agent's INTERNAL tool log, not just
	// "agent_<name> returned ok".
	//
	// Wired by bootstrap to push into the cog's accumulator.
	// Optional: tests + the legacy in-process path leave it
	// nil and the dispatch behaves as before.
	OnDone func(parentCycleID string, rec agent.CompletionRecord)
}

// Tool builds an llm.Tool for this agent. Name is `agent_<spec.Name>`
// (Springdrift convention); description is the agent's own
// description so the cog's LLM can pick the right specialist for a
// given task.
func (d DelegateToAgent) Tool() llm.Tool {
	return llm.Tool{
		Name:        agentToolName(d.Agent.Name()),
		Description: agentToolDescription(d.Agent),
		InputSchema: llm.Schema{
			Name: agentToolName(d.Agent.Name()),
			Properties: map[string]llm.Property{
				"instruction": {
					Type:        "string",
					Description: "Task for the agent. Be specific — what should it find/do/produce.",
				},
				"context": {
					Type:        "string",
					Description: "Relevant context the agent should know (optional).",
				},
			},
			Required: []string{"instruction"},
		},
	}
}

// Execute decodes the input, dispatches the task to the agent, waits
// for the Outcome, and formats the result for the cog's LLM. Errors
// from the agent (max_turns, max errors, etc.) become tool errors so
// the cog's react loop can recover.
func (d DelegateToAgent) Execute(ctx context.Context, input []byte) (string, error) {
	if len(input) == 0 {
		return "", fmt.Errorf("%s: empty input", agentToolName(d.Agent.Name()))
	}
	var in delegateInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("%s: decode input: %w", agentToolName(d.Agent.Name()), err)
	}
	in.Instruction = strings.TrimSpace(in.Instruction)
	if in.Instruction == "" {
		return "", fmt.Errorf("%s: instruction must not be empty", agentToolName(d.Agent.Name()))
	}
	prompt := in.Instruction
	if ctx := strings.TrimSpace(in.Context); ctx != "" {
		prompt = "Context: " + ctx + "\n\nInstruction: " + in.Instruction
	}

	parentCycleID := cyclelog.CycleIDFromContext(ctx)
	reply := make(chan agent.Outcome, 1)
	if err := d.Agent.Submit(ctx, agent.Task{
		Instruction:   prompt,
		ParentCycleID: parentCycleID,
		Reply:         reply,
	}); err != nil {
		return "", fmt.Errorf("%s: submit: %w", agentToolName(d.Agent.Name()), err)
	}

	select {
	case out := <-reply:
		// Build + emit the completion record before returning the
		// tool result. The cog accumulates these per cycle; the
		// archivist threads them into CurationInput so the curator
		// has the agent's internal tool log as ground truth.
		if d.OnDone != nil {
			d.OnDone(parentCycleID, agent.CompletionRecord{
				AgentName:    d.Agent.Name(),
				AgentCycleID: out.AgentCycleID,
				Instruction:  truncateInstruction(in.Instruction),
				OutcomeText:  out.Result,
				Success:      out.IsSuccess(),
				ErrorMessage: errToString(out.Err),
				ToolsUsed:    out.ToolsUsed,
				InputTokens:  out.InputTokens,
				OutputTokens: out.OutputTokens,
				Duration:     out.Duration,
			})
		}
		if !out.IsSuccess() {
			// Agent-level failure: surface as a tool error so the cog
			// can decide whether to retry or fail the cycle.
			return "", fmt.Errorf("%s: %w", agentToolName(d.Agent.Name()), out.Err)
		}
		return out.Result, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// truncateInstruction caps the brief stored on the completion
// record. The full instruction is in the cycle log; this is a
// preview the curator's prompt can format inline without bloating
// the prompt budget.
func truncateInstruction(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// errToString returns the string form of err, or "" when nil.
func errToString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type delegateInput struct {
	Instruction string `json:"instruction"`
	Context     string `json:"context,omitempty"`
}

// agentToolName is `agent_<name>` per Springdrift.
func agentToolName(name string) string {
	return "agent_" + name
}

// agentToolDescription returns the agent's own description, falling
// back to a generic stub when empty (defensive — a registered agent
// should always have one).
func agentToolDescription(a *agent.Agent) string {
	d := a.Description()
	if d == "" {
		return "Delegate to the " + a.HumanName() + " agent."
	}
	return d
}
