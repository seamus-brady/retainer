package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// ScriptedMock is a Mock-compatible Provider that returns a sequence
// of pre-recorded responses, one per Chat call. Used by integration
// tests + the `retainer send --mock-script` CLI to drive
// deterministic tool-use sequences without hitting a real provider.
//
// Script semantics:
//
//   - Responses pop in order. The 1st Chat call returns scripts[0],
//     the 2nd returns scripts[1], etc.
//   - When the queue runs dry, returns an error so over-run isn't
//     silently masked (test expected N turns; got N+1 means the
//     script is incomplete or the cog looped further than intended).
//   - Each script entry produces either a text reply (TextBlock) or
//     a sequence of tool_use blocks (ToolUseBlock). Mixing is
//     supported — text + tool_use in the same response is what
//     Anthropic emits when the model says "I'll use this tool with
//     X intent" before the tool_use.
//
// ChatStructured isn't scripted today — integration tests use Chat
// for tool-use loops; ChatStructured is for the judge/canary, which
// tests typically configure via OptimisticJudge / disabled canary.
// Returns an error if called.
type ScriptedMock struct {
	name    string
	mu      sync.Mutex
	scripts []ScriptedResponse
	idx     int
}

// ScriptedResponse is one entry in the script. Fields:
//
//   - Text:       a final-reply text block (typical end-of-react).
//   - ToolUses:   tool calls the model emits this turn. Each gets an
//     auto-assigned ID if none provided. Both Text and
//     ToolUses can be set in the same response — the response
//     emits ToolUses first, then Text.
//   - StopReason: Anthropic-shape stop reason. Defaults: "tool_use"
//     when ToolUses are non-empty, "end_turn" otherwise.
type ScriptedResponse struct {
	Text       string                  `json:"text,omitempty"`
	ToolUses   []ScriptedToolUse       `json:"tool_uses,omitempty"`
	StopReason string                  `json:"stop_reason,omitempty"`
}

// ScriptedToolUse describes one tool call. Input is JSON-marshalled
// to bytes when the response is built — operators can write the
// expected tool args as a literal map, no manual escaping.
type ScriptedToolUse struct {
	ID    string `json:"id,omitempty"`
	Name  string `json:"name"`
	Input any    `json:"input,omitempty"`
}

// ScriptFile is the on-disk JSON shape. Loaded by LoadScriptedMock.
type ScriptFile struct {
	Responses []ScriptedResponse `json:"responses"`
}

// LoadScriptedMock reads a script JSON file and returns a
// ScriptedMock primed with its responses. Returns an error if the
// file is missing, malformed, or has zero responses (silent zero-
// length scripts mask test setup bugs).
func LoadScriptedMock(path string) (*ScriptedMock, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("script: read %q: %w", path, err)
	}
	var sf ScriptFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("script: parse %q: %w", path, err)
	}
	if len(sf.Responses) == 0 {
		return nil, fmt.Errorf("script: %q has zero responses", path)
	}
	return &ScriptedMock{
		name:    "mock-scripted",
		scripts: sf.Responses,
	}, nil
}

// NewScriptedMock builds a ScriptedMock from in-memory responses
// (test fixture path).
func NewScriptedMock(responses []ScriptedResponse) *ScriptedMock {
	if len(responses) == 0 {
		responses = []ScriptedResponse{{Text: "(empty script)", StopReason: "end_turn"}}
	}
	return &ScriptedMock{
		name:    "mock-scripted",
		scripts: responses,
	}
}

// Name is the provider identifier used in cycle-log + telemetry.
func (s *ScriptedMock) Name() string { return s.name }

// Chat returns the next scripted response or an error when the queue
// is exhausted. Each call advances the cursor under a mutex — safe
// for concurrent callers (rare in test paths but ensures parallel
// canary probes from the policy engine don't confuse the cursor).
func (s *ScriptedMock) Chat(_ context.Context, _ Request) (Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idx >= len(s.scripts) {
		return Response{}, fmt.Errorf("script: exhausted at call %d (len=%d)", s.idx+1, len(s.scripts))
	}
	step := s.scripts[s.idx]
	s.idx++

	resp := Response{}
	for i, tu := range step.ToolUses {
		id := tu.ID
		if id == "" {
			id = fmt.Sprintf("scripted-%d-%d", s.idx, i)
		}
		var inputBytes []byte
		if tu.Input != nil {
			b, err := json.Marshal(tu.Input)
			if err != nil {
				return Response{}, fmt.Errorf("script: marshal tool input for %s: %w", tu.Name, err)
			}
			inputBytes = b
		}
		resp.Content = append(resp.Content, ToolUseBlock{
			ID:    id,
			Name:  tu.Name,
			Input: inputBytes,
		})
	}
	if step.Text != "" {
		resp.Content = append(resp.Content, TextBlock{Text: step.Text})
	}
	if step.StopReason != "" {
		resp.StopReason = step.StopReason
	} else if len(step.ToolUses) > 0 {
		resp.StopReason = "tool_use"
	} else {
		resp.StopReason = "end_turn"
	}
	return resp, nil
}

// ChatStructured isn't scripted. Returns an error so test setups
// that accidentally route a structured call to the mock get a clear
// failure instead of silent zero-decode.
func (s *ScriptedMock) ChatStructured(_ context.Context, _ Request, schema Schema, _ any) (Usage, error) {
	return Usage{}, fmt.Errorf("script: ChatStructured (schema=%q) is not supported; route structured calls elsewhere or use a real provider", schema.Name)
}

// Compile-time check.
var _ Provider = (*ScriptedMock)(nil)
