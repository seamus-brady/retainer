package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/seamus-brady/retainer/internal/librarian"
	"github.com/seamus-brady/retainer/internal/llm"
	"github.com/seamus-brady/retainer/internal/observer"
)

// observerRecallDefaultLimit is what `recall_recent` returns when the
// model doesn't specify a limit. Mirrors the TUI's /recall default.
const observerRecallDefaultLimit = 5

// observerRecallMaxLimit caps the recall_recent response — protects
// the agent's context window and the prompt's token budget. Above
// this, the model should narrow its query.
const observerRecallMaxLimit = 50

// ObserverService is the slice of *observer.Observer the agent's
// tools need. Lets tests substitute a fake without spinning up a
// real librarian + DAG.
type ObserverService interface {
	RecentCycles(limit int) []librarian.NarrativeEntry
	InspectCycle(cycleID string) observer.CycleInspection
	GetFact(key string) *librarian.Fact
}

// ---------------------------------------------------------------------------
// inspect_cycle
// ---------------------------------------------------------------------------

type observerInspectInput struct {
	CycleID string `json:"cycle_id"`
}

// ObserverInspectCycle exposes observer.Observer.InspectCycle to the
// agent. The model passes a full cycle ID or a short prefix — the
// handler resolves the prefix against recent cycles before falling
// back to "not found".
type ObserverInspectCycle struct{ Observer ObserverService }

func (ObserverInspectCycle) Tool() llm.Tool {
	return llm.Tool{
		Name: "inspect_cycle",
		Description: "Look up DAG status + narrative summary + duration + error (if any) for a single cycle by " +
			"its ID. Accepts either a full UUID or a short prefix (8+ chars) — the prefix is resolved against " +
			"recent cycles. Use to drill into what happened during a specific past cycle.",
		InputSchema: llm.Schema{
			Name: "inspect_cycle",
			Properties: map[string]llm.Property{
				"cycle_id": {
					Type:        "string",
					Description: "Cycle ID or 8+ char prefix.",
				},
			},
			Required: []string{"cycle_id"},
		},
	}
}

func (h ObserverInspectCycle) Execute(_ context.Context, input []byte) (string, error) {
	if len(input) == 0 {
		return "", fmt.Errorf("inspect_cycle: empty input")
	}
	var in observerInspectInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("inspect_cycle: decode input: %w", err)
	}
	want := strings.TrimSpace(in.CycleID)
	if want == "" {
		return "", fmt.Errorf("inspect_cycle: cycle_id must not be empty")
	}

	// Try a direct lookup first; if not found, resolve a prefix
	// against recent cycles. Mirrors the TUI's /inspect logic.
	insp := h.Observer.InspectCycle(want)
	if !insp.Found {
		for _, e := range h.Observer.RecentCycles(500) {
			if strings.HasPrefix(e.CycleID, want) {
				insp = h.Observer.InspectCycle(e.CycleID)
				break
			}
		}
	}
	if !insp.Found {
		return "cycle not found: " + want, nil
	}
	return formatCycleInspection(insp), nil
}

// formatCycleInspection produces an LLM-friendly multi-line summary
// of one cycle. Field order matches the TUI's /inspect output so
// operator-side and agent-side views stay consistent. Phase 2C
// extends the format with the rich narrative fields (intent, outcome,
// delegation chain, metrics, strategy) when present; legacy entries
// just skip those lines via the IsZero guards.
func formatCycleInspection(insp observer.CycleInspection) string {
	var b strings.Builder
	fmt.Fprintf(&b, "cycle:    %s\n", insp.CycleID)
	if insp.Type != "" {
		fmt.Fprintf(&b, "type:     %s\n", insp.Type)
	}
	if insp.Status != "" {
		fmt.Fprintf(&b, "status:   %s\n", insp.Status)
	}
	if !insp.StartedAt.IsZero() {
		fmt.Fprintf(&b, "started:  %s\n", insp.StartedAt.Format(time.RFC3339))
	}
	if insp.Duration > 0 {
		fmt.Fprintf(&b, "duration: %s\n", insp.Duration.Round(time.Millisecond))
	}
	if insp.ErrorMessage != "" {
		fmt.Fprintf(&b, "error:    %s\n", insp.ErrorMessage)
	}
	if !insp.Intent.IsZero() {
		if insp.Intent.Classification != "" {
			fmt.Fprintf(&b, "intent:   %s", insp.Intent.Classification)
			if insp.Intent.Domain != "" {
				fmt.Fprintf(&b, " (%s)", insp.Intent.Domain)
			}
			b.WriteString("\n")
		} else if insp.Intent.Domain != "" {
			fmt.Fprintf(&b, "intent:   (%s)\n", insp.Intent.Domain)
		}
		if insp.Intent.Description != "" {
			fmt.Fprintf(&b, "  desc:   %s\n", insp.Intent.Description)
		}
	}
	if !insp.Outcome.IsZero() {
		fmt.Fprintf(&b, "outcome:  %s", insp.Outcome.Status)
		if insp.Outcome.Confidence > 0 {
			fmt.Fprintf(&b, " (confidence %.2f)", insp.Outcome.Confidence)
		}
		b.WriteString("\n")
		if insp.Outcome.Assessment != "" {
			fmt.Fprintf(&b, "  assess: %s\n", insp.Outcome.Assessment)
		}
	}
	if len(insp.Topics) > 0 {
		fmt.Fprintf(&b, "topics:   %s\n", strings.Join(insp.Topics, ", "))
	}
	if len(insp.Keywords) > 0 {
		fmt.Fprintf(&b, "keywords: %s\n", strings.Join(insp.Keywords, ", "))
	}
	if !insp.Entities.IsZero() {
		if len(insp.Entities.Locations) > 0 {
			fmt.Fprintf(&b, "  locs:   %s\n", strings.Join(insp.Entities.Locations, ", "))
		}
		if len(insp.Entities.Organisations) > 0 {
			fmt.Fprintf(&b, "  orgs:   %s\n", strings.Join(insp.Entities.Organisations, ", "))
		}
	}
	if len(insp.DelegationChain) > 0 {
		b.WriteString("delegations:\n")
		for _, d := range insp.DelegationChain {
			fmt.Fprintf(&b, "  - %s", d.Agent)
			if d.AgentCycleID != "" {
				fmt.Fprintf(&b, " (%s)", shortCycleID(d.AgentCycleID))
			}
			b.WriteString("\n")
			if d.Instruction != "" {
				fmt.Fprintf(&b, "    instr:  %s\n", truncateInline(d.Instruction, 200))
			}
			if d.OutcomeText != "" {
				fmt.Fprintf(&b, "    out:    %s\n", truncateInline(strings.ReplaceAll(d.OutcomeText, "\n", " "), 200))
			}
			if len(d.ToolsUsed) > 0 {
				fmt.Fprintf(&b, "    tools:  %s\n", strings.Join(d.ToolsUsed, ", "))
			}
		}
	}
	if !insp.Metrics.IsZero() {
		b.WriteString("metrics: ")
		var parts []string
		if insp.Metrics.TotalDurationMs > 0 {
			parts = append(parts, fmt.Sprintf("%dms", insp.Metrics.TotalDurationMs))
		}
		if insp.Metrics.InputTokens > 0 || insp.Metrics.OutputTokens > 0 {
			parts = append(parts, fmt.Sprintf("tokens=%d/%d", insp.Metrics.InputTokens, insp.Metrics.OutputTokens))
		}
		if insp.Metrics.ToolCalls > 0 {
			parts = append(parts, fmt.Sprintf("tools=%d", insp.Metrics.ToolCalls))
		}
		if insp.Metrics.AgentDelegations > 0 {
			parts = append(parts, fmt.Sprintf("delegations=%d", insp.Metrics.AgentDelegations))
		}
		if insp.Metrics.ModelUsed != "" {
			parts = append(parts, "model="+insp.Metrics.ModelUsed)
		}
		b.WriteString(strings.Join(parts, " "))
		b.WriteString("\n")
	}
	if insp.Summary != "" {
		b.WriteString("---\n")
		b.WriteString(insp.Summary)
	}
	return strings.TrimRight(b.String(), "\n")
}

// ---------------------------------------------------------------------------
// recall_recent
// ---------------------------------------------------------------------------

type observerRecallInput struct {
	Limit int `json:"limit,omitempty"`
}

// ObserverRecallRecent surfaces the librarian's most-recent narrative
// entries — same data the TUI's /recall command shows. Useful when
// the agent needs to remember "what did I just do."
type ObserverRecallRecent struct{ Observer ObserverService }

func (ObserverRecallRecent) Tool() llm.Tool {
	return llm.Tool{
		Name: "recall_recent",
		Description: "List the most-recent narrative entries (oldest in batch first), each showing timestamp, " +
			"status, short cycle ID prefix, and summary. Use to remember what just happened in this session " +
			"or in recent ones. Default limit is 5; max is 50.",
		InputSchema: llm.Schema{
			Name: "recall_recent",
			Properties: map[string]llm.Property{
				"limit": {
					Type:        "integer",
					Description: fmt.Sprintf("Max entries to return (1–%d, default %d).", observerRecallMaxLimit, observerRecallDefaultLimit),
				},
			},
			Required: []string{},
		},
	}
}

func (h ObserverRecallRecent) Execute(_ context.Context, input []byte) (string, error) {
	var in observerRecallInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return "", fmt.Errorf("recall_recent: decode input: %w", err)
		}
	}
	limit := in.Limit
	if limit <= 0 {
		limit = observerRecallDefaultLimit
	}
	if limit > observerRecallMaxLimit {
		limit = observerRecallMaxLimit
	}
	entries := h.Observer.RecentCycles(limit)
	if len(entries) == 0 {
		return "no cycles recorded yet", nil
	}
	var b strings.Builder
	for _, e := range entries {
		// Phase 2C: prefer the rich Outcome.Status when present —
		// it's the curator-derived disposition (success/partial/
		// failure). Fall back to the legacy NarrativeStatus
		// (complete/blocked/error/abandoned) for old entries.
		status := string(e.Status)
		if e.Outcome.Status != "" {
			status = string(e.Outcome.Status)
		}
		fmt.Fprintf(&b, "%s [%s] %s",
			e.Timestamp.Format("15:04:05"),
			status,
			shortCycleID(e.CycleID),
		)
		// Surface intent classification when known — gives the
		// agent a one-glance category before reading the summary.
		if e.Intent.Classification != "" {
			fmt.Fprintf(&b, " <%s>", e.Intent.Classification)
		}
		fmt.Fprintf(&b, " — %s\n",
			truncateInline(strings.ReplaceAll(e.Summary, "\n", " "), 200),
		)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// ---------------------------------------------------------------------------
// get_fact
// ---------------------------------------------------------------------------

type observerGetFactInput struct {
	Key string `json:"key"`
}

// ObserverGetFact looks up a single fact by key with confidence
// decay applied. Covers the same surface as the cog's
// `memory_read` tool; the observer agent gets it too because
// fact-checking is a common observer task ("what did the cog
// remember about X?").
type ObserverGetFact struct{ Observer ObserverService }

func (ObserverGetFact) Tool() llm.Tool {
	return llm.Tool{
		Name: "get_fact",
		Description: "Read the current value of a stored fact by key. Returns the most-recent non-cleared " +
			"entry, with confidence decayed to now. Returns 'not found' when the key was never written or " +
			"has been cleared.",
		InputSchema: llm.Schema{
			Name: "get_fact",
			Properties: map[string]llm.Property{
				"key": {Type: "string", Description: "The fact key to look up."},
			},
			Required: []string{"key"},
		},
	}
}

func (h ObserverGetFact) Execute(_ context.Context, input []byte) (string, error) {
	if len(input) == 0 {
		return "", fmt.Errorf("get_fact: empty input")
	}
	var in observerGetFactInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("get_fact: decode input: %w", err)
	}
	key := strings.TrimSpace(in.Key)
	if key == "" {
		return "", fmt.Errorf("get_fact: key must not be empty")
	}
	f := h.Observer.GetFact(key)
	if f == nil {
		return fmt.Sprintf("No fact found for key %q", key), nil
	}
	return formatFact(*f), nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// shortCycleID returns the first 8 chars (UUID prefix) — same shape
// the TUI uses so operator-side and agent-side reads stay consistent.
func shortCycleID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func truncateInline(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
