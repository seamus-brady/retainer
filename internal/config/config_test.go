package config

import (
	"strings"
	"testing"
)

func TestParseFullSchema(t *testing.T) {
	const src = `
provider        = "mistral"
task_model      = "mistral-small-2603"
reasoning_model = "mistral-small-2603"
max_tokens      = 2048
max_turns       = 5

[agent]
name    = "Retainer"
version = "0.1.0"

[logging]
verbose        = true
retention_days = 30

[retry]
max_retries         = 3
initial_delay_ms    = 500
rate_limit_delay_ms = 5000
overload_delay_ms   = 2000
max_delay_ms        = 60000
`
	c, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := StringOr(c.Provider, ""); got != "mistral" {
		t.Errorf("Provider = %q, want mistral", got)
	}
	if got := StringOr(c.TaskModel, ""); got != "mistral-small-2603" {
		t.Errorf("TaskModel = %q", got)
	}
	if got := IntOr(c.MaxTokens, 0); got != 2048 {
		t.Errorf("MaxTokens = %d", got)
	}
	if got := BoolOr(c.Logging.Verbose, false); !got {
		t.Errorf("Logging.Verbose = false, want true")
	}
	if got := IntOr(c.Retry.RateLimitDelayMs, 0); got != 5000 {
		t.Errorf("Retry.RateLimitDelayMs = %d", got)
	}
}

func TestParseEmpty(t *testing.T) {
	c, err := Parse(strings.NewReader(""))
	if err != nil {
		t.Fatalf("Parse empty: %v", err)
	}
	if c.Provider != nil {
		t.Errorf("Provider should be nil on empty config, got %q", *c.Provider)
	}
	if c.MaxTokens != nil {
		t.Errorf("MaxTokens should be nil on empty config, got %d", *c.MaxTokens)
	}
}

func TestParseUnknownKeysTolerated(t *testing.T) {
	const src = `
provider = "mistral"
not_a_real_field = "ignored"

[unknown_section]
also_ignored = true
`
	c, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := StringOr(c.Provider, ""); got != "mistral" {
		t.Errorf("Provider = %q, want mistral (known fields should still parse alongside unknowns)", got)
	}
}
