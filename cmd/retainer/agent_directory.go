package main

import (
	"github.com/seamus-brady/retainer/internal/agenttokens"
	"github.com/seamus-brady/retainer/internal/curator"
)

// workspaceAgentDirectory satisfies curator.AgentDirectory by
// joining the static set of registered specialist agents (built
// at bootstrap time) with the live token tracker (read each
// cycle). The result is the input to the sensorium <agents>
// block.
//
// The static `entries` list is populated by bootstrap from the
// agents it actually registered as delegate tools — researcher
// (in-process or subprocess) and observer. Future additions:
// when a new specialist lands, append it here and the curator
// picks it up automatically.
type workspaceAgentDirectory struct {
	entries []agentDirEntry
	tokens  *agenttokens.Tracker
}

// agentDirEntry is the static identity of one specialist —
// available regardless of token state. Token data is added at
// query time from the tracker.
type agentDirEntry struct {
	name        string
	description string
}

// Agents returns one curator.AgentInfo per registered specialist,
// joining static identity with current token totals. Available is
// always true today; supervisor-degradation surfacing is the
// natural extension when restart loops become a real concern.
func (d *workspaceAgentDirectory) Agents() []curator.AgentInfo {
	if d == nil || len(d.entries) == 0 {
		return nil
	}
	out := make([]curator.AgentInfo, 0, len(d.entries))
	for _, e := range d.entries {
		info := curator.AgentInfo{
			Name:        e.name,
			Description: e.description,
			Available:   true,
		}
		if d.tokens != nil {
			s := d.tokens.Stats(e.name)
			info.TokensToday = s.TodayInput + s.TodayOutput
			info.TokensLifetime = s.LifetimeInput + s.LifetimeOutput
			info.DispatchCount = s.DispatchCount
		}
		out = append(out, info)
	}
	return out
}
