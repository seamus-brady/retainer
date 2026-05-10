package config

import (
	"strings"
	"testing"
)

// TestApplyEnvOverrides_String covers *string fields by walking
// them generically. Confirms the env var resolves to the right
// TOML key path, the override allocates a fresh pointer (no
// panic on nil starting state), and env wins over a TOML-set
// baseline value.
func TestApplyEnvOverrides_String(t *testing.T) {
	cases := []struct {
		envName string
		ptr     func(*Config) **string
	}{
		{"RETAINER_PROVIDER", func(c *Config) **string { return &c.Provider }},
		{"RETAINER_TASK_MODEL", func(c *Config) **string { return &c.TaskModel }},
		{"RETAINER_REASONING_MODEL", func(c *Config) **string { return &c.ReasoningModel }},
		{"RETAINER_AGENT_NAME", func(c *Config) **string { return &c.Agent.Name }},
		{"RETAINER_POLICY_INPUT_REFUSAL", func(c *Config) **string { return &c.Policy.InputRefusal }},
		{"RETAINER_POLICY_OUTPUT_REFUSAL", func(c *Config) **string { return &c.Policy.OutputRefusal }},
	}
	for _, tc := range cases {
		t.Run(tc.envName, func(t *testing.T) {
			t.Setenv(tc.envName, "from-env")
			c := &Config{}
			if err := applyEnvOverrides(c); err != nil {
				t.Fatal(err)
			}
			ptr := *tc.ptr(c)
			if ptr == nil {
				t.Fatalf("%s: pointer not set", tc.envName)
			}
			if *ptr != "from-env" {
				t.Errorf("%s = %q, want from-env", tc.envName, *ptr)
			}
		})
	}
}

func TestApplyEnvOverrides_Int(t *testing.T) {
	cases := []struct {
		envName string
		want    int
		ptr     func(*Config) **int
	}{
		{"RETAINER_MAX_TOKENS", 4096, func(c *Config) **int { return &c.MaxTokens }},
		{"RETAINER_MAX_TURNS", 5, func(c *Config) **int { return &c.MaxTurns }},
		{"RETAINER_COG_MAX_TOOL_TURNS", 12, func(c *Config) **int { return &c.Cog.MaxToolTurns }},
		{"RETAINER_GATE_TIMEOUT_MS", 7500, func(c *Config) **int { return &c.Gate.TimeoutMs }},
	}
	for _, tc := range cases {
		t.Run(tc.envName, func(t *testing.T) {
			t.Setenv(tc.envName, intStr(tc.want))
			c := &Config{}
			if err := applyEnvOverrides(c); err != nil {
				t.Fatal(err)
			}
			ptr := *tc.ptr(c)
			if ptr == nil {
				t.Fatalf("%s: pointer not set", tc.envName)
			}
			if *ptr != tc.want {
				t.Errorf("%s = %d, want %d", tc.envName, *ptr, tc.want)
			}
		})
	}
}

func TestApplyEnvOverrides_Bool(t *testing.T) {
	cases := []struct {
		envName string
		raw     string
		want    bool
		ptr     func(*Config) **bool
	}{
		{"RETAINER_LOGGING_VERBOSE", "0", false, func(c *Config) **bool { return &c.Logging.Verbose }},
	}
	for _, tc := range cases {
		t.Run(tc.envName, func(t *testing.T) {
			t.Setenv(tc.envName, tc.raw)
			c := &Config{}
			if err := applyEnvOverrides(c); err != nil {
				t.Fatal(err)
			}
			ptr := *tc.ptr(c)
			if ptr == nil {
				t.Fatalf("%s: pointer not set", tc.envName)
			}
			if *ptr != tc.want {
				t.Errorf("%s = %v, want %v", tc.envName, *ptr, tc.want)
			}
		})
	}
}

func TestApplyEnvOverrides_Float(t *testing.T) {
	t.Setenv("RETAINER_POLICY_FABRICATION_MIN_CONFIDENCE", "0.85")
	c := &Config{}
	if err := applyEnvOverrides(c); err != nil {
		t.Fatal(err)
	}
	if c.Policy.Fabrication.MinConfidence == nil ||
		*c.Policy.Fabrication.MinConfidence != 0.85 {
		t.Errorf("min_confidence = %v", c.Policy.Fabrication.MinConfidence)
	}
}

func TestApplyEnvOverrides_NestedSection(t *testing.T) {
	// Confirms the recursive prefix join: [policy.fabrication]
	// becomes RETAINER_POLICY_FABRICATION_<KEY>.
	t.Setenv("RETAINER_POLICY_FABRICATION_ENABLED", "true")
	t.Setenv("RETAINER_POLICY_FABRICATION_MODEL", "haiku")
	t.Setenv("RETAINER_POLICY_FABRICATION_TIMEOUT_MS", "3000")
	c := &Config{}
	if err := applyEnvOverrides(c); err != nil {
		t.Fatal(err)
	}
	if c.Policy.Fabrication.Enabled == nil || !*c.Policy.Fabrication.Enabled {
		t.Errorf("fabrication.enabled not overridden")
	}
	if c.Policy.Fabrication.Model == nil || *c.Policy.Fabrication.Model != "haiku" {
		t.Errorf("fabrication.model not overridden: %v", c.Policy.Fabrication.Model)
	}
	if c.Policy.Fabrication.TimeoutMs == nil || *c.Policy.Fabrication.TimeoutMs != 3000 {
		t.Errorf("fabrication.timeout_ms not overridden: %v", c.Policy.Fabrication.TimeoutMs)
	}
}

func TestApplyEnvOverrides_EnvWinsOverTOML(t *testing.T) {
	tomlValue := "from-toml"
	c := &Config{Provider: &tomlValue}
	t.Setenv("RETAINER_PROVIDER", "from-env")
	if err := applyEnvOverrides(c); err != nil {
		t.Fatal(err)
	}
	if c.Provider == nil || *c.Provider != "from-env" {
		t.Errorf("env did not win over TOML: %v", c.Provider)
	}
}

func TestApplyEnvOverrides_UnsetLeavesTOMLAlone(t *testing.T) {
	tomlValue := "from-toml"
	c := &Config{Provider: &tomlValue}
	if err := applyEnvOverrides(c); err != nil {
		t.Fatal(err)
	}
	if c.Provider == nil || *c.Provider != "from-toml" {
		t.Errorf("TOML value disturbed: %v", c.Provider)
	}
}

func TestApplyEnvOverrides_EmptyStringIsExplicitClear(t *testing.T) {
	t.Setenv("RETAINER_PROVIDER", "")
	c := &Config{}
	tomlValue := "anthropic"
	c.Provider = &tomlValue
	if err := applyEnvOverrides(c); err != nil {
		t.Fatal(err)
	}
	if c.Provider == nil || *c.Provider != "" {
		t.Errorf("expected explicit clear, got %v", c.Provider)
	}
}

func TestApplyEnvOverrides_InvalidIntFailsLoudly(t *testing.T) {
	t.Setenv("RETAINER_COG_MAX_TOOL_TURNS", "banana")
	err := applyEnvOverrides(&Config{})
	if err == nil {
		t.Fatal("expected error on non-numeric int env")
	}
	if !strings.Contains(err.Error(), "RETAINER_COG_MAX_TOOL_TURNS") {
		t.Errorf("error should mention the offending env var: %v", err)
	}
}

func TestApplyEnvOverrides_InvalidBoolFailsLoudly(t *testing.T) {
	t.Setenv("RETAINER_LOGGING_VERBOSE", "maybe")
	err := applyEnvOverrides(&Config{})
	if err == nil {
		t.Fatal("expected error on non-bool env")
	}
}

func TestApplyEnvOverrides_NilConfigSafe(t *testing.T) {
	if err := applyEnvOverrides(nil); err != nil {
		t.Errorf("nil config should be a no-op, got %v", err)
	}
}

// intStr is a tiny helper because t.Setenv wants strings and the
// table-driven tests above carry int wants.
func intStr(n int) string {
	switch n {
	case 4096:
		return "4096"
	case 5:
		return "5"
	case 12:
		return "12"
	case 7500:
		return "7500"
	}
	return ""
}
