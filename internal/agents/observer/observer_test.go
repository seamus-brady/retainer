package observer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/seamus-brady/retainer/internal/agent"
	"github.com/seamus-brady/retainer/internal/llm"
)

type stubProvider struct{}

func (*stubProvider) Name() string { return "stub" }
func (*stubProvider) ChatStructured(context.Context, llm.Request, llm.Schema, any) (llm.Usage, error) {
	return llm.Usage{}, errors.New("not used")
}
func (*stubProvider) Chat(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{Content: []llm.ContentBlock{llm.TextBlock{Text: "ok"}}, StopReason: "end_turn"}, nil
}

type stubDispatcher struct{}

func (stubDispatcher) List() []llm.Tool { return nil }
func (stubDispatcher) Dispatch(context.Context, string, []byte) (string, error) {
	return "", nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNew_BuildsObserverSpec(t *testing.T) {
	a, err := New(&stubProvider{}, "model-x", stubDispatcher{}, agent.Telemetry{}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if a.Name() != Name {
		t.Errorf("name = %q, want %q", a.Name(), Name)
	}
	if a.HumanName() != HumanName {
		t.Errorf("human name = %q", a.HumanName())
	}
	if a.Description() != Description {
		t.Errorf("description = %q", a.Description())
	}
}

func TestSpec_Constants_PostFold(t *testing.T) {
	// SD parity for observer was MaxTurns=6, MaxTokens=2048. With
	// the remembrancer fold (2026-05-07) observer absorbed the
	// deep-archive surface; budget bumped to MaxTurns=8 +
	// MaxTokens=4096 to cover synthesis-shaped flows
	// (consolidation, mining) that previously lived on the
	// remembrancer.
	if MaxTurns != 8 {
		t.Errorf("MaxTurns = %d, want 8 (post-fold)", MaxTurns)
	}
	if MaxTokens != 4096 {
		t.Errorf("MaxTokens = %d, want 4096 (post-fold)", MaxTokens)
	}
	if MaxConsecutiveErrors != 3 {
		t.Errorf("MaxConsecutiveErrors = %d", MaxConsecutiveErrors)
	}
}

func TestSystemPrompt_MentionsExpectedTools(t *testing.T) {
	for _, want := range []string{"inspect_cycle", "recall_recent", "get_fact"} {
		if !strings.Contains(systemPrompt, want) {
			t.Errorf("system prompt missing tool reference: %q", want)
		}
	}
}

func TestSystemPrompt_DefersUnportedTools(t *testing.T) {
	// Defer markers from observer.gleam — these tools depend on
	// subsystems we haven't shipped. They MUST NOT appear in the
	// prompt or the agent will hallucinate calling tools that
	// don't exist.
	//
	// Note: recall_cases + the case-curation tools (correct_case,
	// annotate_case, suppress_case, boost_case) shipped with the
	// CBR work and are listed in the prompt today. The
	// remembrancer fold (2026-05-07) added the deep-archive
	// surface; those tools are also in the prompt now. The list
	// below covers what's still genuinely deferred.
	for _, deferred := range []string{
		"recall_threads",
		"report_false_positive", "review_learning_goals",
		"detect_patterns", "query_tool_activity", "memory_trace_fact",
	} {
		if strings.Contains(systemPrompt, deferred) {
			t.Errorf("system prompt mentions deferred tool %q", deferred)
		}
	}
}

func TestSystemPrompt_FramesAsIntrospectionAgent(t *testing.T) {
	// The Observer's distinguishing feature vs the Researcher is
	// "look inward not outward." Make sure the prompt says so.
	if !strings.Contains(systemPrompt, "introspect") {
		t.Error("prompt should frame the agent as an introspection agent")
	}
}

func TestNew_RejectsNilTools(t *testing.T) {
	_, err := New(&stubProvider{}, "x", nil, agent.Telemetry{}, discardLogger())
	if err == nil {
		t.Fatal("expected error when Tools is nil")
	}
}
