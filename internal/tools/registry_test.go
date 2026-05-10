package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/seamus-brady/retainer/internal/llm"
)

// stubHandler implements Handler with caller-controlled
// Execute behaviour. Lets dispatch tests inject panics + errors
// without spinning real tools.
type stubHandler struct {
	name    string
	exec    func(context.Context, []byte) (string, error)
	descrip string
}

func (s stubHandler) Tool() llm.Tool {
	return llm.Tool{Name: s.name, Description: s.descrip, InputSchema: llm.Schema{Name: s.name}}
}

func (s stubHandler) Execute(ctx context.Context, input []byte) (string, error) {
	if s.exec == nil {
		return "ok", nil
	}
	return s.exec(ctx, input)
}

func TestRegistry_Dispatch_HappyPath(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(stubHandler{name: "echo", exec: func(_ context.Context, in []byte) (string, error) {
		return string(in), nil
	}})
	got, err := r.Dispatch(context.Background(), "echo", []byte("hi"))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if got != "hi" {
		t.Errorf("result = %q, want hi", got)
	}
}

func TestRegistry_Dispatch_UnknownToolReturnsError(t *testing.T) {
	r := NewRegistry()
	_, err := r.Dispatch(context.Background(), "missing", nil)
	if err == nil || !strings.Contains(err.Error(), "no handler registered") {
		t.Errorf("expected unknown-handler error, got %v", err)
	}
}

func TestRegistry_Dispatch_ErrorPassesThrough(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(stubHandler{name: "boom", exec: func(context.Context, []byte) (string, error) {
		return "", errors.New("planned failure")
	}})
	_, err := r.Dispatch(context.Background(), "boom", nil)
	if err == nil || !strings.Contains(err.Error(), "planned failure") {
		t.Errorf("expected error pass-through, got %v", err)
	}
}

func TestRegistry_Dispatch_RecoversPanicAsToolError(t *testing.T) {
	// The 2026-05-09 worker-panic regression: a tool that nil-
	// derefs would crash the worker goroutine and abandon the
	// whole cycle. The Registry now wraps every Execute in a
	// defer-recover so the panic returns as a tool error and
	// the cog can carry on (treats it as a normal IsError tool
	// result the LLM can recover from).
	r := NewRegistry()
	r.MustRegister(stubHandler{name: "deref", exec: func(context.Context, []byte) (string, error) {
		var p *struct{ X int }
		_ = p.X // panics here; the recover wrapping converts it to a tool error
		return "unreachable", nil
	}})

	got, err := r.Dispatch(context.Background(), "deref", []byte("anything"))
	if err == nil {
		t.Fatalf("expected panic to surface as tool error, got result %q", got)
	}
	for _, want := range []string{
		`tool "deref" panicked`,
		"nil pointer dereference",
		"goroutine", // stack trace marker
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %s", want, err.Error())
		}
	}
}

func TestRegistry_Dispatch_RecoversStringPanic(t *testing.T) {
	// Belt-and-braces: panics with non-error values must still
	// produce a useful tool error rather than mystery "panic".
	r := NewRegistry()
	r.MustRegister(stubHandler{name: "yelling", exec: func(context.Context, []byte) (string, error) {
		panic("kapow")
	}})
	_, err := r.Dispatch(context.Background(), "yelling", nil)
	if err == nil {
		t.Fatal("expected panic to surface as tool error")
	}
	if !strings.Contains(err.Error(), "kapow") {
		t.Errorf("error should carry panic value: %s", err.Error())
	}
	if !strings.Contains(err.Error(), `tool "yelling"`) {
		t.Errorf("error should name the tool: %s", err.Error())
	}
}
