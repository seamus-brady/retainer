package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/seamus-brady/retainer/internal/cyclelog"
	"github.com/seamus-brady/retainer/internal/llm"
)

// requestMoreTurnsDefaultIncrement is the default budget bump
// when the LLM doesn't specify how many additional turns it
// needs. Five matches the previous default cap — one more
// "round" of consultation+act before the next check.
const requestMoreTurnsDefaultIncrement = 5

// requestMoreTurnsMaxIncrement caps the per-call increment so a
// single tool invocation can't jump from the start cap straight
// to the hard ceiling. Combined with the cog-side
// hardMaxToolTurns guard, an LLM that's struggling can extend
// gradually rather than all at once.
const requestMoreTurnsMaxIncrement = 10

// CogTurnExtender is the slice of *cog.Cog the
// request_more_turns tool needs. Letting the tool depend on
// this interface keeps tests independent of the cog's full
// goroutine + inbox plumbing — same shape pattern as
// ObserverService / StrategyService.
type CogTurnExtender interface {
	RequestMoreTurns(parentCycleID string, additional int, reason string) (int, error)
}

// RequestMoreTurns is the cog-side tool that lets the LLM
// extend its own tool-turn budget for the current cycle when
// the work it's doing genuinely needs more turns than the
// configured default. The cog enforces a hard ceiling
// (hardMaxToolTurns, currently 20) — calls past the ceiling
// fail with a clear message and the cycle abandons normally if
// the LLM keeps trying.
//
// Use sparingly. Routine work fits well within the default cap;
// reach for this when:
//   - a diagnostic or stress sequence has more steps than the
//     default budget allows
//   - a delegation chain is unusually deep and the cog needs
//     extra turns to summarise across N specialist replies
//   - a self-check loop (e.g. "verify with a tool, then act")
//     is bumping the cap on a normally-short flow
//
// Reason is required and surfaces in cycle-log audit so
// operators can see when the cog needed headroom and why.
type RequestMoreTurns struct {
	Cog CogTurnExtender
}

type requestMoreTurnsInput struct {
	Additional int    `json:"additional,omitempty"`
	Reason     string `json:"reason"`
}

func (RequestMoreTurns) Tool() llm.Tool {
	return llm.Tool{
		Name: "request_more_turns",
		Description: "Extend the current cycle's tool-turn budget. Use when the work in flight needs more " +
			"sequential tool calls than the default cap allows (e.g. a diagnostic walking multiple agents, " +
			"a deep delegation chain, or a self-check loop). Reason is required and recorded for audit. " +
			fmt.Sprintf("Default increment is %d turns; the per-call max is %d. ", requestMoreTurnsDefaultIncrement, requestMoreTurnsMaxIncrement) +
			"The cog enforces a hard ceiling — past it, the cycle ends normally.",
		InputSchema: llm.Schema{
			Name: "request_more_turns",
			Properties: map[string]llm.Property{
				"additional": {
					Type:        "integer",
					Description: fmt.Sprintf("Turns to add (1–%d). Default %d.", requestMoreTurnsMaxIncrement, requestMoreTurnsDefaultIncrement),
				},
				"reason": {
					Type:        "string",
					Description: "Why the extra budget is needed. Recorded for audit.",
				},
			},
			Required: []string{"reason"},
		},
	}
}

func (h RequestMoreTurns) Execute(ctx context.Context, input []byte) (string, error) {
	if h.Cog == nil {
		return "", fmt.Errorf("request_more_turns: cog is not configured")
	}
	if len(input) == 0 {
		return "", fmt.Errorf("request_more_turns: empty input")
	}
	var in requestMoreTurnsInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("request_more_turns: decode input: %w", err)
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		return "", fmt.Errorf("request_more_turns: reason must not be empty")
	}
	additional := in.Additional
	if additional <= 0 {
		additional = requestMoreTurnsDefaultIncrement
	}
	if additional > requestMoreTurnsMaxIncrement {
		additional = requestMoreTurnsMaxIncrement
	}
	parentCycleID := cyclelog.CycleIDFromContext(ctx)
	newCap, err := h.Cog.RequestMoreTurns(parentCycleID, additional, reason)
	if err != nil {
		return "", fmt.Errorf("request_more_turns: %w", err)
	}
	return fmt.Sprintf("tool-turn cap extended to %d (added %d). Reason: %s",
		newCap, additional, reason200Inline(reason)), nil
}

// reason200Inline truncates the reason for the response message
// (kept tighter than the cycle-log version since this round-trips
// to the LLM and counts toward the next system prompt).
func reason200Inline(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
