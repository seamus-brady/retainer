package policy

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// Rule is one entry in a policy rule set. Pattern is the compiled regex;
// Importance × Magnitude contributes to the gate's score.
type Rule struct {
	Name       string
	Pattern    *regexp.Regexp
	Importance float64
	Magnitude  float64
	Domain     string
}

type ruleJSON struct {
	Name       string  `json:"name"`
	Pattern    string  `json:"pattern"`
	Importance float64 `json:"importance"`
	Magnitude  float64 `json:"magnitude"`
	Domain     string  `json:"domain,omitempty"`
}

func (r *Rule) UnmarshalJSON(data []byte) error {
	var raw ruleJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw.Name == "" {
		return fmt.Errorf("policy: rule missing name")
	}
	if raw.Pattern == "" {
		return fmt.Errorf("policy: rule %q missing pattern", raw.Name)
	}
	pattern, err := regexp.Compile(raw.Pattern)
	if err != nil {
		return fmt.Errorf("policy: rule %q: invalid pattern: %w", raw.Name, err)
	}
	if raw.Importance < 0 || raw.Importance > 1 {
		return fmt.Errorf("policy: rule %q: importance %f out of [0,1]", raw.Name, raw.Importance)
	}
	if raw.Magnitude < 0 || raw.Magnitude > 1 {
		return fmt.Errorf("policy: rule %q: magnitude %f out of [0,1]", raw.Name, raw.Magnitude)
	}
	r.Name = raw.Name
	r.Pattern = pattern
	r.Importance = raw.Importance
	r.Magnitude = raw.Magnitude
	r.Domain = raw.Domain
	return nil
}

// RuleSet is the policy ruleset loaded from JSON. Each gate has its own
// rule slice and threshold. Allowlists short-circuit tool-gate evaluation
// for paths/domains explicitly trusted.
//
// CommsOutbound is the comms-specific rule slice — the comms agent's
// send_email tool routes through `EvaluateCommsOutbound` before any
// HTTP. Patterns target external-leakage threats (credentials, internal
// URLs, system internals in prose) rather than the prompt-injection
// shape that drives the input gate. Distinct slice + threshold so
// adjusting comms safety doesn't ripple into TUI replies.
type RuleSet struct {
	Input         []Rule `json:"input_policies"`
	Tool          []Rule `json:"tool_policies"`
	Output        []Rule `json:"output_policies"`
	PostExec      []Rule `json:"post_exec_policies"`
	CommsOutbound []Rule `json:"comms_outbound_policies"`

	PathAllowlist   []string `json:"path_allowlist,omitempty"`
	DomainAllowlist []string `json:"domain_allowlist,omitempty"`

	InputThreshold         float64 `json:"input_threshold"`
	ToolThreshold          float64 `json:"tool_threshold"`
	OutputThreshold        float64 `json:"output_threshold"`
	PostExecThreshold      float64 `json:"post_exec_threshold"`
	CommsOutboundThreshold float64 `json:"comms_outbound_threshold"`
}
