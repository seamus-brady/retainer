package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seamus-brady/retainer/internal/cyclelog"
	"github.com/seamus-brady/retainer/internal/dag"
	"github.com/seamus-brady/retainer/internal/llm"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// scriptedProvider returns canned responses in order, mirroring the
// cog test's stub. Each call pops the next response.
type scriptedProvider struct {
	mu      sync.Mutex
	idx     int
	scripts []llm.Response
	calls   int
	lastReq llm.Request
}

func (*scriptedProvider) Name() string { return "scripted" }

func (*scriptedProvider) ChatStructured(context.Context, llm.Request, llm.Schema, any) (llm.Usage, error) {
	return llm.Usage{}, errors.New("scriptedProvider: ChatStructured not used")
}

func (s *scriptedProvider) Chat(_ context.Context, req llm.Request) (llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.lastReq = req
	if s.idx >= len(s.scripts) {
		return llm.Response{}, errors.New("scriptedProvider: exhausted")
	}
	r := s.scripts[s.idx]
	s.idx++
	return r, nil
}

// erroringProvider always returns the same error. Used to test
// MaxConsecutiveErrors.
type erroringProvider struct {
	calls atomic.Int32
	err   error
}

func (*erroringProvider) Name() string { return "erroring" }
func (*erroringProvider) ChatStructured(context.Context, llm.Request, llm.Schema, any) (llm.Usage, error) {
	return llm.Usage{}, errors.New("not used")
}
func (e *erroringProvider) Chat(context.Context, llm.Request) (llm.Response, error) {
	e.calls.Add(1)
	return llm.Response{}, e.err
}

// fakeDispatcher records dispatch calls.
type fakeDispatcher struct {
	tools []llm.Tool
	mu    sync.Mutex
	calls []dispatchCall
	res   string
	err   error
}

type dispatchCall struct {
	name    string
	input   []byte
	cycleID string
}

func (d *fakeDispatcher) List() []llm.Tool { return d.tools }

func (d *fakeDispatcher) Dispatch(_ context.Context, name string, input []byte) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, dispatchCall{name: name, input: append([]byte(nil), input...)})
	return d.res, d.err
}

func runAgent(t *testing.T, a *Agent) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.Run(ctx)
}

// ---- New ----

func TestNew_RequiresName(t *testing.T) {
	_, err := New(Spec{Provider: &scriptedProvider{}, Tools: &fakeDispatcher{}}, discardLogger())
	if err == nil || !strings.Contains(err.Error(), "Name") {
		t.Fatalf("err = %v", err)
	}
}

func TestNew_RequiresProvider(t *testing.T) {
	_, err := New(Spec{Name: "x", Tools: &fakeDispatcher{}}, discardLogger())
	if err == nil || !strings.Contains(err.Error(), "Provider") {
		t.Fatalf("err = %v", err)
	}
}

func TestNew_RequiresTools(t *testing.T) {
	_, err := New(Spec{Name: "x", Provider: &scriptedProvider{}}, discardLogger())
	if err == nil || !strings.Contains(err.Error(), "Tools") {
		t.Fatalf("err = %v", err)
	}
}

func TestNew_AppliesDefaults(t *testing.T) {
	a, err := New(Spec{Name: "x", Provider: &scriptedProvider{}, Tools: &fakeDispatcher{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.spec.MaxTokens != defaultMaxTokens {
		t.Errorf("MaxTokens = %d", a.spec.MaxTokens)
	}
	if a.spec.MaxTurns != defaultMaxTurns {
		t.Errorf("MaxTurns = %d", a.spec.MaxTurns)
	}
	if a.spec.MaxConsecutiveErrors != defaultMaxConsecutiveErrors {
		t.Errorf("MaxConsecutiveErrors = %d", a.spec.MaxConsecutiveErrors)
	}
}

// ---- Submit / Outcome happy path ----

func TestAgent_SimpleReply(t *testing.T) {
	prov := &scriptedProvider{
		scripts: []llm.Response{
			{Content: []llm.ContentBlock{llm.TextBlock{Text: "hello back"}}, StopReason: "end_turn"},
		},
	}
	disp := &fakeDispatcher{tools: []llm.Tool{{Name: "echo"}}}
	a, _ := New(Spec{Name: "x", HumanName: "X", Provider: prov, Tools: disp}, discardLogger())
	runAgent(t, a)

	reply := make(chan Outcome, 1)
	if err := a.Submit(context.Background(), Task{Instruction: "hi", Reply: reply}); err != nil {
		t.Fatal(err)
	}

	out := <-reply
	if !out.IsSuccess() {
		t.Fatalf("err = %v", out.Err)
	}
	if out.Result != "hello back" {
		t.Errorf("result = %q", out.Result)
	}
	if out.AgentName != "x" {
		t.Errorf("agent_name = %q", out.AgentName)
	}
	if out.AgentCycleID == "" {
		t.Errorf("agent_cycle_id should be populated")
	}
	if out.Duration == 0 {
		t.Errorf("duration should be > 0")
	}
}

// ---- React loop with tools ----

func TestAgent_ReactLoopRunsTool(t *testing.T) {
	prov := &scriptedProvider{
		scripts: []llm.Response{
			{
				Content: []llm.ContentBlock{
					llm.ToolUseBlock{ID: "c1", Name: "echo", Input: []byte(`{"q":"hi"}`)},
				},
				StopReason: "tool_use",
			},
			{
				Content:    []llm.ContentBlock{llm.TextBlock{Text: "tool said hi"}},
				StopReason: "end_turn",
			},
		},
	}
	disp := &fakeDispatcher{tools: []llm.Tool{{Name: "echo"}}, res: "echoed"}
	a, _ := New(Spec{Name: "x", Provider: prov, Tools: disp}, discardLogger())
	runAgent(t, a)

	reply := make(chan Outcome, 1)
	_ = a.Submit(context.Background(), Task{Instruction: "use echo", Reply: reply})
	out := <-reply

	if !out.IsSuccess() {
		t.Fatalf("err: %v", out.Err)
	}
	if out.Result != "tool said hi" {
		t.Errorf("result = %q", out.Result)
	}
	if len(out.ToolsUsed) != 1 || out.ToolsUsed[0] != "echo" {
		t.Errorf("tools_used = %+v", out.ToolsUsed)
	}
	disp.mu.Lock()
	defer disp.mu.Unlock()
	if len(disp.calls) != 1 || disp.calls[0].name != "echo" {
		t.Fatalf("dispatcher calls = %+v", disp.calls)
	}
}

func TestAgent_ToolErrorContinues(t *testing.T) {
	// A failing tool produces an IsError tool_result; the agent's LLM
	// gets to recover, just like the cog. The agent doesn't abort.
	prov := &scriptedProvider{
		scripts: []llm.Response{
			{
				Content: []llm.ContentBlock{
					llm.ToolUseBlock{ID: "c1", Name: "echo", Input: []byte(`{}`)},
				},
				StopReason: "tool_use",
			},
			{
				Content:    []llm.ContentBlock{llm.TextBlock{Text: "tool failed but recovered"}},
				StopReason: "end_turn",
			},
		},
	}
	disp := &fakeDispatcher{tools: []llm.Tool{{Name: "echo"}}, err: errors.New("brave 500")}
	a, _ := New(Spec{Name: "x", Provider: prov, Tools: disp}, discardLogger())
	runAgent(t, a)

	reply := make(chan Outcome, 1)
	_ = a.Submit(context.Background(), Task{Instruction: "use echo", Reply: reply})
	out := <-reply

	if !out.IsSuccess() {
		t.Fatalf("err: %v", out.Err)
	}
	if out.Result != "tool failed but recovered" {
		t.Errorf("result = %q", out.Result)
	}
}

// ---- Bound enforcement ----

func TestAgent_MaxTurnsExhausted(t *testing.T) {
	loop := llm.Response{
		Content:    []llm.ContentBlock{llm.ToolUseBlock{ID: "c", Name: "echo", Input: []byte(`{}`)}},
		StopReason: "tool_use",
	}
	prov := &scriptedProvider{scripts: []llm.Response{loop, loop, loop, loop, loop, loop}}
	disp := &fakeDispatcher{tools: []llm.Tool{{Name: "echo"}}, res: "ok"}
	a, _ := New(Spec{Name: "x", Provider: prov, Tools: disp, MaxTurns: 2}, discardLogger())
	runAgent(t, a)

	reply := make(chan Outcome, 1)
	_ = a.Submit(context.Background(), Task{Instruction: "loop", Reply: reply})
	out := <-reply

	if out.IsSuccess() {
		t.Fatalf("expected failure on max_turns; got result %q", out.Result)
	}
	if !strings.Contains(out.Err.Error(), "max_turns") {
		t.Errorf("err = %v", out.Err)
	}
}

func TestAgent_MaxConsecutiveErrors(t *testing.T) {
	prov := &erroringProvider{err: errors.New("rate limit")}
	disp := &fakeDispatcher{tools: []llm.Tool{{Name: "echo"}}}
	a, _ := New(Spec{
		Name: "x", Provider: prov, Tools: disp,
		MaxConsecutiveErrors: 2, MaxTurns: 5,
	}, discardLogger())
	runAgent(t, a)

	reply := make(chan Outcome, 1)
	_ = a.Submit(context.Background(), Task{Instruction: "ping", Reply: reply})
	out := <-reply

	if out.IsSuccess() {
		t.Fatalf("expected failure on consecutive errors; got %q", out.Result)
	}
	if !strings.Contains(out.Err.Error(), "consecutive LLM errors") {
		t.Errorf("err = %v", out.Err)
	}
	if got := prov.calls.Load(); got != 2 {
		t.Errorf("provider calls = %d, want 2 (MaxConsecutiveErrors)", got)
	}
}

func TestAgent_ErrorThenRecoveryResetsCounter(t *testing.T) {
	// Error → success → error → success: should pass all four turns
	// without tripping MaxConsecutiveErrors=2.
	prov := &alternatingProvider{}
	disp := &fakeDispatcher{tools: []llm.Tool{{Name: "echo"}}, res: "ok"}
	a, _ := New(Spec{
		Name: "x", Provider: prov, Tools: disp,
		MaxConsecutiveErrors: 2, MaxTurns: 6,
	}, discardLogger())
	runAgent(t, a)

	reply := make(chan Outcome, 1)
	_ = a.Submit(context.Background(), Task{Instruction: "ping", Reply: reply})
	out := <-reply
	if !out.IsSuccess() {
		t.Errorf("expected success after recovery, got err: %v", out.Err)
	}
}

// alternatingProvider returns error, then success, then error, etc. Tests
// that a successful call resets the consecutive-error counter.
type alternatingProvider struct {
	mu    sync.Mutex
	count int
}

func (*alternatingProvider) Name() string { return "alt" }
func (*alternatingProvider) ChatStructured(context.Context, llm.Request, llm.Schema, any) (llm.Usage, error) {
	return llm.Usage{}, errors.New("not used")
}
func (a *alternatingProvider) Chat(context.Context, llm.Request) (llm.Response, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.count++
	if a.count%2 == 1 {
		return llm.Response{}, errors.New("blip")
	}
	return llm.Response{Content: []llm.ContentBlock{llm.TextBlock{Text: "ok"}}}, nil
}

// ---- Submit guards ----

func TestSubmit_RequiresReplyChannel(t *testing.T) {
	prov := &scriptedProvider{}
	disp := &fakeDispatcher{tools: []llm.Tool{{Name: "echo"}}}
	a, _ := New(Spec{Name: "x", Provider: prov, Tools: disp}, discardLogger())
	err := a.Submit(context.Background(), Task{Instruction: "x"})
	if err == nil || !strings.Contains(err.Error(), "Reply") {
		t.Fatalf("err = %v", err)
	}
}

func TestSubmit_GeneratesTaskIDIfEmpty(t *testing.T) {
	prov := &scriptedProvider{
		scripts: []llm.Response{{Content: []llm.ContentBlock{llm.TextBlock{Text: "ok"}}, StopReason: "end_turn"}},
	}
	disp := &fakeDispatcher{tools: []llm.Tool{{Name: "echo"}}}
	a, _ := New(Spec{Name: "x", Provider: prov, Tools: disp}, discardLogger())
	runAgent(t, a)

	reply := make(chan Outcome, 1)
	_ = a.Submit(context.Background(), Task{Instruction: "x", Reply: reply})
	out := <-reply
	if out.TaskID == "" {
		t.Errorf("task_id should be auto-generated")
	}
}

func TestSubmit_RespectsCancelledCtx(t *testing.T) {
	prov := &scriptedProvider{}
	disp := &fakeDispatcher{tools: []llm.Tool{{Name: "echo"}}}
	a, _ := New(Spec{Name: "x", Provider: prov, Tools: disp}, discardLogger())
	// Don't run the agent; fill the inbox so Submit blocks.
	for i := 0; i < cap(a.inbox); i++ {
		a.inbox <- Task{TaskID: "filler", Reply: make(chan Outcome, 1)}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := a.Submit(ctx, Task{Instruction: "x", Reply: make(chan Outcome, 1)})
	if err == nil {
		t.Fatal("expected ctx-cancelled error")
	}
}

// ---- Activities ----

func drainActivities(a *Agent, deadline time.Duration) []Activity {
	var out []Activity
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	for {
		select {
		case act := <-a.Activities():
			out = append(out, act)
		case <-timer.C:
			return out
		}
	}
}

func TestAgent_ActivityEmittedOnTask(t *testing.T) {
	prov := &scriptedProvider{
		scripts: []llm.Response{{Content: []llm.ContentBlock{llm.TextBlock{Text: "ok"}}, StopReason: "end_turn"}},
	}
	disp := &fakeDispatcher{tools: []llm.Tool{{Name: "echo"}}}
	a, _ := New(Spec{Name: "researcher", Provider: prov, Tools: disp}, discardLogger())
	runAgent(t, a)

	reply := make(chan Outcome, 1)
	_ = a.Submit(context.Background(), Task{Instruction: "x", Reply: reply})
	<-reply
	acts := drainActivities(a, 100*time.Millisecond)

	sawThinking := false
	sawIdle := false
	for _, act := range acts {
		if act.AgentName != "researcher" {
			t.Errorf("activity has wrong agent name: %+v", act)
		}
		if act.Status == StatusThinking {
			sawThinking = true
		}
		if act.Status == StatusIdle {
			sawIdle = true
		}
	}
	if !sawThinking || !sawIdle {
		t.Errorf("missing transitions; got %+v", acts)
	}
}

func TestAgent_ActivityCarriesToolNames(t *testing.T) {
	prov := &scriptedProvider{
		scripts: []llm.Response{
			{
				Content: []llm.ContentBlock{
					llm.ToolUseBlock{ID: "a", Name: "brave_web_search", Input: []byte(`{}`)},
					llm.ToolUseBlock{ID: "b", Name: "jina_reader", Input: []byte(`{}`)},
				},
				StopReason: "tool_use",
			},
			{Content: []llm.ContentBlock{llm.TextBlock{Text: "done"}}, StopReason: "end_turn"},
		},
	}
	disp := &fakeDispatcher{tools: []llm.Tool{{Name: "brave_web_search"}, {Name: "jina_reader"}}, res: "ok"}
	a, _ := New(Spec{Name: "x", Provider: prov, Tools: disp}, discardLogger())
	runAgent(t, a)

	reply := make(chan Outcome, 1)
	_ = a.Submit(context.Background(), Task{Instruction: "x", Reply: reply})
	<-reply
	acts := drainActivities(a, 100*time.Millisecond)

	var hit *Activity
	for i := range acts {
		if acts[i].Status == StatusUsingTools && len(acts[i].ToolNames) > 0 {
			hit = &acts[i]
			break
		}
	}
	if hit == nil {
		t.Fatalf("no UsingTools activity with names; got %+v", acts)
	}
	if len(hit.ToolNames) != 2 || hit.ToolNames[0] != "brave_web_search" || hit.ToolNames[1] != "jina_reader" {
		t.Errorf("tool names = %+v", hit.ToolNames)
	}
}

func TestAgent_NoSubscriberDoesNotDeadlock(t *testing.T) {
	prov := &scriptedProvider{
		scripts: []llm.Response{{Content: []llm.ContentBlock{llm.TextBlock{Text: "ok"}}, StopReason: "end_turn"}},
	}
	disp := &fakeDispatcher{tools: []llm.Tool{{Name: "echo"}}}
	a, _ := New(Spec{Name: "x", Provider: prov, Tools: disp}, discardLogger())
	runAgent(t, a)

	// Don't read activities. Run several tasks (re-using the provider
	// would exhaust scripts; just check one task's worth doesn't hang).
	reply := make(chan Outcome, 1)
	_ = a.Submit(context.Background(), Task{Instruction: "x", Reply: reply})
	select {
	case out := <-reply:
		if !out.IsSuccess() {
			t.Errorf("err: %v", out.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent hung when no activity subscriber")
	}
}

// ---- Status string ----

func TestStatus_String(t *testing.T) {
	for _, c := range []struct {
		s    Status
		want string
	}{
		{StatusIdle, "idle"},
		{StatusThinking, "thinking"},
		{StatusUsingTools, "using_tools"},
		{Status(99), "unknown"},
	} {
		if got := c.s.String(); got != c.want {
			t.Errorf("Status(%d).String() = %q, want %q", int(c.s), got, c.want)
		}
	}
}

// ---- Accessors ----

func TestAgent_Accessors(t *testing.T) {
	a, _ := New(Spec{
		Name: "researcher", HumanName: "Researcher",
		Description: "Search the web.",
		Provider:    &scriptedProvider{}, Tools: &fakeDispatcher{},
	}, discardLogger())
	if a.Name() != "researcher" {
		t.Errorf("Name = %q", a.Name())
	}
	if a.HumanName() != "Researcher" {
		t.Errorf("HumanName = %q", a.HumanName())
	}
	if a.Description() != "Search the web." {
		t.Errorf("Description = %q", a.Description())
	}
}

// ---- Telemetry: cycle-log + DAG emission ----

// captureSink collects every Event the agent emits — used to assert
// on event ordering, parent_id chains, instance_id stamping.
type captureSink struct {
	events []cyclelog.Event
}

func (c *captureSink) Emit(e cyclelog.Event) error {
	c.events = append(c.events, e)
	return nil
}

// captureDAG records StartCycle / CompleteCycle calls.
type captureDAG struct {
	starts    []dagStart
	completes []dagComplete
}

type dagStart struct {
	ID       dag.CycleID
	ParentID dag.CycleID
	NodeType dag.NodeType
}

type dagComplete struct {
	ID       dag.CycleID
	Status   dag.Status
	ErrorMsg string
}

func (c *captureDAG) StartCycle(id, parentID dag.CycleID, nt dag.NodeType) {
	c.starts = append(c.starts, dagStart{ID: id, ParentID: parentID, NodeType: nt})
}
func (c *captureDAG) CompleteCycle(id dag.CycleID, status dag.Status, msg string) {
	c.completes = append(c.completes, dagComplete{ID: id, Status: status, ErrorMsg: msg})
}

func TestAgent_EmitsCycleLogEventsForOneTurn(t *testing.T) {
	sink := &captureSink{}
	dagRec := &captureDAG{}
	prov := &scriptedProvider{
		scripts: []llm.Response{
			{Content: []llm.ContentBlock{llm.TextBlock{Text: "done"}}, StopReason: "end_turn"},
		},
	}
	a, err := New(Spec{
		Name:       "test-agent",
		Provider:   prov,
		Tools:      &fakeDispatcher{},
		CycleLog:   sink,
		DAG:        dagRec,
		InstanceID: "abc12345",
	}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	reply := make(chan Outcome, 1)
	if err := a.Submit(ctx, Task{
		TaskID:        "task-1",
		ParentCycleID: "cog-cycle-1",
		Instruction:   "do thing",
		Reply:         reply,
	}); err != nil {
		t.Fatal(err)
	}
	out := <-reply
	if out.Err != nil {
		t.Fatalf("err: %v", out.Err)
	}

	// Expected events (in order): agent_cycle_start, llm_request,
	// llm_response, agent_cycle_complete.
	wantTypes := []cyclelog.EventType{
		cyclelog.EventAgentCycleStart,
		cyclelog.EventLLMRequest,
		cyclelog.EventLLMResponse,
		cyclelog.EventAgentCycleComplete,
	}
	if len(sink.events) != len(wantTypes) {
		t.Fatalf("got %d events, want %d:\n%+v", len(sink.events), len(wantTypes), sink.events)
	}
	for i, want := range wantTypes {
		if sink.events[i].Type != want {
			t.Errorf("event[%d] = %q, want %q", i, sink.events[i].Type, want)
		}
		if sink.events[i].InstanceID != "abc12345" {
			t.Errorf("event[%d] missing instance_id", i)
		}
	}

	// agent_cycle_start.parent_id must be the cog cycle.
	if sink.events[0].ParentID != "cog-cycle-1" {
		t.Errorf("agent_cycle_start.parent_id = %q, want cog-cycle-1", sink.events[0].ParentID)
	}
	// llm_request / llm_response.parent_id should be the agent's task ID.
	if sink.events[1].ParentID != "task-1" || sink.events[2].ParentID != "task-1" {
		t.Errorf("llm event parent_id wrong: %+v", sink.events)
	}

	// DAG: one StartCycle (NodeAgent, parent=cog-cycle-1) + one CompleteCycle.
	if len(dagRec.starts) != 1 {
		t.Fatalf("got %d StartCycle calls, want 1", len(dagRec.starts))
	}
	if dagRec.starts[0].NodeType != dag.NodeAgent {
		t.Errorf("nodeType = %q, want NodeAgent", dagRec.starts[0].NodeType)
	}
	if dagRec.starts[0].ParentID != "cog-cycle-1" {
		t.Errorf("DAG parent = %q, want cog-cycle-1", dagRec.starts[0].ParentID)
	}
	if len(dagRec.completes) != 1 {
		t.Fatalf("got %d CompleteCycle calls, want 1", len(dagRec.completes))
	}
	if dagRec.completes[0].Status != dag.StatusComplete {
		t.Errorf("complete status = %q", dagRec.completes[0].Status)
	}
}

func TestAgent_EmitsToolCallAndToolResult(t *testing.T) {
	sink := &captureSink{}
	disp := &fakeDispatcher{
		tools: []llm.Tool{{Name: "echo"}},
		res:   "ok",
	}
	prov := &scriptedProvider{
		scripts: []llm.Response{
			{
				Content: []llm.ContentBlock{
					llm.ToolUseBlock{ID: "t1", Name: "echo", Input: []byte(`{"q":"x"}`)},
				},
				StopReason: "tool_use",
			},
			{Content: []llm.ContentBlock{llm.TextBlock{Text: "done"}}, StopReason: "end_turn"},
		},
	}
	a, _ := New(Spec{
		Name:     "x",
		Provider: prov,
		Tools:    disp,
		CycleLog: sink,
	}, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	reply := make(chan Outcome, 1)
	_ = a.Submit(ctx, Task{TaskID: "task-1", Instruction: "use echo", Reply: reply})
	out := <-reply
	if out.Err != nil {
		t.Fatal(out.Err)
	}

	// Expected: agent_cycle_start, llm_request, llm_response,
	// tool_call, tool_result, llm_request, llm_response,
	// agent_cycle_complete.
	wantTypes := []cyclelog.EventType{
		cyclelog.EventAgentCycleStart,
		cyclelog.EventLLMRequest,
		cyclelog.EventLLMResponse,
		cyclelog.EventToolCall,
		cyclelog.EventToolResult,
		cyclelog.EventLLMRequest,
		cyclelog.EventLLMResponse,
		cyclelog.EventAgentCycleComplete,
	}
	if len(sink.events) != len(wantTypes) {
		t.Fatalf("got %d events, want %d:\n%+v", len(sink.events), len(wantTypes), sink.events)
	}
	for i, want := range wantTypes {
		if sink.events[i].Type != want {
			t.Errorf("event[%d] = %q, want %q", i, sink.events[i].Type, want)
		}
	}

	// Find the tool_call + tool_result and check shape.
	for _, e := range sink.events {
		if e.Type == cyclelog.EventToolCall && (e.ToolName != "echo" || e.ToolInputLen == 0) {
			t.Errorf("tool_call wrong: %+v", e)
		}
		if e.Type == cyclelog.EventToolResult && (e.ToolName != "echo" || !e.Success) {
			t.Errorf("tool_result wrong: %+v", e)
		}
	}
}

func TestAgent_EmitsCompleteEventOnError(t *testing.T) {
	sink := &captureSink{}
	dagRec := &captureDAG{}
	prov := &erroringProvider{err: errors.New("boom")}
	a, _ := New(Spec{
		Name:                 "x",
		Provider:             prov,
		Tools:                &fakeDispatcher{},
		CycleLog:             sink,
		DAG:                  dagRec,
		MaxConsecutiveErrors: 1,
	}, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	reply := make(chan Outcome, 1)
	_ = a.Submit(ctx, Task{TaskID: "task-1", Instruction: "fail", Reply: reply})
	out := <-reply
	if out.Err == nil {
		t.Fatal("expected error")
	}

	// Last event should be agent_cycle_complete with status=error.
	last := sink.events[len(sink.events)-1]
	if last.Type != cyclelog.EventAgentCycleComplete {
		t.Errorf("last event = %q, want agent_cycle_complete", last.Type)
	}
	if last.Status != cyclelog.StatusError {
		t.Errorf("status = %q, want error", last.Status)
	}
	if last.Error == "" {
		t.Error("error message should be populated")
	}

	// DAG: CompleteCycle called with StatusError.
	if len(dagRec.completes) != 1 || dagRec.completes[0].Status != dag.StatusError {
		t.Errorf("DAG complete wrong: %+v", dagRec.completes)
	}
}

// fakeFabricationGate implements AgentFabricationGate for tests.
// shouldGateNames lists the tool names the gate considers high-risk;
// verdict is what ScoreToolInput returns regardless of input. The
// recordedInputs slice captures what was scored so tests can assert
// the gate received the right (toolName, input, log) tuple.
type fakeFabricationGate struct {
	shouldGateNames map[string]struct{}
	verdict         string // "allow" | "block"
	flagged         []string
	recordedInputs  []fakeGateCall
	mu              sync.Mutex
}

type fakeGateCall struct {
	tool    string
	input   string
	logSize int
}

func (g *fakeFabricationGate) ShouldGate(name string) bool {
	_, ok := g.shouldGateNames[name]
	return ok
}

func (g *fakeFabricationGate) ScoreToolInput(_ context.Context, tool, input string, log []FabricationToolEvent) FabricationGateResult {
	g.mu.Lock()
	g.recordedInputs = append(g.recordedInputs, fakeGateCall{tool: tool, input: input, logSize: len(log)})
	g.mu.Unlock()
	return FabricationGateResult{
		Verdict:       g.verdict,
		Score:         0.9,
		Trail:         "fake gate decision",
		FlaggedClaims: g.flagged,
	}
}

func TestAgent_FabricationGateBlocksHighRiskTool(t *testing.T) {
	prov := &scriptedProvider{
		scripts: []llm.Response{
			{
				Content: []llm.ContentBlock{
					llm.ToolUseBlock{ID: "c1", Name: "send_email", Input: []byte(`{"body":"fabricated content"}`)},
				},
				StopReason: "tool_use",
			},
			{
				Content:    []llm.ContentBlock{llm.TextBlock{Text: "I should verify first"}},
				StopReason: "end_turn",
			},
		},
	}
	disp := &fakeDispatcher{tools: []llm.Tool{{Name: "send_email"}}, res: "should-not-be-called"}
	gate := &fakeFabricationGate{
		shouldGateNames: map[string]struct{}{"send_email": {}},
		verdict:         "block",
		flagged:         []string{"fabricated URL"},
	}
	a, _ := New(Spec{
		Name:            "x",
		Provider:        prov,
		Tools:           disp,
		FabricationGate: gate,
	}, discardLogger())
	runAgent(t, a)

	reply := make(chan Outcome, 1)
	_ = a.Submit(context.Background(), Task{TaskID: "t1", Instruction: "send the email", Reply: reply})
	out := <-reply
	if out.Err != nil {
		t.Fatalf("err: %v", out.Err)
	}
	disp.mu.Lock()
	defer disp.mu.Unlock()
	if len(disp.calls) != 0 {
		t.Errorf("dispatcher should NOT have been called when gate blocked; got %+v", disp.calls)
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if len(gate.recordedInputs) != 1 {
		t.Fatalf("gate should have been scored once; got %d", len(gate.recordedInputs))
	}
	if gate.recordedInputs[0].tool != "send_email" {
		t.Errorf("gate scored wrong tool: %+v", gate.recordedInputs[0])
	}
}

func TestAgent_FabricationGateSkipsLowRiskTool(t *testing.T) {
	// brave_web_search isn't in the high-risk set; gate.ShouldGate
	// returns false. Dispatch proceeds normally.
	prov := &scriptedProvider{
		scripts: []llm.Response{
			{
				Content: []llm.ContentBlock{
					llm.ToolUseBlock{ID: "c1", Name: "brave_web_search", Input: []byte(`{"q":"x"}`)},
				},
				StopReason: "tool_use",
			},
			{
				Content:    []llm.ContentBlock{llm.TextBlock{Text: "got results"}},
				StopReason: "end_turn",
			},
		},
	}
	disp := &fakeDispatcher{tools: []llm.Tool{{Name: "brave_web_search"}}, res: "results"}
	gate := &fakeFabricationGate{
		shouldGateNames: map[string]struct{}{"send_email": {}}, // NOT including brave_web_search
		verdict:         "block",                                // would block IF called
	}
	a, _ := New(Spec{Name: "x", Provider: prov, Tools: disp, FabricationGate: gate}, discardLogger())
	runAgent(t, a)

	reply := make(chan Outcome, 1)
	_ = a.Submit(context.Background(), Task{TaskID: "t1", Instruction: "search", Reply: reply})
	out := <-reply
	if out.Err != nil {
		t.Fatalf("err: %v", out.Err)
	}
	disp.mu.Lock()
	defer disp.mu.Unlock()
	if len(disp.calls) != 1 {
		t.Errorf("low-risk tool should have dispatched normally; calls = %+v", disp.calls)
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if len(gate.recordedInputs) != 0 {
		t.Errorf("gate should NOT have been scored for low-risk tool; got %+v", gate.recordedInputs)
	}
}

func TestAgent_FabricationGateAllowsLetsDispatchProceed(t *testing.T) {
	// Gate marks send_email high-risk but returns allow; dispatch
	// proceeds. Confirms allow is a no-op vs the unguarded path
	// other than the LLM round-trip cost.
	prov := &scriptedProvider{
		scripts: []llm.Response{
			{
				Content: []llm.ContentBlock{
					llm.ToolUseBlock{ID: "c1", Name: "send_email", Input: []byte(`{"body":"verified content"}`)},
				},
				StopReason: "tool_use",
			},
			{
				Content:    []llm.ContentBlock{llm.TextBlock{Text: "sent"}},
				StopReason: "end_turn",
			},
		},
	}
	disp := &fakeDispatcher{tools: []llm.Tool{{Name: "send_email"}}, res: "delivered"}
	gate := &fakeFabricationGate{
		shouldGateNames: map[string]struct{}{"send_email": {}},
		verdict:         "allow",
	}
	a, _ := New(Spec{Name: "x", Provider: prov, Tools: disp, FabricationGate: gate}, discardLogger())
	runAgent(t, a)

	reply := make(chan Outcome, 1)
	_ = a.Submit(context.Background(), Task{TaskID: "t1", Instruction: "send", Reply: reply})
	out := <-reply
	if out.Err != nil {
		t.Fatalf("err: %v", out.Err)
	}
	disp.mu.Lock()
	defer disp.mu.Unlock()
	if len(disp.calls) != 1 {
		t.Errorf("allow verdict should have let dispatch proceed; calls = %+v", disp.calls)
	}
}

func TestAgent_FabricationGateLogReflectsPriorTools(t *testing.T) {
	// Two-tool sequence: brave_web_search (low-risk) succeeds, then
	// send_email (high-risk). The gate's tool log on the second
	// dispatch should contain the brave_web_search output.
	prov := &scriptedProvider{
		scripts: []llm.Response{
			{
				Content: []llm.ContentBlock{
					llm.ToolUseBlock{ID: "c1", Name: "brave_web_search", Input: []byte(`{"q":"x"}`)},
				},
				StopReason: "tool_use",
			},
			{
				Content: []llm.ContentBlock{
					llm.ToolUseBlock{ID: "c2", Name: "send_email", Input: []byte(`{"body":"based on search"}`)},
				},
				StopReason: "tool_use",
			},
			{
				Content:    []llm.ContentBlock{llm.TextBlock{Text: "done"}},
				StopReason: "end_turn",
			},
		},
	}
	disp := &fakeDispatcher{tools: []llm.Tool{{Name: "brave_web_search"}, {Name: "send_email"}}, res: "search-result-bytes"}
	gate := &fakeFabricationGate{
		shouldGateNames: map[string]struct{}{"send_email": {}},
		verdict:         "allow",
	}
	a, _ := New(Spec{Name: "x", Provider: prov, Tools: disp, FabricationGate: gate}, discardLogger())
	runAgent(t, a)

	reply := make(chan Outcome, 1)
	_ = a.Submit(context.Background(), Task{TaskID: "t1", Instruction: "search and send", Reply: reply})
	out := <-reply
	if out.Err != nil {
		t.Fatalf("err: %v", out.Err)
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if len(gate.recordedInputs) != 1 {
		t.Fatalf("gate should have been scored once (for send_email only); got %d", len(gate.recordedInputs))
	}
	if gate.recordedInputs[0].logSize != 1 {
		t.Errorf("expected gate to see 1 prior tool event (brave_web_search); got logSize=%d", gate.recordedInputs[0].logSize)
	}
}

func TestAgent_FabricationGateNilIsNoOp(t *testing.T) {
	// Nil gate → unguarded dispatch path. The legacy behaviour
	// before this hook landed.
	prov := &scriptedProvider{
		scripts: []llm.Response{
			{
				Content: []llm.ContentBlock{
					llm.ToolUseBlock{ID: "c1", Name: "send_email", Input: []byte(`{"body":"x"}`)},
				},
				StopReason: "tool_use",
			},
			{
				Content:    []llm.ContentBlock{llm.TextBlock{Text: "sent"}},
				StopReason: "end_turn",
			},
		},
	}
	disp := &fakeDispatcher{tools: []llm.Tool{{Name: "send_email"}}, res: "ok"}
	a, _ := New(Spec{Name: "x", Provider: prov, Tools: disp}, discardLogger())
	runAgent(t, a)

	reply := make(chan Outcome, 1)
	_ = a.Submit(context.Background(), Task{TaskID: "t1", Instruction: "send", Reply: reply})
	out := <-reply
	if out.Err != nil {
		t.Fatalf("err: %v", out.Err)
	}
	disp.mu.Lock()
	defer disp.mu.Unlock()
	if len(disp.calls) != 1 {
		t.Errorf("nil gate should pass through; calls = %+v", disp.calls)
	}
}

func TestAgent_NilCycleLogIsSilent(t *testing.T) {
	prov := &scriptedProvider{
		scripts: []llm.Response{
			{Content: []llm.ContentBlock{llm.TextBlock{Text: "done"}}, StopReason: "end_turn"},
		},
	}
	a, _ := New(Spec{Name: "x", Provider: prov, Tools: &fakeDispatcher{}}, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	reply := make(chan Outcome, 1)
	_ = a.Submit(ctx, Task{TaskID: "task-1", Instruction: "x", Reply: reply})
	out := <-reply
	if out.Err != nil {
		t.Fatal(out.Err)
	}
	// No assertions on events — the test confirms running without
	// telemetry doesn't panic / fail.
}
