package cyclelog

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCycleIDFromContext_Empty(t *testing.T) {
	if got := CycleIDFromContext(context.Background()); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestCycleIDFromContext_RoundTrip(t *testing.T) {
	ctx := WithCycleID(context.Background(), "cyc-1")
	if got := CycleIDFromContext(ctx); got != "cyc-1" {
		t.Errorf("got %q, want 'cyc-1'", got)
	}
}

func TestCycleIDFromContext_OverridesEarlierValue(t *testing.T) {
	// Nesting WithCycleID should produce a context where the most-
	// recent value wins. Useful when sub-actor work re-wraps the
	// context with its own cycle id.
	ctx := WithCycleID(context.Background(), "outer")
	ctx = WithCycleID(ctx, "inner")
	if got := CycleIDFromContext(ctx); got != "inner" {
		t.Errorf("got %q, want 'inner'", got)
	}
}

func TestCycleIDFromContext_IgnoresWrongType(t *testing.T) {
	// Ensure a manually-built context with the same key (impossible
	// from outside the package since the key is unexported) doesn't
	// confuse the extractor — guard against type pollution from any
	// future additions of context keys.
	ctx := context.WithValue(context.Background(), struct{}{}, "fake")
	if got := CycleIDFromContext(ctx); got != "" {
		t.Errorf("got %q, want empty (unrelated key)", got)
	}
}

func TestWriter_EmitsJSONLine(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	err := w.Emit(Event{
		Type:    EventCycleStart,
		CycleID: "abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("missing newline: %q", got)
	}
	var decoded Event
	if err := json.Unmarshal(bytes.TrimSpace([]byte(got)), &decoded); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if decoded.Type != EventCycleStart || decoded.CycleID != "abc" {
		t.Fatalf("decoded mismatch: %+v", decoded)
	}
	if decoded.Timestamp.IsZero() {
		t.Fatalf("timestamp not auto-set")
	}
}

func TestWriter_OmitsUnsetFields(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.Emit(Event{Type: EventCycleStart, CycleID: "x", Timestamp: time.Unix(1, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, k := range []string{"model", "stop_reason", "error", "parent_id"} {
		if strings.Contains(got, `"`+k+`":`) {
			t.Errorf("expected %q omitted, got: %s", k, got)
		}
	}
}

func TestWriter_ConcurrentEmit(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	var wg sync.WaitGroup
	const n = 50
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = w.Emit(Event{Type: EventCycleStart, CycleID: "x"})
		}()
	}
	wg.Wait()
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != n {
		t.Fatalf("got %d lines, want %d", len(lines), n)
	}
	for i, line := range lines {
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Errorf("line %d: invalid json: %v", i, err)
		}
	}
}

func TestNopSink_DropsEvents(t *testing.T) {
	if err := NopSink().Emit(Event{}); err != nil {
		t.Fatalf("nop sink errored: %v", err)
	}
}

// ---- Emitter ----

func TestEmitter_StampsInstanceID(t *testing.T) {
	var got Event
	sink := capturingSink{capture: func(e Event) { got = e }}
	em := NewEmitter(sink, "abc12345")
	if err := em.Emit(Event{Type: EventCycleStart, CycleID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if got.InstanceID != "abc12345" {
		t.Errorf("InstanceID = %q, want abc12345", got.InstanceID)
	}
}

func TestEmitter_RespectsCallerSuppliedInstanceID(t *testing.T) {
	var got Event
	sink := capturingSink{capture: func(e Event) { got = e }}
	em := NewEmitter(sink, "abc12345")
	if err := em.Emit(Event{Type: EventCycleStart, CycleID: "c1", InstanceID: "override"}); err != nil {
		t.Fatal(err)
	}
	if got.InstanceID != "override" {
		t.Errorf("caller-supplied InstanceID should win; got %q", got.InstanceID)
	}
}

func TestEmitter_EmptyInstanceIDLeavesEmpty(t *testing.T) {
	var got Event
	sink := capturingSink{capture: func(e Event) { got = e }}
	em := NewEmitter(sink, "")
	if err := em.Emit(Event{Type: EventCycleStart, CycleID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if got.InstanceID != "" {
		t.Errorf("empty emitter should not stamp; got %q", got.InstanceID)
	}
}

func TestEmitter_NilSinkSilentlyDrops(t *testing.T) {
	em := NewEmitter(nil, "abc")
	if err := em.Emit(Event{Type: EventCycleStart, CycleID: "c1"}); err != nil {
		t.Errorf("nil sink should be a no-op; got err %v", err)
	}
}

func TestEmitter_ImplementsSink(t *testing.T) {
	var _ Sink = (*Emitter)(nil)
}

// capturingSink lets tests inspect the event they emitted.
type capturingSink struct {
	capture func(Event)
}

func (c capturingSink) Emit(e Event) error {
	c.capture(e)
	return nil
}

// ---- New event types ----

func TestNewEventTypes_Defined(t *testing.T) {
	for _, et := range []EventType{
		EventAgentCycleStart,
		EventAgentCycleComplete,
		EventToolCall,
		EventToolResult,
	} {
		if et == "" {
			t.Errorf("event type undefined")
		}
	}
}

// ---- New event-shape fields round-trip ----

func TestEvent_AgentFields_RoundTrip(t *testing.T) {
	original := Event{
		Type:         EventToolCall,
		CycleID:      "task-1",
		InstanceID:   "abc12345",
		ParentID:     "agent-cycle-1",
		ToolName:     "memory_write",
		ToolInputLen: 42,
	}
	body, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var got Event
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != EventToolCall || got.InstanceID != "abc12345" || got.ToolName != "memory_write" || got.ToolInputLen != 42 {
		t.Errorf("round trip lost fields: %+v", got)
	}
}
