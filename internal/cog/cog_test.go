package cog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seamus-brady/retainer/internal/cyclelog"
	"github.com/seamus-brady/retainer/internal/dag"
	"github.com/seamus-brady/retainer/internal/llm"
	"github.com/seamus-brady/retainer/internal/policy"
)

type recordingSink struct {
	mu     sync.Mutex
	events []cyclelog.Event
}

func (r *recordingSink) Emit(ev cyclelog.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

func (r *recordingSink) snapshot() []cyclelog.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]cyclelog.Event, len(r.events))
	copy(out, r.events)
	return out
}

// waitForEvents polls snapshot() until len(events) >= want or the
// deadline expires. Returns the latest snapshot. Used to dodge the
// race between the cog's reply-channel send and the cycle_complete
// emission — the reply happens first; the event lands in the sink
// shortly after on a separate code path. Reading immediately after
// <-Submit can catch the gap on slower runners (Linux CI).
func (r *recordingSink) waitForEvents(t *testing.T, want int, timeout time.Duration) []cyclelog.Event {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := r.snapshot(); len(got) >= want {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	return r.snapshot()
}

func runDAGForTest(t *testing.T) *dag.DAG {
	t.Helper()
	d := dag.New()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go d.Run(ctx)
	return d
}

type fakeProvider struct {
	mu       sync.Mutex
	reply    string
	err      error
	delay    time.Duration
	requests []llm.Request
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) ChatStructured(ctx context.Context, req llm.Request, schema llm.Schema, dst any) (llm.Usage, error) {
	return llm.Usage{}, errors.New("fakeProvider: ChatStructured not implemented for cog tests")
}

func (f *fakeProvider) Chat(ctx context.Context, req llm.Request) (llm.Response, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	delay := f.delay
	reply := f.reply
	err := f.err
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return llm.Response{}, ctx.Err()
		}
	}
	if err != nil {
		return llm.Response{}, err
	}
	return llm.Response{
		Content:    []llm.ContentBlock{llm.TextBlock{Text: reply}},
		StopReason: "end_turn",
	}, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestCog_BasicFlow(t *testing.T) {
	c := New(Config{
		Provider: &fakeProvider{reply: "hi"},
		Model:    "fake",
		Logger:   discardLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	r := <-c.Submit(ctx, "hello")
	if r.Err != nil {
		t.Fatalf("err: %v", r.Err)
	}
	if r.Text != "hi" {
		t.Fatalf("text = %q, want hi", r.Text)
	}
}

func TestCog_HistoryAccumulates(t *testing.T) {
	f := &fakeProvider{reply: "first reply"}
	c := New(Config{
		Provider: f,
		Model:    "fake",
		Logger:   discardLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	r1 := <-c.Submit(ctx, "msg one")
	if r1.Err != nil {
		t.Fatalf("r1 err: %v", r1.Err)
	}

	f.mu.Lock()
	f.reply = "second reply"
	f.mu.Unlock()

	r2 := <-c.Submit(ctx, "msg two")
	if r2.Err != nil {
		t.Fatalf("r2 err: %v", r2.Err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 2 {
		t.Fatalf("got %d requests, want 2", len(f.requests))
	}
	// Second request must include msg one, first reply, msg two.
	msgs := f.requests[1].Messages
	if len(msgs) != 3 {
		t.Fatalf("second request msgs = %d, want 3", len(msgs))
	}
	if msgs[0].Role != llm.RoleUser || msgs[1].Role != llm.RoleAssistant || msgs[2].Role != llm.RoleUser {
		t.Fatalf("roles wrong: %v %v %v", msgs[0].Role, msgs[1].Role, msgs[2].Role)
	}
}

func TestCog_QueuesWhenBusy(t *testing.T) {
	f := &fakeProvider{reply: "ok", delay: 80 * time.Millisecond}
	c := New(Config{
		Provider: f,
		Model:    "fake",
		Logger:   discardLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	r1 := c.Submit(ctx, "first")
	r2 := c.Submit(ctx, "second")

	if reply := <-r1; reply.Err != nil {
		t.Fatalf("r1 err: %v", reply.Err)
	}
	if reply := <-r2; reply.Err != nil {
		t.Fatalf("r2 err: %v", reply.Err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 2 {
		t.Fatalf("got %d requests, want 2", len(f.requests))
	}
}

func TestCog_QueueFullReturnsError(t *testing.T) {
	f := &fakeProvider{reply: "ok", delay: 200 * time.Millisecond}
	c := New(Config{
		Provider:      f,
		Model:         "fake",
		Logger:        discardLogger(),
		InputQueueCap: 1,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	r1 := c.Submit(ctx, "in flight")
	r2 := c.Submit(ctx, "first queued")
	r3 := c.Submit(ctx, "overflow")

	// r1 succeeds eventually; r2 succeeds eventually; r3 must fail.
	reply3 := <-r3
	if reply3.Err == nil {
		t.Fatalf("expected queue-full error on third submission")
	}
	if !strings.Contains(reply3.Err.Error(), "queue full") {
		t.Fatalf("err = %v, want 'queue full'", reply3.Err)
	}
	// Drain the others so the cog finishes cleanly.
	<-r1
	<-r2
}

func TestCog_WatchdogFiresOnSlowProvider(t *testing.T) {
	f := &fakeProvider{reply: "late", delay: 500 * time.Millisecond}
	c := New(Config{
		Provider:    f,
		Model:       "fake",
		Logger:      discardLogger(),
		GateTimeout: 30 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	reply := <-c.Submit(ctx, "hello")
	if reply.Err == nil {
		t.Fatal("expected watchdog timeout error")
	}
	if !strings.Contains(reply.Err.Error(), "watchdog") {
		t.Fatalf("err = %v, want watchdog timeout", reply.Err)
	}
}

func TestCog_ProviderErrorSurfaces(t *testing.T) {
	sentinel := errors.New("provider down")
	f := &fakeProvider{err: sentinel}
	c := New(Config{
		Provider: f,
		Model:    "fake",
		Logger:   discardLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	reply := <-c.Submit(ctx, "hello")
	if reply.Err == nil {
		t.Fatal("expected provider error")
	}
	if !errors.Is(reply.Err, sentinel) {
		t.Fatalf("err = %v, want %v", reply.Err, sentinel)
	}
}

func TestCog_AfterErrorCanContinue(t *testing.T) {
	// After a provider error, the cog should return to Idle and accept the
	// next UserInput cleanly.
	f := &fakeProvider{err: errors.New("first call fails")}
	c := New(Config{
		Provider: f,
		Model:    "fake",
		Logger:   discardLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	if reply := <-c.Submit(ctx, "first"); reply.Err == nil {
		t.Fatal("expected first to error")
	}

	f.mu.Lock()
	f.err = nil
	f.reply = "recovered"
	f.mu.Unlock()

	reply := <-c.Submit(ctx, "second")
	if reply.Err != nil {
		t.Fatalf("second err: %v", reply.Err)
	}
	if reply.Text != "recovered" {
		t.Fatalf("text = %q, want recovered", reply.Text)
	}
}

func TestCog_EmitsCyclelogEvents(t *testing.T) {
	sink := &recordingSink{}
	c := New(Config{
		Provider: &fakeProvider{reply: "hi"},
		Model:    "fake",
		Logger:   discardLogger(),
		CycleLog: sink,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	r := <-c.Submit(ctx, "hello")
	if r.Err != nil {
		t.Fatalf("err: %v", r.Err)
	}

	events := sink.waitForEvents(t, 4, time.Second)
	if len(events) < 4 {
		t.Fatalf("got %d events, want at least 4 (start, llm_request, llm_response, complete): %+v", len(events), events)
	}
	wantSeq := []cyclelog.EventType{
		cyclelog.EventCycleStart,
		cyclelog.EventLLMRequest,
		cyclelog.EventLLMResponse,
		cyclelog.EventCycleComplete,
	}
	for i, want := range wantSeq {
		if events[i].Type != want {
			t.Errorf("event[%d] type = %q, want %q", i, events[i].Type, want)
		}
	}
	cycleID := events[0].CycleID
	if cycleID == "" {
		t.Fatal("cycle_id missing on cycle_start")
	}
	for i, e := range events[:4] {
		if e.CycleID != cycleID {
			t.Errorf("event[%d] cycle_id = %q, want %q (mismatched)", i, e.CycleID, cycleID)
		}
	}
	if events[3].Status != cyclelog.StatusComplete {
		t.Errorf("complete status = %q, want complete", events[3].Status)
	}
}

func TestCog_RecordsCycleInDAG(t *testing.T) {
	sink := &recordingSink{}
	d := runDAGForTest(t)
	c := New(Config{
		Provider: &fakeProvider{reply: "hi"},
		Model:    "fake",
		Logger:   discardLogger(),
		CycleLog: sink,
		DAG:      d,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	r := <-c.Submit(ctx, "hello")
	if r.Err != nil {
		t.Fatalf("err: %v", r.Err)
	}

	events := sink.waitForEvents(t, 1, time.Second)
	cycleID := events[0].CycleID
	node := d.Get(dag.CycleID(cycleID))
	if node == nil {
		t.Fatalf("DAG missing node for cycle %s", cycleID)
	}
	if node.Type != dag.NodeCognitive {
		t.Errorf("node type = %q, want cognitive", node.Type)
	}
	if node.Status != dag.StatusComplete {
		t.Errorf("node status = %q, want complete", node.Status)
	}
}

func TestCog_WatchdogEmitsAbandonEvent(t *testing.T) {
	sink := &recordingSink{}
	c := New(Config{
		Provider:    &fakeProvider{reply: "late", delay: 500 * time.Millisecond},
		Model:       "fake",
		Logger:      discardLogger(),
		CycleLog:    sink,
		GateTimeout: 30 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	reply := <-c.Submit(ctx, "slow")
	if reply.Err == nil {
		t.Fatal("expected watchdog timeout error")
	}

	// Wait briefly so watchdog event has time to be emitted.
	time.Sleep(20 * time.Millisecond)

	events := sink.snapshot()
	var sawWatchdog, sawAbandon bool
	for _, e := range events {
		if e.Type == cyclelog.EventWatchdogFire {
			sawWatchdog = true
		}
		if e.Type == cyclelog.EventCycleComplete && e.Status == cyclelog.StatusAbandon {
			sawAbandon = true
		}
	}
	if !sawWatchdog {
		t.Error("expected watchdog_fire event")
	}
	if !sawAbandon {
		t.Error("expected cycle_complete with status=abandoned")
	}
}

func TestCog_PolicyAllowsAndDispatches(t *testing.T) {
	engine := policy.New(policy.Config{
		Rules: &policy.RuleSet{InputThreshold: 0.4, OutputThreshold: 0.4},
	})
	c := New(Config{
		Provider: &fakeProvider{reply: "hi"},
		Model:    "fake",
		Logger:   discardLogger(),
		Policy:   engine,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	r := <-c.Submit(ctx, "hello there")
	if r.Err != nil {
		t.Fatalf("err: %v", r.Err)
	}
	if r.Text != "hi" {
		t.Fatalf("text = %q, want hi", r.Text)
	}
}

func TestCog_PolicyBlocksInputAndSkipsLLM(t *testing.T) {
	engine := policy.New(policy.Config{
		Rules: &policy.RuleSet{
			InputThreshold:  0.4,
			OutputThreshold: 0.4,
			Input: []policy.Rule{
				{
					Name:       "block_marker",
					Pattern:    regexp.MustCompile(`(?i)\bblockme\b`),
					Importance: 1.0,
					Magnitude:  1.0,
				},
			},
		},
	})
	f := &fakeProvider{reply: "should not see this"}
	c := New(Config{
		Provider:         f,
		Model:            "fake",
		Logger:           discardLogger(),
		Policy:           engine,
		InputRefusalText: "no thanks",
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	r := <-c.Submit(ctx, "blockme please")
	if r.Kind != ReplyKindRefusal {
		t.Fatalf("kind = %v, want Refusal; err=%v text=%q", r.Kind, r.Err, r.Text)
	}
	if r.Text != "no thanks" {
		t.Fatalf("text = %q, want %q", r.Text, "no thanks")
	}
	if r.Err != nil {
		t.Errorf("Err should be nil for Refusal, got %v", r.Err)
	}

	f.mu.Lock()
	got := len(f.requests)
	f.mu.Unlock()
	if got != 0 {
		t.Fatalf("LLM was called %d times despite input policy block", got)
	}
}

func TestCog_RefusalRecordedOnDAGNode(t *testing.T) {
	d := runDAGForTest(t)
	engine := policy.New(policy.Config{
		Rules: &policy.RuleSet{
			InputThreshold: 0.4,
			Input: []policy.Rule{
				{
					Name:       "block_marker",
					Pattern:    regexp.MustCompile(`(?i)\bblockme\b`),
					Importance: 1.0,
					Magnitude:  1.0,
				},
			},
		},
	})
	c := New(Config{
		Provider: &fakeProvider{},
		Model:    "fake",
		Logger:   discardLogger(),
		Policy:   engine,
		DAG:      d,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	r := <-c.Submit(ctx, "blockme right now")
	if r.Kind != ReplyKindRefusal {
		t.Fatalf("kind = %v, want Refusal", r.Kind)
	}

	// Find the cycle's DAG node and verify the error string is captured.
	// We don't know the cycle id directly, but there should be exactly one node.
	children := d.Children("")
	if len(children) != 1 {
		t.Fatalf("got %d root nodes, want 1", len(children))
	}
	n := children[0]
	if n.Status != dag.StatusError {
		t.Errorf("node status = %q, want error", n.Status)
	}
	if n.ErrorMessage == "" {
		t.Error("ErrorMessage should be populated on refused cycle")
	}
	if !strings.Contains(n.ErrorMessage, "input policy") {
		t.Errorf("ErrorMessage = %q, want 'input policy' in it", n.ErrorMessage)
	}
}

func TestCog_PolicyEmitsDecisionEvents(t *testing.T) {
	sink := &recordingSink{}
	engine := policy.New(policy.Config{
		Rules: &policy.RuleSet{InputThreshold: 0.4, OutputThreshold: 0.4},
	})
	c := New(Config{
		Provider: &fakeProvider{reply: "hi"},
		Model:    "fake",
		Logger:   discardLogger(),
		Policy:   engine,
		CycleLog: sink,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	if r := <-c.Submit(ctx, "hello"); r.Err != nil {
		t.Fatalf("err: %v", r.Err)
	}

	// At least 4 events for the success path: cycle_start,
	// policy_decision (input), llm_request, llm_response,
	// policy_decision (output), cycle_complete.
	events := sink.waitForEvents(t, 4, time.Second)
	var sawInputDecision, sawOutputDecision bool
	for _, e := range events {
		if e.Type != cyclelog.EventPolicyDecision {
			continue
		}
		switch e.Gate {
		case "input":
			sawInputDecision = true
			if e.Verdict != "allow" {
				t.Errorf("input verdict = %q, want allow", e.Verdict)
			}
		case "output":
			sawOutputDecision = true
			if e.Verdict != "allow" {
				t.Errorf("output verdict = %q, want allow", e.Verdict)
			}
		}
	}
	if !sawInputDecision {
		t.Error("missing input policy_decision event")
	}
	if !sawOutputDecision {
		t.Error("missing output policy_decision event")
	}
}

type fakeDispatcher struct {
	tools  []llm.Tool
	mu     sync.Mutex
	calls  []dispatchCall
	result string
	err    error
}

type dispatchCall struct {
	name  string
	input []byte
}

func (d *fakeDispatcher) List() []llm.Tool { return d.tools }

func (d *fakeDispatcher) Dispatch(ctx context.Context, name string, input []byte) (string, error) {
	d.mu.Lock()
	d.calls = append(d.calls, dispatchCall{name: name, input: append([]byte(nil), input...)})
	res := d.result
	err := d.err
	d.mu.Unlock()
	return res, err
}

// scriptedProvider returns canned responses in order. Each call pops the next
// response. Used to script multi-turn react loops.
type scriptedProvider struct {
	mu       sync.Mutex
	idx      int
	scripts  []llm.Response
	requests []llm.Request
}

func (*scriptedProvider) Name() string { return "scripted" }

func (*scriptedProvider) ChatStructured(context.Context, llm.Request, llm.Schema, any) (llm.Usage, error) {
	return llm.Usage{}, errors.New("scriptedProvider: ChatStructured not implemented")
}

func (s *scriptedProvider) Chat(_ context.Context, req llm.Request) (llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req)
	if s.idx >= len(s.scripts) {
		return llm.Response{}, fmt.Errorf("scriptedProvider: exhausted at call %d", s.idx)
	}
	r := s.scripts[s.idx]
	s.idx++
	return r, nil
}

// dispatchCallCtx is a fakeDispatcher variant that records the context
// passed to Dispatch — proves the cog injects the in-flight cog cycle ID
// via cyclelog.WithCycleID so memory tools can stamp facts with
// provenance.
type ctxRecordingDispatcher struct {
	tools  []llm.Tool
	mu     sync.Mutex
	ctxIDs []string
	result string
}

func (d *ctxRecordingDispatcher) List() []llm.Tool { return d.tools }

func (d *ctxRecordingDispatcher) Dispatch(ctx context.Context, _ string, _ []byte) (string, error) {
	d.mu.Lock()
	d.ctxIDs = append(d.ctxIDs, cyclelog.CycleIDFromContext(ctx))
	res := d.result
	d.mu.Unlock()
	return res, nil
}

func TestCog_DispatchCarriesCycleIDInContext(t *testing.T) {
	disp := &ctxRecordingDispatcher{
		tools:  []llm.Tool{{Name: "echo"}},
		result: "ok",
	}
	prov := &scriptedProvider{
		scripts: []llm.Response{
			{
				Content:    []llm.ContentBlock{llm.ToolUseBlock{ID: "c1", Name: "echo", Input: []byte(`{}`)}},
				StopReason: "tool_use",
			},
			{Content: []llm.ContentBlock{llm.TextBlock{Text: "done"}}, StopReason: "end_turn"},
		},
	}
	c := New(Config{Provider: prov, Model: "fake", Logger: discardLogger(), Tools: disp})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	r := <-c.Submit(ctx, "do it")
	if r.Err != nil {
		t.Fatalf("err: %v", r.Err)
	}

	disp.mu.Lock()
	defer disp.mu.Unlock()
	if len(disp.ctxIDs) != 1 {
		t.Fatalf("dispatch calls = %d, want 1", len(disp.ctxIDs))
	}
	if disp.ctxIDs[0] == "" {
		t.Errorf("dispatcher saw empty cycle ID — cog should inject it via context")
	}
}

func TestCog_ReactLoopRunsTool(t *testing.T) {
	disp := &fakeDispatcher{
		tools: []llm.Tool{{
			Name:        "echo",
			Description: "echoes args back",
			InputSchema: llm.Schema{Properties: map[string]llm.Property{"q": {Type: "string"}}, Required: []string{"q"}},
		}},
		result: "tool said: hi",
	}
	prov := &scriptedProvider{
		scripts: []llm.Response{
			{
				Content: []llm.ContentBlock{
					llm.ToolUseBlock{ID: "call_1", Name: "echo", Input: []byte(`{"q":"hi"}`)},
				},
				StopReason: "tool_use",
			},
			{
				Content:    []llm.ContentBlock{llm.TextBlock{Text: "final answer"}},
				StopReason: "end_turn",
			},
		},
	}
	c := New(Config{
		Provider: prov,
		Model:    "fake",
		Logger:   discardLogger(),
		Tools:    disp,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	r := <-c.Submit(ctx, "use the echo tool")
	if r.Err != nil {
		t.Fatalf("err: %v", r.Err)
	}
	if r.Text != "final answer" {
		t.Fatalf("text = %q, want 'final answer'", r.Text)
	}

	disp.mu.Lock()
	calls := append([]dispatchCall(nil), disp.calls...)
	disp.mu.Unlock()
	if len(calls) != 1 || calls[0].name != "echo" {
		t.Fatalf("dispatcher calls = %+v, want one 'echo'", calls)
	}

	prov.mu.Lock()
	defer prov.mu.Unlock()
	if len(prov.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2 (initial + post-tool)", len(prov.requests))
	}
	// Both requests must advertise the tool.
	for i, req := range prov.requests {
		if len(req.Tools) != 1 || req.Tools[0].Name != "echo" {
			t.Errorf("req[%d] tools = %+v, want one 'echo'", i, req.Tools)
		}
	}
	// Second request must include assistant(tool_use) + user(tool_result).
	msgs := prov.requests[1].Messages
	if len(msgs) != 3 {
		t.Fatalf("post-tool msgs = %d, want 3 (user, assistant, user)", len(msgs))
	}
	if _, ok := msgs[1].Content[0].(llm.ToolUseBlock); !ok {
		t.Errorf("msg[1].content[0] = %T, want ToolUseBlock", msgs[1].Content[0])
	}
	tr, ok := msgs[2].Content[0].(llm.ToolResultBlock)
	if !ok {
		t.Fatalf("msg[2].content[0] = %T, want ToolResultBlock", msgs[2].Content[0])
	}
	if tr.ToolUseID != "call_1" || tr.Content != "tool said: hi" {
		t.Errorf("tool_result = %+v", tr)
	}
}

func TestCog_ReactLoopSurfacesToolError(t *testing.T) {
	// When a tool's Dispatch returns an error, the cog should NOT abandon
	// the cycle — it should surface the error to the model as
	// IsError=true so the model can recover. Subsequent LLM call gets the
	// error tool_result, and the model's text reply lands as the final.
	dispErr := errors.New("brave api down")
	disp := &fakeDispatcher{
		tools: []llm.Tool{{Name: "echo"}},
		err:   dispErr,
	}
	prov := &scriptedProvider{
		scripts: []llm.Response{
			{
				Content:    []llm.ContentBlock{llm.ToolUseBlock{ID: "c1", Name: "echo", Input: []byte(`{}`)}},
				StopReason: "tool_use",
			},
			{
				Content:    []llm.ContentBlock{llm.TextBlock{Text: "sorry, search failed"}},
				StopReason: "end_turn",
			},
		},
	}
	c := New(Config{Provider: prov, Model: "fake", Logger: discardLogger(), Tools: disp})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	r := <-c.Submit(ctx, "do it")
	if r.Err != nil {
		t.Fatalf("err: %v", r.Err)
	}
	if r.Text != "sorry, search failed" {
		t.Fatalf("text = %q", r.Text)
	}

	// The 2nd request must have a ToolResultBlock with IsError=true and
	// the error message as content.
	prov.mu.Lock()
	defer prov.mu.Unlock()
	if len(prov.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(prov.requests))
	}
	msgs := prov.requests[1].Messages
	tr, ok := msgs[len(msgs)-1].Content[0].(llm.ToolResultBlock)
	if !ok {
		t.Fatalf("last block = %T", msgs[len(msgs)-1].Content[0])
	}
	if !tr.IsError {
		t.Errorf("IsError = false, want true")
	}
	if !strings.Contains(tr.Content, "brave api down") {
		t.Errorf("content = %q, want error message", tr.Content)
	}
}

func TestCog_ToolUseWithoutDispatcherIsError(t *testing.T) {
	// If a model emits tool_use but the cog has no Tools registered (a
	// protocol violation — the request shouldn't have advertised tools)
	// the cycle abandons with an error.
	prov := &scriptedProvider{scripts: []llm.Response{{
		Content:    []llm.ContentBlock{llm.ToolUseBlock{ID: "c1", Name: "echo", Input: []byte(`{}`)}},
		StopReason: "tool_use",
	}}}
	c := New(Config{Provider: prov, Model: "fake", Logger: discardLogger()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	r := <-c.Submit(ctx, "x")
	if r.Err == nil || !strings.Contains(r.Err.Error(), "no ToolDispatcher") {
		t.Fatalf("err = %v, want no-dispatcher error", r.Err)
	}
}

func TestCog_ReactLoopRespectsMaxTurns(t *testing.T) {
	disp := &fakeDispatcher{
		tools:  []llm.Tool{{Name: "echo", InputSchema: llm.Schema{}}},
		result: "ok",
	}
	// Always reply with another tool_use so we eventually trip max turns.
	loopResp := llm.Response{
		Content:    []llm.ContentBlock{llm.ToolUseBlock{ID: "call_x", Name: "echo", Input: []byte(`{}`)}},
		StopReason: "tool_use",
	}
	prov := &scriptedProvider{scripts: []llm.Response{loopResp, loopResp, loopResp, loopResp, loopResp, loopResp}}
	c := New(Config{
		Provider:     prov,
		Model:        "fake",
		Logger:       discardLogger(),
		Tools:        disp,
		MaxToolTurns: 2,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	r := <-c.Submit(ctx, "loop forever")
	if r.Err == nil {
		t.Fatal("expected max_tool_turns error")
	}
	if !strings.Contains(r.Err.Error(), "max_tool_turns") {
		t.Fatalf("err = %v, want max_tool_turns mention", r.Err)
	}
}

// ---- Activity channel ----

// drainActivities pulls every Activity sent so far + any that arrive
// within the deadline. Used by the activity tests since the cog doesn't
// expose a sync barrier on its emissions.
func drainActivities(c *Cog, deadline time.Duration) []Activity {
	var out []Activity
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	for {
		select {
		case a := <-c.Activities():
			out = append(out, a)
		case <-timer.C:
			return out
		}
	}
}

func TestCog_ActivityEmittedOnUserInputCycle(t *testing.T) {
	c := New(Config{
		Provider: &fakeProvider{reply: "ok"},
		Model:    "fake",
		Logger:   discardLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	r := <-c.Submit(ctx, "hi")
	if r.Err != nil {
		t.Fatalf("err: %v", r.Err)
	}
	// Drain after the reply so the activity goroutine has time to emit.
	acts := drainActivities(c, 100*time.Millisecond)

	// Expect at least: Thinking → Idle.
	sawThinking := false
	sawIdle := false
	for _, a := range acts {
		if a.Status == StatusThinking {
			sawThinking = true
		}
		if a.Status == StatusIdle {
			sawIdle = true
		}
	}
	if !sawThinking {
		t.Errorf("missing StatusThinking activity; got %+v", acts)
	}
	if !sawIdle {
		t.Errorf("missing StatusIdle activity; got %+v", acts)
	}
}

func TestCog_ActivityCarriesToolNames(t *testing.T) {
	disp := &fakeDispatcher{
		tools:  []llm.Tool{{Name: "echo"}},
		result: "ok",
	}
	prov := &scriptedProvider{
		scripts: []llm.Response{
			{
				Content: []llm.ContentBlock{
					llm.ToolUseBlock{ID: "c1", Name: "brave_web_search", Input: []byte(`{}`)},
					llm.ToolUseBlock{ID: "c2", Name: "jina_reader", Input: []byte(`{}`)},
				},
				StopReason: "tool_use",
			},
			{Content: []llm.ContentBlock{llm.TextBlock{Text: "done"}}, StopReason: "end_turn"},
		},
	}
	c := New(Config{Provider: prov, Model: "fake", Logger: discardLogger(), Tools: disp})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	r := <-c.Submit(ctx, "go")
	if r.Err != nil {
		t.Fatalf("err: %v", r.Err)
	}
	acts := drainActivities(c, 100*time.Millisecond)

	var toolAct *Activity
	for i := range acts {
		if acts[i].Status == StatusUsingTools && len(acts[i].ToolNames) > 0 {
			toolAct = &acts[i]
			break
		}
	}
	if toolAct == nil {
		t.Fatalf("no UsingTools activity with tool names; got %+v", acts)
	}
	if len(toolAct.ToolNames) != 2 {
		t.Fatalf("tool names = %+v, want 2", toolAct.ToolNames)
	}
	if toolAct.ToolNames[0] != "brave_web_search" || toolAct.ToolNames[1] != "jina_reader" {
		t.Errorf("tool names order wrong: %+v", toolAct.ToolNames)
	}
}

func TestCog_ActivityCarriesTurnAndCycleID(t *testing.T) {
	c := New(Config{
		Provider: &fakeProvider{reply: "ok"},
		Model:    "fake",
		Logger:   discardLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	r := <-c.Submit(ctx, "hi")
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	acts := drainActivities(c, 100*time.Millisecond)

	// The cog emits StatusThinking twice per dispatch: once on the
	// bare transition (turn=0, before pendingTurns increments), then
	// again from dispatchLLM after the increment. Subscribers always
	// see the latest, so we look for ANY StatusThinking activity with
	// the post-increment turn count.
	sawTurnAware := false
	sawCycleID := ""
	for _, a := range acts {
		if a.Status != StatusThinking {
			continue
		}
		if a.CycleID != "" {
			sawCycleID = a.CycleID
		}
		if a.Turn >= 1 {
			sawTurnAware = true
		}
	}
	if sawCycleID == "" {
		t.Errorf("no StatusThinking activity carried a cycle_id; got %+v", acts)
	}
	if !sawTurnAware {
		t.Errorf("no StatusThinking activity with turn >= 1; got %+v", acts)
	}
}

func TestCog_ActivityNoSubscriberDoesNotDeadlock(t *testing.T) {
	// Deliberately do not read c.Activities(). The channel buffer
	// (16) plus the non-blocking send means activity emissions drop
	// silently — the cycle must still succeed end to end.
	c := New(Config{
		Provider: &fakeProvider{reply: "ok"},
		Model:    "fake",
		Logger:   discardLogger(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go c.Run(ctx)

	for i := 0; i < 30; i++ {
		r := <-c.Submit(ctx, "hi")
		if r.Err != nil {
			t.Fatalf("cycle %d errored: %v", i, r.Err)
		}
	}
}

func TestCog_SubmitWithCancelledCtx(t *testing.T) {
	c := New(Config{
		Provider: &fakeProvider{reply: "hi"},
		Model:    "fake",
		Logger:   discardLogger(),
	})
	// Don't run the cog. Inbox never drains, Submit's send will block until
	// ctx fires.
	for i := 0; i < cap(c.inbox); i++ {
		c.inbox <- watchdogFire{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reply := <-c.Submit(ctx, "hello")
	if reply.Err == nil {
		t.Fatal("expected ctx error from Submit")
	}
}

// ---- RequestMoreTurns ----

// Note on testing the cap-extension end-to-end: integrating
// "tool calls request_more_turns mid-cycle" is best exercised
// against a real LLM via the integration suite — between cog
// turns is exactly the window we'd need to time the bump, and
// scripted-provider scaffolding is too coarse to land it
// reliably. The unit tests below cover the public-API surface
// (rejections, clamping, the default constants).

func TestRequestMoreTurns_RejectsWhenNoCycleInFlight(t *testing.T) {
	c := New(Config{
		Provider: &fakeProvider{reply: "ok"},
		Model:    "fake",
		Logger:   discardLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	_, err := c.RequestMoreTurns("", 3, "test")
	if err == nil {
		t.Fatal("expected error when no cycle is in flight")
	}
	if !strings.Contains(err.Error(), "no cycle in flight") {
		t.Errorf("err = %v", err)
	}
}

func TestRequestMoreTurns_RejectsZeroOrNegativeAdditional(t *testing.T) {
	c := New(Config{
		Provider: &fakeProvider{reply: "ok"},
		Model:    "fake",
		Logger:   discardLogger(),
	})
	if _, err := c.RequestMoreTurns("", 0, "test"); err == nil {
		t.Error("expected error for additional=0")
	}
	if _, err := c.RequestMoreTurns("", -5, "test"); err == nil {
		t.Error("expected error for negative additional")
	}
}

func TestRequestMoreTurns_RejectsEmptyReason(t *testing.T) {
	c := New(Config{
		Provider: &fakeProvider{reply: "ok"},
		Model:    "fake",
		Logger:   discardLogger(),
	})
	if _, err := c.RequestMoreTurns("", 3, ""); err == nil {
		t.Error("expected error for empty reason")
	}
}

func TestOnRequestMoreTurns_ClampsAtHardCap(t *testing.T) {
	// Direct test of the handler path — bypasses the actor inbox
	// to avoid Submit-vs-cycle race. Sets up the state as if a
	// cycle is in flight near the hard cap, then asks for a
	// huge jump. Result must clamp at hardMaxToolTurns.
	c := New(Config{
		Provider:     &fakeProvider{reply: "ok"},
		Model:        "fake",
		Logger:       discardLogger(),
		MaxToolTurns: hardMaxToolTurns - 1,
	})
	c.state.pendingCycleID = "cyc-test"
	c.state.pendingMaxToolTurns = hardMaxToolTurns - 1

	reply := make(chan requestMoreTurnsResult, 1)
	c.onRequestMoreTurns(requestMoreTurnsMsg{
		additional:    100,
		reason:        "big jump",
		parentCycleID: "cyc-test",
		reply:         reply,
	})
	r := <-reply
	if r.Err != nil {
		t.Fatalf("unexpected error: %v", r.Err)
	}
	if r.NewCap != hardMaxToolTurns {
		t.Errorf("NewCap = %d, want %d (hard cap)", r.NewCap, hardMaxToolTurns)
	}
}

func TestOnRequestMoreTurns_RefusesAtHardCap(t *testing.T) {
	c := New(Config{Provider: &fakeProvider{reply: "ok"}, Model: "fake", Logger: discardLogger()})
	c.state.pendingCycleID = "cyc-test"
	c.state.pendingMaxToolTurns = hardMaxToolTurns

	reply := make(chan requestMoreTurnsResult, 1)
	c.onRequestMoreTurns(requestMoreTurnsMsg{
		additional:    1,
		reason:        "more please",
		parentCycleID: "cyc-test",
		reply:         reply,
	})
	r := <-reply
	if r.Err == nil {
		t.Fatal("expected error when already at hard cap")
	}
	if r.NewCap != hardMaxToolTurns {
		t.Errorf("NewCap = %d, want %d (unchanged)", r.NewCap, hardMaxToolTurns)
	}
}

func TestOnRequestMoreTurns_RejectsStaleParentCycle(t *testing.T) {
	c := New(Config{Provider: &fakeProvider{reply: "ok"}, Model: "fake", Logger: discardLogger()})
	c.state.pendingCycleID = "cyc-current"
	c.state.pendingMaxToolTurns = c.cfg.MaxToolTurns

	reply := make(chan requestMoreTurnsResult, 1)
	c.onRequestMoreTurns(requestMoreTurnsMsg{
		additional:    3,
		reason:        "test",
		parentCycleID: "cyc-stale",
		reply:         reply,
	})
	r := <-reply
	if r.Err == nil {
		t.Fatal("expected stale-parent error")
	}
	if !strings.Contains(r.Err.Error(), "stale parent") {
		t.Errorf("err = %v, want stale-parent mention", r.Err)
	}
}

func TestOnRequestMoreTurns_RaisesCapOnHappyPath(t *testing.T) {
	c := New(Config{Provider: &fakeProvider{reply: "ok"}, Model: "fake", Logger: discardLogger()})
	c.state.pendingCycleID = "cyc-test"
	c.state.pendingMaxToolTurns = 10

	reply := make(chan requestMoreTurnsResult, 1)
	c.onRequestMoreTurns(requestMoreTurnsMsg{
		additional:    5,
		reason:        "diagnostic walk",
		parentCycleID: "cyc-test",
		reply:         reply,
	})
	r := <-reply
	if r.Err != nil {
		t.Fatalf("unexpected error: %v", r.Err)
	}
	if r.NewCap != 15 {
		t.Errorf("NewCap = %d, want 15", r.NewCap)
	}
	if c.state.pendingMaxToolTurns != 15 {
		t.Errorf("state.pendingMaxToolTurns = %d, want 15", c.state.pendingMaxToolTurns)
	}
}

func TestNew_DefaultMaxToolTurnsIsTen(t *testing.T) {
	c := New(Config{Provider: &fakeProvider{reply: "ok"}, Model: "fake", Logger: discardLogger()})
	if c.cfg.MaxToolTurns != defaultMaxToolTurns {
		t.Errorf("default MaxToolTurns = %d, want %d", c.cfg.MaxToolTurns, defaultMaxToolTurns)
	}
	if defaultMaxToolTurns != 10 {
		t.Errorf("defaultMaxToolTurns = %d, want 10 (this test pins the bump)", defaultMaxToolTurns)
	}
}

func TestHardMaxToolTurns_PinnedAt20(t *testing.T) {
	if hardMaxToolTurns != 20 {
		t.Errorf("hardMaxToolTurns = %d, want 20", hardMaxToolTurns)
	}
}

// ---- Autonomous-cycle trace events ----

func TestCog_AutonomousCycleEmitsTrace(t *testing.T) {
	c := New(Config{
		Provider: &fakeProvider{reply: "scheduled work done"},
		Model:    "fake",
		Logger:   discardLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	traceCh, cancelSub := c.SubscribeTraces(4)
	defer cancelSub()

	r := <-c.SubmitWithSource(ctx, "send weekly summary", policy.SourceAutonomous)
	if r.Err != nil {
		t.Fatal(r.Err)
	}

	select {
	case got := <-traceCh:
		if got.Body != "scheduled work done" {
			t.Errorf("trace body = %q, want %q", got.Body, "scheduled work done")
		}
		if got.CycleID == "" {
			t.Error("trace CycleID empty")
		}
		if got.Source != "autonomous" {
			t.Errorf("trace source = %q, want autonomous", got.Source)
		}
	case <-time.After(time.Second):
		t.Fatal("no trace event after autonomous cycle complete")
	}
}

func TestCog_InteractiveCycleDoesNotEmitTrace(t *testing.T) {
	c := New(Config{
		Provider: &fakeProvider{reply: "ok"},
		Model:    "fake",
		Logger:   discardLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	traceCh, cancelSub := c.SubscribeTraces(4)
	defer cancelSub()

	r := <-c.Submit(ctx, "interactive prompt")
	if r.Err != nil {
		t.Fatal(r.Err)
	}

	select {
	case got := <-traceCh:
		t.Errorf("interactive cycle should NOT emit trace; got %+v", got)
	case <-time.After(150 * time.Millisecond):
		// Pass — silence is correct.
	}
}

// ---- max_context_messages cap ----

func TestCog_MaxContextMessagesTruncates(t *testing.T) {
	f := &fakeProvider{reply: "ok"}
	// Cap at 4 messages → after a few exchanges, the oldest get
	// dropped while role alternation is preserved.
	c := New(Config{
		Provider:           f,
		Model:              "fake",
		Logger:             discardLogger(),
		MaxContextMessages: 4,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	for i := 0; i < 5; i++ {
		if r := <-c.Submit(ctx, "msg"); r.Err != nil {
			t.Fatal(r.Err)
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 5 {
		t.Fatalf("requests = %d, want 5", len(f.requests))
	}
	// The 5th request's history should be capped: prior 4 messages
	// max + the new user input. Without the cap it would have been
	// 9 messages (4 user + 4 assistant + 1 new user).
	last := f.requests[4]
	if len(last.Messages) > 5 {
		t.Errorf("last request msg count = %d, expected ≤ 5 with cap=4 + 1 new", len(last.Messages))
	}
	if last.Messages[0].Role != llm.RoleUser {
		t.Errorf("truncated history must start with user-role; got %v", last.Messages[0].Role)
	}
}
