package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/seamus-brady/retainer/internal/librarian"
	"github.com/seamus-brady/retainer/internal/llm"
	"github.com/seamus-brady/retainer/internal/remembrancer"
)

// remembrancerSearchDefaultLimit caps the deep_search response so
// the agent's context window doesn't drown in match-floods. Operator
// tooling that wants the full archive should read JSONL directly.
const remembrancerSearchDefaultLimit = 20
const remembrancerSearchMaxLimit = 100

// RemembrancerDeps is the slice of state the remembrancer tools
// need: the workspace data dir (for direct JSONL reads + writes)
// and a logger for read-side debug logs.
//
// Each tool struct embeds *RemembrancerDeps so main.go can wire
// once and pass the same deps to every tool.
type RemembrancerDeps struct {
	DataDir string
	Logger  *slog.Logger
}

// ---------------------------------------------------------------------------
// deep_search
// ---------------------------------------------------------------------------

type deepSearchInput struct {
	Query     string `json:"query"`
	StartDate string `json:"start_date,omitempty"` // YYYY-MM-DD
	EndDate   string `json:"end_date,omitempty"`   // YYYY-MM-DD
	Limit     int    `json:"limit,omitempty"`
}

// DeepSearch reaches across the FULL narrative archive (bypasses
// the librarian's 60-day hot index) for keyword matches. Use when
// the agent needs to find something older than recall_recent's
// window can see.
type DeepSearch struct{ Deps *RemembrancerDeps }

func (DeepSearch) Tool() llm.Tool {
	return llm.Tool{
		Name: "deep_search",
		Description: "Search the full narrative archive (months / years of cycles) for keyword matches. " +
			"Bypasses recall_recent's 60-day window — use when you need to find something older. " +
			"Optional date bounds narrow the scan; default limit is 20, max 100.",
		InputSchema: llm.Schema{
			Name: "deep_search",
			Properties: map[string]llm.Property{
				"query": {
					Type:        "string",
					Description: "Space-separated keywords. Matches case-insensitively in narrative summaries, keywords, and domain.",
				},
				"start_date": {Type: "string", Description: "Optional inclusive lower bound (YYYY-MM-DD)."},
				"end_date":   {Type: "string", Description: "Optional inclusive upper bound (YYYY-MM-DD)."},
				"limit":      {Type: "integer", Description: fmt.Sprintf("Max matches (1–%d, default %d).", remembrancerSearchMaxLimit, remembrancerSearchDefaultLimit)},
			},
			Required: []string{"query"},
		},
	}
}

func (h DeepSearch) Execute(_ context.Context, input []byte) (string, error) {
	if len(input) == 0 {
		return "", fmt.Errorf("deep_search: empty input")
	}
	var in deepSearchInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("deep_search: decode input: %w", err)
	}
	keywords := splitKeywords(in.Query)
	if len(keywords) == 0 {
		return "", fmt.Errorf("deep_search: query must contain at least one keyword")
	}
	start, end, err := parseDateBounds(in.StartDate, in.EndDate)
	if err != nil {
		return "", fmt.Errorf("deep_search: %w", err)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = remembrancerSearchDefaultLimit
	}
	if limit > remembrancerSearchMaxLimit {
		limit = remembrancerSearchMaxLimit
	}

	entries, err := remembrancer.ReadNarrative(h.Deps.DataDir, start, end, h.Deps.Logger)
	if err != nil {
		return "", fmt.Errorf("deep_search: read narrative: %w", err)
	}
	matches := remembrancer.Search(entries, keywords)
	if len(matches) == 0 {
		return "no matches", nil
	}
	if len(matches) > limit {
		matches = matches[len(matches)-limit:] // most-recent N
	}
	return formatNarrativeMatches(matches), nil
}

// ---------------------------------------------------------------------------
// find_connections
// ---------------------------------------------------------------------------

type findConnectionsInput struct {
	Topic string `json:"topic"`
}

// FindConnections cross-references a topic across narrative + facts
// + cases. Returns counts + 2-3 samples per store — the agent uses
// it to orient before deciding which store to drill into.
type FindConnections struct{ Deps *RemembrancerDeps }

func (FindConnections) Tool() llm.Tool {
	return llm.Tool{
		Name: "find_connections",
		Description: "Cross-reference a topic across narrative, facts, and CBR cases. Returns hit counts " +
			"and 2-3 sample entries per store (most-recent first). Use to orient when you don't know " +
			"yet which memory store has the answer.",
		InputSchema: llm.Schema{
			Name: "find_connections",
			Properties: map[string]llm.Property{
				"topic": {Type: "string", Description: "The topic / keyword to cross-reference."},
			},
			Required: []string{"topic"},
		},
	}
}

func (h FindConnections) Execute(_ context.Context, input []byte) (string, error) {
	if len(input) == 0 {
		return "", fmt.Errorf("find_connections: empty input")
	}
	var in findConnectionsInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("find_connections: decode input: %w", err)
	}
	topic := strings.TrimSpace(in.Topic)
	if topic == "" {
		return "", fmt.Errorf("find_connections: topic must not be empty")
	}

	narrative, err := remembrancer.ReadNarrative(h.Deps.DataDir, time.Time{}, time.Time{}, h.Deps.Logger)
	if err != nil {
		return "", fmt.Errorf("find_connections: read narrative: %w", err)
	}
	facts, err := remembrancer.ReadFacts(h.Deps.DataDir, time.Time{}, time.Time{}, h.Deps.Logger)
	if err != nil {
		return "", fmt.Errorf("find_connections: read facts: %w", err)
	}
	cases, err := remembrancer.ReadCases(h.Deps.DataDir, h.Deps.Logger)
	if err != nil {
		return "", fmt.Errorf("find_connections: read cases: %w", err)
	}

	r := remembrancer.FindConnections(narrative, facts, cases, topic)
	return formatConnectionsResult(r), nil
}

// mine_patterns / consolidate_memory / write_consolidation_report
// retired 2026-05-04. Pattern mining + consolidation now run on
// the metalearning pool's tick (internal/metalearning/), persisting
// durable artifacts to data/patterns/ + data/knowledge/consolidation/.
// The agent reads them via deep_search.

// ---------------------------------------------------------------------------
// formatters
// ---------------------------------------------------------------------------

func splitKeywords(query string) []string {
	out := []string{}
	for _, w := range strings.Fields(query) {
		w = strings.TrimSpace(w)
		if w != "" {
			out = append(out, w)
		}
	}
	return out
}

func parseDateBounds(start, end string) (time.Time, time.Time, error) {
	var startT, endT time.Time
	if start != "" {
		t, err := time.Parse("2006-01-02", start)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("start_date %q must be YYYY-MM-DD", start)
		}
		startT = t
	}
	if end != "" {
		t, err := time.Parse("2006-01-02", end)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("end_date %q must be YYYY-MM-DD", end)
		}
		// Treat end as inclusive of the whole UTC day.
		endT = t.Add(24*time.Hour - time.Nanosecond)
	}
	return startT, endT, nil
}

func formatNarrativeMatches(entries []librarian.NarrativeEntry) string {
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "%s [%s] %s — %s\n",
			e.Timestamp.Format("2006-01-02 15:04"),
			e.Status,
			shortCycleID(e.CycleID),
			truncateInline(strings.ReplaceAll(e.Summary, "\n", " "), 200),
		)
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatConnectionsResult(r remembrancer.ConnectionsResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "narrative: %d hits\n", r.Counts.Narrative)
	for _, e := range r.NarrativeSamples {
		fmt.Fprintf(&b, "  - %s [%s] %s — %s\n",
			e.Timestamp.Format("2006-01-02"),
			e.Status,
			shortCycleID(e.CycleID),
			truncateInline(e.Summary, 120),
		)
	}
	fmt.Fprintf(&b, "facts: %d hits\n", r.Counts.Facts)
	for _, f := range r.FactSamples {
		fmt.Fprintf(&b, "  - %s = %s (confidence %.2f)\n", f.Key, truncateInline(f.Value, 80), f.Confidence)
	}
	fmt.Fprintf(&b, "cases: %d hits\n", r.Counts.Cases)
	for _, c := range r.CaseSamples {
		intent := c.Problem.Intent
		if intent == "" {
			intent = "(no intent)"
		}
		fmt.Fprintf(&b, "  - %s [%s] — %s\n", shortCycleID(c.ID), c.Category, truncateInline(intent, 120))
	}
	return strings.TrimRight(b.String(), "\n")
}

