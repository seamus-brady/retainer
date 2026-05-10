// Package tools is the bridge between cog.ToolDispatcher and the per-tool
// handlers. The cog imports nothing from here directly — it consumes the
// dispatcher through the cog.ToolDispatcher interface — so the tools
// surface evolves without coupling the cognitive loop to it.
//
// A handler exposes one llm.Tool plus a single Execute(ctx, raw-JSON) entry
// point. The model sees the tool's name + schema; the cog hands raw input
// JSON straight to Execute. Errors returned from Execute are surfaced to
// the model as a tool_result with IsError=true so it can recover; only
// genuinely catastrophic things (panics, ctx cancellations) bubble up.
package tools

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"

	"github.com/seamus-brady/retainer/internal/llm"
)

// Handler is implemented by every tool the cog can dispatch.
type Handler interface {
	Tool() llm.Tool
	Execute(ctx context.Context, input []byte) (string, error)
}

// Registry is a name → Handler map. Implements cog.ToolDispatcher when
// passed via Config.Tools.
type Registry struct {
	handlers map[string]Handler
}

func NewRegistry() *Registry {
	return &Registry{handlers: map[string]Handler{}}
}

// Register adds h under h.Tool().Name. Returns an error if the name is
// already taken — collisions are configuration bugs.
func (r *Registry) Register(h Handler) error {
	name := h.Tool().Name
	if name == "" {
		return fmt.Errorf("tools: handler has empty Tool().Name")
	}
	if _, exists := r.handlers[name]; exists {
		return fmt.Errorf("tools: %q already registered", name)
	}
	r.handlers[name] = h
	return nil
}

// MustRegister panics on Register error. Convenient for startup wiring
// where a duplicate name is a programmer mistake we want to fail loud.
func (r *Registry) MustRegister(h Handler) {
	if err := r.Register(h); err != nil {
		panic(err)
	}
}

// List returns the llm.Tool definitions for every registered handler.
// Sorted by tool name for deterministic ordering — load-bearing for
// upstream prompt-cache hits, since Go map iteration is randomised
// across runs and the tool list is part of the cacheable system-prompt
// prefix on Anthropic.
func (r *Registry) List() []llm.Tool {
	out := make([]llm.Tool, 0, len(r.handlers))
	for _, h := range r.handlers {
		out = append(out, h.Tool())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Dispatch routes one tool call by name. A missing name is surfaced as an
// error — the cog turns it into a ToolResultBlock with IsError=true so the
// model can correct itself.
//
// Defence in depth: every Execute call is wrapped in a defer-recover so a
// nil-deref or any other panic in tool code returns as a tool error rather
// than crashing the worker goroutine. The full stack is captured + logged
// via slog at ERROR level + propagated into the returned error message so
// the panic origin is visible in both slog AND the cycle-log JSONL (whose
// `tool_result` event carries the error string). Without this, a single
// buggy tool can abandon the whole cycle and the operator sees only "nil
// pointer dereference" with no stack to debug from.
func (r *Registry) Dispatch(ctx context.Context, name string, input []byte) (s string, derr error) {
	h, ok := r.handlers[name]
	if !ok {
		return "", fmt.Errorf("tools: no handler registered for %q", name)
	}
	defer func() {
		if rec := recover(); rec != nil {
			stack := debug.Stack()
			slog.Error("tool dispatch panic recovered",
				"tool", name,
				"input_len", len(input),
				"recover", fmt.Sprintf("%v", rec),
				"stack", string(stack),
			)
			s = ""
			derr = fmt.Errorf("tool %q panicked: %v\n%s", name, rec, truncateStack(stack, 30))
		}
	}()
	return h.Execute(ctx, input)
}

// truncateStack keeps the first n lines of a stack trace so cycle-log
// strings stay readable while still carrying the panic location. Full
// stack lands in slog regardless.
func truncateStack(stack []byte, n int) string {
	if len(stack) == 0 {
		return ""
	}
	count := 0
	for i, b := range stack {
		if b == '\n' {
			count++
			if count >= n {
				return string(stack[:i]) + "\n…(truncated; full stack in slog)"
			}
		}
	}
	return string(stack)
}

// Names returns the registered tool names; useful for startup logs.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.handlers))
	for n := range r.handlers {
		out = append(out, n)
	}
	return out
}
