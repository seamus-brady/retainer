package agent

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Registry is the lookup the cog (and other agents) use to find a
// running specialist by name. Mirrors Springdrift's
// `_impl_docs/ref/springdrift/src/agent/registry.gleam` — a small
// in-memory map of name → *Agent, with a Dispatch helper that
// encapsulates the Submit + reply-channel boilerplate.
//
// The Registry does not own agent lifecycle (start/stop) — that's the
// supervisor's job. It only tracks which agents are reachable from
// other actors, so the cog can route a delegate-tool call to the
// right specialist.
type Registry struct {
	mu     sync.RWMutex
	agents map[string]*Agent
}

func NewRegistry() *Registry {
	return &Registry{agents: make(map[string]*Agent)}
}

// Register adds an agent. Returns an error if the name is already
// taken — collisions are configuration bugs (each agent is a singleton
// in V1; teams of multiple instances under one name are deferred).
func (r *Registry) Register(a *Agent) error {
	if a == nil {
		return fmt.Errorf("agent.Registry: nil agent")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.agents[a.Name()]; exists {
		return fmt.Errorf("agent.Registry: %q already registered", a.Name())
	}
	r.agents[a.Name()] = a
	return nil
}

// MustRegister panics on Register error. Convenient for startup
// wiring where a duplicate name is a programmer mistake.
func (r *Registry) MustRegister(a *Agent) {
	if err := r.Register(a); err != nil {
		panic(err)
	}
}

// Get returns the named agent, or nil when absent.
func (r *Registry) Get(name string) *Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.agents[name]
}

// Names returns the registered agent names in lexical order. Used by
// the delegate tool to surface available agents.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.agents))
	for name := range r.agents {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// List returns snapshots of all registered agents. Order matches Names.
// Useful for surfacing each agent as a delegate tool to the cog's LLM.
func (r *Registry) List() []*Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.agents))
	for name := range r.agents {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]*Agent, len(names))
	for i, n := range names {
		out[i] = r.agents[n]
	}
	return out
}

// Dispatch is the synchronous "submit a task and wait for the outcome"
// helper. The caller passes the parent cog cycle ID for provenance —
// the agent stamps it onto the Outcome's AgentCycleID + the cycle log
// (eventually) so tooling can walk the parent → child tree.
//
// Returns the Outcome on success or an error on ctx cancellation or
// agent-not-found. A failed task (Outcome.IsSuccess() == false) is
// still returned without an error — the caller decides how to surface
// the failure to the LLM.
func (r *Registry) Dispatch(ctx context.Context, name, instruction, parentCycleID string) (Outcome, error) {
	a := r.Get(name)
	if a == nil {
		return Outcome{}, fmt.Errorf("agent.Registry: no agent registered with name %q", name)
	}
	reply := make(chan Outcome, 1)
	if err := a.Submit(ctx, Task{
		Instruction:   instruction,
		ParentCycleID: parentCycleID,
		Reply:         reply,
	}); err != nil {
		return Outcome{}, fmt.Errorf("agent.Registry: submit to %q: %w", name, err)
	}
	select {
	case out := <-reply:
		return out, nil
	case <-ctx.Done():
		// The agent will continue processing the task — its Reply
		// send is buffered so it won't block. We just stop waiting.
		return Outcome{}, ctx.Err()
	}
}
