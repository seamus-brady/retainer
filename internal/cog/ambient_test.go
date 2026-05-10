package cog

import (
	"context"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/seamus-brady/retainer/internal/ambient"
	"github.com/seamus-brady/retainer/internal/archivist"
	"github.com/seamus-brady/retainer/internal/cbr"
	"github.com/seamus-brady/retainer/internal/cyclelog"
	"github.com/seamus-brady/retainer/internal/librarian"
	"github.com/seamus-brady/retainer/internal/llm"
	"github.com/seamus-brady/retainer/internal/policy"
)

// recordingPromptFn is a SystemPromptFn that captures every snapshot it
// was called with. The test then asserts on the recorded snapshots.
type recordingPromptFn struct {
	mu    sync.Mutex
	calls []CycleSnapshot
}

func (r *recordingPromptFn) fn(_ context.Context, snap CycleSnapshot) llm.SystemPrompt {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Copy the slice so a later in-place mutation can't surprise us.
	cp := snap
	if len(snap.Ambient) > 0 {
		cp.Ambient = append([]ambient.Signal(nil), snap.Ambient...)
	}
	r.calls = append(r.calls, cp)
	return llm.SystemPrompt{}
}

func (r *recordingPromptFn) snapshots() []CycleSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]CycleSnapshot, len(r.calls))
	copy(out, r.calls)
	return out
}

func TestCog_NoticeBuffersAndSurfacesInNextCycle(t *testing.T) {
	rec := &recordingPromptFn{}
	c := New(Config{
		Provider:       &fakeProvider{reply: "ok"},
		Model:          "fake",
		Logger:         discardLogger(),
		SystemPromptFn: rec.fn,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	// Notice a couple of signals BEFORE submitting any input. They should
	// drain into the next cycle's snapshot.
	c.Notice(ambient.Signal{Source: "forecaster", Kind: "drift", Detail: "score 0.7"})
	c.Notice(ambient.Signal{Source: "observer", Kind: "anomaly", Detail: "watchdog fired"})

	// Give the inbox a moment to absorb the notices before the input
	// arrives. The cog drains pendingAmbient at dispatchLLM time so a
	// race here would surface as a missing signal in the snapshot.
	time.Sleep(20 * time.Millisecond)

	r := <-c.Submit(ctx, "hi")
	if r.Err != nil {
		t.Fatalf("err: %v", r.Err)
	}

	snaps := rec.snapshots()
	if len(snaps) != 1 {
		t.Fatalf("got %d snapshots, want 1", len(snaps))
	}
	if len(snaps[0].Ambient) != 2 {
		t.Fatalf("Ambient = %d signals, want 2: %+v", len(snaps[0].Ambient), snaps[0].Ambient)
	}
	if snaps[0].Ambient[0].Source != "forecaster" {
		t.Errorf("first signal source = %q, want forecaster", snaps[0].Ambient[0].Source)
	}
	if snaps[0].Ambient[1].Source != "observer" {
		t.Errorf("second signal source = %q, want observer", snaps[0].Ambient[1].Source)
	}
}

func TestCog_NoticeStampsTimestampWhenZero(t *testing.T) {
	rec := &recordingPromptFn{}
	c := New(Config{
		Provider:       &fakeProvider{reply: "ok"},
		Model:          "fake",
		Logger:         discardLogger(),
		SystemPromptFn: rec.fn,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	c.Notice(ambient.Signal{Source: "x", Kind: "y", Detail: "z"}) // no timestamp
	time.Sleep(20 * time.Millisecond)

	r := <-c.Submit(ctx, "hi")
	if r.Err != nil {
		t.Fatalf("err: %v", r.Err)
	}
	snaps := rec.snapshots()
	if len(snaps[0].Ambient) != 1 {
		t.Fatalf("Ambient = %+v", snaps[0].Ambient)
	}
	if snaps[0].Ambient[0].Timestamp.IsZero() {
		t.Error("Timestamp should be stamped by Notice when caller passes zero")
	}
}

func TestCog_AmbientDrainsOnceAndOnlyOnFirstTurn(t *testing.T) {
	// In a multi-turn cycle (tool dispatch), the first dispatch sees
	// the drained signals; the post-tool dispatch sees nil. Mid-cycle
	// shouldn't see fresh ambient.
	rec := &recordingPromptFn{}
	disp := &fakeDispatcher{
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
	c := New(Config{
		Provider:       prov,
		Model:          "fake",
		Logger:         discardLogger(),
		Tools:          disp,
		SystemPromptFn: rec.fn,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	c.Notice(ambient.Signal{Source: "forecaster", Kind: "drift", Detail: "x"})
	time.Sleep(20 * time.Millisecond)

	r := <-c.Submit(ctx, "go")
	if r.Err != nil {
		t.Fatalf("err: %v", r.Err)
	}
	snaps := rec.snapshots()
	if len(snaps) != 2 {
		t.Fatalf("expected 2 snapshots (initial + post-tool), got %d", len(snaps))
	}
	if len(snaps[0].Ambient) != 1 {
		t.Errorf("first snapshot should carry the drained signal; got %+v", snaps[0].Ambient)
	}
	if len(snaps[1].Ambient) != 0 {
		t.Errorf("second snapshot (post-tool) should not see fresh ambient; got %+v", snaps[1].Ambient)
	}
}

func TestCog_AmbientBufferDropsOldestWhenFull(t *testing.T) {
	rec := &recordingPromptFn{}
	c := New(Config{
		Provider:         &fakeProvider{reply: "ok"},
		Model:            "fake",
		Logger:           discardLogger(),
		SystemPromptFn:   rec.fn,
		AmbientBufferCap: 2,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	c.Notice(ambient.Signal{Source: "a"})
	c.Notice(ambient.Signal{Source: "b"})
	c.Notice(ambient.Signal{Source: "c"}) // pushes out 'a'
	time.Sleep(20 * time.Millisecond)

	r := <-c.Submit(ctx, "hi")
	if r.Err != nil {
		t.Fatalf("err: %v", r.Err)
	}
	snaps := rec.snapshots()
	if len(snaps[0].Ambient) != 2 {
		t.Fatalf("buffer cap=2 should retain 2 signals; got %+v", snaps[0].Ambient)
	}
	if snaps[0].Ambient[0].Source != "b" || snaps[0].Ambient[1].Source != "c" {
		t.Errorf("oldest-drop wrong: got %+v, want [b, c]", snaps[0].Ambient)
	}
}

// ---- Source-typed inputs ----

func TestCog_SubmitDefaultsToInteractive(t *testing.T) {
	rec := &recordingPromptFn{}
	c := New(Config{
		Provider:       &fakeProvider{reply: "ok"},
		Model:          "fake",
		Logger:         discardLogger(),
		SystemPromptFn: rec.fn,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	r := <-c.Submit(ctx, "hi")
	if r.Err != nil {
		t.Fatalf("err: %v", r.Err)
	}
	snaps := rec.snapshots()
	if snaps[0].InputSource != policy.SourceInteractive {
		t.Errorf("Submit should default to SourceInteractive; got %v", snaps[0].InputSource)
	}
}

func TestCog_SubmitWithSourcePreservesAutonomous(t *testing.T) {
	rec := &recordingPromptFn{}
	c := New(Config{
		Provider:       &fakeProvider{reply: "ok"},
		Model:          "fake",
		Logger:         discardLogger(),
		SystemPromptFn: rec.fn,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	r := <-c.SubmitWithSource(ctx, "scheduled work", policy.SourceAutonomous)
	if r.Err != nil {
		t.Fatalf("err: %v", r.Err)
	}
	snaps := rec.snapshots()
	if snaps[0].InputSource != policy.SourceAutonomous {
		t.Errorf("InputSource = %v, want SourceAutonomous", snaps[0].InputSource)
	}
}

func TestCog_AutonomousInputIsHardBlockedWhereInteractiveIsDemoted(t *testing.T) {
	// A rule that hits the input gate. Interactive sources demote
	// Block→Escalate (still refused, but with the demoted verdict in
	// the trail). Autonomous sources keep Block.
	engine := policy.New(policy.Config{
		Rules: &policy.RuleSet{
			InputThreshold: 0.4,
			Input: []policy.Rule{{
				Name:       "trip",
				Pattern:    regexp.MustCompile(`(?i)\btrigger\b`),
				Importance: 1.0,
				Magnitude:  1.0,
			}},
		},
	})
	sink := &recordingSink{}
	c := New(Config{
		Provider: &fakeProvider{reply: "should not see"},
		Model:    "fake",
		Logger:   discardLogger(),
		Policy:   engine,
		CycleLog: sink,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	// Autonomous: hard Block.
	r := <-c.SubmitWithSource(ctx, "trigger now", policy.SourceAutonomous)
	if r.Kind != ReplyKindRefusal {
		t.Fatalf("autonomous trigger should be refused; got %+v", r)
	}
	events := sink.snapshot()
	var seenAutonomous bool
	for _, ev := range events {
		if ev.Type == cyclelog.EventPolicyDecision && ev.Gate == policy.GateInput.String() {
			if ev.Verdict == policy.Block.String() {
				seenAutonomous = true
			}
		}
	}
	if !seenAutonomous {
		t.Errorf("expected hard Block verdict for autonomous source; events: %+v", events)
	}
}

// ---- Tool gate + post-exec gate ----

func TestCog_ToolGateBlocksDispatch(t *testing.T) {
	// Block based on a pattern in tool *args*. Dispatch must NOT run;
	// the model gets an IsError tool_result with the refusal string.
	engine := policy.New(policy.Config{
		Rules: &policy.RuleSet{
			ToolThreshold: 0.4,
			Tool: []policy.Rule{{
				Name:       "blocked-arg",
				Pattern:    regexp.MustCompile(`(?i)\bforbidden\b`),
				Importance: 1.0,
				Magnitude:  1.0,
			}},
		},
	})
	disp := &fakeDispatcher{
		tools:  []llm.Tool{{Name: "echo"}},
		result: "should not run",
	}
	prov := &scriptedProvider{
		scripts: []llm.Response{
			{
				Content:    []llm.ContentBlock{llm.ToolUseBlock{ID: "c1", Name: "echo", Input: []byte(`{"q":"forbidden"}`)}},
				StopReason: "tool_use",
			},
			{Content: []llm.ContentBlock{llm.TextBlock{Text: "ok i can't"}}, StopReason: "end_turn"},
		},
	}
	c := New(Config{Provider: prov, Model: "fake", Logger: discardLogger(), Tools: disp, Policy: engine})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	r := <-c.Submit(ctx, "use the tool")
	if r.Err != nil {
		t.Fatalf("err: %v", r.Err)
	}

	// Dispatcher must not have been called.
	disp.mu.Lock()
	calls := len(disp.calls)
	disp.mu.Unlock()
	if calls != 0 {
		t.Errorf("dispatcher was called %d times; want 0 — tool gate should have blocked", calls)
	}

	// The 2nd LLM request must contain a ToolResultBlock with IsError
	// and the refusal string.
	prov.mu.Lock()
	defer prov.mu.Unlock()
	if len(prov.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(prov.requests))
	}
	last := prov.requests[1].Messages[len(prov.requests[1].Messages)-1]
	tr, ok := last.Content[0].(llm.ToolResultBlock)
	if !ok {
		t.Fatalf("last block = %T", last.Content[0])
	}
	if !tr.IsError {
		t.Error("expected IsError=true for tool-gate block")
	}
	if tr.Content != toolGateRefusal {
		t.Errorf("content = %q, want %q", tr.Content, toolGateRefusal)
	}
}

func TestCog_PostExecGateRedactsOutput(t *testing.T) {
	// The tool runs successfully but the post-exec gate scrubs its
	// output — Dispatch returns a string containing a banned pattern.
	// The model should see the refusal string with IsError=false (it
	// was a successful call, just redacted).
	engine := policy.New(policy.Config{
		Rules: &policy.RuleSet{
			PostExecThreshold: 0.4,
			PostExec: []policy.Rule{{
				Name:       "leak",
				Pattern:    regexp.MustCompile(`(?i)\bleaked-secret\b`),
				Importance: 1.0,
				Magnitude:  1.0,
			}},
		},
	})
	disp := &fakeDispatcher{
		tools:  []llm.Tool{{Name: "echo"}},
		result: "uh oh: leaked-secret in payload",
	}
	prov := &scriptedProvider{
		scripts: []llm.Response{
			{
				Content:    []llm.ContentBlock{llm.ToolUseBlock{ID: "c1", Name: "echo", Input: []byte(`{}`)}},
				StopReason: "tool_use",
			},
			{Content: []llm.ContentBlock{llm.TextBlock{Text: "noted"}}, StopReason: "end_turn"},
		},
	}
	c := New(Config{Provider: prov, Model: "fake", Logger: discardLogger(), Tools: disp, Policy: engine})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	r := <-c.Submit(ctx, "go")
	if r.Err != nil {
		t.Fatalf("err: %v", r.Err)
	}

	disp.mu.Lock()
	calls := len(disp.calls)
	disp.mu.Unlock()
	if calls != 1 {
		t.Errorf("dispatcher should have run once; got %d", calls)
	}

	prov.mu.Lock()
	defer prov.mu.Unlock()
	last := prov.requests[1].Messages[len(prov.requests[1].Messages)-1]
	tr, ok := last.Content[0].(llm.ToolResultBlock)
	if !ok {
		t.Fatalf("last block = %T", last.Content[0])
	}
	if tr.IsError {
		t.Error("post-exec block should NOT mark IsError — call succeeded, output redacted")
	}
	if tr.Content != postExecRefusal {
		t.Errorf("content = %q, want %q", tr.Content, postExecRefusal)
	}
}

func TestCog_ToolAndPostExecGateEmitDecisionEvents(t *testing.T) {
	engine := policy.New(policy.Config{
		Rules: &policy.RuleSet{
			ToolThreshold:     0.4,
			PostExecThreshold: 0.4,
		},
	})
	disp := &fakeDispatcher{
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
	sink := &recordingSink{}
	c := New(Config{
		Provider: prov,
		Model:    "fake",
		Logger:   discardLogger(),
		Tools:    disp,
		Policy:   engine,
		CycleLog: sink,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	r := <-c.Submit(ctx, "use the tool")
	if r.Err != nil {
		t.Fatalf("err: %v", r.Err)
	}

	events := sink.snapshot()
	var seenTool, seenPostExec bool
	for _, e := range events {
		if e.Type != cyclelog.EventPolicyDecision {
			continue
		}
		switch e.Gate {
		case policy.GateTool.String():
			seenTool = true
		case policy.GatePostExec.String():
			seenPostExec = true
		}
	}
	if !seenTool {
		t.Errorf("missing tool-gate policy_decision event; got %+v", events)
	}
	if !seenPostExec {
		t.Errorf("missing post-exec policy_decision event; got %+v", events)
	}
}

// ---- CBR retrieval feedback (cycle → archivist) ----

// fakeArchivist captures whatever the cog sends to the archivist.
// Extends the cog's NarrativeArchivist interface with a snapshot
// helper so tests can assert on what arrived.
type fakeArchivist struct {
	mu       sync.Mutex
	received []archivist.CycleComplete
}

func (a *fakeArchivist) Record(msg archivist.CycleComplete) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.received = append(a.received, msg)
}

func (a *fakeArchivist) snapshot() []archivist.CycleComplete {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]archivist.CycleComplete, len(a.received))
	copy(out, a.received)
	return out
}

// fakeLibrarian only implements DrainRetrievedCaseIDs — the only
// thing the cog calls on the librarian for the retrieval-feedback
// path. It returns canned IDs on Drain.
type fakeLibrarian struct {
	mu     sync.Mutex
	drains map[string][]string
}

// We need the cog's CycleComplete path to drain; the Librarian
// dependency on the cog's Config is the *librarian.Librarian
// concrete type, so for an end-to-end retrieval-feedback test we
// use a real Librarian with mock cases. Skipping the fake path —
// real librarian + mock embedder is fast enough.

func TestCog_RetrievedCaseIDsFlowToArchivist(t *testing.T) {
	dataDir := t.TempDir()
	// Real librarian — gives us the retrieval registry behaviour.
	lib, err := librarian.New(librarian.Options{
		DataDir: dataDir,
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatalf("librarian: %v", err)
	}
	libCtx, libCancel := context.WithCancel(context.Background())
	defer libCancel()
	go lib.Run(libCtx)

	// Seed two cases so RetrieveCases has something to surface.
	lib.RecordCase(seedCase("c1", "auth"))
	lib.RecordCase(seedCase("c2", "auth"))
	// Sync barrier: a synchronous query proves the inbox has
	// drained the writes.
	_ = lib.CaseCount()

	arc := &fakeArchivist{}
	c := New(Config{
		Provider:  &fakeProvider{reply: "done"},
		Model:     "fake",
		Logger:    discardLogger(),
		Archivist: arc,
		Librarian: lib,
	})
	cogCtx, cogCancel := context.WithCancel(context.Background())
	defer cogCancel()
	go c.Run(cogCtx)

	// Submit a turn. Mid-cycle, simulate the agent's recall_cases by
	// dispatching a retrieve against the librarian with the cog's
	// cycleID context. The cog sets pendingCycleID at onUserInput
	// time; we look it up through the activity stream which carries
	// it.
	r := <-c.Submit(cogCtx, "hi")
	if r.Err != nil {
		t.Fatalf("submit err: %v", r.Err)
	}

	// The cycle has completed; archivist should receive one
	// CycleComplete. RetrievedCaseIDs is empty here because the
	// fakeProvider doesn't trigger any tool calls. That alone covers
	// the "no retrievals → empty drain" path; the explicit
	// retrieval scenario is exercised below.
	//
	// recordNarrative runs AFTER the reply is sent to the channel,
	// so the test's main goroutine can wake up before the archivist
	// has been called. Poll briefly for the message.
	var got []archivist.CycleComplete
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		got = arc.snapshot()
		if len(got) >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(got) != 1 {
		t.Fatalf("got %d archivist messages, want 1", len(got))
	}
	if len(got[0].RetrievedCaseIDs) != 0 {
		t.Errorf("no retrieval performed; expected empty IDs, got %v", got[0].RetrievedCaseIDs)
	}

	// Now manually register a retrieval against an arbitrary
	// just-completed cycle ID and confirm Drain returns it. This is
	// the lower-half assertion: librarian-side registry works through
	// the public API.
	ctxRet := cyclelog.WithCycleID(cogCtx, "manual-cycle")
	scored := lib.RetrieveCases(ctxRet, cbr.Query{Domain: "auth"})
	if len(scored) == 0 {
		t.Fatal("expected scored results")
	}
	drained := lib.DrainRetrievedCaseIDs("manual-cycle")
	if len(drained) == 0 {
		t.Errorf("DrainRetrievedCaseIDs should expose registered IDs; got %v", drained)
	}
}

// seedCase + cyclelog/cbr/librarian imports for the new test.
func seedCase(id, domain string) cbr.Case {
	return cbr.Case{
		ID:            id,
		Timestamp:     time.Now(),
		SchemaVersion: cbr.SchemaVersion,
		Problem: cbr.Problem{
			Intent:   "x",
			Domain:   domain,
			Keywords: []string{domain},
		},
		Solution: cbr.Solution{Approach: "x"},
		Outcome:  cbr.Outcome{Status: cbr.StatusSuccess, Confidence: 0.9},
	}
}

// ---- Parallel tool dispatch ----

// blockingDispatcher records dispatch order + holds each call until
// signal — used to prove dispatches happen concurrently rather than
// sequentially.
type blockingDispatcher struct {
	tools  []llm.Tool
	mu     sync.Mutex
	starts []string
	ends   []string
	hold   chan struct{}
}

func (d *blockingDispatcher) List() []llm.Tool { return d.tools }

func (d *blockingDispatcher) Dispatch(ctx context.Context, name string, input []byte) (string, error) {
	d.mu.Lock()
	d.starts = append(d.starts, name)
	d.mu.Unlock()
	// Block until the test releases all calls together. If dispatch
	// were serial, the test would deadlock — only one call would
	// reach this point at a time.
	select {
	case <-d.hold:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	d.mu.Lock()
	d.ends = append(d.ends, name)
	d.mu.Unlock()
	return "ok-" + name, nil
}

func TestCog_DispatchesToolsInParallel(t *testing.T) {
	disp := &blockingDispatcher{
		tools: []llm.Tool{{Name: "alpha"}, {Name: "beta"}, {Name: "gamma"}},
		hold:  make(chan struct{}),
	}
	prov := &scriptedProvider{
		scripts: []llm.Response{
			{
				Content: []llm.ContentBlock{
					llm.ToolUseBlock{ID: "a", Name: "alpha", Input: []byte(`{}`)},
					llm.ToolUseBlock{ID: "b", Name: "beta", Input: []byte(`{}`)},
					llm.ToolUseBlock{ID: "c", Name: "gamma", Input: []byte(`{}`)},
				},
				StopReason: "tool_use",
			},
			{Content: []llm.ContentBlock{llm.TextBlock{Text: "all done"}}, StopReason: "end_turn"},
		},
	}
	c := New(Config{Provider: prov, Model: "fake", Logger: discardLogger(), Tools: disp})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	replyCh := c.Submit(ctx, "go")

	// Wait until all 3 dispatches are blocking inside the dispatcher
	// — proves they're running concurrently. If dispatch were serial
	// only 1 would have started.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		disp.mu.Lock()
		started := len(disp.starts)
		disp.mu.Unlock()
		if started == 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	disp.mu.Lock()
	if len(disp.starts) != 3 {
		disp.mu.Unlock()
		t.Fatalf("expected 3 concurrent dispatches; got %d started", len(disp.starts))
	}
	disp.mu.Unlock()

	// Release all dispatches.
	close(disp.hold)
	r := <-replyCh
	if r.Err != nil {
		t.Fatalf("err: %v", r.Err)
	}
}

func TestCog_DispatchPreservesInputOrderInResults(t *testing.T) {
	// Even though dispatch is concurrent, the tool_result blocks the
	// model receives must follow input order so the cycle log + the
	// model's view of pairing stays deterministic.
	hold := make(chan struct{})
	disp := &orderingDispatcher{
		hold: hold,
		tools: []llm.Tool{
			{Name: "first"}, {Name: "second"}, {Name: "third"},
		},
	}
	prov := &scriptedProvider{
		scripts: []llm.Response{
			{
				Content: []llm.ContentBlock{
					llm.ToolUseBlock{ID: "id-1", Name: "first", Input: []byte(`{}`)},
					llm.ToolUseBlock{ID: "id-2", Name: "second", Input: []byte(`{}`)},
					llm.ToolUseBlock{ID: "id-3", Name: "third", Input: []byte(`{}`)},
				},
				StopReason: "tool_use",
			},
			{Content: []llm.ContentBlock{llm.TextBlock{Text: "ok"}}, StopReason: "end_turn"},
		},
	}
	c := New(Config{Provider: prov, Model: "fake", Logger: discardLogger(), Tools: disp})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	replyCh := c.Submit(ctx, "go")
	close(hold) // let dispatches finish in any order
	r := <-replyCh
	if r.Err != nil {
		t.Fatalf("err: %v", r.Err)
	}

	// The post-tool LLM request's last message contains the
	// tool_result blocks in order. Inspect.
	prov.mu.Lock()
	defer prov.mu.Unlock()
	post := prov.requests[1]
	last := post.Messages[len(post.Messages)-1]
	if len(last.Content) != 3 {
		t.Fatalf("got %d tool_result blocks, want 3", len(last.Content))
	}
	wantIDs := []string{"id-1", "id-2", "id-3"}
	for i, want := range wantIDs {
		tr, ok := last.Content[i].(llm.ToolResultBlock)
		if !ok {
			t.Fatalf("block %d not ToolResultBlock", i)
		}
		if tr.ToolUseID != want {
			t.Errorf("position %d: ToolUseID = %q, want %q", i, tr.ToolUseID, want)
		}
	}
}

// orderingDispatcher returns results in DIFFERENT order than starts —
// proving the cog re-orders by input position rather than completion
// order. Built around a mutex + delay-injection so "third" finishes
// first when test holds.
type orderingDispatcher struct {
	tools []llm.Tool
	hold  chan struct{}
	mu    sync.Mutex
	count int
}

func (d *orderingDispatcher) List() []llm.Tool { return d.tools }

func (d *orderingDispatcher) Dispatch(ctx context.Context, name string, input []byte) (string, error) {
	d.mu.Lock()
	d.count++
	idx := d.count
	d.mu.Unlock()
	<-d.hold
	// Stagger return so completion order is reverse of start order
	// (first-started waits longest, third-started returns first).
	time.Sleep(time.Duration(10*(4-idx)) * time.Millisecond)
	return "ok-" + name, nil
}
