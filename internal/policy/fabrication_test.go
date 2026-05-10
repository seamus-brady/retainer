package policy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/seamus-brady/retainer/internal/llm"
)

// fakeStructuredProvider populates the dst struct with a
// scripted fabricationVerdict. Lets the scorer tests exercise
// the verdict-mapping logic without spinning up a real LLM.
type fakeStructuredProvider struct {
	verdict fabricationVerdict
	err     error
	calls   int
}

func (*fakeStructuredProvider) Name() string { return "fake-structured" }
func (f *fakeStructuredProvider) Chat(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, errors.New("not used")
}
func (f *fakeStructuredProvider) ChatStructured(_ context.Context, _ llm.Request, _ llm.Schema, dst any) (llm.Usage, error) {
	f.calls++
	if f.err != nil {
		return llm.Usage{}, f.err
	}
	if v, ok := dst.(*fabricationVerdict); ok {
		*v = f.verdict
	}
	return llm.Usage{}, nil
}

func newFabricationFixture(verdict fabricationVerdict, err error, minConfidence float64) *fabricationScorer {
	return newFabricationScorer(FabricationScorerConfig{
		Provider:      &fakeStructuredProvider{verdict: verdict, err: err},
		Model:         "test-model",
		Timeout:       2 * time.Second,
		MinConfidence: minConfidence,
	})
}

func TestFabricationScore_AllowVerdict(t *testing.T) {
	s := newFabricationFixture(fabricationVerdict{
		Verdict:    "allow",
		Reasoning:  "every claim is grounded",
		Confidence: 0.85,
	}, nil, 0.7)
	r := s.Score(context.Background(), "the file is intact", []ToolEvent{
		{Name: "read_file", Output: "package main\n\nfunc main() {}\n"},
	})
	if r.Verdict != Allow {
		t.Errorf("verdict = %v, want Allow", r.Verdict)
	}
	if r.Score != 0.85 {
		t.Errorf("score = %f, want 0.85", r.Score)
	}
	if !strings.Contains(r.Trail, "allow") {
		t.Errorf("trail missing 'allow': %s", r.Trail)
	}
}

func TestFabricationScore_EscalateAboveThreshold(t *testing.T) {
	s := newFabricationFixture(fabricationVerdict{
		Verdict:       "escalate",
		Reasoning:     "result.Location not in tool log",
		Confidence:    0.9,
		FlaggedClaims: []string{"result.Location == nil — no read_file or search_code call retrieved this field"},
	}, nil, 0.7)
	r := s.Score(context.Background(), "the bug is in result.Location == nil", []ToolEvent{
		{Name: "agent_observer", Output: "no related cycles found"},
	})
	if r.Verdict != Escalate {
		t.Errorf("verdict = %v, want Escalate (above threshold)", r.Verdict)
	}
	flagged := FlaggedClaims(r)
	if len(flagged) != 1 {
		t.Errorf("flagged count = %d, want 1; got %v", len(flagged), flagged)
	}
	if !strings.Contains(flagged[0], "result.Location") {
		t.Errorf("flagged claim wrong: %v", flagged)
	}
}

func TestFabricationScore_SubthresholdLogsButDoesNotGate(t *testing.T) {
	// Confidence below MinConfidence: telemetry only, verdict
	// stays Allow so the cog doesn't append a verification
	// footer for low-signal events. Operator still sees the
	// score in the cycle log.
	s := newFabricationFixture(fabricationVerdict{
		Verdict:    "escalate",
		Reasoning:  "borderline; one identifier might not be grounded",
		Confidence: 0.5,
	}, nil, 0.7)
	r := s.Score(context.Background(), "some output", []ToolEvent{
		{Name: "x", Output: "y"},
	})
	if r.Verdict != Allow {
		t.Errorf("verdict = %v, want Allow (subthreshold)", r.Verdict)
	}
	if !strings.Contains(r.Trail, "subthreshold") {
		t.Errorf("trail missing subthreshold marker: %s", r.Trail)
	}
}

func TestFabricationScore_NoToolLogShortCircuitsAllow(t *testing.T) {
	prov := &fakeStructuredProvider{}
	s := newFabricationScorer(FabricationScorerConfig{
		Provider: prov, Model: "m", Timeout: time.Second, MinConfidence: 0.7,
	})
	r := s.Score(context.Background(), "hello there", nil)
	if r.Verdict != Allow {
		t.Errorf("verdict = %v, want Allow (no tools to verify against)", r.Verdict)
	}
	if prov.calls != 0 {
		t.Errorf("LLM called %d times; should short-circuit without invoking the scorer", prov.calls)
	}
	if !strings.Contains(r.Trail, "no tool log") {
		t.Errorf("trail should explain skip: %s", r.Trail)
	}
}

func TestFabricationScore_EmptyOutputShortCircuits(t *testing.T) {
	prov := &fakeStructuredProvider{}
	s := newFabricationScorer(FabricationScorerConfig{
		Provider: prov, Model: "m", Timeout: time.Second, MinConfidence: 0.7,
	})
	r := s.Score(context.Background(), "", []ToolEvent{{Name: "x", Output: "y"}})
	if r.Verdict != Allow {
		t.Errorf("verdict = %v, want Allow (empty output)", r.Verdict)
	}
	if prov.calls != 0 {
		t.Errorf("LLM should not be called for empty output")
	}
}

func TestFabricationScore_ScorerErrorFailsOpen(t *testing.T) {
	// Network error / timeout / etc must not gate the cycle.
	// The safety layer fails open so the cog's progress isn't
	// blocked by an unreachable scorer. The Inconclusive flag
	// surfaces the failure for operator review.
	s := newFabricationFixture(fabricationVerdict{}, errors.New("provider unreachable"), 0.7)
	r := s.Score(context.Background(), "any output", []ToolEvent{{Name: "x", Output: "y"}})
	if r.Verdict != Allow {
		t.Errorf("verdict = %v, want Allow (fail-open)", r.Verdict)
	}
	if !r.Inconclusive {
		t.Error("expected Inconclusive=true on scorer error")
	}
	if !strings.Contains(r.Trail, "provider unreachable") {
		t.Errorf("trail missing error context: %s", r.Trail)
	}
}

func TestFabricationScore_OutOfRangeConfidenceFailsOpen(t *testing.T) {
	s := newFabricationFixture(fabricationVerdict{
		Verdict:    "escalate",
		Confidence: 1.5, // bogus
	}, nil, 0.7)
	r := s.Score(context.Background(), "any output", []ToolEvent{{Name: "x", Output: "y"}})
	if r.Verdict != Allow || !r.Inconclusive {
		t.Errorf("expected Allow + Inconclusive on out-of-range confidence; got %v inconclusive=%v",
			r.Verdict, r.Inconclusive)
	}
}

func TestFabricationScore_UnknownVerdictFailsOpen(t *testing.T) {
	s := newFabricationFixture(fabricationVerdict{
		Verdict:    "weird",
		Confidence: 0.9,
	}, nil, 0.7)
	r := s.Score(context.Background(), "any output", []ToolEvent{{Name: "x", Output: "y"}})
	if r.Verdict != Allow || !r.Inconclusive {
		t.Errorf("expected Allow + Inconclusive on unknown verdict; got %v inconclusive=%v",
			r.Verdict, r.Inconclusive)
	}
}

func TestEngine_EvaluateOutputFabrication_NoScorerConfigured(t *testing.T) {
	rs, _, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	e := New(Config{Rules: rs}) // no FabricationScorer
	r := e.EvaluateOutputFabrication(context.Background(), "any output", []ToolEvent{{Name: "x", Output: "y"}})
	if r.Verdict != Allow {
		t.Errorf("verdict = %v, want Allow", r.Verdict)
	}
	if !strings.Contains(r.Trail, "not configured") {
		t.Errorf("trail should explain absence: %s", r.Trail)
	}
	if e.HasFabricationScorer() {
		t.Error("HasFabricationScorer() = true, want false")
	}
}

func TestEngine_EvaluateOutputFabrication_ScorerWired(t *testing.T) {
	rs, _, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	prov := &fakeStructuredProvider{verdict: fabricationVerdict{
		Verdict: "allow", Reasoning: "ok", Confidence: 0.9,
	}}
	e := New(Config{
		Rules: rs,
		Fabrication: FabricationScorerConfig{
			Provider: prov, Model: "m", Timeout: time.Second,
		},
	})
	if !e.HasFabricationScorer() {
		t.Fatal("HasFabricationScorer() = false")
	}
	r := e.EvaluateOutputFabrication(context.Background(), "out", []ToolEvent{{Name: "t", Output: "o"}})
	if r.Verdict != Allow {
		t.Errorf("verdict = %v", r.Verdict)
	}
	if prov.calls != 1 {
		t.Errorf("scorer call count = %d, want 1", prov.calls)
	}
}

func TestFlaggedClaims_FiltersBySource(t *testing.T) {
	r := Result{Triggered: []Trigger{
		{Source: "fabrication", RuleName: "claim 1"},
		{Source: "deterministic", RuleName: "rule X"},
		{Source: "fabrication", RuleName: "claim 2"},
	}}
	got := FlaggedClaims(r)
	if len(got) != 2 {
		t.Errorf("expected 2 fabrication claims, got %d: %v", len(got), got)
	}
	for _, want := range []string{"claim 1", "claim 2"} {
		found := false
		for _, c := range got {
			if c == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing claim %q in %v", want, got)
		}
	}
}

func TestTruncateForToolLog_KeepsShort(t *testing.T) {
	in := strings.Repeat("a", 100)
	got := truncateForToolLog(in)
	if got != in {
		t.Error("short input should pass through unchanged")
	}
}

func TestTruncateForToolLog_CapsLong(t *testing.T) {
	in := strings.Repeat("a", 8192)
	got := truncateForToolLog(in)
	if len(got) >= len(in) {
		t.Error("long input should be truncated")
	}
	if !strings.Contains(got, "truncated for fabrication-scorer evidence pass") {
		t.Errorf("missing truncation marker in: %s", got[len(got)-100:])
	}
}

// ---- ScoreToolInput ----

func TestScoreToolInput_AllowsGroundedInput(t *testing.T) {
	s := newFabricationFixture(fabricationVerdict{
		Verdict:    "allow",
		Reasoning:  "URLs in the email body all appear in brave_web_search results",
		Confidence: 0.92,
	}, nil, 0.7)
	r := s.ScoreToolInput(context.Background(), "send_email",
		`{"body":"https://example.com/page"}`,
		[]ToolEvent{{Name: "brave_web_search", Output: "result: https://example.com/page"}},
	)
	if r.Verdict != Allow {
		t.Errorf("verdict = %v, want Allow", r.Verdict)
	}
}

func TestScoreToolInput_BlocksAboveThreshold(t *testing.T) {
	// Above threshold → BLOCK (not Escalate). Tool dispatch is one-shot;
	// blocking is the only meaningful intervention.
	s := newFabricationFixture(fabricationVerdict{
		Verdict:       "escalate",
		Reasoning:     "URL https://wimo.com/ic-r8600 not present in any tool result",
		Confidence:    0.95,
		FlaggedClaims: []string{"https://wimo.com/ic-r8600 — not in tool log"},
	}, nil, 0.7)
	r := s.ScoreToolInput(context.Background(), "send_email",
		`{"body":"check https://wimo.com/ic-r8600 for pricing"}`,
		[]ToolEvent{{Name: "brave_web_search", Output: "no results for ICOM"}},
	)
	if r.Verdict != Block {
		t.Errorf("verdict = %v, want Block (above threshold)", r.Verdict)
	}
	if r.Score != 0.95 {
		t.Errorf("score = %f, want 0.95", r.Score)
	}
	if len(r.Triggered) == 0 {
		t.Errorf("expected flagged claims to surface in Triggered")
	}
}

func TestScoreToolInput_SubthresholdAllows(t *testing.T) {
	// Below threshold → log + allow. False positives shouldn't
	// block; they should land in the trail for operator review.
	s := newFabricationFixture(fabricationVerdict{
		Verdict:    "escalate",
		Reasoning:  "weak signal",
		Confidence: 0.4,
	}, nil, 0.7)
	r := s.ScoreToolInput(context.Background(), "send_email",
		`{"body":"x"}`,
		[]ToolEvent{{Name: "brave_web_search", Output: "x"}},
	)
	if r.Verdict != Allow {
		t.Errorf("verdict = %v, want Allow (subthreshold)", r.Verdict)
	}
	if !strings.Contains(r.Trail, "subthreshold") {
		t.Errorf("trail missing 'subthreshold': %s", r.Trail)
	}
}

func TestScoreToolInput_EmptyInputShortCircuits(t *testing.T) {
	prov := &fakeStructuredProvider{}
	s := newFabricationScorer(FabricationScorerConfig{
		Provider: prov,
		Model:    "x",
	})
	r := s.ScoreToolInput(context.Background(), "send_email", "  ",
		[]ToolEvent{{Name: "brave_web_search", Output: "x"}})
	if r.Verdict != Allow {
		t.Errorf("verdict = %v, want Allow (empty input)", r.Verdict)
	}
	if prov.calls != 0 {
		t.Errorf("LLM should NOT have been called for empty input; calls = %d", prov.calls)
	}
}

func TestScoreToolInput_EmptyToolLogShortCircuits(t *testing.T) {
	prov := &fakeStructuredProvider{}
	s := newFabricationScorer(FabricationScorerConfig{
		Provider: prov,
		Model:    "x",
	})
	r := s.ScoreToolInput(context.Background(), "send_email",
		`{"body":"x"}`, nil)
	if r.Verdict != Allow {
		t.Errorf("verdict = %v, want Allow (no tool log)", r.Verdict)
	}
	if prov.calls != 0 {
		t.Errorf("LLM should NOT have been called when tool log is empty; calls = %d", prov.calls)
	}
}

func TestScoreToolInput_ScorerErrorFailsOpen(t *testing.T) {
	s := newFabricationFixture(fabricationVerdict{}, errors.New("connection refused"), 0.7)
	r := s.ScoreToolInput(context.Background(), "send_email",
		`{"body":"x"}`,
		[]ToolEvent{{Name: "brave_web_search", Output: "x"}},
	)
	if r.Verdict != Allow {
		t.Errorf("verdict = %v, want Allow (fail-open)", r.Verdict)
	}
	if !r.Inconclusive {
		t.Errorf("expected Inconclusive=true on scorer error")
	}
}

// ---- IsHighRiskTool ----

func TestIsHighRiskTool_DefaultsCoverComms(t *testing.T) {
	s := newFabricationScorer(FabricationScorerConfig{
		Provider: &fakeStructuredProvider{},
		Model:    "x",
	})
	for _, name := range []string{"send_email", "save_to_library", "export_pdf",
		"create_draft", "update_draft", "promote_draft"} {
		if !s.IsHighRiskTool(name) {
			t.Errorf("default high-risk set should include %q", name)
		}
	}
	for _, name := range []string{"brave_web_search", "memory_read",
		"library_search", "agent_researcher"} {
		if s.IsHighRiskTool(name) {
			t.Errorf("default high-risk set should NOT include benign tool %q", name)
		}
	}
}

func TestIsHighRiskTool_OperatorOverride(t *testing.T) {
	s := newFabricationScorer(FabricationScorerConfig{
		Provider:      &fakeStructuredProvider{},
		Model:         "x",
		HighRiskTools: []string{"custom_publish_tool"},
	})
	if !s.IsHighRiskTool("custom_publish_tool") {
		t.Error("operator-supplied set should be honoured")
	}
	if s.IsHighRiskTool("send_email") {
		t.Error("operator-supplied set should REPLACE defaults, not extend")
	}
}

func TestIsHighRiskTool_EmptySetDisables(t *testing.T) {
	// Set HighRiskTools to non-nil empty slice → input-side
	// scoring effectively disabled (no tool ever matches).
	s := newFabricationScorer(FabricationScorerConfig{
		Provider:      &fakeStructuredProvider{},
		Model:         "x",
		HighRiskTools: []string{},
	})
	if s.IsHighRiskTool("send_email") {
		t.Error("empty HighRiskTools should mean no tool is high-risk")
	}
}
