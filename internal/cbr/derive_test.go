package cbr

import (
	"reflect"
	"testing"
	"time"
)

// ---- Classify ----

func TestClassify_FailureWithPitfallsIsPitfall(t *testing.T) {
	got := Classify(Problem{}, Solution{}, Outcome{
		Status:   StatusFailure,
		Pitfalls: []string{"don't retry on 500"},
	})
	if got != CategoryPitfall {
		t.Errorf("got %q, want %q", got, CategoryPitfall)
	}
}

func TestClassify_FailureNoPitfallsIsTroubleshooting(t *testing.T) {
	// SD's rule: failure with no pitfalls listed = Troubleshooting
	// (we know it didn't work but haven't extracted lessons yet).
	got := Classify(Problem{}, Solution{}, Outcome{Status: StatusFailure})
	if got != CategoryTroubleshooting {
		t.Errorf("got %q, want troubleshooting", got)
	}
}

func TestClassify_PartialIsDomainKnowledge(t *testing.T) {
	got := Classify(Problem{}, Solution{}, Outcome{Status: StatusPartial})
	if got != CategoryDomainKnowledge {
		t.Errorf("got %q, want domain_knowledge", got)
	}
}

func TestClassify_ConversationGetsNoCategory(t *testing.T) {
	// Load-bearing: Conversation cycles produce cases for audit
	// but should NOT surface as a pattern in CBR retrieval.
	for _, status := range []Status{StatusSuccess, StatusPartial, StatusFailure} {
		got := Classify(
			Problem{IntentClass: IntentConversation, Intent: "operator greeted me"},
			Solution{Approach: "Hello back."},
			Outcome{Status: status, Pitfalls: []string{"x"}},
		)
		if got != "" {
			t.Errorf("Conversation cycle (status=%s) should have empty category; got %q", status, got)
		}
	}
}

func TestClassify_CodePatternMarkers(t *testing.T) {
	cases := []struct {
		approach string
		want     Category
	}{
		{"wrote a function to parse the input", CategoryCodePattern},
		{"crafted a regex that handles edge cases", CategoryCodePattern},
		{"snippet for sql query batching", CategoryCodePattern},
		{"updated foo.go and bar.ts", CategoryCodePattern},
	}
	for _, tc := range cases {
		got := Classify(Problem{}, Solution{Approach: tc.approach}, Outcome{Status: StatusSuccess})
		if got != tc.want {
			t.Errorf("approach=%q: got %q, want %q", tc.approach, got, tc.want)
		}
	}
}

func TestClassify_TroubleshootingVerbs(t *testing.T) {
	for _, intent := range []string{"debug auth flow", "fix the leak", "diagnose the timeout", "investigate cycle errors", "trace the request"} {
		got := Classify(Problem{Intent: intent}, Solution{Approach: "looked into it"}, Outcome{Status: StatusSuccess})
		if got != CategoryTroubleshooting {
			t.Errorf("intent=%q: got %q, want troubleshooting", intent, got)
		}
	}
}

func TestClassify_StrategyWhenAgentsOrToolsUsed(t *testing.T) {
	got := Classify(
		Problem{Intent: "summarise paper"},
		Solution{Approach: "delegated to researcher", AgentsUsed: []string{"researcher"}},
		Outcome{Status: StatusSuccess},
	)
	if got != CategoryStrategy {
		t.Errorf("got %q, want strategy", got)
	}

	got = Classify(
		Problem{Intent: "research"},
		Solution{Approach: "fetched URL", ToolsUsed: []string{"jina_reader"}},
		Outcome{Status: StatusSuccess},
	)
	if got != CategoryStrategy {
		t.Errorf("with tools: got %q, want strategy", got)
	}
}

func TestClassify_FallbackDomainKnowledge(t *testing.T) {
	got := Classify(
		Problem{Intent: "explain how http works"},
		Solution{Approach: "summarised the protocol"},
		Outcome{Status: StatusSuccess},
	)
	if got != CategoryDomainKnowledge {
		t.Errorf("got %q, want domain_knowledge", got)
	}
}

// ---- DeriveProblem ----

func TestDeriveProblem_TrimsAndCaps(t *testing.T) {
	long := make([]byte, 1000)
	for i := range long {
		long[i] = 'a'
	}
	p := DeriveProblem(string(long))
	if len(p.UserInput) != 600 {
		t.Errorf("UserInput len = %d, want 600 (cap)", len(p.UserInput))
	}
}

func TestDeriveProblem_FirstSentenceAsIntent(t *testing.T) {
	p := DeriveProblem("Debug the auth flow. There's a token issue lingering.")
	if p.Intent != "debug the auth flow" {
		t.Errorf("Intent = %q, want %q", p.Intent, "debug the auth flow")
	}
}

func TestDeriveProblem_KeywordsExtracted(t *testing.T) {
	p := DeriveProblem("Investigate timeout errors in the auth service")
	for _, want := range []string{"investigate", "timeout", "errors", "auth", "service"} {
		if !contains(p.Keywords, want) {
			t.Errorf("missing keyword %q in %v", want, p.Keywords)
		}
	}
	for _, banned := range []string{"the", "in"} {
		if contains(p.Keywords, banned) {
			t.Errorf("stopword/short %q should not appear in %v", banned, p.Keywords)
		}
	}
}

func TestDeriveProblem_RespectsKeywordLimit(t *testing.T) {
	long := "alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu"
	p := DeriveProblem(long)
	if len(p.Keywords) > 8 {
		t.Errorf("keywords cap exceeded; got %d", len(p.Keywords))
	}
}

// ---- DeriveSolution ----

func TestDeriveSolution_DedupesAgentsAndTools(t *testing.T) {
	s := DeriveSolution("Did the work.", []string{"researcher", "researcher", "observer"}, []string{"grep", "", "grep"})
	if !reflect.DeepEqual(s.AgentsUsed, []string{"researcher", "observer"}) {
		t.Errorf("agents = %v", s.AgentsUsed)
	}
	if !reflect.DeepEqual(s.ToolsUsed, []string{"grep"}) {
		t.Errorf("tools = %v", s.ToolsUsed)
	}
}

// ---- DeriveOutcome ----

func TestDeriveOutcome_SuccessHasHigherConfidence(t *testing.T) {
	pass := DeriveOutcome(true, "delivered cleanly")
	fail := DeriveOutcome(false, "tool errored")
	if pass.Status != StatusSuccess || fail.Status != StatusFailure {
		t.Errorf("status mismatch")
	}
	if !(pass.Confidence > fail.Confidence) {
		t.Errorf("success conf %g should beat failure %g", pass.Confidence, fail.Confidence)
	}
}

// ---- NewCase ----

func TestNewCase_AssemblesAllFields(t *testing.T) {
	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	// Intent deliberately doesn't match a troubleshooting verb so
	// the classifier falls through to CategoryStrategy (agents used).
	c := NewCase(
		"cycle-1",
		Problem{Intent: "summarise paper", Domain: "research"},
		Solution{Approach: "delegated to specialist", AgentsUsed: []string{"researcher"}},
		Outcome{Status: StatusSuccess, Confidence: 0.9},
		[]float32{0.1, 0.2},
		"hugot/test/v1",
		now,
	)
	if c.ID == "" {
		t.Error("case ID empty")
	}
	if c.SourceNarrativeID != "cycle-1" {
		t.Error("source narrative ID not stamped")
	}
	if c.Category != CategoryStrategy {
		t.Errorf("category = %q, want strategy", c.Category)
	}
	if !c.Timestamp.Equal(now) {
		t.Error("timestamp not stamped")
	}
	if c.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %d", c.SchemaVersion)
	}
}
