package curator

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/seamus-brady/retainer/internal/ambient"
	"github.com/seamus-brady/retainer/internal/cbr"
	"github.com/seamus-brady/retainer/internal/librarian"
)

// recalledCasesPerCategory caps how many cases each Category contributes
// to the rendered <recalled_cases> block. Bigger than 1 (so the agent
// sees alternatives within a category) but small enough that 5 categories
// can't blow the context budget alone.
const recalledCasesPerCategory = 2

// recalledCasesTotalCap is the overall limit on cases injected into the
// sensorium per cycle. SD/Memento's finding (paper 2510.04618) is that
// more than four retrieved cases starts poisoning model context — we
// honour that ceiling regardless of how many categories are populated.
const recalledCasesTotalCap = 4

// recalledCasesSummaryChars truncates each case's lesson + problem brief
// inline so a single high-volume case doesn't push the rest out.
const recalledCasesSummaryChars = 200

// Sensorium — the agent's ambient self-perception block, injected into the
// system prompt every cycle. Ported from Springdrift's
// narrative/curator.gleam build_sensorium pattern: a single XML block with
// many small sections, each rendered by its own function. Sections that
// would render empty (because their substrate isn't shipped yet) are
// scaffolded as functions that return "" — the section list is filtered
// for non-empty before joining.
//
// Substrate-portable in this slice (current Retainer): clock, situation,
// vitals (partial), memory, ambient (when cog.Notice() has been called).
//
// Substrate-deferred (return "" until the underlying subsystem ships):
// schedule (scheduler), tasks (planner), delegations (needs cog wiring),
// captures, strategies, learning_goals, affect_warnings, intray (comms),
// integrity (dprime cut), knowledge.
//
// The block lives in the Dynamic half of the SystemPrompt — it changes
// per cycle (clock, queue depth, recent activity), so it sits *after*
// the cache_control marker. The Stable half (persona + skills) is
// untouched by this slice.

// sensoriumInputs collects everything build_sensorium needs from outside
// the curator. Built fresh per buildPrompt call from CycleContext +
// CuratorConfig + librarian queries.
type sensoriumInputs struct {
	now            time.Time
	cycleID        string
	sessionSince   time.Time
	inputSource    string
	queueDepth     int
	messageCount   int
	agentsActive   int
	agents         []AgentInfo
	recentEntries  []librarian.NarrativeEntry
	narrativeCount int
	factCount      int
	caseCount      int
	ambient        []ambient.Signal
	// integrityFact carries the latest fabrication-audit summary
	// (key=integrity_suspect_replies_7d, value=JSON with
	// count/examined). Nil means the worker hasn't run yet on
	// this workspace; renderSensoriumIntegrity returns "" in that
	// case so the sensorium stays quiet until there's a signal.
	integrityFact *librarian.Fact
	// pendingCaptures is the count of Pending entries in the
	// captures store. Zero means no outstanding commitments
	// (fresh workspace, or all promises acted on / expired) and
	// renderSensoriumCaptures emits no element.
	pendingCaptures int
	// recalledCases is the librarian's score-sorted retrieval result
	// for this cycle's user input. Empty when there's nothing to
	// query against (no input, librarian unwired, or zero cases in
	// the corpus). The renderer partitions by Category and applies
	// per-category + total caps before injecting.
	recalledCases []cbr.Scored
}

// buildSensorium assembles the <sensorium> XML block. Returns "" when
// every section is empty (so the curator omits the block entirely
// rather than emitting a useless wrapper).
func buildSensorium(in sensoriumInputs) string {
	sections := []string{
		renderSensoriumClock(in.now, in.sessionSince, in.recentEntries, in.cycleID),
		renderSensoriumSituation(inputSourceOrDefault(in.inputSource), in.queueDepth, in.messageCount),
		renderSensoriumSchedule(),
		renderSensoriumIntray(),
		renderSensoriumVitals(approxCyclesToday(in.recentEntries, in.now), in.agentsActive),
		renderSensoriumAgents(in.agents),
		renderSensoriumDelegations(),
		renderSensoriumAmbient(in.ambient, in.now),
		renderSensoriumTasks(),
		renderSensoriumCaptures(in.pendingCaptures),
		renderSensoriumLearningGoals(),
		renderSensoriumAffectWarnings(),
		renderSensoriumSkillProcedures(),
		renderSensoriumKnowledge(),
		renderSensoriumMemory(in.narrativeCount, in.factCount, in.caseCount),
		renderSensoriumIntegrity(in.integrityFact),
		renderSensoriumActivePitfalls(in.recalledCases),
		renderSensoriumRecalledCases(in.recalledCases),
	}
	var nonEmpty []string
	for _, s := range sections {
		if s != "" {
			nonEmpty = append(nonEmpty, s)
		}
	}
	if len(nonEmpty) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<!-- Sensorium: ambient perception injected each cycle. No tool calls needed. -->\n<sensorium>\n")
	b.WriteString(strings.Join(nonEmpty, "\n"))
	b.WriteString("\n</sensorium>")
	return b.String()
}

// ---------------------------------------------------------------------------
// Substrate-portable sections (rendered today)
// ---------------------------------------------------------------------------

// renderSensoriumClock — temporal orientation. now (ISO), session uptime,
// optional cycle_id (8-char prefix), optional last_cycle elapsed.
func renderSensoriumClock(now, sessionSince time.Time, recent []librarian.NarrativeEntry, cycleID string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `  <clock now=%q session_uptime=%q`, now.UTC().Format(time.RFC3339), formatElapsedSince(now, sessionSince))
	if cycleID != "" {
		short := cycleID
		if len(short) > 8 {
			short = short[:8]
		}
		fmt.Fprintf(&b, ` cycle_id=%q`, short)
	}
	if len(recent) > 0 {
		fmt.Fprintf(&b, ` last_cycle=%q`, formatElapsedSince(now, recent[0].Timestamp))
	}
	b.WriteString(`/>`)
	return b.String()
}

// renderSensoriumSituation — who triggered this cycle, what's waiting,
// conversation depth. Active-thread attribute is omitted today (threading
// not shipped); when threading lands, this function gains a thread arg.
func renderSensoriumSituation(input string, queueDepth, messageCount int) string {
	return fmt.Sprintf(`  <situation input=%q queue_depth="%d" conversation_depth="%d"/>`,
		xmlEscape(input), queueDepth, messageCount)
}

// renderSensoriumVitals — operational telemetry the agent cares about.
// Today: cycles_today + agents_active. When affect / forecaster / planner
// budgets ship, this gains agent_health, last_failure, novelty, success_rate,
// recent_failures, cost_trend, cbr_hit_rate, cycles_remaining,
// tokens_remaining as additional attrs.
func renderSensoriumVitals(cyclesToday, agentsActive int) string {
	if cyclesToday == 0 && agentsActive == 0 {
		return ""
	}
	return fmt.Sprintf(`  <vitals cycles_today="%d" agents_active="%d"/>`, cyclesToday, agentsActive)
}

// renderSensoriumMemory — durable-store counts the agent should be aware
// of every cycle. Today: narrative entries + persistent facts + CBR
// cases. When artifacts + threads ship, this gains those attrs.
//
// Note: SD's <memory> tag also carries last_consolidation timestamps from
// the remembrancer. Ours doesn't yet (remembrancer is deferred); we can
// extend this when remembrancer lands.
func renderSensoriumMemory(narrativeCount, factCount, caseCount int) string {
	if narrativeCount == 0 && factCount == 0 && caseCount == 0 {
		return ""
	}
	return fmt.Sprintf(`  <memory narrative_entries="%d" facts="%d" cases="%d"/>`, narrativeCount, factCount, caseCount)
}

// renderSensoriumIntegrity — the agent's view of its own
// fabrication-audit numbers. Reads the fact written by the
// metalearning fabrication_audit worker
// (key=integrity_suspect_replies_7d, value=JSON with
// count/examined). When the fact is missing (fresh workspace,
// audit hasn't run yet) the section renders empty so the
// sensorium stays quiet until there's a signal.
//
// Ported from SD's narrative/curator.gleam render_sensorium_integrity.
// Renders only the audit attrs today; voice-drift attrs slot in when
// that worker lands.
//
// The rendered attrs are deliberately minimal — counts only, no
// interpretive adjectives. SD's lesson: "data, not achievements" —
// the agent surfaces its own integrity number as ambient context, not
// as something to narrate.
func renderSensoriumIntegrity(fact *librarian.Fact) string {
	if fact == nil {
		return ""
	}
	var v struct {
		Count    int `json:"count"`
		Examined int `json:"examined"`
	}
	if err := json.Unmarshal([]byte(fact.Value), &v); err != nil {
		return ""
	}
	return fmt.Sprintf(`  <integrity suspect_replies_7d="%d" replies_examined_7d="%d"/>`, v.Count, v.Examined)
}

// ---------------------------------------------------------------------------
// Substrate-deferred sections (return "" until the underlying subsystem
// ships). Defined explicitly so the buildSensorium structure is complete
// from the start — adding sections later is just changing the body of
// each function to read from its substrate.
// ---------------------------------------------------------------------------

// renderSensoriumSchedule — pending / overdue scheduled jobs. Waits for
// the scheduler subsystem.
func renderSensoriumSchedule() string { return "" }

// renderSensoriumIntray — pending operator-uploaded files / messages.
// Waits for the comms subsystem.
func renderSensoriumIntray() string { return "" }

// renderSensoriumAgents — the per-cycle catalogue of specialist
// agents the cog can delegate to, with cumulative token cost.
// Read by the cog's LLM in every system prompt so routing
// decisions happen with current cost data, not just static tool
// descriptions.
//
// Renders as one <agent .../> per registered specialist. Empty
// list returns "" so the section drops out.
//
// The `tool` attribute is the explicit `agent_<name>` mapping —
// without it small models sometimes fail to bridge the
// "agent named X" → "tool named agent_X" inference and
// confabulate that the agent isn't reachable. Including the
// tool name removes the inference burden entirely.
//
// XML shape (one self-closing element per agent):
//
//	<agents>
//	  <agent name="researcher" tool="agent_researcher"
//	         description="..." available="true"
//	         tokens_today="1234" tokens_lifetime="50000" dispatches="42"/>
//	  ...
//	</agents>
//
// Attributes are XML-escaped via xmlEscape; integer fields are
// formatted with %d.
func renderSensoriumAgents(agents []AgentInfo) string {
	if len(agents) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("  <agents>\n")
	for _, a := range agents {
		fmt.Fprintf(&b,
			`    <agent name=%q tool="agent_%s" description=%q available="%t" tokens_today="%d" tokens_lifetime="%d" dispatches="%d"/>`,
			a.Name, a.Name, xmlEscape(a.Description), a.Available,
			a.TokensToday, a.TokensLifetime, a.DispatchCount,
		)
		b.WriteString("\n")
	}
	b.WriteString("  </agents>")
	return b.String()
}

// renderSensoriumDelegations — active agent delegations from cog state.
// Distinct from renderSensoriumAgents (the catalogue) — this section
// is for in-flight dispatches when the curator gains live cog state.
// Today returns "" until that wiring lands.
func renderSensoriumDelegations() string { return "" }

// renderSensoriumAmbient — ambient signals drained from the cog's
// Notice() buffer for this cycle. Forecaster suggestions, observer
// flags, in-process telemetry — anything published via
// cog.Notice(ambient.Signal). Empty buffer renders "" so the section
// drops out of the assembled <sensorium>.
//
// Each signal becomes a `<signal>` element with source / kind / detail
// attrs and the timestamp formatted relative to `now` for readability.
// The XML escaping handles user-visible content (Detail) so a quoted
// signal can't break the wrapper.
func renderSensoriumAmbient(signals []ambient.Signal, now time.Time) string {
	if len(signals) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("  <ambient>\n")
	for _, s := range signals {
		fmt.Fprintf(&b,
			`    <signal source=%q kind=%q age=%q>%s</signal>`,
			xmlEscape(s.Source),
			xmlEscape(s.Kind),
			formatElapsedSince(now, s.Timestamp),
			xmlEscape(s.Detail),
		)
		b.WriteString("\n")
	}
	b.WriteString("  </ambient>")
	return b.String()
}

// renderSensoriumTasks — active planner tasks + endeavour phase progress.
// Waits for the planner subsystem.
func renderSensoriumTasks() string { return "" }

// renderSensoriumCaptures — pending commitment-tracker entries.
// Surfaces the count of captures awaiting agent action so the
// agent perceives its own outstanding promises every cycle.
//
// Returns "" when no Pending captures exist (fresh workspace,
// or every promise was acted on / expired). The element only
// shows up when there's something to perceive — quiet by default.
//
// Ported from SD's narrative/curator render-side that reads the
// captures store and emits `<captures pending="N"/>`. SD's full
// element also names the most recent capture's text + age; ours
// just shows the count for V1. Operators / agents drill into
// specifics by reading the captures JSONL directly. The
// list_captures tool ports as v1.1.
func renderSensoriumCaptures(pendingCount int) string {
	if pendingCount <= 0 {
		return ""
	}
	return fmt.Sprintf(`  <captures pending="%d"/>`, pendingCount)
}

// renderSensoriumLearningGoals — top active learning goals by priority.
// Waits for the learning-goals subsystem.
func renderSensoriumLearningGoals() string { return "" }

// renderSensoriumAffectWarnings — strong negative correlations between
// affect dimensions and outcome success. Waits for affect + Phase D
// correlation analysis.
func renderSensoriumAffectWarnings() string { return "" }

// renderSensoriumSkillProcedures — action-class → skill mapping the
// agent should consult before acting. Substrate (skills) exists; the
// action-class table is operator-config not yet exposed. Lands as a
// follow-up when the skill-procedures table is wired.
func renderSensoriumSkillProcedures() string { return "" }

// renderSensoriumKnowledge — workspace draft links. Waits for the
// knowledge / drafts subsystem.
func renderSensoriumKnowledge() string { return "" }

// pickRecalledCases applies the per-category + total-cap filtering
// over a score-sorted retrieval result. Returns a flat slice in
// rendered order (category groups in first-seen order, score-
// descending within each group). Exposed at package level so
// buildSensoriumBlock can derive the surfaced IDs without
// re-walking the renderer's output.
//
// Cases without a Category are dropped — there's no useful bucket
// for them in the sensorium block.
func pickRecalledCases(scored []cbr.Scored) []cbr.Scored {
	if len(scored) == 0 {
		return nil
	}
	grouped := make(map[cbr.Category][]cbr.Scored)
	var order []cbr.Category
	for _, s := range scored {
		cat := s.Case.Category
		if cat == "" {
			continue
		}
		if _, seen := grouped[cat]; !seen {
			order = append(order, cat)
		}
		grouped[cat] = append(grouped[cat], s)
	}
	if len(order) == 0 {
		return nil
	}
	out := make([]cbr.Scored, 0, recalledCasesTotalCap)
	for _, cat := range order {
		picks := grouped[cat]
		if len(picks) > recalledCasesPerCategory {
			picks = picks[:recalledCasesPerCategory]
		}
		if len(out)+len(picks) > recalledCasesTotalCap {
			picks = picks[:recalledCasesTotalCap-len(out)]
		}
		out = append(out, picks...)
		if len(out) >= recalledCasesTotalCap {
			break
		}
	}
	return out
}

// renderSensoriumRecalledCases — top retrieved CBR cases for this
// cycle, grouped by Category and capped per-category + overall.
// Empty input renders "" so the section drops out entirely.
//
// XML shape:
//
//	<recalled_cases>
//	  <category name="strategy" count="2">
//	    <case id="abc12345" score="0.82" intent="data_query" domain="weather">
//	      <problem>look up current weather conditions</problem>
//	      <solution>delegate to researcher with brave_search</solution>
//	      <lesson>cleaned up; 7d forecast was reliable</lesson>
//	    </case>
//	    ...
//	  </category>
//	  <category name="pitfall" count="1">
//	    <case id="def67890" score="0.71" intent="data_query" domain="weather">
//	      <problem>...</problem>
//	      <pitfall>forecasts beyond 7 days are unreliable</pitfall>
//	    </case>
//	  </category>
//	</recalled_cases>
//
// Why XML over a flat list:
//   - Category is the load-bearing organising signal — the LLM's
//     attention should partition Strategy vs Pitfall vs CodePattern
//     vs Troubleshooting vs DomainKnowledge differently.
//   - SD's curator emits the same shape; downstream curator-prompts
//     and the cog's persona instructions reference it by category name.
//
// Cases without a Category (deterministic classifier returned
// CategoryDomainKnowledge fallback OR conversation cases that legit
// have no category) are skipped — there's no useful bucket for them.
func renderSensoriumRecalledCases(scored []cbr.Scored) string {
	picked := pickRecalledCases(scored)
	if len(picked) == 0 {
		return ""
	}
	// Re-group the (already capped) picks by Category for rendering.
	// Order preserved by walking picked once and remembering first
	// appearance — pickRecalledCases already grouped contiguously
	// by category, so this mirrors the input.
	type bucket struct {
		cat   cbr.Category
		cases []cbr.Scored
	}
	var buckets []bucket
	idx := make(map[cbr.Category]int)
	for _, s := range picked {
		cat := s.Case.Category
		if i, ok := idx[cat]; ok {
			buckets[i].cases = append(buckets[i].cases, s)
			continue
		}
		idx[cat] = len(buckets)
		buckets = append(buckets, bucket{cat: cat, cases: []cbr.Scored{s}})
	}
	var b strings.Builder
	b.WriteString("  <recalled_cases>\n")
	for _, bk := range buckets {
		fmt.Fprintf(&b, "    <category name=%q count=\"%d\">\n", string(bk.cat), len(bk.cases))
		for _, s := range bk.cases {
			renderRecalledCaseElement(&b, s)
		}
		b.WriteString("    </category>\n")
	}
	b.WriteString("  </recalled_cases>")
	return b.String()
}

// renderSensoriumActivePitfalls hoists pitfalls from recalled cases
// into a dedicated, high-prominence block ABOVE <recalled_cases>.
//
// Why a separate block: pitfalls are imperative behavioural rules
// ("Never claim an action was completed without tool confirmation").
// Embedded inside `<case><pitfall>…</pitfall></case>` they're easy
// for a model to skim past — they live alongside problem text,
// solution prose, and assessments, all of which read as
// description, not instruction. Pulling them up into
// `<active_pitfalls>` flips the framing: the model sees them as
// rules currently in scope, not historical observations.
//
// Real-world driver: Mistral Large repeatedly fabricated tool
// outcomes in a session where pitfalls like "Never claim work that
// tools did not actually do" were already present in three recalled
// cases — the rules WERE in the prompt; the model just didn't treat
// them as load-bearing. Hoisting + relabelling fixes the framing.
//
// Dedup: same pitfall string from multiple cases collapses to one
// entry. Order is by first-seen. Empty pitfalls are skipped.
func renderSensoriumActivePitfalls(scored []cbr.Scored) string {
	picked := pickRecalledCases(scored)
	if len(picked) == 0 {
		return ""
	}
	seen := make(map[string]struct{})
	var pitfalls []string
	for _, s := range picked {
		for _, p := range s.Case.Outcome.Pitfalls {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if _, dup := seen[p]; dup {
				continue
			}
			seen[p] = struct{}{}
			pitfalls = append(pitfalls, p)
		}
	}
	if len(pitfalls) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  <active_pitfalls count=\"%d\">\n", len(pitfalls))
	b.WriteString("    <!-- Behavioural rules learned from past failures. " +
		"Treat as imperatives in scope this cycle. -->\n")
	for _, p := range pitfalls {
		fmt.Fprintf(&b, "    <pitfall>%s</pitfall>\n",
			xmlEscape(truncateForRecall(p)))
	}
	b.WriteString("  </active_pitfalls>")
	return b.String()
}

// renderRecalledCaseElement writes one <case> element. Intent/domain
// attrs surface only when populated — empty-string attrs would just
// add noise without help.
func renderRecalledCaseElement(b *strings.Builder, s cbr.Scored) {
	c := s.Case
	id := c.ID
	if len(id) > 8 {
		id = id[:8]
	}
	fmt.Fprintf(b, "      <case id=%q score=\"%.2f\"", id, s.Score)
	if c.Problem.IntentClass != "" {
		fmt.Fprintf(b, " intent=%q", string(c.Problem.IntentClass))
	}
	if c.Problem.Domain != "" {
		fmt.Fprintf(b, " domain=%q", xmlEscape(c.Problem.Domain))
	}
	b.WriteString(">\n")
	if c.Problem.Intent != "" {
		fmt.Fprintf(b, "        <problem>%s</problem>\n",
			xmlEscape(truncateForRecall(c.Problem.Intent)))
	}
	if c.Solution.Approach != "" {
		fmt.Fprintf(b, "        <solution>%s</solution>\n",
			xmlEscape(truncateForRecall(c.Solution.Approach)))
	}
	if c.Outcome.Assessment != "" {
		fmt.Fprintf(b, "        <lesson>%s</lesson>\n",
			xmlEscape(truncateForRecall(c.Outcome.Assessment)))
	}
	for _, p := range c.Outcome.Pitfalls {
		if p == "" {
			continue
		}
		fmt.Fprintf(b, "        <pitfall>%s</pitfall>\n",
			xmlEscape(truncateForRecall(p)))
	}
	b.WriteString("      </case>\n")
}

// truncateForRecall keeps inline case text under the configured cap so
// a single verbose case can't push other budget consumers (sensorium
// blocks, preamble) out.
func truncateForRecall(s string) string {
	if len(s) <= recalledCasesSummaryChars {
		return s
	}
	return s[:recalledCasesSummaryChars] + "..."
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// formatElapsedSince produces a two-unit string for human-readable
// durations in sensorium attributes. Shows the coarsest meaningful unit
// plus the next one down so the agent gets useful precision without
// absurdity at long sessions:
//
//	< 1 minute → "30s"
//	< 1 hour   → "5m 30s"
//	< 1 day    → "3h 12m"
//	≥ 1 day    → "2d 5h"
//
// Cache-safe: sensorium lives in the SystemPrompt's Dynamic half (per
// the prompt-caching slice), so per-second changes here don't poison
// the cacheable prefix.
func formatElapsedSince(now, then time.Time) string {
	if then.IsZero() || then.After(now) {
		return "0s"
	}
	d := now.Sub(then)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		m := int(d.Minutes())
		s := int(d.Seconds()) - m*60
		return fmt.Sprintf("%dm %ds", m, s)
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		return fmt.Sprintf("%dh %dm", h, m)
	default:
		days := int(d.Hours()) / 24
		h := int(d.Hours()) - days*24
		return fmt.Sprintf("%dd %dh", days, h)
	}
}

// approxCyclesToday counts narrative entries from the same calendar day
// as `now`. Approximation — when the DAG exposes a per-day cycle count
// query, this will read directly from the DAG. For now narrative-count
// is a fine proxy (one cycle ≈ one narrative entry).
func approxCyclesToday(entries []librarian.NarrativeEntry, now time.Time) int {
	year, month, day := now.UTC().Date()
	count := 0
	for _, e := range entries {
		ey, em, ed := e.Timestamp.UTC().Date()
		if ey == year && em == month && ed == day {
			count++
		}
	}
	return count
}

// inputSourceOrDefault matches the existing inputSource() helper but is
// kept distinct so sensorium-specific defaulting stays local. Today
// they're identical.
func inputSourceOrDefault(s string) string {
	if s == "" {
		return "user"
	}
	return s
}

// xmlEscape escapes the five XML-special chars for safe inclusion in
// attribute values. Mirrors the helper in internal/skills.
func xmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return r.Replace(s)
}
