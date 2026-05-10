package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/seamus-brady/retainer/internal/cbr"
)

// fakeCBR captures librarian-side calls so tests can assert on what
// the tool tried to do. Methods either return canned data or an
// error — selectable per test.
type fakeCBR struct {
	cases       map[string]cbr.Case
	scored      []cbr.Scored
	suppressErr error
	boostErr    error
	missing     map[string]bool
}

func newFakeCBR() *fakeCBR {
	return &fakeCBR{cases: make(map[string]cbr.Case), missing: make(map[string]bool)}
}

func (f *fakeCBR) seed(c cbr.Case) {
	f.cases[c.ID] = c
}

func (f *fakeCBR) RetrieveCases(_ context.Context, q cbr.Query) []cbr.Scored {
	return f.scored
}

func (f *fakeCBR) GetCase(id string) (cbr.Case, bool) {
	if f.missing[id] {
		return cbr.Case{}, false
	}
	c, ok := f.cases[id]
	return c, ok
}

func (f *fakeCBR) SuppressCase(id string) (cbr.Case, error) {
	if f.suppressErr != nil {
		return cbr.Case{}, f.suppressErr
	}
	c, ok := f.cases[id]
	if !ok {
		return cbr.Case{}, errors.New("not found")
	}
	c.Redacted = true
	f.cases[id] = c
	return c, nil
}

func (f *fakeCBR) UnsuppressCase(id string) (cbr.Case, error) {
	c, ok := f.cases[id]
	if !ok {
		return cbr.Case{}, errors.New("not found")
	}
	c.Redacted = false
	f.cases[id] = c
	return c, nil
}

func (f *fakeCBR) BoostCase(id string, delta float64) (cbr.Case, error) {
	if f.boostErr != nil {
		return cbr.Case{}, f.boostErr
	}
	c, ok := f.cases[id]
	if !ok {
		return cbr.Case{}, errors.New("not found")
	}
	next := c.Outcome.Confidence + delta
	if next < 0 {
		next = 0
	}
	if next > 1 {
		next = 1
	}
	c.Outcome.Confidence = next
	f.cases[id] = c
	return c, nil
}

func (f *fakeCBR) AnnotateCase(id, pitfall string) (cbr.Case, error) {
	c, ok := f.cases[id]
	if !ok {
		return cbr.Case{}, errors.New("not found")
	}
	c.Outcome.Pitfalls = append(c.Outcome.Pitfalls, pitfall)
	f.cases[id] = c
	return c, nil
}

func (f *fakeCBR) CorrectCase(id string, fields cbr.Case) (cbr.Case, error) {
	c, ok := f.cases[id]
	if !ok {
		return cbr.Case{}, errors.New("not found")
	}
	c.Problem = fields.Problem
	c.Solution = fields.Solution
	c.Outcome = fields.Outcome
	if fields.Category != "" {
		c.Category = fields.Category
	}
	f.cases[id] = c
	return c, nil
}

// ---- recall_cases ----

func TestRecallCases_RequiresAtLeastOneSignal(t *testing.T) {
	h := ObserverRecallCases{Lib: newFakeCBR()}
	_, err := h.Execute(context.Background(), []byte(`{}`))
	if err == nil {
		t.Fatal("expected error for empty query")
	}
}

func TestRecallCases_FormatsScoredOutput(t *testing.T) {
	f := newFakeCBR()
	now := time.Now()
	f.scored = []cbr.Scored{
		{
			Score: 0.93,
			Case: cbr.Case{
				ID:        "abc-123-def",
				Timestamp: now,
				Problem:   cbr.Problem{Intent: "debug auth", Domain: "auth", Keywords: []string{"oauth", "token"}},
				Solution:  cbr.Solution{Approach: "trace the OAuth round trip"},
				Outcome:   cbr.Outcome{Status: cbr.StatusSuccess, Confidence: 0.85, Assessment: "found stale token"},
				Category:  cbr.CategoryTroubleshooting,
			},
		},
	}
	h := ObserverRecallCases{Lib: f}
	out, err := h.Execute(context.Background(), []byte(`{"intent":"debug auth"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{"abc-123-", "0.930", "troubleshooting", "trace the OAuth", "stale token"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q in:\n%s", want, out)
		}
	}
}

func TestRecallCases_NoResultsMessage(t *testing.T) {
	h := ObserverRecallCases{Lib: newFakeCBR()}
	out, err := h.Execute(context.Background(), []byte(`{"intent":"x"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "no similar cases") {
		t.Errorf("expected no-results message; got %q", out)
	}
}

func TestRecallCases_ClampsAboveMaxResults(t *testing.T) {
	f := newFakeCBR()
	called := 0
	f.scored = []cbr.Scored{}
	wrapped := &interceptedCBR{inner: f, onRetrieve: func(q cbr.Query) { called = q.MaxResults }}
	h := ObserverRecallCases{Lib: wrapped}
	if _, err := h.Execute(context.Background(), []byte(`{"intent":"x","max_results":99}`)); err != nil {
		t.Fatal(err)
	}
	if called > recallCasesMaxLimit {
		t.Errorf("MaxResults clamp failed: got %d, want ≤ %d", called, recallCasesMaxLimit)
	}
}

// interceptedCBR wraps fakeCBR to spy on RetrieveCases args.
type interceptedCBR struct {
	inner      *fakeCBR
	onRetrieve func(cbr.Query)
}

func (i *interceptedCBR) RetrieveCases(ctx context.Context, q cbr.Query) []cbr.Scored {
	i.onRetrieve(q)
	return i.inner.RetrieveCases(ctx, q)
}
func (i *interceptedCBR) GetCase(id string) (cbr.Case, bool)         { return i.inner.GetCase(id) }
func (i *interceptedCBR) SuppressCase(id string) (cbr.Case, error)   { return i.inner.SuppressCase(id) }
func (i *interceptedCBR) UnsuppressCase(id string) (cbr.Case, error) { return i.inner.UnsuppressCase(id) }
func (i *interceptedCBR) BoostCase(id string, d float64) (cbr.Case, error) {
	return i.inner.BoostCase(id, d)
}
func (i *interceptedCBR) AnnotateCase(id, p string) (cbr.Case, error) { return i.inner.AnnotateCase(id, p) }
func (i *interceptedCBR) CorrectCase(id string, f cbr.Case) (cbr.Case, error) {
	return i.inner.CorrectCase(id, f)
}

// ---- case_curate ----

func TestCaseCurate_RejectsEmptyInput(t *testing.T) {
	h := ObserverCaseCurate{Lib: newFakeCBR()}
	if _, err := h.Execute(context.Background(), nil); err == nil {
		t.Error("empty input should error")
	}
}

func TestCaseCurate_RejectsMissingAction(t *testing.T) {
	h := ObserverCaseCurate{Lib: newFakeCBR()}
	if _, err := h.Execute(context.Background(), []byte(`{"case_id":"c1"}`)); err == nil {
		t.Error("missing action should error")
	}
}

func TestCaseCurate_RejectsUnknownAction(t *testing.T) {
	h := ObserverCaseCurate{Lib: newFakeCBR()}
	if _, err := h.Execute(context.Background(), []byte(`{"action":"explode","case_id":"c1"}`)); err == nil {
		t.Error("unknown action should error")
	}
}

func TestCaseCurate_RejectsEmptyCaseID(t *testing.T) {
	h := ObserverCaseCurate{Lib: newFakeCBR()}
	if _, err := h.Execute(context.Background(), []byte(`{"action":"suppress","case_id":""}`)); err == nil {
		t.Error("empty case_id should error")
	}
}

// suppress / unsuppress

func TestCaseCurate_Suppress(t *testing.T) {
	f := newFakeCBR()
	f.seed(cbr.Case{ID: "c1", Outcome: cbr.Outcome{Confidence: 0.5}})
	h := ObserverCaseCurate{Lib: f}
	out, err := h.Execute(context.Background(), []byte(`{"action":"suppress","case_id":"c1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "suppressed") {
		t.Errorf("output: %q", out)
	}
	if !f.cases["c1"].Redacted {
		t.Error("case not actually suppressed")
	}
}

func TestCaseCurate_Unsuppress(t *testing.T) {
	f := newFakeCBR()
	f.seed(cbr.Case{ID: "c1", Redacted: true})
	h := ObserverCaseCurate{Lib: f}
	out, err := h.Execute(context.Background(), []byte(`{"action":"unsuppress","case_id":"c1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "unsuppressed") {
		t.Errorf("output: %q", out)
	}
	if f.cases["c1"].Redacted {
		t.Error("case still marked redacted")
	}
}

// boost

func TestCaseCurate_BoostAdjustsConfidence(t *testing.T) {
	f := newFakeCBR()
	f.seed(cbr.Case{ID: "c1", Outcome: cbr.Outcome{Confidence: 0.5}})
	h := ObserverCaseCurate{Lib: f}
	out, err := h.Execute(context.Background(), []byte(`{"action":"boost","case_id":"c1","delta":0.3}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "0.80") {
		t.Errorf("output: %q", out)
	}
}

func TestCaseCurate_BoostRejectsOutOfRangeDelta(t *testing.T) {
	h := ObserverCaseCurate{Lib: newFakeCBR()}
	for _, d := range []float64{-2.0, 1.5} {
		body := []byte(`{"action":"boost","case_id":"c1","delta":` + ftoa(d) + `}`)
		if _, err := h.Execute(context.Background(), body); err == nil {
			t.Errorf("delta=%g should error", d)
		}
	}
}

// annotate

func TestCaseCurate_AnnotateAppendsPitfall(t *testing.T) {
	f := newFakeCBR()
	f.seed(cbr.Case{ID: "c1"})
	h := ObserverCaseCurate{Lib: f}
	out, err := h.Execute(context.Background(),
		[]byte(`{"action":"annotate","case_id":"c1","pitfall":"watch out for stale tokens"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1 total pitfalls") {
		t.Errorf("output: %q", out)
	}
	if len(f.cases["c1"].Outcome.Pitfalls) != 1 {
		t.Error("pitfall not appended")
	}
}

func TestCaseCurate_AnnotateRejectsEmptyPitfall(t *testing.T) {
	h := ObserverCaseCurate{Lib: newFakeCBR()}
	if _, err := h.Execute(context.Background(),
		[]byte(`{"action":"annotate","case_id":"c1","pitfall":""}`)); err == nil {
		t.Error("empty pitfall should error")
	}
}

// correct

func TestCaseCurate_CorrectUpdatesCategoryAndAssessment(t *testing.T) {
	f := newFakeCBR()
	f.seed(cbr.Case{
		ID:       "c1",
		Category: cbr.CategoryDomainKnowledge,
		Outcome:  cbr.Outcome{Confidence: 0.5, Assessment: "vague"},
	})
	h := ObserverCaseCurate{Lib: f}
	out, err := h.Execute(context.Background(),
		[]byte(`{"action":"correct","case_id":"c1","category":"strategy","assessment":"actually a strategy that worked"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "strategy") {
		t.Errorf("output: %q", out)
	}
	updated := f.cases["c1"]
	if updated.Category != cbr.CategoryStrategy {
		t.Errorf("category = %q", updated.Category)
	}
	if updated.Outcome.Assessment != "actually a strategy that worked" {
		t.Errorf("assessment not updated: %q", updated.Outcome.Assessment)
	}
}

func TestCaseCurate_CorrectAcceptsZeroConfidenceAsExplicit(t *testing.T) {
	f := newFakeCBR()
	f.seed(cbr.Case{ID: "c1", Outcome: cbr.Outcome{Confidence: 0.5}})
	h := ObserverCaseCurate{Lib: f}
	if _, err := h.Execute(context.Background(),
		[]byte(`{"action":"correct","case_id":"c1","confidence":0}`)); err != nil {
		t.Fatal(err)
	}
	if f.cases["c1"].Outcome.Confidence != 0 {
		t.Errorf("explicit zero confidence should apply; got %g", f.cases["c1"].Outcome.Confidence)
	}
}

func TestCaseCurate_CorrectRejectsUnknownCategory(t *testing.T) {
	f := newFakeCBR()
	f.seed(cbr.Case{ID: "c1"})
	h := ObserverCaseCurate{Lib: f}
	if _, err := h.Execute(context.Background(),
		[]byte(`{"action":"correct","case_id":"c1","category":"made-up"}`)); err == nil {
		t.Error("unknown category should error")
	}
}

func TestCaseCurate_CorrectRejectsConfidenceOutOfRange(t *testing.T) {
	f := newFakeCBR()
	f.seed(cbr.Case{ID: "c1"})
	h := ObserverCaseCurate{Lib: f}
	if _, err := h.Execute(context.Background(),
		[]byte(`{"action":"correct","case_id":"c1","confidence":1.5}`)); err == nil {
		t.Error("confidence>1 should error")
	}
}

func TestCaseCurate_CorrectRejectsMissingCase(t *testing.T) {
	h := ObserverCaseCurate{Lib: newFakeCBR()}
	if _, err := h.Execute(context.Background(),
		[]byte(`{"action":"correct","case_id":"nope","category":"strategy"}`)); err == nil {
		t.Error("missing case should error")
	}
}

// ftoa formats a float for inline JSON without dragging in fmt for
// these tiny test bodies.
func ftoa(f float64) string {
	return strings.TrimRight(strings.TrimRight(formatFloat(f), "0"), ".")
}

func formatFloat(f float64) string {
	// Sufficient precision for the test deltas used above.
	const digits = 6
	const e = 1e6
	if f == 0 {
		return "0"
	}
	neg := f < 0
	if neg {
		f = -f
	}
	whole := int64(f)
	frac := int64((f - float64(whole)) * e)
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	b.WriteString(itoaInt(whole))
	b.WriteByte('.')
	fracStr := itoaInt(frac)
	for len(fracStr) < digits {
		fracStr = "0" + fracStr
	}
	b.WriteString(fracStr)
	return b.String()
}

func itoaInt(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for n > 0 {
		pos--
		b[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(b[pos:])
}
