package curator

// AgentDirectory is the slice of the agent registry the curator
// reads each cycle to populate the sensorium <agents> block. The
// production implementation lives in `cmd/retainer` (constructed
// from the registered specialist agents + agenttokens.Tracker).
//
// The interface is intentionally minimal — Agents() returns a
// snapshot, the curator copies what it needs into the rendered
// XML, no further calls. This keeps the curator independent of
// the agent registry's concrete shape (in-process *agent.Agent,
// SubprocessDelegate, future ACP-bridged remote agents — all map
// to AgentInfo).
//
// When Agents() returns empty (no specialists registered) the
// sensorium section drops out — agents-aware downstream readers
// shouldn't see an empty <agents/> wrapper.
type AgentDirectory interface {
	Agents() []AgentInfo
}

// AgentInfo is one specialist's cog-visible identity for the
// sensorium block. Carries enough for the cog's LLM to make a
// routing decision without follow-up tool calls.
type AgentInfo struct {
	// Name is the machine identifier ("researcher", "observer").
	Name string
	// Description is the one-line summary the cog's LLM sees on
	// the delegate tool. Mirrored here so the sensorium block is
	// self-contained.
	Description string
	// Available reports whether the specialist is currently
	// reachable (configured + running, not in a degraded restart
	// loop). V1 is a static "configured = available"; future
	// supervisor-degradation work flips this to false on
	// permanent failure.
	Available bool
	// TokensToday is the sum of input + output tokens used by
	// this agent so far today (workspace-local day). Resets at
	// midnight per agenttokens.Tracker semantics.
	TokensToday int
	// TokensLifetime is the all-time sum of input + output tokens.
	TokensLifetime int
	// DispatchCount is the cumulative number of tasks this agent
	// has handled.
	DispatchCount int
}

// emptyAgentDirectory is the zero-value directory the curator
// uses when none was wired. Returns no agents, so the sensorium
// section drops out.
type emptyAgentDirectory struct{}

func (emptyAgentDirectory) Agents() []AgentInfo { return nil }
