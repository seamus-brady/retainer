// Package curator owns the system-prompt assembly path. It sits between
// the cognitive loop and the identity layer: the cog asks for "the system
// prompt for this cycle," the curator merges static identity files with
// memory-derived slots queried from the librarian, and returns the
// rendered string.
//
// The curator is its own actor (hand-rolled goroutine + tagged inbox +
// Run loop), supervised under actor.Permanent. Per the project's "agents
// are named actors" rule, system-prompt assembly is a named, supervised,
// observable process — not a method tucked into the cog.
//
// Selective port from Springdrift's narrative/curator.gleam:
//
//   Load-bearing here:
//     - persona render (identity package)
//     - preamble render with slot substitution + OMIT-IF rules
//     - librarian queries: thread/fact counts, recent narrative, recent
//       persistent facts
//     - per-cycle Context (cycle_id, input_source, queue_depth)
//
//   Deferred (will land alongside the subsystems they depend on):
//     - sensorium XML block (waits on threading + scheduler + affect)
//     - virtual-memory scratchpad (Letta-style working memory slots)
//     - bootstrap-skills inlining (waits on the skills subsystem)
//     - inter-agent context injection (waits on the agent framework)
//     - performance summary, agent health, delegations
//
// The curator never owns its own indexes — the librarian is the single
// owner of all memory state. The curator only queries.
package curator

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/seamus-brady/retainer/internal/ambient"
	"github.com/seamus-brady/retainer/internal/cbr"
	"github.com/seamus-brady/retainer/internal/cyclelog"
	"github.com/seamus-brady/retainer/internal/identity"
	"github.com/seamus-brady/retainer/internal/librarian"
	"github.com/seamus-brady/retainer/internal/llm"
	"github.com/seamus-brady/retainer/internal/metalearning"
	"github.com/seamus-brady/retainer/internal/skills"
)

const (
	// recalledCasesQueryMaxResults is the size of the retrieval pool
	// the sensorium pulls before per-category partitioning. Bigger
	// than the rendered cap so we can surface a mix of categories
	// rather than the top-N of one dominant kind.
	recalledCasesQueryMaxResults = 12

	inboxBufferSize = 64

	// recentNarrativeForSlots bounds how many entries the curator pulls
	// to populate the {{recent_narrative}} and {{last_session_summary}}
	// slots. Five mirrors Springdrift's curator.gleam choice — enough
	// continuity, small enough to not bloat the preamble.
	recentNarrativeForSlots = 5

	// recentFactsForSlots bounds how many persistent facts get inlined
	// into the {{recent_fact_sample}} slot. Three matches Springdrift.
	recentFactsForSlots = 3

	// recentEntrySummaryChars caps each narrative entry in the
	// {{recent_narrative}} slot. Long summaries truncate; the
	// remembrancer is the path for full archive reads.
	recentEntrySummaryChars = 200
)

// LibrarianQuery is the slice of *librarian.Librarian the curator needs.
// Letting the curator depend on this interface keeps tests independent
// of the librarian's SQLite-and-JSONL implementation.
type LibrarianQuery interface {
	RecentNarrative(limit int) []librarian.NarrativeEntry
	PersistentFactCount() int
	RecentPersistentFacts(limit int) []librarian.Fact
	CaseCount() int
	// GetFact returns the most-recent non-cleared fact for key,
	// or nil. The sensorium's <integrity> block reads
	// `integrity_suspect_replies_7d` this way; future single-key
	// sensorium consumers (voice drift, affect correlation, etc.)
	// will route through the same accessor.
	GetFact(key string) *librarian.Fact
	// RetrieveCases scores cases against the query and returns them
	// sorted by score descending. Used by the sensorium's
	// <recalled_cases> block. ctx propagates cancellation through to
	// the embedder when the embedding signal is in play.
	RetrieveCases(ctx context.Context, q cbr.Query) []cbr.Scored
}

// Config wires the curator's collaborators. Identity and Librarian are
// required; the rest have sensible defaults.
type Config struct {
	// Identity holds the loaded persona + preamble template. The curator
	// re-renders these every cycle with fresh slot values.
	Identity *identity.Identity
	// Librarian provides the memory queries the curator merges into slots.
	// May be nil for tests that only exercise the identity-render path —
	// when nil, memory-derived slots stay empty and OMIT-IF rules drop
	// the corresponding preamble lines.
	Librarian LibrarianQuery
	// Workspace is the resolved workspace directory; surfaced as the
	// {{workspace}} slot for orientation in the persona.
	Workspace string
	// AgentName is the {{agent_name}} slot value.
	AgentName string
	// AgentVersion is the {{agent_version}} slot value (optional —
	// empty string is fine; OMIT-IF will drop dependent lines).
	AgentVersion string
	// NowFn returns the current time. Defaults to time.Now; tests
	// inject a deterministic clock.
	NowFn func() time.Time
	// IDFn returns the per-assembly work-unit ID stamped onto every
	// curator_assembled event. Defaults to uuid.NewString; tests
	// inject a deterministic generator.
	IDFn func() string
	// Logger receives diagnostic logs. Defaults to slog.Default().
	Logger *slog.Logger
	// CycleLog receives a `curator_assembled` event after every prompt
	// build. Optional — when nil, assembly happens silently. Wiring it
	// closes the audit gap: an inspect tool can show, per cycle,
	// prompt size + how many memory entries the curator pulled in.
	CycleLog cyclelog.Sink
	// Skills is the set of discovered skills available to the cog.
	// The curator filters via skills.ForAgent("cognitive") +
	// skills.ForContext (currently with empty domains so context-
	// scoped skills inject unconditionally) and renders the
	// resulting set as <available_skills>.
	Skills []skills.SkillMeta
	// BootstrapSkillIDs lists the skill IDs whose full body is
	// inlined into the prompt on the first cycle of a fresh session
	// (recent narrative empty). Mirrors Springdrift's
	// `bootstrap_skill_ids` curator state. Empty list = no
	// bootstrap (operator opt-out). Skill IDs that don't resolve
	// against `Skills` are silently skipped — bootstrap is best-
	// effort.
	BootstrapSkillIDs []string
	// SessionSince records when the cog started (or the curator was
	// constructed). Used by the sensorium <clock> section as the
	// session_uptime reference. Defaults to time.Now() at curator
	// construction; main.go can override.
	SessionSince time.Time
	// AgentsActive is the count of registered specialist agents. Used
	// by the sensorium <vitals> section. Today this is a static value
	// passed at construction; when the supervisor exposes a live count
	// it can become dynamic.
	AgentsActive int
	// Agents, when non-nil, is queried each cycle to populate the
	// sensorium <agents> block — the per-cycle catalogue of
	// specialists the cog can delegate to plus their token cost.
	// Nil disables the block; the section renders empty and drops
	// out of the assembled <sensorium>.
	Agents AgentDirectory
	// Captures, when non-nil, is queried each cycle for the count
	// of pending commitment-tracker entries. Surfaced via the
	// sensorium's <captures pending="N"/> element when the count
	// is positive. Nil disables the block.
	Captures CapturesQuery
}

// CapturesQuery is the narrow seam the curator uses to read the
// captures-store's Pending count. *captures.Store satisfies this
// via Go's structural typing on CountPending.
type CapturesQuery interface {
	CountPending() int
}

// CycleContext carries per-cycle, ephemeral data the curator can't
// derive itself. The cog passes one of these on every BuildSystemPrompt
// call. Currently small — extends as new ambient signals land.
type CycleContext struct {
	// CycleID is the UUID of the in-flight cycle.
	CycleID string
	// InputSource distinguishes user input from scheduler-triggered work.
	// "user" or "scheduler". Empty defaults to "user".
	InputSource string
	// QueueDepth is the number of UserInputs waiting behind the current
	// one. Surfaced via {{queue_depth}}.
	QueueDepth int
	// MessageCount is the total count of messages in the conversation
	// history at the start of this cycle. Surfaced via {{message_count}}.
	MessageCount int
	// AmbientSignals is the cog's drained ambient buffer for this
	// cycle: forecaster suggestions, observer flags, in-process
	// telemetry the agent should perceive but didn't trigger this
	// cycle. Empty when nothing has been Notice()'d since the previous
	// cycle. Rendered into the <ambient> sensorium section.
	AmbientSignals []ambient.Signal
	// UserInput is the operator's text for this cycle (or the
	// scheduler-injected prompt for autonomous cycles). The curator
	// derives a CBR retrieval query from it for the <recalled_cases>
	// sensorium block. Empty input skips retrieval entirely — there's
	// nothing to query against.
	UserInput string
}

// Curator is the actor wrapping system-prompt assembly. Construct with
// New, then call Run under a supervisor; the cog calls BuildSystemPrompt.
type Curator struct {
	cfg   Config
	inbox chan message
}

// New constructs a Curator. Returns an error if Identity is nil — without
// an identity there's no character to render. Librarian may be nil
// (tests / startup before librarian is wired).
func New(cfg Config) (*Curator, error) {
	if cfg.Identity == nil {
		return nil, fmt.Errorf("curator: Identity is required")
	}
	if cfg.NowFn == nil {
		cfg.NowFn = time.Now
	}
	if cfg.IDFn == nil {
		cfg.IDFn = uuid.NewString
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.SessionSince.IsZero() {
		cfg.SessionSince = cfg.NowFn()
	}
	return &Curator{
		cfg:   cfg,
		inbox: make(chan message, inboxBufferSize),
	}, nil
}

// Run is the actor loop. Block until ctx is cancelled. Wrap with
// actor.Run under actor.Permanent so panics restart cleanly.
func (c *Curator) Run(ctx context.Context) error {
	c.cfg.Logger.Info("curator started")
	defer c.cfg.Logger.Info("curator stopped")
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case m := <-c.inbox:
			c.handle(m)
		}
	}
}

// BuildSystemPrompt assembles the cache-aware system prompt for the given
// cycle. Synchronous — blocks on the curator's inbox. Safe to call from
// any goroutine.
//
// The returned SystemPrompt has two halves:
//
//   - Stable: persona + bootstrap_skills (when present) + <available_skills>.
//     Intended to be byte-identical across cycles in a session so the
//     upstream prompt cache hits. Does change on a fresh-session first
//     cycle (bootstrap inlines); cycle 2+ in the same session settle on a
//     stable prefix.
//   - Dynamic: rendered preamble (date, time, queue depth, recent
//     narrative). Per-cycle by definition; never byte-identical across
//     cycles.
//
// The Anthropic adapter places a cache_control marker between the two;
// other providers concatenate.
//
// reqCtx is the caller's request context (the cog's dispatch ctx). It's
// forwarded to substrate calls that need cancellation awareness — most
// notably the librarian's RetrieveCases for the <recalled_cases> block.
// Use context.Background() when no caller-side cancellation applies
// (tests, one-shot init-time validation).
func (c *Curator) BuildSystemPrompt(reqCtx context.Context, cyc CycleContext) llm.SystemPrompt {
	reply := make(chan llm.SystemPrompt, 1)
	c.inbox <- buildPromptMsg{reqCtx: reqCtx, cyc: cyc, reply: reply}
	return <-reply
}

// BuildSystemPromptString returns the assembled system prompt as a single
// concatenated string. Convenience for callers that don't care about the
// stable/dynamic split (tests, init-time validation).
func (c *Curator) BuildSystemPromptString(reqCtx context.Context, cyc CycleContext) string {
	return c.BuildSystemPrompt(reqCtx, cyc).Concat()
}

func (c *Curator) handle(m message) {
	switch v := m.(type) {
	case buildPromptMsg:
		v.reply <- c.buildPrompt(v.reqCtx, v.cyc)
	}
}

// buildPrompt is pure given (config, cycleContext, librarian-snapshot).
// Splitting this out from handle makes the slot-assembly logic directly
// testable without spinning up an actor.
//
// reqCtx is forwarded to substrate calls that may take meaningful time
// (case retrieval -> embedder).
//
// The final prompt structure is:
//
//   Stable:
//     <persona text>
//     <bootstrap_skills>     (fresh session only)
//     <available_skills>     (when skills configured)
//   Dynamic:
//     <rendered preamble>
func (c *Curator) buildPrompt(reqCtx context.Context, cyc CycleContext) llm.SystemPrompt {
	slots, stats := c.assembleSlots(cyc)
	persona, preamble := c.cfg.Identity.RenderParts(slots)

	// Stable prefix: persona + bootstrap (if applicable) + available skills.
	var stable strings.Builder
	stable.WriteString(persona)
	if bootstrap := c.renderBootstrapSkills(stats); bootstrap != "" {
		stable.WriteString("\n\n")
		stable.WriteString(bootstrap)
	}
	if xml := c.renderAvailableSkills(); xml != "" {
		stable.WriteString("\n\n")
		stable.WriteString(xml)
	}

	// Dynamic suffix: sensorium block + preamble. Sensorium pulls
	// per-cycle ambient signals (clock, situation, vitals, memory
	// counts) from substrate and assembles SD's XML pattern. Empty
	// sections are filtered before the block is wrapped, so deferred
	// subsystems (scheduler, planner, etc.) contribute nothing today.
	var dynamic strings.Builder
	if sens := c.buildSensoriumBlock(reqCtx, cyc, &stats); sens != "" {
		dynamic.WriteString(sens)
	}
	if preamble != "" {
		if dynamic.Len() > 0 {
			dynamic.WriteString("\n\n")
		}
		dynamic.WriteString(preamble)
	}

	prompt := llm.SystemPrompt{
		Stable:  stable.String(),
		Dynamic: dynamic.String(),
	}

	c.emitAssembled(cyc, prompt, stats)
	return prompt
}

// buildSensoriumBlock collects the inputs the sensorium needs from the
// curator's collaborators and builds the XML block. The actual
// per-section render lives in sensorium.go; this is the wiring layer
// that decides what to pass in.
//
// reqCtx is forwarded to RetrieveCases so embedder cancellation
// propagates from the cog's dispatch ctx. When reqCtx is nil
// (legacy callers / pre-Phase-3 tests), context.Background is
// substituted to keep retrieval working.
//
// stats is mutated in place: when retrieval surfaces cases that
// pass the renderer's per-category + total caps, their IDs are
// written to stats.recalledCaseIDs so emitAssembled can include
// them on the curator_assembled cycle-log event.
func (c *Curator) buildSensoriumBlock(reqCtx context.Context, cyc CycleContext, stats *assemblyStats) string {
	if reqCtx == nil {
		reqCtx = context.Background()
	}
	var recent []librarian.NarrativeEntry
	if c.cfg.Librarian != nil {
		recent = c.cfg.Librarian.RecentNarrative(recentNarrativeForSlots)
	}
	var agentInfos []AgentInfo
	if c.cfg.Agents != nil {
		agentInfos = c.cfg.Agents.Agents()
	}
	var recalled []cbr.Scored
	if c.cfg.Librarian != nil && strings.TrimSpace(cyc.UserInput) != "" {
		recalled = c.cfg.Librarian.RetrieveCases(reqCtx, buildRecalledCasesQuery(cyc))
	}
	picked := pickRecalledCases(recalled)
	if stats != nil && len(picked) > 0 {
		ids := make([]string, 0, len(picked))
		for _, s := range picked {
			ids = append(ids, s.Case.ID)
		}
		stats.recalledCaseIDs = ids
	}
	var integrityFact *librarian.Fact
	if c.cfg.Librarian != nil {
		integrityFact = c.cfg.Librarian.GetFact(metalearning.FabricationAuditFactKey)
	}
	pendingCaptures := 0
	if c.cfg.Captures != nil {
		pendingCaptures = c.cfg.Captures.CountPending()
	}
	return buildSensorium(sensoriumInputs{
		now:            c.cfg.NowFn(),
		cycleID:        cyc.CycleID,
		sessionSince:   c.cfg.SessionSince,
		inputSource:    cyc.InputSource,
		queueDepth:     cyc.QueueDepth,
		messageCount:   cyc.MessageCount,
		agentsActive:   c.cfg.AgentsActive,
		agents:         agentInfos,
		recentEntries:  recent,
		narrativeCount: stats.narrativeEntries,
		factCount:      stats.factCount,
		caseCount:      stats.caseCount,
		ambient:        cyc.AmbientSignals,
		integrityFact:   integrityFact,
		pendingCaptures: pendingCaptures,
		recalledCases:  picked,
	})
}

// buildRecalledCasesQuery turns a CycleContext into a cbr.Query for the
// sensorium's <recalled_cases> retrieval. Today the query is intentionally
// minimal — Intent text comes from the user input verbatim, and the rest
// of the signals (Domain, Keywords, Entities, QueryComplexity) wait on
// per-cycle classifiers that haven't landed.
//
// Setting MaxResults to 0 lets CaseBase use its DefaultMaxResults (4) as
// the K cap. The renderer further partitions per-category, so the
// effective surface in the prompt is K_per_category × number_of_categories.
func buildRecalledCasesQuery(cyc CycleContext) cbr.Query {
	return cbr.Query{
		Intent:     strings.TrimSpace(cyc.UserInput),
		MaxResults: recalledCasesQueryMaxResults,
	}
}

// renderAvailableSkills filters the configured skills for the
// cognitive agent + active query domains and returns the
// <available_skills> XML block. Empty when no skills configured or
// none match the filters.
func (c *Curator) renderAvailableSkills() string {
	if len(c.cfg.Skills) == 0 {
		return ""
	}
	scoped := skills.ForAgent(c.cfg.Skills, "cognitive")
	scoped = skills.ForContext(scoped, nil) // domain extraction is deferred
	return skills.ToSystemPromptXML(scoped)
}

// renderBootstrapSkills inlines the bodies of configured bootstrap
// skill IDs on the first cycle of a fresh session. "Fresh session"
// means the librarian returned zero recent narrative entries — the
// agent has nothing to lean on, so the procedures get inlined
// verbatim rather than waiting on a `read_skill` round-trip.
//
// Returns "" when:
//
//   - the recent narrative is non-empty (not a fresh session),
//   - BootstrapSkillIDs is empty (operator opt-out),
//   - none of the configured IDs resolve against c.cfg.Skills,
//   - or all the matching skill bodies fail to read from disk.
//
// Bodies that fail to read are silently skipped (best-effort) so
// one missing file doesn't take out the whole bootstrap.
func (c *Curator) renderBootstrapSkills(stats assemblyStats) string {
	if stats.narrativeEntries > 0 {
		return ""
	}
	if len(c.cfg.BootstrapSkillIDs) == 0 || len(c.cfg.Skills) == 0 {
		return ""
	}
	type bodied struct{ id, name, body string }
	var bodies []bodied
	for _, wantID := range c.cfg.BootstrapSkillIDs {
		var meta *skills.SkillMeta
		for i := range c.cfg.Skills {
			if c.cfg.Skills[i].ID == wantID {
				meta = &c.cfg.Skills[i]
				break
			}
		}
		if meta == nil {
			continue
		}
		body, err := os.ReadFile(meta.Path)
		if err != nil {
			c.cfg.Logger.Warn("curator: bootstrap skill body unreadable; skipping",
				"id", wantID, "path", meta.Path, "err", err)
			continue
		}
		bodies = append(bodies, bodied{id: wantID, name: meta.Name, body: string(body)})
	}
	if len(bodies) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("<!-- Bootstrap: the bodies of a few critical skills, ")
	b.WriteString("inlined on this fresh-session first cycle so you can ")
	b.WriteString("act before having to call read_skill. After this cycle ")
	b.WriteString("use read_skill on demand. -->\n<bootstrap_skills>\n")
	for i, body := range bodies {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "  <skill id=%q name=%q>\n%s\n  </skill>", body.id, body.name, body.body)
	}
	b.WriteString("\n</bootstrap_skills>")
	return b.String()
}

// assemblyStats is the librarian-visible state at the moment of build.
// Used both to populate the curator_assembled cycle-log event and as a
// directly-testable return from assembleSlots.
type assemblyStats struct {
	narrativeEntries int
	factSampleCount  int
	factCount        int
	caseCount        int
	// recalledCaseIDs is the list of (full) case IDs the sensorium
	// actually surfaced via <recalled_cases> after per-category +
	// total-cap filtering. Recorded in the curator_assembled event
	// so audit + integration tests can verify "this case reached
	// the agent" without parsing the prompt body.
	recalledCaseIDs []string
}

// emitAssembled writes a curator_assembled event with a fresh work-unit
// ID and parent_id pointing to the cog cycle. The cog cycle drives
// everything — every piece of sub-actor work (curator, future agents)
// is parented to it so an inspect tool can walk the tree.
//
// PromptChars records the total assembled length (Stable + Dynamic) so
// the existing audit shape doesn't change.
func (c *Curator) emitAssembled(cyc CycleContext, prompt llm.SystemPrompt, stats assemblyStats) {
	if c.cfg.CycleLog == nil || cyc.CycleID == "" {
		// No sink, or no cog cycle to attribute to — emission is purely
		// for audit, so silently skip rather than spam unattributable
		// events. ParentID is the load-bearing link; without a cog
		// cycle we can't write a useful event.
		return
	}
	assemblyID := c.cfg.IDFn()
	var recalledIDs []string
	if len(stats.recalledCaseIDs) > 0 {
		recalledIDs = make([]string, 0, len(stats.recalledCaseIDs))
		for _, id := range stats.recalledCaseIDs {
			short := id
			if len(short) > 8 {
				short = short[:8]
			}
			recalledIDs = append(recalledIDs, short)
		}
	}
	if err := c.cfg.CycleLog.Emit(cyclelog.Event{
		Type:             cyclelog.EventCuratorAssembled,
		CycleID:          assemblyID,
		ParentID:         cyc.CycleID,
		PromptChars:      len(prompt.Concat()),
		NarrativeEntries: stats.narrativeEntries,
		FactSampleCount:  stats.factSampleCount,
		FactCount:        stats.factCount,
		RecalledCaseIDs:  recalledIDs,
	}); err != nil {
		c.cfg.Logger.Warn("curator: cyclelog emit failed",
			"assembly_id", assemblyID,
			"cog_cycle_id", cyc.CycleID,
			"err", err)
	}
}

// assembleSlots merges static (agent, workspace, time) + cycle (input
// source, queue depth) + memory (librarian counts, recent narrative,
// recent facts) slots. Memory slots are empty strings when the librarian
// is nil or returns nothing — OMIT-IF-EMPTY rules in the preamble
// template drop the corresponding lines.
//
// Returns the slot map plus an assemblyStats snapshot so callers (and
// the curator_assembled cycle-log event) can audit what the librarian
// surfaced this cycle without re-querying.
func (c *Curator) assembleSlots(cyc CycleContext) (map[string]string, assemblyStats) {
	var stats assemblyStats
	now := c.cfg.NowFn()
	slots := identity.ContextToSlots(identity.Context{
		AgentName: c.cfg.AgentName,
		Workspace: c.cfg.Workspace,
		Now:       now,
	})
	slots["agent_version"] = c.cfg.AgentVersion

	// Cycle-context slots. Empty CycleID just means "no current cycle"
	// (e.g. startup-time prompt build); the slot stays empty.
	slots["cycle_id"] = cyc.CycleID
	slots["input_source"] = inputSource(cyc.InputSource)
	slots["queue_depth"] = intToStr(cyc.QueueDepth)
	slots["message_count"] = intToStr(cyc.MessageCount)

	// Memory-derived slots. All default to empty when the librarian is
	// absent — the preamble's OMIT-IF rules clean up the gaps.
	slots["persistent_fact_count"] = ""
	slots["recent_fact_sample"] = ""
	slots["recent_narrative"] = ""
	slots["last_session_summary"] = ""

	if c.cfg.Librarian == nil {
		return slots, stats
	}

	if n := c.cfg.Librarian.PersistentFactCount(); n > 0 {
		slots["persistent_fact_count"] = intToStr(n)
		stats.factCount = n
	}
	if facts := c.cfg.Librarian.RecentPersistentFacts(recentFactsForSlots); len(facts) > 0 {
		slots["recent_fact_sample"] = formatFacts(facts)
		stats.factSampleCount = len(facts)
	}
	if entries := c.cfg.Librarian.RecentNarrative(recentNarrativeForSlots); len(entries) > 0 {
		slots["recent_narrative"] = formatNarrative(entries)
		slots["last_session_summary"] = mostRecentSummary(entries)
		stats.narrativeEntries = len(entries)
	}
	stats.caseCount = c.cfg.Librarian.CaseCount()
	return slots, stats
}

// formatFacts renders persistent facts as a bullet list:
//
//	- key = value (confidence: 0.85)
//
// Newest first (matches RecentPersistentFacts ordering).
func formatFacts(facts []librarian.Fact) string {
	var b strings.Builder
	for i, f := range facts {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "- %s = %s (confidence: %.2f)", f.Key, f.Value, f.Confidence)
	}
	return b.String()
}

// formatNarrative renders recent entries oldest-first as a bullet list
// with truncated summaries. Mirrors Springdrift's curator preamble shape:
//
//	- HH:MM: <summary truncated to N chars>
func formatNarrative(entries []librarian.NarrativeEntry) string {
	// We get newest-last from RecentNarrative; render in chronological
	// order so the preamble reads naturally as a timeline.
	var b strings.Builder
	for i, e := range entries {
		if i > 0 {
			b.WriteString("\n")
		}
		summary := truncate(strings.TrimSpace(e.Summary), recentEntrySummaryChars)
		fmt.Fprintf(&b, "- %s: %s", e.Timestamp.Format("15:04"), summary)
	}
	return b.String()
}

// mostRecentSummary returns the summary of the most recent entry in
// `entries`. RecentNarrative returns newest-last, so that's the final
// element.
func mostRecentSummary(entries []librarian.NarrativeEntry) string {
	if len(entries) == 0 {
		return ""
	}
	return strings.TrimSpace(entries[len(entries)-1].Summary)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func inputSource(s string) string {
	if s == "" {
		return "user"
	}
	return s
}

func intToStr(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d", n)
}

// message is the curator inbox sum type.
type message interface{ isCuratorMsg() }

type buildPromptMsg struct {
	reqCtx context.Context
	cyc    CycleContext
	reply  chan<- llm.SystemPrompt
}

func (buildPromptMsg) isCuratorMsg() {}
