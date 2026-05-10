package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	_ "embed"
)

//go:embed defaults.json
var defaultRulesJSON []byte

// EmbeddedDefaults returns the JSON bytes of the embedded baseline ruleset.
// Used by `retainer init` to seed a workspace's policy.json.
func EmbeddedDefaults() []byte {
	out := make([]byte, len(defaultRulesJSON))
	copy(out, defaultRulesJSON)
	return out
}

// Load reads the policy ruleset from path. If path is empty, returns the
// embedded defaults. If the path is non-empty but the file doesn't exist,
// also falls back to embedded defaults — useful so workspaces without a
// policy.json still have safety. The returned source string identifies
// where the rules came from (path or "<embedded defaults>").
func Load(path string) (*RuleSet, string, error) {
	if path == "" {
		return parseRules(defaultRulesJSON, "<embedded defaults>")
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		rs, _, err := parseRules(defaultRulesJSON, "<embedded defaults>")
		return rs, "<embedded defaults>", err
	}
	if err != nil {
		return nil, "", fmt.Errorf("policy: open %s: %w", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, "", fmt.Errorf("policy: read %s: %w", path, err)
	}
	return parseRules(data, path)
}

func parseRules(data []byte, source string) (*RuleSet, string, error) {
	var rs RuleSet
	if err := json.Unmarshal(data, &rs); err != nil {
		return nil, source, fmt.Errorf("policy: parse %s: %w", source, err)
	}
	if err := validateThresholds(&rs); err != nil {
		return nil, source, err
	}
	return &rs, source, nil
}

func validateThresholds(rs *RuleSet) error {
	for name, t := range map[string]float64{
		"input_threshold":          rs.InputThreshold,
		"tool_threshold":           rs.ToolThreshold,
		"output_threshold":         rs.OutputThreshold,
		"post_exec_threshold":      rs.PostExecThreshold,
		"comms_outbound_threshold": rs.CommsOutboundThreshold,
	} {
		if t < 0 || t > 1 {
			return fmt.Errorf("policy: %s out of range [0,1]: %f", name, t)
		}
	}
	return nil
}
