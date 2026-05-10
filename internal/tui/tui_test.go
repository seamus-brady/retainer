package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/seamus-brady/retainer/internal/agent"
	"github.com/seamus-brady/retainer/internal/cog"
	"github.com/seamus-brady/retainer/internal/llm"
	"github.com/seamus-brady/retainer/internal/tui/mdrender"
)

// modelWith builds a Model with busy=true and a recorded cog Activity
// for terse test setup. The agentActivities map is initialised so
// nested-label tests can populate specific agents' state.
func modelWith(act cog.Activity) Model {
	return Model{
		busy:            true,
		activity:        act,
		agentActivities: make(map[string]agent.Activity),
	}
}

// statusLabel is the only TUI logic worth direct-testing today — the
// rest is Bubble Tea wiring that's better exercised end-to-end. These
// tests pin down the label transitions a user actually sees, so we
// don't accidentally regress to "thinking..." for everything.

func TestStatusLabel_IdleWhenNotBusy(t *testing.T) {
	m := Model{}
	if got := m.statusLabel(); got != "idle" {
		t.Errorf("got %q, want idle", got)
	}
}

func TestStatusLabel_FallsBackToThinkingWhenBusyWithoutActivity(t *testing.T) {
	// The brief window between busy=true and the first activity push
	// should still show *something* useful.
	m := Model{busy: true}
	if got := m.statusLabel(); got != "thinking" {
		t.Errorf("got %q, want thinking", got)
	}
}

func TestStatusLabel_EvaluatingPolicy(t *testing.T) {
	m := Model{busy: true, activity: cog.Activity{Status: cog.StatusEvaluatingPolicy}}
	if got := m.statusLabel(); got != "evaluating policy" {
		t.Errorf("got %q, want 'evaluating policy'", got)
	}
}

func TestStatusLabel_ThinkingFirstTurn(t *testing.T) {
	m := Model{busy: true, activity: cog.Activity{Status: cog.StatusThinking, Turn: 1, MaxTurns: 5}}
	if got := m.statusLabel(); got != "thinking" {
		t.Errorf("turn 1 should not show count, got %q", got)
	}
}

func TestStatusLabel_ThinkingLaterTurnShowsCount(t *testing.T) {
	m := Model{busy: true, activity: cog.Activity{Status: cog.StatusThinking, Turn: 3, MaxTurns: 5}}
	if got := m.statusLabel(); got != "thinking (turn 3/5)" {
		t.Errorf("got %q", got)
	}
}

func TestStatusLabel_UsingOneTool(t *testing.T) {
	m := Model{busy: true, activity: cog.Activity{Status: cog.StatusUsingTools, ToolNames: []string{"brave_web_search"}}}
	if got := m.statusLabel(); got != "using tool: brave_web_search" {
		t.Errorf("got %q", got)
	}
}

func TestStatusLabel_UsingMultipleTools(t *testing.T) {
	m := Model{busy: true, activity: cog.Activity{
		Status:    cog.StatusUsingTools,
		ToolNames: []string{"brave_web_search", "jina_reader"},
	}}
	if got := m.statusLabel(); got != "using tools: brave_web_search, jina_reader" {
		t.Errorf("got %q", got)
	}
}

func TestStatusLabel_UsingToolsNoNames(t *testing.T) {
	// Defensive: if UsingTools fires with empty ToolNames (shouldn't,
	// but) we still want a usable label rather than a crash or
	// trailing colon.
	m := Model{busy: true, activity: cog.Activity{Status: cog.StatusUsingTools}}
	if got := m.statusLabel(); got != "using tools" {
		t.Errorf("got %q", got)
	}
}

func TestStatusLabel_IdleStatusFallsBackToThinking(t *testing.T) {
	// If a final StatusIdle activity arrives before the assistantReply
	// flips busy=false, we'd briefly hit this state. Show a generic
	// thinking label rather than "idle" while busy is still true.
	m := Model{busy: true, activity: cog.Activity{Status: cog.StatusIdle}}
	if got := m.statusLabel(); got != "thinking" {
		t.Errorf("got %q, want thinking (race fallback)", got)
	}
}

// ---- nested agent labels (PR 1) ----

func TestStatusLabel_NestedAgentThinking(t *testing.T) {
	// Cog is mid-delegation; researcher reports Thinking. Status
	// should be the nested view, not the cog's "using tool: agent_X".
	m := modelWith(cog.Activity{Status: cog.StatusUsingTools, ToolNames: []string{"agent_researcher"}})
	m.agentActivities["researcher"] = agent.Activity{
		AgentName: "researcher", Status: agent.StatusThinking, Turn: 1, MaxTurns: 8,
	}
	if got := m.statusLabel(); got != "researcher: thinking" {
		t.Errorf("got %q, want 'researcher: thinking'", got)
	}
}

func TestStatusLabel_NestedAgentUsingTool(t *testing.T) {
	m := modelWith(cog.Activity{Status: cog.StatusUsingTools, ToolNames: []string{"agent_researcher"}})
	m.agentActivities["researcher"] = agent.Activity{
		AgentName: "researcher",
		Status:    agent.StatusUsingTools,
		ToolNames: []string{"brave_web_search"},
	}
	if got := m.statusLabel(); got != "researcher: using tool: brave_web_search" {
		t.Errorf("got %q", got)
	}
}

func TestStatusLabel_NestedAgentMultipleTools(t *testing.T) {
	m := modelWith(cog.Activity{Status: cog.StatusUsingTools, ToolNames: []string{"agent_researcher"}})
	m.agentActivities["researcher"] = agent.Activity{
		AgentName: "researcher",
		Status:    agent.StatusUsingTools,
		ToolNames: []string{"brave_web_search", "jina_reader"},
	}
	if got := m.statusLabel(); got != "researcher: using tools: brave_web_search, jina_reader" {
		t.Errorf("got %q", got)
	}
}

func TestStatusLabel_NestedAgentLaterTurnShowsCount(t *testing.T) {
	m := modelWith(cog.Activity{Status: cog.StatusUsingTools, ToolNames: []string{"agent_researcher"}})
	m.agentActivities["researcher"] = agent.Activity{
		AgentName: "researcher", Status: agent.StatusThinking, Turn: 4, MaxTurns: 8,
	}
	if got := m.statusLabel(); got != "researcher: thinking (turn 4/8)" {
		t.Errorf("got %q", got)
	}
}

func TestStatusLabel_NestedFallsBackWhenAgentNotPushed(t *testing.T) {
	// Cog says "using agent_researcher" but no agent activity received
	// yet — fall back to the cog-level label so the user sees something.
	m := modelWith(cog.Activity{Status: cog.StatusUsingTools, ToolNames: []string{"agent_researcher"}})
	if got := m.statusLabel(); got != "using tool: agent_researcher" {
		t.Errorf("got %q (no agent activity yet)", got)
	}
}

func TestStatusLabel_NestedFallsBackWhenAgentIdle(t *testing.T) {
	// If the agent's most recent state is Idle (e.g. it just
	// finished but the cog hasn't deactivated yet), show the cog
	// label rather than "researcher: " (empty inner label).
	m := modelWith(cog.Activity{Status: cog.StatusUsingTools, ToolNames: []string{"agent_researcher"}})
	m.agentActivities["researcher"] = agent.Activity{
		AgentName: "researcher", Status: agent.StatusIdle,
	}
	if got := m.statusLabel(); got != "using tool: agent_researcher" {
		t.Errorf("got %q (agent idle should fall back)", got)
	}
}

func TestStatusLabel_NonAgentToolUnchanged(t *testing.T) {
	// Cog calling memory_write directly — no agent_<name> prefix —
	// should display the cog label as before.
	m := modelWith(cog.Activity{Status: cog.StatusUsingTools, ToolNames: []string{"memory_write"}})
	m.agentActivities["researcher"] = agent.Activity{
		AgentName: "researcher", Status: agent.StatusThinking,
	}
	if got := m.statusLabel(); got != "using tool: memory_write" {
		t.Errorf("got %q", got)
	}
}

func TestStatusLabel_MultipleCogToolsNoNestingEvenIfOneIsAgent(t *testing.T) {
	// When the cog dispatches multiple tools in one turn, only one
	// of which is a delegation, surface the cog summary rather than
	// guessing which to nest. (Springdrift's parallel-dispatch UX
	// is a future enhancement.)
	m := modelWith(cog.Activity{
		Status:    cog.StatusUsingTools,
		ToolNames: []string{"memory_write", "agent_researcher"},
	})
	m.agentActivities["researcher"] = agent.Activity{
		AgentName: "researcher", Status: agent.StatusThinking,
	}
	if got := m.statusLabel(); got != "using tools: memory_write, agent_researcher" {
		t.Errorf("got %q", got)
	}
}

// ---- agentNameFromToolName ----

func TestAgentNameFromToolName_StripsPrefix(t *testing.T) {
	name, ok := agentNameFromToolName("agent_researcher")
	if !ok || name != "researcher" {
		t.Errorf("got (%q, %v)", name, ok)
	}
}

func TestAgentNameFromToolName_RejectsNonPrefix(t *testing.T) {
	_, ok := agentNameFromToolName("brave_web_search")
	if ok {
		t.Errorf("brave_web_search should not be detected as agent tool")
	}
}

func TestAgentNameFromToolName_RejectsEmptyAfterPrefix(t *testing.T) {
	_, ok := agentNameFromToolName("agent_")
	if ok {
		t.Errorf("'agent_' alone should not match")
	}
}

// ---- agentStatusLabel ----

func TestAgentStatusLabel_IdleEmpty(t *testing.T) {
	if got := agentStatusLabel(agent.Activity{Status: agent.StatusIdle}); got != "" {
		t.Errorf("got %q, want empty (idle)", got)
	}
}

func TestAgentStatusLabel_Thinking(t *testing.T) {
	if got := agentStatusLabel(agent.Activity{Status: agent.StatusThinking, Turn: 1, MaxTurns: 8}); got != "thinking" {
		t.Errorf("got %q", got)
	}
}

func TestAgentStatusLabel_ThinkingLaterTurn(t *testing.T) {
	if got := agentStatusLabel(agent.Activity{Status: agent.StatusThinking, Turn: 5, MaxTurns: 8}); got != "thinking (turn 5/8)" {
		t.Errorf("got %q", got)
	}
}

func TestAgentStatusLabel_UsingOneTool(t *testing.T) {
	if got := agentStatusLabel(agent.Activity{
		Status: agent.StatusUsingTools, ToolNames: []string{"jina_reader"},
	}); got != "using tool: jina_reader" {
		t.Errorf("got %q", got)
	}
}

// ---- findAgent ----

func TestFindAgent_ReturnsMatching(t *testing.T) {
	a, _ := agent.New(agent.Spec{
		Name: "researcher", Provider: stubProviderForFind{}, Tools: stubDispatcherForFind{},
	}, nil)
	m := Model{agents: []*agent.Agent{a}}
	if got := m.findAgent("researcher"); got != a {
		t.Errorf("got %v, want the registered agent", got)
	}
}

func TestFindAgent_ReturnsNilWhenAbsent(t *testing.T) {
	m := Model{agents: nil}
	if got := m.findAgent("nope"); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

// stubs for findAgent — agent.New requires non-nil Provider + Tools but
// we're not running the agent here, just testing TUI lookup.
type stubProviderForFind struct{}

func (stubProviderForFind) Name() string { return "stub" }
func (stubProviderForFind) Chat(_ context.Context, _ llm.Request) (llm.Response, error) {
	return llm.Response{}, nil
}
func (stubProviderForFind) ChatStructured(_ context.Context, _ llm.Request, _ llm.Schema, _ any) (llm.Usage, error) {
	return llm.Usage{}, nil
}

type stubDispatcherForFind struct{}

func (stubDispatcherForFind) List() []llm.Tool { return nil }
func (stubDispatcherForFind) Dispatch(context.Context, string, []byte) (string, error) {
	return "", nil
}

// ---- markdown rendering ----

func TestMarkdownWrapWidth_FloorsAt20(t *testing.T) {
	if got := markdownWrapWidth(0); got != 20 {
		t.Errorf("got %d, want 20 (floor)", got)
	}
	if got := markdownWrapWidth(15); got != 20 {
		t.Errorf("got %d, want 20 (floor)", got)
	}
}

func TestMarkdownWrapWidth_SubtractsLabelWidth(t *testing.T) {
	if got := markdownWrapWidth(80); got != 80-labelWidth {
		t.Errorf("got %d, want %d", got, 80-labelWidth)
	}
}

func TestNewMarkdownRenderer_ReturnsRenderer(t *testing.T) {
	r := mdrender.New(80)
	if r == nil {
		t.Fatal("got nil renderer for width=80")
	}
	out := r.Render("# Hello")
	if !strings.Contains(out, "Hello") {
		t.Errorf("rendered output missing 'Hello': %q", out)
	}
}

func TestRenderAssistantMarkdown_NilRendererFallsBackToPlain(t *testing.T) {
	// Pre-WindowSizeMsg the renderer is nil; we must still render
	// something useful — fall through to plain word-wrap.
	m := Model{width: 80}
	got := m.renderAssistantMarkdown("plain text", 73)
	if !strings.Contains(got, "plain text") {
		t.Errorf("got %q, want it to contain 'plain text'", got)
	}
}

func TestRenderAssistantMarkdown_RendersHeading(t *testing.T) {
	m := Model{width: 80, mdRenderer: mdrender.New(73), mdRendererWidth: 73}
	got := m.renderAssistantMarkdown("# Heading\n\nbody text", 73)
	// glamour preserves the visible text but interleaves ANSI escape
	// codes between words ("body\x1b[0m\x1b[38;5;252m text"), so we
	// assert each word independently rather than as a multi-word
	// substring. Style codes are intentionally not pinned — they
	// depend on terminal style and could shift across glamour
	// versions.
	for _, want := range []string{"Heading", "body", "text"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered output missing %q: %q", want, got)
		}
	}
}

func TestRenderAssistantMarkdown_IndentsContinuationLines(t *testing.T) {
	// Multi-line markdown should have continuation lines indented by
	// labelWidth so they sit under the role-label column. The first
	// line is left un-indented because the caller writes the role
	// label before it.
	m := Model{width: 80, mdRenderer: mdrender.New(73), mdRendererWidth: 73}
	got := m.renderAssistantMarkdown("line one\n\nline two", 73)
	lines := strings.Split(got, "\n")
	if len(lines) < 2 {
		t.Fatalf("expected multiple lines, got %d: %q", len(lines), got)
	}
	pad := strings.Repeat(" ", labelWidth)
	for i := 1; i < len(lines); i++ {
		// Allow blank lines through (glamour inserts those between
		// blocks). Non-blank continuation lines must start with the
		// pad.
		if lines[i] == "" {
			continue
		}
		if !strings.HasPrefix(lines[i], pad) {
			t.Errorf("line %d not indented: %q", i, lines[i])
		}
	}
}

func TestRenderAssistantMarkdown_TrimsLeadingTrailingBlankLines(t *testing.T) {
	// glamour pads the rendered block with blank lines on both ends.
	// We trim those so the assistant message flows under its role
	// label without a gap.
	m := Model{width: 80, mdRenderer: mdrender.New(73), mdRendererWidth: 73}
	got := m.renderAssistantMarkdown("hello", 73)
	if strings.HasPrefix(got, "\n") {
		t.Errorf("output should not start with newline: %q", got)
	}
	if strings.HasSuffix(got, "\n") {
		t.Errorf("output should not end with newline: %q", got)
	}
}

func TestRenderMessages_AssistantUsesMarkdown_NonAssistantPlain(t *testing.T) {
	// End-to-end pin: assistant messages flow through glamour (so a
	// heading shows up styled), but error / system / user messages
	// do not (so a `---` in inspect output stays as `---` rather than
	// becoming a horizontal rule).
	m := Model{
		width:           80,
		mdRenderer:      mdrender.New(73),
		mdRendererWidth: 73,
		messages: []message{
			{role: roleUser, text: "what's up?"},
			{role: roleAssistant, text: "# Hello\n\nworld"},
			{role: roleSystem, text: "section\n---\nbelow"},
		},
	}
	out := m.renderMessages()
	if !strings.Contains(out, "what's up?") {
		t.Errorf("user text missing: %q", out)
	}
	if !strings.Contains(out, "Hello") || !strings.Contains(out, "world") {
		t.Errorf("assistant text missing: %q", out)
	}
	// The literal `---` should still appear in system output —
	// glamour would have replaced it with a horizontal rule.
	if !strings.Contains(out, "---") {
		t.Errorf("system message's '---' was rewritten — markdown leaked into system output: %q", out)
	}
}
