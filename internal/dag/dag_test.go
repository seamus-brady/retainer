package dag

import (
	"context"
	"testing"
	"time"
)

func runDAG(t *testing.T) *DAG {
	t.Helper()
	d := New()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go d.Run(ctx)
	return d
}

func TestDAG_StartAndGet(t *testing.T) {
	d := runDAG(t)
	d.StartCycle("c1", "", NodeCognitive)

	got := d.Get("c1")
	if got == nil {
		t.Fatal("got nil; expected node")
	}
	if got.ID != "c1" || got.Type != NodeCognitive || got.Status != StatusInProgress {
		t.Fatalf("unexpected node: %+v", got)
	}
	if got.StartedAt.IsZero() {
		t.Fatal("StartedAt should be set")
	}
	if !got.CompletedAt.IsZero() {
		t.Fatal("CompletedAt should be zero before completion")
	}
}

func TestDAG_Complete(t *testing.T) {
	d := runDAG(t)
	d.StartCycle("c1", "", NodeCognitive)
	d.CompleteCycle("c1", StatusComplete, "")

	got := d.Get("c1")
	if got == nil {
		t.Fatal("nil node")
	}
	if got.Status != StatusComplete {
		t.Fatalf("status = %q, want complete", got.Status)
	}
	if got.CompletedAt.IsZero() {
		t.Fatal("CompletedAt should be set after completion")
	}
}

func TestDAG_GetMissingReturnsNil(t *testing.T) {
	d := runDAG(t)
	if got := d.Get("does-not-exist"); got != nil {
		t.Fatalf("got %+v, want nil", got)
	}
}

func TestDAG_CompleteUnknownIsNoop(t *testing.T) {
	d := runDAG(t)
	// Out-of-order delivery: complete arrives for a cycle never started.
	d.CompleteCycle("ghost", StatusComplete, "")
	if got := d.Get("ghost"); got != nil {
		t.Fatalf("ghost got recorded: %+v", got)
	}
}

func TestDAG_CompleteRecordsErrorMessage(t *testing.T) {
	d := runDAG(t)
	d.StartCycle("c1", "", NodeCognitive)
	d.CompleteCycle("c1", StatusError, "policy block: jailbreak_marker matched")

	got := d.Get("c1")
	if got == nil {
		t.Fatal("nil node")
	}
	if got.Status != StatusError {
		t.Fatalf("status = %q", got.Status)
	}
	if got.ErrorMessage != "policy block: jailbreak_marker matched" {
		t.Fatalf("ErrorMessage = %q", got.ErrorMessage)
	}
}

func TestDAG_Children(t *testing.T) {
	d := runDAG(t)
	d.StartCycle("parent", "", NodeCognitive)
	d.StartCycle("child1", "parent", NodeAgent)
	d.StartCycle("child2", "parent", NodeAgent)
	d.StartCycle("orphan", "", NodeCognitive)

	kids := d.Children("parent")
	if len(kids) != 2 {
		t.Fatalf("got %d children, want 2: %+v", len(kids), kids)
	}
	seen := map[CycleID]bool{}
	for _, k := range kids {
		seen[k.ID] = true
	}
	if !seen["child1"] || !seen["child2"] {
		t.Fatalf("missing expected children: %+v", kids)
	}
}

func TestDAG_SnapshotDoesNotAlias(t *testing.T) {
	d := runDAG(t)
	d.StartCycle("c1", "", NodeCognitive)
	first := d.Get("c1")
	first.Status = StatusError // mutate the snapshot

	second := d.Get("c1")
	if second.Status != StatusInProgress {
		t.Fatalf("snapshot mutation leaked into actor state: %q", second.Status)
	}
}

func TestDAG_RunHonorsContextCancel(t *testing.T) {
	d := New()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err == nil || err.Error() != context.Canceled.Error() {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
