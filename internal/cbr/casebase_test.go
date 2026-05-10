package cbr

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/seamus-brady/retainer/internal/embed"
)

// ---- Utility / Jaccard / Cosine pure helpers ----

func TestUtility_NilStatsIsNeutral(t *testing.T) {
	if got := Utility(nil); got != 0.5 {
		t.Errorf("nil stats utility = %g, want 0.5", got)
	}
}

func TestUtility_LaplaceSmoothing(t *testing.T) {
	// 3 retrievals, 2 successes → (2+1)/(3+2) = 0.6
	got := Utility(&UsageStats{RetrievalCount: 3, RetrievalSuccessCount: 2})
	if math.Abs(got-0.6) > 1e-9 {
		t.Errorf("got %g, want 0.6", got)
	}
}

func TestUtility_AllSuccessHigherThanAllFailure(t *testing.T) {
	allSuccess := Utility(&UsageStats{RetrievalCount: 5, RetrievalSuccessCount: 5})
	allFail := Utility(&UsageStats{RetrievalCount: 5, RetrievalSuccessCount: 0})
	if !(allSuccess > allFail) {
		t.Errorf("success=%g should beat failure=%g", allSuccess, allFail)
	}
}

func TestJaccard_Empty(t *testing.T) {
	if got := jaccard(nil, []string{"x"}); got != 0 {
		t.Errorf("empty A → %g, want 0", got)
	}
	if got := jaccard([]string{"x"}, nil); got != 0 {
		t.Errorf("empty B → %g, want 0", got)
	}
}

func TestJaccard_FullOverlap(t *testing.T) {
	if got := jaccard([]string{"a", "b"}, []string{"a", "b"}); got != 1.0 {
		t.Errorf("full overlap = %g, want 1.0", got)
	}
}

func TestJaccard_Partial(t *testing.T) {
	// {a, b} ∩ {b, c} = {b}, ∪ = {a, b, c} → 1/3
	got := jaccard([]string{"a", "b"}, []string{"b", "c"})
	if math.Abs(got-1.0/3.0) > 1e-9 {
		t.Errorf("got %g, want %g", got, 1.0/3.0)
	}
}

func TestCosine_Identical(t *testing.T) {
	v := []float32{1, 2, 3}
	if got := cosineSimilarity(v, v); math.Abs(got-1.0) > 1e-6 {
		t.Errorf("self-cosine = %g, want 1", got)
	}
}

func TestCosine_Orthogonal(t *testing.T) {
	a := []float32{1, 0}
	b := []float32{0, 1}
	if got := cosineSimilarity(a, b); math.Abs(got) > 1e-9 {
		t.Errorf("orthogonal cosine = %g, want 0", got)
	}
}

func TestCosine_LengthMismatch(t *testing.T) {
	if got := cosineSimilarity([]float32{1, 2}, []float32{1, 2, 3}); got != 0 {
		t.Errorf("mismatched lengths should yield 0; got %g", got)
	}
}

func TestCosine_ZeroVector(t *testing.T) {
	if got := cosineSimilarity([]float32{0, 0}, []float32{1, 1}); got != 0 {
		t.Errorf("zero vector should yield 0; got %g", got)
	}
}

// ---- CaseTokens ----

func TestCaseTokens_DedupesAndLowercases(t *testing.T) {
	c := Case{
		Problem: Problem{
			Intent:   "Debug Auth",
			Domain:   "AUTH",
			Keywords: []string{"oauth", "OAuth", "token"},
			Entities: []string{"GitHub"},
		},
		Solution: Solution{
			Approach:   "Investigate the OAuth flow logs",
			ToolsUsed:  []string{"grep"},
			AgentsUsed: []string{"researcher"},
		},
	}
	tokens := CaseTokens(c)
	for _, want := range []string{"oauth", "token", "github", "grep", "researcher", "debug auth", "auth", "investigate", "flow", "logs"} {
		if !contains(tokens, want) {
			t.Errorf("missing token %q in %v", want, tokens)
		}
	}
	// Each token should appear at most once
	seen := make(map[string]int)
	for _, t := range tokens {
		seen[t]++
	}
	for tok, n := range seen {
		if n > 1 {
			t.Errorf("token %q appeared %d times — should be deduped", tok, n)
		}
	}
}

func TestCaseTokens_SkipsShortApproachWords(t *testing.T) {
	c := Case{
		Problem: Problem{Intent: "do thing"},
		Solution: Solution{
			Approach: "we ran an analysis on the data",
		},
	}
	tokens := CaseTokens(c)
	// SD's filter is `length > 2`, so 1-2 char words drop. Three-char
	// words like "the" survive — that matches SD parity (port is
	// length-based, not stoplist-based).
	for _, banned := range []string{"we", "an", "on"} {
		if contains(tokens, banned) {
			t.Errorf("short word %q should not be tokenised", banned)
		}
	}
	if !contains(tokens, "analysis") {
		t.Errorf("'analysis' should be tokenised; tokens=%v", tokens)
	}
}

// ---- CaseBase: Retain, Get, Count, Remove ----

func TestCaseBase_RetainAndCount(t *testing.T) {
	cb := NewCaseBase(nil)
	if cb.Count() != 0 {
		t.Errorf("empty count = %d, want 0", cb.Count())
	}
	cb.Retain(testCase("c1", "auth", "debug auth"))
	cb.Retain(testCase("c2", "research", "find paper"))
	if cb.Count() != 2 {
		t.Errorf("after 2 retains, count = %d, want 2", cb.Count())
	}
}

func TestCaseBase_RetainReplacesOnSameID(t *testing.T) {
	cb := NewCaseBase(nil)
	cb.Retain(testCase("c1", "auth", "debug auth"))
	cb.Retain(testCase("c1", "research", "find paper")) // same ID, different content
	if cb.Count() != 1 {
		t.Errorf("upsert should not duplicate; count = %d", cb.Count())
	}
	got, ok := cb.Get("c1")
	if !ok {
		t.Fatal("c1 missing after upsert")
	}
	if got.Problem.Domain != "research" {
		t.Errorf("upsert didn't replace domain: got %q", got.Problem.Domain)
	}
}

func TestCaseBase_RedactedExcludedFromCount(t *testing.T) {
	cb := NewCaseBase(nil)
	c := testCase("c1", "auth", "debug auth")
	c.Redacted = true
	cb.Retain(c)
	if cb.Count() != 0 {
		t.Errorf("redacted case shouldn't count; got %d", cb.Count())
	}
	if cb.CountIncludingRedacted() != 1 {
		t.Errorf("debug count should include redacted; got %d", cb.CountIncludingRedacted())
	}
}

func TestCaseBase_Remove(t *testing.T) {
	cb := NewCaseBase(nil)
	cb.Retain(testCase("c1", "auth", "debug auth"))
	cb.Retain(testCase("c2", "research", "find paper"))
	cb.Remove("c1")
	if cb.Count() != 1 {
		t.Errorf("after remove, count = %d, want 1", cb.Count())
	}
	if _, ok := cb.Get("c1"); ok {
		t.Errorf("c1 still gettable after Remove")
	}
}

// ---- CaseBase: Retrieve scoring ----

func TestRetrieve_EmptyBaseReturnsNil(t *testing.T) {
	cb := NewCaseBase(nil)
	got := cb.Retrieve(context.Background(), Query{Intent: "x"})
	if got != nil {
		t.Errorf("empty base should return nil; got %+v", got)
	}
}

func TestRetrieve_DomainBoostsExactMatch(t *testing.T) {
	cb := NewCaseBase(nil)
	cb.Retain(testCase("c1", "auth", "debug login"))
	cb.Retain(testCase("c2", "research", "find paper"))
	got := cb.Retrieve(context.Background(), Query{
		Intent: "debug login",
		Domain: "auth",
	})
	if len(got) == 0 {
		t.Fatal("expected results")
	}
	if got[0].Case.ID != "c1" {
		t.Errorf("auth-domain match should rank first; got %q", got[0].Case.ID)
	}
}

func TestRetrieve_RespectsMaxResults(t *testing.T) {
	cb := NewCaseBase(nil)
	for i := 0; i < 10; i++ {
		cb.Retain(testCase("c"+itoa(i), "auth", "task "+itoa(i)))
	}
	got := cb.Retrieve(context.Background(), Query{Domain: "auth", MaxResults: 3})
	if len(got) != 3 {
		t.Errorf("MaxResults=3 → got %d", len(got))
	}
}

func TestRetrieve_DefaultMaxResultsIsFour(t *testing.T) {
	cb := NewCaseBase(nil)
	for i := 0; i < 10; i++ {
		cb.Retain(testCase("c"+itoa(i), "auth", "task "+itoa(i)))
	}
	got := cb.Retrieve(context.Background(), Query{Domain: "auth"}) // MaxResults left at zero
	if len(got) != 4 {
		t.Errorf("default cap K=4 not honored; got %d", len(got))
	}
}

func TestRetrieve_RedactedCasesExcluded(t *testing.T) {
	cb := NewCaseBase(nil)
	c1 := testCase("c1", "auth", "debug login")
	c1.Redacted = true
	cb.Retain(c1)
	cb.Retain(testCase("c2", "auth", "debug login"))
	got := cb.Retrieve(context.Background(), Query{Domain: "auth"})
	for _, s := range got {
		if s.Case.ID == "c1" {
			t.Errorf("redacted case c1 should be excluded; got %+v", got)
		}
	}
}

func TestRetrieve_OrderedByScoreDescending(t *testing.T) {
	cb := NewCaseBase(nil)
	// Two cases, c1 matches all signals, c2 matches less.
	cb.Retain(testCase("c1", "auth", "debug login"))
	weak := testCase("c2", "auth", "research paper")
	cb.Retain(weak)
	got := cb.Retrieve(context.Background(), Query{
		Intent: "debug login", Domain: "auth", Keywords: []string{"debug", "login"},
	})
	if len(got) < 2 {
		t.Fatalf("expected 2 results; got %d", len(got))
	}
	if got[0].Score < got[1].Score {
		t.Errorf("results not sorted descending: %v", got)
	}
}

// ---- CaseBase: Recency ranking ----

func TestRetrieve_RecencyHelpsNewerCases(t *testing.T) {
	cb := NewCaseBase(nil)
	now := time.Now()
	older := testCase("c-old", "auth", "debug")
	older.Timestamp = now.Add(-30 * 24 * time.Hour)
	newer := testCase("c-new", "auth", "debug")
	newer.Timestamp = now
	cb.Retain(older)
	cb.Retain(newer)

	got := cb.Retrieve(context.Background(), Query{Domain: "auth", Intent: "debug"})
	if len(got) < 2 {
		t.Fatalf("expected 2 results; got %d", len(got))
	}
	if got[0].Case.ID != "c-new" {
		t.Errorf("newer case should rank first; got %q", got[0].Case.ID)
	}
}

// ---- CaseBase: Embedding signal ----

// fakeEmbedderOnText returns a vector matching the literal map, errors otherwise.
type fakeEmbedderOnText struct {
	dim     int
	mapping map[string][]float32
	id      string
}

func (f *fakeEmbedderOnText) Embed(_ context.Context, text string) ([]float32, error) {
	if v, ok := f.mapping[text]; ok {
		return v, nil
	}
	return make([]float32, f.dim), nil
}
func (f *fakeEmbedderOnText) Dimensions() int { return f.dim }
func (f *fakeEmbedderOnText) ID() string      { return f.id }
func (f *fakeEmbedderOnText) Close() error    { return nil }

func TestRetrieve_EmbeddingSignalContributes(t *testing.T) {
	// Set up two cases with equal field/index signals but different
	// embeddings; the one with the closer vector should win.
	dim := 4
	closeVec := []float32{1, 0, 0, 0}
	farVec := []float32{0, 1, 0, 0}

	cb := NewCaseBase(embed.NewMock(dim))
	c1 := testCase("c1", "auth", "task")
	c1.Embedding = closeVec
	c1.EmbedderID = "test/v1"
	cb.Retain(c1)

	c2 := testCase("c2", "auth", "task")
	c2.Embedding = farVec
	c2.EmbedderID = "test/v1"
	cb.Retain(c2)

	// Use a fake embedder so we control what the query embeds to.
	cb.embedder = &fakeEmbedderOnText{
		dim:     dim,
		mapping: map[string][]float32{"task auth ": closeVec, "task auth": closeVec},
		id:      "test/v1",
	}

	got := cb.Retrieve(context.Background(), Query{Intent: "task", Domain: "auth"})
	if len(got) < 2 {
		t.Fatalf("expected 2 results; got %d", len(got))
	}
	if got[0].Case.ID != "c1" {
		t.Errorf("closer embedding should rank first; got %q score=%g vs %q score=%g",
			got[0].Case.ID, got[0].Score, got[1].Case.ID, got[1].Score)
	}
}

func TestRetrieve_NoEmbedderRenormalizesWeights(t *testing.T) {
	// With no embedder, the embedding signal drops to zero and the
	// other 5 weights renormalise. Verify retrieval still works.
	cb := NewCaseBase(nil)
	cb.Retain(testCase("c1", "auth", "task"))
	got := cb.Retrieve(context.Background(), Query{Intent: "task", Domain: "auth"})
	if len(got) != 1 {
		t.Fatalf("expected 1 result; got %d", len(got))
	}
	if got[0].Score == 0 {
		t.Error("score should be non-zero when other signals match")
	}
	if got[0].Score > 1.0+1e-9 {
		t.Errorf("score = %g, should be ≤ 1.0 after renormalisation", got[0].Score)
	}
}

// ---- effectiveWeights renormalization ----

func TestEffectiveWeights_PassesThroughWhenEmbeddingsAvailable(t *testing.T) {
	w := DefaultWeights()
	got := effectiveWeights(w, true)
	if got != w {
		t.Errorf("with embeddings, weights should pass through; got %+v want %+v", got, w)
	}
}

func TestEffectiveWeights_RenormalizesOnEmbeddingsAbsent(t *testing.T) {
	w := DefaultWeights()
	got := effectiveWeights(w, false)
	if got.Embedding != 0 {
		t.Errorf("embedding weight should be 0 when absent; got %g", got.Embedding)
	}
	sum := got.Field + got.Index + got.Recency + got.Domain + got.Utility
	original := w.Field + w.Index + w.Recency + w.Domain + w.Embedding + w.Utility
	if math.Abs(sum-original) > 1e-9 {
		t.Errorf("renormalised sum = %g, should equal original %g", sum, original)
	}
}

func TestEffectiveWeights_DegenerateAllZeroFalsesBackToEven(t *testing.T) {
	zero := Weights{}
	got := effectiveWeights(zero, false)
	// Should be 0.2 across the five active signals.
	for _, v := range []float64{got.Field, got.Index, got.Recency, got.Domain, got.Utility} {
		if math.Abs(v-0.2) > 1e-9 {
			t.Errorf("degenerate fallback expected 0.2, got %g", v)
		}
	}
}

// ---- helpers ----

func testCase(id, domain, intent string) Case {
	return Case{
		ID:            id,
		Timestamp:     time.Now(),
		SchemaVersion: SchemaVersion,
		Problem: Problem{
			Intent:   intent,
			Domain:   domain,
			Keywords: []string{intent, domain},
		},
		Solution: Solution{Approach: "approach for " + intent},
		Outcome:  Outcome{Status: StatusSuccess, Confidence: 0.9},
	}
}

func contains(slice []string, want string) bool {
	for _, s := range slice {
		if s == want {
			return true
		}
	}
	return false
}

// itoa is a stdlib alias to keep the test file's import block tight.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
