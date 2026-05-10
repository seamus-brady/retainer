package identity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ctx() Context {
	return Context{
		AgentName: "Nemo",
		Workspace: "/tmp/ws",
		Now:       time.Date(2026, 4, 30, 9, 0, 0, 0, time.UTC),
	}
}

func TestContextToSlots(t *testing.T) {
	slots := ContextToSlots(ctx())
	if slots["agent_name"] != "Nemo" {
		t.Errorf("agent_name = %q", slots["agent_name"])
	}
	if slots["workspace"] != "/tmp/ws" {
		t.Errorf("workspace = %q", slots["workspace"])
	}
	if slots["date"] != "2026-04-30" {
		t.Errorf("date = %q", slots["date"])
	}
	if !strings.HasPrefix(slots["time"], "09:00") {
		t.Errorf("time = %q, want 09:00 prefix", slots["time"])
	}
}

func TestRender_AddsMemoryDerivedSlots(t *testing.T) {
	id := &Identity{
		Persona:  "I am {{agent_name}}.",
		Preamble: "Recent threads: {{thread_count}}\nFresh fact: {{fresh_fact}} [OMIT IF EMPTY]",
	}
	slots := ContextToSlots(ctx())
	slots["thread_count"] = "3"
	slots["fresh_fact"] = "" // OMIT IF EMPTY drops the line
	got := id.Render(slots)
	if !strings.Contains(got, "I am Nemo.") {
		t.Errorf("persona missing: %q", got)
	}
	if !strings.Contains(got, "Recent threads: 3") {
		t.Errorf("memory slot missing: %q", got)
	}
	if strings.Contains(got, "Fresh fact:") {
		t.Errorf("OMIT IF EMPTY should drop fresh_fact line: %q", got)
	}
}

func TestRender_OmitsZeroCount_PrefixForm(t *testing.T) {
	// Springdrift's rule fires when the trimmed line starts with "0 " —
	// the slot-builder convention is "<count> <label>" (e.g. "0 cycles
	// today" / "3 cycles today") so the count comes first.
	id := &Identity{
		Persona:  "I am {{agent_name}}.",
		Preamble: "{{summary_zero}} [OMIT IF ZERO]\n{{summary_three}} [OMIT IF ZERO]",
	}
	slots := ContextToSlots(ctx())
	slots["summary_zero"] = "0 active threads"
	slots["summary_three"] = "3 active threads"
	got := id.Render(slots)
	if strings.Contains(got, "0 active threads") {
		t.Errorf("'0 active threads' should drop: %q", got)
	}
	if !strings.Contains(got, "3 active threads") {
		t.Errorf("'3 active threads' should remain: %q", got)
	}
}

func TestRender_OmitsZeroCount_MidString(t *testing.T) {
	// " 0 " inside a sentence also matches — Springdrift's second arm.
	id := &Identity{Persona: "I am {{agent_name}}.", Preamble: "{{line}} [OMIT IF ZERO]"}
	slots := ContextToSlots(ctx())
	slots["line"] = "Today had 0 successful cycles"
	got := id.Render(slots)
	if strings.Contains(got, "Today had") {
		t.Errorf("' 0 ' mid-string should drop: %q", got)
	}
}

func TestRender_OmitsZeroCount_DoesNotMatchTrailingZeroAlone(t *testing.T) {
	// Defensive — pin Springdrift's rule shape. "Active threads: 0"
	// (trailing zero, no space after) does NOT match: the rule
	// requires "0 " or " 0 " specifically. Slot builders that want
	// the line to drop must produce "0 <label>"-shaped text.
	id := &Identity{Persona: "I am {{agent_name}}.", Preamble: "Active threads: {{count}} [OMIT IF ZERO]"}
	slots := ContextToSlots(ctx())
	slots["count"] = "0"
	got := id.Render(slots)
	if !strings.Contains(got, "Active threads: 0") {
		t.Errorf("trailing-zero-alone should NOT match Springdrift's rule: %q", got)
	}
}

func TestRender_OmitsZeroCount_NotForNonZeroPrefix(t *testing.T) {
	id := &Identity{Persona: "I am {{agent_name}}.", Preamble: "{{summary}} [OMIT IF ZERO]"}
	slots := ContextToSlots(ctx())
	slots["summary"] = "10 active threads"
	got := id.Render(slots)
	if !strings.Contains(got, "10 active threads") {
		t.Errorf("'10 active threads' should NOT match zero rule: %q", got)
	}
}

func TestRender_DropsLinesWithUnresolvedSlots(t *testing.T) {
	id := &Identity{
		Persona:  "I am {{agent_name}}.\nI live at {{nonexistent_slot}}.",
		Preamble: "",
	}
	got := id.Render(ContextToSlots(ctx()))
	if !strings.Contains(got, "I am Nemo.") {
		t.Errorf("kept line missing: %q", got)
	}
	if strings.Contains(got, "I live at") {
		t.Errorf("unresolved-slot line should be dropped: %q", got)
	}
}

func TestBuildSystemPrompt_StillWorks(t *testing.T) {
	// BuildSystemPrompt should remain a thin wrapper over Render so
	// existing callers (tests, init-time wiring) don't have to change.
	id := &Identity{Persona: "I am {{agent_name}} at {{workspace}}.", Preamble: ""}
	got := id.BuildSystemPrompt(ctx())
	if !strings.Contains(got, "I am Nemo at /tmp/ws.") {
		t.Errorf("got %q", got)
	}
}

func TestLoad_UsesEmbeddedDefaultsWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	id, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(id.Persona, "{{agent_name}}") {
		t.Fatalf("default persona missing slot: %q", id.Persona)
	}
	if !strings.Contains(id.Source, "embedded") {
		t.Errorf("source = %q, want 'embedded' marker", id.Source)
	}
}

func TestLoad_ReadsFilesWhenPresent(t *testing.T) {
	dir := t.TempDir()
	identityDir := filepath.Join(dir, IdentitySubdir)
	if err := os.MkdirAll(identityDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(identityDir, PersonaFilename), []byte("custom persona"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(identityDir, PreambleFilename), []byte("custom preamble"), 0o644); err != nil {
		t.Fatal(err)
	}

	id, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if id.Persona != "custom persona" {
		t.Fatalf("persona = %q", id.Persona)
	}
	if id.Preamble != "custom preamble" {
		t.Fatalf("preamble = %q", id.Preamble)
	}
}

func TestBuildSystemPrompt_SubstitutesSlots(t *testing.T) {
	id := &Identity{
		Persona:  "You are {{agent_name}}.",
		Preamble: "Today is {{date}}.",
	}
	got := id.BuildSystemPrompt(ctx())
	if !strings.Contains(got, "You are Nemo.") {
		t.Errorf("agent_name not substituted: %q", got)
	}
	if !strings.Contains(got, "Today is 2026-04-30.") {
		t.Errorf("date not substituted: %q", got)
	}
}

func TestBuildSystemPrompt_DropsLinesWithUnresolvedSlots(t *testing.T) {
	id := &Identity{
		Persona:  "Hello {{agent_name}}.\nUnknown {{not_a_real_slot}} here.\nPath: {{workspace}}.",
		Preamble: "",
	}
	got := id.BuildSystemPrompt(ctx())
	if strings.Contains(got, "Unknown") {
		t.Errorf("unresolved-slot line not dropped: %q", got)
	}
	if !strings.Contains(got, "Hello Nemo.") {
		t.Error("resolved persona line missing")
	}
	if !strings.Contains(got, "Path: /tmp/ws.") {
		t.Error("workspace line missing")
	}
}

func TestBuildSystemPrompt_OmitIfEmptyDropsBlankLine(t *testing.T) {
	id := &Identity{
		Persona:  "Persona.",
		Preamble: "Threads: {{thread_count}} [OMIT IF EMPTY]\nKept: stuff [OMIT IF EMPTY]",
	}
	// thread_count slot is unresolved, so the first line drops via the
	// unresolved-slot rule (before OMIT IF even fires). The second line
	// has content, OMIT IF EMPTY does nothing, line is kept with tag
	// stripped.
	got := id.BuildSystemPrompt(ctx())
	if strings.Contains(got, "Threads:") {
		t.Errorf("threads line should be dropped: %q", got)
	}
	if !strings.Contains(got, "Kept: stuff") {
		t.Errorf("kept line missing or tag not stripped: %q", got)
	}
	if strings.Contains(got, "[OMIT IF EMPTY]") {
		t.Errorf("OMIT IF tag not stripped: %q", got)
	}
}

func TestBuildSystemPrompt_OmitIfEmptyDropsTrailingColon(t *testing.T) {
	// A line that resolves to "Title:" with trailing colon should drop.
	id := &Identity{
		Persona:  "Persona.",
		Preamble: "Title: {{agent_name}} [OMIT IF EMPTY]\nLabel: [OMIT IF EMPTY]",
	}
	// First line resolves to "Title: Nemo" → kept. Second line: agent_name
	// not present, so the tag has only "Label:" left → trailing colon → drop.
	got := id.BuildSystemPrompt(Context{AgentName: "Nemo", Now: time.Now()})
	if !strings.Contains(got, "Title: Nemo") {
		t.Errorf("first line missing: %q", got)
	}
	if strings.Contains(got, "Label:") {
		t.Errorf("trailing-colon line should drop: %q", got)
	}
}

func TestBuildSystemPrompt_PersonaAndPreambleJoinedWithBlankLine(t *testing.T) {
	id := &Identity{
		Persona:  "Persona text.",
		Preamble: "Preamble text.",
	}
	got := id.BuildSystemPrompt(ctx())
	if !strings.Contains(got, "Persona text.\n\nPreamble text.") {
		t.Errorf("expected blank line between sections: %q", got)
	}
}

func TestBuildSystemPrompt_EmptyPreambleProducesNoTrailingNewlines(t *testing.T) {
	id := &Identity{
		Persona:  "Just persona.",
		Preamble: "",
	}
	got := id.BuildSystemPrompt(ctx())
	if got != "Just persona." {
		t.Errorf("got %q, want %q", got, "Just persona.")
	}
}

func TestEmbeddedFiles_HasBothDefaults(t *testing.T) {
	files := EmbeddedFiles()
	if _, ok := files[PersonaFilename]; !ok {
		t.Error("EmbeddedFiles missing persona.md")
	}
	if _, ok := files[PreambleFilename]; !ok {
		t.Error("EmbeddedFiles missing session_preamble.md")
	}
}

// ---- persona content guards ----
//
// These assertions pin claims the persona makes, and claims it must
// NOT make. Each entry corresponds to a row in
// doc/roadmap/planned/persona-implementation.md — when a deferred
// subsystem ships, that subsystem's PR moves the matching guard from
// "absent" to "present."

func TestPersona_NoVerneOrCaptainNemoReference(t *testing.T) {
	// {{agent_name}} is operator-configurable; "Nemo" is just the default.
	// The persona must not anchor itself to Verne's Captain Nemo.
	body := string(embeddedPersona)
	for _, banned := range []string{"Verne", "Captain Nemo", "namesake"} {
		if strings.Contains(body, banned) {
			t.Errorf("persona contains %q — should be name-agnostic", banned)
		}
	}
}

func TestPersona_RendersWithDifferentAgentName(t *testing.T) {
	// Drop-in pin: the persona reads sensibly when {{agent_name}} is
	// not "Nemo". Guards against a future edit hard-coding the default.
	id, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rendered := id.Render(ContextToSlots(Context{
		AgentName: "Sage",
		Workspace: "/tmp/ws",
		Now:       time.Now(),
	}))
	if !strings.Contains(rendered, "I am Sage.") {
		t.Errorf("agent_name slot didn't render: %q", rendered[:200])
	}
	if strings.Contains(rendered, "{{agent_name}}") {
		t.Errorf("unresolved slot leaked: %q", rendered)
	}
}

func TestPersona_DoesNotDeclareWorkspace(t *testing.T) {
	// The "My workspace is {{workspace}}." line was dropped per
	// operator request — operational details aren't recited unless
	// directly relevant. The {{workspace}} slot remains available
	// in ContextToSlots; the persona just doesn't surface it as a
	// declarative line.
	body := string(embeddedPersona)
	if strings.Contains(body, "My workspace is") {
		t.Error("persona reintroduced 'My workspace is' line")
	}
	if strings.Contains(body, "{{workspace}}") {
		t.Error("persona references {{workspace}} slot — should be removed")
	}
}

func TestPersona_HasArtificialRetainerFraming(t *testing.T) {
	// The Artificial-Retainer framing is the persona's load-bearing
	// category claim. After the anti-recital pass, it lives in a
	// single short opening — "Retainer is an Artificial Retainer
	// system" — so the agent doesn't have a quotable role-definition
	// paragraph to mine for self-introductions. Pin both the category
	// claim and the principal/operator concept.
	body := string(embeddedPersona)
	for _, want := range []string{
		"Artificial Retainer",
		"Retainer agent",
		"principal",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("persona missing framing phrase %q", want)
		}
	}
}

func TestPersona_NoFabricatedCodeReferencesGuidance(t *testing.T) {
	// 2026-05-09 incident: Nemo produced a confident-sounding root-
	// cause analysis citing `result.Location == nil` for a worker
	// panic — but no such field exists anywhere in the codebase.
	// The agent reached for a plausible Go error pattern rather than
	// admitting it didn't have a stack trace. The persona's
	// "Plausibility is not evidence" rule existed but wasn't load-
	// bearing enough on this specific failure mode. The new
	// "Code references must be grounded" rule is the surgical fix —
	// pin it so a future edit doesn't drop it.
	body := string(embeddedPersona)
	for _, want := range []string{
		"Code references must be grounded",
		"actually inspected that code in the current cycle",
		"I do not invent the reference",
		"hard rule",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("persona missing fabrication-prevention phrase %q", want)
		}
	}
}

func TestPersona_AntiRecitalGuidance(t *testing.T) {
	// When asked who/what they are, the agent should keep it short
	// and concrete rather than reciting the role description back.
	// Pin the explicit instruction so a future edit doesn't drop
	// it and let the persona drift back into self-recital territory.
	body := string(embeddedPersona)
	for _, want := range []string{
		"who I am",
		"don't recite my role",
		"how I introduce myself",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("persona missing anti-recital phrase %q", want)
		}
	}
}

func TestPersona_WalksConversationBeforeFacts(t *testing.T) {
	// 2026-05-09 incident: a webui reconnect mid-session left Nemo
	// with the chat history visibly above but the agent reaching for
	// memory_read when the operator said "send it as an attachment
	// now?" — even though "it" had been named two messages earlier.
	// The agent reported "no facts stored" instead of using the
	// referent right above. Pin the rule so a future edit doesn't
	// drop the discipline and the agent slides back into asking
	// "what file?" when the answer is in the prior message.
	body := string(embeddedPersona)
	for _, want := range []string{
		"walk recent conversation",
		"shared context",
		"crossing session boundaries",
		"different things",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("persona missing chat-first-then-facts phrase %q", want)
		}
	}
}

func TestPersona_NoRecitalFuel(t *testing.T) {
	// Defensive: previous persona drafts had quotable taglines that
	// the model mined for self-introductions ("I do the work, report
	// what I know and don't know, and carry the institutional memory";
	// "doesn't perform the act of being helpful in place of actually
	// helping"; "I'm a colleague, not a guardrail"). Pin those out of
	// the file. The principles they expressed are still in the persona
	// — but rephrased so they read as instructions to the agent, not
	// as recital fodder.
	body := string(embeddedPersona)
	for _, banned := range []string{
		"carry the institutional memory",
		"perform the act of being helpful in place of actually helping",
		"I'm a colleague, not a guardrail",
		"running instance",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("persona contains recital-magnet phrase %q", banned)
		}
	}
}

func TestPersona_HonestyDisciplineSubprinciples(t *testing.T) {
	body := string(embeddedPersona)
	for _, want := range []string{
		"Claiming vs. doing",
		"Tool naming is binding",
		"Observation vs. explanation",
		"Not knowing",
		"I don't know — let me check",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("persona missing honesty-discipline phrase %q", want)
		}
	}
}

func TestPersona_JudgmentPosture(t *testing.T) {
	// The "say it once, then execute" posture survives the anti-
	// recital pass — but the standalone "I'm a colleague, not a
	// guardrail" tagline is gone (model was extending it as
	// "not a guardrail, not a therapist, not a novelty"). Pin the
	// operative principle, not the slogan.
	body := string(embeddedPersona)
	for _, want := range []string{
		"once, clearly",
		"push back",
		"not to refuse work",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("persona missing judgment phrase %q", want)
		}
	}
}

func TestPersona_HumorRecognition(t *testing.T) {
	// Reading register is part of judgment. The agent must recognise
	// jokes / banter / absurdist prompts and respond in kind rather
	// than treating every input as a research task — and it must
	// push back before delegating real cycles (Brave search,
	// researcher dispatch) on something clearly not a real query.
	// The example in the persona references the actual failure that
	// motivated this guidance ("fart backwards through time" → Brave
	// search) so a future edit can't drop the principle without also
	// dropping the cautionary tale.
	body := string(embeddedPersona)
	for _, want := range []string{
		"obviously a joke",
		"match the register",
		"isn't a deadpan response to a joke",
		"push back once before complying",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("persona missing humor-recognition phrase %q", want)
		}
	}
}

func TestPersona_DelegationAndSkills(t *testing.T) {
	body := string(embeddedPersona)
	for _, want := range []string{
		"single point of control",
		"researcher",
		"observer",
		"agent is my hands, not my brain",
		"read_skill",
		"<available_skills>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("persona missing delegation/skills phrase %q", want)
		}
	}
}

func TestPersona_NoReferencesToDeferredSubsystems(t *testing.T) {
	// Defensive: each absent string corresponds to a Tier 2 row in
	// doc/roadmap/planned/persona-implementation.md. When the
	// underlying subsystem ships, that subsystem's PR will *both*
	// add the persona claim back and delete the matching guard
	// here. Catching a stray reintroduction is the whole point of
	// this test — the persona is a runtime contract, and a claim
	// without substrate is a hallucination waiting to happen.
	body := string(embeddedPersona)
	for _, banned := range []string{
		// CBR / tasks / threads / strategies / learning goals
		"CBR cases",
		"strategies, learning goals",
		"active tasks",
		"create an endeavour",
		"endeavour",
		// Tools we don't have
		"get_active_work",
		"introspect",
		// Specialist agents we don't have
		"the planner",
		"to the planner",
		// Sensorium block is substrate-shipped (Phase 3 onward —
		// recalled_cases, strategies, agents, vitals, etc.) so the
		// persona may reference it. Affect remains deferred.
		"affect readings",
		"My affect is stable",
		// Scheduler
		"Scheduler discipline",
		"scheduled job",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("persona reintroduced deferred-subsystem reference %q", banned)
		}
	}
}

func TestPersona_NoNormativeCalculusSubstrateClaim(t *testing.T) {
	// CLAUDE.md cuts normative calculus / FlourishingVerdict / Stoic
	// axioms entirely. The persona must not claim them as substrate.
	// The Internal-discipline section's *operative* content (don't
	// perform philosophy / discipline; practice is quiet) survives;
	// the substrate claim does not.
	body := string(embeddedPersona)
	for _, banned := range []string{
		"philosophical traditions that shape",
		"normative calculus",
		"FlourishingVerdict",
		"Stoic axioms",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("persona contains out-of-scope substrate claim %q", banned)
		}
	}
}
