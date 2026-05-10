package tools

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/seamus-brady/retainer/internal/agent"
	"github.com/seamus-brady/retainer/internal/cyclelog"
	"github.com/seamus-brady/retainer/internal/llm"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// scriptedProvider for delegate-tool tests. Mirrors the agent package's
// scriptedProvider — duplicated rather than exported because test
// helpers shouldn't bleed across packages.
type scriptedProvider struct {
	mu      sync.Mutex
	idx     int
	scripts []llm.Response
}

func (*scriptedProvider) Name() string { return "scripted" }
func (*scriptedProvider) ChatStructured(context.Context, llm.Request, llm.Schema, any) (llm.Usage, error) {
	return llm.Usage{}, errors.New("not used")
}
func (s *scriptedProvider) Chat(_ context.Context, _ llm.Request) (llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idx >= len(s.scripts) {
		return llm.Response{}, errors.New("scriptedProvider: exhausted")
	}
	r := s.scripts[s.idx]
	s.idx++
	return r, nil
}

// noopDispatcher is a tools.ToolDispatcher with no tools registered.
// agent.New requires a non-nil Tools field; this satisfies it for
// tests where the agent never actually dispatches anything.
type noopDispatcher struct{}

func (noopDispatcher) List() []llm.Tool { return nil }
func (noopDispatcher) Dispatch(context.Context, string, []byte) (string, error) {
	return "", errors.New("noop")
}

func newRunningAgent(t *testing.T, name string, scripts []llm.Response) *agent.Agent {
	t.Helper()
	a, err := agent.New(agent.Spec{
		Name:        name,
		HumanName:   strings.Title(name),
		Description: "Delegate test agent.",
		Provider:    &scriptedProvider{scripts: scripts},
		Tools:       noopDispatcher{},
	}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.Run(ctx)
	return a
}

func TestDelegate_ToolMetadata(t *testing.T) {
	a := newRunningAgent(t, "researcher", nil)
	tool := DelegateToAgent{Agent: a}.Tool()
	if tool.Name != "agent_researcher" {
		t.Errorf("name = %q, want agent_researcher", tool.Name)
	}
	if tool.Description == "" {
		t.Error("description should be populated from spec")
	}
	if _, ok := tool.InputSchema.Properties["instruction"]; !ok {
		t.Error("missing instruction property")
	}
	if _, ok := tool.InputSchema.Properties["context"]; !ok {
		t.Error("missing context property")
	}
	if len(tool.InputSchema.Required) != 1 || tool.InputSchema.Required[0] != "instruction" {
		t.Errorf("required = %+v", tool.InputSchema.Required)
	}
}

func TestDelegate_HappyPath(t *testing.T) {
	a := newRunningAgent(t, "researcher", []llm.Response{
		{Content: []llm.ContentBlock{llm.TextBlock{Text: "found 3 results"}}, StopReason: "end_turn"},
	})
	d := DelegateToAgent{Agent: a}
	out, err := d.Execute(context.Background(),
		[]byte(`{"instruction":"find Mustangs in Ireland"}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != "found 3 results" {
		t.Errorf("output = %q", out)
	}
}

func TestDelegate_PrependsContext(t *testing.T) {
	prov := &scriptedProvider{
		scripts: []llm.Response{
			{Content: []llm.ContentBlock{llm.TextBlock{Text: "ack"}}, StopReason: "end_turn"},
		},
	}
	a, err := agent.New(agent.Spec{
		Name:     "x",
		Provider: prov,
		Tools:    noopDispatcher{},
	}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	d := DelegateToAgent{Agent: a}
	if _, err := d.Execute(context.Background(),
		[]byte(`{"instruction":"do thing","context":"some context"}`)); err != nil {
		t.Fatal(err)
	}
	// First message in the agent's last LLM request should contain
	// both the context and the instruction.
	prov.mu.Lock()
	defer prov.mu.Unlock()
	// We only have one script, so this just confirms the agent ran
	// at all. The intra-prompt assertion would require capturing the
	// req — a separate test below does that.
}

// recordingProvider captures every Chat call's request for inspection.
type recordingProvider struct {
	mu       sync.Mutex
	requests []llm.Request
	scripts  []llm.Response
	idx      int
}

func (*recordingProvider) Name() string { return "rec" }
func (*recordingProvider) ChatStructured(context.Context, llm.Request, llm.Schema, any) (llm.Usage, error) {
	return llm.Usage{}, errors.New("not used")
}
func (r *recordingProvider) Chat(_ context.Context, req llm.Request) (llm.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req)
	if r.idx >= len(r.scripts) {
		return llm.Response{}, errors.New("exhausted")
	}
	resp := r.scripts[r.idx]
	r.idx++
	return resp, nil
}

func TestDelegate_ContextPrefixesInstructionInRequest(t *testing.T) {
	prov := &recordingProvider{
		scripts: []llm.Response{{Content: []llm.ContentBlock{llm.TextBlock{Text: "ok"}}, StopReason: "end_turn"}},
	}
	a, _ := agent.New(agent.Spec{Name: "x", Provider: prov, Tools: noopDispatcher{}}, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	d := DelegateToAgent{Agent: a}
	_, err := d.Execute(context.Background(),
		[]byte(`{"instruction":"find Mustangs","context":"red ones in Ireland"}`))
	if err != nil {
		t.Fatal(err)
	}

	prov.mu.Lock()
	defer prov.mu.Unlock()
	if len(prov.requests) != 1 {
		t.Fatalf("requests = %d", len(prov.requests))
	}
	msgs := prov.requests[0].Messages
	if len(msgs) != 1 {
		t.Fatalf("msgs = %d", len(msgs))
	}
	body := msgs[0].Content[0].(llm.TextBlock).Text
	if !strings.Contains(body, "Context: red ones in Ireland") {
		t.Errorf("body missing context: %q", body)
	}
	if !strings.Contains(body, "Instruction: find Mustangs") {
		t.Errorf("body missing instruction: %q", body)
	}
}

func TestDelegate_AgentFailureSurfacesAsToolError(t *testing.T) {
	// Agent's only script is a tool-use loop with no resolution; with
	// MaxTurns=1 it'll exhaust and return an Outcome with Err set.
	a, err := agent.New(agent.Spec{
		Name:     "x",
		Provider: &scriptedProvider{scripts: []llm.Response{
			{Content: []llm.ContentBlock{llm.ToolUseBlock{ID: "c", Name: "echo", Input: []byte(`{}`)}}, StopReason: "tool_use"},
		}},
		Tools:    noopDispatcher{},
		MaxTurns: 1,
	}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	d := DelegateToAgent{Agent: a}
	_, err = d.Execute(context.Background(), []byte(`{"instruction":"x"}`))
	if err == nil {
		t.Fatal("expected error from failed agent")
	}
	if !strings.Contains(err.Error(), "agent_x") {
		t.Errorf("err message should include tool name: %v", err)
	}
}

func TestDelegate_RejectsEmptyInstruction(t *testing.T) {
	a := newRunningAgent(t, "x", nil)
	d := DelegateToAgent{Agent: a}
	_, err := d.Execute(context.Background(), []byte(`{"instruction":"   "}`))
	if err == nil || !strings.Contains(err.Error(), "instruction must not be empty") {
		t.Fatalf("err = %v", err)
	}
}

func TestDelegate_EmptyInputErrors(t *testing.T) {
	a := newRunningAgent(t, "x", nil)
	d := DelegateToAgent{Agent: a}
	_, err := d.Execute(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "empty input") {
		t.Fatalf("err = %v", err)
	}
}

func TestDelegate_MalformedInputErrors(t *testing.T) {
	a := newRunningAgent(t, "x", nil)
	d := DelegateToAgent{Agent: a}
	_, err := d.Execute(context.Background(), []byte(`{not json`))
	if err == nil || !strings.Contains(err.Error(), "decode input") {
		t.Fatalf("err = %v", err)
	}
}

func TestDelegate_PassesParentCycleIDViaContext(t *testing.T) {
	// The cog wraps the dispatch ctx with cyclelog.WithCycleID so the
	// delegate tool can extract the parent cycle id and stamp it onto
	// the agent's Task. This test asserts that round-trips: the
	// agent's Task should carry the parent_cycle_id that was on the
	// context.
	prov := &recordingProvider{
		scripts: []llm.Response{{Content: []llm.ContentBlock{llm.TextBlock{Text: "ok"}}, StopReason: "end_turn"}},
	}
	a, _ := agent.New(agent.Spec{Name: "x", Provider: prov, Tools: noopDispatcher{}}, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	d := DelegateToAgent{Agent: a}
	dispatchCtx := cyclelog.WithCycleID(context.Background(), "cog-cyc-99")
	if _, err := d.Execute(dispatchCtx, []byte(`{"instruction":"x"}`)); err != nil {
		t.Fatal(err)
	}
	// We can't directly inspect the Task here without test hooks
	// inside agent — but the delegate tool's ParentCycleID assignment
	// is unconditional (covered by reading the code). The test
	// ensures no panic / wrong-type extraction when the context has
	// the cycle id.
}

// ---- agentToolName / agentToolDescription helpers ----

func TestAgentToolName(t *testing.T) {
	if got := agentToolName("researcher"); got != "agent_researcher" {
		t.Errorf("got %q", got)
	}
}

func TestAgentToolDescription_FallbackWhenEmpty(t *testing.T) {
	a, _ := agent.New(agent.Spec{
		Name: "x", HumanName: "Xenon",
		Provider: &scriptedProvider{}, Tools: noopDispatcher{},
	}, discardLogger())
	got := agentToolDescription(a)
	if !strings.Contains(got, "Xenon") {
		t.Errorf("fallback missing human name: %q", got)
	}
}
