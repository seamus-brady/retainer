package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// Mock echoes the last user message back with a prefix on Chat. For
// ChatStructured, callers register a function that produces the structured
// response per (request, schema). Used when no provider is configured (e.g.
// no API key) and in tests.
type Mock struct {
	mu           sync.Mutex
	structuredFn StructuredFunc
	chatFn       ChatFunc
}

// StructuredFunc receives the request + schema and returns either a value
// to be JSON-marshalled into dst, or an error. Tests use this to inject
// canned structured responses.
type StructuredFunc func(req Request, schema Schema) (any, error)

// ChatFunc lets tests override the default echo behaviour — typically used
// to script tool-use loops (turn 1: ToolUseBlock; turn 2: TextBlock final).
type ChatFunc func(req Request) (Response, error)

func NewMock() *Mock { return &Mock{} }

func (*Mock) Name() string { return "mock" }

// SetStructuredFunc installs the structured-response producer. Safe to call
// from any goroutine.
func (m *Mock) SetStructuredFunc(fn StructuredFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.structuredFn = fn
}

// SetChatFunc installs a Chat override. When nil (default), Chat falls back
// to echoing the last user text.
func (m *Mock) SetChatFunc(fn ChatFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chatFn = fn
}

func (m *Mock) Chat(_ context.Context, req Request) (Response, error) {
	m.mu.Lock()
	fn := m.chatFn
	m.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	last, err := lastUserText(req.Messages)
	if err != nil {
		return Response{}, err
	}
	reply := "mock: " + strings.TrimSpace(last)
	return Response{
		Content:    []ContentBlock{TextBlock{Text: reply}},
		StopReason: "end_turn",
	}, nil
}

// ChatStructured invokes the registered StructuredFunc. If no func is
// registered, returns an error — tests must explicitly opt in to structured
// behaviour via SetStructuredFunc, so silent zero-value decoding doesn't mask
// missing test setup.
func (m *Mock) ChatStructured(_ context.Context, req Request, schema Schema, dst any) (Usage, error) {
	m.mu.Lock()
	fn := m.structuredFn
	m.mu.Unlock()
	if fn == nil {
		return Usage{}, fmt.Errorf("mock: ChatStructured called for schema %q but no StructuredFunc set; use SetStructuredFunc in tests", schema.Name)
	}
	val, err := fn(req, schema)
	if err != nil {
		return Usage{}, err
	}
	body, err := json.Marshal(val)
	if err != nil {
		return Usage{}, fmt.Errorf("mock: marshal structured value: %w", err)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return Usage{}, fmt.Errorf("mock: decode into dst: %w", err)
	}
	return Usage{}, nil
}

func lastUserText(msgs []Message) (string, error) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != RoleUser {
			continue
		}
		var b strings.Builder
		for _, blk := range msgs[i].Content {
			switch v := blk.(type) {
			case TextBlock:
				b.WriteString(v.Text)
			case ToolResultBlock:
				// Tool-loop plumbing on a user message; not user-authored
				// text so the echo path skips it.
			default:
				return "", fmt.Errorf("mock: no handling for content block %T", v)
			}
		}
		return b.String(), nil
	}
	return "", nil
}
