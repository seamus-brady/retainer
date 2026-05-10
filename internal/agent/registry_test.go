package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/seamus-brady/retainer/internal/llm"
)

func newTestAgent(t *testing.T, name string, scripts []llm.Response) *Agent {
	t.Helper()
	prov := &scriptedProvider{scripts: scripts}
	disp := &fakeDispatcher{tools: []llm.Tool{{Name: "echo"}}, res: "ok"}
	a, err := New(Spec{
		Name:        name,
		HumanName:   strings.Title(name),
		Description: "test agent",
		Provider:    prov,
		Tools:       disp,
	}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := NewRegistry()
	a := newTestAgent(t, "researcher", nil)
	if err := r.Register(a); err != nil {
		t.Fatalf("register: %v", err)
	}
	if got := r.Get("researcher"); got != a {
		t.Errorf("Get returned %v, want the registered agent", got)
	}
	if got := r.Get("unknown"); got != nil {
		t.Errorf("Get(unknown) = %v, want nil", got)
	}
}

func TestRegistry_RejectsDuplicates(t *testing.T) {
	r := NewRegistry()
	a := newTestAgent(t, "researcher", nil)
	if err := r.Register(a); err != nil {
		t.Fatal(err)
	}
	b := newTestAgent(t, "researcher", nil)
	err := r.Register(b)
	if err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("err = %v", err)
	}
}

func TestRegistry_RejectsNil(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(nil); err == nil {
		t.Fatal("expected nil-agent error")
	}
}

func TestRegistry_MustRegisterPanicsOnDuplicate(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(newTestAgent(t, "x", nil))
	defer func() {
		if rec := recover(); rec == nil {
			t.Fatal("expected panic on duplicate")
		}
	}()
	r.MustRegister(newTestAgent(t, "x", nil))
}

func TestRegistry_NamesAndList_Sorted(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(newTestAgent(t, "writer", nil))
	r.MustRegister(newTestAgent(t, "researcher", nil))
	r.MustRegister(newTestAgent(t, "comms", nil))

	names := r.Names()
	want := []string{"comms", "researcher", "writer"}
	for i, n := range want {
		if names[i] != n {
			t.Errorf("names[%d] = %q, want %q", i, names[i], n)
		}
	}
	list := r.List()
	if len(list) != 3 {
		t.Fatalf("list len = %d", len(list))
	}
	for i, a := range list {
		if a.Name() != want[i] {
			t.Errorf("list[%d].Name = %q, want %q", i, a.Name(), want[i])
		}
	}
}

// ---- Dispatch ----

func TestRegistry_DispatchHappyPath(t *testing.T) {
	a := newTestAgent(t, "researcher", []llm.Response{
		{Content: []llm.ContentBlock{llm.TextBlock{Text: "found it"}}, StopReason: "end_turn"},
	})
	r := NewRegistry()
	r.MustRegister(a)
	runAgent(t, a)

	out, err := r.Dispatch(context.Background(), "researcher", "find X", "cog-cyc-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !out.IsSuccess() {
		t.Fatalf("agent err: %v", out.Err)
	}
	if out.Result != "found it" {
		t.Errorf("result = %q", out.Result)
	}
	if out.AgentName != "researcher" {
		t.Errorf("agent_name = %q", out.AgentName)
	}
}

func TestRegistry_DispatchUnknownAgent(t *testing.T) {
	r := NewRegistry()
	_, err := r.Dispatch(context.Background(), "nope", "x", "")
	if err == nil || !strings.Contains(err.Error(), "no agent registered") {
		t.Fatalf("err = %v", err)
	}
}

func TestRegistry_DispatchPropagatesAgentFailure(t *testing.T) {
	// Agent returns an Outcome with Err set. Dispatch should still
	// return without an error — the failure is in Outcome.Err, which
	// the caller surfaces to the LLM.
	a := newTestAgent(t, "x", []llm.Response{
		{Content: []llm.ContentBlock{llm.ToolUseBlock{ID: "c", Name: "echo", Input: []byte(`{}`)}}, StopReason: "tool_use"},
	})
	// Provider only has 1 script; max_turns will exhaust.
	a.spec.MaxTurns = 1
	r := NewRegistry()
	r.MustRegister(a)
	runAgent(t, a)

	out, err := r.Dispatch(context.Background(), "x", "x", "")
	if err != nil {
		t.Fatalf("dispatch err: %v", err)
	}
	if out.IsSuccess() {
		t.Errorf("expected agent failure")
	}
}

func TestRegistry_DispatchRespectsCancelledCtx(t *testing.T) {
	// Submit a task to an unstarted agent (no Run goroutine), then
	// cancel ctx; Dispatch should return ctx.Err.
	a := newTestAgent(t, "x", nil)
	r := NewRegistry()
	r.MustRegister(a)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.Dispatch(ctx, "x", "x", "")
	if err == nil {
		t.Fatal("expected ctx error")
	}
}
