package archivist

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/seamus-brady/retainer/internal/agent"
	"github.com/seamus-brady/retainer/internal/cbr"
	"github.com/seamus-brady/retainer/internal/cyclelog"
	"github.com/seamus-brady/retainer/internal/llm"
)

// ---- formatAgentCompletions ----

func TestFormatAgentCompletions_Empty(t *testing.T) {
	if got := formatAgentCompletions(nil); got != "AGENT COMPLETIONS: (none)" {
		t.Errorf("got %q", got)
	}
}

func TestFormatAgentCompletions_RendersInternalToolList(t *testing.T) {
	// Load-bearing: the curator's grounding depends on seeing
	// what tools the agent itself fired, not just "agent_X
	// returned ok". This test pins that surface.
	got := formatAgentCompletions([]agent.CompletionRecord{
		{
			AgentName:    "researcher",
			Instruction:  "find recent papers on auth",
			Success:      true,
			ToolsUsed:    []string{"brave_web_search", "jina_reader"},
			InputTokens:  1200,
			OutputTokens: 300,
		},
	})
	for _, want := range []string{
		"AGENT COMPLETIONS:",
		"researcher (ok)",
		"brief: find recent papers on auth",
		"internal tools: brave_web_search, jina_reader",
		"tokens: 1200 in / 300 out",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatAgentCompletions_FailureShowsErrorMessage(t *testing.T) {
	got := formatAgentCompletions([]agent.CompletionRecord{
		{
			AgentName:    "researcher",
			Instruction:  "find X",
			Success:      false,
			ErrorMessage: "max_turns exhausted without final reply",
			ToolsUsed:    []string{"brave_web_search"},
		},
	})
	for _, want := range []string{
		"researcher (FAILED)",
		"error: max_turns exhausted",
		"internal tools: brave_web_search",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatAgentCompletions_NoInternalToolsAnnotated(t *testing.T) {
	// An agent that returned without firing any tools is
	// noteworthy — the curator should see "(none)" rather than
	// silently omitting the field. Helps catch fabrication
	// patterns ("I researched..." with no research tools fired).
	got := formatAgentCompletions([]agent.CompletionRecord{
		{
			AgentName:   "researcher",
			Instruction: "find X",
			Success:     true,
			ToolsUsed:   nil,
		},
	})
	if !strings.Contains(got, "internal tools: (none — agent made no tool calls)") {
		t.Errorf("expected 'no tool calls' annotation; got:\n%s", got)
	}
}

func TestFormatAgentCompletions_MultipleAgentsPreserveOrder(t *testing.T) {
	got := formatAgentCompletions([]agent.CompletionRecord{
		{AgentName: "researcher", Success: true, ToolsUsed: []string{"a"}},
		{AgentName: "observer", Success: true, ToolsUsed: []string{"b"}},
	})
	rIdx := strings.Index(got, "researcher (ok)")
	oIdx := strings.Index(got, "observer (ok)")
	if rIdx < 0 || oIdx < 0 || rIdx >= oIdx {
		t.Errorf("order not preserved; got:\n%s", got)
	}
}

// ---- buildReflectionUserPrompt + buildCurationUserPrompt ----

func TestBuildReflectionPrompt_IncludesAgentCompletions(t *testing.T) {
	in := CurationInput{
		UserInput: "find recent auth papers",
		ReplyText: "Found three sources.",
		AgentCompletions: []agent.CompletionRecord{
			{
				AgentName: "researcher",
				Success:   true,
				ToolsUsed: []string{"brave_web_search"},
			},
		},
	}
	got := buildReflectionUserPrompt(in)
	if !strings.Contains(got, "AGENT COMPLETIONS:") {
		t.Errorf("reflection prompt missing AGENT COMPLETIONS section:\n%s", got)
	}
	if !strings.Contains(got, "researcher (ok)") {
		t.Errorf("reflection prompt missing agent name + status:\n%s", got)
	}
	if !strings.Contains(got, "internal tools: brave_web_search") {
		t.Errorf("reflection prompt missing internal tool list:\n%s", got)
	}
}

func TestBuildReflectionPrompt_FallsBackToAgentsUsedListWhenNoCompletions(t *testing.T) {
	// Backstop path — when AgentCompletions isn't populated
	// (legacy / test fixtures) but AgentsUsed is, the prompt
	// still surfaces SOMETHING about agent dispatches.
	in := CurationInput{
		UserInput:  "x",
		ReplyText:  "y",
		AgentsUsed: []string{"researcher"},
	}
	got := buildReflectionUserPrompt(in)
	if !strings.Contains(got, "AGENTS DISPATCHED: researcher") {
		t.Errorf("legacy fallback line missing:\n%s", got)
	}
	if strings.Contains(got, "AGENT COMPLETIONS:") {
		t.Errorf("rich format should not appear when AgentCompletions empty:\n%s", got)
	}
}

func TestBuildCurationPrompt_IncludesAgentCompletions(t *testing.T) {
	in := CurationInput{
		UserInput: "find recent auth papers",
		ReplyText: "Found three sources.",
		AgentCompletions: []agent.CompletionRecord{
			{
				AgentName:    "researcher",
				Success:      false,
				ErrorMessage: "max_turns",
				ToolsUsed:    []string{"brave_web_search"},
			},
		},
	}
	got := buildCurationUserPrompt(in, "reflection prose")
	for _, want := range []string{
		"REFLECTION (Phase 1",
		"reflection prose",
		"AGENT COMPLETIONS:",
		"researcher (FAILED)",
		"error: max_turns",
		"Call the record_curation tool",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// ---- HeuristicCurator preserves agent completions handling ----

func TestHeuristicCurator_AgentCompletionsDontConfuseClassification(t *testing.T) {
	// Heuristic doesn't read AgentCompletions for classification
	// (LLMCurator does). But CurationInput should be safe to
	// pass either way without crashing.
	in := CurationInput{
		UserInput: "find auth papers",
		ReplyText: "Found three.",
		AgentCompletions: []agent.CompletionRecord{
			{AgentName: "researcher", Success: true, ToolsUsed: []string{"brave"}, Duration: time.Second},
		},
		CycleStatus: "complete",
	}
	got, err := HeuristicCurator{}.Curate(nil, in)
	if err != nil {
		t.Fatal(err)
	}
	// "find auth papers" — no ack prefix, so Exploration.
	if got.IntentClassification != cbr.IntentExploration {
		t.Errorf("classification = %q, want exploration", got.IntentClassification)
	}
	if got.Status != cbr.StatusSuccess {
		t.Errorf("status = %q, want success", got.Status)
	}
}

// ---- LLMCurator telemetry (Phase 6) ----

// fakeProvider records every Chat / ChatStructured call so tests
// can assert on what the curator dispatched + return whatever
// scripted response (or error) the test needs.
type fakeProvider struct {
	chatResp        llm.Response
	chatErr         error
	structuredUsage llm.Usage
	structuredErr   error
	structuredFn    func(dst any)
	chatCalls       int
	structuredCalls int
}

func (fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Chat(_ context.Context, _ llm.Request) (llm.Response, error) {
	f.chatCalls++
	return f.chatResp, f.chatErr
}

func (f *fakeProvider) ChatStructured(_ context.Context, _ llm.Request, _ llm.Schema, dst any) (llm.Usage, error) {
	f.structuredCalls++
	if f.structuredFn != nil {
		f.structuredFn(dst)
	}
	return f.structuredUsage, f.structuredErr
}

// recordingSink captures emitted events for assertion.
type recordingSink struct {
	events []cyclelog.Event
}

func (r *recordingSink) Emit(ev cyclelog.Event) error {
	r.events = append(r.events, ev)
	return nil
}

// validCurationPayload populates the dst with a minimal but valid
// structured response so curate() doesn't fail on missing fields.
func validCurationPayload(dst any) {
	if p, ok := dst.(*curationPayload); ok {
		*p = curationPayload{
			IntentClassification: "exploration",
			IntentDescription:    "test exploration",
			Domain:               "test",
			Status:               "success",
			Confidence:           0.8,
			Assessment:           "looked good",
		}
	}
}

func TestLLMCurator_EmitsBothEvents(t *testing.T) {
	sink := &recordingSink{}
	prov := &fakeProvider{
		chatResp: llm.Response{
			Content:    []llm.ContentBlock{llm.TextBlock{Text: "reflection text"}},
			StopReason: "end_turn",
			Usage:      llm.Usage{InputTokens: 100, OutputTokens: 50},
		},
		structuredUsage: llm.Usage{InputTokens: 200, OutputTokens: 75},
		structuredFn:    validCurationPayload,
	}
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	idCounter := 0
	c := &LLMCurator{
		Provider: prov,
		Model:    "claude-test",
		CycleLog: sink,
		IDFn: func() string {
			idCounter++
			return "ev-" + string(rune('a'+idCounter-1))
		},
		NowFn: func() time.Time { return now },
	}

	_, err := c.Curate(context.Background(), CurationInput{
		UserInput:     "what's the weather?",
		ReplyText:     "looking it up",
		ParentCycleID: "cog-cyc-1",
	})
	if err != nil {
		t.Fatalf("Curate: %v", err)
	}

	if len(sink.events) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(sink.events), sink.events)
	}

	refEv := sink.events[0]
	if refEv.Type != cyclelog.EventCuratorReflection {
		t.Errorf("event[0] type = %q, want curator_reflection", refEv.Type)
	}
	if refEv.ParentID != "cog-cyc-1" {
		t.Errorf("ParentID = %q, want cog-cyc-1", refEv.ParentID)
	}
	if refEv.Model != "claude-test" {
		t.Errorf("Model = %q", refEv.Model)
	}
	if refEv.InputTokens != 100 || refEv.OutputTokens != 50 {
		t.Errorf("tokens = %d/%d, want 100/50", refEv.InputTokens, refEv.OutputTokens)
	}
	if refEv.StopReason != "end_turn" {
		t.Errorf("StopReason = %q", refEv.StopReason)
	}
	if !refEv.Success {
		t.Errorf("Success should be true")
	}

	curEv := sink.events[1]
	if curEv.Type != cyclelog.EventCuratorCuration {
		t.Errorf("event[1] type = %q, want curator_curation", curEv.Type)
	}
	if curEv.ParentID != "cog-cyc-1" {
		t.Errorf("ParentID = %q", curEv.ParentID)
	}
	if curEv.InputTokens != 200 || curEv.OutputTokens != 75 {
		t.Errorf("tokens = %d/%d, want 200/75", curEv.InputTokens, curEv.OutputTokens)
	}
	if curEv.StopReason != "" {
		t.Errorf("StopReason = %q, want empty (ChatStructured doesn't surface it)", curEv.StopReason)
	}

	if refEv.CycleID == curEv.CycleID {
		t.Errorf("event ids should differ: %q == %q", refEv.CycleID, curEv.CycleID)
	}
}

func TestLLMCurator_EmissionRespectsNilSink(t *testing.T) {
	// No CycleLog: curator runs to completion without panicking
	// and no emission happens.
	prov := &fakeProvider{
		chatResp:        llm.Response{Content: []llm.ContentBlock{llm.TextBlock{Text: "ok"}}},
		structuredUsage: llm.Usage{InputTokens: 10},
		structuredFn:    validCurationPayload,
	}
	c := &LLMCurator{Provider: prov, Model: "m", CycleLog: nil}
	if _, err := c.Curate(context.Background(), CurationInput{UserInput: "hi", ReplyText: "hi"}); err != nil {
		t.Fatalf("Curate: %v", err)
	}
}

func TestLLMCurator_EmitsEvenOnReflectionFailure(t *testing.T) {
	sink := &recordingSink{}
	prov := &fakeProvider{
		chatErr:         context.Canceled, // any error
		structuredUsage: llm.Usage{InputTokens: 10, OutputTokens: 5},
		structuredFn:    validCurationPayload,
	}
	c := &LLMCurator{Provider: prov, Model: "m", CycleLog: sink}
	_, _ = c.Curate(context.Background(), CurationInput{UserInput: "hi", ReplyText: "hi"})

	if len(sink.events) != 2 {
		t.Fatalf("got %d events, want 2 even when reflection failed", len(sink.events))
	}
	if sink.events[0].Success {
		t.Errorf("reflection event must show Success=false on error")
	}
	if sink.events[0].Error == "" {
		t.Errorf("reflection event must carry Error message on failure")
	}
	if !sink.events[1].Success {
		t.Errorf("curation event Success expected (curation succeeded after reflection error)")
	}
}

func TestLLMCurator_RecordsDurationFromNowFn(t *testing.T) {
	sink := &recordingSink{}
	prov := &fakeProvider{
		chatResp:        llm.Response{Content: []llm.ContentBlock{llm.TextBlock{Text: "ok"}}},
		structuredUsage: llm.Usage{},
		structuredFn:    validCurationPayload,
	}
	// NowFn advances by 100ms each call. Reflection: start →
	// after-call = 100ms. Curation: same.
	t0 := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	step := 100 * time.Millisecond
	calls := 0
	c := &LLMCurator{
		Provider: prov,
		Model:    "m",
		CycleLog: sink,
		NowFn: func() time.Time {
			t := t0.Add(time.Duration(calls) * step)
			calls++
			return t
		},
	}
	_, _ = c.Curate(context.Background(), CurationInput{UserInput: "hi", ReplyText: "hi"})

	if len(sink.events) != 2 {
		t.Fatalf("got %d events, want 2", len(sink.events))
	}
	for i, ev := range sink.events {
		if ev.DurationMs != step.Milliseconds() {
			t.Errorf("event[%d] DurationMs = %d, want %d", i, ev.DurationMs, step.Milliseconds())
		}
	}
}

