package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/seamus-brady/retainer/internal/cyclelog"
	"github.com/seamus-brady/retainer/internal/librarian"
	"github.com/seamus-brady/retainer/internal/llm"
)

// memoryQueryFactsLimit caps how many facts memory_query_facts returns
// per call. Mirrors Springdrift's behaviour of "show enough to be
// useful, not enough to flood the prompt."
const memoryQueryFactsLimit = 10

// LibrarianFactStore is the slice of *librarian.Librarian the memory
// tools need. Letting handlers depend on this interface keeps tests
// independent of the librarian's SQLite/JSONL backing.
type LibrarianFactStore interface {
	RecordFact(librarian.Fact)
	GetFact(key string) *librarian.Fact
	ClearFact(key, sourceCycleID string)
	SearchFacts(keyword string, limit int) []librarian.Fact
}

// scopeProperty is shared across the write tool and parse_scope so the
// enum lives in one place.
var scopeProperty = llm.Property{
	Type:        "string",
	Description: "Memory scope: persistent (workspace, survives restarts), session (current run only), ephemeral (cleared at cycle end)",
	Enum:        []string{"persistent", "session", "ephemeral"},
}

// ---------------------------------------------------------------------------
// memory_write
// ---------------------------------------------------------------------------

type memoryWriteInput struct {
	Key        string  `json:"key"`
	Value      string  `json:"value"`
	Scope      string  `json:"scope"`
	Confidence float64 `json:"confidence"`
}

// MemoryWrite stores a single fact. The cog cycle ID is read from the
// dispatch context (set by cog before invoking the dispatcher) so the
// fact's provenance points back to the cycle that wrote it.
type MemoryWrite struct{ Lib LibrarianFactStore }

func (MemoryWrite) Tool() llm.Tool {
	return llm.Tool{
		Name: "memory_write",
		Description: "Store a fact in memory. Persistent facts survive session restarts. " +
			"Session facts are cleared when the session ends. " +
			"Ephemeral facts are cleared at the end of the current cycle.",
		InputSchema: llm.Schema{
			Name: "memory_write",
			Properties: map[string]llm.Property{
				"key": {
					Type:        "string",
					Description: "Unique key for the fact (e.g. 'dublin_rent', 'user_preference_format')",
				},
				"value":      {Type: "string", Description: "The fact value to store"},
				"scope":      scopeProperty,
				"confidence": {Type: "number", Description: "Confidence in the fact (0.0 to 1.0)"},
			},
			Required: []string{"key", "value", "scope", "confidence"},
		},
	}
}

func (m MemoryWrite) Execute(ctx context.Context, input []byte) (string, error) {
	if len(input) == 0 {
		return "", fmt.Errorf("memory_write: empty input")
	}
	var in memoryWriteInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("memory_write: decode input: %w", err)
	}
	key := strings.TrimSpace(in.Key)
	if key == "" {
		return "", fmt.Errorf("memory_write: key must not be empty")
	}
	confidence := clamp01(in.Confidence)
	scope := parseScope(in.Scope)

	m.Lib.RecordFact(librarian.Fact{
		Key:           key,
		Value:         in.Value,
		Scope:         scope,
		Operation:     librarian.FactOperationWrite,
		Confidence:    confidence,
		SourceCycleID: cyclelog.CycleIDFromContext(ctx),
	})

	return fmt.Sprintf("Stored fact %q = %q (scope: %s, confidence: %.2f)",
		key, in.Value, string(scope), confidence), nil
}

// ---------------------------------------------------------------------------
// memory_read
// ---------------------------------------------------------------------------

type memoryReadInput struct {
	Key string `json:"key"`
}

type MemoryRead struct{ Lib LibrarianFactStore }

func (MemoryRead) Tool() llm.Tool {
	return llm.Tool{
		Name:        "memory_read",
		Description: "Read the current value of a fact by key. Returns the latest non-superseded value.",
		InputSchema: llm.Schema{
			Name: "memory_read",
			Properties: map[string]llm.Property{
				"key": {Type: "string", Description: "The key to look up"},
			},
			Required: []string{"key"},
		},
	}
}

func (m MemoryRead) Execute(_ context.Context, input []byte) (string, error) {
	in, err := decodeKeyInput(input, "memory_read")
	if err != nil {
		return "", err
	}
	f := m.Lib.GetFact(in.Key)
	if f == nil {
		return fmt.Sprintf("No fact found for key %q", in.Key), nil
	}
	return formatFact(*f), nil
}

// ---------------------------------------------------------------------------
// memory_clear_key
// ---------------------------------------------------------------------------

type MemoryClearKey struct{ Lib LibrarianFactStore }

func (MemoryClearKey) Tool() llm.Tool {
	return llm.Tool{
		Name:        "memory_clear_key",
		Description: "Remove a fact from memory by key. The fact history is preserved for auditing.",
		InputSchema: llm.Schema{
			Name: "memory_clear_key",
			Properties: map[string]llm.Property{
				"key": {Type: "string", Description: "The key to clear"},
			},
			Required: []string{"key"},
		},
	}
}

func (m MemoryClearKey) Execute(ctx context.Context, input []byte) (string, error) {
	in, err := decodeKeyInput(input, "memory_clear_key")
	if err != nil {
		return "", err
	}
	m.Lib.ClearFact(in.Key, cyclelog.CycleIDFromContext(ctx))
	return fmt.Sprintf("Cleared fact for key %q", in.Key), nil
}

// ---------------------------------------------------------------------------
// memory_query_facts
// ---------------------------------------------------------------------------

type memoryQueryInput struct {
	Keyword string `json:"keyword"`
}

type MemoryQueryFacts struct{ Lib LibrarianFactStore }

func (MemoryQueryFacts) Tool() llm.Tool {
	return llm.Tool{
		Name:        "memory_query_facts",
		Description: "Search memory for facts matching a keyword. Searches both keys and values (case-insensitive substring match).",
		InputSchema: llm.Schema{
			Name: "memory_query_facts",
			Properties: map[string]llm.Property{
				"keyword": {Type: "string", Description: "Search term to match against fact keys and values"},
			},
			Required: []string{"keyword"},
		},
	}
}

func (m MemoryQueryFacts) Execute(_ context.Context, input []byte) (string, error) {
	if len(input) == 0 {
		return "", fmt.Errorf("memory_query_facts: empty input")
	}
	var in memoryQueryInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("memory_query_facts: decode input: %w", err)
	}
	keyword := strings.TrimSpace(in.Keyword)
	if keyword == "" {
		return "", fmt.Errorf("memory_query_facts: keyword must not be empty")
	}
	facts := m.Lib.SearchFacts(keyword, memoryQueryFactsLimit)
	if len(facts) == 0 {
		return fmt.Sprintf("No facts found matching %q", keyword), nil
	}
	return formatFactsList(facts), nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// parseScope mirrors Springdrift: unknown values fall back to Session
// rather than erroring, so a hallucinated scope doesn't break the call.
func parseScope(s string) librarian.FactScope {
	switch s {
	case "persistent":
		return librarian.FactScopePersistent
	case "ephemeral":
		return librarian.FactScopeEphemeral
	default:
		return librarian.FactScopeSession
	}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func decodeKeyInput(input []byte, name string) (memoryReadInput, error) {
	var in memoryReadInput
	if len(input) == 0 {
		return in, fmt.Errorf("%s: empty input", name)
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return in, fmt.Errorf("%s: decode input: %w", name, err)
	}
	in.Key = strings.TrimSpace(in.Key)
	if in.Key == "" {
		return in, fmt.Errorf("%s: key must not be empty", name)
	}
	return in, nil
}

func formatFact(f librarian.Fact) string {
	return fmt.Sprintf("key: %s\nvalue: %s\nscope: %s\nconfidence: %.2f\ntimestamp: %s",
		f.Key, f.Value, string(f.Scope), f.Confidence, f.Timestamp.Format("2006-01-02T15:04:05Z07:00"))
}

func formatFactsList(facts []librarian.Fact) string {
	var b strings.Builder
	for i, f := range facts {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s = %s (confidence: %.2f)", f.Key, f.Value, f.Confidence)
	}
	return b.String()
}
