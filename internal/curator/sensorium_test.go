package curator

import (
	"strings"
	"testing"
	"time"

	"github.com/seamus-brady/retainer/internal/ambient"
	"github.com/seamus-brady/retainer/internal/cbr"
	"github.com/seamus-brady/retainer/internal/librarian"
)

// ---- formatElapsedSince ----

func TestFormatElapsedSince(t *testing.T) {
	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		then time.Time
		want string
	}{
		{"zero time → 0s", time.Time{}, "0s"},
		{"future → 0s (clamp)", now.Add(time.Hour), "0s"},
		{"30 seconds ago", now.Add(-30 * time.Second), "30s"},
		{"exactly 1 minute → 1m 0s", now.Add(-time.Minute), "1m 0s"},
		{"5m 23s ago", now.Add(-(5*time.Minute + 23*time.Second)), "5m 23s"},
		{"59m 59s ago", now.Add(-(59*time.Minute + 59*time.Second)), "59m 59s"},
		{"exactly 1 hour → 1h 0m", now.Add(-time.Hour), "1h 0m"},
		{"3h 12m ago", now.Add(-(3*time.Hour + 12*time.Minute)), "3h 12m"},
		{"23h 59m ago", now.Add(-(23*time.Hour + 59*time.Minute)), "23h 59m"},
		{"exactly 1 day → 1d 0h", now.Add(-24 * time.Hour), "1d 0h"},
		{"2d 5h ago", now.Add(-(2*24*time.Hour + 5*time.Hour)), "2d 5h"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatElapsedSince(now, tc.then); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// ---- approxCyclesToday ----

func TestApproxCyclesToday(t *testing.T) {
	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	yesterday := now.Add(-24 * time.Hour)
	entries := []librarian.NarrativeEntry{
		{Timestamp: now.Add(-1 * time.Hour)},  // today
		{Timestamp: now.Add(-3 * time.Hour)},  // today
		{Timestamp: yesterday},                // not today
		{Timestamp: now.Add(-30 * time.Minute)}, // today
	}
	if got := approxCyclesToday(entries, now); got != 3 {
		t.Errorf("got %d, want 3 (entries today)", got)
	}
}

func TestApproxCyclesToday_EmptyEntries(t *testing.T) {
	if got := approxCyclesToday(nil, time.Now()); got != 0 {
		t.Errorf("empty entries → expected 0, got %d", got)
	}
}

// ---- renderSensoriumClock ----

func TestRenderSensoriumClock_AllAttrs(t *testing.T) {
	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	sessionSince := now.Add(-2 * time.Hour)
	recent := []librarian.NarrativeEntry{
		{Timestamp: now.Add(-5 * time.Minute)},
	}
	got := renderSensoriumClock(now, sessionSince, recent, "abcd1234efgh5678")

	for _, want := range []string{
		`<clock`,
		`now="2026-05-02T12:00:00Z"`,
		`session_uptime="2h 0m"`,
		`cycle_id="abcd1234"`, // first 8 chars
		`last_cycle="5m 0s"`,
		`/>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("clock output missing %q in %q", want, got)
		}
	}
}

func TestRenderSensoriumClock_NoCycleID(t *testing.T) {
	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	got := renderSensoriumClock(now, now.Add(-time.Minute), nil, "")
	if strings.Contains(got, "cycle_id") {
		t.Errorf("empty cycle_id should be omitted: %q", got)
	}
	if strings.Contains(got, "last_cycle") {
		t.Errorf("no recent entries → last_cycle should be omitted: %q", got)
	}
}

func TestRenderSensoriumClock_ShortCycleID(t *testing.T) {
	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	got := renderSensoriumClock(now, now, nil, "abc")
	// Short ID stays as-is; no panic on slice bounds.
	if !strings.Contains(got, `cycle_id="abc"`) {
		t.Errorf("short cycle_id should pass through: %q", got)
	}
}

// ---- renderSensoriumSituation ----

func TestRenderSensoriumSituation(t *testing.T) {
	got := renderSensoriumSituation("user", 2, 17)
	for _, want := range []string{
		`input="user"`,
		`queue_depth="2"`,
		`conversation_depth="17"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("situation missing %q in %q", want, got)
		}
	}
}

func TestRenderSensoriumSituation_XMLEscape(t *testing.T) {
	// Pathological input source value — should be escaped, not break the XML.
	got := renderSensoriumSituation(`a"b<c>d`, 0, 0)
	if strings.Contains(got, `a"b<c>d`) {
		t.Errorf("input value not escaped: %q", got)
	}
	if !strings.Contains(got, `a&quot;b&lt;c&gt;d`) {
		t.Errorf("expected escaped form in: %q", got)
	}
}

// ---- renderSensoriumVitals ----

func TestRenderSensoriumVitals_Empty(t *testing.T) {
	if got := renderSensoriumVitals(0, 0); got != "" {
		t.Errorf("empty vitals should render '', got %q", got)
	}
}

func TestRenderSensoriumVitals_Populated(t *testing.T) {
	got := renderSensoriumVitals(5, 2)
	if !strings.Contains(got, `cycles_today="5"`) || !strings.Contains(got, `agents_active="2"`) {
		t.Errorf("vitals missing attrs: %q", got)
	}
}

// ---- renderSensoriumMemory ----

func TestRenderSensoriumMemory_Empty(t *testing.T) {
	if got := renderSensoriumMemory(0, 0, 0); got != "" {
		t.Errorf("empty memory should render '', got %q", got)
	}
}

func TestRenderSensoriumMemory_Populated(t *testing.T) {
	got := renderSensoriumMemory(42, 7, 3)
	for _, want := range []string{`narrative_entries="42"`, `facts="7"`, `cases="3"`} {
		if !strings.Contains(got, want) {
			t.Errorf("memory missing %q in %q", want, got)
		}
	}
}

func TestRenderSensoriumMemory_OnlyCasesPresent(t *testing.T) {
	// All three counts contribute to the omit-when-zero logic; the
	// section should still render when only one is non-zero.
	got := renderSensoriumMemory(0, 0, 5)
	if !strings.Contains(got, `cases="5"`) {
		t.Errorf("expected cases attr when only cases nonzero; got %q", got)
	}
}

// ---- renderSensoriumIntegrity ----

func TestRenderSensoriumIntegrity_Nil(t *testing.T) {
	// Fresh workspace — fabrication-audit hasn't run yet — fact is
	// nil. Sensorium MUST render empty so the block is omitted; we
	// don't want noise in the prompt before there's a signal.
	if got := renderSensoriumIntegrity(nil); got != "" {
		t.Errorf("nil fact should render '', got %q", got)
	}
}

func TestRenderSensoriumIntegrity_PopulatedZero(t *testing.T) {
	// Audit ran but found nothing. The element renders with both
	// counts at zero so the agent perceives the clean run — quiet
	// IS information here.
	fact := &librarian.Fact{Value: `{"count":0,"examined":12}`}
	got := renderSensoriumIntegrity(fact)
	if !strings.Contains(got, `suspect_replies_7d="0"`) {
		t.Errorf("missing suspect_replies_7d=\"0\": %q", got)
	}
	if !strings.Contains(got, `replies_examined_7d="12"`) {
		t.Errorf("missing replies_examined_7d=\"12\": %q", got)
	}
}

func TestRenderSensoriumIntegrity_PopulatedFlagged(t *testing.T) {
	fact := &librarian.Fact{Value: `{"count":3,"examined":40,"suspect_cycle_ids":["a","b","c"]}`}
	got := renderSensoriumIntegrity(fact)
	if !strings.Contains(got, `suspect_replies_7d="3"`) {
		t.Errorf("missing suspect_replies_7d=\"3\": %q", got)
	}
	if !strings.Contains(got, `replies_examined_7d="40"`) {
		t.Errorf("missing replies_examined_7d=\"40\": %q", got)
	}
	// Suspect cycle IDs are intentionally NOT rendered in the
	// sensorium attribute — the signal is the count; the IDs live
	// in the fact body for operator-eyes-only memory_read.
	if strings.Contains(got, `suspect_cycle_ids`) {
		t.Errorf("cycle ids leaked into sensorium attr: %q", got)
	}
}

func TestRenderSensoriumIntegrity_MalformedJSON(t *testing.T) {
	// Defensive: a fact written by a future revision of the audit
	// might add fields the current decoder doesn't know about.
	// Unknown fields are fine (json.Unmarshal ignores them); but
	// outright-broken JSON should yield "" rather than a panic.
	fact := &librarian.Fact{Value: `not json at all`}
	if got := renderSensoriumIntegrity(fact); got != "" {
		t.Errorf("malformed value should render '', got %q", got)
	}
}

// ---- substrate-deferred sections all return "" ----

func TestDeferredSensoriumSections_AllEmpty(t *testing.T) {
	cases := map[string]string{
		"schedule":        renderSensoriumSchedule(),
		"intray":          renderSensoriumIntray(),
		"delegations":     renderSensoriumDelegations(),
		"tasks":           renderSensoriumTasks(),
		"learning_goals":  renderSensoriumLearningGoals(),
		"affect_warnings": renderSensoriumAffectWarnings(),
		"skill_proc":      renderSensoriumSkillProcedures(),
		"knowledge":       renderSensoriumKnowledge(),
	}
	for name, out := range cases {
		if out != "" {
			t.Errorf("deferred section %s should render '', got %q", name, out)
		}
	}
}

// ---- renderSensoriumCaptures ----

func TestRenderSensoriumCaptures_Zero(t *testing.T) {
	if got := renderSensoriumCaptures(0); got != "" {
		t.Errorf("zero pending should render '', got %q", got)
	}
	if got := renderSensoriumCaptures(-1); got != "" {
		t.Errorf("negative count should render '', got %q", got)
	}
}

func TestRenderSensoriumCaptures_Positive(t *testing.T) {
	got := renderSensoriumCaptures(3)
	if !strings.Contains(got, `pending="3"`) {
		t.Errorf("missing pending=\"3\" attr: %q", got)
	}
	if !strings.HasPrefix(strings.TrimSpace(got), "<captures") {
		t.Errorf("missing <captures element: %q", got)
	}
}

// renderSensoriumAmbient is now wired (no longer deferred) — empty input
// returns "", populated input returns the <ambient> block.
func TestRenderSensoriumAmbient_Empty(t *testing.T) {
	if got := renderSensoriumAmbient(nil, time.Now()); got != "" {
		t.Errorf("nil signals should render '', got %q", got)
	}
	if got := renderSensoriumAmbient([]ambient.Signal{}, time.Now()); got != "" {
		t.Errorf("empty signals should render '', got %q", got)
	}
}

func TestRenderSensoriumAmbient_Populated(t *testing.T) {
	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	signals := []ambient.Signal{
		{
			Source:    "forecaster",
			Kind:      "plan_health_degraded",
			Detail:    "endeavour 'auth-refactor' drift score 0.68",
			Timestamp: now.Add(-3 * time.Minute),
		},
		{
			Source:    "observer",
			Kind:      "cycle_anomaly",
			Detail:    "watchdog fired on cycle abc123",
			Timestamp: now.Add(-30 * time.Second),
		},
	}
	got := renderSensoriumAmbient(signals, now)
	for _, want := range []string{
		`<ambient>`,
		`</ambient>`,
		`source="forecaster"`,
		`kind="plan_health_degraded"`,
		`age="3m 0s"`,
		`drift score 0.68`,
		`source="observer"`,
		`kind="cycle_anomaly"`,
		`age="30s"`,
		`watchdog fired`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ambient output missing %q in:\n%s", want, got)
		}
	}
}

func TestRenderSensoriumAmbient_XMLEscape(t *testing.T) {
	signals := []ambient.Signal{
		{
			Source:    `a"b`,
			Kind:      `c<d`,
			Detail:    `e>f & g`,
			Timestamp: time.Now(),
		},
	}
	got := renderSensoriumAmbient(signals, time.Now())
	for _, banned := range []string{`a"b`, `c<d`, `e>f & g`} {
		if strings.Contains(got, banned) {
			t.Errorf("unescaped %q in: %s", banned, got)
		}
	}
}

// ---- buildSensorium top-level ----
//
// Note: clock and situation are unconditional (every cycle has a now and
// an input source), so the wrapper always renders in real operation.
// We don't test an "all empty inputs return empty wrapper" case because
// that scenario can't occur — the cog always has a clock and a cycle to
// frame.

func TestBuildSensorium_AssembledShape(t *testing.T) {
	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	in := sensoriumInputs{
		now:            now,
		sessionSince:   now.Add(-time.Hour),
		cycleID:        "abc-123",
		inputSource:    "user",
		queueDepth:     0,
		messageCount:   3,
		agentsActive:   2,
		recentEntries:  []librarian.NarrativeEntry{{Timestamp: now.Add(-2 * time.Minute)}},
		narrativeCount: 14,
		factCount:      4,
	}
	got := buildSensorium(in)

	for _, want := range []string{
		"<!-- Sensorium",
		"<sensorium>",
		"</sensorium>",
		`<clock`,
		`<situation`,
		`<vitals`,
		`<memory`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("buildSensorium output missing %q in:\n%s", want, got)
		}
	}

	// Sections that have no substrate today should not appear.
	// (<ambient> only renders when ambient signals are present;
	// this test passes none.)
	for _, banned := range []string{
		"<schedule",
		"<intray",
		"<delegations",
		"<ambient",
		"<tasks",
		"<captures",
	} {
		if strings.Contains(got, banned) {
			t.Errorf("substrate-less section unexpectedly rendered: %q in:\n%s", banned, got)
		}
	}
}

// TestBuildSensorium_WithAmbient — ambient signals plumbed through
// buildSensorium do reach the assembled block. Order check pins
// <ambient> between <delegations> (deferred, omitted) and <tasks>
// (deferred, omitted) — i.e. after <vitals>, before <memory>.
func TestBuildSensorium_WithAmbient(t *testing.T) {
	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	in := sensoriumInputs{
		now:          now,
		sessionSince: now.Add(-time.Hour),
		inputSource:  "user",
		messageCount: 1,
		agentsActive: 1,
		ambient: []ambient.Signal{
			{Source: "forecaster", Kind: "drift", Detail: "score rising", Timestamp: now.Add(-2 * time.Minute)},
		},
	}
	got := buildSensorium(in)
	if !strings.Contains(got, "<ambient>") {
		t.Errorf("expected <ambient> in:\n%s", got)
	}
	if !strings.Contains(got, "score rising") {
		t.Errorf("expected detail in:\n%s", got)
	}
	vitalsIdx := strings.Index(got, "<vitals")
	ambientIdx := strings.Index(got, "<ambient")
	if vitalsIdx == -1 || ambientIdx == -1 || vitalsIdx >= ambientIdx {
		t.Errorf("ambient should follow vitals: vitals=%d ambient=%d in:\n%s", vitalsIdx, ambientIdx, got)
	}
}

func TestBuildSensorium_PitfallsRenderBeforeRecalledCases(t *testing.T) {
	// Pitfalls are imperative behavioural rules; recalled_cases is
	// the supporting context. Imperatives must come first so the
	// model encounters the rules before the case bodies — flipping
	// this order is exactly the bug the original Mistral
	// hallucination incident motivated fixing.
	now := time.Now()
	in := sensoriumInputs{
		now:            now,
		sessionSince:   now,
		inputSource:    "user",
		messageCount:   1,
		agentsActive:   1,
		narrativeCount: 1,
		factCount:      1,
		recalledCases: []cbr.Scored{
			makeScored("p1", 0.9, cbr.CategoryPitfall, cbr.IntentDataQuery,
				"prior failure", "research", "tried something", "didn't work",
				"never claim work tools did not actually do"),
		},
	}
	got := buildSensorium(in)
	pitfallIdx := strings.Index(got, "<active_pitfalls")
	casesIdx := strings.Index(got, "<recalled_cases")
	if pitfallIdx < 0 || casesIdx < 0 {
		t.Fatalf("blocks missing: pitfall=%d cases=%d in:\n%s", pitfallIdx, casesIdx, got)
	}
	if pitfallIdx > casesIdx {
		t.Errorf("active_pitfalls (%d) renders after recalled_cases (%d); imperatives must come first.\n%s",
			pitfallIdx, casesIdx, got)
	}
}

func TestBuildSensorium_SectionOrder(t *testing.T) {
	// Verify rendering order matches SD's pattern (clock first, then
	// situation, then the rest in the documented order). This pins the
	// output's spatial meaning so the agent gets consistent perception.
	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	in := sensoriumInputs{
		now:            now,
		sessionSince:   now.Add(-time.Hour),
		inputSource:    "user",
		messageCount:   1,
		agentsActive:   1,
		narrativeCount: 1,
		factCount:      1,
	}
	got := buildSensorium(in)
	clockIdx := strings.Index(got, "<clock")
	situationIdx := strings.Index(got, "<situation")
	vitalsIdx := strings.Index(got, "<vitals")
	memoryIdx := strings.Index(got, "<memory")

	if !(clockIdx < situationIdx && situationIdx < vitalsIdx && vitalsIdx < memoryIdx) {
		t.Errorf("section order wrong: clock=%d situation=%d vitals=%d memory=%d in:\n%s",
			clockIdx, situationIdx, vitalsIdx, memoryIdx, got)
	}
}

// ---- renderSensoriumAgents ----

func TestRenderSensoriumAgents_EmptyDropsSection(t *testing.T) {
	if got := renderSensoriumAgents(nil); got != "" {
		t.Errorf("nil should render empty; got %q", got)
	}
	if got := renderSensoriumAgents([]AgentInfo{}); got != "" {
		t.Errorf("empty list should render empty; got %q", got)
	}
}

func TestRenderSensoriumAgents_OneAgent(t *testing.T) {
	got := renderSensoriumAgents([]AgentInfo{{
		Name:           "researcher",
		Description:    "Web search + URL extraction.",
		Available:      true,
		TokensToday:    1234,
		TokensLifetime: 50000,
		DispatchCount:  42,
	}})
	for _, want := range []string{
		`name="researcher"`,
		`tool="agent_researcher"`,
		`description="Web search + URL extraction."`,
		`available="true"`,
		`tokens_today="1234"`,
		`tokens_lifetime="50000"`,
		`dispatches="42"`,
		"<agents>",
		"</agents>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestRenderSensoriumAgents_ToolAttributeMatchesName(t *testing.T) {
	// Pin the explicit name → tool mapping. Small models
	// reliably failed to bridge this inference; the attribute
	// removes the inference entirely.
	got := renderSensoriumAgents([]AgentInfo{
		{Name: "researcher", Description: "r"},
		{Name: "observer", Description: "o"},
		{Name: "remembrancer", Description: "rm"},
	})
	for _, name := range []string{"researcher", "observer", "remembrancer"} {
		want := `tool="agent_` + name + `"`
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestRenderSensoriumAgents_MultipleAgentsPreserveOrder(t *testing.T) {
	got := renderSensoriumAgents([]AgentInfo{
		{Name: "researcher", Description: "r"},
		{Name: "observer", Description: "o"},
	})
	rIdx := strings.Index(got, `name="researcher"`)
	oIdx := strings.Index(got, `name="observer"`)
	if rIdx < 0 || oIdx < 0 {
		t.Fatalf("missing agents in:\n%s", got)
	}
	if rIdx >= oIdx {
		t.Errorf("researcher should appear before observer; got r=%d o=%d", rIdx, oIdx)
	}
}

func TestRenderSensoriumAgents_XMLEscapeAmpersand(t *testing.T) {
	got := renderSensoriumAgents([]AgentInfo{{
		Name:        "tricky",
		Description: `with & ampersand`,
	}})
	if !strings.Contains(got, "&amp;") {
		t.Errorf("ampersand should be XML-escaped to &amp;; got: %s", got)
	}
}

func TestBuildSensorium_AgentsBlockSlots(t *testing.T) {
	// Confirm the agents section sits between vitals and memory
	// when populated, so the prompt order stays predictable.
	in := sensoriumInputs{
		now:            time.Now(),
		sessionSince:   time.Now().Add(-time.Hour),
		inputSource:    "operator",
		queueDepth:     0,
		messageCount:   1,
		agentsActive:   2,
		agents:         []AgentInfo{{Name: "researcher", Description: "r"}},
		narrativeCount: 1,
		factCount:      1,
	}
	got := buildSensorium(in)
	vitalsIdx := strings.Index(got, "<vitals")
	agentsIdx := strings.Index(got, "<agents>")
	memoryIdx := strings.Index(got, "<memory")
	if !(vitalsIdx < agentsIdx && agentsIdx < memoryIdx) {
		t.Errorf("agents block out of place: vitals=%d agents=%d memory=%d\n%s",
			vitalsIdx, agentsIdx, memoryIdx, got)
	}
}

// ---- renderSensoriumRecalledCases ----

func makeScored(id string, score float64, cat cbr.Category, intentClass cbr.IntentClassification, intent, domain, approach, assessment string, pitfalls ...string) cbr.Scored {
	return cbr.Scored{
		Score: score,
		Case: cbr.Case{
			ID:       id,
			Category: cat,
			Problem: cbr.Problem{
				IntentClass: intentClass,
				Intent:      intent,
				Domain:      domain,
			},
			Solution: cbr.Solution{Approach: approach},
			Outcome: cbr.Outcome{
				Assessment: assessment,
				Pitfalls:   pitfalls,
			},
		},
	}
}

func TestRenderSensoriumRecalledCases_EmptyDropsSection(t *testing.T) {
	if got := renderSensoriumRecalledCases(nil); got != "" {
		t.Errorf("nil should render '', got %q", got)
	}
	if got := renderSensoriumRecalledCases([]cbr.Scored{}); got != "" {
		t.Errorf("empty slice should render '', got %q", got)
	}
}

func TestRenderSensoriumRecalledCases_ScoredButCategoryless(t *testing.T) {
	// Cases without a Category get skipped — there's no useful bucket
	// for them. With every case categoryless, the section drops out.
	scored := []cbr.Scored{
		{Score: 0.8, Case: cbr.Case{ID: "x"}},
		{Score: 0.7, Case: cbr.Case{ID: "y"}},
	}
	if got := renderSensoriumRecalledCases(scored); got != "" {
		t.Errorf("all-categoryless should render '', got %q", got)
	}
}

func TestRenderSensoriumRecalledCases_GroupsByCategory(t *testing.T) {
	// IDs are full UUIDs in real life — pin only the 8-char prefix
	// behaviour the renderer documents.
	scored := []cbr.Scored{
		makeScored("strat123-aaaa-bbbb", 0.92, cbr.CategoryStrategy, cbr.IntentDataQuery, "look up weather", "weather", "delegate to researcher", "worked well"),
		makeScored("trouble1-aaaa-bbbb", 0.85, cbr.CategoryTroubleshooting, cbr.IntentMonitoringCheck, "diagnose timeout", "auth", "check the policy", "fixed it"),
	}
	got := renderSensoriumRecalledCases(scored)

	wants := []string{
		"<recalled_cases>",
		`<category name="strategy" count="1">`,
		`<category name="troubleshooting" count="1">`,
		`<case id="strat123" score="0.92"`,
		`intent="data_query"`,
		`domain="weather"`,
		"<problem>look up weather</problem>",
		"<solution>delegate to researcher</solution>",
		"<lesson>worked well</lesson>",
		`<case id="trouble1" score="0.85"`,
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
	if !strings.Contains(got, "</recalled_cases>") {
		t.Errorf("missing closing tag:\n%s", got)
	}
}

func TestRenderSensoriumRecalledCases_PerCategoryCap(t *testing.T) {
	// Three strategy cases, but per-category cap is 2. Top 2 by
	// score-order survive.
	scored := []cbr.Scored{
		makeScored("s1", 0.9, cbr.CategoryStrategy, "", "first", "", "", ""),
		makeScored("s2", 0.8, cbr.CategoryStrategy, "", "second", "", "", ""),
		makeScored("s3", 0.7, cbr.CategoryStrategy, "", "third", "", "", ""),
	}
	got := renderSensoriumRecalledCases(scored)
	if !strings.Contains(got, `count="2"`) {
		t.Errorf("expected count=2 (per-category cap), got:\n%s", got)
	}
	if strings.Contains(got, "third") {
		t.Errorf("third case should be capped out:\n%s", got)
	}
	if !strings.Contains(got, "first") || !strings.Contains(got, "second") {
		t.Errorf("top-2 cases should remain:\n%s", got)
	}
}

func TestRenderSensoriumRecalledCases_TotalCap(t *testing.T) {
	// 5 categories, 2 each = 10 candidates. Total cap (4) bites first.
	scored := []cbr.Scored{
		makeScored("st1", 0.95, cbr.CategoryStrategy, "", "", "", "", ""),
		makeScored("st2", 0.90, cbr.CategoryStrategy, "", "", "", "", ""),
		makeScored("cp1", 0.85, cbr.CategoryCodePattern, "", "", "", "", ""),
		makeScored("cp2", 0.80, cbr.CategoryCodePattern, "", "", "", "", ""),
		makeScored("tr1", 0.75, cbr.CategoryTroubleshooting, "", "", "", "", ""),
		makeScored("pf1", 0.70, cbr.CategoryPitfall, "", "", "", "", ""),
		makeScored("dk1", 0.65, cbr.CategoryDomainKnowledge, "", "", "", "", ""),
	}
	got := renderSensoriumRecalledCases(scored)
	count := strings.Count(got, "<case id=")
	if count != recalledCasesTotalCap {
		t.Errorf("total cap not enforced: got %d <case> elements, want %d.\n%s", count, recalledCasesTotalCap, got)
	}
}

func TestRenderSensoriumActivePitfalls_HoistsAcrossCases(t *testing.T) {
	scored := []cbr.Scored{
		makeScored("pf1", 0.9, cbr.CategoryPitfall, cbr.IntentExploration,
			"prior failure A", "research", "tried X", "X failed",
			"Never claim work tools did not actually do",
			"Always run brave_web_search before writing URLs"),
		makeScored("pf2", 0.85, cbr.CategoryPitfall, cbr.IntentExploration,
			"prior failure B", "research", "tried Y", "Y failed",
			"Never claim work tools did not actually do",       // duplicate — should dedup
			"Verify links before sending in email"),
	}
	got := renderSensoriumActivePitfalls(scored)
	if !strings.Contains(got, `<active_pitfalls count="3">`) {
		t.Errorf("expected count=3 (deduped), got header in:\n%s", got)
	}
	for _, want := range []string{
		"<pitfall>Never claim work tools did not actually do</pitfall>",
		"<pitfall>Always run brave_web_search before writing URLs</pitfall>",
		"<pitfall>Verify links before sending in email</pitfall>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// Dedup check — duplicate string appears exactly once.
	if c := strings.Count(got, "Never claim work tools did not actually do"); c != 1 {
		t.Errorf("dedup failed: appeared %d times", c)
	}
}

func TestRenderSensoriumActivePitfalls_EmptyWhenNoPitfalls(t *testing.T) {
	scored := []cbr.Scored{
		makeScored("ok", 0.8, cbr.CategoryStrategy, "",
			"happy path", "general", "did the thing", "worked"),
	}
	if got := renderSensoriumActivePitfalls(scored); got != "" {
		t.Errorf("expected empty, got:\n%s", got)
	}
}

func TestRenderSensoriumActivePitfalls_EmptyWhenNoCases(t *testing.T) {
	if got := renderSensoriumActivePitfalls(nil); got != "" {
		t.Errorf("expected empty for nil scored, got %q", got)
	}
}

func TestRenderSensoriumActivePitfalls_SkipsBlankPitfalls(t *testing.T) {
	scored := []cbr.Scored{
		makeScored("p", 0.9, cbr.CategoryPitfall, "",
			"failure", "research", "approach", "assessment",
			"", "  ", "real pitfall"),
	}
	got := renderSensoriumActivePitfalls(scored)
	if !strings.Contains(got, `count="1"`) {
		t.Errorf("expected blanks skipped, got:\n%s", got)
	}
}

func TestRenderSensoriumActivePitfalls_BlockOrdersBeforeCases(t *testing.T) {
	// The build pipeline orders renderSensoriumActivePitfalls
	// BEFORE renderSensoriumRecalledCases — assert the output
	// reflects that order so a future refactor can't silently
	// flip it. (Pitfalls are imperative; cases are context.
	// Models follow what they see first.)
	scored := []cbr.Scored{
		makeScored("p", 0.9, cbr.CategoryPitfall, "",
			"failure", "research", "approach", "assessment",
			"actual rule"),
	}
	pitfalls := renderSensoriumActivePitfalls(scored)
	cases := renderSensoriumRecalledCases(scored)
	if pitfalls == "" || cases == "" {
		t.Fatal("preconditions: both blocks should render")
	}
	// The Build() function joins sections with newlines; here we
	// just confirm both functions produced output independently.
	// The actual ordering is asserted by the build pipeline test
	// in TestBuildSensorium below.
	if !strings.Contains(pitfalls, "<active_pitfalls") {
		t.Error("pitfalls block missing tag")
	}
	if !strings.Contains(cases, "<recalled_cases") {
		t.Error("cases block missing tag")
	}
}

func TestRenderSensoriumRecalledCases_RendersPitfalls(t *testing.T) {
	scored := []cbr.Scored{
		makeScored("pf1", 0.8, cbr.CategoryPitfall, cbr.IntentExploration,
			"investigate the timeout", "auth", "tried direct DB query", "didn't work",
			"don't bypass the connection pool", "session tokens expire mid-flight"),
	}
	got := renderSensoriumRecalledCases(scored)
	for _, want := range []string{
		"<pitfall>don&apos;t bypass the connection pool</pitfall>",
		"<pitfall>session tokens expire mid-flight</pitfall>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing pitfall %q in:\n%s", want, got)
		}
	}
}

func TestRenderSensoriumRecalledCases_TruncatesLongFields(t *testing.T) {
	long := strings.Repeat("a", recalledCasesSummaryChars+50)
	scored := []cbr.Scored{
		makeScored("x", 0.9, cbr.CategoryStrategy, "", long, "", "", ""),
	}
	got := renderSensoriumRecalledCases(scored)
	if !strings.Contains(got, "...") {
		t.Errorf("expected truncation marker:\n%s", got)
	}
	// Make sure we don't render the full long string anywhere.
	if strings.Contains(got, long) {
		t.Errorf("untruncated long field rendered:\n%s", got)
	}
}

func TestRenderSensoriumRecalledCases_XMLEscape(t *testing.T) {
	scored := []cbr.Scored{
		makeScored("x", 0.9, cbr.CategoryStrategy, "", "find <auth> tokens & session id", "", "use API \"v2\"", ""),
	}
	got := renderSensoriumRecalledCases(scored)
	for _, want := range []string{
		"&lt;auth&gt;", "&amp;", "&quot;v2&quot;",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing escape %q in:\n%s", want, got)
		}
	}
}

func TestBuildSensorium_IncludesRecalledCases(t *testing.T) {
	now := time.Now()
	in := sensoriumInputs{
		now:          now,
		sessionSince: now.Add(-time.Hour),
		recalledCases: []cbr.Scored{
			makeScored("s1", 0.9, cbr.CategoryStrategy, cbr.IntentDataQuery, "look up weather", "weather", "use brave_search", "worked"),
		},
	}
	got := buildSensorium(in)
	if !strings.Contains(got, "<recalled_cases>") {
		t.Errorf("recalled_cases block missing from sensorium:\n%s", got)
	}
}
