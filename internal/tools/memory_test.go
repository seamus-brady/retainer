package tools

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seamus-brady/retainer/internal/cyclelog"
	"github.com/seamus-brady/retainer/internal/librarian"
)

// fakeLib implements LibrarianFactStore for handler tests.
type fakeLib struct {
	mu          sync.Mutex
	written     []librarian.Fact
	cleared     []clearCall
	get         *librarian.Fact
	searchHit   []librarian.Fact
	lastSearch  string
	lastSearchN int
}

type clearCall struct {
	key, cycleID string
}

func (f *fakeLib) RecordFact(fact librarian.Fact) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written = append(f.written, fact)
}

func (f *fakeLib) GetFact(key string) *librarian.Fact {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.get != nil && f.get.Key == key {
		// return a copy
		fact := *f.get
		return &fact
	}
	return nil
}

func (f *fakeLib) ClearFact(key, cycleID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared = append(f.cleared, clearCall{key: key, cycleID: cycleID})
}

func (f *fakeLib) SearchFacts(keyword string, limit int) []librarian.Fact {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastSearch = keyword
	f.lastSearchN = limit
	return f.searchHit
}

// ---- memory_write ----

func TestMemoryWrite_HappyPath(t *testing.T) {
	lib := &fakeLib{}
	h := MemoryWrite{Lib: lib}
	ctx := cyclelog.WithCycleID(context.Background(), "cyc-1")
	out, err := h.Execute(ctx, []byte(`{"key":"user_name","value":"Seamus","scope":"persistent","confidence":0.95}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"user_name"`) || !strings.Contains(out, `"Seamus"`) {
		t.Errorf("output missing fields: %q", out)
	}
	if !strings.Contains(out, "0.95") {
		t.Errorf("output missing confidence: %q", out)
	}
	if len(lib.written) != 1 {
		t.Fatalf("written = %d, want 1", len(lib.written))
	}
	w := lib.written[0]
	if w.Key != "user_name" || w.Value != "Seamus" || w.Scope != librarian.FactScopePersistent {
		t.Errorf("wrote = %+v", w)
	}
	if w.Confidence != 0.95 {
		t.Errorf("confidence = %f", w.Confidence)
	}
	if w.SourceCycleID != "cyc-1" {
		t.Errorf("source_cycle_id = %q, want 'cyc-1'", w.SourceCycleID)
	}
	if w.Operation != librarian.FactOperationWrite {
		t.Errorf("operation = %q", w.Operation)
	}
}

func TestMemoryWrite_TrimsKey(t *testing.T) {
	lib := &fakeLib{}
	h := MemoryWrite{Lib: lib}
	_, err := h.Execute(context.Background(), []byte(`{"key":"  ws  ","value":"v","scope":"persistent","confidence":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if lib.written[0].Key != "ws" {
		t.Errorf("key = %q, want trimmed 'ws'", lib.written[0].Key)
	}
}

func TestMemoryWrite_RejectsEmptyKey(t *testing.T) {
	lib := &fakeLib{}
	h := MemoryWrite{Lib: lib}
	_, err := h.Execute(context.Background(), []byte(`{"key":"   ","value":"v","scope":"persistent","confidence":1}`))
	if err == nil || !strings.Contains(err.Error(), "key must not be empty") {
		t.Fatalf("err = %v", err)
	}
	if len(lib.written) != 0 {
		t.Errorf("should not have written: %+v", lib.written)
	}
}

func TestMemoryWrite_ClampsConfidence(t *testing.T) {
	lib := &fakeLib{}
	h := MemoryWrite{Lib: lib}
	_, _ = h.Execute(context.Background(), []byte(`{"key":"k","value":"v","scope":"persistent","confidence":1.5}`))
	_, _ = h.Execute(context.Background(), []byte(`{"key":"k","value":"v","scope":"persistent","confidence":-0.2}`))
	if lib.written[0].Confidence != 1.0 {
		t.Errorf("clamp-high = %f, want 1.0", lib.written[0].Confidence)
	}
	if lib.written[1].Confidence != 0.0 {
		t.Errorf("clamp-low = %f, want 0.0", lib.written[1].Confidence)
	}
}

func TestMemoryWrite_UnknownScopeFallsBackToSession(t *testing.T) {
	// Mirrors Springdrift: a hallucinated scope doesn't error — falls
	// back to Session so the call still does something useful.
	lib := &fakeLib{}
	h := MemoryWrite{Lib: lib}
	_, err := h.Execute(context.Background(), []byte(`{"key":"k","value":"v","scope":"forever","confidence":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if lib.written[0].Scope != librarian.FactScopeSession {
		t.Errorf("scope = %q, want session", lib.written[0].Scope)
	}
}

func TestMemoryWrite_AcceptsAllValidScopes(t *testing.T) {
	lib := &fakeLib{}
	h := MemoryWrite{Lib: lib}
	for _, scope := range []string{"persistent", "session", "ephemeral"} {
		_, err := h.Execute(context.Background(),
			[]byte(`{"key":"k","value":"v","scope":"`+scope+`","confidence":1}`))
		if err != nil {
			t.Errorf("scope %q errored: %v", scope, err)
		}
	}
	wantScopes := []librarian.FactScope{
		librarian.FactScopePersistent,
		librarian.FactScopeSession,
		librarian.FactScopeEphemeral,
	}
	for i, w := range lib.written {
		if w.Scope != wantScopes[i] {
			t.Errorf("written[%d].Scope = %q, want %q", i, w.Scope, wantScopes[i])
		}
	}
}

func TestMemoryWrite_EmptyInputErrors(t *testing.T) {
	h := MemoryWrite{Lib: &fakeLib{}}
	_, err := h.Execute(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "empty input") {
		t.Fatalf("err = %v", err)
	}
}

func TestMemoryWrite_MalformedInputErrors(t *testing.T) {
	h := MemoryWrite{Lib: &fakeLib{}}
	_, err := h.Execute(context.Background(), []byte(`{not json`))
	if err == nil || !strings.Contains(err.Error(), "decode input") {
		t.Fatalf("err = %v", err)
	}
}

func TestMemoryWrite_NoCycleIDInContextLeavesProvenanceBlank(t *testing.T) {
	lib := &fakeLib{}
	h := MemoryWrite{Lib: lib}
	_, err := h.Execute(context.Background(),
		[]byte(`{"key":"k","value":"v","scope":"persistent","confidence":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if lib.written[0].SourceCycleID != "" {
		t.Errorf("source_cycle_id = %q, want empty", lib.written[0].SourceCycleID)
	}
}

// ---- memory_read ----

func TestMemoryRead_FoundFact(t *testing.T) {
	ts := time.Date(2026, 4, 30, 9, 0, 0, 0, time.UTC)
	lib := &fakeLib{
		get: &librarian.Fact{
			Key: "user_name", Value: "Seamus",
			Scope: librarian.FactScopePersistent, Confidence: 0.93, Timestamp: ts,
		},
	}
	h := MemoryRead{Lib: lib}
	out, err := h.Execute(context.Background(), []byte(`{"key":"user_name"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"key: user_name", "value: Seamus", "scope: persistent", "0.93", "2026-04-30T09:00:00"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output: %q", want, out)
		}
	}
}

func TestMemoryRead_NotFound(t *testing.T) {
	h := MemoryRead{Lib: &fakeLib{}}
	out, err := h.Execute(context.Background(), []byte(`{"key":"nope"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No fact found") || !strings.Contains(out, `"nope"`) {
		t.Errorf("unexpected output: %q", out)
	}
}

func TestMemoryRead_RejectsEmptyKey(t *testing.T) {
	h := MemoryRead{Lib: &fakeLib{}}
	_, err := h.Execute(context.Background(), []byte(`{"key":""}`))
	if err == nil || !strings.Contains(err.Error(), "key must not be empty") {
		t.Fatalf("err = %v", err)
	}
}

func TestMemoryRead_EmptyInputErrors(t *testing.T) {
	h := MemoryRead{Lib: &fakeLib{}}
	_, err := h.Execute(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "empty input") {
		t.Fatalf("err = %v", err)
	}
}

func TestMemoryRead_MalformedInputErrors(t *testing.T) {
	h := MemoryRead{Lib: &fakeLib{}}
	_, err := h.Execute(context.Background(), []byte(`not json`))
	if err == nil || !strings.Contains(err.Error(), "decode input") {
		t.Fatalf("err = %v", err)
	}
}

// ---- memory_clear_key ----

func TestMemoryClear_RecordsTombstoneWithCycleID(t *testing.T) {
	lib := &fakeLib{}
	h := MemoryClearKey{Lib: lib}
	ctx := cyclelog.WithCycleID(context.Background(), "cyc-7")
	out, err := h.Execute(ctx, []byte(`{"key":"old_pref"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"old_pref"`) {
		t.Errorf("output missing key: %q", out)
	}
	if len(lib.cleared) != 1 {
		t.Fatalf("cleared = %+v", lib.cleared)
	}
	if lib.cleared[0].key != "old_pref" || lib.cleared[0].cycleID != "cyc-7" {
		t.Errorf("clear call = %+v", lib.cleared[0])
	}
}

func TestMemoryClear_RejectsEmptyKey(t *testing.T) {
	h := MemoryClearKey{Lib: &fakeLib{}}
	_, err := h.Execute(context.Background(), []byte(`{"key":"   "}`))
	if err == nil || !strings.Contains(err.Error(), "key must not be empty") {
		t.Fatalf("err = %v", err)
	}
}

// ---- memory_query_facts ----

func TestMemoryQuery_FormatsHits(t *testing.T) {
	lib := &fakeLib{
		searchHit: []librarian.Fact{
			{Key: "user_name", Value: "Seamus", Confidence: 0.9},
			{Key: "tz", Value: "Europe/Dublin", Confidence: 1.0},
		},
	}
	h := MemoryQueryFacts{Lib: lib}
	out, err := h.Execute(context.Background(), []byte(`{"keyword":"Sea"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "user_name = Seamus") || !strings.Contains(out, "tz = Europe/Dublin") {
		t.Errorf("output: %q", out)
	}
	if lib.lastSearch != "Sea" {
		t.Errorf("librarian saw keyword %q", lib.lastSearch)
	}
	if lib.lastSearchN != memoryQueryFactsLimit {
		t.Errorf("limit = %d, want %d", lib.lastSearchN, memoryQueryFactsLimit)
	}
}

func TestMemoryQuery_NoHits(t *testing.T) {
	h := MemoryQueryFacts{Lib: &fakeLib{}}
	out, err := h.Execute(context.Background(), []byte(`{"keyword":"nope"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No facts found matching") || !strings.Contains(out, `"nope"`) {
		t.Errorf("output: %q", out)
	}
}

func TestMemoryQuery_RejectsEmptyKeyword(t *testing.T) {
	h := MemoryQueryFacts{Lib: &fakeLib{}}
	_, err := h.Execute(context.Background(), []byte(`{"keyword":"  "}`))
	if err == nil || !strings.Contains(err.Error(), "keyword must not be empty") {
		t.Fatalf("err = %v", err)
	}
}

func TestMemoryQuery_EmptyInputErrors(t *testing.T) {
	h := MemoryQueryFacts{Lib: &fakeLib{}}
	_, err := h.Execute(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "empty input") {
		t.Fatalf("err = %v", err)
	}
}

func TestMemoryQuery_MalformedInputErrors(t *testing.T) {
	h := MemoryQueryFacts{Lib: &fakeLib{}}
	_, err := h.Execute(context.Background(), []byte(`{not json`))
	if err == nil || !strings.Contains(err.Error(), "decode input") {
		t.Fatalf("err = %v", err)
	}
}

// ---- helpers ----

func TestParseScope(t *testing.T) {
	cases := []struct {
		in   string
		want librarian.FactScope
	}{
		{"persistent", librarian.FactScopePersistent},
		{"ephemeral", librarian.FactScopeEphemeral},
		{"session", librarian.FactScopeSession},
		{"unknown", librarian.FactScopeSession},
		{"", librarian.FactScopeSession},
	}
	for _, c := range cases {
		if got := parseScope(c.in); got != c.want {
			t.Errorf("parseScope(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestClamp01(t *testing.T) {
	if got := clamp01(-1); got != 0 {
		t.Errorf("clamp(-1) = %f", got)
	}
	if got := clamp01(2); got != 1 {
		t.Errorf("clamp(2) = %f", got)
	}
	if got := clamp01(0.5); got != 0.5 {
		t.Errorf("clamp(0.5) = %f", got)
	}
}

func TestMemoryToolMetadata(t *testing.T) {
	tools := []Handler{
		MemoryWrite{},
		MemoryRead{},
		MemoryClearKey{},
		MemoryQueryFacts{},
	}
	wantNames := []string{"memory_write", "memory_read", "memory_clear_key", "memory_query_facts"}
	for i, h := range tools {
		got := h.Tool()
		if got.Name != wantNames[i] {
			t.Errorf("[%d].Name = %q, want %q", i, got.Name, wantNames[i])
		}
		if got.Description == "" {
			t.Errorf("[%d].Description empty", i)
		}
	}
}

// ---- Registry round-trip ----

func TestMemoryTools_RegisterableTogether(t *testing.T) {
	lib := &fakeLib{}
	r := NewRegistry()
	for _, h := range []Handler{
		MemoryWrite{Lib: lib},
		MemoryRead{Lib: lib},
		MemoryClearKey{Lib: lib},
		MemoryQueryFacts{Lib: lib},
	} {
		if err := r.Register(h); err != nil {
			t.Fatalf("register %s: %v", h.Tool().Name, err)
		}
	}
	if len(r.Names()) != 4 {
		t.Fatalf("names = %+v", r.Names())
	}
}

// ---- End-to-end against real librarian ----

func TestMemoryTools_AgainstRealLibrarian(t *testing.T) {
	// Wire the real librarian + the four tool handlers; exercise the
	// full write → read → query → clear → read loop.
	dir := t.TempDir()
	lib, err := librarian.New(librarian.Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go lib.Run(ctx)

	r := NewRegistry()
	r.MustRegister(MemoryWrite{Lib: lib})
	r.MustRegister(MemoryRead{Lib: lib})
	r.MustRegister(MemoryClearKey{Lib: lib})
	r.MustRegister(MemoryQueryFacts{Lib: lib})

	dispatchCtx := cyclelog.WithCycleID(ctx, "cyc-1")

	// write
	if _, err := r.Dispatch(dispatchCtx, "memory_write",
		[]byte(`{"key":"city","value":"Cavan","scope":"persistent","confidence":1}`)); err != nil {
		t.Fatal(err)
	}
	// read
	out, err := r.Dispatch(dispatchCtx, "memory_read", []byte(`{"key":"city"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "value: Cavan") {
		t.Errorf("read after write: %q", out)
	}
	// query
	out, err = r.Dispatch(dispatchCtx, "memory_query_facts", []byte(`{"keyword":"avan"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "city = Cavan") {
		t.Errorf("query: %q", out)
	}
	// clear
	if _, err := r.Dispatch(dispatchCtx, "memory_clear_key", []byte(`{"key":"city"}`)); err != nil {
		t.Fatal(err)
	}
	// read after clear
	out, err = r.Dispatch(dispatchCtx, "memory_read", []byte(`{"key":"city"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No fact found") {
		t.Errorf("read after clear: %q", out)
	}
}
