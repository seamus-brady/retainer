package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/seamus-brady/retainer/internal/cbr"
	"github.com/seamus-brady/retainer/internal/llm"
)

// CBRService is the slice of *librarian.Librarian the observer's CBR
// tools need. Lets tests substitute a fake without spinning up a
// real librarian.
type CBRService interface {
	RetrieveCases(ctx context.Context, q cbr.Query) []cbr.Scored
	GetCase(id string) (cbr.Case, bool)
	SuppressCase(id string) (cbr.Case, error)
	UnsuppressCase(id string) (cbr.Case, error)
	BoostCase(id string, delta float64) (cbr.Case, error)
	AnnotateCase(id, pitfall string) (cbr.Case, error)
	CorrectCase(id string, fields cbr.Case) (cbr.Case, error)
}

// recallCasesDefaultLimit is the K cap when the agent doesn't specify
// — matches cbr.DefaultMaxResults (4 per Memento paper).
const recallCasesDefaultLimit = cbr.DefaultMaxResults

// recallCasesMaxLimit caps the upper bound. Above this, the agent's
// context window starts paying for noise — operator can override via
// future config but the tool refuses absurd values upfront.
const recallCasesMaxLimit = 8

// ---------------------------------------------------------------------------
// recall_cases
// ---------------------------------------------------------------------------

type recallCasesInput struct {
	Intent     string   `json:"intent,omitempty"`
	Domain     string   `json:"domain,omitempty"`
	Keywords   []string `json:"keywords,omitempty"`
	Entities   []string `json:"entities,omitempty"`
	MaxResults int      `json:"max_results,omitempty"`
}

// ObserverRecallCases is the agent-facing CBR retrieval tool. The
// model passes a query (intent / domain / keywords / entities) and
// gets back the top-K scored cases with their problem / solution /
// outcome surfaces.
type ObserverRecallCases struct{ Lib CBRService }

func (ObserverRecallCases) Tool() llm.Tool {
	return llm.Tool{
		Name: "recall_cases",
		Description: "Find similar past cases by intent, domain, keywords, or entities. Returns up to " +
			"max_results cases (default 4, max 8) ranked by 6-signal weighted similarity. Each case " +
			"includes problem, solution, outcome and category. Use when you want to know how a similar " +
			"problem was handled before — strategies that worked, pitfalls to avoid.",
		InputSchema: llm.Schema{
			Name: "recall_cases",
			Properties: map[string]llm.Property{
				"intent": {
					Type:        "string",
					Description: "Short verb-phrase describing the goal (e.g. 'debug auth', 'summarise paper').",
				},
				"domain": {
					Type:        "string",
					Description: "Subject area (e.g. 'auth', 'research', 'scheduler').",
				},
				"keywords": {
					Type:        "array",
					Description: "Salient non-entity terms that describe the problem.",
				},
				"entities": {
					Type:        "array",
					Description: "Named things involved (people, repos, files, services).",
				},
				"max_results": {
					Type:        "integer",
					Description: fmt.Sprintf("Max cases to return (1–%d, default %d).", recallCasesMaxLimit, recallCasesDefaultLimit),
				},
			},
			Required: []string{},
		},
	}
}

func (h ObserverRecallCases) Execute(ctx context.Context, input []byte) (string, error) {
	var in recallCasesInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return "", fmt.Errorf("recall_cases: decode input: %w", err)
		}
	}
	if in.Intent == "" && in.Domain == "" && len(in.Keywords) == 0 && len(in.Entities) == 0 {
		return "", fmt.Errorf("recall_cases: at least one of intent / domain / keywords / entities is required")
	}
	limit := in.MaxResults
	if limit <= 0 {
		limit = recallCasesDefaultLimit
	}
	if limit > recallCasesMaxLimit {
		limit = recallCasesMaxLimit
	}

	scored := h.Lib.RetrieveCases(ctx, cbr.Query{
		Intent:     in.Intent,
		Domain:     in.Domain,
		Keywords:   in.Keywords,
		Entities:   in.Entities,
		MaxResults: limit,
	})
	if len(scored) == 0 {
		return "no similar cases found", nil
	}
	return formatScoredCases(scored), nil
}

// formatScoredCases renders an LLM-friendly multi-line summary of K
// retrieved cases. Each case shows: short ID, score, category,
// problem / solution / outcome highlights. Kept compact so the
// agent's context budget isn't blown on N case-dumps.
func formatScoredCases(scored []cbr.Scored) string {
	var b strings.Builder
	for i, s := range scored {
		c := s.Case
		fmt.Fprintf(&b, "[%d] case=%s score=%.3f category=%s status=%s confidence=%.2f\n",
			i+1, shortCycleID(c.ID), s.Score, c.Category, c.Outcome.Status, c.Outcome.Confidence)
		if c.Problem.Intent != "" {
			fmt.Fprintf(&b, "    problem.intent: %s\n", c.Problem.Intent)
		}
		if c.Problem.Domain != "" {
			fmt.Fprintf(&b, "    problem.domain: %s\n", c.Problem.Domain)
		}
		if len(c.Problem.Keywords) > 0 {
			fmt.Fprintf(&b, "    problem.keywords: %s\n", strings.Join(c.Problem.Keywords, ", "))
		}
		if c.Solution.Approach != "" {
			fmt.Fprintf(&b, "    solution: %s\n", truncateInline(c.Solution.Approach, 240))
		}
		if c.Outcome.Assessment != "" {
			fmt.Fprintf(&b, "    assessment: %s\n", truncateInline(c.Outcome.Assessment, 240))
		}
		if len(c.Outcome.Pitfalls) > 0 {
			fmt.Fprintf(&b, "    pitfalls: %s\n", strings.Join(c.Outcome.Pitfalls, "; "))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// ---------------------------------------------------------------------------
// case_curate (action-discriminated CBR mutation)
// ---------------------------------------------------------------------------
//
// One tool replaces five (suppress / unsuppress / boost / annotate /
// correct). Mirrors the strategy_curate pattern from PR #58. The
// underlying librarian methods are unchanged — only the LLM-facing
// surface is consolidated.

// caseCurateAction is the discriminator. Names match the prior tool
// names so audit trails (logs, persona prose, integration tests) read
// the same.
const (
	caseCurateActionSuppress   = "suppress"
	caseCurateActionUnsuppress = "unsuppress"
	caseCurateActionBoost      = "boost"
	caseCurateActionAnnotate   = "annotate"
	caseCurateActionCorrect    = "correct"
)

type caseCurateInput struct {
	Action string `json:"action"`
	CaseID string `json:"case_id"`

	// boost
	Delta float64 `json:"delta,omitempty"`

	// annotate
	Pitfall string `json:"pitfall,omitempty"`

	// correct
	Category   string  `json:"category,omitempty"`
	Assessment string  `json:"assessment,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
}

// ObserverCaseCurate is the consolidated CBR mutation tool. The
// `action` field selects which librarian method runs. Per-action
// fields are optional and validated only when their action fires —
// keeps the schema small while preserving every prior tool's exact
// validation rules.
type ObserverCaseCurate struct{ Lib CBRService }

func (ObserverCaseCurate) Tool() llm.Tool {
	return llm.Tool{
		Name: "case_curate",
		Description: "Mutate one CBR case. The `action` discriminator selects the operation: " +
			"`suppress` (exclude from retrieval), `unsuppress` (restore), `boost` (adjust outcome " +
			"confidence by signed delta), `annotate` (append a pitfall), `correct` (update " +
			"category / assessment / confidence). The on-disk JSONL archive is preserved — every " +
			"action appends a new record that supersedes. Use when an operator wants to fix " +
			"misclassified or misleading cases.",
		InputSchema: llm.Schema{
			Name: "case_curate",
			Properties: map[string]llm.Property{
				"action": {
					Type: "string",
					Description: "Which mutation to perform: suppress | unsuppress | boost | annotate | correct.",
					Enum: []string{
						caseCurateActionSuppress,
						caseCurateActionUnsuppress,
						caseCurateActionBoost,
						caseCurateActionAnnotate,
						caseCurateActionCorrect,
					},
				},
				"case_id": {Type: "string", Description: "The case ID to mutate."},
				"delta": {
					Type:        "number",
					Description: "boost: signed delta in [-1.0, 1.0]; final confidence clamped to [0, 1].",
				},
				"pitfall": {
					Type:        "string",
					Description: "annotate: short watch-out sentence appended to the case's outcome pitfalls.",
				},
				"category": {
					Type: "string",
					Description: "correct: new category. One of: strategy, code_pattern, troubleshooting, " +
						"pitfall, domain_knowledge.",
				},
				"assessment": {Type: "string", Description: "correct: new assessment text."},
				"confidence": {
					Type:        "number",
					Description: "correct: new outcome confidence in [0.0, 1.0].",
				},
			},
			Required: []string{"action", "case_id"},
		},
	}
}

func (h ObserverCaseCurate) Execute(_ context.Context, input []byte) (string, error) {
	if len(input) == 0 {
		return "", fmt.Errorf("case_curate: empty input")
	}
	// Decode twice so we can detect whether `confidence` was actually
	// present (zero-value 0.0 is a legitimate confidence). Cheap; the
	// payload is small. Mirrors the prior correct_case behaviour.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(input, &raw); err != nil {
		return "", fmt.Errorf("case_curate: decode input: %w", err)
	}
	var in caseCurateInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("case_curate: decode input: %w", err)
	}
	id := strings.TrimSpace(in.CaseID)
	if id == "" {
		return "", fmt.Errorf("case_curate: case_id must not be empty")
	}

	switch strings.TrimSpace(in.Action) {
	case caseCurateActionSuppress:
		return h.suppress(id)
	case caseCurateActionUnsuppress:
		return h.unsuppress(id)
	case caseCurateActionBoost:
		return h.boost(id, in.Delta)
	case caseCurateActionAnnotate:
		return h.annotate(id, in.Pitfall)
	case caseCurateActionCorrect:
		_, hasConfidence := raw["confidence"]
		return h.correct(id, in, hasConfidence)
	case "":
		return "", fmt.Errorf("case_curate: action must not be empty")
	default:
		return "", fmt.Errorf("case_curate: unknown action %q (allowed: suppress, unsuppress, boost, annotate, correct)", in.Action)
	}
}

func (h ObserverCaseCurate) suppress(id string) (string, error) {
	updated, err := h.Lib.SuppressCase(id)
	if err != nil {
		return "", fmt.Errorf("case_curate suppress: %w", err)
	}
	return fmt.Sprintf("suppressed case %s (redacted=true)", shortCycleID(updated.ID)), nil
}

func (h ObserverCaseCurate) unsuppress(id string) (string, error) {
	updated, err := h.Lib.UnsuppressCase(id)
	if err != nil {
		return "", fmt.Errorf("case_curate unsuppress: %w", err)
	}
	return fmt.Sprintf("unsuppressed case %s (redacted=false)", shortCycleID(updated.ID)), nil
}

func (h ObserverCaseCurate) boost(id string, delta float64) (string, error) {
	if delta < -1.0 || delta > 1.0 {
		return "", fmt.Errorf("case_curate boost: delta %g out of range [-1.0, 1.0]", delta)
	}
	updated, err := h.Lib.BoostCase(id, delta)
	if err != nil {
		return "", fmt.Errorf("case_curate boost: %w", err)
	}
	return fmt.Sprintf("boosted case %s confidence=%.2f (delta %+.2f)",
		shortCycleID(updated.ID), updated.Outcome.Confidence, delta), nil
}

func (h ObserverCaseCurate) annotate(id, pitfall string) (string, error) {
	pitfall = strings.TrimSpace(pitfall)
	if pitfall == "" {
		return "", fmt.Errorf("case_curate annotate: pitfall must not be empty")
	}
	updated, err := h.Lib.AnnotateCase(id, pitfall)
	if err != nil {
		return "", fmt.Errorf("case_curate annotate: %w", err)
	}
	return fmt.Sprintf("annotated case %s — %d total pitfalls",
		shortCycleID(updated.ID), len(updated.Outcome.Pitfalls)), nil
}

func (h ObserverCaseCurate) correct(id string, in caseCurateInput, hasConfidence bool) (string, error) {
	current, ok := h.Lib.GetCase(id)
	if !ok {
		return "", fmt.Errorf("case_curate correct: case %q not found", id)
	}

	fields := cbr.Case{
		Problem:  current.Problem,
		Solution: current.Solution,
		Outcome:  current.Outcome,
	}
	if cat := strings.TrimSpace(in.Category); cat != "" {
		switch cbr.Category(cat) {
		case cbr.CategoryStrategy, cbr.CategoryCodePattern, cbr.CategoryTroubleshooting,
			cbr.CategoryPitfall, cbr.CategoryDomainKnowledge:
			fields.Category = cbr.Category(cat)
		default:
			return "", fmt.Errorf("case_curate correct: unknown category %q (allowed: strategy, code_pattern, troubleshooting, pitfall, domain_knowledge)", cat)
		}
	}
	if assess := strings.TrimSpace(in.Assessment); assess != "" {
		fields.Outcome.Assessment = assess
	}
	if hasConfidence {
		if in.Confidence < 0 || in.Confidence > 1 {
			return "", fmt.Errorf("case_curate correct: confidence %g out of range [0.0, 1.0]", in.Confidence)
		}
		fields.Outcome.Confidence = in.Confidence
	}

	updated, err := h.Lib.CorrectCase(id, fields)
	if err != nil {
		return "", fmt.Errorf("case_curate correct: %w", err)
	}
	return fmt.Sprintf("corrected case %s — category=%s confidence=%.2f",
		shortCycleID(updated.ID), updated.Category, updated.Outcome.Confidence), nil
}
