package metalearning

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/seamus-brady/retainer/internal/cbr"
	"github.com/seamus-brady/retainer/internal/librarian"
	"github.com/seamus-brady/retainer/internal/remembrancer"
)

// weeklyConsolidationWindow is the lookback for the weekly worker.
// Matches the worker's Interval: each run covers the 7 days before
// `now`. Decoupled from the Interval constant so tests can override
// in isolation.
const weeklyConsolidationWindow = 7 * 24 * time.Hour

// weeklyConsolidationPeriod is the period label used in the report
// filename + audit record. Stable string so reports group cleanly
// when the operator browses data/reports/.
const weeklyConsolidationPeriod = "weekly"

// weeklyConsolidationTopicSamples bounds how many sample summaries
// we include per topic in the heuristic body. Keeps the report
// readable; deeper detail is one `deep_search` call away.
const weeklyConsolidationTopicSamples = 3

// WeeklyConsolidation is the worker function for the
// `weekly_consolidation` worker. Reads narrative + facts + cases for
// the last 7 days, builds a heuristic markdown digest (top topics +
// counts + sample excerpts), and persists it as
// data/reports/YYYY-MM-DD-weekly.md.
//
// V1 deliberately does NO LLM call. The body is a structured
// summary the agent can later read via deep_search. LLM-driven
// synthesis (where the worker calls a Provider to write narrative
// prose over the gathered material) is the intended follow-up;
// the spec at doc/specs/dream-cycle-metalearning.md flags this.
//
// When the run produces zero entries (cold workspace, week with no
// activity), the function returns nil without writing a report —
// the operator gets no empty file. The worker's last_run_at still
// advances so the next run is one full week later.
func WeeklyConsolidation(ctx context.Context, deps Deps) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	end := deps.NowFn()
	start := end.Add(-weeklyConsolidationWindow)

	narrative, err := remembrancer.ReadNarrative(deps.DataDir, start, end, deps.Logger)
	if err != nil {
		return fmt.Errorf("read narrative: %w", err)
	}
	facts, err := remembrancer.ReadFacts(deps.DataDir, start, end, deps.Logger)
	if err != nil {
		return fmt.Errorf("read facts: %w", err)
	}
	cases, err := remembrancer.ReadCases(deps.DataDir, deps.Logger)
	if err != nil {
		return fmt.Errorf("read cases: %w", err)
	}
	cases = casesInWindow(cases, start, end)

	if len(narrative) == 0 && len(facts) == 0 && len(cases) == 0 {
		deps.Logger.Debug("weekly_consolidation: empty week; no report written",
			"start", start.Format("2006-01-02"), "end", end.Format("2006-01-02"))
		return nil
	}

	body := renderWeeklyBody(start, end, narrative, facts, cases)
	title := fmt.Sprintf("Weekly review %s – %s",
		start.Format("2006-01-02"), end.Format("2006-01-02"))
	slug := remembrancer.SlugifyTitle(title)
	dateStr := end.Format("2006-01-02")

	reportPath, err := remembrancer.WriteReport(deps.DataDir, dateStr, slug, body)
	if err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	run := remembrancer.Run{
		Timestamp:  end,
		Period:     weeklyConsolidationPeriod,
		StartDate:  start.Format("2006-01-02"),
		EndDate:    dateStr,
		ReportPath: reportPath,
		Stats: remembrancer.RunStats{
			NarrativeEntries: len(narrative),
			Facts:            len(facts),
			Cases:            len(cases),
		},
	}
	if _, err := remembrancer.WriteRun(deps.DataDir, run); err != nil {
		// Audit-log failure is non-fatal — the report itself
		// landed. Log + continue so the worker's success state
		// reflects the user-visible artifact.
		deps.Logger.Warn("weekly_consolidation: audit log append failed",
			"err", err, "report", reportPath)
	}
	deps.Logger.Info("weekly_consolidation: report written",
		"path", reportPath,
		"narrative", len(narrative),
		"facts", len(facts),
		"cases", len(cases),
	)
	return nil
}

// casesInWindow filters the case archive to those whose Timestamp
// falls inside [start, end]. The cases JSONL doesn't ship a per-day
// reader (cases are global), so we filter post-read.
func casesInWindow(in []cbr.Case, start, end time.Time) []cbr.Case {
	out := make([]cbr.Case, 0, len(in))
	for _, c := range in {
		if c.Timestamp.IsZero() {
			continue
		}
		if c.Timestamp.Before(start) || c.Timestamp.After(end) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// renderWeeklyBody builds the markdown body for the weekly report.
// Heuristic-only — counts, top topics, sample summaries. The agent
// later reads the report via deep_search; this body is structured
// enough for that pattern to find the right report quickly.
//
// Layout (kept stable for deep_search reliability):
//
//	# Weekly review YYYY-MM-DD – YYYY-MM-DD
//	## Counts
//	- N narrative entries
//	- N facts
//	- N cases
//	## Top topics
//	- topic: N entries
//	## Sample entries
//	- ISO timestamp — summary (one line)
//	## Cases
//	- ISO — domain — intent → status (confidence)
func renderWeeklyBody(start, end time.Time, narrative []librarian.NarrativeEntry, facts []librarian.Fact, cases []cbr.Case) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Weekly review %s – %s\n\n",
		start.Format("2006-01-02"), end.Format("2006-01-02"))

	b.WriteString("## Counts\n\n")
	fmt.Fprintf(&b, "- %d narrative entries\n", len(narrative))
	fmt.Fprintf(&b, "- %d facts\n", len(facts))
	fmt.Fprintf(&b, "- %d cases\n\n", len(cases))

	if topics := topTopics(narrative, 5); len(topics) > 0 {
		b.WriteString("## Top topics\n\n")
		for _, t := range topics {
			fmt.Fprintf(&b, "- %s: %d entries\n", t.label, t.count)
		}
		b.WriteString("\n")
	}

	if samples := topSampleEntries(narrative, weeklyConsolidationTopicSamples); len(samples) > 0 {
		b.WriteString("## Sample entries\n\n")
		for _, s := range samples {
			fmt.Fprintf(&b, "- %s — %s\n",
				s.Timestamp.Format(time.RFC3339), oneLine(s.Summary, 200))
		}
		b.WriteString("\n")
	}

	if len(cases) > 0 {
		b.WriteString("## Cases\n\n")
		caseSamples := cases
		if len(caseSamples) > weeklyConsolidationTopicSamples {
			caseSamples = caseSamples[len(caseSamples)-weeklyConsolidationTopicSamples:]
		}
		for _, c := range caseSamples {
			fmt.Fprintf(&b, "- %s — %s — %s → %s (%.2f)\n",
				c.Timestamp.Format(time.RFC3339),
				orDash(c.Problem.Domain),
				orDash(c.Problem.Intent),
				string(c.Outcome.Status),
				c.Outcome.Confidence,
			)
		}
		b.WriteString("\n")
	}

	return strings.TrimRight(b.String(), "\n") + "\n"
}

// topicCount is a small (label, count) pair sortable by count desc.
type topicCount struct {
	label string
	count int
}

// topTopics tallies narrative entries by Domain (when set) +
// returns the top-N labels by count. Cheap O(n) pass; n is the
// week's narrative count, typically tens to low hundreds.
func topTopics(entries []librarian.NarrativeEntry, n int) []topicCount {
	counts := make(map[string]int)
	for _, e := range entries {
		domain := strings.TrimSpace(e.Intent.Domain)
		if domain == "" {
			continue
		}
		counts[domain]++
	}
	if len(counts) == 0 {
		return nil
	}
	out := make([]topicCount, 0, len(counts))
	for label, c := range counts {
		out = append(out, topicCount{label: label, count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].label < out[j].label
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// topSampleEntries picks the most-recent N narrative entries with a
// non-empty summary. The remembrancer's reader returns entries in
// chronological order (oldest first), so we read from the tail.
func topSampleEntries(entries []librarian.NarrativeEntry, n int) []librarian.NarrativeEntry {
	out := make([]librarian.NarrativeEntry, 0, n)
	for i := len(entries) - 1; i >= 0 && len(out) < n; i-- {
		if strings.TrimSpace(entries[i].Summary) == "" {
			continue
		}
		out = append(out, entries[i])
	}
	return out
}

// oneLine collapses whitespace + truncates to max chars so a multi-
// paragraph summary lands as a single readable bullet.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// orDash returns "—" for empty strings so columns stay aligned in
// the bullet output. Cheap presentation polish.
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
